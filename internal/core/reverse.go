package core

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// ReverseFulfillment corrects the entire original document. Actual partial
// returns remain a separate workflow; callers must not invent netting edits.
func (h *Handlers) ReverseFulfillment(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		table := map[string]string{"goods_receipt": "goods_receipts", "delivery_order": "delivery_orders"}[kind]
		httpx.Command(c, h.Pool, kind+".reverse", true, 200, func(tx pgx.Tx) (any, error) {
			ctx := c.Request.Context()
			var contractID uuid.UUID
			var status string
			if err := tx.QueryRow(ctx, `SELECT d.contract_id,d.status FROM `+table+` d JOIN contracts c ON c.id=d.contract_id WHERE d.id=$1 AND ($2::uuid IS NULL OR c.owner_id=$2) FOR UPDATE OF d`, id, p.OwnerFilter()).Scan(&contractID, &status); err != nil {
				return nil, err
			}
			if status != "confirmed" {
				return nil, apperrors.Rule("document_not_confirmed", "Only a confirmed document can be reversed.", nil)
			}
			// Same line lock order as confirmation. A reversal may never create negative stock.
			if _, err := tx.Exec(ctx, `SELECT id FROM contract_lines WHERE contract_id=$1 ORDER BY id FOR UPDATE`, contractID); err != nil {
				return nil, err
			}
			if kind == "goods_receipt" {
				var invalid bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (
     SELECT 1 FROM contract_lines cl WHERE cl.contract_id=$1 AND
      coalesce((SELECT sum(g.accepted_quantity) FROM goods_receipt_lines g JOIN goods_receipts r ON r.id=g.goods_receipt_id JOIN purchase_order_lines pol ON pol.id=g.purchase_order_line_id WHERE pol.contract_line_id=cl.id AND r.status='confirmed' AND r.id<>$2),0)
       < coalesce((SELECT sum(l.quantity) FROM delivery_order_lines l JOIN delivery_orders d ON d.id=l.delivery_order_id WHERE l.contract_line_id=cl.id AND d.status='confirmed'),0))`, contractID, id).Scan(&invalid); err != nil {
					return nil, err
				}
				if invalid {
					return nil, apperrors.Rule("reversal_has_dependents", "Reverse dependent deliveries before this receipt.", nil)
				}
			} else {
				// Serialize with finance allocation/trigger checks before reading
				// their dependent totals; a reversal cannot race a new collection.
				if _, err := tx.Exec(ctx, `SELECT id FROM payment_milestones WHERE contract_id=$1 ORDER BY id FOR UPDATE`, contractID); err != nil {
					return nil, err
				}
				var dependent bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_milestones m WHERE m.contract_id=$1 AND (
       m.trigger='acceptance' AND m.triggered_on IS NOT NULL OR m.trigger='delivery_complete' AND EXISTS(SELECT 1 FROM receipt_allocations a WHERE a.milestone_id=m.id AND a.reversed_at IS NULL)))`, contractID).Scan(&dependent); err != nil {
					return nil, err
				}
				if dependent {
					return nil, apperrors.Rule("reversal_has_dependents", "Correct acceptance or dependent payment allocations before reversing this delivery.", nil)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE `+table+` SET status='reversed',version=version+1,updated_at=now() WHERE id=$1`, id); err != nil {
				return nil, err
			}
			var reversalID uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO document_reversals(document_type,document_id,reason,created_by) VALUES($1,$2,$3,$4) RETURNING id`, kind, id, in.Reason, p.UserID).Scan(&reversalID); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at=now() WHERE contract_id=$1`, contractID); err != nil {
				return nil, err
			}
			return gin.H{"id": id, "status": "reversed", "reversal_id": reversalID}, audit.Record(ctx, tx, "reverse", kind, id, map[string]any{"reason": in.Reason, "reversal_id": reversalID})
		})
	}
}
