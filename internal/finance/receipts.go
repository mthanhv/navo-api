package finance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/numbering"
	"navo-api/internal/response"
)

// Receipt is money received from a customer (phiếu thu, RV-yy-nnnn).
type Receipt struct {
	ID           uuid.UUID       `json:"id"`
	Number       string          `json:"number"`
	CustomerID   uuid.UUID       `json:"customer_id"`
	CustomerName string          `json:"customer_name"`
	ReceivedOn   string          `json:"received_on"`
	Amount       decimal.Decimal `json:"amount"`
	Currency     string          `json:"currency"`
	Method       string          `json:"method"`
	Reference    *string         `json:"reference"`
	Note         *string         `json:"note"`
	Status       string          `json:"status"`
	Allocated    decimal.Decimal `json:"allocated"`
	Unallocated  decimal.Decimal `json:"unallocated"`
	Allocations  []Allocation    `json:"allocations,omitempty"`
	// Suggestions are the customer's collectable, still-open amounts in the same currency.
	Suggestions []Milestone `json:"suggestions,omitempty"`
	Exported    bool        `json:"exported"`
}

// Allocation is part of a receipt applied to a milestone.
type Allocation struct {
	ID             uuid.UUID       `json:"id"`
	MilestoneID    uuid.UUID       `json:"milestone_id"`
	MilestoneName  string          `json:"milestone_name"`
	ContractNumber string          `json:"contract_number"`
	Amount         decimal.Decimal `json:"amount"`
	CreatedAt      time.Time       `json:"created_at"`
	ReversedAt     *time.Time      `json:"reversed_at"`
	ReverseReason  *string         `json:"reverse_reason"`
}

const receiptSelect = `SELECT r.id, r.number, r.customer_id, o.name, r.received_on, r.amount::text, r.currency, r.method, r.reference, r.note, r.status,
	coalesce((SELECT sum(a.amount) FROM receipt_allocations a WHERE a.receipt_id = r.id AND a.reversed_at IS NULL), 0)::text, r.misa_export_id IS NOT NULL
	FROM receipts r JOIN organizations o ON o.id = r.customer_id`

func scanReceipt(row pgx.Row) (*Receipt, error) {
	var r Receipt
	var on time.Time
	var amount, allocated string
	if err := row.Scan(&r.ID, &r.Number, &r.CustomerID, &r.CustomerName, &on, &amount, &r.Currency, &r.Method, &r.Reference, &r.Note, &r.Status, &allocated, &r.Exported); err != nil {
		return nil, err
	}
	r.Currency = strings.TrimSpace(r.Currency)
	r.ReceivedOn = on.Format(time.DateOnly)
	r.Amount, r.Allocated = decimalx.MustParse(amount), decimalx.MustParse(allocated)
	r.Unallocated = r.Amount.Sub(r.Allocated)
	return &r, nil
}

func (h *Handlers) loadReceipt(ctx context.Context, q db.Querier, id uuid.UUID, withDetail bool) (*Receipt, error) {
	r, err := scanReceipt(q.QueryRow(ctx, receiptSelect+` WHERE r.id = $1`, id))
	if err != nil || !withDetail {
		return r, err
	}
	rows, err := q.Query(ctx, `SELECT a.id, a.milestone_id, m.name, c.number, a.amount::text, a.created_at, a.reversed_at, a.reverse_reason
		FROM receipt_allocations a JOIN payment_milestones m ON m.id = a.milestone_id JOIN contracts c ON c.id = m.contract_id
		WHERE a.receipt_id = $1 ORDER BY a.created_at`, id)
	if err != nil {
		return nil, err
	}
	r.Allocations = []Allocation{}
	for rows.Next() {
		var a Allocation
		var amount string
		if err := rows.Scan(&a.ID, &a.MilestoneID, &a.MilestoneName, &a.ContractNumber, &amount, &a.CreatedAt, &a.ReversedAt, &a.ReverseReason); err != nil {
			rows.Close()
			return nil, err
		}
		a.Amount = decimalx.MustParse(amount)
		r.Allocations = append(r.Allocations, a)
	}
	rows.Close()
	ms, err := h.loadMilestones(ctx, q, `c.customer_id = $1 AND c.currency = $2 AND c.status IN ('active', 'closed')`, true, r.CustomerID, r.Currency)
	if err != nil {
		return nil, err
	}
	r.Suggestions = []Milestone{}
	for _, m := range ms {
		if m.Status != "paid" {
			r.Suggestions = append(r.Suggestions, m) // "wait" ones are shown but cannot take money
		}
	}
	return r, nil
}

type receiptInput struct {
	CustomerID string  `json:"customer_id"`
	ReceivedOn *string `json:"received_on"`
	Amount     string  `json:"amount"`
	Currency   string  `json:"currency"`
	Method     string  `json:"method"`
	Reference  *string `json:"reference"`
	Note       *string `json:"note"`
}

