package core

import (
	"context"
	"fmt"
	"strings"
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

// Maintenance & repair contracts (REQUIREMENTS 5.13): the equipment a contract covers and the
// service visits (SV-…) that fulfil its service lines. A finished visit counts as delivered
// (line_delivered), so progress, payment terms, acceptance and closing reuse the existing rules.

// Equipment is one machine the contract maintains.
type Equipment struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Model    *string   `json:"model"`
	Serial   *string   `json:"serial"`
	Location *string   `json:"location"`
}

// ServiceVisit is a planned or finished visit.
type ServiceVisit struct {
	ID            uuid.UUID          `json:"id"`
	Number        string             `json:"number"`
	ContractID    uuid.UUID          `json:"contract_id"`
	Kind          string             `json:"kind"`
	Status        string             `json:"status"`
	PlannedOn     *string            `json:"planned_on"`
	AssigneeID    *uuid.UUID         `json:"assignee_id"`
	AssigneeName  *string            `json:"assignee_name"`
	EquipmentID   *uuid.UUID         `json:"equipment_id"`
	EquipmentName *string            `json:"equipment_name"`
	Description   *string            `json:"description"`
	Urgency       string             `json:"urgency"`
	DoneOn        *string            `json:"done_on"`
	WorkDone      *string            `json:"work_done"`
	Findings      *string            `json:"findings"`
	DoneByName    *string            `json:"done_by_name"`
	CancelReason  *string            `json:"cancel_reason"`
	CreatedAt     time.Time          `json:"created_at"`
	Lines         []ServiceVisitLine `json:"lines"`
}

// ServiceVisitLine is how much of a service line a visit fulfils.
type ServiceVisitLine struct {
	ContractLineID uuid.UUID       `json:"contract_line_id"`
	LineNo         int             `json:"line_no"`
	Name           string          `json:"name_on_contract"`
	Unit           string          `json:"unit"`
	Quantity       decimal.Decimal `json:"quantity"`
}

const visitSelect = `SELECT v.id, v.number, v.contract_id, v.kind, v.status, v.planned_on, v.assignee_id, a.full_name, v.equipment_id, e.name,
	v.description, v.urgency, v.done_on, v.work_done, v.findings, d.full_name, v.cancel_reason, v.created_at
	FROM service_visits v JOIN contracts c ON c.id = v.contract_id LEFT JOIN users a ON a.id = v.assignee_id
	LEFT JOIN contract_equipment e ON e.id = v.equipment_id LEFT JOIN users d ON d.id = v.done_by`

