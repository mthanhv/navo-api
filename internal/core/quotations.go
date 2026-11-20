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

// Quotation is a price offer to a customer (owner decision 2026-11-20). It is a draft
// until sent; a sent one is won by turning it into a contract, or marked lost with a reason.
// "expired" is not stored: a sent quotation past its valid-until date shows as expired.
type Quotation struct {
	ID             uuid.UUID       `json:"id"`
	Number         string          `json:"number"`
	Revision       int             `json:"revision"`
	Status         string          `json:"status"`
	State          string          `json:"state"`
	CustomerID     uuid.UUID       `json:"customer_id"`
	CustomerCode   string          `json:"customer_code"`
	CustomerName   string          `json:"customer_name"`
	ContactID      *uuid.UUID      `json:"contact_id"`
	ContactName    *string         `json:"contact_name"`
	Title          string          `json:"title"`
	Currency       string          `json:"currency"`
	ValidUntil     string          `json:"valid_until"`
	Terms          *string         `json:"terms"`
	SentOn         *string         `json:"sent_on"`
	LostReason     *string         `json:"lost_reason"`
	ContractID     *uuid.UUID      `json:"contract_id"`
	ContractNumber *string         `json:"contract_number"`
	OwnerID        uuid.UUID       `json:"owner_id"`
	OwnerName      string          `json:"owner_name"`
	Subtotal       string          `json:"subtotal"`
	Tax            string          `json:"tax"`
	Total          string          `json:"total"`
	LineCount      int             `json:"line_count"`
	Version        int64           `json:"version"`
	CreatedAt      time.Time       `json:"created_at"`
	Lines          []QuotationLine `json:"lines,omitempty"`
	// Detail only: the seller's name for the printed quotation.
	CompanyName string `json:"company_name,omitempty"`
}

// QuotationLine is one offered item; the name is the wording printed for the customer.
type QuotationLine struct {
	LineNo    int       `json:"line_no"`
	ProductID uuid.UUID `json:"product_id"`
	Name      string    `json:"name"`
	Unit      string    `json:"unit"`
	Quantity  string    `json:"quantity"`
	UnitPrice string    `json:"unit_price"`
	TaxRate   *string   `json:"tax_rate"`
	LeadTime  *string   `json:"lead_time"`
	Amount    string    `json:"amount"`
}

const quotationSelect = `SELECT q.id, q.number, q.revision, q.status, q.customer_id, o.code, o.name, q.contact_id, ct.full_name, q.title, q.currency,
	to_char(q.valid_until,'YYYY-MM-DD'), q.terms, to_char(q.sent_on,'YYYY-MM-DD'), q.lost_reason, q.contract_id, k.number, q.owner_id, u.full_name, q.version, q.created_at,
	COALESCE((SELECT sum(l.quantity*l.unit_price) FROM quotation_lines l WHERE l.quotation_id=q.id),0)::text,
	COALESCE((SELECT sum(round(l.quantity*l.unit_price*COALESCE(l.tax_rate,0),2)) FROM quotation_lines l WHERE l.quotation_id=q.id),0)::text,
	(SELECT count(*) FROM quotation_lines l WHERE l.quotation_id=q.id)
	FROM quotations q JOIN organizations o ON o.id=q.customer_id JOIN users u ON u.id=q.owner_id
	LEFT JOIN contacts ct ON ct.id=q.contact_id LEFT JOIN contracts k ON k.id=q.contract_id`

func (h *Handlers) scanQuotation(row pgx.Row) (Quotation, error) {
	var q Quotation
	var sub, tax string
	err := row.Scan(&q.ID, &q.Number, &q.Revision, &q.Status, &q.CustomerID, &q.CustomerCode, &q.CustomerName, &q.ContactID, &q.ContactName, &q.Title, &q.Currency,
		&q.ValidUntil, &q.Terms, &q.SentOn, &q.LostReason, &q.ContractID, &q.ContractNumber, &q.OwnerID, &q.OwnerName, &q.Version, &q.CreatedAt, &sub, &tax, &q.LineCount)
	if err != nil {
		return q, err
	}
	s, t := decimal.RequireFromString(sub).Round(2), decimal.RequireFromString(tax).Round(2)
	q.Subtotal, q.Tax, q.Total = s.String(), t.String(), s.Add(t).String()
	q.State = q.Status
	if q.Status == "sent" && q.ValidUntil < h.today().Format(time.DateOnly) {
		q.State = "expired"
	}
	return q, nil
}