func trimmedPtr(s *string, max int, field string, errs *apperrors.FieldErrors) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	if len([]rune(t)) > max {
		errs.Add(field, "too_long", map[string]any{"max": max})
	}
	return &t
}

func currency(errs *apperrors.FieldErrors, raw string) string {
	c := strings.ToUpper(strings.TrimSpace(raw))
	if len(c) != 3 || strings.Trim(c, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		errs.Add("currency", "currency", nil)
	}
	return c
}

// CreateReceipt handles POST /receipts (finance.write): numbered RV-{yy}-{nnnn} by the year received.
func (h *Handlers) CreateReceipt(c *gin.Context) {
	p := auth.Principal(c)
	var in receiptInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	customer := httpx.UUID(&errs, "customer_id", in.CustomerID)
	on := httpx.Date(&errs, "received_on", in.ReceivedOn)
	if on == nil {
		errs.Add("received_on", "required", nil)
	} else if on.After(h.today()) {
		errs.Add("received_on", "not_future", nil)
	}
	amount := decimalx.Positive(&errs, "amount", in.Amount).Round(2)
	cur := currency(&errs, in.Currency)
	if in.Method != "bank" && in.Method != "cash" {
		errs.Add("method", "one_of", map[string]any{"values": []string{"bank", "cash"}})
	}
	ref := trimmedPtr(in.Reference, 200, "reference", &errs)
	note := trimmedPtr(in.Note, 500, "note", &errs)
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "receipts.create", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var isCustomer bool
		if err := tx.QueryRow(ctx, `SELECT is_customer FROM organizations WHERE id = $1`, customer).Scan(&isCustomer); err != nil || !isCustomer {
			return nil, apperrors.Invalid("customer_id", "not_customer", nil)
		}
		seq, err := numbering.NextYearly(ctx, tx, "RV", on.Year()%100)
		if err != nil {
			return nil, err
		}
		number := numbering.YearlyNumber("RV", on.Year(), seq)
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO receipts (number, customer_id, received_on, amount, currency, method, reference, note, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, number, customer, *on, amount, cur, in.Method, ref, note, p.UserID).Scan(&id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "create", "receipt", id, map[string]any{"number": number, "amount": amount.String(), "currency": cur}); err != nil {
			return nil, err
		}
		return h.loadReceipt(ctx, tx, id, true)
	})
}

// GetReceipt handles GET /receipts/:id with allocations and what it could pay.
func (h *Handlers) GetReceipt(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	r, err := h.loadReceipt(c.Request.Context(), h.Pool, id, true)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, r)
}

