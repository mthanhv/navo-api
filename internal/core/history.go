package core

import (
	"time"

	"github.com/gin-gonic/gin"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/auth"
	"navo-api/internal/documents"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// HistoryEntry is one recorded change to a contract or one of its documents.
// Only who, when, what and the person's own reason or note are shown: the raw audit
// payload can hold purchase prices and is never returned.
type HistoryEntry struct {
	At          time.Time `json:"at"`
	Actor       *string   `json:"actor"`
	Action      string    `json:"action"`
	EntityType  string    `json:"entity_type"`
	EntityLabel string    `json:"entity_label"`
	Detail      *string   `json:"detail"`
}

// financeEntities are hidden from people without finance access.
var financeEntities = map[string]bool{"receipt": true, "receipt_allocation": true, "sales_invoice": true}

// contractEntitiesSQL lists everything that belongs to contract $1 as (type, id, label,
// owner_type, doc_kind); the last two are set for files so their visibility can be checked.
const contractEntitiesSQL = `
SELECT 'contract', c.id, coalesce(c.number, c.draft_number), NULL::text, NULL::text FROM contracts c WHERE c.id = $1
UNION ALL SELECT 'purchase_order', id, number, NULL, NULL FROM purchase_orders WHERE contract_id = $1
UNION ALL SELECT 'goods_receipt', id, number, NULL, NULL FROM goods_receipts WHERE contract_id = $1
UNION ALL SELECT 'delivery_order', id, number, NULL, NULL FROM delivery_orders WHERE contract_id = $1
UNION ALL SELECT 'payment_milestone', id, name, NULL, NULL FROM payment_milestones WHERE contract_id = $1
UNION ALL SELECT 'sales_invoice', id, number, NULL, NULL FROM sales_invoices WHERE contract_id = $1
UNION ALL SELECT 'override_request', r.id, coalesce(po.number, d.number, ''), NULL, NULL FROM quantity_override_requests r
	LEFT JOIN purchase_orders po ON r.kind = 'purchase_order' AND po.id = r.document_id
	LEFT JOIN delivery_orders d ON r.kind = 'delivery_order' AND d.id = r.document_id WHERE r.contract_id = $1
UNION ALL SELECT 'receipt_allocation', a.id, rc.number, NULL, NULL FROM receipt_allocations a
	JOIN payment_milestones m ON m.id = a.milestone_id JOIN receipts rc ON rc.id = a.receipt_id WHERE m.contract_id = $1
UNION ALL SELECT DISTINCT 'receipt', rc.id, rc.number, NULL, NULL FROM receipts rc
	JOIN receipt_allocations a ON a.receipt_id = rc.id JOIN payment_milestones m ON m.id = a.milestone_id WHERE m.contract_id = $1
UNION ALL SELECT 'acceptance', id, number, NULL, NULL FROM acceptances WHERE contract_id = $1
UNION ALL SELECT 'customer_return', id, number, NULL, NULL FROM customer_returns WHERE contract_id = $1
UNION ALL SELECT 'supplier_return', id, number, NULL, NULL FROM supplier_returns WHERE contract_id = $1
UNION ALL SELECT DISTINCT 'file', l.file_id, f.original_name, l.owner_type, l.doc_kind FROM attachment_links l
	JOIN files f ON f.id = l.file_id WHERE l.contract_id = $1`

// ContractHistory handles GET /contracts/:id/history: newest first, at most 300 entries,
// within the caller's scope and field permissions.
func (h *Handlers) ContractHistory(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var visible bool
	if err := h.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contracts WHERE id = $1 AND ($2::uuid IS NULL OR owner_id = $2))`, id, p.OwnerFilter()).Scan(&visible); err != nil {
		response.Fail(c, err)
		return
	}
	if !visible {
		response.Fail(c, apperrors.NotFound())
		return
	}
	rows, err := h.Pool.Query(ctx, `WITH ents(entity_type, entity_id, label, owner_type, doc_kind) AS (`+contractEntitiesSQL+`)
		SELECT a.occurred_at, u.full_name, a.action, a.entity_type, e.label,
		       coalesce(a.changes->>'reason', a.changes->>'note'), e.owner_type, e.doc_kind
		FROM audit_logs a JOIN ents e ON e.entity_type = a.entity_type AND e.entity_id = a.entity_id
		LEFT JOIN users u ON u.id = a.actor_id
		ORDER BY a.occurred_at DESC, a.id DESC LIMIT 300`, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		var ownerType, docKind *string
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.EntityType, &e.EntityLabel, &e.Detail, &ownerType, &docKind); err != nil {
			response.Fail(c, err)
			return
		}
		if financeEntities[e.EntityType] && !p.Can(access.FinanceRead) {
			continue
		}
		if e.EntityType == "file" && (ownerType == nil || docKind == nil || !documents.Visible(p, *ownerType, *docKind)) {
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}
