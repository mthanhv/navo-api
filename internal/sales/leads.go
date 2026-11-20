// Package sales holds leads (potential customers), the sales activity log and the sales
// report (REQUIREMENTS 5.14, owner answers 1.9). A lead is an organization that has not bought
// yet, so its contacts, activities and quotations stay with it when it becomes a customer.
package sales

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"navo-api/internal/access"
	"navo-api/internal/activitylog"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/crm"
	"navo-api/internal/httpx"
	"navo-api/internal/masterdata"
	"navo-api/internal/response"
)

// Handlers serve leads, activities and the sales report.
type Handlers struct {
	Pool *pgxpool.Pool
	TZ   *time.Location
}

func (h *Handlers) today() time.Time {
	tz := h.TZ
	if tz == nil {
		tz = time.UTC
	}
	n := time.Now().In(tz)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// Lead is a potential customer with what sales knows and what happened last and next.
type Lead struct {
	ID             uuid.UUID  `json:"id"`
	Code           string     `json:"code"`
	Name           string     `json:"name"`
	TaxCode        *string    `json:"tax_code"`
	IsCustomer     bool       `json:"is_customer"`
	Stage          string     `json:"stage"`
	Source         *string    `json:"source"`
	Industry       *string    `json:"industry"`
	Interest       *string    `json:"interest"`
	EstimatedValue *string    `json:"estimated_value"`
	LostReason     *string    `json:"lost_reason"`
	RemindOn       *string    `json:"remind_on"`
	OwnerID        *uuid.UUID `json:"owner_id"`
	OwnerName      *string    `json:"owner_name"`
	LeadSince      time.Time  `json:"lead_since"`
	WonAt          *time.Time `json:"won_at"`
	Contacts       int        `json:"contacts"`
	PrimaryContact *string    `json:"primary_contact"`
	PrimaryPhone   *string    `json:"primary_phone"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	LastActivity   *string    `json:"last_activity_kind"`
	NextPlannedOn  *string    `json:"next_planned_on"`
	NextPlanned    *string    `json:"next_planned"`
	Version        int64      `json:"version"`
	CanEdit        bool       `json:"can_edit"`
}

const leadSelect = `SELECT o.id, o.code, o.name, o.tax_code, o.is_customer, o.lead_stage, o.lead_source, o.industry, o.interest, o.estimated_value::text,
	o.lost_reason, to_char(o.remind_on,'YYYY-MM-DD'), o.owner_id, ou.full_name, coalesce(o.lead_since, o.created_at), o.won_at,
	(SELECT count(*) FROM contacts ct WHERE ct.organization_id = o.id), pc.full_name, pc.phone,
	la.done_at, la.kind, to_char(na.planned_on,'YYYY-MM-DD'), na.summary, o.version,
	($1::uuid IS NULL OR o.owner_id IS NULL OR o.owner_id = $1)
	FROM organizations o LEFT JOIN users ou ON ou.id = o.owner_id
	LEFT JOIN LATERAL (SELECT full_name, phone FROM contacts WHERE organization_id = o.id ORDER BY is_primary DESC, full_name LIMIT 1) pc ON true
	LEFT JOIN LATERAL (SELECT done_at, kind FROM activities WHERE organization_id = o.id AND status = 'done' ORDER BY done_at DESC LIMIT 1) la ON true
	LEFT JOIN LATERAL (SELECT planned_on, summary FROM activities WHERE organization_id = o.id AND status = 'planned' ORDER BY planned_on NULLS LAST, created_at LIMIT 1) na ON true`

func scanLead(r pgx.Row) (Lead, error) {
	var l Lead
	err := r.Scan(&l.ID, &l.Code, &l.Name, &l.TaxCode, &l.IsCustomer, &l.Stage, &l.Source, &l.Industry, &l.Interest, &l.EstimatedValue,
		&l.LostReason, &l.RemindOn, &l.OwnerID, &l.OwnerName, &l.LeadSince, &l.WonAt, &l.Contacts, &l.PrimaryContact, &l.PrimaryPhone,
		&l.LastActivityAt, &l.LastActivity, &l.NextPlannedOn, &l.NextPlanned, &l.Version, &l.CanEdit)
	if err == nil && l.EstimatedValue != nil {
		v := decimal.RequireFromString(*l.EstimatedValue).String()
		l.EstimatedValue = &v
	}
	return l, err
}

// scope: sales see their own leads and unassigned ones; whole-company roles see all.
const leadScope = `o.lead_stage IS NOT NULL AND ($1::uuid IS NULL OR o.owner_id IS NULL OR o.owner_id = $1)`

var stageGroups = map[string][]string{
	"open": {"new", "contacted", "qualified", "quoted"}, "won": {"won"}, "lost": {"lost"}, "later": {"later"},
}

// ListLeads handles GET /leads?tab=open|won|lost|later|all&q=&owner_id=&stale=1.
func (h *Handlers) ListLeads(c *gin.Context) {
	p := auth.Principal(c)
	page, size := httpx.Page(c)
	tab := c.DefaultQuery("tab", "open")
	if _, ok := stageGroups[tab]; !ok && tab != "all" {
		response.Fail(c, apperrors.Invalid("tab", "one_of", map[string]any{"values": []string{"open", "won", "lost", "later", "all"}}))
		return
	}
	var owner *uuid.UUID
	if raw := c.Query("owner_id"); raw != "" && p.OwnerFilter() == nil {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Fail(c, apperrors.Invalid("owner_id", "uuid", nil))
			return
		}
		owner = &id
	}
	q := strings.TrimSpace(c.Query("q"))
	stale := c.Query("stale") == "1"
	rows, err := h.Pool.Query(c.Request.Context(), leadSelect+` WHERE `+leadScope+` AND ($2::uuid IS NULL OR o.owner_id = $2)
		AND ($3 = '' OR strpos(vn_fold(o.name), vn_fold($3)) > 0 OR strpos(vn_fold(o.code), vn_fold($3)) > 0 OR strpos(vn_fold(o.industry), vn_fold($3)) > 0
		     OR EXISTS (SELECT 1 FROM contacts ct WHERE ct.organization_id = o.id AND (strpos(vn_fold(ct.full_name), vn_fold($3)) > 0 OR strpos(vn_fold(ct.phone), vn_fold($3)) > 0)))
		ORDER BY coalesce(na.planned_on, '9999-12-31'), coalesce(la.done_at, o.lead_since) DESC, o.name, o.id`, p.OwnerFilter(), owner, q)
	if err != nil {
		response.Fail(c, err)
		return
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Lead, error) { return scanLead(r) })
	if err != nil {
		response.Fail(c, err)
		return
	}
	cutoff := time.Now().AddDate(0, 0, -14)
	counts := map[string]int{"open": 0, "won": 0, "lost": 0, "later": 0, "all": 0, "stale": 0}
	items := []Lead{}
	for _, l := range all {
		group := "open"
		for g, stages := range stageGroups {
			for _, s := range stages {
				if s == l.Stage {
					group = g
				}
			}
		}
		isStale := group == "open" && l.LeadSince.Before(cutoff) && (l.LastActivityAt == nil || l.LastActivityAt.Before(cutoff))
		counts[group]++
		counts["all"]++
		if isStale {
			counts["stale"]++
		}
		if (tab == "all" || tab == group) && (!stale || isStale) {
			items = append(items, l)
		}
	}
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	response.ListCounts(c, items[start:end], page, size, len(items), counts)
}

func (h *Handlers) loadLead(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, p *access.Principal, id uuid.UUID) (*Lead, error) {
	l, err := scanLead(q.QueryRow(ctx, leadSelect+` WHERE `+leadScope+` AND o.id = $2`, p.OwnerFilter(), id))
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// GetLead handles GET /leads/:id.
func (h *Handlers) GetLead(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	l, err := h.loadLead(c.Request.Context(), h.Pool, auth.Principal(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, l)
}

var sources = map[string]bool{"referral": true, "trade_show": true, "website": true, "cold_call": true, "zalo": true, "existing_customer": true, "other": true}

type leadInput struct {
	Version        *int64           `json:"version"`
	Name           string           `json:"name"`
	TaxCode        *string          `json:"tax_code"`
	Source         *string          `json:"source"`
	Industry       *string          `json:"industry"`
	Interest       *string          `json:"interest"`
	EstimatedValue *string          `json:"estimated_value"`
	OwnerID        *string          `json:"owner_id"`
	Contacts       []crm.NewContact `json:"contacts"`
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

func (in *leadInput) validate(creating bool) (*uuid.UUID, *decimal.Decimal, error) {
	var errs apperrors.FieldErrors
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		errs.Add("name", "required", nil)
	} else if len([]rune(in.Name)) > 300 {
		errs.Add("name", "too_long", map[string]any{"max": 300})
	}
	in.TaxCode, in.Source, in.Industry, in.Interest = trimmed(in.TaxCode), trimmed(in.Source), trimmed(in.Industry), trimmed(in.Interest)
	if in.TaxCode != nil && !taxCode(*in.TaxCode) {
		errs.Add("tax_code", "tax_code", nil)
	}
	if in.Source != nil && !sources[*in.Source] {
		errs.Add("source", "one_of", nil)
	}
	if in.Industry != nil && len([]rune(*in.Industry)) > 80 {
		errs.Add("industry", "too_long", map[string]any{"max": 80})
	}
	if in.Interest != nil && len([]rune(*in.Interest)) > 1000 {
		errs.Add("interest", "too_long", map[string]any{"max": 1000})
	}
	var value *decimal.Decimal
	if v := trimmed(in.EstimatedValue); v != nil {
		d, err := decimal.NewFromString(*v)
		if err != nil || d.IsNegative() {
			errs.Add("estimated_value", "decimal", nil)
		} else {
			d = d.Round(2)
			value = &d
		}
	}
	var owner *uuid.UUID
	if v := trimmed(in.OwnerID); v != nil {
		id := httpx.UUID(&errs, "owner_id", *v)
		owner = &id
	}
	if !creating && len(in.Contacts) > 0 {
		errs.Add("contacts", "edit_contacts_separately", nil)
	}
	crm.CheckContacts(&errs, in.Contacts)
	return owner, value, errs.Err()
}

func taxCode(s string) bool {
	if len(s) != 10 && len(s) != 14 {
		return false
	}
	for i, r := range s {
		if i == 10 && r == '-' {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// assignable checks a chosen owner: only whole-company roles give a lead to someone else.
func assignable(ctx context.Context, tx pgx.Tx, p *access.Principal, owner *uuid.UUID) (uuid.UUID, error) {
	if owner == nil || *owner == p.UserID {
		return p.UserID, nil
	}
	if p.OwnerFilter() != nil {
		return uuid.Nil, apperrors.Forbidden("assign_owner")
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id = $1 AND u.is_active AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('sales','sales_manager','admin')))`, *owner).Scan(&ok); err != nil || !ok {
		return uuid.Nil, apperrors.Invalid("owner_id", "not_found", nil)
	}
	return *owner, nil
}