func (h *Handlers) loadQuotation(ctx context.Context, q db.Querier, id uuid.UUID, p *access.Principal) (*Quotation, error) {
	out, err := h.scanQuotation(q.QueryRow(ctx, quotationSelect+` WHERE q.id=$1 AND ($2::uuid IS NULL OR q.owner_id=$2)`, id, p.OwnerFilter()))
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT line_no, product_id, name, unit, quantity::text, unit_price::text, tax_rate::text, lead_time, round(quantity*unit_price,2)::text
		FROM quotation_lines WHERE quotation_id=$1 ORDER BY line_no`, id)
	if err != nil {
		return nil, err
	}
	out.Lines, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (QuotationLine, error) {
		var l QuotationLine
		if err := r.Scan(&l.LineNo, &l.ProductID, &l.Name, &l.Unit, &l.Quantity, &l.UnitPrice, &l.TaxRate, &l.LeadTime, &l.Amount); err != nil {
			return l, err
		}
		// Plain decimals ("20", not "20.000000"), as the other endpoints return them.
		l.Quantity, l.UnitPrice, l.Amount = decimalx.MustParse(l.Quantity).String(), decimalx.MustParse(l.UnitPrice).String(), decimalx.MustParse(l.Amount).String()
		if l.TaxRate != nil {
			r := decimalx.MustParse(*l.TaxRate).String()
			l.TaxRate = &r
		}
		return l, nil
	})
	if err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `SELECT name FROM company_settings`).Scan(&out.CompanyName); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListQuotations handles GET /quotations?tab=open|won|lost|all&q=…: open means draft or sent (expired included).
func (h *Handlers) ListQuotations(c *gin.Context) {
	p := auth.Principal(c)
	page, size := httpx.Page(c)
	tab := c.DefaultQuery("tab", "open")
	if tab != "open" && tab != "won" && tab != "lost" && tab != "all" {
		response.Fail(c, apperrors.Invalid("tab", "one_of", map[string]any{"values": []string{"open", "won", "lost", "all"}}))
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	rows, err := h.Pool.Query(c.Request.Context(), quotationSelect+`
		WHERE ($1::uuid IS NULL OR q.owner_id=$1)
		  AND ($2='' OR q.number ILIKE '%'||$2||'%' OR q.title ILIKE '%'||$2||'%' OR o.name ILIKE '%'||$2||'%' OR o.code ILIKE '%'||$2||'%')
		ORDER BY (q.status IN ('draft','sent')) DESC, q.valid_until, q.created_at DESC, q.id`, p.OwnerFilter(), q)
	if err != nil {
		response.Fail(c, err)
		return
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Quotation, error) { return h.scanQuotation(r) })
	if err != nil {
		response.Fail(c, err)
		return
	}
	counts := map[string]int{"open": 0, "won": 0, "lost": 0, "all": len(all)}
	items := []Quotation{}
	for _, x := range all {
		group := "open"
		if x.Status == "won" || x.Status == "lost" {
			group = x.Status
		}
		counts[group]++
		if tab == "all" || tab == group {
			items = append(items, x)
		}
	}
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	response.ListCounts(c, items[start:end], page, size, len(items), counts)
}

// GetQuotation handles GET /quotations/:id.
func (h *Handlers) GetQuotation(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	q, err := h.loadQuotation(c.Request.Context(), h.Pool, id, auth.Principal(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, q)
}

type quotationLineInput struct {
	ProductID string  `json:"product_id"`
	Name      *string `json:"name"`
	Unit      *string `json:"unit"`
	Quantity  string  `json:"quantity"`
	UnitPrice string  `json:"unit_price"`
	TaxRate   *string `json:"tax_rate"`
	LeadTime  *string `json:"lead_time"`
}

type quotationInput struct {
	Version    *int64  `json:"version"`
	CustomerID string  `json:"customer_id"`
	ContactID  *string `json:"contact_id"`
	Title      string  `json:"title"`
	Currency   string  `json:"currency"`
	ValidUntil *string `json:"valid_until"`
	Terms      *string `json:"terms"`
	// OwnerID is the person in charge (PIC). Default: the creator; only people who see the
	// whole company may give a quotation to someone else (same rule as contracts).
	OwnerID *string              `json:"owner_id"`
	Lines   []quotationLineInput `json:"lines"`
}

// CreateQuotation handles POST /quotations.
func (h *Handlers) CreateQuotation(c *gin.Context) { h.saveQuotation(c, false) }

// UpdateQuotation handles PATCH /quotations/:id. Changing a sent quotation makes the next
// revision and puts it back to draft, so it is clear the customer has not seen it yet.
func (h *Handlers) UpdateQuotation(c *gin.Context) { h.saveQuotation(c, true) }

func (h *Handlers) saveQuotation(c *gin.Context, editing bool) {
	var editingID uuid.UUID
	if editing {
		var err error
		if editingID, err = httpx.PathID(c, "id"); err != nil {
			response.Fail(c, err)
			return
		}
	}
	p := auth.Principal(c)
	var in quotationInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	customerID := httpx.UUID(&errs, "customer_id", in.CustomerID)
	var contactID *uuid.UUID
	if in.ContactID != nil && *in.ContactID != "" {
		id := httpx.UUID(&errs, "contact_id", *in.ContactID)
		contactID = &id
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		errs.Add("title", "required", nil)
	} else if len([]rune(in.Title)) > 300 {
		errs.Add("title", "too_long", map[string]any{"max": 300})
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(in.Currency) != 3 || strings.Trim(in.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		errs.Add("currency", "currency", nil)
	}
	validUntil := httpx.Date(&errs, "valid_until", in.ValidUntil)
	if validUntil == nil && (in.ValidUntil == nil || *in.ValidUntil == "") {
		errs.Add("valid_until", "required", nil)
	}
	var ownerID *uuid.UUID
	if in.OwnerID != nil && *in.OwnerID != "" {
		id := httpx.UUID(&errs, "owner_id", *in.OwnerID)
		ownerID = &id
	}
	in.Terms = trimmed(in.Terms)
	if in.Terms != nil && len([]rune(*in.Terms)) > 4000 {
		errs.Add("terms", "too_long", map[string]any{"max": 4000})
	}
	if len(in.Lines) == 0 {
		errs.Add("lines", "required", map[string]any{"min": 1})
	}
	type line struct {
		product    uuid.UUID
		name, unit *string
		qty, price decimal.Decimal
		tax        *decimal.Decimal
		leadTime   *string
	}
	lines := make([]line, len(in.Lines))
	for i, l := range in.Lines {
		f := func(name string) string { return fmt.Sprintf("lines[%d].%s", i, name) }
		lines[i] = line{product: httpx.UUID(&errs, f("product_id"), l.ProductID), name: trimmed(l.Name), unit: trimmed(l.Unit),
			qty: decimalx.Positive(&errs, f("quantity"), l.Quantity), price: decimalx.NonNegative(&errs, f("unit_price"), l.UnitPrice), leadTime: trimmed(l.LeadTime)}
		if l.TaxRate != nil && *l.TaxRate != "" {
			rate := decimalx.NonNegative(&errs, f("tax_rate"), *l.TaxRate)
			if rate.GreaterThan(decimal.NewFromInt(1)) {
				errs.Add(f("tax_rate"), "less_or_equal", map[string]any{"max": "1"})
			}
			lines[i].tax = &rate
		}
		if lines[i].leadTime != nil && len([]rune(*lines[i].leadTime)) > 100 {
			errs.Add(f("lead_time"), "too_long", map[string]any{"max": 100})
		}
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	if ownerID != nil && *ownerID != p.UserID && p.OwnerFilter() != nil {
		response.Fail(c, apperrors.Forbidden("assign_owner"))
		return
	}
	scope, status := "quotations.create", httpx.Created
	if editing {
		scope, status = "quotations.update", 200
	}
	httpx.Command(c, h.Pool, scope, false, status, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var isCustomer bool
		if err := tx.QueryRow(ctx, `SELECT is_customer FROM organizations WHERE id=$1`, customerID).Scan(&isCustomer); err != nil || !isCustomer {
			return nil, apperrors.Invalid("customer_id", "not_customer", nil)
		}
		if contactID != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM contacts WHERE id=$1 AND organization_id=$2)`, *contactID, customerID).Scan(&ok); err != nil || !ok {
				return nil, apperrors.Invalid("contact_id", "not_found", nil)
			}
		}
		if ownerID != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND u.is_active AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role IN ('sales','admin')))`, *ownerID).Scan(&ok); err != nil || !ok {
				return nil, apperrors.Invalid("owner_id", "not_found", nil)
			}
		}
		id := editingID
		action := "create"
		if editing {
			var st string
			var version int64
			if err := tx.QueryRow(ctx, `SELECT status, version FROM quotations WHERE id=$1 AND ($2::uuid IS NULL OR owner_id=$2) FOR UPDATE`, id, p.OwnerFilter()).Scan(&st, &version); err != nil {
				return nil, err
			}
			if st == "won" || st == "lost" {
				return nil, apperrors.Rule("quotation_closed", "This quotation is already won or lost. Reopen a lost one to change it.", nil)
			}
			if in.Version == nil || *in.Version != version {
				return nil, apperrors.Conflict("version_conflict", "This quotation changed. Reload before editing.", nil)
			}
			// A sent quotation that changes is a new revision the customer has not seen yet.
			if _, err := tx.Exec(ctx, `UPDATE quotations SET customer_id=$2, contact_id=$3, title=$4, currency=$5, valid_until=$6, terms=$7, owner_id=COALESCE($8, owner_id),
				revision = revision + CASE WHEN status='sent' THEN 1 ELSE 0 END, status='draft', version=version+1, updated_at=now() WHERE id=$1`,
				id, customerID, contactID, in.Title, in.Currency, validUntil, in.Terms, ownerID); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM quotation_lines WHERE quotation_id=$1`, id); err != nil {
				return nil, err
			}
			action = "update"
		} else {
			year := h.today().Year()
			seq, err := numbering.NextYearly(ctx, tx, "QT", year)
			if err != nil {
				return nil, err
			}
			owner := p.UserID
			if ownerID != nil {
				owner = *ownerID
			}
			if err := tx.QueryRow(ctx, `INSERT INTO quotations (number, customer_id, contact_id, title, currency, valid_until, terms, owner_id, created_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, numbering.YearlyNumber("QT", year, seq), customerID, contactID, in.Title, in.Currency, validUntil, in.Terms, owner, p.UserID).Scan(&id); err != nil {
				return nil, err
			}
		}
		for i, l := range lines {
			var name, unit string
			if err := tx.QueryRow(ctx, `SELECT name, unit FROM products WHERE id=$1`, l.product).Scan(&name, &unit); err != nil {
				return nil, apperrors.Invalid(fmt.Sprintf("lines[%d].product_id", i), "not_found", nil)
			}
			if l.name != nil {
				name = *l.name
			}
			if l.unit != nil {
				unit = *l.unit
			}
			if _, err := tx.Exec(ctx, `INSERT INTO quotation_lines (quotation_id, line_no, product_id, name, unit, quantity, unit_price, tax_rate, lead_time)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, i+1, l.product, name, unit, l.qty, l.price, l.tax, l.leadTime); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, action, "quotation", id, map[string]any{"title": in.Title, "customer_id": customerID, "owner_id": ownerID, "lines": len(lines)}); err != nil {
			return nil, err
		}
		return h.loadQuotation(ctx, tx, id, p)
	})
}

