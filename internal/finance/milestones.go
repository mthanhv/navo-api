// Package finance records money in Navo first (decision 1.4): payment milestones of a
// contract, sales and purchase invoices, receipts and payments, and the allocation
// of received money to what is owed (REQUIREMENTS N-07; mockup Công nợ).
package finance

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// Handlers serve the finance endpoints.
type Handlers struct {
	Pool *pgxpool.Pool
	TZ   *time.Location
}

func (h *Handlers) today() time.Time {
	n := time.Now().In(h.TZ)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func dateStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

// Milestone is one payment term of a contract and, once it has happened, an amount owed.
type Milestone struct {
	ID             uuid.UUID        `json:"id"`
	ContractID     uuid.UUID        `json:"contract_id"`
	ContractNumber string           `json:"contract_number"`
	CustomerID     uuid.UUID        `json:"customer_id"`
	CustomerName   string           `json:"customer_name"`
	Seq            int              `json:"seq"`
	Version        int64            `json:"version"`
	Name           string           `json:"name"`
	Percent        *decimal.Decimal `json:"percent"`
	Amount         decimal.Decimal  `json:"amount"`
	Currency       string           `json:"currency"`
	Trigger        string           `json:"trigger"`
	DaysAfter      int              `json:"days_after"`
	FixedDueOn     *string          `json:"fixed_due_on"`
	// TriggeredOn is when the event happened (signing, last delivery, acceptance…); nil = not yet.
	TriggeredOn *string `json:"triggered_on"`
	TriggerNote *string `json:"trigger_note"`
	// AcceptanceID is the acceptance document that made an "after acceptance" term due.
	AcceptanceID *uuid.UUID `json:"acceptance_id"`
	// Source names the event: "DO-26-0129-01" for full delivery, the note for acceptance.
	Source string           `json:"source"`
	DueOn  *string          `json:"due_on"`
	Paid   *decimal.Decimal `json:"paid,omitempty"`
	Open   *decimal.Decimal `json:"open,omitempty"`
	// Status: wait (not collectable yet), later (due in more than 30 days), soon, late, paid.
	Status   string `json:"status"`
	DaysLate int    `json:"days_late,omitempty"`
}

// milestoneSQL loads milestones with what decides their trigger date: the contract's
// signing date and, for "after full delivery", whether every line is delivered and the last delivery.
const milestoneSQL = `
SELECT m.id, m.contract_id, c.number, c.customer_id, cu.name, m.seq, m.version, m.name, m.percent::text, m.amount::text, c.currency, m.trigger,
       m.days_after, m.fixed_due_on, m.triggered_on, m.trigger_note, m.acceptance_id, c.signed_on,
       NOT EXISTS (SELECT 1 FROM contract_lines cl WHERE cl.contract_id = c.id AND cl.quantity > line_delivered(cl.id)) AS fully_delivered,
       (SELECT d.number || ' ' || d.delivered_on::text FROM delivery_orders d WHERE d.contract_id = c.id AND d.status = 'confirmed'
        ORDER BY d.delivered_on DESC, d.number DESC LIMIT 1) AS last_delivery,
       coalesce((SELECT sum(a.amount) FROM receipt_allocations a WHERE a.milestone_id = m.id AND a.reversed_at IS NULL), 0)::text
FROM payment_milestones m JOIN contracts c ON c.id = m.contract_id JOIN organizations cu ON cu.id = c.customer_id`

func (h *Handlers) loadMilestones(ctx context.Context, q db.Querier, where string, withMoney bool, args ...any) ([]Milestone, error) {
	rows, err := q.Query(ctx, milestoneSQL+` WHERE `+where+` ORDER BY c.number, m.seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	today := h.today()
	out := []Milestone{}
	for rows.Next() {
		var m Milestone
		var number *string
		var percent *string
		var amount, paid string
		var fixed, triggered, signed *time.Time
		var fullyDelivered bool
		var lastDelivery *string
		if err := rows.Scan(&m.ID, &m.ContractID, &number, &m.CustomerID, &m.CustomerName, &m.Seq, &m.Version, &m.Name, &percent, &amount, &m.Currency, &m.Trigger,
			&m.DaysAfter, &fixed, &triggered, &m.TriggerNote, &m.AcceptanceID, &signed, &fullyDelivered, &lastDelivery, &paid); err != nil {
			return nil, err
		}
		if number != nil {
			m.ContractNumber = *number
		}
		if percent != nil {
			p := decimalx.MustParse(*percent)
			m.Percent = &p
		}
		m.Amount = decimalx.MustParse(amount)
		m.FixedDueOn = dateStr(fixed)
		// When did the trigger happen?
		var on *time.Time
		switch m.Trigger {
		case "signing", "date":
			on = signed
			m.Source = "signing"
		case "delivery_complete":
			if fullyDelivered && lastDelivery != nil {
				num, d, _ := strings.Cut(*lastDelivery, " ")
				if t, err := time.Parse(time.DateOnly, d); err == nil {
					on = &t
					m.Source = num
				}
			}
		default:
			on = triggered
			if m.TriggerNote != nil {
				m.Source = *m.TriggerNote
			}
		}
		m.TriggeredOn = dateStr(on)
		var due *time.Time
		if m.Trigger == "date" {
			due = fixed
		} else if on != nil {
			d := on.AddDate(0, 0, m.DaysAfter)
			due = &d
		}
		m.DueOn = dateStr(due)
		paidD := decimalx.MustParse(paid)
		open := decimal.Max(m.Amount.Sub(paidD), decimal.Zero)
		switch {
		case open.IsZero():
			m.Status = "paid"
		case on == nil:
			m.Status = "wait"
		case due.Before(today):
			m.Status = "late"
			m.DaysLate = int(today.Sub(*due).Hours() / 24)
		case !due.After(today.AddDate(0, 0, 30)):
			m.Status = "soon"
		default:
			m.Status = "later"
		}
		if withMoney {
			m.Paid, m.Open = &paidD, &open
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ContractMilestones handles GET /contracts/:id/milestones. Amounts paid are shown
// only with finance access.
func (h *Handlers) ContractMilestones(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.Pool.QueryRow(c.Request.Context(), `SELECT 1 FROM contracts WHERE id = $1 AND ($2::uuid IS NULL OR owner_id = $2)`, id, p.OwnerFilter()).Scan(new(int)); err != nil {
		response.Fail(c, err)
		return
	}
	ms, err := h.loadMilestones(c.Request.Context(), h.Pool, `m.contract_id = $1`, p.Can(access.FinanceRead), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, ms)
}

type milestoneInput struct {
	Version    *int64  `json:"version"`
	Name       string  `json:"name"`
	Percent    *string `json:"percent"`
	Amount     string  `json:"amount"`
	Trigger    string  `json:"trigger"`
	DaysAfter  int     `json:"days_after"`
	FixedDueOn *string `json:"fixed_due_on"`
}

// DraftTerm is a payment term saved atomically with a new contract draft.
type DraftTerm = milestoneInput

// CreateDraftTerms uses the same validation as manually entered terms. Amounts
// are explicit; percentages are descriptive until the commercial basis is agreed.
func CreateDraftTerms(ctx context.Context, tx pgx.Tx, contractID, actor uuid.UUID, terms []DraftTerm) error {
	for i, term := range terms {
		pct, amount, due, err := term.validate()
		if err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO payment_milestones (contract_id, seq, name, percent, amount, trigger, days_after, fixed_due_on, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, contractID, i+1, term.Name, pct, amount, term.Trigger, term.DaysAfter, due, actor).Scan(&id); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, "create", "payment_milestone", id, map[string]any{"name": term.Name, "amount": amount.String(), "trigger": term.Trigger}); err != nil {
			return err
		}
	}
	return nil
}

func (in *milestoneInput) validate() (*decimal.Decimal, decimal.Decimal, *time.Time, error) {
	var errs apperrors.FieldErrors
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		errs.Add("name", "required", nil)
	} else if len([]rune(in.Name)) > 200 {
		errs.Add("name", "too_long", map[string]any{"max": 200})
	}
	var pct *decimal.Decimal
	if in.Percent != nil && *in.Percent != "" {
		v := decimalx.Positive(&errs, "percent", *in.Percent)
		if v.GreaterThan(decimal.NewFromInt(100)) {
			errs.Add("percent", "less_or_equal", map[string]any{"max": "100"})
		}
		pct = &v
	}
	amount := decimalx.Positive(&errs, "amount", in.Amount)
	if amount.IsPositive() && !amount.Round(2).IsPositive() {
		errs.Add("amount", "positive", nil)
	}
	switch in.Trigger {
	case "signing", "delivery_complete", "acceptance", "date", "manual":
	default:
		errs.Add("trigger", "one_of", map[string]any{"values": []string{"signing", "delivery_complete", "acceptance", "date", "manual"}})
	}
	if in.DaysAfter < 0 || in.DaysAfter > 3650 {
		errs.Add("days_after", "range", map[string]any{"min": 0, "max": 3650})
	}
	due := httpx.Date(&errs, "fixed_due_on", in.FixedDueOn)
	if in.Trigger == "date" && due == nil {
		errs.Add("fixed_due_on", "required", nil)
	}
	if in.Trigger != "date" {
		due = nil
	}
	return pct, amount.Round(2), due, errs.Err()
}

func canEditTerms(p *access.Principal) bool {
	return p.Can(access.ContractsWrite) || p.Can(access.FinanceWrite)
}

// CreateMilestone handles POST /contracts/:id/milestones.
func (h *Handlers) CreateMilestone(c *gin.Context) {
	p := auth.Principal(c)
	if !canEditTerms(p) {
		response.Fail(c, apperrors.Forbidden("payment_terms"))
		return
	}
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in milestoneInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	pct, amount, due, err := in.validate()
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "milestones.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id = $1 AND ($2::uuid IS NULL OR owner_id = $2) FOR UPDATE`, id, scopeFor(p)).Scan(&status); err != nil {
			return nil, err
		}
		if status == "cancelled" || status == "closed" {
			return nil, apperrors.Rule("contract_not_open", "Payment terms can be changed only on draft or active contracts.", nil)
		}
		var mid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO payment_milestones (contract_id, seq, name, percent, amount, trigger, days_after, fixed_due_on, created_by)
			VALUES ($1, coalesce((SELECT max(seq) FROM payment_milestones WHERE contract_id = $1), 0) + 1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			id, in.Name, pct, amount, in.Trigger, in.DaysAfter, due, p.UserID).Scan(&mid); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "create", "payment_milestone", mid, map[string]any{"name": in.Name, "amount": amount.String(), "trigger": in.Trigger}); err != nil {
			return nil, err
		}
		ms, err := h.loadMilestones(ctx, tx, `m.id = $1`, p.Can(access.FinanceRead), mid)
		if err != nil || len(ms) == 0 {
			return nil, err
		}
		return ms[0], nil
	})
}

