package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/numbering"
	"navo-api/internal/response"
)

// PurchaseOrder is sent to a supplier and always belongs to one contract (BR-03).
type PurchaseOrder struct {
	ID             uuid.UUID `json:"id"`
	Number         string    `json:"number"`
	ContractID     uuid.UUID `json:"contract_id"`
	ContractNumber string    `json:"contract_number"`
	SupplierID     uuid.UUID `json:"supplier_id"`
	SupplierName   string    `json:"supplier_name"`
	Status         string    `json:"status"`
	Currency       string    `json:"currency"`
	Lines          []POLine  `json:"lines"`
}

// POLine references exactly one line of the same contract. UnitPrice is the
// purchase price and is omitted without purchase_cost.read (never just hidden by the client).
type POLine struct {
	ID               uuid.UUID        `json:"id"`
	ContractLineID   uuid.UUID        `json:"contract_line_id"`
	ContractLineNo   int              `json:"contract_line_no"`
	NameOnContract   string           `json:"name_on_contract"`
	SupplierItemName *string          `json:"supplier_item_name"`
	Unit             string           `json:"unit"`
	Quantity         decimal.Decimal  `json:"quantity"`
	UnitPrice        *decimal.Decimal `json:"unit_price,omitempty"`
	ExpectedOn       *string          `json:"expected_on"`
	Received         decimal.Decimal  `json:"received"`
}

func loadPO(ctx context.Context, q db.Querier, id uuid.UUID, p *access.Principal) (*PurchaseOrder, error) {
	var po PurchaseOrder
	err := q.QueryRow(ctx, `SELECT po.id, po.number, po.contract_id, c.number, po.supplier_id, s.name, po.status, po.currency
		FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id JOIN organizations s ON s.id = po.supplier_id
		WHERE po.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, p.OwnerFilter()).
		Scan(&po.ID, &po.Number, &po.ContractID, &po.ContractNumber, &po.SupplierID, &po.SupplierName, &po.Status, &po.Currency)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT pol.id, pol.contract_line_id, cl.line_no, cl.name_on_contract, pol.supplier_item_name, cl.unit,
			pol.quantity::text, pol.unit_price::text, pol.expected_on,
			coalesce((SELECT sum(grl.accepted_quantity) FROM goods_receipt_lines grl JOIN goods_receipts gr ON gr.id = grl.goods_receipt_id
			          WHERE grl.purchase_order_line_id = pol.id AND gr.status = 'confirmed'), 0)::text
		FROM purchase_order_lines pol JOIN contract_lines cl ON cl.id = pol.contract_line_id
		WHERE pol.purchase_order_id = $1 ORDER BY cl.line_no, pol.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seeCost := p.Can(access.PurchaseCostRead)
	po.Lines = []POLine{}
	for rows.Next() {
		var l POLine
		var qty, price, received string
		var expected *time.Time
		if err := rows.Scan(&l.ID, &l.ContractLineID, &l.ContractLineNo, &l.NameOnContract, &l.SupplierItemName, &l.Unit,
			&qty, &price, &expected, &received); err != nil {
			return nil, err
		}
		l.Quantity, l.Received, l.ExpectedOn = decimalx.MustParse(qty), decimalx.MustParse(received), dateString(expected)
		if seeCost {
			v := decimalx.MustParse(price)
			l.UnitPrice = &v
		}
		po.Lines = append(po.Lines, l)
	}
	return &po, rows.Err()
}

type poLineInput struct {
	ContractLineID   string  `json:"contract_line_id"`
	Quantity         string  `json:"quantity"`
	UnitPrice        string  `json:"unit_price"`
	SupplierItemName *string `json:"supplier_item_name"`
	ExpectedOn       *string `json:"expected_on"`
}

type poInput struct {
	SupplierID string        `json:"supplier_id"`
	Currency   string        `json:"currency"`
	Lines      []poLineInput `json:"lines"`
}

// CreatePurchaseOrder handles POST /contracts/:id/purchase-orders (a draft).
// Lines can only be taken from the contract; ordering more than the contract
// is allowed in a draft and refused at confirmation (BR-05).
func (h *Handlers) CreatePurchaseOrder(c *gin.Context) {
	p := auth.Principal(c)
	contractID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in poInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	supplierID := httpx.UUID(&errs, "supplier_id", in.SupplierID)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(in.Currency) != 3 || strings.Trim(in.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		errs.Add("currency", "currency", nil)
	}
	if len(in.Lines) == 0 {
		errs.Add("lines", "required", map[string]any{"min": 1})
	}
	type line struct {
		contractLine uuid.UUID
		qty, price   decimal.Decimal
		name         *string
		expected     *time.Time
	}
	lines := make([]line, len(in.Lines))
	seen := map[uuid.UUID]bool{}
	for i, l := range in.Lines {
		f := func(n string) string { return fmt.Sprintf("lines[%d].%s", i, n) }
		lines[i] = line{
			contractLine: httpx.UUID(&errs, f("contract_line_id"), l.ContractLineID),
			qty:          decimalx.Positive(&errs, f("quantity"), l.Quantity),
			price:        decimalx.NonNegative(&errs, f("unit_price"), l.UnitPrice),
			name:         trimmed(l.SupplierItemName),
			expected:     httpx.Date(&errs, f("expected_on"), l.ExpectedOn),
		}
		if seen[lines[i].contractLine] {
			errs.Add(f("contract_line_id"), "duplicate", nil)
		}
		seen[lines[i].contractLine] = true
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "purchase_orders.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		k, err := lockContract(ctx, tx, contractID, p, false)
		if err != nil {
			return nil, err
		}
		if k.Status != "active" {
			return nil, apperrors.Rule("contract_not_active", "Activate the contract before ordering from suppliers.", map[string]any{"status": k.Status})
		}
		var isSupplier bool
		if err := tx.QueryRow(ctx, `SELECT is_supplier FROM organizations WHERE id = $1`, supplierID).Scan(&isSupplier); err != nil || !isSupplier {
			return nil, apperrors.Invalid("supplier_id", "not_supplier", nil)
		}
		var lineErrs apperrors.FieldErrors
		for i, l := range lines {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contract_lines WHERE id = $1 AND contract_id = $2)`, l.contractLine, contractID).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				lineErrs.Add(fmt.Sprintf("lines[%d].contract_line_id", i), "not_in_contract", nil)
			}
		}
		if err := lineErrs.Err(); err != nil {
			return nil, err
		}
		number, err := numbering.NextChild(ctx, tx, contractID, "PO")
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO purchase_orders (contract_id, number, supplier_id, currency, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, contractID, number, supplierID, in.Currency, p.UserID).Scan(&id); err != nil {
			return nil, err
		}
		for _, l := range lines {
			if _, err := tx.Exec(ctx, `INSERT INTO purchase_order_lines (purchase_order_id, contract_id, contract_line_id, quantity, unit_price, supplier_item_name, expected_on)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, contractID, l.contractLine, l.qty, l.price, l.name, l.expected); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "purchase_order", id, map[string]any{"number": number, "contract_id": contractID,
			"supplier_id": supplierID, "lines": len(lines)}); err != nil {
			return nil, err
		}
		return loadPO(ctx, tx, id, p)
	})
}

// GetPurchaseOrder handles GET /purchase-orders/:id.
func (h *Handlers) GetPurchaseOrder(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	po, err := loadPO(c.Request.Context(), h.Pool, id, auth.Principal(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, po)
}

// ConfirmPurchaseOrder handles POST /purchase-orders/:id/confirm. Contract
// lines are locked in id order and totals re-read after the lock, so two
// confirmations cannot together exceed the contract (AC-12).
func (h *Handlers) ConfirmPurchaseOrder(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "purchase_orders.confirm", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		var contractID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT po.status, po.contract_id FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id
			WHERE po.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF po`, id, p.OwnerFilter()).Scan(&status, &contractID); err != nil {
			return nil, err
		}
		if status != "draft" {
			return nil, apperrors.Rule("document_not_draft", "Only a draft purchase order can be confirmed.", map[string]any{"status": status})
		}
		rows, err := tx.Query(ctx, `SELECT cl.id, cl.line_no, cl.quantity::text, pol.quantity::text
			FROM contract_lines cl JOIN purchase_order_lines pol ON pol.contract_line_id = cl.id AND pol.purchase_order_id = $1
			ORDER BY cl.id FOR UPDATE OF cl`, id)
		if err != nil {
			return nil, err
		}
		type check struct {
			lineID           uuid.UUID
			lineNo           int
			contractQty, req decimal.Decimal
		}
		var checks []check
		for rows.Next() {
			var ch check
			var cq, rq string
			if err := rows.Scan(&ch.lineID, &ch.lineNo, &cq, &rq); err != nil {
				rows.Close()
				return nil, err
			}
			ch.contractQty, ch.req = decimalx.MustParse(cq), decimalx.MustParse(rq)
			checks = append(checks, ch)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		lineIDs := make([]uuid.UUID, len(checks))
		for i, ch := range checks {
			lineIDs[i] = ch.lineID
			var ordered string
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(pol.quantity), 0)::text FROM purchase_order_lines pol
				JOIN purchase_orders po ON po.id = pol.purchase_order_id WHERE pol.contract_line_id = $1 AND po.status = 'confirmed'`, ch.lineID).Scan(&ordered); err != nil {
				return nil, err
			}
			already := decimalx.MustParse(ordered)
			if already.Add(ch.req).GreaterThan(ch.contractQty) {
				// BR-05: an over-order needs Admin approval (approval requests: next increment).
				return nil, apperrors.Rule("quantity_exceeds_contract", "This order would exceed the contract quantity. Save as draft or ask Admin to approve.",
					map[string]any{"line_no": ch.lineNo, "contract_quantity": ch.contractQty.String(), "already_ordered": already.String(), "requested": ch.req.String()})
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE purchase_orders SET status = 'confirmed', confirmed_at = now(), confirmed_by = $2, version = version + 1, updated_at = now()
			WHERE id = $1`, id, p.UserID); err != nil {
			return nil, err
		}
		if err := touchLines(ctx, tx, lineIDs); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "confirm", "purchase_order", id, map[string]any{"status": []string{"draft", "confirmed"}}); err != nil {
			return nil, err
		}
		return loadPO(ctx, tx, id, p)
	})
}