// QuotationStatus handles POST /quotations/:id/{send,lost,reopen}.
func (h *Handlers) QuotationStatus(to string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := httpx.PathID(c, "id")
		if err != nil {
			response.Fail(c, err)
			return
		}
		p := auth.Principal(c)
		var in struct {
			Reason *string `json:"reason"`
		}
		if to == "lost" {
			if err := httpx.Decode(c, &in); err != nil {
				response.Fail(c, err)
				return
			}
			in.Reason = trimmed(in.Reason)
			if in.Reason == nil {
				response.Fail(c, apperrors.Invalid("reason", "required", nil))
				return
			}
			if len([]rune(*in.Reason)) > 500 {
				response.Fail(c, apperrors.Invalid("reason", "too_long", map[string]any{"max": 500}))
				return
			}
		}
		httpx.Command(c, h.Pool, "quotations."+to, true, 200, func(tx pgx.Tx) (any, error) {
			ctx := c.Request.Context()
			var st string
			if err := tx.QueryRow(ctx, `SELECT status FROM quotations WHERE id=$1 AND ($2::uuid IS NULL OR owner_id=$2) FOR UPDATE`, id, p.OwnerFilter()).Scan(&st); err != nil {
				return nil, err
			}
			var sql string
			args := []any{id}
			switch {
			case to == "send" && (st == "draft" || st == "sent"):
				sql = `UPDATE quotations SET status='sent', sent_on=$2, version=version+1, updated_at=now() WHERE id=$1`
				args = append(args, h.today())
			case to == "lost" && (st == "draft" || st == "sent"):
				sql = `UPDATE quotations SET status='lost', lost_reason=$2, version=version+1, updated_at=now() WHERE id=$1`
				args = append(args, in.Reason)
			case to == "reopen" && st == "lost":
				sql = `UPDATE quotations SET status='draft', lost_reason=NULL, version=version+1, updated_at=now() WHERE id=$1`
			default:
				return nil, apperrors.Rule("quotation_status", "This quotation cannot change that way from its current status.", map[string]any{"status": st})
			}
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				return nil, err
			}
			if err := audit.Record(ctx, tx, to, "quotation", id, map[string]any{"from": st, "reason": in.Reason}); err != nil {
				return nil, err
			}
			return h.loadQuotation(ctx, tx, id, p)
		})
	}
}