// scopeFor lets finance staff manage terms of any contract; others only their own.
func scopeFor(p *access.Principal) *uuid.UUID {
	if p.Can(access.FinanceWrite) {
		return nil
	}
	return p.OwnerFilter()
}

// UpdateMilestone handles PATCH /milestones/:id; the amount cannot go below what is already paid.
func (h *Handlers) UpdateMilestone(c *gin.Context) {
	p := auth.Principal(c)
	if !canEditTerms(p) {
		response.Fail(c, apperrors.Forbidden("payment_terms"))
		return
	}
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in milestoneInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	pct, amount, due, err := in.validate()
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "milestones.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var version int64
		var status string
		if err := tx.QueryRow(ctx, `SELECT m.version,c.status FROM payment_milestones m JOIN contracts c ON c.id=m.contract_id WHERE m.id=$1 AND ($2::uuid IS NULL OR c.owner_id=$2) FOR UPDATE OF m`, id, scopeFor(p)).Scan(&version, &status); err != nil {
			return nil, err
		}
		if status == "closed" || status == "cancelled" {
			return nil, apperrors.Rule("contract_not_open", "This contract's terms cannot change.", nil)
		}
		if in.Version == nil || *in.Version != version {
			return nil, apperrors.Conflict("version_conflict", "This payment term changed. Reload it before saving.", nil)
		}
		var paid string
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT sum(a.amount) FROM receipt_allocations a WHERE a.milestone_id = m.id AND a.reversed_at IS NULL), 0)::text
			FROM payment_milestones m JOIN contracts c ON c.id = m.contract_id WHERE m.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, scopeFor(p)).Scan(&paid); err != nil {
			return nil, err
		}
		if amount.LessThan(decimalx.MustParse(paid)) {
			return nil, apperrors.Invalid("amount", "below_paid", map[string]any{"paid": paid})
		}
		var triggered bool
		if err := tx.QueryRow(ctx, `SELECT triggered_on IS NOT NULL OR EXISTS(SELECT 1 FROM sales_invoices WHERE milestone_id=m.id) OR EXISTS(SELECT 1 FROM receipt_allocations WHERE milestone_id=m.id) FROM payment_milestones m WHERE id=$1`, id).Scan(&triggered); err != nil {
			return nil, err
		}
		if triggered {
			return nil, apperrors.Rule("milestone_in_use", "Correct linked events and money before changing this term.", nil)
		}
		if err := recordTermHistory(ctx, tx, id, p.UserID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_milestones SET name = $2, percent = $3, amount = $4, trigger = $5, days_after = $6, fixed_due_on = $7,
				triggered_on = CASE WHEN $5 IN ('acceptance', 'manual') THEN triggered_on END, version = version + 1 WHERE id = $1`,
			id, in.Name, pct, amount, in.Trigger, in.DaysAfter, due); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "update", "payment_milestone", id, map[string]any{"name": in.Name, "amount": amount.String(), "trigger": in.Trigger}); err != nil {
			return nil, err
		}
		ms, err := h.loadMilestones(ctx, tx, `m.id = $1`, p.Can(access.FinanceRead), id)
		if err != nil || len(ms) == 0 {
			return nil, err
		}
		return ms[0], nil
	})
}

// DeleteMilestone handles DELETE /milestones/:id while nothing was paid against it.
func (h *Handlers) DeleteMilestone(c *gin.Context) {
	p := auth.Principal(c)
	if !canEditTerms(p) {
		response.Fail(c, apperrors.Forbidden("payment_terms"))
		return
	}
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "milestones.delete", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		if err := tx.QueryRow(ctx, `SELECT c.status FROM payment_milestones m JOIN contracts c ON c.id=m.contract_id WHERE m.id=$1 AND ($2::uuid IS NULL OR c.owner_id=$2) FOR UPDATE OF m`, id, scopeFor(p)).Scan(&status); err != nil {
			return nil, err
		}
		if status == "closed" || status == "cancelled" {
			return nil, apperrors.Rule("contract_not_open", "This contract's terms cannot change.", nil)
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT m.triggered_on IS NOT NULL OR EXISTS(SELECT 1 FROM payment_term_history WHERE milestone_id=m.id) OR EXISTS (SELECT 1 FROM receipt_allocations WHERE milestone_id = m.id) OR EXISTS (SELECT 1 FROM sales_invoices WHERE milestone_id = m.id)
			FROM payment_milestones m JOIN contracts c ON c.id = m.contract_id WHERE m.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF m`, id, scopeFor(p)).Scan(&used); err != nil {
			return nil, err
		}
		if used {
			return nil, apperrors.Rule("milestone_in_use", "Money or an invoice is already linked to this milestone.", nil)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM payment_milestones WHERE id = $1`, id); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "deleted": true}, audit.Record(ctx, tx, "delete", "payment_milestone", id, nil)
	})
}

type triggerInput struct {
	On   *string `json:"on"`
	Note *string `json:"note"`
}

// TriggerMilestone handles POST /milestones/:id/trigger: records that acceptance (or
// another manual event) happened, so the amount becomes collectable.
func (h *Handlers) TriggerMilestone(c *gin.Context) {
	p := auth.Principal(c)
	if !canEditTerms(p) {
		response.Fail(c, apperrors.Forbidden("payment_terms"))
		return
	}
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in triggerInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	on := httpx.Date(&errs, "on", in.On)
	if on == nil && len(errs) == 0 {
		errs.Add("on", "required", nil)
	}
	if on != nil && on.After(h.today()) {
		errs.Add("on", "not_future", nil)
	}
	note := ""
	if in.Note != nil {
		note = strings.TrimSpace(*in.Note)
	}
	if note == "" {
		errs.Add("note", "required", nil)
	}
	if len([]rune(note)) > 200 {
		errs.Add("note", "too_long", map[string]any{"max": 200})
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "milestones.trigger", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var contractID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT m.contract_id FROM payment_milestones m JOIN contracts c ON c.id=m.contract_id WHERE m.id=$1 AND c.status='active' AND ($2::uuid IS NULL OR c.owner_id=$2)`, id, scopeFor(p)).Scan(&contractID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM contract_lines WHERE contract_id=$1 ORDER BY id FOR UPDATE`, contractID); err != nil {
			return nil, err
		}
		var trigger string
		if err := tx.QueryRow(ctx, `SELECT m.trigger FROM payment_milestones m JOIN contracts c ON c.id = m.contract_id
			WHERE m.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF m`, id, scopeFor(p)).Scan(&trigger); err != nil {
			return nil, err
		}
		if trigger != "acceptance" && trigger != "manual" {
			return nil, apperrors.Rule("milestone_automatic", "This milestone starts automatically (signing, full delivery or a fixed date).", nil)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT triggered_on IS NOT NULL FROM payment_milestones WHERE id=$1`, id).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, apperrors.Rule("milestone_already_triggered", "Correct the existing event before recording a new one.", nil)
		}
		if trigger == "acceptance" {
			// Acceptance terms fall due from acceptance documents when every line is fully
			// accepted (owner decision 2026-11-11); there is no second, hand-entered path.
			return nil, apperrors.Rule("acceptance_from_documents", "Record the customer's acceptance on the contract; this term falls due when every line is fully accepted.", nil)
		}
		if err := recordTermHistory(ctx, tx, id, p.UserID); err != nil {
			return nil, err
		}
		var noteArg *string
		if note != "" {
			noteArg = &note
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_milestones SET triggered_on = $2, trigger_note = $3, version = version + 1 WHERE id = $1`, id, *on, noteArg); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "trigger", "payment_milestone", id, map[string]any{"on": on.Format(time.DateOnly), "note": note}); err != nil {
			return nil, err
		}
		ms, err := h.loadMilestones(ctx, tx, `m.id = $1`, p.Can(access.FinanceRead), id)
		if err != nil || len(ms) == 0 {
			return nil, err
		}
		return ms[0], nil
	})
}

// CurrencySummary is one row of "Tổng quan theo loại tiền": currencies are never added up.
type CurrencySummary struct {
	Currency    string          `json:"currency"`
	Uncollected decimal.Decimal `json:"uncollected"`
	DueOpen     decimal.Decimal `json:"due_open"`
	Overdue     decimal.Decimal `json:"overdue"`
	Unallocated decimal.Decimal `json:"unallocated"`
}

// Receivables handles GET /receivables?tab=late|soon|wait|all&customer_id=: what customers
// owe, by milestone, with per-tab counts and the per-currency summary.
func (h *Handlers) Receivables(c *gin.Context) {
	ctx := c.Request.Context()
	tab := c.DefaultQuery("tab", "late")
	where := `c.status IN ('active', 'closed')`
	args := []any{}
	if cust := c.Query("customer_id"); cust != "" {
		id, err := uuid.Parse(cust)
		if err != nil {
			response.Fail(c, apperrors.Invalid("customer_id", "uuid", nil))
			return
		}
		where += ` AND c.customer_id = $1`
		args = append(args, id)
	}
	ms, err := h.loadMilestones(ctx, h.Pool, where, true, args...)
	if err != nil {
		response.Fail(c, err)
		return
	}
	counts := map[string]int{"late": 0, "soon": 0, "wait": 0, "all": len(ms)}
	sums := map[string]*CurrencySummary{}
	sum := func(cur string) *CurrencySummary {
		if sums[cur] == nil {
			sums[cur] = &CurrencySummary{Currency: cur}
		}
		return sums[cur]
	}
	items := []Milestone{}
	for _, m := range ms {
		if m.Status == "late" || m.Status == "soon" || m.Status == "wait" {
			counts[m.Status]++
		}
		s := sum(m.Currency)
		if m.Status != "wait" {
			s.DueOpen = s.DueOpen.Add(*m.Open)
		}
		if m.Status == "late" {
			s.Overdue = s.Overdue.Add(*m.Open)
		}
		if tab == "all" || m.Status == tab {
			items = append(items, m)
		}
	}
	// Contract value not yet collected (active contracts), and money received but not allocated.
	vrows, err := h.Pool.Query(ctx, `SELECT c.currency, sum(v.value - coalesce(p.paid, 0))::text FROM contracts c
		JOIN LATERAL (SELECT coalesce(sum(round(cl.quantity * cl.unit_price, 2)), 0) AS value FROM contract_lines cl WHERE cl.contract_id = c.id) v ON true
		LEFT JOIN LATERAL (SELECT sum(a.amount) AS paid FROM receipt_allocations a JOIN payment_milestones m ON m.id = a.milestone_id
			WHERE m.contract_id = c.id AND a.reversed_at IS NULL) p ON true
		WHERE c.status IN ('active', 'closed') GROUP BY c.currency`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	for vrows.Next() {
		var cur, v string
		if err := vrows.Scan(&cur, &v); err != nil {
			vrows.Close()
			response.Fail(c, err)
			return
		}
		sum(strings.TrimSpace(cur)).Uncollected = decimalx.MustParse(v)
	}
	vrows.Close()
	urows, err := h.Pool.Query(ctx, `SELECT r.currency, sum(r.amount - coalesce((SELECT sum(a.amount) FROM receipt_allocations a WHERE a.receipt_id = r.id AND a.reversed_at IS NULL), 0))::text
		FROM receipts r WHERE r.status = 'posted' GROUP BY r.currency`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	for urows.Next() {
		var cur, v string
		if err := urows.Scan(&cur, &v); err != nil {
			urows.Close()
			response.Fail(c, err)
			return
		}
		sum(strings.TrimSpace(cur)).Unallocated = decimalx.MustParse(v)
	}
	urows.Close()
	summary := []CurrencySummary{}
	for _, s := range sums {
		summary = append(summary, *s)
	}
	sort.Slice(summary, func(i, j int) bool {
		if (summary[i].Currency == "VND") != (summary[j].Currency == "VND") {
			return summary[i].Currency == "VND"
		}
		return summary[i].Currency < summary[j].Currency
	})
	rank := map[string]int{"late": 0, "soon": 1, "later": 2, "wait": 3, "paid": 4}
	sort.SliceStable(items, func(i, j int) bool {
		if rank[items[i].Status] != rank[items[j].Status] {
			return rank[items[i].Status] < rank[items[j].Status]
		}
		a, b := "9999", "9999"
		if items[i].DueOn != nil {
			a = *items[i].DueOn
		}
		if items[j].DueOn != nil {
			b = *items[j].DueOn
		}
		return a < b
	})
	response.OK(c, gin.H{"items": items, "counts": counts, "summary": summary, "as_of": time.Now()})
}

func recordTermHistory(ctx context.Context, tx pgx.Tx, id, actor uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO payment_term_history(milestone_id,version,snapshot,changed_by) SELECT id,version,to_jsonb(m),$2 FROM payment_milestones m WHERE id=$1 ON CONFLICT DO NOTHING`, id, actor)
	return err
}