func loadVisits(ctx context.Context, q db.Querier, where string, args ...any) ([]ServiceVisit, error) {
	rows, err := q.Query(ctx, visitSelect+` WHERE `+where+` ORDER BY v.planned_on NULLS FIRST, v.number`, args...)
	if err != nil {
		return nil, err
	}
	out := []ServiceVisit{}
	ids := []uuid.UUID{}
	for rows.Next() {
		var v ServiceVisit
		var planned, done *time.Time
		if err := rows.Scan(&v.ID, &v.Number, &v.ContractID, &v.Kind, &v.Status, &planned, &v.AssigneeID, &v.AssigneeName, &v.EquipmentID, &v.EquipmentName,
			&v.Description, &v.Urgency, &done, &v.WorkDone, &v.Findings, &v.DoneByName, &v.CancelReason, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		v.PlannedOn, v.DoneOn, v.Lines = dateString(planned), dateString(done), []ServiceVisitLine{}
		out = append(out, v)
		ids = append(ids, v.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	lrows, err := q.Query(ctx, `SELECT vl.visit_id, vl.contract_line_id, cl.line_no, cl.name_on_contract, cl.unit, vl.quantity::text
		FROM service_visit_lines vl JOIN contract_lines cl ON cl.id = vl.contract_line_id WHERE vl.visit_id = ANY($1) ORDER BY cl.line_no`, ids)
	if err != nil {
		return nil, err
	}
	defer lrows.Close()
	index := map[uuid.UUID]int{}
	for i, v := range out {
		index[v.ID] = i
	}
	for lrows.Next() {
		var visit uuid.UUID
		var l ServiceVisitLine
		var qty string
		if err := lrows.Scan(&visit, &l.ContractLineID, &l.LineNo, &l.Name, &l.Unit, &qty); err != nil {
			return nil, err
		}
		l.Quantity = decimalx.MustParse(qty)
		out[index[visit]].Lines = append(out[index[visit]].Lines, l)
	}
	return out, lrows.Err()
}

func loadVisit(ctx context.Context, q db.Querier, id uuid.UUID) (*ServiceVisit, error) {
	list, err := loadVisits(ctx, q, `v.id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, apperrors.NotFound()
	}
	return &list[0], nil
}

// ListServiceVisits handles GET /contracts/:id/service-visits.
func (h *Handlers) ListServiceVisits(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	ctx := c.Request.Context()
	if _, err := lockContract(ctx, h.Pool, id, p, false); err != nil {
		response.Fail(c, err)
		return
	}
	list, err := loadVisits(ctx, h.Pool, `v.contract_id = $1`, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, list)
}

// UpcomingServiceVisits handles GET /service-visits?days=14: planned visits in the caller's
// scope (overdue first), for the deliveries page and the overview.
func (h *Handlers) UpcomingServiceVisits(c *gin.Context) {
	p := auth.Principal(c)
	until := h.today().AddDate(0, 0, 14)
	type row struct {
		ServiceVisit
		ContractNumber string `json:"contract_number"`
		CustomerName   string `json:"customer_name"`
	}
	list, err := loadVisits(c.Request.Context(), h.Pool, `v.status = 'planned' AND c.status = 'active' AND (v.planned_on IS NULL OR v.planned_on <= $1) AND ($2::uuid IS NULL OR c.owner_id = $2)`, until, p.OwnerFilter())
	if err != nil {
		response.Fail(c, err)
		return
	}
	out := []row{}
	for _, v := range list {
		var number, customer string
		if err := h.Pool.QueryRow(c.Request.Context(), `SELECT coalesce(c.number, c.draft_number), o.name FROM contracts c JOIN organizations o ON o.id = c.customer_id WHERE c.id = $1`, v.ContractID).Scan(&number, &customer); err != nil {
			response.Fail(c, err)
			return
		}
		out = append(out, row{v, number, customer})
	}
	response.OK(c, out)
}

type visitLineInput struct {
	ContractLineID string `json:"contract_line_id"`
	Quantity       string `json:"quantity"`
}

type visitInput struct {
	Kind        string           `json:"kind"`
	PlannedOn   *string          `json:"planned_on"`
	AssigneeID  *string          `json:"assignee_id"`
	EquipmentID *string          `json:"equipment_id"`
	Description *string          `json:"description"`
	Urgency     *string          `json:"urgency"`
	Lines       []visitLineInput `json:"lines"`
}

type parsedVisit struct {
	kind, urgency      string
	planned            *time.Time
	assignee, equip    *uuid.UUID
	description        *string
	lines              []parsedVisitLine
}

type parsedVisitLine struct {
	line uuid.UUID
	qty  decimal.Decimal
}

func optionalID(errs *apperrors.FieldErrors, field string, raw *string) *uuid.UUID {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	id := httpx.UUID(errs, field, *raw)
	return &id
}

func (in *visitInput) parse(creating bool) (parsedVisit, error) {
	var errs apperrors.FieldErrors
	out := parsedVisit{kind: in.Kind, urgency: "normal"}
	if creating && in.Kind != "scheduled" && in.Kind != "repair" {
		errs.Add("kind", "one_of", map[string]any{"values": []string{"scheduled", "repair"}})
	}
	out.planned = httpx.Date(&errs, "planned_on", in.PlannedOn)
	if creating && in.Kind == "scheduled" && out.planned == nil && (in.PlannedOn == nil || *in.PlannedOn == "") {
		errs.Add("planned_on", "required", nil)
	}
	out.assignee = optionalID(&errs, "assignee_id", in.AssigneeID)
	out.equip = optionalID(&errs, "equipment_id", in.EquipmentID)
	out.description = trimmed(in.Description)
	if out.description != nil && len([]rune(*out.description)) > 2000 {
		errs.Add("description", "too_long", map[string]any{"max": 2000})
	}
	if creating && in.Kind == "repair" && out.description == nil {
		errs.Add("description", "required", nil)
	}
	if in.Urgency != nil && *in.Urgency != "" {
		out.urgency = *in.Urgency
	}
	if out.urgency != "normal" && out.urgency != "urgent" {
		errs.Add("urgency", "one_of", map[string]any{"values": []string{"normal", "urgent"}})
	}
	seen := map[uuid.UUID]bool{}
	for i, l := range in.Lines {
		f := func(n string) string { return fmt.Sprintf("lines[%d].%s", i, n) }
		pl := parsedVisitLine{line: httpx.UUID(&errs, f("contract_line_id"), l.ContractLineID), qty: decimalx.Positive(&errs, f("quantity"), l.Quantity)}
		if seen[pl.line] {
			errs.Add(f("contract_line_id"), "duplicate", nil)
		}
		seen[pl.line] = true
		out.lines = append(out.lines, pl)
	}
	if creating && in.Kind == "scheduled" && len(in.Lines) == 0 {
		errs.Add("lines", "required", map[string]any{"min": 1})
	}
	return out, errs.Err()
}

// checkVisitRefs: lines are this contract's service lines; the assignee is an active
// person who can do field work; the equipment is this contract's.
func checkVisitRefs(ctx context.Context, tx pgx.Tx, contractID uuid.UUID, v parsedVisit) error {
	var errs apperrors.FieldErrors
	for i, l := range v.lines {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contract_lines WHERE id = $1 AND contract_id = $2 AND line_type = 'service')`, l.line, contractID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			errs.Add(fmt.Sprintf("lines[%d].contract_line_id", i), "not_service_line", nil)
		}
	}
	if v.assignee != nil {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u WHERE u.id = $1 AND u.is_active AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('delivery', 'sales', 'admin')))`, *v.assignee).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			errs.Add("assignee_id", "not_found", nil)
		}
	}
	if v.equip != nil {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contract_equipment WHERE id = $1 AND contract_id = $2)`, *v.equip, contractID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			errs.Add("equipment_id", "not_found", nil)
		}
	}
	return errs.Err()
}