// DeleteQuotation handles DELETE /quotations/:id: only a draft that was never sent.
func (h *Handlers) DeleteQuotation(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	httpx.Command(c, h.Pool, "quotations.delete", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var st string
		var rev int
		var sent *time.Time
		if err := tx.QueryRow(ctx, `SELECT status, revision, sent_on FROM quotations WHERE id=$1 AND ($2::uuid IS NULL OR owner_id=$2) FOR UPDATE`, id, p.OwnerFilter()).Scan(&st, &rev, &sent); err != nil {
			return nil, err
		}
		if st != "draft" || rev != 1 || sent != nil {
			return nil, apperrors.Rule("quotation_was_sent", "A quotation the customer has seen is kept. Mark it lost instead.", nil)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM quotations WHERE id=$1`, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "delete", "quotation", id, nil); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": true}, nil
	})
}

// QuotationContractInput handles GET /quotations/:id/contract-input: the new-contract form
// filled from the quotation. Nothing is saved until the contract is created with quotation_id.
func (h *Handlers) QuotationContractInput(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	q, err := h.loadQuotation(c.Request.Context(), h.Pool, id, auth.Principal(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	if q.Status != "draft" && q.Status != "sent" {
		response.Fail(c, apperrors.Rule("quotation_closed", "Only an open quotation can become a contract.", nil))
		return
	}
	ref := q.Number
	if q.Revision > 1 {
		ref = fmt.Sprintf("%s rev %d", q.Number, q.Revision)
	}
	// The quotation's PIC becomes the contract's owner.
	owner := q.OwnerID.String()
	in := contractInput{CustomerID: q.CustomerID.String(), Title: q.Title, Currency: q.Currency, CustomerReference: &ref, OwnerID: &owner, Lines: []contractLineInput{}}
	for _, l := range q.Lines {
		name, unit := l.Name, l.Unit
		in.Lines = append(in.Lines, contractLineInput{ProductID: l.ProductID.String(), NameOnContract: &name, Unit: &unit, Quantity: l.Quantity, UnitPrice: l.UnitPrice, TaxRate: l.TaxRate})
	}
	response.OK(c, in)
}

// winQuotation links a new contract to the quotation it came from and marks the quotation won.
func winQuotation(ctx context.Context, tx pgx.Tx, p *access.Principal, quotationID, contractID, customerID uuid.UUID) error {
	var st string
	var customer uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT status, customer_id FROM quotations WHERE id=$1 AND ($2::uuid IS NULL OR owner_id=$2) FOR UPDATE`, quotationID, p.OwnerFilter()).Scan(&st, &customer); err != nil {
		return apperrors.Invalid("quotation_id", "not_found", nil)
	}
	if st != "draft" && st != "sent" {
		return apperrors.Rule("quotation_closed", "This quotation is already won or lost.", nil)
	}
	if customer != customerID {
		return apperrors.Invalid("customer_id", "quotation_customer", nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE quotations SET status='won', contract_id=$2, version=version+1, updated_at=now() WHERE id=$1`, quotationID, contractID); err != nil {
		return err
	}
	// The quotation's files (the customer's request, our offer) also appear in the contract's folder.
	if _, err := tx.Exec(ctx, `INSERT INTO attachment_links (file_id, owner_type, owner_id, customer_id, contract_id, doc_kind, created_by)
		SELECT l.file_id, 'contract', $2, l.customer_id, $2, CASE WHEN l.doc_kind IN ('quotation','customer_request','correspondence','photo') THEN l.doc_kind ELSE 'other' END, $3
		FROM attachment_links l WHERE l.owner_type='quotation' AND l.owner_id=$1 ON CONFLICT DO NOTHING`, quotationID, contractID, p.UserID); err != nil {
		return err
	}
	return audit.Record(ctx, tx, "won", "quotation", quotationID, map[string]any{"contract_id": contractID})
}
