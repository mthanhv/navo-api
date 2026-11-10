package core

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// DeleteContract handles DELETE /contracts/:id: a draft contract has no official
// number and no documents yet, so a mistaken one is removed (its lines go with it).
func (h *Handlers) DeleteContract(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contracts.delete", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		k, err := lockContract(ctx, tx, id, p, true)
		if err != nil {
			return nil, err
		}
		if k.Status != "draft" {
			return nil, apperrors.Rule("document_not_draft", "Only a draft contract can be deleted.", map[string]any{"status": k.Status})
		}
		var draftNumber string
		if err := tx.QueryRow(ctx, `DELETE FROM contracts WHERE id = $1 RETURNING draft_number`, id).Scan(&draftNumber); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "deleted": true}, audit.Record(ctx, tx, "delete", "contract", id, map[string]any{"draft_number": draftNumber})
	})
}

// cancelSpec describes one cancellable document table.
type cancelSpec struct {
	table, entity, kind string // kind: override request kind, "" when the document has none
}

var (
	cancelPO = cancelSpec{"purchase_orders", "purchase_order", "purchase_order"}
	cancelGR = cancelSpec{"goods_receipts", "goods_receipt", ""}
	cancelDO = cancelSpec{"delivery_orders", "delivery_order", "delivery_order"}
)

// CancelPurchaseOrder handles POST /purchase-orders/:id/cancel.
func (h *Handlers) CancelPurchaseOrder(c *gin.Context) { h.cancel(c, cancelPO) }

// CancelGoodsReceipt handles POST /goods-receipts/:id/cancel.
func (h *Handlers) CancelGoodsReceipt(c *gin.Context) { h.cancel(c, cancelGR) }

// CancelDeliveryOrder handles POST /delivery-orders/:id/cancel.
func (h *Handlers) CancelDeliveryOrder(c *gin.Context) { h.cancel(c, cancelDO) }

// cancel marks a draft document cancelled: it keeps its number and lines for the
// record but no longer counts anywhere (every quantity reads confirmed documents
// and plans read drafts). An open Admin request for it is withdrawn.
func (h *Handlers) cancel(c *gin.Context, s cancelSpec) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, s.table+".cancel", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status, number string
		if err := tx.QueryRow(ctx, `SELECT d.status, d.number FROM `+s.table+` d JOIN contracts c ON c.id = d.contract_id
			WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF d`, id, p.OwnerFilter()).Scan(&status, &number); err != nil {
			return nil, err
		}
		if status != "draft" {
			return nil, apperrors.Rule("document_not_draft", "Only a draft can be cancelled.", map[string]any{"status": status})
		}
		if s.entity == "purchase_order" {
			// A draft goods receipt against this order would be left pointing at nothing useful.
			var drafts int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM goods_receipts WHERE purchase_order_id = $1 AND status = 'draft'`, id).Scan(&drafts); err != nil {
				return nil, err
			}
			if drafts > 0 {
				return nil, apperrors.Rule("has_draft_receipts", "Cancel the draft goods receipts of this order first.", map[string]any{"count": drafts})
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table+` SET status = 'cancelled', version = version + 1, updated_at = now() WHERE id = $1`, id); err != nil {
			return nil, err
		}
		if s.kind != "" {
			if err := withdrawOverride(ctx, tx, s.kind, id, p.UserID); err != nil {
				return nil, err
			}
		}
		return gin.H{"id": id, "number": number, "status": "cancelled"},
			audit.Record(ctx, tx, "cancel", s.entity, id, map[string]any{"status": []string{"draft", "cancelled"}})
	})
}

func withdrawOverride(ctx context.Context, tx pgx.Tx, kind string, docID, actor uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE quantity_override_requests SET status = 'withdrawn', decided_by = $3, decided_at = now()
		WHERE kind = $1 AND document_id = $2 AND status = 'pending'`, kind, docID, actor)
	return err
}
