package core

// Contract lifecycle after activation (REQUIREMENTS N-04, N-06, BR-06, AC-05/06/07/10):
// close, cancel and reopen; amendments; acceptance; customer and supplier returns.
// Quantities are always net: line_received / line_delivered / line_accepted in SQL.

import (
	"context"
	"strconv"
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
	"navo-api/internal/finance"
	"navo-api/internal/httpx"
	"navo-api/internal/numbering"
	"navo-api/internal/response"
)

func reasonOf(errs *apperrors.FieldErrors, field, raw string) string {
	r := strings.TrimSpace(raw)
	if r == "" {
		errs.Add(field, "required", nil)
	} else if len([]rune(r)) > 500 {
		errs.Add(field, "too_long", map[string]any{"max": 500})
	}
	return r
}

// eventDate requires a date that is not in the future and not before the contract was signed.
func eventDate(ctx context.Context, q db.Querier, errs *apperrors.FieldErrors, field string, raw *string, contractID uuid.UUID, today time.Time) *time.Time {
	d := httpx.Date(errs, field, raw)
	if d == nil {
		if raw == nil || *raw == "" {
			errs.Add(field, "required", nil)
		}
		return nil
	}
	if d.After(today) {
		errs.Add(field, "not_future", nil)
		return nil
	}
	var signed *time.Time
	if err := q.QueryRow(ctx, `SELECT signed_on FROM contracts WHERE id = $1`, contractID).Scan(&signed); err == nil && signed != nil && d.Before(*signed) {
		errs.Add(field, "before_signing", map[string]any{"signed_on": signed.Format(time.DateOnly)})
		return nil
	}
	return d
}

// lockAllLines locks a contract's lines in id order, the order every confirmation uses.
func lockAllLines(ctx context.Context, tx pgx.Tx, contractID uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT id FROM contract_lines WHERE contract_id = $1 ORDER BY id FOR UPDATE`, contractID)
	return err
}

// ---------------------------------------------------------------- close, cancel, reopen

type reasonInput struct {
	Reason string `json:"reason"`
}

// ChangeContractStatus handles POST /contracts/:id/close, /cancel and /reopen.
//   - close: every line fully delivered (net) and nothing left in draft. Money still owed
//     stays in receivables; closing only ends operational work.
//   - cancel: nothing delivered (net), nothing still on order, no drafts, no collected money
//     and no live sales invoices. Received stock may remain and is named in the reason.
//   - reopen: a closed or cancelled contract becomes active again, for corrections.
func (h *Handlers) ChangeContractStatus(to string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := auth.Principal(c)
		id, err := httpx.PathID(c, "id")
		if err != nil {
			response.Fail(c, err)
			return
		}
		var in reasonInput
		if err := httpx.Decode(c, &in); err != nil {
			response.Fail(c, err)
			return
		}
		var errs apperrors.FieldErrors
		in.Reason = reasonOf(&errs, "reason", in.Reason)
		if err := errs.Err(); err != nil {
			response.Fail(c, err)
			return
		}
		httpx.Command(c, h.Pool, "contracts."+to, true, 200, func(tx pgx.Tx) (any, error) {
			ctx := c.Request.Context()
			ref, err := lockContract(ctx, tx, id, p, true)
			if err != nil {
				return nil, err
			}
			if err := lockAllLines(ctx, tx, id); err != nil {
				return nil, err
			}
			from := ref.Status
			switch to {
			case "active":
				if from != "closed" && from != "cancelled" {
					return nil, apperrors.Rule("contract_not_closed", "Only a closed or cancelled contract can be reopened.", nil)
				}
			case "closed", "cancelled":
				if from != "active" {
					return nil, apperrors.Rule("contract_not_active", "Only an active contract can be closed or cancelled.", nil)
				}
				var drafts bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM purchase_orders WHERE contract_id = $1 AND status = 'draft')
					OR EXISTS (SELECT 1 FROM goods_receipts WHERE contract_id = $1 AND status = 'draft')
					OR EXISTS (SELECT 1 FROM delivery_orders WHERE contract_id = $1 AND status = 'draft')`, id).Scan(&drafts); err != nil {
					return nil, err
				}
				if drafts {
					return nil, apperrors.Rule("contract_has_drafts", "Confirm or cancel the draft purchase orders, receipts and deliveries first.", nil)
				}
			}
			if to == "closed" {
				var open bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contract_lines cl WHERE cl.contract_id = $1 AND line_delivered(cl.id) < cl.quantity)`, id).Scan(&open); err != nil {
					return nil, err
				}
				if open {
					return nil, apperrors.Rule("contract_not_delivered", "Every line must be fully delivered before the contract is closed. Use an amendment to reduce a quantity, or cancel instead.", nil)
				}
			}
			if to == "cancelled" {
				var blocked string
				if err := tx.QueryRow(ctx, `SELECT CASE
					WHEN EXISTS (SELECT 1 FROM contract_lines cl WHERE cl.contract_id = $1 AND line_delivered(cl.id) > 0) THEN 'delivered'
					WHEN EXISTS (SELECT 1 FROM purchase_order_lines pol JOIN purchase_orders po ON po.id = pol.purchase_order_id
					             WHERE po.contract_id = $1 AND po.status = 'confirmed' AND pol.quantity - pol.cancelled_quantity > po_line_received(pol.id)) THEN 'on_order'
					WHEN EXISTS (SELECT 1 FROM receipt_allocations a JOIN payment_milestones m ON m.id = a.milestone_id WHERE m.contract_id = $1 AND a.reversed_at IS NULL) THEN 'collected'
					WHEN EXISTS (SELECT 1 FROM sales_invoices WHERE contract_id = $1 AND status <> 'cancelled') THEN 'invoiced'
					ELSE '' END`, id).Scan(&blocked); err != nil {
					return nil, err
				}
				if blocked != "" {
					return nil, apperrors.Rule("contract_cancel_blocked", "This contract has activity that must be corrected before it can be cancelled.", map[string]any{"because": blocked})
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE contracts SET status = $2, version = version + 1, updated_at = now() WHERE id = $1`, id, to); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO contract_status_changes (contract_id, from_status, to_status, reason, created_by) VALUES ($1, $2, $3, $4, $5)`,
				id, from, to, in.Reason, p.UserID); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at = now() WHERE contract_id = $1`, id); err != nil {
				return nil, err
			}
			action := map[string]string{"active": "reopen", "closed": "close", "cancelled": "cancel"}[to]
			if err := audit.Record(ctx, tx, action, "contract", id, map[string]any{"from": from, "to": to, "reason": in.Reason}); err != nil {
				return nil, err
			}
			return h.loadContract(ctx, tx, id, p)
		})
	}
}