// CreateLead handles POST /leads: a new potential customer, with its people.
func (h *Handlers) CreateLead(c *gin.Context) {
	p := auth.Principal(c)
	var in leadInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	ownerIn, value, err := in.validate(true)
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "leads.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		owner, err := assignable(ctx, tx, p, ownerIn)
		if err != nil {
			return nil, err
		}
		code, err := masterdata.GenerateCode(ctx, tx, in.Name)
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO organizations (code, name, tax_code, lead_stage, lead_source, industry, interest, estimated_value, owner_id, lead_since)
			VALUES ($1, $2, $3, 'new', $4, $5, $6, $7, $8, now()) RETURNING id`, code, in.Name, in.TaxCode, in.Source, in.Industry, in.Interest, value, owner).Scan(&id); err != nil {
			return nil, err
		}
		if err := crm.InsertContacts(ctx, tx, id, in.Contacts); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "create", "lead", id, map[string]any{"name": in.Name, "owner_id": owner, "contacts": len(in.Contacts)}); err != nil {
			return nil, err
		}
		return h.loadLead(ctx, tx, p, id)
	})
}

// lockLead loads a lead the caller may change (own or unassigned; any for whole-company roles).
func lockLead(ctx context.Context, tx pgx.Tx, p *access.Principal, id uuid.UUID) (string, int64, error) {
	var stage string
	var version int64
	err := tx.QueryRow(ctx, `SELECT lead_stage, version FROM organizations o WHERE o.id = $2 AND `+leadScope+` FOR UPDATE`, p.OwnerFilter(), id).Scan(&stage, &version)
	return stage, version, err
}

// UpdateLead handles PATCH /leads/:id (contacts change through the contact endpoints).
func (h *Handlers) UpdateLead(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in leadInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	ownerIn, value, err := in.validate(false)
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "leads.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		_, version, err := lockLead(ctx, tx, p, id)
		if err != nil {
			return nil, err
		}
		if in.Version == nil || *in.Version != version {
			return nil, apperrors.Conflict("version_conflict", "This lead changed. Reload before editing.", nil)
		}
		var owner *uuid.UUID
		if ownerIn != nil {
			o, err := assignable(ctx, tx, p, ownerIn)
			if err != nil {
				return nil, err
			}
			owner = &o
		}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET name = $2, tax_code = $3, lead_source = $4, industry = $5, interest = $6, estimated_value = $7,
			owner_id = COALESCE($8, owner_id), version = version + 1, updated_at = now() WHERE id = $1`, id, in.Name, in.TaxCode, in.Source, in.Industry, in.Interest, value, owner); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "update", "lead", id, map[string]any{"name": in.Name, "owner_id": owner}); err != nil {
			return nil, err
		}
		return h.loadLead(ctx, tx, p, id)
	})
}