// ListReceipts handles GET /receipts?unallocated=1&customer_id=&period=YYYY-MM.
func (h *Handlers) ListReceipts(c *gin.Context) {
	ctx := c.Request.Context()
	page, size := httpx.Page(c)
	where := []string{"true"}
	args := []any{}
	if v := c.Query("customer_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			response.Fail(c, apperrors.Invalid("customer_id", "uuid", nil))
			return
		}
		args = append(args, id)
		where = append(where, "r.customer_id = $1")
	}
	if period := c.Query("period"); period != "" {
		start, err := time.Parse("2006-01", period)
		if err != nil {
			response.Fail(c, apperrors.Invalid("period", "format", nil))
			return
		}
		n := len(args)
		args = append(args, start, start.AddDate(0, 1, 0))
		where = append(where, fmt.Sprintf("r.received_on >= $%d AND r.received_on < $%d", n+1, n+2))
	}
	if c.Query("unallocated") == "1" {
		where = append(where, `r.status = 'posted' AND r.amount > coalesce((SELECT sum(a.amount) FROM receipt_allocations a WHERE a.receipt_id = r.id AND a.reversed_at IS NULL), 0)`)
	}
	rows, err := h.Pool.Query(ctx, receiptSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY r.received_on DESC, r.number DESC`, args...)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	all := []*Receipt{}
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			response.Fail(c, err)
			return
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	start := min((page-1)*size, len(all))
	end := min(start+size, len(all))
	response.List(c, all[start:end], page, size, len(all))
}

type allocateInput struct {
	Allocations []struct {
		MilestoneID string `json:"milestone_id"`
		Amount      string `json:"amount"`
	} `json:"allocations"`
}

// Allocate handles POST /receipts/:id/allocations (idempotent). Each amount goes to a
// collectable milestone of the same customer and currency, never above what is still
// open or what is left on the receipt; the rest stays unallocated (AC-08).
func (h *Handlers) Allocate(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in allocateInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	if len(in.Allocations) == 0 {
		errs.Add("allocations", "required", map[string]any{"min": 1})
	}
	type want struct {
		milestone uuid.UUID
		amount    decimal.Decimal
	}
	wants := make([]want, len(in.Allocations))
	seen := map[uuid.UUID]bool{}
	for i, a := range in.Allocations {
		f := func(n string) string { return "allocations[" + itoa(i) + "]." + n }
		wants[i] = want{httpx.UUID(&errs, f("milestone_id"), a.MilestoneID), decimalx.Positive(&errs, f("amount"), a.Amount).Round(2)}
		if seen[wants[i].milestone] {
			errs.Add(f("milestone_id"), "duplicate", nil)
		}
		seen[wants[i].milestone] = true
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "receipts.allocate", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		// Lock the receipt first, then milestones in id order, so concurrent allocations serialize.
		if err := tx.QueryRow(ctx, `SELECT id FROM receipts WHERE id = $1 FOR UPDATE`, id).Scan(new(uuid.UUID)); err != nil {
			return nil, err
		}
		// A lock-waiting SELECT retains its original statement snapshot. Read sums
		// in a new statement after acquiring the lock, not in that SELECT.
		r, err := scanReceipt(tx.QueryRow(ctx, receiptSelect+` WHERE r.id = $1`, id))
		if err != nil {
			return nil, err
		}
		if r.Status != "posted" {
			return nil, apperrors.Rule("receipt_cancelled", "This receipt was cancelled.", nil)
		}
		total := decimal.Zero
		for _, w := range wants {
			total = total.Add(w.amount)
		}
		if total.GreaterThan(r.Unallocated) {
			return nil, apperrors.Rule("allocation_exceeds_receipt", "The amounts are more than what is left on this receipt.",
				map[string]any{"left": r.Unallocated.String(), "requested": total.String()})
		}
		ids := make([]uuid.UUID, len(wants))
		for i, w := range wants {
			ids[i] = w.milestone
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM payment_milestones WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids); err != nil {
			return nil, err
		}
		ms, err := h.loadMilestones(ctx, tx, `m.id = ANY($1)`, true, ids)
		if err != nil {
			return nil, err
		}
		byID := map[uuid.UUID]Milestone{}
		for _, m := range ms {
			byID[m.ID] = m
		}
		for i, w := range wants {
			m, ok := byID[w.milestone]
			field := "allocations[" + itoa(i) + "]"
			switch {
			case !ok || m.CustomerID != r.CustomerID:
				return nil, apperrors.Invalid(field+".milestone_id", "other_customer", nil)
			case m.Currency != r.Currency:
				return nil, apperrors.Invalid(field+".milestone_id", "other_currency", map[string]any{"currency": m.Currency})
			case m.Status == "wait":
				return nil, apperrors.Invalid(field+".milestone_id", "not_collectable", nil)
			case w.amount.GreaterThan(*m.Open):
				return nil, apperrors.Invalid(field+".amount", "exceeds_open", map[string]any{"open": m.Open.String()})
			}
			if _, err := tx.Exec(ctx, `INSERT INTO receipt_allocations (receipt_id, milestone_id, amount, created_by) VALUES ($1, $2, $3, $4)`, id, w.milestone, w.amount, p.UserID); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "allocate", "receipt", id, map[string]any{"total": total.String(), "lines": len(wants)}); err != nil {
			return nil, err
		}
		return h.loadReceipt(ctx, tx, id, true)
	})
}

type reverseInput struct {
	Reason string `json:"reason"`
}

// ReverseAllocation handles POST /allocations/:id/reverse: a confirmed allocation is
// never edited; it is reversed with a reason and the money is free again.
func (h *Handlers) ReverseAllocation(c *gin.Context) {
	p := auth.Principal(c)
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
	if in.Reason == "" || len([]rune(in.Reason)) > 300 {
		response.Fail(c, apperrors.Invalid("reason", "required", nil))
		return
	}
	httpx.Command(c, h.Pool, "allocations.reverse", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var receiptID uuid.UUID
		var reversed *time.Time
		if err := tx.QueryRow(ctx, `SELECT receipt_id, reversed_at FROM receipt_allocations WHERE id = $1 FOR UPDATE`, id).Scan(&receiptID, &reversed); err != nil {
			return nil, err
		}
		if reversed != nil {
			return nil, apperrors.Rule("allocation_reversed", "This allocation was already reversed.", nil)
		}
		if _, err := tx.Exec(ctx, `UPDATE receipt_allocations SET reversed_at = now(), reversed_by = $2, reverse_reason = $3 WHERE id = $1`, id, p.UserID, in.Reason); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "reverse", "receipt_allocation", id, map[string]any{"reason": in.Reason}); err != nil {
			return nil, err
		}
		return h.loadReceipt(ctx, tx, receiptID, true)
	})
}

func itoa(i int) string { return decimal.NewFromInt(int64(i)).String() }