// ResetMilestone corrects a manual event without erasing its historical date/source.
func (h *Handlers) ResetMilestone(c *gin.Context) {
	p := auth.Principal(c)
	if !canEditTerms(p) {
		response.Fail(c, apperrors.Forbidden("payment_terms"))
		return
	}
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
	httpx.Command(c, h.Pool, "milestones.reset", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var cid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT m.contract_id FROM payment_milestones m JOIN contracts c ON c.id=m.contract_id WHERE m.id=$1 AND c.status='active' AND ($2::uuid IS NULL OR c.owner_id=$2)`, id, scopeFor(p)).Scan(&cid); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM contract_lines WHERE contract_id=$1 ORDER BY id FOR UPDATE`, cid); err != nil {
			return nil, err
		}
		var event bool
		var fromAcceptance bool
		if err := tx.QueryRow(ctx, `SELECT triggered_on IS NOT NULL, acceptance_id IS NOT NULL FROM payment_milestones WHERE id=$1 FOR UPDATE`, id).Scan(&event, &fromAcceptance); err != nil {
			return nil, err
		}
		if !event {
			return nil, apperrors.Rule("milestone_not_triggered", "There is no recorded event to correct.", nil)
		}
		if fromAcceptance {
			return nil, apperrors.Rule("acceptance_from_documents", "This date comes from an acceptance document; reverse that document to correct it.", nil)
		}
		var dependent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM receipt_allocations WHERE milestone_id=$1 AND reversed_at IS NULL)`, id).Scan(&dependent); err != nil {
			return nil, err
		}
		if dependent {
			return nil, apperrors.Rule("reversal_has_dependents", "Reverse allocations before correcting this event.", nil)
		}
		if err := recordTermHistory(ctx, tx, id, p.UserID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_milestones SET triggered_on=NULL,trigger_note=NULL,version=version+1 WHERE id=$1`, id); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "reset": true}, audit.Record(ctx, tx, "reset_event", "payment_milestone", id, map[string]any{"reason": in.Reason})
	})
}