// ---------------------------------------------------------------- amendments

type amendLineInput struct {
	ContractLineID string  `json:"contract_line_id"`
	Quantity       string  `json:"quantity"`
	UnitPrice      string  `json:"unit_price"`
	DueOn          *string `json:"due_on"`
}

type amendNewLineInput struct {
	ProductID      string  `json:"product_id"`
	NameOnContract *string `json:"name_on_contract"`
	Unit           *string `json:"unit"`
	Quantity       string  `json:"quantity"`
	UnitPrice      string  `json:"unit_price"`
	TaxRate        *string `json:"tax_rate"`
	DueOn          *string `json:"due_on"`
}

type amendInput struct {
	Version   int64               `json:"version"`
	SignedOn  *string             `json:"signed_on"`
	Reference *string             `json:"reference"`
	Reason    string              `json:"reason"`
	Lines     []amendLineInput    `json:"lines"`
	NewLines  []amendNewLineInput `json:"new_lines"`
}

// Amendment is a signed change to an active contract (phụ lục), kept as it was signed.
type Amendment struct {
	ID        uuid.UUID         `json:"id"`
	Number    string            `json:"number"`
	SignedOn  string            `json:"signed_on"`
	Reference *string           `json:"reference"`
	Reason    string            `json:"reason"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	Lines     []AmendmentChange `json:"lines"`
}

// AmendmentChange is one line before and after; Before is null for an added line.
type AmendmentChange struct {
	ContractLineID uuid.UUID      `json:"contract_line_id"`
	LineNo         int            `json:"line_no"`
	Change         string         `json:"change"`
	Before         map[string]any `json:"before"`
	After          map[string]any `json:"after"`
}

func lineSnapshot(qty, price decimal.Decimal, due *time.Time) map[string]any {
	return map[string]any{"quantity": qty.String(), "unit_price": price.String(), "due_on": dateString(due)}
}

// AmendContract handles POST /contracts/:id/amendments. Quantities may not go below what
// was delivered or accepted (BR-06); product and unit never change on a used line, so a
// different item is a new line. Payment terms are not recalculated: the money tab shows
// when terms no longer add up to the contract value.
func (h *Handlers) AmendContract(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in amendInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contracts.amend", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		ref, err := lockContract(ctx, tx, id, p, true)
		if err != nil {
			return nil, err
		}
		if ref.Status != "active" {
			return nil, apperrors.Rule("contract_not_active", "Only an active contract can be amended.", nil)
		}
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM contracts WHERE id = $1`, id).Scan(&version); err != nil {
			return nil, err
		}
		if version != in.Version {
			return nil, apperrors.Conflict("version_conflict", "The contract changed since you opened it. Reload and try again.", nil)
		}
		if err := lockAllLines(ctx, tx, id); err != nil {
			return nil, err
		}
		var errs apperrors.FieldErrors
		signed := eventDate(ctx, tx, &errs, "signed_on", in.SignedOn, id, h.today())
		reason := reasonOf(&errs, "reason", in.Reason)
		reference := trimmed(in.Reference)
		if reference != nil && len([]rune(*reference)) > 100 {
			errs.Add("reference", "too_long", map[string]any{"max": 100})
		}
		if len(in.Lines) == 0 && len(in.NewLines) == 0 {
			errs.Add("lines", "required", nil)
		}
		type change struct {
			line               uuid.UUID
			lineNo             int
			before, after      map[string]any
			qty, price         decimal.Decimal
			due                *time.Time
			qtyUp, deliveredUp bool
		}
		var changes []change
		seen := map[uuid.UUID]bool{}
		for i, l := range in.Lines {
			f := func(name string) string { return "lines[" + itoa(i) + "]." + name }
			lid := httpx.UUID(&errs, f("contract_line_id"), l.ContractLineID)
			qty := decimalx.Positive(&errs, f("quantity"), l.Quantity)
			price := decimalx.NonNegative(&errs, f("unit_price"), l.UnitPrice)
			due := httpx.Date(&errs, f("due_on"), l.DueOn)
			if lid == uuid.Nil || seen[lid] {
				if seen[lid] {
					errs.Add(f("contract_line_id"), "duplicate", nil)
				}
				continue
			}
			seen[lid] = true
			var lineNo int
			var oldQty, oldPrice, delivered, accepted string
			var oldDue *time.Time
			if err := tx.QueryRow(ctx, `SELECT line_no, quantity::text, unit_price::text, due_on, line_delivered(id)::text, line_accepted(id)::text
				FROM contract_lines WHERE id = $1 AND contract_id = $2`, lid, id).Scan(&lineNo, &oldQty, &oldPrice, &oldDue, &delivered, &accepted); err != nil {
				if err == pgx.ErrNoRows {
					errs.Add(f("contract_line_id"), "not_in_contract", nil)
					continue
				}
				return nil, err
			}
			oq, op, dq, aq := decimalx.MustParse(oldQty), decimalx.MustParse(oldPrice), decimalx.MustParse(delivered), decimalx.MustParse(accepted)
			if floor := decimal.Max(dq, aq); qty.LessThan(floor) {
				errs.Add(f("quantity"), "below_delivered", map[string]any{"min": floor.String()})
				continue
			}
			sameDue := (oldDue == nil && due == nil) || (oldDue != nil && due != nil && oldDue.Equal(*due))
			if qty.Equal(oq) && price.Equal(op) && sameDue {
				errs.Add(f("quantity"), "unchanged", nil)
				continue
			}
			changes = append(changes, change{line: lid, lineNo: lineNo, before: lineSnapshot(oq, op, oldDue), after: lineSnapshot(qty, price, due),
				qty: qty, price: price, due: due, qtyUp: qty.GreaterThan(oq), deliveredUp: !dq.LessThan(oq)})
		}
		type added struct {
			product    uuid.UUID
			name, unit *string
			qty, price decimal.Decimal
			tax        *decimal.Decimal
			due        *time.Time
		}
		var adds []added
		for i, l := range in.NewLines {
			f := func(name string) string { return "new_lines[" + itoa(i) + "]." + name }
			a := added{product: httpx.UUID(&errs, f("product_id"), l.ProductID), qty: decimalx.Positive(&errs, f("quantity"), l.Quantity),
				price: decimalx.NonNegative(&errs, f("unit_price"), l.UnitPrice), due: httpx.Date(&errs, f("due_on"), l.DueOn),
				name: trimmed(l.NameOnContract), unit: trimmed(l.Unit)}
			if l.TaxRate != nil && strings.TrimSpace(*l.TaxRate) != "" {
				t := decimalx.NonNegative(&errs, f("tax_rate"), *l.TaxRate)
				if t.GreaterThan(decimal.NewFromInt(1)) {
					errs.Add(f("tax_rate"), "less_or_equal", map[string]any{"max": 1})
				}
				a.tax = &t
			}
			adds = append(adds, a)
		}
		if err := errs.Err(); err != nil {
			return nil, err
		}
		// A contract already fully delivered and paid "after full delivery" would become
		// undelivered again; that payment would then hang on an event that no longer holds.
		reopensDelivery := len(adds) > 0
		for _, ch := range changes {
			reopensDelivery = reopensDelivery || (ch.qtyUp && ch.deliveredUp)
		}
		if reopensDelivery {
			var paid bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_milestones m JOIN receipt_allocations a ON a.milestone_id = m.id
				WHERE m.contract_id = $1 AND m.trigger = 'delivery_complete' AND a.reversed_at IS NULL)
				AND NOT EXISTS (SELECT 1 FROM contract_lines cl WHERE cl.contract_id = $1 AND line_delivered(cl.id) < cl.quantity)`, id).Scan(&paid); err != nil {
				return nil, err
			}
			if paid {
				return nil, apperrors.Rule("amendment_affects_paid_term", "Money was collected on full delivery. Record the extra goods on a new contract, or reverse that allocation first.", nil)
			}
		}
		number, err := numbering.NextChild(ctx, tx, id, "AM")
		if err != nil {
			return nil, err
		}
		var aid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO contract_amendments (contract_id, number, signed_on, reference, reason, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, id, number, signed, reference, reason, p.UserID).Scan(&aid); err != nil {
			return nil, err
		}
		touched := []uuid.UUID{}
		for _, ch := range changes {
			if _, err := tx.Exec(ctx, `UPDATE contract_lines SET quantity = $2, unit_price = $3, due_on = $4 WHERE id = $1`, ch.line, ch.qty, ch.price, ch.due); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO contract_amendment_lines (amendment_id, contract_line_id, change, before, after) VALUES ($1, $2, 'changed', $3, $4)`,
				aid, ch.line, ch.before, ch.after); err != nil {
				return nil, err
			}
			touched = append(touched, ch.line)
		}
		if len(adds) > 0 {
			var next int
			if err := tx.QueryRow(ctx, `SELECT coalesce(max(line_no), 0) + 1 FROM contract_lines WHERE contract_id = $1`, id).Scan(&next); err != nil {
				return nil, err
			}
			for i, a := range adds {
				var pname, punit string
				if err := tx.QueryRow(ctx, `SELECT name, unit FROM products WHERE id = $1`, a.product).Scan(&pname, &punit); err != nil {
					if err == pgx.ErrNoRows {
						return nil, apperrors.Invalid("new_lines["+itoa(i)+"].product_id", "not_found", nil)
					}
					return nil, err
				}
				name, unit := pname, punit
				if a.name != nil {
					name = *a.name
				}
				if a.unit != nil {
					unit = *a.unit
				}
				var lid uuid.UUID
				if err := tx.QueryRow(ctx, `INSERT INTO contract_lines (contract_id, line_no, product_id, name_on_contract, unit, quantity, unit_price, tax_rate, due_on)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, id, next+i, a.product, name, unit, a.qty, a.price, a.tax, a.due).Scan(&lid); err != nil {
					return nil, err
				}
				after := lineSnapshot(a.qty, a.price, a.due)
				after["name_on_contract"], after["unit"] = name, unit
				if _, err := tx.Exec(ctx, `INSERT INTO contract_amendment_lines (amendment_id, contract_line_id, change, before, after) VALUES ($1, $2, 'added', NULL, $3)`,
					aid, lid, after); err != nil {
					return nil, err
				}
				touched = append(touched, lid)
			}
		}
		if err := touchLines(ctx, tx, touched); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE contracts SET version = version + 1, updated_at = now() WHERE id = $1`, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "amend", "contract", id, map[string]any{"number": number, "reason": reason, "changed": len(changes), "added": len(adds)}); err != nil {
			return nil, err
		}
		return loadAmendment(ctx, tx, aid)
	})
}

