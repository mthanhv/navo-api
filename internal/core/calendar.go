package core

import (
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/auth"
	"navo-api/internal/finance"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// CalendarItem is one dated thing on a contract's row of the delivery calendar: a purchase order
// (a bar from ordering to the last expected arrival), a delivery (a dot on its planned or delivered
// date) or the contract's final due date. State: done, late, firm (planned and on time) or plan
// (tentative or draft).
type CalendarItem struct {
	// Kind: po and do (the overview row), due and signed (contract dates), gr, ac and pay
	// (goods received, acceptances and payment terms: shown when the contract is expanded).
	Kind   string    `json:"kind"`
	ID     uuid.UUID `json:"id"`
	Number string    `json:"number"`
	Start  string    `json:"start_on"`
	End    string    `json:"end_on"`
	State  string    `json:"state"`
	Detail string    `json:"detail"`
}

// CalendarRow is one contract with its items in the period.
type CalendarRow struct {
	ContractID   uuid.UUID      `json:"contract_id"`
	Number       string         `json:"contract_number"`
	CustomerName string         `json:"customer_name"`
	OwnerName    string         `json:"owner_name"`
	FinalDueOn   *string        `json:"final_due_on"`
	Late         int            `json:"late"`
	Items        []CalendarItem `json:"items"`
	createdAt    time.Time
}

// ReportCalendar handles GET /reports/calendar?from=&to=&customer_id=&owner_id=&late=1&show=po,do:
// purchase orders and deliveries of active and closed contracts over a period (at most a year),
// one row per contract, newest contract first. Sales see their own contracts.
func (h *Handlers) ReportCalendar(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	today := h.today()
	var errs apperrors.FieldErrors
	str := func(k string) *string { v := c.Query(k); return &v }
	from, to := httpx.Date(&errs, "from", str("from")), httpx.Date(&errs, "to", str("to"))
	if from == nil {
		f := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		from = &f
	}
	if to == nil {
		t := from.AddDate(0, 3, -1)
		to = &t
	}
	if to.Before(*from) {
		errs.Add("to", "after_start", nil)
	} else if to.Sub(*from) > 366*24*time.Hour {
		errs.Add("to", "range", map[string]any{"min": 1, "max": 366})
	}
	customer := optionalUUID(&errs, "customer_id", c.Query("customer_id"))
	owner := optionalUUID(&errs, "owner_id", c.Query("owner_id"))
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	show := map[string]bool{"po": true, "do": true}
	if v := c.Query("show"); v != "" {
		show = map[string]bool{}
		for _, k := range strings.Split(v, ",") {
			show[strings.TrimSpace(k)] = true
		}
	}
	rows := map[uuid.UUID]*CalendarRow{}
	scope := `c.status IN ('active', 'closed') AND ($1::uuid IS NULL OR c.owner_id = $1) AND ($2::uuid IS NULL OR c.customer_id = $2) AND ($3::uuid IS NULL OR c.owner_id = $3)`
	base := []any{p.OwnerFilter(), customer, owner}
	period := append(append([]any{}, base...), *from, *to) // $4, $5

	// Contracts in scope that have anything in the period, with their final due date.
	crows, err := h.Pool.Query(ctx, `SELECT c.id, c.number, o.name, u.full_name, (SELECT max(cl.due_on) FROM contract_lines cl WHERE cl.contract_id = c.id), c.created_at, c.signed_on
		FROM contracts c JOIN organizations o ON o.id = c.customer_id JOIN users u ON u.id = c.owner_id WHERE `+scope, base...)
	if err != nil {
		response.Fail(c, err)
		return
	}
	for crows.Next() {
		r := &CalendarRow{Items: []CalendarItem{}}
		var due, signed *time.Time
		if err := crows.Scan(&r.ContractID, &r.Number, &r.CustomerName, &r.OwnerName, &due, &r.createdAt, &signed); err != nil {
			crows.Close()
			response.Fail(c, err)
			return
		}
		r.FinalDueOn = dateString(due)
		if due != nil && !due.Before(*from) && !due.After(*to) {
			r.Items = append(r.Items, CalendarItem{Kind: "due", ID: r.ContractID, Number: r.Number, Start: *r.FinalDueOn, End: *r.FinalDueOn, State: "firm"})
		}
		if signed != nil && !signed.Before(*from) && !signed.After(*to) {
			on := signed.Format(time.DateOnly)
			r.Items = append(r.Items, CalendarItem{Kind: "signed", ID: r.ContractID, Number: r.Number, Start: on, End: on, State: "done"})
		}
		rows[r.ContractID] = r
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		response.Fail(c, err)
		return
	}

	if show["po"] {
		// A PO runs from ordering (confirmation, else creation) to its last expected arrival.
		prows, err := h.Pool.Query(ctx, `SELECT po.id, po.number, po.contract_id, po.status, s.name, coalesce(po.confirmed_at, po.created_at)::date,
				max(pol.expected_on),
				bool_and(po_line_received(pol.id) >= pol.quantity - pol.cancelled_quantity),
				bool_or(pol.expected_on < $6::date AND po_line_received(pol.id) < pol.quantity - pol.cancelled_quantity)
			FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id JOIN organizations s ON s.id = po.supplier_id
			JOIN purchase_order_lines pol ON pol.purchase_order_id = po.id
			WHERE `+scope+` AND po.status <> 'cancelled'
			GROUP BY po.id, s.name
			HAVING coalesce(po.confirmed_at, po.created_at)::date <= $5::date AND coalesce(max(pol.expected_on), coalesce(po.confirmed_at, po.created_at)::date) >= $4::date`,
			append(period, today)...)
		if err != nil {
			response.Fail(c, err)
			return
		}
		for prows.Next() {
			var it CalendarItem
			var contractID uuid.UUID
			var status, supplier string
			var start time.Time
			var end *time.Time
			var full, late *bool
			if err := prows.Scan(&it.ID, &it.Number, &contractID, &status, &supplier, &start, &end, &full, &late); err != nil {
				prows.Close()
				response.Fail(c, err)
				return
			}
			it.Kind, it.Detail = "po", supplier
			it.Start = start.Format(time.DateOnly)
			it.End = it.Start
			if end != nil && end.After(start) {
				it.End = end.Format(time.DateOnly)
			}
			switch {
			case status == "draft":
				it.State = "plan"
			case full != nil && *full:
				it.State = "done"
			case late != nil && *late:
				it.State = "late"
			default:
				it.State = "firm"
			}
			if r := rows[contractID]; r != nil {
				r.Items = append(r.Items, it)
			}
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			response.Fail(c, err)
			return
		}
	}

	if show["do"] {
		// A delivery is a dot on its delivered date, else its planned date (undated plans are not on a calendar).
		drows, err := h.Pool.Query(ctx, `SELECT d.id, d.number, d.contract_id, d.status, d.firmness, coalesce(d.delivered_on, d.planned_on),
				(SELECT string_agg(cl.line_no::text || ': ' || trim_scale(dol.quantity)::text || ' ' || cl.unit, ' · ' ORDER BY cl.line_no)
				 FROM delivery_order_lines dol JOIN contract_lines cl ON cl.id = dol.contract_line_id WHERE dol.delivery_order_id = d.id)
			FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id
			WHERE `+scope+` AND d.status IN ('draft', 'confirmed') AND coalesce(d.delivered_on, d.planned_on) BETWEEN $4::date AND $5::date`, period...)
		if err != nil {
			response.Fail(c, err)
			return
		}
		for drows.Next() {
			var it CalendarItem
			var contractID uuid.UUID
			var status, firmness string
			var on time.Time
			var detail *string
			if err := drows.Scan(&it.ID, &it.Number, &contractID, &status, &firmness, &on, &detail); err != nil {
				drows.Close()
				response.Fail(c, err)
				return
			}
			it.Kind = "do"
			it.Start = on.Format(time.DateOnly)
			it.End = it.Start
			if detail != nil {
				it.Detail = *detail
			}
			switch {
			case status == "confirmed":
				it.State = "done"
			case firmness == "firm" && on.Before(today):
				it.State = "late" // a firm promise that has passed (a tentative one is not overdue)
			case firmness == "firm":
				it.State = "firm"
			default:
				it.State = "plan"
			}
			if r := rows[contractID]; r != nil {
				r.Items = append(r.Items, it)
			}
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			response.Fail(c, err)
			return
		}
	}

	if err := h.calendarMilestones(c, rows, p.Can(access.FinanceRead), *from, *to); err != nil {
		response.Fail(c, err)
		return
	}

	out := []*CalendarRow{}
	lateOnly := c.Query("late") == "1"
	for _, r := range rows {
		docs := 0
		for _, it := range r.Items {
			if it.Kind != "due" && it.Kind != "signed" {
				docs++
			}
			if it.State == "late" {
				r.Late++
			}
		}
		if docs == 0 || (lateOnly && r.Late == 0) {
			continue
		}
		sort.Slice(r.Items, func(i, j int) bool { return r.Items[i].Start < r.Items[j].Start })
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.After(out[j].createdAt) }) // newest contract first
	response.OK(c, gin.H{"from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly), "today": today.Format(time.DateOnly), "rows": out})
}

// calendarMilestones adds what an expanded contract shows besides orders and deliveries: goods
// received, acceptances and (for people who see money) payment terms falling due in the period.
func (h *Handlers) calendarMilestones(c *gin.Context, rows map[uuid.UUID]*CalendarRow, money bool, from, to time.Time) error {
	ctx := c.Request.Context()
	if len(rows) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	add := func(contractID uuid.UUID, it CalendarItem) {
		if r := rows[contractID]; r != nil {
			r.Items = append(r.Items, it)
		}
	}
	events, err := h.Pool.Query(ctx, `SELECT 'gr', g.id, g.number, g.contract_id, g.received_on FROM goods_receipts g
			WHERE g.contract_id = ANY($1) AND g.status = 'confirmed' AND g.received_on BETWEEN $2 AND $3
		UNION ALL
		SELECT 'ac', a.id, a.number, a.contract_id, a.accepted_on FROM acceptances a
			WHERE a.contract_id = ANY($1) AND a.status = 'confirmed' AND a.accepted_on BETWEEN $2 AND $3`, ids, from, to)
	if err != nil {
		return err
	}
	for events.Next() {
		var it CalendarItem
		var contractID uuid.UUID
		var on time.Time
		if err := events.Scan(&it.Kind, &it.ID, &it.Number, &contractID, &on); err != nil {
			events.Close()
			return err
		}
		it.Start, it.End, it.State = on.Format(time.DateOnly), on.Format(time.DateOnly), "done"
		add(contractID, it)
	}
	events.Close()
	if err := events.Err(); err != nil {
		return err
	}
	if !money {
		return nil
	}
	ms, err := finance.ContractMilestones(ctx, h.Pool, h.TZ, ids, true)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.DueOn == nil || *m.DueOn < from.Format(time.DateOnly) || *m.DueOn > to.Format(time.DateOnly) {
			continue // not due yet (waiting for an event) or outside the period
		}
		st := map[string]string{"paid": "done", "late": "late", "wait": "plan"}[m.Status]
		if st == "" {
			st = "firm"
		}
		add(m.ContractID, CalendarItem{Kind: "pay", ID: m.ID, Number: m.Name, Start: *m.DueOn, End: *m.DueOn, State: st, Detail: m.Amount.String() + " " + m.Currency})
	}
	return nil
}
