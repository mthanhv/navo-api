package core

import (
	"context"
	"fmt"
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

// GoodsReceipt records goods arriving from a supplier against one PO.
type GoodsReceipt struct {
	ID              uuid.UUID `json:"id"`
	Number          string    `json:"number"`
	PurchaseOrderID uuid.UUID `json:"purchase_order_id"`
	ContractID      uuid.UUID `json:"contract_id"`
	Status          string    `json:"status"`
	ReceivedOn      string    `json:"received_on"`
	Lines           []GRLine  `json:"lines"`
}

// GRLine splits accepted and rejected goods; only accepted goods count as received (AC-05).
type GRLine struct {
	PurchaseOrderLineID uuid.UUID       `json:"purchase_order_line_id"`
	ContractLineNo      int             `json:"contract_line_no"`
	Accepted            decimal.Decimal `json:"accepted_quantity"`
	Rejected            decimal.Decimal `json:"rejected_quantity"`
}

func loadGR(ctx context.Context, q db.Querier, id uuid.UUID, p *access.Principal) (*GoodsReceipt, error) {
	var g GoodsReceipt
	var received time.Time
	err := q.QueryRow(ctx, `SELECT gr.id, gr.number, gr.purchase_order_id, gr.contract_id, gr.status, gr.received_on
		FROM goods_receipts gr JOIN contracts c ON c.id = gr.contract_id
		WHERE gr.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, p.OwnerFilter()).
		Scan(&g.ID, &g.Number, &g.PurchaseOrderID, &g.ContractID, &g.Status, &received)
	if err != nil {
		return nil, err
	}
	g.ReceivedOn = received.Format(time.DateOnly)
	rows, err := q.Query(ctx, `SELECT grl.purchase_order_line_id, cl.line_no, grl.accepted_quantity::text, grl.rejected_quantity::text
		FROM goods_receipt_lines grl JOIN purchase_order_lines pol ON pol.id = grl.purchase_order_line_id
		JOIN contract_lines cl ON cl.id = pol.contract_line_id WHERE grl.goods_receipt_id = $1 ORDER BY cl.line_no`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g.Lines = []GRLine{}
	for rows.Next() {
		var l GRLine
		var a, r string
		if err := rows.Scan(&l.PurchaseOrderLineID, &l.ContractLineNo, &a, &r); err != nil {
			return nil, err
		}
		l.Accepted, l.Rejected = decimalx.MustParse(a), decimalx.MustParse(r)
		g.Lines = append(g.Lines, l)
	}
	return &g, rows.Err()
}

type grLineInput struct {
	PurchaseOrderLineID string `json:"purchase_order_line_id"`
	AcceptedQuantity    string `json:"accepted_quantity"`
	RejectedQuantity    string `json:"rejected_quantity"`
}

type grInput struct {
	ReceivedOn *string       `json:"received_on"`
	Lines      []grLineInput `json:"lines"`
}

// CreateGoodsReceipt handles POST /purchase-orders/:id/goods-receipts (a draft).
func (h *Handlers) CreateGoodsReceipt(c *gin.Context) {
	p := auth.Principal(c)
	poID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in grInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	receivedOn := httpx.Date(&errs, "received_on", in.ReceivedOn)
	if receivedOn != nil && receivedOn.After(h.today()) {
		errs.Add("received_on", "not_future", nil)
	}
	if receivedOn == nil && len(errs) == 0 {
		errs.Add("received_on", "required", nil)
	}
	if len(in.Lines) == 0 {
		errs.Add("lines", "required", map[string]any{"min": 1})
	}
	type line struct {
		poLine             uuid.UUID
		accepted, rejected decimal.Decimal
	}
	lines := make([]line, len(in.Lines))
	for i, l := range in.Lines {
		f := func(n string) string { return fmt.Sprintf("lines[%d].%s", i, n) }
		lines[i].poLine = httpx.UUID(&errs, f("purchase_order_line_id"), l.PurchaseOrderLineID)
		if l.AcceptedQuantity == "" {
			l.AcceptedQuantity = "0"
		}
		if l.RejectedQuantity == "" {
			l.RejectedQuantity = "0"
		}
		lines[i].accepted = decimalx.NonNegative(&errs, f("accepted_quantity"), l.AcceptedQuantity)
		lines[i].rejected = decimalx.NonNegative(&errs, f("rejected_quantity"), l.RejectedQuantity)
		if lines[i].accepted.Add(lines[i].rejected).IsZero() {
			errs.Add(f("accepted_quantity"), "greater_than", map[string]any{"min": "0"})
		}
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "goods_receipts.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		var contractID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT po.status, po.contract_id FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id
			WHERE po.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF po`, poID, p.OwnerFilter()).Scan(&status, &contractID); err != nil {
			return nil, err
		}
		if status != "confirmed" {
			return nil, apperrors.Rule("purchase_order_not_confirmed", "Goods can only be received against a confirmed purchase order.", map[string]any{"status": status})
		}
		number, err := numbering.NextChild(ctx, tx, contractID, "GR")
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO goods_receipts (contract_id, purchase_order_id, number, received_on, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, contractID, poID, number, *receivedOn, p.UserID).Scan(&id); err != nil {
			return nil, err
		}
		for _, l := range lines {
			// The composite FK refuses a line that belongs to another purchase order.
			if _, err := tx.Exec(ctx, `INSERT INTO goods_receipt_lines (goods_receipt_id, purchase_order_id, purchase_order_line_id, accepted_quantity, rejected_quantity)
				VALUES ($1, $2, $3, $4, $5)`, id, poID, l.poLine, l.accepted, l.rejected); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "goods_receipt", id, map[string]any{"number": number, "purchase_order_id": poID, "lines": len(lines)}); err != nil {
			return nil, err
		}
		return loadGR(ctx, tx, id, p)
	})
}

