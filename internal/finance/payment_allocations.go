package finance

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// lockInvoice reads totals only after waiting for the invoice lock. The caller
// holds the payment lock first when allocating an existing advance.
func lockInvoice(ctx context.Context, tx pgx.Tx, invoice, supplier uuid.UUID, cur string, amount decimal.Decimal) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM purchase_invoices WHERE id=$1 FOR UPDATE`, invoice).Scan(&status); err != nil {
		return err
	}
	if status != "recorded" {
		return apperrors.Rule("invoice_not_recorded", "This invoice cannot receive money.", nil)
	}
	var sup uuid.UUID
	var currency, total, paid string
	if err := tx.QueryRow(ctx, `SELECT supplier_id,currency,amount_total::text,coalesce((SELECT sum(a.amount) FROM payment_allocations a JOIN payments p ON p.id=a.payment_id WHERE a.purchase_invoice_id=i.id AND a.reversed_at IS NULL AND p.status='posted'),0)::text FROM purchase_invoices i WHERE id=$1`, invoice).Scan(&sup, &currency, &total, &paid); err != nil {
		return err
	}
	if sup != supplier {
		return apperrors.Invalid("purchase_invoice_id", "other_supplier", nil)
	}
	if strings.TrimSpace(currency) != cur {
		return apperrors.Invalid("purchase_invoice_id", "other_currency", nil)
	}
	if left := decimalx.MustParse(total).Sub(decimalx.MustParse(paid)); amount.GreaterThan(left) {
		return apperrors.Invalid("amount", "exceeds_open", map[string]any{"open": left.String()})
	}
	return nil
}

func (h *Handlers) AllocatePayment(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in struct {
		InvoiceID string `json:"purchase_invoice_id"`
		Amount    string `json:"amount"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	invoice := httpx.UUID(&errs, "purchase_invoice_id", in.InvoiceID)
	amount := decimalx.Positive(&errs, "amount", in.Amount).Round(2)
	if amount.IsZero() {
		errs.Add("amount", "greater_than", map[string]any{"min": "0"})
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "payments.allocate", true, 201, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM payments WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
			return nil, err
		}
		if status != "posted" {
			return nil, apperrors.Rule("payment_not_posted", "This payment cannot be allocated.", nil)
		}
		var supplier uuid.UUID
		var cur, total, used string
		if err := tx.QueryRow(ctx, `SELECT supplier_id,currency,amount::text,coalesce((SELECT sum(amount) FROM payment_allocations WHERE payment_id=p.id AND reversed_at IS NULL),0)::text FROM payments p WHERE id=$1`, id).Scan(&supplier, &cur, &total, &used); err != nil {
			return nil, err
		}
		if amount.GreaterThan(decimalx.MustParse(total).Sub(decimalx.MustParse(used))) {
			return nil, apperrors.Rule("allocation_exceeds_receipt", "Allocation exceeds the available payment.", nil)
		}
		if err := lockInvoice(ctx, tx, invoice, supplier, strings.TrimSpace(cur), amount); err != nil {
			return nil, err
		}
		var aid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO payment_allocations(payment_id,purchase_invoice_id,amount,created_by) VALUES($1,$2,$3,$4) RETURNING id`, id, invoice, amount, p.UserID).Scan(&aid); err != nil {
			return nil, err
		}
		return gin.H{"id": aid, "payment_id": id, "purchase_invoice_id": invoice, "amount": amount}, audit.Record(ctx, tx, "allocate", "payment", id, map[string]any{"allocation_id": aid, "invoice_id": invoice, "amount": amount.String()})
	})
}

func (h *Handlers) ReversePaymentAllocation(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in reverseInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		response.Fail(c, apperrors.Invalid("reason", "required", nil))
		return
	}
	httpx.Command(c, h.Pool, "payment_allocations.reverse", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var pid, iid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT payment_id,purchase_invoice_id FROM payment_allocations WHERE id=$1`, id).Scan(&pid, &iid); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM payments WHERE id=$1 FOR UPDATE`, pid); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM purchase_invoices WHERE id=$1 FOR UPDATE`, iid); err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `UPDATE payment_allocations SET reversed_at=now(),reversed_by=$2,reverse_reason=$3 WHERE id=$1 AND reversed_at IS NULL`, id, auth.Principal(c).UserID, in.Reason)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, apperrors.Rule("allocation_reversed", "This allocation was already reversed.", nil)
		}
		return gin.H{"id": id, "reversed": true}, audit.Record(ctx, tx, "reverse", "payment_allocation", id, map[string]any{"reason": in.Reason})
	})
}

// ReverseCash retains original money and export history; live allocations must be
// explicitly reversed first so users can see and correct the downstream effects.
func (h *Handlers) ReverseCash(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := httpx.PathID(c, "id")
		if err != nil {
			response.Fail(c, err)
			return
		}
		var in reverseInput
		if err := httpx.Decode(c, &in); err != nil {
			response.Fail(c, err)
			return
		}
		in.Reason = strings.TrimSpace(in.Reason)
		if in.Reason == "" || len([]rune(in.Reason)) > 500 {
			response.Fail(c, apperrors.Invalid("reason", "required", nil))
			return
		}
		table, alloc, fk := "receipts", "receipt_allocations", "receipt_id"
		if kind == "payment" {
			table, alloc, fk = "payments", "payment_allocations", "payment_id"
		}
		httpx.Command(c, h.Pool, kind+".reverse", true, 200, func(tx pgx.Tx) (any, error) {
			ctx := c.Request.Context()
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('misa_export'))`); err != nil {
				return nil, err
			}
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM `+table+` WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
				return nil, err
			}
			if status != "posted" {
				return nil, apperrors.Rule("document_not_posted", "Only a posted document can be reversed.", nil)
			}
			var dependent bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+alloc+` WHERE `+fk+`=$1 AND reversed_at IS NULL)`, id).Scan(&dependent); err != nil {
				return nil, err
			}
			if dependent {
				return nil, apperrors.Rule("reversal_has_dependents", "Reverse the allocations before reversing this cash document.", nil)
			}
			if _, err := tx.Exec(ctx, `UPDATE `+table+` SET status='reversed' WHERE id=$1`, id); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE misa_exports SET correction_needed=true WHERE id=(SELECT misa_export_id FROM `+table+` WHERE id=$1)`, id); err != nil {
				return nil, err
			}
			var rid uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO document_reversals(document_type,document_id,reason,created_by) VALUES($1,$2,$3,$4) RETURNING id`, kind, id, in.Reason, auth.Principal(c).UserID).Scan(&rid); err != nil {
				return nil, err
			}
			return gin.H{"id": id, "status": "reversed", "reversal_id": rid}, audit.Record(ctx, tx, "reverse", kind, id, map[string]any{"reason": in.Reason, "reversal_id": rid})
		})
	}
}

func (h *Handlers) PaymentAllocations(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT a.id,a.purchase_invoice_id,i.number,a.amount::text,a.reversed_at,a.reverse_reason FROM payment_allocations a JOIN purchase_invoices i ON i.id=a.purchase_invoice_id WHERE a.payment_id=$1 ORDER BY a.created_at,a.id`, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var aid, iid uuid.UUID
		var number, amount string
		var reason *string
		var reversed any
		if err := rows.Scan(&aid, &iid, &number, &amount, &reversed, &reason); err != nil {
			response.Fail(c, err)
			return
		}
		out = append(out, map[string]any{"id": aid, "purchase_invoice_id": iid, "number": number, "amount": amount, "reversed_at": reversed, "reverse_reason": reason})
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}