func activeServiceContract(ctx context.Context, tx pgx.Tx, id uuid.UUID, p *access.Principal) (*contractRef, error) {
	k, err := lockContract(ctx, tx, id, p, true)
	if err != nil {
		return nil, err
	}
	if k.Status != "active" {
		return nil, apperrors.Rule("contract_not_active", "Activate the contract before planning service visits.", map[string]any{"status": k.Status})
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM contracts WHERE id = $1`, id).Scan(&kind); err != nil {
		return nil, err
	}
	if kind != "service" {
		return nil, apperrors.Rule("not_service_contract", "Service visits belong to maintenance & repair contracts.", nil)
	}
	return k, nil
}

func insertVisit(ctx context.Context, tx pgx.Tx, contractID, actor uuid.UUID, v parsedVisit) (uuid.UUID, string, error) {
	number, err := numbering.NextChild(ctx, tx, contractID, "SV")
	if err != nil {
		return uuid.Nil, "", err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO service_visits (number, contract_id, kind, planned_on, assignee_id, equipment_id, description, urgency, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, number, contractID, v.kind, v.planned, v.assignee, v.equip, v.description, v.urgency, actor).Scan(&id); err != nil {
		return uuid.Nil, "", err
	}
	for _, l := range v.lines {
		if _, err := tx.Exec(ctx, `INSERT INTO service_visit_lines (visit_id, contract_line_id, quantity) VALUES ($1, $2, $3)`, id, l.line, l.qty); err != nil {
			return uuid.Nil, "", err
		}
	}
	return id, number, nil
}

// CreateServiceVisit handles POST /contracts/:id/service-visits: a planned visit, or a repair
// request (no date yet) when the customer reports a problem.
func (h *Handlers) CreateServiceVisit(c *gin.Context) {
	contractID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	var in visitInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	v, err := in.parse(true)
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "service_visits.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := activeServiceContract(ctx, tx, contractID, p); err != nil {
			return nil, err
		}
		if err := checkVisitRefs(ctx, tx, contractID, v); err != nil {
			return nil, err
		}
		id, number, err := insertVisit(ctx, tx, contractID, p.UserID, v)
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "create", "service_visit", id, map[string]any{"number": number, "kind": v.kind, "planned_on": dateString(v.planned), "lines": len(v.lines)}); err != nil {
			return nil, err
		}
		return loadVisit(ctx, tx, id)
	})
}

// ScheduleServiceVisits handles POST /contracts/:id/service-visits/schedule
// {contract_line_id, frequency monthly|quarterly|half_yearly|yearly, start_on, assignee_id?}:
// one visit (quantity 1) per period from start_on, inside the service period, for the
// visits of the line not yet done or planned.
func (h *Handlers) ScheduleServiceVisits(c *gin.Context) {
	contractID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	var in struct {
		ContractLineID string  `json:"contract_line_id"`
		Frequency      string  `json:"frequency"`
		StartOn        *string `json:"start_on"`
		AssigneeID     *string `json:"assignee_id"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	line := httpx.UUID(&errs, "contract_line_id", in.ContractLineID)
	months := map[string]int{"monthly": 1, "quarterly": 3, "half_yearly": 6, "yearly": 12}[in.Frequency]
	if months == 0 {
		errs.Add("frequency", "one_of", map[string]any{"values": []string{"monthly", "quarterly", "half_yearly", "yearly"}})
	}
	start := httpx.Date(&errs, "start_on", in.StartOn)
	if start == nil && (in.StartOn == nil || *in.StartOn == "") {
		errs.Add("start_on", "required", nil)
	}
	assignee := optionalID(&errs, "assignee_id", in.AssigneeID)
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "service_visits.schedule", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := activeServiceContract(ctx, tx, contractID, p); err != nil {
			return nil, err
		}
		v := parsedVisit{kind: "scheduled", urgency: "normal", assignee: assignee, lines: []parsedVisitLine{{line: line, qty: decimal.NewFromInt(1)}}}
		if err := checkVisitRefs(ctx, tx, contractID, v); err != nil {
			return nil, err
		}
		var open string
		var to *time.Time
		if err := tx.QueryRow(ctx, `SELECT (cl.quantity - line_delivered(cl.id) - coalesce((SELECT sum(vl.quantity) FROM service_visit_lines vl JOIN service_visits sv ON sv.id = vl.visit_id
				WHERE vl.contract_line_id = cl.id AND sv.status = 'planned'), 0))::text, c.service_to
			FROM contract_lines cl JOIN contracts c ON c.id = cl.contract_id WHERE cl.id = $1`, line).Scan(&open, &to); err != nil {
			return nil, err
		}
		count := int(decimalx.MustParse(open).Floor().IntPart())
		if count <= 0 {
			return nil, apperrors.Rule("nothing_to_schedule", "Every visit of this line is already done or planned.", nil)
		}
		ids := []uuid.UUID{}
		for i := 0; i < count && i < 120; i++ {
			on := start.AddDate(0, months*i, 0)
			if to != nil && on.After(*to) {
				break
			}
			v.planned = &on
			id, _, err := insertVisit(ctx, tx, contractID, p.UserID, v)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, apperrors.Invalid("start_on", "after_service_period", nil)
		}
		if err := audit.Record(ctx, tx, "schedule", "contract", contractID, map[string]any{"line": line, "frequency": in.Frequency, "visits": len(ids)}); err != nil {
			return nil, err
		}
		return loadVisits(ctx, tx, `v.id = ANY($1)`, ids)
	})
}