// ConfirmGoodsReceipt handles POST /goods-receipts/:id/confirm. Accepted goods
// may not exceed what was ordered (O-02 over-receipt threshold not yet decided: refuse).
func (h *Handlers) ConfirmGoodsReceipt(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "goods_receipts.confirm", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		if err := tx.QueryRow(ctx, `SELECT gr.status FROM goods_receipts gr JOIN contracts c ON c.id = gr.contract_id
			WHERE gr.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF gr`, id, p.OwnerFilter()).Scan(&status); err != nil {
			return nil, err
		}
		if status != "draft" {
			return nil, apperrors.Rule("document_not_draft", "Only a draft goods receipt can be confirmed.", map[string]any{"status": status})
		}
		rows, err := tx.Query(ctx, `SELECT pol.id, pol.contract_line_id, cl.line_no, (pol.quantity-pol.cancelled_quantity)::text, grl.accepted_quantity::text
			FROM purchase_order_lines pol JOIN goods_receipt_lines grl ON grl.purchase_order_line_id = pol.id AND grl.goods_receipt_id = $1
			JOIN contract_lines cl ON cl.id = pol.contract_line_id
			ORDER BY pol.id FOR UPDATE OF pol`, id)
		if err != nil {
			return nil, err
		}
		type check struct {
			poLine, contractLine uuid.UUID
			lineNo               int
			ordered, accepted    decimal.Decimal
		}
		var checks []check
		for rows.Next() {
			var ch check
			var o, a string
			if err := rows.Scan(&ch.poLine, &ch.contractLine, &ch.lineNo, &o, &a); err != nil {
				rows.Close()
				return nil, err
			}
			ch.ordered, ch.accepted = decimalx.MustParse(o), decimalx.MustParse(a)
			checks = append(checks, ch)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		lineIDs := make([]uuid.UUID, 0, len(checks))
		for _, ch := range checks {
			lineIDs = append(lineIDs, ch.contractLine)
			var already string
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(grl.accepted_quantity), 0)::text FROM goods_receipt_lines grl
				JOIN goods_receipts gr ON gr.id = grl.goods_receipt_id WHERE grl.purchase_order_line_id = $1 AND gr.status = 'confirmed'`, ch.poLine).Scan(&already); err != nil {
				return nil, err
			}
			if decimalx.MustParse(already).Add(ch.accepted).GreaterThan(ch.ordered) {
				return nil, apperrors.Rule("receipt_exceeds_ordered", "Accepted goods would exceed the quantity ordered on this purchase order line.",
					map[string]any{"line_no": ch.lineNo, "ordered": ch.ordered.String(), "already_received": already, "accepted": ch.accepted.String()})
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE goods_receipts SET status = 'confirmed', confirmed_at = now(), confirmed_by = $2, version = version + 1, updated_at = now()
			WHERE id = $1`, id, p.UserID); err != nil {
			return nil, err
		}
		if err := touchLines(ctx, tx, lineIDs); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "confirm", "goods_receipt", id, map[string]any{"status": []string{"draft", "confirmed"}}); err != nil {
			return nil, err
		}
		return loadGR(ctx, tx, id, p)
	})
}

// DeliveryOrder is a planned (draft) or confirmed delivery to the customer.
type DeliveryOrder struct {
	ID          uuid.UUID     `json:"id"`
	Number      string        `json:"number"`
	ContractID  uuid.UUID     `json:"contract_id"`
	Status      string        `json:"status"`
	PlannedOn   *string       `json:"planned_on"`
	Firmness    string        `json:"firmness"`
	Route       string        `json:"route"`
	DeliveredOn *string       `json:"delivered_on"`
	Lines       []DOLine      `json:"lines"`
	Override    *OverrideInfo `json:"override"`
}

