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
	"navo-api/internal/progress"
	"navo-api/internal/response"
)

// Contract is a sales contract with its lines' progress.
type Contract struct {
	ID                uuid.UUID `json:"id"`
	Number            *string   `json:"number"`
	DraftNumber       string    `json:"draft_number"`
	Status            string    `json:"status"`
	CustomerID        uuid.UUID `json:"customer_id"`
	CustomerCode      string    `json:"customer_code"`
	CustomerName      string    `json:"customer_name"`
	Title             string    `json:"title"`
	CustomerReference *string   `json:"customer_reference"`
	Currency          string    `json:"currency"`
	SignedOn          *string   `json:"signed_on"`
	OwnerID           uuid.UUID `json:"owner_id"`
	OwnerName         string    `json:"owner_name"`
	Version           int64     `json:"version"`
	DeliveryLocation  *string   `json:"delivery_location"`
	// Value is the sum of quantity × unit price (sales prices, visible with the contract).
	Value      string    `json:"value"`
	FinalDueOn *string   `json:"final_due_on"`
	Lines      []LineRow `json:"lines,omitempty"`
	Summary    *Summary  `json:"summary,omitempty"`
	// List view only (active contracts): delivery progress and what needs attention.
	Progress  *Summary           `json:"progress,omitempty"`
	Attention *ContractAttention `json:"attention,omitempty"`
}

// ContractAttention is the contract list's "needs attention" cell: the most
// severe flag over its lines, the line it belongs to, and how many lines need action.
type ContractAttention struct {
	Count int            `json:"count"`
	Flag  *progress.Flag `json:"flag"`
	Line  string         `json:"line_name,omitempty"`
	Unit  string         `json:"unit,omitempty"`
}

const contractSelect = `SELECT c.id, c.number, c.draft_number, c.status, c.customer_id, o.code, o.name, c.title,
	c.customer_reference, c.currency, c.signed_on, c.owner_id, u.full_name, c.version, c.delivery_location,
	(SELECT coalesce(round(sum(cl.quantity * cl.unit_price), 2), 0)::text FROM contract_lines cl WHERE cl.contract_id = c.id),
	(SELECT max(cl.due_on) FROM contract_lines cl WHERE cl.contract_id = c.id)
	FROM contracts c JOIN organizations o ON o.id = c.customer_id JOIN users u ON u.id = c.owner_id`

func scanContract(row pgx.Row) (Contract, error) {
	var k Contract
	var signed, finalDue *time.Time
	err := row.Scan(&k.ID, &k.Number, &k.DraftNumber, &k.Status, &k.CustomerID, &k.CustomerCode, &k.CustomerName, &k.Title,
		&k.CustomerReference, &k.Currency, &signed, &k.OwnerID, &k.OwnerName, &k.Version, &k.DeliveryLocation, &k.Value, &finalDue)
	k.SignedOn, k.FinalDueOn = dateString(signed), dateString(finalDue)
	if err == nil {
		k.Value = decimalx.MustParse(k.Value).String()
	}
	return k, err
}