// lockVisit loads a visit within the caller's scope, with its contract's lines locked.
func lockVisit(ctx context.Context, tx pgx.Tx, id uuid.UUID, p *access.Principal) (contractID uuid.UUID, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT v.contract_id, v.status FROM service_visits v JOIN contracts c ON c.id = v.contract_id
		WHERE v.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2) FOR UPDATE OF v`, id, p.OwnerFilter()).Scan(&contractID, &status)
	if err != nil {
		return
	}
	_, err = tx.Exec(ctx, `SELECT id FROM contract_lines WHERE contract_id = $1 ORDER BY id FOR UPDATE`, contractID)
	return
}

// UpdateServiceVisit handles PATCH /service-visits/:id: date, person, equipment, description
// and lines of a visit not done yet.
func (h *Handlers) UpdateServiceVisit(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	var in visitInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	v, err := in.parse(false)
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "service_visits.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		contractID, status, err := lockVisit(ctx, tx, id, p)
		if err != nil {
			return nil, err
		}
		if status != "planned" {
			return nil, apperrors.Rule("visit_not_planned", "Only a visit not done yet can be changed.", map[string]any{"status": status})
		}
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM service_visits WHERE id = $1`, id).Scan(&kind); err != nil {
			return nil, err
		}
		if kind == "scheduled" && v.planned == nil {
			return nil, apperrors.Invalid("planned_on", "required", nil)
		}
		if err := checkVisitRefs(ctx, tx, contractID, v); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE service_visits SET planned_on = $2, assignee_id = $3, equipment_id = $4, description = $5, urgency = $6,
			version = version + 1, updated_at = now() WHERE id = $1`, id, v.planned, v.assignee, v.equip, v.description, v.urgency); err != nil {
			return nil, err
		}
		if in.Lines != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM service_visit_lines WHERE visit_id = $1`, id); err != nil {
				return nil, err
			}
			for _, l := range v.lines {
				if _, err := tx.Exec(ctx, `INSERT INTO service_visit_lines (visit_id, contract_line_id, quantity) VALUES ($1, $2, $3)`, id, l.line, l.qty); err != nil {
					return nil, err
				}
			}
		}
		if err := audit.Record(ctx, tx, "update", "service_visit", id, map[string]any{"planned_on": dateString(v.planned), "assignee_id": v.assignee}); err != nil {
			return nil, err
		}
		return loadVisit(ctx, tx, id)
	})
}

// CompleteServiceVisit handles POST /service-visits/:id/done {done_on, work_done, findings}:
// the visit happened; its lines count as delivered (never beyond the contract).
func (h *Handlers) CompleteServiceVisit(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	var in struct {
		DoneOn   *string `json:"done_on"`
		WorkDone *string `json:"work_done"`
		Findings *string `json:"findings"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	done := httpx.Date(&errs, "done_on", in.DoneOn)
	if done == nil && (in.DoneOn == nil || *in.DoneOn == "") {
		errs.Add("done_on", "required", nil)
	}
	if done != nil && done.After(h.today()) {
		errs.Add("done_on", "not_future", nil)
	}
	work, findings := trimmed(in.WorkDone), trimmed(in.Findings)
	if work == nil {
		errs.Add("work_done", "required", nil)
	}
	for _, f := range []struct {
		name string
		v    *string
	}{{"work_done", work}, {"findings", findings}} {
		if f.v != nil && len([]rune(*f.v)) > 4000 {
			errs.Add(f.name, "too_long", map[string]any{"max": 4000})
		}
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "service_visits.done", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		contractID, status, err := lockVisit(ctx, tx, id, p)
		if err != nil {
			return nil, err
		}
		if status != "planned" {
			return nil, apperrors.Rule("visit_not_planned", "This visit is already recorded or cancelled.", map[string]any{"status": status})
		}
		var cstatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id = $1`, contractID).Scan(&cstatus); err != nil {
			return nil, err
		}
		if cstatus != "active" {
			return nil, apperrors.Rule("contract_not_active", "The contract is not active.", map[string]any{"status": cstatus})
		}
		var over bool
		var lineNo *int
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM service_visit_lines vl JOIN contract_lines cl ON cl.id = vl.contract_line_id
				WHERE vl.visit_id = $1 AND line_delivered(cl.id) + vl.quantity > cl.quantity),
			(SELECT min(cl.line_no) FROM service_visit_lines vl JOIN contract_lines cl ON cl.id = vl.contract_line_id WHERE vl.visit_id = $1 AND line_delivered(cl.id) + vl.quantity > cl.quantity)`, id).Scan(&over, &lineNo); err != nil {
			return nil, err
		}
		if over {
			return nil, apperrors.Rule("delivery_exceeds_contract", "This visit would go beyond the visits on the contract. Amend the contract first.", map[string]any{"line_no": lineNo})
		}
		if _, err := tx.Exec(ctx, `UPDATE service_visits SET status = 'done', done_on = $2, work_done = $3, findings = $4, done_by = $5, done_at = now(),
			version = version + 1, updated_at = now() WHERE id = $1`, id, done, work, findings, p.UserID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at = now() WHERE id IN (SELECT contract_line_id FROM service_visit_lines WHERE visit_id = $1)`, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "complete", "service_visit", id, map[string]any{"done_on": done.Format(time.DateOnly)}); err != nil {
			return nil, err
		}
		return loadVisit(ctx, tx, id)
	})
}

// CancelServiceVisit handles POST /service-visits/:id/cancel {reason}: a visit not done yet.
func (h *Handlers) CancelServiceVisit(c *gin.Context) { h.closeVisit(c, "cancel") }

// ReverseServiceVisit handles POST /service-visits/:id/reverse {reason}: a recorded visit was
// a mistake; its lines are no longer delivered. Not when accepted work or collected
// "after full delivery" money depends on it (same rule as deliveries).
func (h *Handlers) ReverseServiceVisit(c *gin.Context) { h.closeVisit(c, "reverse") }

func (h *Handlers) closeVisit(c *gin.Context, action string) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
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
	httpx.Command(c, h.Pool, "service_visits."+action, true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		contractID, status, err := lockVisit(ctx, tx, id, p)
		if err != nil {
			return nil, err
		}
		if action == "cancel" {
			if status != "planned" {
				return nil, apperrors.Rule("visit_not_planned", "Only a visit not done yet can be cancelled.", map[string]any{"status": status})
			}
			if _, err := tx.Exec(ctx, `UPDATE service_visits SET status = 'cancelled', cancel_reason = $2, version = version + 1, updated_at = now() WHERE id = $1`, id, in.Reason); err != nil {
				return nil, err
			}
			return loadVisit(ctx, tx, id)
		}
		if status != "done" {
			return nil, apperrors.Rule("document_not_confirmed", "Only a recorded visit can be reversed.", nil)
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM payment_milestones WHERE contract_id = $1 ORDER BY id FOR UPDATE`, contractID); err != nil {
			return nil, err
		}
		var dependent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_milestones m WHERE m.contract_id = $1 AND m.trigger = 'delivery_complete'
				AND EXISTS(SELECT 1 FROM receipt_allocations a WHERE a.milestone_id = m.id AND a.reversed_at IS NULL))
			OR EXISTS(SELECT 1 FROM service_visit_lines l WHERE l.visit_id = $2 AND line_delivered(l.contract_line_id) - l.quantity < line_accepted(l.contract_line_id))`, contractID, id).Scan(&dependent); err != nil {
			return nil, err
		}
		if dependent {
			return nil, apperrors.Rule("reversal_has_dependents", "Correct acceptance or dependent payment allocations before reversing this visit.", nil)
		}
		if _, err := tx.Exec(ctx, `UPDATE service_visits SET status = 'reversed', version = version + 1, updated_at = now() WHERE id = $1`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO document_reversals (document_type, document_id, reason, created_by) VALUES ('service_visit', $1, $2, $3)`, id, in.Reason, p.UserID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at = now() WHERE contract_id = $1`, contractID); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "reverse", "service_visit", id, map[string]any{"reason": in.Reason}); err != nil {
			return nil, err
		}
		return loadVisit(ctx, tx, id)
	})
}

