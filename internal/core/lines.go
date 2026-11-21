package core

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/progress"
	"navo-api/internal/response"
)

// NextDelivery is the nearest planned (draft) delivery order for a line.
type NextDelivery struct {
	Number    string          `json:"number"`
	PlannedOn string          `json:"planned_on"`
	Firmness  string          `json:"firmness"`
	Quantity  decimal.Decimal `json:"quantity"`
}

// LineRow is one contract line with its progress (REQUIREMENTS 5.4).
type LineRow struct {
	ContractLineID   uuid.UUID       `json:"contract_line_id"`
	ContractID       uuid.UUID       `json:"contract_id"`
	ContractNumber   string          `json:"contract_number"`
	CustomerID       uuid.UUID       `json:"customer_id"`
	CustomerName     string          `json:"customer_name"`
	OwnerID          uuid.UUID       `json:"owner_id"`
	OwnerName        string          `json:"owner_name"`
	LineNo           int             `json:"line_no"`
	NameOnContract   string          `json:"name_on_contract"`
	Unit             string          `json:"unit"`
	ContractQuantity decimal.Decimal `json:"contract_quantity"`
	Ordered          decimal.Decimal `json:"ordered"`
	Received         decimal.Decimal `json:"received"`
	Delivered        decimal.Decimal `json:"delivered"`
	// Accepted is what the customer has accepted (acceptance documents); Delivered and
	// Received are net of customer and supplier returns.
	Accepted decimal.Decimal `json:"accepted"`
	// UnitPrice is the contract's sales price for the line (visible with the contract, never a purchase price).
	UnitPrice decimal.Decimal `json:"unit_price"`
	Remaining decimal.Decimal `json:"remaining"`
	Over      decimal.Decimal `json:"over"`
	Available decimal.Decimal `json:"available"`
	// Planned is on open (draft) deliveries or planned service visits, dated or not; Unplanned is
	// what still needs a plan: max(remaining - planned, 0).
	Planned              decimal.Decimal `json:"planned"`
	Unplanned            decimal.Decimal `json:"unplanned"`
	DueOn                *string         `json:"due_on"`
	NextDelivery         *NextDelivery   `json:"next_delivery"`
	Flags                []progress.Flag `json:"flags"`
	LastBusinessChangeAt time.Time       `json:"last_business_change_at"`
	// LineType: goods, or service (fulfilled by service visits: nothing to order or receive).
	LineType string `json:"line_type"`
}

// Each quantity is aggregated in its own subquery and joined on the contract
// line, so joining POs, receipts and deliveries cannot multiply rows (TECHNICAL_DESIGN 3.3).
const lineProgressSQL = `
WITH lines AS (
    SELECT cl.id, cl.contract_id, coalesce(c.number, c.draft_number) AS contract_number, c.customer_id, o.name AS customer_name,
           c.owner_id, u.full_name AS owner_name, cl.line_no, cl.name_on_contract, cl.unit, cl.quantity, cl.unit_price, cl.due_on,
           cl.last_business_change_at, cl.line_type, c.created_at AS contract_created_at
    FROM contract_lines cl
    JOIN contracts c ON c.id = cl.contract_id
    JOIN organizations o ON o.id = c.customer_id
    JOIN users u ON u.id = c.owner_id
    WHERE ($1::uuid IS NULL OR c.owner_id = $1)
      AND (($2::uuid IS NOT NULL AND c.id = $2) OR ($2::uuid IS NULL AND c.status = 'active'))
), ordered AS (
    SELECT pol.contract_line_id, sum(pol.quantity-pol.cancelled_quantity) AS q
    FROM purchase_order_lines pol JOIN purchase_orders po ON po.id = pol.purchase_order_id
    WHERE po.status = 'confirmed' AND pol.contract_line_id IN (SELECT id FROM lines)
    GROUP BY 1
), received_by_po_line AS (
    SELECT pol.id AS purchase_order_line_id, po_line_received(pol.id) AS q
    FROM purchase_order_lines pol WHERE pol.contract_line_id IN (SELECT id FROM lines)
), received AS (
    SELECT pol.contract_line_id, sum(r.q) AS q
    FROM received_by_po_line r JOIN purchase_order_lines pol ON pol.id = r.purchase_order_line_id
    WHERE pol.contract_line_id IN (SELECT id FROM lines)
    GROUP BY 1
), late_receipts AS (
    SELECT pol.contract_line_id, bool_or(true) AS late
    FROM purchase_order_lines pol
    JOIN purchase_orders po ON po.id = pol.purchase_order_id
    LEFT JOIN received_by_po_line r ON r.purchase_order_line_id = pol.id
    WHERE po.status = 'confirmed' AND pol.expected_on < $3 AND coalesce(r.q, 0) < (pol.quantity-pol.cancelled_quantity)
      AND pol.contract_line_id IN (SELECT id FROM lines)
    GROUP BY 1
), delivered AS (
    SELECT id AS contract_line_id, line_delivered(id) AS q, line_accepted(id) AS a FROM lines
), next_plan AS (
    SELECT DISTINCT ON (contract_line_id) contract_line_id, number, planned_on, firmness, quantity FROM (
        SELECT dol.contract_line_id, d.number, d.planned_on, d.firmness, dol.quantity
        FROM delivery_order_lines dol JOIN delivery_orders d ON d.id = dol.delivery_order_id
        WHERE d.status = 'draft' AND d.planned_on IS NOT NULL AND dol.contract_line_id IN (SELECT id FROM lines)
        UNION ALL -- a planned service visit is the service line's next "delivery" (dates agreed with the customer)
        SELECT vl.contract_line_id, v.number, v.planned_on, 'firm', vl.quantity
        FROM service_visit_lines vl JOIN service_visits v ON v.id = vl.visit_id
        WHERE v.status = 'planned' AND v.planned_on IS NOT NULL AND vl.contract_line_id IN (SELECT id FROM lines)
    ) plans
    ORDER BY contract_line_id, planned_on, number
), planned AS (
    SELECT contract_line_id, sum(quantity) AS q FROM (
        SELECT dol.contract_line_id, dol.quantity
        FROM delivery_order_lines dol JOIN delivery_orders d ON d.id = dol.delivery_order_id
        WHERE d.status = 'draft' AND dol.contract_line_id IN (SELECT id FROM lines)
        UNION ALL
        SELECT vl.contract_line_id, vl.quantity
        FROM service_visit_lines vl JOIN service_visits v ON v.id = vl.visit_id
        WHERE v.status = 'planned' AND vl.contract_line_id IN (SELECT id FROM lines)
    ) p GROUP BY 1
)
SELECT l.id, l.contract_id, l.contract_number, l.customer_id, l.customer_name, l.owner_id, l.owner_name, l.line_no, l.name_on_contract,
       l.unit, l.quantity::text, coalesce(o.q, 0)::text, coalesce(r.q, 0)::text, coalesce(d.q, 0)::text, coalesce(d.a, 0)::text, l.unit_price::text, l.due_on,
       coalesce(lr.late, false), np.number, np.planned_on, np.firmness, np.quantity::text, l.last_business_change_at, l.line_type,
       coalesce(pl.q, 0)::text
FROM lines l
LEFT JOIN ordered o ON o.contract_line_id = l.id
LEFT JOIN received r ON r.contract_line_id = l.id
LEFT JOIN delivered d ON d.contract_line_id = l.id
LEFT JOIN late_receipts lr ON lr.contract_line_id = l.id
LEFT JOIN next_plan np ON np.contract_line_id = l.id
LEFT JOIN planned pl ON pl.contract_line_id = l.id
ORDER BY l.contract_created_at DESC, l.contract_id, l.line_no -- newest contract first, its lines in order
`

// QueryLines returns line progress for the caller's scope. With contractID it
// returns that contract's lines (any status); otherwise lines of active contracts.
func QueryLines(ctx context.Context, q db.Querier, owner, contractID *uuid.UUID, today time.Time) ([]LineRow, error) {
	horizon, err := horizonDays(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, lineProgressSQL, owner, contractID, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LineRow{}
	for rows.Next() {
		var r LineRow
		var qty, ordered, received, delivered, accepted, price, planned string
		var due *time.Time
		var late bool
		var planNumber, planFirmness, planQty *string
		var planOn *time.Time
		if err := rows.Scan(&r.ContractLineID, &r.ContractID, &r.ContractNumber, &r.CustomerID, &r.CustomerName, &r.OwnerID, &r.OwnerName,
			&r.LineNo, &r.NameOnContract, &r.Unit, &qty, &ordered, &received, &delivered, &accepted, &price, &due, &late,
			&planNumber, &planOn, &planFirmness, &planQty, &r.LastBusinessChangeAt, &r.LineType, &planned); err != nil {
			return nil, err
		}
		facts := progress.Facts{
			Contract: decimalx.MustParse(qty), Ordered: decimalx.MustParse(ordered),
			Received: decimalx.MustParse(received), Delivered: decimalx.MustParse(delivered),
			DueOn: due, ReceiptOverdue: late, Planned: decimalx.MustParse(planned),
		}
		if planNumber != nil {
			facts.NextPlan = &progress.Plan{Number: *planNumber, On: *planOn, Firm: *planFirmness == "firm", Quantity: decimalx.MustParse(*planQty)}
			r.NextDelivery = &NextDelivery{Number: *planNumber, PlannedOn: planOn.Format(time.DateOnly), Firmness: *planFirmness, Quantity: facts.NextPlan.Quantity}
		}
		r.ContractQuantity, r.Ordered, r.Received, r.Delivered = facts.Contract, facts.Ordered, facts.Received, facts.Delivered
		r.Remaining, r.Over, r.Available = progress.Remaining(facts), progress.Over(facts), progress.Available(facts)
		r.Planned, r.Unplanned = facts.Planned, progress.Unplanned(facts)
		if r.LineType == "service" {
			// A service is done by visiting, not bought and stocked: the ordering and receiving
			// rules do not apply. "No plan" then means visits not scheduled, "overdue" a visit missed.
			facts.Ordered, facts.Received, facts.ReceiptOverdue = facts.Contract, decimal.Max(facts.Contract, facts.Delivered), false
			r.Ordered, r.Available = facts.Contract, decimal.Zero
		}
		r.Accepted, r.UnitPrice = decimalx.MustParse(accepted), decimalx.MustParse(price)
		r.DueOn = dateString(due)
		r.Flags = progress.Flags(facts, today, horizon)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReportContractLines handles GET /reports/contract-lines?tab=action|soon|all&flag=&contract_id=&customer_id=&owner_id=&page=.
// Lines of active contracts in the caller's scope, after the shared flag rules.
// "action" = a critical or warning flag; "soon" = a delivery planned in the next 7
// days. Counts for each tab come with the page.
func (h *Handlers) ReportContractLines(c *gin.Context) {
	p := auth.Principal(c)
	var errs apperrors.FieldErrors
	contractID := optionalUUID(&errs, "contract_id", c.Query("contract_id"))
	customer := optionalUUID(&errs, "customer_id", c.Query("customer_id"))
	ownerParam := optionalUUID(&errs, "owner_id", c.Query("owner_id"))
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	owner := p.OwnerFilter()
	if owner == nil {
		owner = ownerParam
	}
	rows, err := QueryLines(c.Request.Context(), h.Pool, owner, contractID, h.today())
	if err != nil {
		response.Fail(c, err)
		return
	}
	tab := c.Query("tab")
	flag := strings.TrimSpace(c.Query("flag"))
	if flag == "needs_action" {
		tab, flag = "action", ""
	}
	soonUntil := h.today().AddDate(0, 0, 7).Format(time.DateOnly)
	counts := map[string]int{"action": 0, "soon": 0, "all": 0}
	filtered := []LineRow{}
	for _, r := range rows {
		if customer != nil && r.CustomerID != *customer {
			continue
		}
		action := NeedsAction(r)
		soon := r.NextDelivery != nil && r.NextDelivery.PlannedOn <= soonUntil
		counts["all"]++
		if action {
			counts["action"]++
		}
		if soon {
			counts["soon"]++
		}
		if (tab == "action" && !action) || (tab == "soon" && !soon) || (flag != "" && !hasFlag(r, flag)) {
			continue
		}
		filtered = append(filtered, r)
	}
	page, size := httpx.Page(c)
	start := min((page-1)*size, len(filtered))
	end := min(start+size, len(filtered))
	response.ListCounts(c, filtered[start:end], page, size, len(filtered), counts)
}

// NeedsAction reports whether a line has a critical or warning flag.
func NeedsAction(r LineRow) bool {
	for _, f := range r.Flags {
		if f.Severity == progress.Critical || f.Severity == progress.Warning {
			return true
		}
	}
	return false
}

func hasFlag(r LineRow, code string) bool {
	for _, f := range r.Flags {
		if f.Code == code {
			return true
		}
	}
	return false
}