// SetStage handles POST /leads/:id/stage {stage, lost_reason, remind_on}. "won" is reached by a
// signed contract or Convert; the other stages can be set by hand.
func (h *Handlers) SetStage(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in struct {
		Stage      string  `json:"stage"`
		LostReason *string `json:"lost_reason"`
		RemindOn   *string `json:"remind_on"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	if !map[string]bool{"new": true, "contacted": true, "qualified": true, "quoted": true, "lost": true, "later": true}[in.Stage] {
		errs.Add("stage", "one_of", nil)
	}
	in.LostReason = trimmed(in.LostReason)
	if in.Stage == "lost" && in.LostReason == nil {
		errs.Add("lost_reason", "required", nil)
	} else if in.LostReason != nil && len([]rune(*in.LostReason)) > 500 {
		errs.Add("lost_reason", "too_long", map[string]any{"max": 500})
	}
	remind := httpx.Date(&errs, "remind_on", in.RemindOn)
	if in.Stage == "later" && remind == nil {
		errs.Add("remind_on", "required", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	if in.Stage != "lost" {
		in.LostReason = nil
	}
	if in.Stage != "later" {
		remind = nil
	}
	httpx.Command(c, h.Pool, "leads.stage", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		from, _, err := lockLead(ctx, tx, p, id)
		if err != nil {
			return nil, err
		}
		if from == "won" {
			return nil, apperrors.Rule("lead_won", "This lead is already a customer.", nil)
		}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET lead_stage = $2, lost_reason = $3, remind_on = $4, version = version + 1, updated_at = now() WHERE id = $1`,
			id, in.Stage, in.LostReason, remind); err != nil {
			return nil, err
		}
		if remind != nil {
			// "Later" leaves a reminder in the owner's to-do list.
			if _, err := tx.Exec(ctx, `INSERT INTO activities (kind, status, organization_id, summary, planned_on, owner_id, created_by)
				SELECT 'call', 'planned', o.id, 'Liên hệ lại (đã hẹn để sau)', $2, coalesce(o.owner_id, $3), $3 FROM organizations o WHERE o.id = $1`, id, remind, p.UserID); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "stage", "lead", id, map[string]any{"from": from, "to": in.Stage, "reason": in.LostReason}); err != nil {
			return nil, err
		}
		return h.loadLead(ctx, tx, p, id)
	})
}

