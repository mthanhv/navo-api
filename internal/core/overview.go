package core

import (
	"context"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"navo-api/internal/auth"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/progress"
	"navo-api/internal/response"
)

// Summary counts a contract's lines that are fully ordered, received and delivered
// (the contract page's process bar). Lines are counted, never quantities of different units.
type Summary struct {
	Lines         int `json:"lines"`
	FullyOrdered  int `json:"fully_ordered"`
	FullyReceived int `json:"fully_received"`
	Delivered     int `json:"fully_delivered"`
}

func summarize(rows []LineRow) Summary {
	s := Summary{Lines: len(rows)}
	for _, r := range rows {
		if !r.Ordered.LessThan(r.ContractQuantity) {
			s.FullyOrdered++
		}
		if !r.Received.LessThan(r.ContractQuantity) {
			s.FullyReceived++
		}
		if !r.Remaining.IsPositive() {
			s.Delivered++
		}
	}
	return s
}

// Upcoming is a planned delivery in the coming days.
type Upcoming struct {
	ID             uuid.UUID `json:"id"`
	Number         string    `json:"number"`
	ContractID     uuid.UUID `json:"contract_id"`
	ContractNumber string    `json:"contract_number"`
	CustomerName   string    `json:"customer_name"`
	PlannedOn      string    `json:"planned_on"`
	Firmness       string    `json:"firmness"`
	Lines          int       `json:"lines"`
	// With a single line: what is delivered, so the list can say "Gioăng DN100 · 80 cái".
	ItemName *string `json:"item_name"`
	Quantity *string `json:"quantity"`
	Unit     *string `json:"unit"`
}

// Overview is the dashboard of one user's scope (REQUIREMENTS 9.2 S-01).
type Overview struct {
	AsOf             time.Time      `json:"as_of"`
	ActiveContracts  int            `json:"active_contracts"`
	LinesRemaining   int            `json:"lines_remaining"`
	POsAwaitingGoods int            `json:"purchase_orders_awaiting_goods"`
	FlagCounts       map[string]int `json:"flag_counts"`
	NeedsAction      int            `json:"needs_action"`
	// Attention lists lines needing action (critical/warning) and, after them,
	// lines still to be ordered from suppliers; most severe first, at most 200.
	Attention []LineRow  `json:"attention"`
	Upcoming  []Upcoming `json:"upcoming"`
	// LineStatus splits every line of the active contracts by its most severe flag (chart).
	LineStatus LineStatus `json:"line_status"`
	// Weekly counts deliveries made (past six weeks) and planned (this week and five ahead).
	Weekly []WeekDeliveries `json:"weekly"`
}

// LineStatus is how many contract lines are in each state; the four add up to all lines.
type LineStatus struct {
	Delivered int `json:"delivered"`
	OnTrack   int `json:"on_track"`
	Warning   int `json:"warning"`
	Critical  int `json:"critical"`
}

// WeekDeliveries is one Monday-to-Sunday week of delivery orders.
type WeekDeliveries struct {
	WeekStart string `json:"week_start"`
	Delivered int    `json:"delivered"`
	Planned   int    `json:"planned"`
}

// weekly counts confirmed deliveries by the day delivered and open ones by the day planned.
func weekly(ctx context.Context, h *Handlers, owner *uuid.UUID, today time.Time) ([]WeekDeliveries, error) {
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	first := monday.AddDate(0, 0, -6*7)
	out := make([]WeekDeliveries, 12)
	for i := range out {
		out[i].WeekStart = first.AddDate(0, 0, 7*i).Format(time.DateOnly)
	}
	rows, err := h.Pool.Query(ctx, `SELECT (coalesce(d.delivered_on, d.planned_on) - $2::date) / 7, d.status = 'confirmed', count(*)
		FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id
		WHERE d.status IN ('draft', 'confirmed') AND ($1::uuid IS NULL OR c.owner_id = $1)
		  AND coalesce(d.delivered_on, d.planned_on) >= $2::date AND coalesce(d.delivered_on, d.planned_on) < $2::date + 84
		GROUP BY 1, 2`, owner, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var week, n int
		var done bool
		if err := rows.Scan(&week, &done, &n); err != nil {
			return nil, err
		}
		if week < 0 || week >= len(out) {
			continue
		}
		if done {
			out[week].Delivered += n
		} else {
			out[week].Planned += n
		}
	}
	return out, rows.Err()
}

