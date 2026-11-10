package finance

import (
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
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/numbering"
	"navo-api/internal/response"
)

// Invoice is a sales invoice (SI-yy-nnnn-pp, with the legal e-invoice number) or a
// supplier invoice (PI-yy-nnnn). Amounts are as printed on the invoice (O-04).
type Invoice struct {
	ID             uuid.UUID        `json:"id"`
	Kind           string           `json:"kind"` // sales | purchase
	Number         string           `json:"number"`
	LegalNumber    string           `json:"legal_number"`
	IssuedOn       string           `json:"issued_on"`
	DueOn          *string          `json:"due_on,omitempty"`
	PartnerID      uuid.UUID        `json:"partner_id"`
	PartnerName    string           `json:"partner_name"`
	ContractID     *uuid.UUID       `json:"contract_id,omitempty"`
	ContractNumber *string          `json:"contract_number,omitempty"`
	Description    string           `json:"description,omitempty"`
	MilestoneID    *uuid.UUID       `json:"milestone_id,omitempty"`
	Orders         []string         `json:"purchase_orders,omitempty"`
	Currency       string           `json:"currency"`
	AmountNet      decimal.Decimal  `json:"amount_net"`
	VATAmount      decimal.Decimal  `json:"vat_amount"`
	AmountTotal    decimal.Decimal  `json:"amount_total"`
	Paid           *decimal.Decimal `json:"paid"` // nil: cash is allocated to obligations, not this sales invoice
	Status         string           `json:"status"`
	Exported       bool             `json:"exported"`
}

type amountsInput struct {
	LegalNumber string  `json:"legal_number"`
	IssuedOn    *string `json:"issued_on"`
	Currency    string  `json:"currency"`
	AmountNet   string  `json:"amount_net"`
	VATAmount   string  `json:"vat_amount"`
}

func (in *amountsInput) parse(errs *apperrors.FieldErrors, today time.Time) (*time.Time, decimal.Decimal, decimal.Decimal, string) {
	in.LegalNumber = strings.TrimSpace(in.LegalNumber)
	if in.LegalNumber == "" {
		errs.Add("legal_number", "required", nil)
	} else if len(in.LegalNumber) > 50 {
		errs.Add("legal_number", "too_long", map[string]any{"max": 50})
	}
	on := httpx.Date(errs, "issued_on", in.IssuedOn)
	if on == nil {
		errs.Add("issued_on", "required", nil)
	} else if on.After(today) {
		errs.Add("issued_on", "not_future", nil)
	}
	net := decimalx.NonNegative(errs, "amount_net", in.AmountNet).Round(2)
	vat := decimalx.NonNegative(errs, "vat_amount", in.VATAmount).Round(2)
	return on, net, vat, currency(errs, in.Currency)
}

type salesInvoiceInput struct {
	amountsInput
	Description string  `json:"description"`
	MilestoneID *string `json:"milestone_id"`
}