// ConvertLead handles POST /leads/:id/convert: the lead becomes a customer now.
func (h *Handlers) ConvertLead(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "leads.convert", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		from, _, err := lockLead(ctx, tx, p, id)
		if err != nil {
			return nil, err
		}
		if err := activitylog.Won(ctx, tx, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "convert", "lead", id, map[string]any{"from": from}); err != nil {
			return nil, err
		}
		return h.loadLead(ctx, tx, p, id)
	})
}

var fold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

var companyWords = map[string]bool{"cong": true, "ty": true, "cty": true, "tnhh": true, "cp": true, "co": true, "phan": true, "mtv": true, "jsc": true, "ltd": true, "corp": true, "company": true, "va": true, "and": true}

func nameWords(s string) []string {
	s, _, _ = transform.String(fold, strings.ToLower(s))
	s = strings.ReplaceAll(s, "đ", "d")
	out := []string{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if !companyWords[w] {
			out = append(out, w)
		}
	}
	return out
}

func similar(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, w := range a {
		set[w] = true
	}
	common := 0
	for _, w := range b {
		if set[w] {
			common++
		}
	}
	return float64(common)/float64(max(len(a), len(b))) >= 0.6
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) > 9 {
		d = d[len(d)-9:] // 0903… and +84903… are the same number
	}
	return d
}