func (h *Handlers) loadContract(ctx context.Context, q db.Querier, id uuid.UUID, p *access.Principal) (*Contract, error) {
	k, err := scanContract(q.QueryRow(ctx, contractSelect+` WHERE c.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, p.OwnerFilter()))
	if err != nil {
		return nil, err
	}
	k.Lines, err = QueryLines(ctx, q, p.OwnerFilter(), &k.ID, h.today())
	if err != nil {
		return nil, err
	}
	s := summarize(k.Lines)
	k.Summary = &s
	return &k, nil
}

type contractLineInput struct {
	ProductID      string  `json:"product_id"`
	NameOnContract *string `json:"name_on_contract"`
	Unit           *string `json:"unit"`
	Quantity       string  `json:"quantity"`
	UnitPrice      string  `json:"unit_price"`
	TaxRate        *string `json:"tax_rate"`
	DueOn          *string `json:"due_on"`
}

type contractInput struct {
	CustomerID        string              `json:"customer_id"`
	Title             string              `json:"title"`
	CustomerReference *string             `json:"customer_reference"`
	DeliveryLocation  *string             `json:"delivery_location"`
	Currency          string              `json:"currency"`
	OwnerID           *string             `json:"owner_id"`
	Lines             []contractLineInput `json:"lines"`
}

type parsedLine struct {
	productID uuid.UUID
	name      *string
	unit      *string
	quantity  decimal.Decimal
	unitPrice decimal.Decimal
	taxRate   *decimal.Decimal
	dueOn     *time.Time
}

// CreateContract handles POST /contracts (a draft; numbered when activated).
func (h *Handlers) CreateContract(c *gin.Context) {
	p := auth.Principal(c)
	var in contractInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	customerID := httpx.UUID(&errs, "customer_id", in.CustomerID)
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		errs.Add("title", "required", nil)
	} else if len([]rune(in.Title)) > 300 {
		errs.Add("title", "too_long", map[string]any{"max": 300})
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(in.Currency) != 3 || strings.Trim(in.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		errs.Add("currency", "currency", nil)
	}
	if in.CustomerReference != nil {
		if ref := strings.TrimSpace(*in.CustomerReference); ref == "" {
			in.CustomerReference = nil
		} else if len([]rune(ref)) > 100 {
			errs.Add("customer_reference", "too_long", map[string]any{"max": 100})
		} else {
			in.CustomerReference = &ref
		}
	}
	in.DeliveryLocation = trimmed(in.DeliveryLocation)
	if in.DeliveryLocation != nil && len([]rune(*in.DeliveryLocation)) > 300 {
		errs.Add("delivery_location", "too_long", map[string]any{"max": 300})
	}
	owner := p.UserID
	if in.OwnerID != nil && *in.OwnerID != "" {
		owner = httpx.UUID(&errs, "owner_id", *in.OwnerID)
	}
	if len(in.Lines) == 0 {
		errs.Add("lines", "required", map[string]any{"min": 1})
	}
	lines := make([]parsedLine, len(in.Lines))
	for i, l := range in.Lines {
		f := func(name string) string { return fmt.Sprintf("lines[%d].%s", i, name) }
		lines[i] = parsedLine{
			productID: httpx.UUID(&errs, f("product_id"), l.ProductID),
			quantity:  decimalx.Positive(&errs, f("quantity"), l.Quantity),
			unitPrice: decimalx.NonNegative(&errs, f("unit_price"), l.UnitPrice),
			dueOn:     httpx.Date(&errs, f("due_on"), l.DueOn),
			name:      trimmed(l.NameOnContract),
			unit:      trimmed(l.Unit),
		}
		if l.TaxRate != nil && *l.TaxRate != "" {
			rate := decimalx.NonNegative(&errs, f("tax_rate"), *l.TaxRate)
			if rate.GreaterThan(decimal.NewFromInt(1)) {
				errs.Add(f("tax_rate"), "less_or_equal", map[string]any{"max": "1"})
			}
			lines[i].taxRate = &rate
		}
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	// Sales own their contracts; only users who see the whole company may assign another owner.
	if owner != p.UserID && p.OwnerFilter() != nil {
		response.Fail(c, apperrors.Forbidden("assign_owner"))
		return
	}
	httpx.Command(c, h.Pool, "contracts.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var isCustomer bool
		if err := tx.QueryRow(ctx, `SELECT is_customer FROM organizations WHERE id = $1`, customerID).Scan(&isCustomer); err != nil || !isCustomer {
			return nil, apperrors.Invalid("customer_id", "not_customer", nil)
		}
		products, err := loadProducts(ctx, tx, lines)
		if err != nil {
			return nil, err
		}
		var lineErrs apperrors.FieldErrors
		for i, l := range lines {
			if _, ok := products[l.productID]; !ok {
				lineErrs.Add(fmt.Sprintf("lines[%d].product_id", i), "not_found", nil)
			}
		}
		if err := lineErrs.Err(); err != nil {
			return nil, err
		}
		seq, err := numbering.NextYearly(ctx, tx, "DRAFT", 0)
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO contracts (draft_number, customer_id, title, customer_reference, currency, owner_id, created_by, delivery_location)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			numbering.DraftNumber(seq), customerID, in.Title, in.CustomerReference, in.Currency, owner, p.UserID, in.DeliveryLocation).Scan(&id); err != nil {
			return nil, err
		}
		for i, l := range lines {
			prod := products[l.productID]
			name, unit := prod.name, prod.unit
			if l.name != nil {
				name = *l.name // the customer's wording is kept as the document's snapshot (BR-01)
			}
			if l.unit != nil {
				unit = *l.unit
			}
			if _, err := tx.Exec(ctx, `INSERT INTO contract_lines (contract_id, line_no, product_id, name_on_contract, unit, quantity, unit_price, tax_rate, due_on)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, id, i+1, l.productID, name, unit, l.quantity, l.unitPrice, l.taxRate, l.dueOn); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "contract", id, map[string]any{"title": in.Title, "customer_id": customerID,
			"currency": in.Currency, "owner_id": owner, "lines": len(lines)}); err != nil {
			return nil, err
		}
		return h.loadContract(ctx, tx, id, p)
	})
}