// CreateSalesInvoice handles POST /contracts/:id/sales-invoices (finance.write).
func (h *Handlers) CreateSalesInvoice(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in salesInvoiceInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	on, net, vat, cur := in.parse(&errs, h.today())
	in.Description = strings.TrimSpace(in.Description)
	if in.Description == "" {
		errs.Add("description", "required", nil)
	} else if len([]rune(in.Description)) > 300 {
		errs.Add("description", "too_long", map[string]any{"max": 300})
	}
	var milestone *uuid.UUID
	if in.MilestoneID != nil && *in.MilestoneID != "" {
		m := httpx.UUID(&errs, "milestone_id", *in.MilestoneID)
		milestone = &m
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "sales_invoices.create", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status, contractCurrency string
		if err := tx.QueryRow(ctx, `SELECT status, currency FROM contracts WHERE id = $1 FOR UPDATE`, id).Scan(&status, &contractCurrency); err != nil {
			return nil, err
		}
		if status != "active" && status != "closed" {
			return nil, apperrors.Rule("contract_not_active", "Activate the contract before recording invoices.", nil)
		}
		if cur != strings.TrimSpace(contractCurrency) {
			return nil, apperrors.Invalid("currency", "other_currency", map[string]any{"currency": contractCurrency})
		}
		if milestone != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_milestones WHERE id = $1 AND contract_id = $2)`, *milestone, id).Scan(&ok); err != nil || !ok {
				return nil, apperrors.Invalid("milestone_id", "not_in_contract", nil)
			}
		}
		number, err := numbering.NextChild(ctx, tx, id, "SI")
		if err != nil {
			return nil, err
		}
		var iid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO sales_invoices (contract_id, number, legal_number, issued_on, description, milestone_id, currency, amount_net, vat_amount, amount_total, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`, id, number, in.LegalNumber, *on, in.Description, milestone, cur, net, vat, net.Add(vat), p.UserID).Scan(&iid); err != nil {
			if apperrors.From(err).Reason == "duplicate" {
				return nil, apperrors.Invalid("legal_number", "taken", nil)
			}
			return nil, err
		}
		if err := audit.Record(ctx, tx, "create", "sales_invoice", iid, map[string]any{"number": number, "legal_number": in.LegalNumber, "total": net.Add(vat).String()}); err != nil {
			return nil, err
		}
		return h.invoiceByID(c, tx, "sales", iid)
	})
}

type purchaseInvoiceInput struct {
	amountsInput
	SupplierID     string   `json:"supplier_id"`
	DueOn          *string  `json:"due_on"`
	PurchaseOrders []string `json:"purchase_order_ids"`
}