// Duplicate is an existing partner that looks like the one being added.
type Duplicate struct {
	ID     uuid.UUID `json:"id"`
	Code   string    `json:"code"`
	Name   string    `json:"name"`
	Kind   string    `json:"kind"` // customer, supplier, lead
	Owner  *string   `json:"owner_name"`
	Reason string    `json:"reason"` // tax_code, name, phone
}

// Duplicates handles POST /leads/duplicates {name, tax_code, phones}: partners that may be the
// same company (same tax code, similar name, or a contact with the same phone). Read only.
func (h *Handlers) Duplicates(c *gin.Context) {
	var in struct {
		Name    string   `json:"name"`
		TaxCode string   `json:"tax_code"`
		Phones  []string `json:"phones"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	ctx := c.Request.Context()
	rows, err := h.Pool.Query(ctx, `SELECT o.id, o.code, o.name, coalesce(o.tax_code,''), CASE WHEN o.is_customer THEN 'customer' WHEN o.lead_stage IS NOT NULL THEN 'lead' ELSE 'supplier' END, u.full_name,
		coalesce((SELECT array_agg(phone) FROM contacts WHERE organization_id = o.id AND phone IS NOT NULL), '{}')
		FROM organizations o LEFT JOIN users u ON u.id = o.owner_id`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	want := nameWords(in.Name)
	phones := map[string]bool{}
	for _, ph := range in.Phones {
		if d := digits(ph); len(d) >= 8 {
			phones[d] = true
		}
	}
	tax := strings.TrimSpace(in.TaxCode)
	out := []Duplicate{}
	for rows.Next() {
		var d Duplicate
		var t string
		var ps []string
		if err := rows.Scan(&d.ID, &d.Code, &d.Name, &t, &d.Kind, &d.Owner, &ps); err != nil {
			response.Fail(c, err)
			return
		}
		switch {
		case tax != "" && t == tax:
			d.Reason = "tax_code"
		case similar(want, nameWords(d.Name)):
			d.Reason = "name"
		default:
			for _, ph := range ps {
				if phones[digits(ph)] {
					d.Reason = "phone"
				}
			}
		}
		if d.Reason != "" {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Reason < out[j].Reason })
	if len(out) > 10 {
		out = out[:10]
	}
	response.OK(c, out)
}

// Owner is a person a lead, quotation or contract can be given to.
type Owner struct {
	ID       uuid.UUID `json:"id"`
	FullName string    `json:"full_name"`
}

// Owners handles GET /people/owners: whom the caller may assign work to (themselves only, for sales).
func (h *Handlers) Owners(c *gin.Context) {
	p := auth.Principal(c)
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT u.id, u.full_name FROM users u WHERE u.is_active
		AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('sales','sales_manager','admin'))
		AND ($1::uuid IS NULL OR u.id = $1) ORDER BY vn_fold(u.full_name), u.full_name, u.id`, p.OwnerFilter())
	if err != nil {
		response.Fail(c, err)
		return
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Owner])
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

var _ = fmt.Sprintf