// ReportOverview handles GET /reports/overview.
func (h *Handlers) ReportOverview(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	owner := p.OwnerFilter()
	today := h.today()
	rows, err := QueryLines(ctx, h.Pool, owner, nil, today)
	if err != nil {
		response.Fail(c, err)
		return
	}
	o := Overview{AsOf: time.Now(), FlagCounts: map[string]int{}, Attention: []LineRow{}, Upcoming: []Upcoming{}}
	var attention []LineRow
	for _, r := range rows {
		if r.Remaining.IsPositive() {
			o.LinesRemaining++
		}
		switch rank := severityRank(r); {
		case !r.Remaining.IsPositive():
			o.LineStatus.Delivered++
		case rank == 0:
			o.LineStatus.Critical++
		case rank == 1:
			o.LineStatus.Warning++
		default:
			o.LineStatus.OnTrack++
		}
		for _, f := range r.Flags {
			o.FlagCounts[f.Code]++
		}
		if NeedsAction(r) {
			o.NeedsAction++
			attention = append(attention, r)
		} else if hasFlag(r, "under_ordered") {
			attention = append(attention, r)
		}
	}
	sort.SliceStable(attention, func(i, j int) bool { return severityRank(attention[i]) < severityRank(attention[j]) })
	if len(attention) > 200 {
		attention = attention[:200]
	}
	o.Attention = append(o.Attention, attention...)
	if err := h.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM contracts WHERE status = 'active' AND ($1::uuid IS NULL OR owner_id = $1)),
			(SELECT count(DISTINCT po.id) FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id
			 JOIN purchase_order_lines pol ON pol.purchase_order_id = po.id
			 WHERE po.status = 'confirmed' AND ($1::uuid IS NULL OR c.owner_id = $1)
			   AND (pol.quantity-pol.cancelled_quantity) > po_line_received(pol.id))`,
		owner).Scan(&o.ActiveContracts, &o.POsAwaitingGoods); err != nil {
		response.Fail(c, err)
		return
	}
	o.Upcoming, err = upcoming(ctx, h, owner, today)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if o.Weekly, err = weekly(ctx, h, owner, today); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

func severityRank(r LineRow) int {
	if len(r.Flags) == 0 {
		return 9
	}
	switch r.Flags[0].Severity {
	case progress.Critical:
		return 0
	case progress.Warning:
		return 1
	}
	return 2
}

func upcoming(ctx context.Context, h *Handlers, owner *uuid.UUID, today time.Time) ([]Upcoming, error) {
	rows, err := h.Pool.Query(ctx, `SELECT d.id, d.number, c.id, c.number, o.name, d.planned_on, d.firmness,
			(SELECT count(*) FROM delivery_order_lines WHERE delivery_order_id = d.id),
			(SELECT cl.name_on_contract FROM delivery_order_lines dol JOIN contract_lines cl ON cl.id = dol.contract_line_id WHERE dol.delivery_order_id = d.id LIMIT 1),
			(SELECT dol.quantity::text FROM delivery_order_lines dol WHERE dol.delivery_order_id = d.id LIMIT 1),
			(SELECT cl.unit FROM delivery_order_lines dol JOIN contract_lines cl ON cl.id = dol.contract_line_id WHERE dol.delivery_order_id = d.id LIMIT 1)
		FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id JOIN organizations o ON o.id = c.customer_id
		WHERE d.status = 'draft' AND d.planned_on BETWEEN $2 AND $2 + 7 AND ($1::uuid IS NULL OR c.owner_id = $1)
		ORDER BY d.planned_on, d.number LIMIT 10`, owner, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Upcoming{}
	for rows.Next() {
		var u Upcoming
		var on time.Time
		if err := rows.Scan(&u.ID, &u.Number, &u.ContractID, &u.ContractNumber, &u.CustomerName, &on, &u.Firmness, &u.Lines, &u.ItemName, &u.Quantity, &u.Unit); err != nil {
			return nil, err
		}
		if u.Lines != 1 {
			u.ItemName, u.Quantity, u.Unit = nil, nil, nil
		} else if u.Quantity != nil {
			q := decimalx.MustParse(*u.Quantity).String()
			u.Quantity = &q
		}
		u.PlannedOn = on.Format(time.DateOnly)
		out = append(out, u)
	}
	return out, rows.Err()
}

// Documents are a contract's purchase orders, goods receipts and delivery orders.
type Documents struct {
	PurchaseOrders []*PurchaseOrder `json:"purchase_orders"`
	GoodsReceipts  []*GoodsReceipt  `json:"goods_receipts"`
	DeliveryOrders []*DeliveryOrder `json:"delivery_orders"`
	// After activation: signed amendments, acceptances and returns (all kept, reversed ones marked).
	Amendments      []*Amendment `json:"amendments"`
	Acceptances     []*EventDoc  `json:"acceptances"`
	CustomerReturns []*EventDoc  `json:"customer_returns"`
	SupplierReturns []*EventDoc  `json:"supplier_returns"`
}

// ContractDocuments handles GET /contracts/:id/documents.
func (h *Handlers) ContractDocuments(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if _, err := lockContract(ctx, h.Pool, id, p, false); err != nil {
		response.Fail(c, err)
		return
	}
	ids := func(sql string) ([]uuid.UUID, error) {
		rows, err := h.Pool.Query(ctx, sql, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []uuid.UUID
		for rows.Next() {
			var x uuid.UUID
			if err := rows.Scan(&x); err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, rows.Err()
	}
	docs := Documents{PurchaseOrders: []*PurchaseOrder{}, GoodsReceipts: []*GoodsReceipt{}, DeliveryOrders: []*DeliveryOrder{},
		Amendments: []*Amendment{}, Acceptances: []*EventDoc{}, CustomerReturns: []*EventDoc{}, SupplierReturns: []*EventDoc{}}
	poIDs, err := ids(`SELECT id FROM purchase_orders WHERE contract_id = $1 ORDER BY number`)
	if err == nil {
		for _, x := range poIDs {
			po, e := loadPO(ctx, h.Pool, x, p)
			if e != nil {
				err = e
				break
			}
			docs.PurchaseOrders = append(docs.PurchaseOrders, po)
		}
	}
	if err == nil {
		var grIDs []uuid.UUID
		if grIDs, err = ids(`SELECT id FROM goods_receipts WHERE contract_id = $1 ORDER BY number`); err == nil {
			for _, x := range grIDs {
				gr, e := loadGR(ctx, h.Pool, x, p)
				if e != nil {
					err = e
					break
				}
				docs.GoodsReceipts = append(docs.GoodsReceipts, gr)
			}
		}
	}
	if err == nil {
		var doIDs []uuid.UUID
		if doIDs, err = ids(`SELECT id FROM delivery_orders WHERE contract_id = $1 ORDER BY number`); err == nil {
			for _, x := range doIDs {
				d, e := loadDO(ctx, h.Pool, x, p)
				if e != nil {
					err = e
					break
				}
				docs.DeliveryOrders = append(docs.DeliveryOrders, d)
			}
		}
	}
	if err == nil {
		err = lifecycleDocs(ctx, h.Pool, id, &docs)
	}
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, docs)
}