func loadAmendment(ctx context.Context, q db.Querier, id uuid.UUID) (*Amendment, error) {
	var a Amendment
	var signed time.Time
	if err := q.QueryRow(ctx, `SELECT a.id, a.number, a.signed_on, a.reference, a.reason, u.full_name, a.created_at
		FROM contract_amendments a JOIN users u ON u.id = a.created_by WHERE a.id = $1`, id).
		Scan(&a.ID, &a.Number, &signed, &a.Reference, &a.Reason, &a.CreatedBy, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.SignedOn = signed.Format(time.DateOnly)
	rows, err := q.Query(ctx, `SELECT l.contract_line_id, cl.line_no, l.change, l.before, l.after FROM contract_amendment_lines l
		JOIN contract_lines cl ON cl.id = l.contract_line_id WHERE l.amendment_id = $1 ORDER BY cl.line_no`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	a.Lines = []AmendmentChange{}
	for rows.Next() {
		var ch AmendmentChange
		if err := rows.Scan(&ch.ContractLineID, &ch.LineNo, &ch.Change, &ch.Before, &ch.After); err != nil {
			return nil, err
		}
		a.Lines = append(a.Lines, ch)
	}
	return &a, rows.Err()
}

// ---------------------------------------------------------------- acceptance and returns

type qtyLineInput struct {
	ContractLineID      string `json:"contract_line_id"`
	PurchaseOrderLineID string `json:"purchase_order_line_id"`
	Quantity            string `json:"quantity"`
}

type eventInput struct {
	On       *string        `json:"on"`
	Evidence string         `json:"evidence"`
	Reason   string         `json:"reason"`
	Lines    []qtyLineInput `json:"lines"`
}

// EventDoc is an acceptance, customer return or supplier return.
type EventDoc struct {
	ID              uuid.UUID      `json:"id"`
	Kind            string         `json:"kind"`
	Number          string         `json:"number"`
	On              string         `json:"on"`
	Note            string         `json:"note"`
	Status          string         `json:"status"`
	PurchaseOrderID *uuid.UUID     `json:"purchase_order_id,omitempty"`
	CreatedBy       string         `json:"created_by"`
	Lines           []EventDocLine `json:"lines"`
}

// EventDocLine is one quantity on an event document.
type EventDocLine struct {
	ContractLineID      uuid.UUID  `json:"contract_line_id"`
	PurchaseOrderLineID *uuid.UUID `json:"purchase_order_line_id,omitempty"`
	LineNo              int        `json:"line_no"`
	Name                string     `json:"name_on_contract"`
	Unit                string     `json:"unit"`
	Quantity            string     `json:"quantity"`
}

var eventTables = map[string]struct{ table, lines, fk, date, note, prefix string }{
	"acceptance":      {"acceptances", "acceptance_lines", "acceptance_id", "accepted_on", "evidence", "AC"},
	"customer_return": {"customer_returns", "customer_return_lines", "customer_return_id", "returned_on", "reason", "CR"},
	"supplier_return": {"supplier_returns", "supplier_return_lines", "supplier_return_id", "returned_on", "reason", "SR"},
}

func loadEventDoc(ctx context.Context, q db.Querier, kind string, id uuid.UUID) (*EventDoc, error) {
	t := eventTables[kind]
	d := EventDoc{Kind: kind}
	var on time.Time
	poCol := "NULL::uuid"
	if kind == "supplier_return" {
		poCol = "d.purchase_order_id"
	}
	if err := q.QueryRow(ctx, `SELECT d.id, d.number, d.`+t.date+`, d.`+t.note+`, d.status, `+poCol+`, u.full_name FROM `+t.table+` d
		JOIN users u ON u.id = d.created_by WHERE d.id = $1`, id).Scan(&d.ID, &d.Number, &on, &d.Note, &d.Status, &d.PurchaseOrderID, &d.CreatedBy); err != nil {
		return nil, err
	}
	d.On = on.Format(time.DateOnly)
	sql := `SELECT l.contract_line_id, NULL::uuid, cl.line_no, cl.name_on_contract, cl.unit, l.quantity::text FROM ` + t.lines + ` l
		JOIN contract_lines cl ON cl.id = l.contract_line_id WHERE l.` + t.fk + ` = $1 ORDER BY cl.line_no`
	if kind == "supplier_return" {
		sql = `SELECT pol.contract_line_id, l.purchase_order_line_id, cl.line_no, cl.name_on_contract, cl.unit, l.quantity::text FROM supplier_return_lines l
			JOIN purchase_order_lines pol ON pol.id = l.purchase_order_line_id JOIN contract_lines cl ON cl.id = pol.contract_line_id
			WHERE l.supplier_return_id = $1 ORDER BY cl.line_no`
	}
	rows, err := q.Query(ctx, sql, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Lines = []EventDocLine{}
	for rows.Next() {
		var l EventDocLine
		if err := rows.Scan(&l.ContractLineID, &l.PurchaseOrderLineID, &l.LineNo, &l.Name, &l.Unit, &l.Quantity); err != nil {
			return nil, err
		}
		l.Quantity = decimalx.MustParse(l.Quantity).String()
		d.Lines = append(d.Lines, l)
	}
	return &d, rows.Err()
}

// CreateEventDoc handles POST /contracts/:id/acceptances, /contracts/:id/customer-returns
// and /purchase-orders/:id/supplier-returns. Each is recorded confirmed, with a date that
// is not in the future, a reason or evidence, and quantities checked against net figures:
//   - acceptance: accepted + new ≤ delivered (per line); then "after acceptance" terms sync.
//   - customer return: ≤ delivered − accepted (accepted goods are reversed first).
//   - supplier return: ≤ received on that PO line, and stock must still cover deliveries.
func (h *Handlers) CreateEventDoc(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := auth.Principal(c)
		parent, err := httpx.PathID(c, "id")
		if err != nil {
			response.Fail(c, err)
			return
		}
		var in eventInput
		if err := httpx.Decode(c, &in); err != nil {
			response.Fail(c, err)
			return
		}
		t := eventTables[kind]
		httpx.Command(c, h.Pool, kind+".create", true, httpx.Created, func(tx pgx.Tx) (any, error) {
			ctx := c.Request.Context()
			contractID, poID := parent, (*uuid.UUID)(nil)
			if kind == "supplier_return" {
				var cid uuid.UUID
				var status string
				if err := tx.QueryRow(ctx, `SELECT po.contract_id, po.status FROM purchase_orders po JOIN contracts c ON c.id = po.contract_id
					WHERE po.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, parent, p.OwnerFilter()).Scan(&cid, &status); err != nil {
					return nil, err
				}
				if status != "confirmed" {
					return nil, apperrors.Rule("purchase_order_not_confirmed", "Goods can be returned only on a confirmed purchase order.", nil)
				}
				contractID, poID = cid, &parent
			}
			ref, err := lockContract(ctx, tx, contractID, p, true)
			if err != nil {
				return nil, err
			}
			// Acceptance can follow closing (closing needs full delivery; acceptance often comes later).
			if ref.Status != "active" && !(kind == "acceptance" && ref.Status == "closed") {
				return nil, apperrors.Rule("contract_not_active", "Reopen the contract first.", nil)
			}
			if err := lockAllLines(ctx, tx, contractID); err != nil {
				return nil, err
			}
			var errs apperrors.FieldErrors
			on := eventDate(ctx, tx, &errs, "on", in.On, contractID, h.today())
			note := in.Reason
			if kind == "acceptance" {
				note = in.Evidence
			}
			note = reasonOf(&errs, t.note, note)
			if len(in.Lines) == 0 {
				errs.Add("lines", "required", nil)
			}
			type want struct {
				line, poLine uuid.UUID
				qty          decimal.Decimal
			}
			var wants []want
			seen := map[uuid.UUID]bool{}
			for i, l := range in.Lines {
				f := func(name string) string { return "lines[" + itoa(i) + "]." + name }
				var w want
				w.qty = decimalx.Positive(&errs, f("quantity"), l.Quantity)
				key := uuid.Nil
				if kind == "supplier_return" {
					w.poLine = httpx.UUID(&errs, f("purchase_order_line_id"), l.PurchaseOrderLineID)
					key = w.poLine
					if w.poLine != uuid.Nil {
						if err := tx.QueryRow(ctx, `SELECT contract_line_id FROM purchase_order_lines WHERE id = $1 AND purchase_order_id = $2`, w.poLine, parent).Scan(&w.line); err != nil {
							if err == pgx.ErrNoRows {
								errs.Add(f("purchase_order_line_id"), "not_in_contract", nil)
								continue
							}
							return nil, err
						}
					}
				} else {
					w.line = httpx.UUID(&errs, f("contract_line_id"), l.ContractLineID)
					key = w.line
					if w.line != uuid.Nil {
						var ok bool
						if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contract_lines WHERE id = $1 AND contract_id = $2)`, w.line, contractID).Scan(&ok); err != nil {
							return nil, err
						}
						if !ok {
							errs.Add(f("contract_line_id"), "not_in_contract", nil)
							continue
						}
					}
				}
				if key != uuid.Nil && seen[key] {
					errs.Add(f("quantity"), "duplicate", nil)
					continue
				}
				seen[key] = true
				if key == uuid.Nil || w.qty.IsZero() {
					continue
				}
				// Each check reads net figures after the line locks above.
				var limit string
				switch kind {
				case "acceptance":
					err = tx.QueryRow(ctx, `SELECT (line_delivered($1) - line_accepted($1))::text`, w.line).Scan(&limit)
				case "customer_return":
					err = tx.QueryRow(ctx, `SELECT (line_delivered($1) - line_accepted($1))::text`, w.line).Scan(&limit)
				case "supplier_return":
					err = tx.QueryRow(ctx, `SELECT least(po_line_received($1), line_received($2) - line_delivered($2))::text`, w.poLine, w.line).Scan(&limit)
				}
				if err != nil {
					return nil, err
				}
				if max := decimalx.MustParse(limit); w.qty.GreaterThan(max) {
					errs.Add(f("quantity"), "exceeds_available", map[string]any{"max": decimal.Max(max, decimal.Zero).String()})
					continue
				}
				wants = append(wants, w)
			}
			if err := errs.Err(); err != nil {
				return nil, err
			}
			if kind == "customer_return" {
				if err := guardDeliveryTerms(ctx, tx, contractID); err != nil {
					return nil, err
				}
			}
			number, err := numbering.NextChild(ctx, tx, contractID, t.prefix)
			if err != nil {
				return nil, err
			}
			var id uuid.UUID
			switch kind {
			case "supplier_return":
				err = tx.QueryRow(ctx, `INSERT INTO supplier_returns (contract_id, purchase_order_id, number, returned_on, reason, created_by)
					VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, contractID, poID, number, on, note, p.UserID).Scan(&id)
			default:
				err = tx.QueryRow(ctx, `INSERT INTO `+t.table+` (contract_id, number, `+t.date+`, `+t.note+`, created_by)
					VALUES ($1, $2, $3, $4, $5) RETURNING id`, contractID, number, on, note, p.UserID).Scan(&id)
			}
			if err != nil {
				return nil, err
			}
			touched := []uuid.UUID{}
			for _, w := range wants {
				if kind == "supplier_return" {
					_, err = tx.Exec(ctx, `INSERT INTO supplier_return_lines (supplier_return_id, purchase_order_line_id, quantity) VALUES ($1, $2, $3)`, id, w.poLine, w.qty)
				} else {
					_, err = tx.Exec(ctx, `INSERT INTO `+t.lines+` (`+t.fk+`, contract_id, contract_line_id, quantity) VALUES ($1, $2, $3, $4)`, id, contractID, w.line, w.qty)
				}
				if err != nil {
					return nil, err
				}
				touched = append(touched, w.line)
			}
			if err := touchLines(ctx, tx, touched); err != nil {
				return nil, err
			}
			if kind == "acceptance" {
				if err := finance.SyncAcceptanceTerms(ctx, tx, contractID, p.UserID); err != nil {
					return nil, err
				}
			}
			if err := audit.Record(ctx, tx, "create", kind, id, map[string]any{"number": number, "reason": note, "lines": len(wants)}); err != nil {
				return nil, err
			}
			return loadEventDoc(ctx, tx, kind, id)
		})
	}
}

// guardDeliveryTerms refuses lowering net deliveries while money collected on "after full
// delivery" depends on them (the same rule as reversing a delivery).
func guardDeliveryTerms(ctx context.Context, tx pgx.Tx, contractID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM payment_milestones WHERE contract_id = $1 ORDER BY id FOR UPDATE`, contractID); err != nil {
		return err
	}
	var paid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_milestones m JOIN receipt_allocations a ON a.milestone_id = m.id
		WHERE m.contract_id = $1 AND m.trigger = 'delivery_complete' AND a.reversed_at IS NULL)`, contractID).Scan(&paid); err != nil {
		return err
	}
	if paid {
		return apperrors.Rule("reversal_has_dependents", "Money was collected on full delivery. Reverse that allocation first.", nil)
	}
	return nil
}

// ReverseEventDoc handles POST /acceptances/:id/reverse, /customer-returns/:id/reverse and
// /supplier-returns/:id/reverse. The document stays, marked reversed, with a linked reason.
func (h *Handlers) ReverseEventDoc(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := auth.Principal(c)
		id, err := httpx.PathID(c, "id")
		if err != nil {
			response.Fail(c, err)
			return
		}
		var in reasonInput
		if err := httpx.Decode(c, &in); err != nil {
			response.Fail(c, err)
			return
		}
		var errs apperrors.FieldErrors
		in.Reason = reasonOf(&errs, "reason", in.Reason)
		if err := errs.Err(); err != nil {
			response.Fail(c, err)
			return
		}
		t := eventTables[kind]
		httpx.Command(c, h.Pool, kind+".reverse", true, 200, func(tx pgx.Tx) (any, error) {
			ctx := c.Request.Context()
			var contractID uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT d.contract_id FROM `+t.table+` d JOIN contracts c ON c.id = d.contract_id
				WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, p.OwnerFilter()).Scan(&contractID); err != nil {
				return nil, err
			}
			if _, err := lockContract(ctx, tx, contractID, p, true); err != nil {
				return nil, err
			}
			if err := lockAllLines(ctx, tx, contractID); err != nil {
				return nil, err
			}
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM `+t.table+` WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
				return nil, err
			}
			if status != "confirmed" {
				return nil, apperrors.Rule("document_not_confirmed", "Only a confirmed document can be reversed.", nil)
			}
			// What undoing this does to the net figures, line by line.
			var blocked string
			switch kind {
			case "customer_return": // goods count as delivered again: they must still be in stock and within the contract
				err = tx.QueryRow(ctx, `SELECT CASE
					WHEN EXISTS (SELECT 1 FROM customer_return_lines l WHERE l.customer_return_id = $1 AND line_received(l.contract_line_id) < line_delivered(l.contract_line_id) + l.quantity) THEN 'insufficient_stock'
					WHEN EXISTS (SELECT 1 FROM customer_return_lines l JOIN contract_lines cl ON cl.id = l.contract_line_id WHERE l.customer_return_id = $1 AND line_delivered(cl.id) + l.quantity > cl.quantity) THEN 'delivery_exceeds_contract'
					ELSE '' END`, id).Scan(&blocked)
			case "supplier_return": // goods count as received again: within what was ordered
				err = tx.QueryRow(ctx, `SELECT CASE
					WHEN EXISTS (SELECT 1 FROM supplier_return_lines l JOIN purchase_order_lines pol ON pol.id = l.purchase_order_line_id
					             WHERE l.supplier_return_id = $1 AND po_line_received(pol.id) + l.quantity > pol.quantity - pol.cancelled_quantity) THEN 'receipt_exceeds_ordered'
					ELSE '' END`, id).Scan(&blocked)
			}
			if err != nil {
				return nil, err
			}
			if blocked != "" {
				return nil, apperrors.Rule(blocked, "Undoing this document would break a quantity rule.", nil)
			}
			if _, err := tx.Exec(ctx, `UPDATE `+t.table+` SET status = 'reversed', version = version + 1 WHERE id = $1`, id); err != nil {
				return nil, err
			}
			if kind == "acceptance" {
				if err := finance.SyncAcceptanceTerms(ctx, tx, contractID, p.UserID); err != nil {
					return nil, err
				}
			}
			var rid uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO document_reversals (document_type, document_id, reason, created_by) VALUES ($1, $2, $3, $4) RETURNING id`,
				kind, id, in.Reason, p.UserID).Scan(&rid); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at = now() WHERE contract_id = $1`, contractID); err != nil {
				return nil, err
			}
			if err := audit.Record(ctx, tx, "reverse", kind, id, map[string]any{"reason": in.Reason, "reversal_id": rid}); err != nil {
				return nil, err
			}
			return loadEventDoc(ctx, tx, kind, id)
		})
	}
}

// lifecycleDocs adds amendments, acceptances and returns to a contract's documents.
func lifecycleDocs(ctx context.Context, q db.Querier, contractID uuid.UUID, docs *Documents) error {
	ids := func(sql string) ([]uuid.UUID, error) {
		rows, err := q.Query(ctx, sql, contractID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	}
	am, err := ids(`SELECT id FROM contract_amendments WHERE contract_id = $1 ORDER BY number`)
	if err != nil {
		return err
	}
	for _, x := range am {
		a, err := loadAmendment(ctx, q, x)
		if err != nil {
			return err
		}
		docs.Amendments = append(docs.Amendments, a)
	}
	for kind, dst := range map[string]*[]*EventDoc{"acceptance": &docs.Acceptances, "customer_return": &docs.CustomerReturns, "supplier_return": &docs.SupplierReturns} {
		list, err := ids(`SELECT id FROM ` + eventTables[kind].table + ` WHERE contract_id = $1 ORDER BY number`)
		if err != nil {
			return err
		}
		for _, x := range list {
			d, err := loadEventDoc(ctx, q, kind, x)
			if err != nil {
				return err
			}
			*dst = append(*dst, d)
		}
	}
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }
