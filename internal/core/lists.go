package core

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// POSummary is one purchase order in the cross-contract list (menu "Đơn mua").
type POSummary struct {
	ID             uuid.UUID `json:"id"`
	Number         string    `json:"number"`
	Status         string    `json:"status"`
	ContractID     uuid.UUID `json:"contract_id"`
	ContractNumber string    `json:"contract_number"`
	CustomerName   string    `json:"customer_name"`
	SupplierName   string    `json:"supplier_name"`
	Lines          int       `json:"lines"`
	LinesReceived  int       `json:"lines_fully_received"`
	NextExpectedOn *string   `json:"next_expected_on"`
	Late           bool      `json:"late"`
	OverrideStatus *string   `json:"override_status"`
}

// ListPurchaseOrders handles GET /purchase-orders?view=awaiting|draft|all.
func (h *Handlers) ListPurchaseOrders(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	page, size := httpx.Page(c)
	view := c.DefaultQuery("view", "awaiting")
	const base = `
WITH po_lines AS (
    SELECT pol.purchase_order_id, pol.quantity - pol.cancelled_quantity AS quantity, pol.expected_on,
           po_line_received(pol.id) AS received
    FROM purchase_order_lines pol
), agg AS (
    SELECT purchase_order_id, count(*) AS lines, count(*) FILTER (WHERE received >= quantity) AS done,
           min(expected_on) FILTER (WHERE received < quantity) AS next_eta,
           bool_or(received < quantity AND expected_on < $3) AS late
    FROM po_lines GROUP BY 1
), list AS (
    SELECT po.id, po.number, po.status, c.id AS contract_id, c.number AS contract_number, o.name AS customer_name, s.name AS supplier_name,
           a.lines, a.done, a.next_eta, coalesce(a.late, false) AS late,
           (SELECT r.status FROM quantity_override_requests r WHERE r.kind = 'purchase_order' AND r.document_id = po.id ORDER BY r.requested_at DESC LIMIT 1) AS override_status
    FROM purchase_orders po
    JOIN contracts c ON c.id = po.contract_id JOIN organizations o ON o.id = c.customer_id JOIN organizations s ON s.id = po.supplier_id
    JOIN agg a ON a.purchase_order_id = po.id
    WHERE ($1::uuid IS NULL OR c.owner_id = $1)
      AND ($2 = 'all' OR ($2 = 'draft' AND po.status = 'draft') OR ($2 = 'awaiting' AND po.status = 'confirmed' AND a.done < a.lines))
)
SELECT *, count(*) OVER () FROM list ORDER BY late DESC, next_eta NULLS LAST, number LIMIT $4 OFFSET $5`
	rows, err := h.Pool.Query(ctx, base, p.OwnerFilter(), view, h.today(), size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []POSummary{}
	total := 0
	for rows.Next() {
		var s POSummary
		var eta *time.Time
		if err := rows.Scan(&s.ID, &s.Number, &s.Status, &s.ContractID, &s.ContractNumber, &s.CustomerName, &s.SupplierName,
			&s.Lines, &s.LinesReceived, &eta, &s.Late, &s.OverrideStatus, &total); err != nil {
			response.Fail(c, err)
			return
		}
		s.NextExpectedOn = dateString(eta)
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, page, size, total)
}

// DOSummary is one delivery order in the cross-contract list (menu "Giao hàng").
type DOSummary struct {
	ID             uuid.UUID `json:"id"`
	Number         string    `json:"number"`
	Status         string    `json:"status"`
	ContractID     uuid.UUID `json:"contract_id"`
	ContractNumber string    `json:"contract_number"`
	CustomerName   string    `json:"customer_name"`
	PlannedOn      *string   `json:"planned_on"`
	Firmness       string    `json:"firmness"`
	DeliveredOn    *string   `json:"delivered_on"`
	Lines          int       `json:"lines"`
	Overdue        bool      `json:"overdue"`
	OverrideStatus *string   `json:"override_status"`
}

// ListDeliveryOrders handles GET /delivery-orders?view=planned|delivered.
func (h *Handlers) ListDeliveryOrders(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	page, size := httpx.Page(c)
	delivered := c.DefaultQuery("view", "planned") == "delivered"
	order := `d.planned_on NULLS LAST, d.number`
	if delivered {
		order = `d.delivered_on DESC, d.number DESC`
	}
	rows, err := h.Pool.Query(ctx, `SELECT d.id, d.number, d.status, c.id, c.number, o.name, d.planned_on, d.firmness, d.delivered_on,
			(SELECT count(*) FROM delivery_order_lines WHERE delivery_order_id = d.id),
			(d.status = 'draft' AND d.firmness = 'firm' AND d.planned_on < $3),
			(SELECT r.status FROM quantity_override_requests r WHERE r.kind = 'delivery_order' AND r.document_id = d.id ORDER BY r.requested_at DESC LIMIT 1),
			count(*) OVER ()
		FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id JOIN organizations o ON o.id = c.customer_id
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) AND d.status = $2
		ORDER BY `+order+` LIMIT $4 OFFSET $5`, p.OwnerFilter(), map[bool]string{false: "draft", true: "confirmed"}[delivered], h.today(), size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []DOSummary{}
	total := 0
	for rows.Next() {
		var s DOSummary
		var planned, done *time.Time
		if err := rows.Scan(&s.ID, &s.Number, &s.Status, &s.ContractID, &s.ContractNumber, &s.CustomerName, &planned, &s.Firmness, &done,
			&s.Lines, &s.Overdue, &s.OverrideStatus, &total); err != nil {
			response.Fail(c, err)
			return
		}
		s.PlannedOn, s.DeliveredOn = dateString(planned), dateString(done)
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, page, size, total)
}
