package core

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/auth"
	"navo-api/internal/response"
)

// SearchHit is one result of the top-bar search.
type SearchHit struct {
	ID         uuid.UUID  `json:"id"`
	Label      string     `json:"label"`  // number or code
	Detail     string     `json:"detail"` // title, supplier or name
	ContractID *uuid.UUID `json:"contract_id,omitempty"`
}

// SearchResults groups hits by kind, at most 5 each.
type SearchResults struct {
	Contracts      []SearchHit `json:"contracts"`
	PurchaseOrders []SearchHit `json:"purchase_orders"`
	DeliveryOrders []SearchHit `json:"delivery_orders"`
	Customers      []SearchHit `json:"customers"`
}

// Search handles GET /search?q=: contracts, purchase orders, deliveries and customers
// in the caller's scope whose number, title, reference or name contains q (2+ characters).
func (h *Handlers) Search(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	q := strings.TrimSpace(c.Query("q"))
	out := SearchResults{Contracts: []SearchHit{}, PurchaseOrders: []SearchHit{}, DeliveryOrders: []SearchHit{}, Customers: []SearchHit{}}
	if len([]rune(q)) < 2 {
		response.OK(c, out)
		return
	}
	owner := p.OwnerFilter()
	var err error
	if out.Contracts, err = hits(ctx, h, `SELECT c.id, coalesce(c.number, c.draft_number), c.title || ' · ' || o.name, NULL::uuid
		FROM contracts c JOIN organizations o ON o.id = c.customer_id
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) AND c.status <> 'cancelled'
		  AND (c.number ILIKE '%' || $2 || '%' OR c.draft_number ILIKE '%' || $2 || '%' OR c.title ILIKE '%' || $2 || '%'
		       OR c.customer_reference ILIKE '%' || $2 || '%' OR o.name ILIKE '%' || $2 || '%')
		ORDER BY c.number DESC NULLS LAST LIMIT 5`, owner, q); err != nil {
		response.Fail(c, err)
		return
	}
	if out.PurchaseOrders, err = hits(ctx, h, `SELECT po.id, po.number, s.name || ' · ' || c.number, c.id
		FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id JOIN organizations s ON s.id = po.supplier_id
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) AND (po.number ILIKE '%' || $2 || '%' OR s.name ILIKE '%' || $2 || '%')
		ORDER BY po.number DESC LIMIT 5`, owner, q); err != nil {
		response.Fail(c, err)
		return
	}
	if out.DeliveryOrders, err = hits(ctx, h, `SELECT d.id, d.number, o.name || ' · ' || c.number, c.id
		FROM delivery_orders d JOIN contracts c ON c.id = d.contract_id JOIN organizations o ON o.id = c.customer_id
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) AND d.number ILIKE '%' || $2 || '%'
		ORDER BY d.number DESC LIMIT 5`, owner, q); err != nil {
		response.Fail(c, err)
		return
	}
	// Customers: everyone with contract access may look up partners (master data is shared).
	if out.Customers, err = hits(ctx, h, `SELECT id, code, name, NULL::uuid FROM organizations
		WHERE $1::uuid IS NULL AND is_customer AND (code ILIKE '%' || $2 || '%' OR name ILIKE '%' || $2 || '%')
		ORDER BY name LIMIT 5`, nil, q); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

func hits(ctx context.Context, h *Handlers, sql string, owner *uuid.UUID, q string) ([]SearchHit, error) {
	rows, err := h.Pool.Query(ctx, sql, owner, q)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SearchHit])
}
