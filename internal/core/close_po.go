package core

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// ClosePurchaseOrder cancels only the unreceived balance, preserving original
// quantities and a separate immutable closure record.
func (h *Handlers) ClosePurchaseOrder(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		response.Fail(c, apperrors.Invalid("reason", "required", nil))
		return
	}
	httpx.Command(c, h.Pool, "purchase_orders.close", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var cid uuid.UUID
		var status string
		if err := tx.QueryRow(ctx, `SELECT po.contract_id,po.status FROM purchase_orders po JOIN contracts c ON c.id=po.contract_id WHERE po.id=$1 AND ($2::uuid IS NULL OR c.owner_id=$2) FOR UPDATE OF po`, id, p.OwnerFilter()).Scan(&cid, &status); err != nil {
			return nil, err
		}
		if status != "confirmed" {
			return nil, apperrors.Rule("purchase_order_not_confirmed", "Only a confirmed PO can be closed.", nil)
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM purchase_order_lines WHERE purchase_order_id=$1 ORDER BY id FOR UPDATE`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM contract_lines WHERE contract_id=$1 ORDER BY id FOR UPDATE`, cid); err != nil {
			return nil, err
		}
		var drafts bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM goods_receipts WHERE purchase_order_id=$1 AND status='draft')`, id).Scan(&drafts); err != nil {
			return nil, err
		}
		if drafts {
			return nil, apperrors.Rule("has_draft_receipts", "Cancel draft receipts before closing the PO.", nil)
		}
		rows, err := tx.Query(ctx, `SELECT pol.id,(pol.quantity-pol.cancelled_quantity-po_line_received(pol.id))::text FROM purchase_order_lines pol WHERE pol.purchase_order_id=$1 ORDER BY pol.id`, id)
		if err != nil {
			return nil, err
		}
		quantities := map[string]string{}
		for rows.Next() {
			var lid uuid.UUID
			var raw string
			if err := rows.Scan(&lid, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			v, err := decimal.NewFromString(raw)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if v.IsPositive() {
				quantities[lid.String()] = v.String()
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(quantities) == 0 {
			return nil, apperrors.Rule("nothing_to_close", "No outstanding quantity remains.", nil)
		}
		for lid, qty := range quantities {
			if _, err := tx.Exec(ctx, `UPDATE purchase_order_lines SET cancelled_quantity=cancelled_quantity+$2 WHERE id=$1`, lid, qty); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO purchase_order_closures(purchase_order_id,quantities,reason,created_by) VALUES($1,$2,$3,$4)`, id, quantities, in.Reason, p.UserID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at=now() WHERE contract_id=$1`, cid); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "close_outstanding", "purchase_order", id, map[string]any{"reason": in.Reason, "quantities": quantities}); err != nil {
			return nil, err
		}
		return loadPO(ctx, tx, id, p)
	})
}