// ListEquipment handles GET /contracts/:id/equipment.
func (h *Handlers) ListEquipment(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	ctx := c.Request.Context()
	if _, err := lockContract(ctx, h.Pool, id, auth.Principal(c), false); err != nil {
		response.Fail(c, err)
		return
	}
	list, err := loadEquipment(ctx, h.Pool, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, list)
}

func loadEquipment(ctx context.Context, q db.Querier, contractID uuid.UUID) ([]Equipment, error) {
	rows, err := q.Query(ctx, `SELECT id, name, model, serial, location FROM contract_equipment WHERE contract_id = $1 ORDER BY position, created_at`, contractID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Equipment])
}

// SaveEquipment handles PUT /contracts/:id/equipment [{id?, name, model, serial, location}]:
// the whole list; rows with an id are updated, new ones added, missing ones removed (their
// visits keep their history without the link).
func (h *Handlers) SaveEquipment(c *gin.Context) {
	contractID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	var in []struct {
		ID       *string `json:"id"`
		Name     string  `json:"name"`
		Model    *string `json:"model"`
		Serial   *string `json:"serial"`
		Location *string `json:"location"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	ids := make([]*uuid.UUID, len(in))
	for i := range in {
		f := func(n string) string { return fmt.Sprintf("[%d].%s", i, n) }
		in[i].Name = strings.TrimSpace(in[i].Name)
		in[i].Model, in[i].Serial, in[i].Location = trimmed(in[i].Model), trimmed(in[i].Serial), trimmed(in[i].Location)
		if in[i].Name == "" || len([]rune(in[i].Name)) > 200 {
			errs.Add(f("name"), "required", nil)
		}
		for _, x := range []struct {
			name string
			v    *string
			max  int
		}{{"model", in[i].Model, 120}, {"serial", in[i].Serial, 120}, {"location", in[i].Location, 200}} {
			if x.v != nil && len([]rune(*x.v)) > x.max {
				errs.Add(f(x.name), "too_long", map[string]any{"max": x.max})
			}
		}
		ids[i] = optionalID(&errs, f("id"), in[i].ID)
	}
	if len(in) > 200 {
		errs.Add("equipment", "too_many", map[string]any{"max": 200})
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contract_equipment.save", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := lockContract(ctx, tx, contractID, p, true); err != nil {
			return nil, err
		}
		keep := []uuid.UUID{}
		for i, e := range in {
			if ids[i] != nil {
				tag, err := tx.Exec(ctx, `UPDATE contract_equipment SET name = $3, model = $4, serial = $5, location = $6, position = $7 WHERE id = $1 AND contract_id = $2`,
					*ids[i], contractID, e.Name, e.Model, e.Serial, e.Location, i)
				if err != nil {
					return nil, err
				}
				if tag.RowsAffected() == 1 {
					keep = append(keep, *ids[i])
					continue
				}
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO contract_equipment (contract_id, position, name, model, serial, location) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
				contractID, i, e.Name, e.Model, e.Serial, e.Location).Scan(&id); err != nil {
				return nil, err
			}
			keep = append(keep, id)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM contract_equipment WHERE contract_id = $1 AND NOT (id = ANY($2))`, contractID, keep); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "update_equipment", "contract", contractID, map[string]any{"count": len(in)}); err != nil {
			return nil, err
		}
		return loadEquipment(ctx, tx, contractID)
	})
}