type productInfo struct{ name, unit string }

func loadProducts(ctx context.Context, tx pgx.Tx, lines []parsedLine) (map[uuid.UUID]productInfo, error) {
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.productID
	}
	rows, err := tx.Query(ctx, `SELECT id, name, unit FROM products WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]productInfo{}
	for rows.Next() {
		var id uuid.UUID
		var pi productInfo
		if err := rows.Scan(&id, &pi.name, &pi.unit); err != nil {
			return nil, err
		}
		out[id] = pi
	}
	return out, rows.Err()
}

// GetContract handles GET /contracts/:id.
func (h *Handlers) GetContract(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	k, err := h.loadContract(c.Request.Context(), h.Pool, id, auth.Principal(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, k)
}

// ListContracts handles GET /contracts?tab=action|soon|all|draft&status=&q=&customer_id=&owner_id=&page=.
// The tabs follow the contract list screen (S-03): active contracts needing action,
// with a delivery planned in the next 7 days, or all; drafts on their own. Counts for
// every tab come back with the page so the tab labels stay right while filtering.
func (h *Handlers) ListContracts(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	page, size := httpx.Page(c)
	q := strings.TrimSpace(c.Query("q"))
	var errs apperrors.FieldErrors
	customer := optionalUUID(&errs, "customer_id", c.Query("customer_id"))
	ownerParam := optionalUUID(&errs, "owner_id", c.Query("owner_id"))
	tab := c.Query("tab")
	if tab != "" && tab != "action" && tab != "soon" && tab != "all" && tab != "draft" {
		errs.Add("tab", "one_of", map[string]any{"values": []string{"action", "soon", "all", "draft"}})
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	owner := p.OwnerFilter()
	if owner == nil {
		owner = ownerParam // only people who see everyone can narrow to one owner
	}
	statuses := []string{"draft", "active", "closed", "cancelled"}
	if tab != "" {
		statuses = []string{"draft", "active"}
	} else if s := c.Query("status"); s != "" {
		statuses = []string{s}
	}
	rows, err := h.Pool.Query(ctx, contractSelect+`
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) AND c.status = ANY($2) AND ($4::uuid IS NULL OR c.customer_id = $4)
		  AND ($3 = '' OR c.number ILIKE '%' || $3 || '%' OR c.draft_number ILIKE '%' || $3 || '%' OR c.title ILIKE '%' || $3 || '%'
		       OR o.name ILIKE '%' || $3 || '%' OR o.code ILIKE '%' || $3 || '%' OR c.customer_reference ILIKE '%' || $3 || '%')
		ORDER BY (c.status = 'active') DESC, c.number DESC NULLS LAST, c.created_at DESC, c.id`, owner, statuses, q, customer)
	if err != nil {
		response.Fail(c, err)
		return
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contract, error) { return scanContract(r) })
	if err != nil {
		response.Fail(c, err)
		return
	}
	lines, err := QueryLines(ctx, h.Pool, owner, nil, h.today())
	if err != nil {
		response.Fail(c, err)
		return
	}
	type agg struct {
		rows []LineRow
		soon bool
	}
	byContract := map[uuid.UUID]*agg{}
	soonUntil := h.today().AddDate(0, 0, 7).Format(time.DateOnly)
	for _, l := range lines {
		a := byContract[l.ContractID]
		if a == nil {
			a = &agg{}
			byContract[l.ContractID] = a
		}
		a.rows = append(a.rows, l)
		if l.NextDelivery != nil && l.NextDelivery.PlannedOn <= soonUntil {
			a.soon = true
		}
	}
	counts := map[string]int{"action": 0, "soon": 0, "all": 0, "draft": 0}
	items := []Contract{}
	for _, k := range all {
		if k.Status == "active" {
			a := byContract[k.ID]
			if a == nil {
				a = &agg{}
			}
			s := summarize(a.rows)
			k.Progress = &s
			k.Attention = contractAttention(a.rows)
			counts["all"]++
			if k.Attention.Count > 0 {
				counts["action"]++
			}
			if a.soon {
				counts["soon"]++
			}
			if tab == "" || tab == "all" || (tab == "action" && k.Attention.Count > 0) || (tab == "soon" && a.soon) {
				items = append(items, k)
			}
			continue
		}
		if k.Status == "draft" {
			counts["draft"]++
		}
		if tab == "" || tab == "draft" {
			items = append(items, k)
		}
	}
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	response.ListCounts(c, items[start:end], page, size, len(items), counts)
}

// contractAttention summarises a contract's lines for the list: the most severe
// flag (critical, then warning, info, done) and how many lines need action.
func contractAttention(rows []LineRow) *ContractAttention {
	out := &ContractAttention{}
	best := 99
	for _, r := range rows {
		action := false
		for i := range r.Flags {
			f := r.Flags[i]
			if f.Severity == progress.Critical || f.Severity == progress.Warning {
				action = true
			}
			if rk := flagRank(f.Severity); rk < best {
				best, out.Flag, out.Line, out.Unit = rk, &f, r.NameOnContract, r.Unit
			}
		}
		if action {
			out.Count++
		}
	}
	return out
}

func flagRank(s progress.Severity) int {
	switch s {
	case progress.Critical:
		return 0
	case progress.Warning:
		return 1
	case progress.Info:
		return 2
	}
	return 3
}

func optionalUUID(errs *apperrors.FieldErrors, field, raw string) *uuid.UUID {
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		errs.Add(field, "uuid", nil)
		return nil
	}
	return &id
}

// Owner is someone responsible for contracts (the owner filter).
type Owner struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// ListOwners handles GET /contracts/owners: people who own contracts the caller can see.
func (h *Handlers) ListOwners(c *gin.Context) {
	p := auth.Principal(c)
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT DISTINCT u.id, u.full_name FROM contracts c JOIN users u ON u.id = c.owner_id
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) ORDER BY u.full_name, u.id`, p.OwnerFilter())
	if err != nil {
		response.Fail(c, err)
		return
	}
	owners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Owner])
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, owners)
}