// CreatePurchaseInvoice handles POST /purchase-invoices (finance.write + purchase prices):
// one supplier invoice may cover purchase orders of several contracts.
func (h *Handlers) CreatePurchaseInvoice(c *gin.Context) {
	p := auth.Principal(c)
	var in purchaseInvoiceInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	on, net, vat, cur := in.parse(&errs, h.today())
	supplier := httpx.UUID(&errs, "supplier_id", in.SupplierID)
	due := httpx.Date(&errs, "due_on", in.DueOn)
	pos := make([]uuid.UUID, 0, len(in.PurchaseOrders))
	for i, raw := range in.PurchaseOrders {
		pos = append(pos, httpx.UUID(&errs, "purchase_order_ids["+itoa(i)+"]", raw))
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "purchase_invoices.create", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var isSupplier bool
		if err := tx.QueryRow(ctx, `SELECT is_supplier FROM organizations WHERE id = $1`, supplier).Scan(&isSupplier); err != nil || !isSupplier {
			return nil, apperrors.Invalid("supplier_id", "not_supplier", nil)
		}
		if len(pos) > 0 {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM purchase_orders WHERE id = ANY($1) AND supplier_id = $2 AND status = 'confirmed'`, pos, supplier).Scan(&n); err != nil {
				return nil, err
			}
			if n != len(pos) {
				return nil, apperrors.Invalid("purchase_order_ids", "other_supplier", nil)
			}
		}
		seq, err := numbering.NextYearly(ctx, tx, "PI", on.Year()%100)
		if err != nil {
			return nil, err
		}
		number := numbering.YearlyNumber("PI", on.Year(), seq)
		var iid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO purchase_invoices (number, supplier_id, legal_number, issued_on, due_on, currency, amount_net, vat_amount, amount_total, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`, number, supplier, in.LegalNumber, *on, due, cur, net, vat, net.Add(vat), p.UserID).Scan(&iid); err != nil {
			if apperrors.From(err).Reason == "duplicate" {
				return nil, apperrors.Invalid("legal_number", "taken", nil)
			}
			return nil, err
		}
		for _, po := range pos {
			if _, err := tx.Exec(ctx, `INSERT INTO purchase_invoice_orders (purchase_invoice_id, purchase_order_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, iid, po); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "purchase_invoice", iid, map[string]any{"number": number, "legal_number": in.LegalNumber, "total": net.Add(vat).String()}); err != nil {
			return nil, err
		}
		return h.invoiceByID(c, tx, "purchase", iid)
	})
}

const salesSelect = `SELECT i.id, 'sales', i.number, i.legal_number, i.issued_on, NULL::date, c.customer_id, o.name, c.id, c.number, i.description, i.milestone_id,
	'{}'::text[], i.currency, i.amount_net::text, i.vat_amount::text, i.amount_total::text,
	NULL::text, i.status, i.misa_export_id IS NOT NULL
	FROM sales_invoices i JOIN contracts c ON c.id = i.contract_id JOIN organizations o ON o.id = c.customer_id`

const purchaseSelect = `SELECT i.id, 'purchase', i.number, i.legal_number, i.issued_on, i.due_on, i.supplier_id, o.name, NULL::uuid, NULL::text, '', NULL::uuid,
	coalesce((SELECT array_agg(po.number ORDER BY po.number) FROM purchase_invoice_orders x JOIN purchase_orders po ON po.id = x.purchase_order_id WHERE x.purchase_invoice_id = i.id), '{}'),
	i.currency, i.amount_net::text, i.vat_amount::text, i.amount_total::text,
	coalesce((SELECT sum(a.amount) FROM payment_allocations a JOIN payments p ON p.id=a.payment_id WHERE a.purchase_invoice_id=i.id AND a.reversed_at IS NULL AND p.status='posted'),0)::text, i.status, i.misa_export_id IS NOT NULL
	FROM purchase_invoices i JOIN organizations o ON o.id = i.supplier_id`

func scanInvoice(row pgx.Row) (Invoice, error) {
	var v Invoice
	var issued time.Time
	var due *time.Time
	var net, vat, total string
	var paid *string
	err := row.Scan(&v.ID, &v.Kind, &v.Number, &v.LegalNumber, &issued, &due, &v.PartnerID, &v.PartnerName, &v.ContractID, &v.ContractNumber, &v.Description,
		&v.MilestoneID, &v.Orders, &v.Currency, &net, &vat, &total, &paid, &v.Status, &v.Exported)
	if err != nil {
		return v, err
	}
	v.Currency = strings.TrimSpace(v.Currency)
	v.IssuedOn = issued.Format(time.DateOnly)
	v.DueOn = dateStr(due)
	v.AmountNet, v.VATAmount, v.AmountTotal = decimalx.MustParse(net), decimalx.MustParse(vat), decimalx.MustParse(total)
	if paid != nil {
		d := decimalx.MustParse(*paid)
		v.Paid = &d
	}
	return v, nil
}

func (h *Handlers) invoiceByID(c *gin.Context, tx pgx.Tx, kind string, id uuid.UUID) (any, error) {
	sql := map[string]string{"sales": salesSelect, "purchase": purchaseSelect}[kind]
	return scanInvoice(tx.QueryRow(c.Request.Context(), sql+` WHERE i.id = $1`, id))
}

// ListInvoices handles GET /invoices?kind=sales|purchase&period=YYYY-MM&contract_id=.
// Supplier invoices need purchase-price access too.
func (h *Handlers) ListInvoices(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	kind := c.DefaultQuery("kind", "sales")
	if kind == "purchase" && !p.Can(access.PurchaseCostRead) {
		response.Fail(c, apperrors.Forbidden(string(access.PurchaseCostRead)))
		return
	}
	sql := map[string]string{"sales": salesSelect, "purchase": purchaseSelect}[kind]
	if sql == "" {
		response.Fail(c, apperrors.Invalid("kind", "one_of", map[string]any{"values": []string{"sales", "purchase"}}))
		return
	}
	where := []string{"true"}
	args := []any{}
	if period := c.Query("period"); period != "" {
		start, err := time.Parse("2006-01", period)
		if err != nil {
			response.Fail(c, apperrors.Invalid("period", "format", nil))
			return
		}
		args = append(args, start, start.AddDate(0, 1, 0))
		where = append(where, "i.issued_on >= $1 AND i.issued_on < $2")
	}
	if v := c.Query("contract_id"); v != "" && kind == "sales" {
		id, err := uuid.Parse(v)
		if err != nil {
			response.Fail(c, apperrors.Invalid("contract_id", "uuid", nil))
			return
		}
		args = append(args, id)
		where = append(where, "i.contract_id = $"+itoa(len(args)))
	}
	rows, err := h.Pool.Query(ctx, sql+` WHERE `+strings.Join(where, " AND ")+` ORDER BY i.issued_on DESC, i.number DESC`, args...)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []Invoice{}
	for rows.Next() {
		v, err := scanInvoice(rows)
		if err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, items)
}

// CancelInvoice handles POST /invoices/:kind/:id/cancel: a wrong entry is cancelled
// (number kept); one already exported to MISA must be corrected in MISA too.
func (h *Handlers) CancelInvoice(c *gin.Context) {
	kind := c.Param("kind")
	table := map[string]string{"sales": "sales_invoices", "purchase": "purchase_invoices"}[kind]
	if table == "" {
		response.Fail(c, apperrors.NotFound())
		return
	}
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, table+".cancel", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		// Use the same order as export creation before locking a document.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('misa_export'))`); err != nil {
			return nil, err
		}
		var status string
		var exported bool
		if err := tx.QueryRow(ctx, `SELECT status, misa_export_id IS NOT NULL FROM `+table+` WHERE id = $1 FOR UPDATE`, id).Scan(&status, &exported); err != nil {
			return nil, err
		}
		if status == "cancelled" {
			return nil, apperrors.Rule("already_cancelled", "This invoice is already cancelled.", nil)
		}
		if kind == "purchase" {
			var paid bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_allocations WHERE purchase_invoice_id = $1 AND reversed_at IS NULL)`, id).Scan(&paid); err != nil {
				return nil, err
			}
			if paid {
				return nil, apperrors.Rule("invoice_has_payments", "Reverse the linked payments before cancelling this invoice.", nil)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET status = 'cancelled' WHERE id = $1`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE misa_exports SET correction_needed = true WHERE id = (SELECT misa_export_id FROM `+table+` WHERE id = $1)`, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "cancel", strings.TrimSuffix(table, "s"), id, map[string]any{"exported_to_misa": exported}); err != nil {
			return nil, err
		}
		return h.invoiceByID(c, tx, kind, id)
	})
}

type paymentInput struct {
	SupplierID        string  `json:"supplier_id"`
	PaidOn            *string `json:"paid_on"`
	Amount            string  `json:"amount"`
	Currency          string  `json:"currency"`
	Method            string  `json:"method"`
	Reference         *string `json:"reference"`
	PurchaseInvoiceID *string `json:"purchase_invoice_id"`
}

// Payment is money paid to a supplier (phiếu chi, PV-yy-nnnn).
type Payment struct {
	ID            uuid.UUID       `json:"id"`
	Number        string          `json:"number"`
	SupplierID    uuid.UUID       `json:"supplier_id"`
	SupplierName  string          `json:"supplier_name"`
	PaidOn        string          `json:"paid_on"`
	Amount        decimal.Decimal `json:"amount"`
	Currency      string          `json:"currency"`
	Method        string          `json:"method"`
	Reference     *string         `json:"reference"`
	InvoiceNumber *string         `json:"purchase_invoice_number"`
	Exported      bool            `json:"exported"`
	Allocated     decimal.Decimal `json:"allocated"`
	Unallocated   decimal.Decimal `json:"unallocated"`
	Status        string          `json:"status"`
}

const paymentSelect = `SELECT p.id, p.number, p.supplier_id, o.name, p.paid_on, p.amount::text, p.currency, p.method, p.reference, pi.number, p.misa_export_id IS NOT NULL, coalesce((SELECT sum(amount) FROM payment_allocations WHERE payment_id=p.id AND reversed_at IS NULL),0)::text, p.status
	FROM payments p JOIN organizations o ON o.id = p.supplier_id LEFT JOIN purchase_invoices pi ON pi.id = p.purchase_invoice_id`

func scanPayment(row pgx.Row) (Payment, error) {
	var v Payment
	var on time.Time
	var amount, allocated string
	err := row.Scan(&v.ID, &v.Number, &v.SupplierID, &v.SupplierName, &on, &amount, &v.Currency, &v.Method, &v.Reference, &v.InvoiceNumber, &v.Exported, &allocated, &v.Status)
	if err != nil {
		return v, err
	}
	v.Allocated = decimalx.MustParse(allocated)
	v.PaidOn, v.Amount, v.Currency = on.Format(time.DateOnly), decimalx.MustParse(amount), strings.TrimSpace(v.Currency)
	v.Unallocated = v.Amount.Sub(v.Allocated)
	return v, err
}

// CreatePayment handles POST /payments (finance.write).
func (h *Handlers) CreatePayment(c *gin.Context) {
	p := auth.Principal(c)
	var in paymentInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	supplier := httpx.UUID(&errs, "supplier_id", in.SupplierID)
	on := httpx.Date(&errs, "paid_on", in.PaidOn)
	if on == nil {
		errs.Add("paid_on", "required", nil)
	} else if on.After(h.today()) {
		errs.Add("paid_on", "not_future", nil)
	}
	amount := decimalx.Positive(&errs, "amount", in.Amount).Round(2)
	cur := currency(&errs, in.Currency)
	if in.Method != "bank" && in.Method != "cash" {
		errs.Add("method", "one_of", map[string]any{"values": []string{"bank", "cash"}})
	}
	ref := trimmedPtr(in.Reference, 200, "reference", &errs)
	var invoice *uuid.UUID
	if in.PurchaseInvoiceID != nil && *in.PurchaseInvoiceID != "" {
		v := httpx.UUID(&errs, "purchase_invoice_id", *in.PurchaseInvoiceID)
		invoice = &v
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "payments.create", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var isSupplier bool
		if err := tx.QueryRow(ctx, `SELECT is_supplier FROM organizations WHERE id = $1`, supplier).Scan(&isSupplier); err != nil || !isSupplier {
			return nil, apperrors.Invalid("supplier_id", "not_supplier", nil)
		}
		if invoice != nil {
			if err := lockInvoice(ctx, tx, *invoice, supplier, cur, amount); err != nil {
				return nil, err
			}
		}
		seq, err := numbering.NextYearly(ctx, tx, "PV", on.Year()%100)
		if err != nil {
			return nil, err
		}
		number := numbering.YearlyNumber("PV", on.Year(), seq)
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO payments (number, supplier_id, paid_on, amount, currency, method, reference, purchase_invoice_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, number, supplier, *on, amount, cur, in.Method, ref, invoice, p.UserID).Scan(&id); err != nil {
			return nil, err
		}
		if invoice != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO payment_allocations(payment_id,purchase_invoice_id,amount,created_by) VALUES($1,$2,$3,$4)`, id, *invoice, amount, p.UserID); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "payment", id, map[string]any{"number": number, "amount": amount.String(), "currency": cur}); err != nil {
			return nil, err
		}
		return scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1`, id))
	})
}

// ListPayments handles GET /payments?period=YYYY-MM.
func (h *Handlers) ListPayments(c *gin.Context) {
	ctx := c.Request.Context()
	sql, args := paymentSelect+` WHERE true`, []any{}
	if period := c.Query("period"); period != "" {
		start, err := time.Parse("2006-01", period)
		if err != nil {
			response.Fail(c, apperrors.Invalid("period", "format", nil))
			return
		}
		sql += ` AND p.paid_on >= $1 AND p.paid_on < $2`
		args = append(args, start, start.AddDate(0, 1, 0))
	}
	rows, err := h.Pool.Query(ctx, sql+` ORDER BY p.paid_on DESC, p.number DESC`, args...)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []Payment{}
	for rows.Next() {
		v, err := scanPayment(rows)
		if err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, items)
}
