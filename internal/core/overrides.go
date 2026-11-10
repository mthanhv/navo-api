package core

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// OverrideInfo is the latest approval request of a document, shown to its owner.
type OverrideInfo struct {
	ID           uuid.UUID `json:"id"`
	Status       string    `json:"status"`
	Reason       string    `json:"reason"`
	DecisionNote *string   `json:"decision_note"`
}

func latestOverride(ctx context.Context, q db.Querier, kind string, docID uuid.UUID) (*OverrideInfo, error) {
	var o OverrideInfo
	err := q.QueryRow(ctx, `SELECT id, status, reason, decision_note FROM quantity_override_requests
		WHERE kind = $1 AND document_id = $2 ORDER BY requested_at DESC LIMIT 1`, kind, docID).Scan(&o.ID, &o.Status, &o.Reason, &o.DecisionNote)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

type overrideInput struct {
	Reason      string  `json:"reason"`
	DeliveredOn *string `json:"delivered_on"`
}

// RequestPOOverride handles POST /purchase-orders/:id/override-request.
func (h *Handlers) RequestPOOverride(c *gin.Context) { h.requestOverride(c, "purchase_order") }

// RequestDOOverride handles POST /delivery-orders/:id/override-request.
func (h *Handlers) RequestDOOverride(c *gin.Context) { h.requestOverride(c, "delivery_order") }

func (h *Handlers) requestOverride(c *gin.Context, kind string) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in overrideInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		errs.Add("reason", "required", nil)
	} else if len([]rune(in.Reason)) > 1000 {
		errs.Add("reason", "too_long", map[string]any{"max": 1000})
	}
	delivered := httpx.Date(&errs, "delivered_on", in.DeliveredOn)
	if kind == "delivery_order" && delivered == nil && len(errs) == 0 {
		errs.Add("delivered_on", "required", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	table := map[string]string{"purchase_order": "purchase_orders", "delivery_order": "delivery_orders"}[kind]
	httpx.Command(c, h.Pool, kind+".override_request", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var status string
		var contractID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT d.status, d.contract_id FROM `+table+` d JOIN contracts c ON c.id = d.contract_id
			WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF d`, id, p.OwnerFilter()).Scan(&status, &contractID); err != nil {
			return nil, err
		}
		if status != "draft" {
			return nil, apperrors.Rule("document_not_draft", "Only a draft document can be sent for approval.", map[string]any{"status": status})
		}
		var o OverrideInfo
		err := tx.QueryRow(ctx, `INSERT INTO quantity_override_requests (kind, document_id, contract_id, reason, delivered_on, requested_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, status, reason, decision_note`, kind, id, contractID, in.Reason, delivered, p.UserID).
			Scan(&o.ID, &o.Status, &o.Reason, &o.DecisionNote)
		if err != nil {
			if apperrors.From(err).Reason == "duplicate" {
				return nil, apperrors.Rule("override_already_pending", "This document is already waiting for Admin approval.", nil)
			}
			return nil, err
		}
		return o, audit.Record(ctx, tx, "request_override", kind, id, map[string]any{"request_id": o.ID, "reason": in.Reason})
	})
}

// OverrideLine shows Admin what is asked, in quantities only (no purchase prices).
type OverrideLine struct {
	LineNo           int             `json:"line_no"`
	NameOnContract   string          `json:"name_on_contract"`
	Unit             string          `json:"unit"`
	ContractQuantity decimal.Decimal `json:"contract_quantity"`
	AlreadyDone      decimal.Decimal `json:"already_done"`
	Requested        decimal.Decimal `json:"requested"`
	Over             decimal.Decimal `json:"over"`
}

// OverrideRequest is one approval request as Admin sees it.
type OverrideRequest struct {
	ID             uuid.UUID      `json:"id"`
	Kind           string         `json:"kind"`
	DocumentID     uuid.UUID      `json:"document_id"`
	DocumentNumber string         `json:"document_number"`
	ContractID     uuid.UUID      `json:"contract_id"`
	ContractNumber string         `json:"contract_number"`
	CustomerName   string         `json:"customer_name"`
	Reason         string         `json:"reason"`
	DeliveredOn    *string        `json:"delivered_on"`
	Status         string         `json:"status"`
	RequestedBy    string         `json:"requested_by"`
	RequestedAt    time.Time      `json:"requested_at"`
	DecisionNote   *string        `json:"decision_note"`
	DecidedBy      *string        `json:"decided_by"`
	Lines          []OverrideLine `json:"lines"`
}

const overrideSelect = `SELECT r.id, r.kind, r.document_id, coalesce(po.number, d.number), r.contract_id, c.number, o.name, r.reason, r.delivered_on,
		r.status, ru.full_name, r.requested_at, r.decision_note, du.full_name
	FROM quantity_override_requests r
	JOIN contracts c ON c.id = r.contract_id JOIN organizations o ON o.id = c.customer_id JOIN users ru ON ru.id = r.requested_by
	LEFT JOIN users du ON du.id = r.decided_by
	LEFT JOIN purchase_orders po ON r.kind = 'purchase_order' AND po.id = r.document_id
	LEFT JOIN delivery_orders d ON r.kind = 'delivery_order' AND d.id = r.document_id`

func scanOverride(ctx context.Context, q db.Querier, row pgx.Row) (*OverrideRequest, error) {
	var r OverrideRequest
	var delivered *time.Time
	if err := row.Scan(&r.ID, &r.Kind, &r.DocumentID, &r.DocumentNumber, &r.ContractID, &r.ContractNumber, &r.CustomerName, &r.Reason, &delivered,
		&r.Status, &r.RequestedBy, &r.RequestedAt, &r.DecisionNote, &r.DecidedBy); err != nil {
		return nil, err
	}
	r.DeliveredOn = dateString(delivered)
	lineSQL := map[string]string{
		"purchase_order": `SELECT cl.line_no, cl.name_on_contract, cl.unit, cl.quantity::text, pol.quantity::text,
				coalesce((SELECT sum(x.quantity) FROM purchase_order_lines x JOIN purchase_orders xp ON xp.id = x.purchase_order_id
				          WHERE x.contract_line_id = cl.id AND xp.status = 'confirmed'), 0)::text
			FROM purchase_order_lines pol JOIN contract_lines cl ON cl.id = pol.contract_line_id WHERE pol.purchase_order_id = $1 ORDER BY cl.line_no`,
		"delivery_order": `SELECT cl.line_no, cl.name_on_contract, cl.unit, cl.quantity::text, dol.quantity::text,
				coalesce((SELECT sum(x.quantity) FROM delivery_order_lines x JOIN delivery_orders xd ON xd.id = x.delivery_order_id
				          WHERE x.contract_line_id = cl.id AND xd.status = 'confirmed'), 0)::text
			FROM delivery_order_lines dol JOIN contract_lines cl ON cl.id = dol.contract_line_id WHERE dol.delivery_order_id = $1 ORDER BY cl.line_no`,
	}[r.Kind]
	rows, err := q.Query(ctx, lineSQL, r.DocumentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r.Lines = []OverrideLine{}
	for rows.Next() {
		var l OverrideLine
		var cq, rq, done string
		if err := rows.Scan(&l.LineNo, &l.NameOnContract, &l.Unit, &cq, &rq, &done); err != nil {
			return nil, err
		}
		l.ContractQuantity, l.Requested, l.AlreadyDone = decimalx.MustParse(cq), decimalx.MustParse(rq), decimalx.MustParse(done)
		l.Over = decimal.Max(l.AlreadyDone.Add(l.Requested).Sub(l.ContractQuantity), decimal.Zero)
		r.Lines = append(r.Lines, l)
	}
	return &r, rows.Err()
}

// ListOverrides handles GET /override-requests?status=pending|decided (Admin).
func (h *Handlers) ListOverrides(c *gin.Context) {
	ctx := c.Request.Context()
	page, size := httpx.Page(c)
	decided := c.Query("status") == "decided"
	var total int
	if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM quantity_override_requests WHERE (status = 'pending') = $1 AND status <> 'withdrawn'`, !decided).Scan(&total); err != nil {
		response.Fail(c, err)
		return
	}
	order := map[bool]string{false: `requested_at`, true: `decided_at DESC`}[decided]
	rows, err := h.Pool.Query(ctx, `SELECT id FROM quantity_override_requests WHERE (status = 'pending') = $1 AND status <> 'withdrawn' ORDER BY `+order+`, id LIMIT $2 OFFSET $3`,
		!decided, size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		response.Fail(c, err)
		return
	}
	items := []*OverrideRequest{}
	for _, id := range ids {
		r, err := scanOverride(ctx, h.Pool, h.Pool.QueryRow(ctx, overrideSelect+` WHERE r.id = $1`, id))
		if err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, r)
	}
	response.List(c, items, page, size, total)
}

type decisionInput struct {
	Note *string `json:"note"`
}

// ApproveOverride handles POST /override-requests/:id/approve: the document is
// confirmed in the same transaction, beyond the contract quantity but never beyond
// goods actually received.
func (h *Handlers) ApproveOverride(c *gin.Context) { h.decide(c, true) }

// RejectOverride handles POST /override-requests/:id/reject; a note is required.
func (h *Handlers) RejectOverride(c *gin.Context) { h.decide(c, false) }

func (h *Handlers) decide(c *gin.Context, approve bool) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in decisionInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	note := trimmed(in.Note)
	if !approve && note == nil {
		response.Fail(c, apperrors.Invalid("note", "required", nil))
		return
	}
	scope := "override_requests.reject"
	if approve {
		scope = "override_requests.approve"
	}
	httpx.Command(c, h.Pool, scope, true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var kind, status string
		var docID uuid.UUID
		var delivered *time.Time
		if err := tx.QueryRow(ctx, `SELECT kind, document_id, status, delivered_on FROM quantity_override_requests WHERE id = $1 FOR UPDATE`, id).
			Scan(&kind, &docID, &status, &delivered); err != nil {
			return nil, err
		}
		if status != "pending" {
			return nil, apperrors.Rule("override_already_decided", "This request has already been decided.", map[string]any{"status": status})
		}
		newStatus := "rejected"
		if approve {
			newStatus = "approved"
			var err error
			if kind == "purchase_order" {
				err = confirmPOTx(ctx, tx, docID, p.UserID, true)
			} else {
				err = confirmDOTx(ctx, tx, docID, *delivered, p.UserID, true)
			}
			if err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE quantity_override_requests SET status = $2, decided_by = $3, decided_at = now(), decision_note = $4 WHERE id = $1`,
			id, newStatus, p.UserID, note); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, newStatus, "override_request", id, map[string]any{"status": []string{"pending", newStatus}, "note": note,
			"document_id": docID}); err != nil {
			return nil, err
		}
		return scanOverride(ctx, tx, tx.QueryRow(ctx, overrideSelect+` WHERE r.id = $1`, id))
	})
}