type activateInput struct {
	SignedOn *string `json:"signed_on"`
}

// ActivateContract handles POST /contracts/:id/activate: the contract gets its
// official number SC-{year signed}-{nnnn} and becomes usable for POs and deliveries.
func (h *Handlers) ActivateContract(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in activateInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	signed := httpx.Date(&errs, "signed_on", in.SignedOn)
	if signed == nil && len(errs) == 0 {
		errs.Add("signed_on", "required", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contracts.activate", true, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		k, err := lockContract(ctx, tx, id, p, true)
		if err != nil {
			return nil, err
		}
		if k.Status != "draft" {
			return nil, apperrors.Rule("contract_not_draft", "Only a draft contract can be activated.", map[string]any{"status": k.Status})
		}
		seq, err := numbering.NextYearly(ctx, tx, "SC", signed.Year())
		if err != nil {
			return nil, err
		}
		number := numbering.ContractNumber(signed.Year(), seq)
		if _, err := tx.Exec(ctx, `UPDATE contracts SET status = 'active', number = $2, signed_on = $3, version = version + 1, updated_at = now()
			WHERE id = $1`, id, number, *signed); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, "activate", "contract", id, map[string]any{"status": []string{"draft", "active"},
			"number": number, "signed_on": signed.Format(time.DateOnly)}); err != nil {
			return nil, err
		}
		return h.loadContract(ctx, tx, id, p)
	})
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