// DOLine is a quantity of one contract line.
type DOLine struct {
	ContractLineID uuid.UUID       `json:"contract_line_id"`
	ContractLineNo int             `json:"contract_line_no"`
	Quantity       decimal.Decimal `json:"quantity"`
}

func loadDO(ctx context.Context, q db.Querier, id uuid.UUID, p *access.Principal) (*DeliveryOrder, error) {
	var d DeliveryOrder
	var planned, delivered *time.Time
	err := q.QueryRow(ctx, `SELECT d.id, d.number, d.contract_id, d.status, d.planned_on, d.firmness, d.route, d.delivered_on
		FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`,
		id, p.OwnerFilter()).Scan(&d.ID, &d.Number, &d.ContractID, &d.Status, &planned, &d.Firmness, &d.Route, &delivered)
	if err != nil {
		return nil, err
	}
	d.PlannedOn, d.DeliveredOn = dateString(planned), dateString(delivered)
	if d.Override, err = latestOverride(ctx, q, "delivery_order", id); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT dol.contract_line_id, cl.line_no, dol.quantity::text FROM delivery_order_lines dol
		JOIN contract_lines cl ON cl.id = dol.contract_line_id WHERE dol.delivery_order_id = $1 ORDER BY cl.line_no`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Lines = []DOLine{}
	for rows.Next() {
		var l DOLine
		var qty string
		if err := rows.Scan(&l.ContractLineID, &l.ContractLineNo, &qty); err != nil {
			return nil, err
		}
		l.Quantity = decimalx.MustParse(qty)
		d.Lines = append(d.Lines, l)
	}
	return &d, rows.Err()
}

type doLineInput struct {
	ContractLineID string `json:"contract_line_id"`
	Quantity       string `json:"quantity"`
}

type doInput struct {
	PlannedOn *string       `json:"planned_on"`
	Firmness  string        `json:"firmness"`
	Route     string        `json:"route"`
	Lines     []doLineInput `json:"lines"`
}

// CreateDeliveryOrder handles POST /contracts/:id/delivery-orders: a planned
// delivery with a tentative or firm date (never silently promoted to firm).
func (h *Handlers) CreateDeliveryOrder(c *gin.Context) {
	p := auth.Principal(c)
	contractID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in doInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	planned := httpx.Date(&errs, "planned_on", in.PlannedOn)
	if in.Firmness == "" {
		in.Firmness = "tentative"
	}
	if in.Firmness != "tentative" && in.Firmness != "firm" {
		errs.Add("firmness", "one_of", map[string]any{"values": []string{"tentative", "firm"}})
	}
	if in.Firmness == "firm" && planned == nil {
		errs.Add("planned_on", "required_when_firm", nil)
	}
	if in.Route == "" {
		in.Route = "via_company"
	}
	if in.Route != "via_company" && in.Route != "direct" {
		errs.Add("route", "one_of", map[string]any{"values": []string{"via_company", "direct"}})
	}
	if len(in.Lines) == 0 {
		errs.Add("lines", "required", map[string]any{"min": 1})
	}
	type line struct {
		contractLine uuid.UUID
		qty          decimal.Decimal
	}
	lines := make([]line, len(in.Lines))
	seen := map[uuid.UUID]bool{}
	for i, l := range in.Lines {
		f := func(n string) string { return fmt.Sprintf("lines[%d].%s", i, n) }
		lines[i] = line{contractLine: httpx.UUID(&errs, f("contract_line_id"), l.ContractLineID), qty: decimalx.Positive(&errs, f("quantity"), l.Quantity)}
		if seen[lines[i].contractLine] {
			errs.Add(f("contract_line_id"), "duplicate", nil)
		}
		seen[lines[i].contractLine] = true
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "delivery_orders.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		k, err := lockContract(ctx, tx, contractID, p, false)
		if err != nil {
			return nil, err
		}
		if k.Status != "active" {
			return nil, apperrors.Rule("contract_not_active", "Activate the contract before planning deliveries.", map[string]any{"status": k.Status})
		}
		number, err := numbering.NextChild(ctx, tx, contractID, "DO")
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO delivery_orders (contract_id, number, planned_on, firmness, route, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, contractID, number, planned, in.Firmness, in.Route, p.UserID).Scan(&id); err != nil {
			return nil, err
		}
		for _, l := range lines {
			// The composite FK refuses a line of another contract (AC-03 for deliveries).
			if _, err := tx.Exec(ctx, `INSERT INTO delivery_order_lines (delivery_order_id, contract_id, contract_line_id, quantity) VALUES ($1, $2, $3, $4)`,
				id, contractID, l.contractLine, l.qty); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "delivery_order", id, map[string]any{"number": number, "contract_id": contractID,
			"planned_on": dateString(planned), "firmness": in.Firmness, "route": in.Route, "lines": len(lines)}); err != nil {
			return nil, err
		}
		return loadDO(ctx, tx, id, p)
	})
}

type confirmDOInput struct {
	DeliveredOn *string `json:"delivered_on"`
}

// ConfirmDeliveryOrder handles POST /delivery-orders/:id/confirm for the DO's owner scope.
func (h *Handlers) ConfirmDeliveryOrder(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in confirmDOInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	delivered := httpx.Date(&errs, "delivered_on", in.DeliveredOn)
	if delivered != nil && delivered.After(h.today()) {
		errs.Add("delivered_on", "not_future", nil)
	}
	if delivered == nil && len(errs) == 0 {
		errs.Add("delivered_on", "required", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "delivery_orders.confirm", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var found uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT d.id FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id
			WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, p.OwnerFilter()).Scan(&found); err != nil {
			return nil, err
		}
		if err := confirmDOTx(ctx, tx, id, *delivered, p.UserID, false); err != nil {
			return nil, err
		}
		return loadDO(ctx, tx, id, p)
	})
}

// confirmDOTx confirms a planned delivery. Delivered quantities count once even if
// retried (AC-04) and need goods received for the line (a PO is not proof the goods
// exist; no override can change that). Exceeding the contract needs an approved
// Admin override (allowOverContract, BR-05).
func confirmDOTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, deliveredOn time.Time, actor uuid.UUID, allowOverContract bool) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM delivery_orders WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
		return err
	}
	if status != "draft" {
		return apperrors.Rule("document_not_draft", "Only a planned delivery can be confirmed.", map[string]any{"status": status})
	}
	checks, err := lockLinesFor(ctx, tx, `SELECT cl.id, cl.line_no, cl.quantity::text, dol.quantity::text
		FROM contract_lines cl JOIN delivery_order_lines dol ON dol.contract_line_id = cl.id AND dol.delivery_order_id = $1
		ORDER BY cl.id FOR UPDATE OF cl`, id)
	if err != nil {
		return err
	}
	lineIDs := make([]uuid.UUID, len(checks))
	for i, ch := range checks {
		lineIDs[i] = ch.lineID
		var deliveredSoFar, received string
		if err := tx.QueryRow(ctx, `SELECT
				coalesce((SELECT sum(dol.quantity) FROM delivery_order_lines dol JOIN delivery_orders d ON d.id = dol.delivery_order_id
				          WHERE dol.contract_line_id = $1 AND d.status = 'confirmed'), 0)::text,
				coalesce((SELECT sum(grl.accepted_quantity) FROM goods_receipt_lines grl JOIN goods_receipts gr ON gr.id = grl.goods_receipt_id
				          JOIN purchase_order_lines pol ON pol.id = grl.purchase_order_line_id
				          WHERE pol.contract_line_id = $1 AND gr.status = 'confirmed'), 0)::text`, ch.lineID).Scan(&deliveredSoFar, &received); err != nil {
			return err
		}
		done, in := decimalx.MustParse(deliveredSoFar), decimalx.MustParse(received)
		if !allowOverContract && done.Add(ch.req).GreaterThan(ch.contractQty) {
			return apperrors.Rule("delivery_exceeds_contract", "This delivery would exceed the contract quantity. Ask Admin to approve an over-delivery.",
				map[string]any{"line_no": ch.lineNo, "contract_quantity": ch.contractQty.String(), "already_delivered": done.String(), "requested": ch.req.String()})
		}
		if available := in.Sub(done); ch.req.GreaterThan(available) {
			return apperrors.Rule("insufficient_stock", "Not enough goods have been received for this line yet.",
				map[string]any{"line_no": ch.lineNo, "available": available.String(), "requested": ch.req.String()})
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_orders SET status = 'confirmed', delivered_on = $2, confirmed_at = now(), confirmed_by = $3,
		version = version + 1, updated_at = now() WHERE id = $1`, id, deliveredOn, actor); err != nil {
		return err
	}
	if err := touchLines(ctx, tx, lineIDs); err != nil {
		return err
	}
	return audit.Record(ctx, tx, "confirm", "delivery_order", id, map[string]any{"status": []string{"draft", "confirmed"},
		"delivered_on": deliveredOn.Format(time.DateOnly), "over_contract_approved": allowOverContract})
}
