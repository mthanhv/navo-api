// Package masterdata manages customers, suppliers and the product catalogue (N-02, N-03).
package masterdata

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/crm"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// Handlers serve the master-data endpoints.
type Handlers struct{ Pool *pgxpool.Pool }

var (
	orgCode = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)
	taxCode = regexp.MustCompile(`^[0-9]{10}(-[0-9]{3})?$`) // same rule as the organizations.tax_code CHECK
)

// Organization is a customer and/or supplier.
type Organization struct {
	ID                 uuid.UUID `json:"id"`
	Code               string    `json:"code"`
	Name               string    `json:"name"`
	IsCustomer         bool      `json:"is_customer"`
	IsSupplier         bool      `json:"is_supplier"`
	TaxCode            *string   `json:"tax_code"`
	InvoiceCompanyName *string   `json:"invoice_company_name"`
	InvoiceTaxCode     *string   `json:"invoice_tax_code"`
	InvoiceAddress     *string   `json:"invoice_address"`
	InvoiceEmail       *string   `json:"invoice_email"`
	// Contacts is how many people are recorded; PrimaryContact is the main one, for lists.
	Contacts       int     `json:"contacts"`
	PrimaryContact *string `json:"primary_contact"`
	PrimaryPhone   *string `json:"primary_phone"`
	PrimaryEmail   *string `json:"primary_email"`
	// OwnerID is the salesperson who looks after a customer; CanManageContacts says
	// whether the caller may change or email this partner's contacts.
	OwnerID           *uuid.UUID `json:"owner_id"`
	OwnerName         *string    `json:"owner_name"`
	CanManageContacts bool       `json:"can_manage_contacts"`
}

type organizationInput struct {
	Code               string  `json:"code"`
	Name               string  `json:"name"`
	IsCustomer         bool    `json:"is_customer"`
	IsSupplier         bool    `json:"is_supplier"`
	TaxCode            *string `json:"tax_code"`
	InvoiceCompanyName *string `json:"invoice_company_name"`
	InvoiceTaxCode     *string `json:"invoice_tax_code"`
	InvoiceAddress     *string `json:"invoice_address"`
	InvoiceEmail       *string `json:"invoice_email"`
	// OwnerID: absent keeps the owner; "" clears it. Only people who see the whole
	// company may set it; a salesperson creating a customer becomes its owner.
	OwnerID *string `json:"owner_id"`
	// Contacts: only on create, added in the same transaction (add customer/supplier form).
	// Later changes go through the contact endpoints, which apply the owner rule.
	Contacts []crm.NewContact `json:"contacts"`
	creating bool
}

// resolveOwner returns the owner to store (keep=true: leave as is).
func resolveOwner(ctx context.Context, tx pgx.Tx, p *access.Principal, in organizationInput, creating bool) (owner *uuid.UUID, keep bool, err error) {
	if p.OwnerFilter() != nil { // a salesperson
		if in.OwnerID != nil {
			return nil, false, apperrors.Forbidden("owner_change")
		}
		if creating && in.IsCustomer {
			id := p.UserID
			return &id, false, nil
		}
		return nil, !creating, nil
	}
	if in.OwnerID == nil {
		return nil, !creating, nil
	}
	if *in.OwnerID == "" {
		return nil, false, nil
	}
	id, perr := uuid.Parse(*in.OwnerID)
	var ok bool
	if perr == nil {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u JOIN user_roles r ON r.user_id = u.id
			WHERE u.id = $1 AND u.is_active AND r.role IN ('sales', 'admin'))`, id).Scan(&ok); err != nil {
			return nil, false, err
		}
	}
	if !ok {
		return nil, false, apperrors.Invalid("owner_id", "not_found", nil)
	}
	return &id, false, nil
}

func (in *organizationInput) validate() error {
	in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
	in.Name = strings.TrimSpace(in.Name)
	var errs apperrors.FieldErrors
	if in.Code == "" && in.creating {
		// Generated from the name on save (SuggestCode).
	} else if !orgCode.MatchString(in.Code) {
		errs.Add("code", "format", map[string]any{"pattern": "2-12 letters A-Z or digits"})
	}
	if in.Name == "" {
		errs.Add("name", "required", nil)
	} else if len([]rune(in.Name)) > 300 {
		errs.Add("name", "too_long", map[string]any{"max": 300})
	}
	if !in.IsCustomer && !in.IsSupplier {
		errs.Add("is_customer", "customer_or_supplier", nil)
	}
	if in.TaxCode != nil && *in.TaxCode != "" && !taxCode.MatchString(*in.TaxCode) {
		errs.Add("tax_code", "tax_code", nil)
	}
	if in.TaxCode != nil && *in.TaxCode == "" {
		in.TaxCode = nil
	}
	in.InvoiceCompanyName = blankToNil(in.InvoiceCompanyName)
	in.InvoiceTaxCode = blankToNil(in.InvoiceTaxCode)
	in.InvoiceAddress = blankToNil(in.InvoiceAddress)
	in.InvoiceEmail = blankToNil(in.InvoiceEmail)
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{
		{"invoice_company_name", in.InvoiceCompanyName, 300},
		{"invoice_address", in.InvoiceAddress, 300},
	} {
		if field.value != nil && len([]rune(*field.value)) > field.max {
			errs.Add(field.name, "too_long", map[string]any{"max": field.max})
		}
	}
	if in.InvoiceTaxCode != nil && !taxCode.MatchString(*in.InvoiceTaxCode) {
		errs.Add("invoice_tax_code", "tax_code", nil)
	}
	if in.InvoiceEmail != nil {
		address, err := mail.ParseAddress(*in.InvoiceEmail)
		if err != nil || address.Address != *in.InvoiceEmail || len(*in.InvoiceEmail) > 254 ||
			!strings.Contains((*in.InvoiceEmail)[strings.LastIndex(*in.InvoiceEmail, "@")+1:], ".") {
			errs.Add("invoice_email", "email", nil)
		} else {
			lower := strings.ToLower(*in.InvoiceEmail)
			in.InvoiceEmail = &lower
		}
	}
	if len(in.Contacts) > 50 {
		errs.Add("contacts", "too_many", map[string]any{"max": 50})
	}
	crm.CheckContacts(&errs, in.Contacts)
	return errs.Err()
}

// CreateOrganization handles POST /organizations.
func (h *Handlers) CreateOrganization(c *gin.Context) {
	p := auth.Principal(c)
	var in organizationInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.creating = true
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "organizations.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		owner, _, err := resolveOwner(ctx, tx, p, in, true)
		if err != nil {
			return nil, err
		}
		if in.Code == "" {
			if in.Code, err = suggestCode(ctx, tx, in.Name, in.IsCustomer); err != nil {
				return nil, err
			}
		}
		var id uuid.UUID
		err = tx.QueryRow(ctx, `INSERT INTO organizations
			(code, name, is_customer, is_supplier, tax_code, invoice_company_name, invoice_tax_code, invoice_address, invoice_email, owner_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			in.Code, in.Name, in.IsCustomer, in.IsSupplier, in.TaxCode, in.InvoiceCompanyName, in.InvoiceTaxCode, in.InvoiceAddress, in.InvoiceEmail, owner).Scan(&id)
		if err != nil {
			if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
				return nil, apperrors.Invalid("code", "taken", nil)
			}
			return nil, err
		}
		if len(in.Contacts) > 0 {
			if !p.Can(access.ContactsWrite) {
				return nil, apperrors.Forbidden("missing_permission")
			}
			if err := crm.InsertContacts(ctx, tx, id, in.Contacts); err != nil {
				return nil, err
			}
		}
		o, err := loadOrganization(ctx, tx, p, id)
		if err != nil {
			return nil, err
		}
		return o, audit.Record(ctx, tx, "create", "organization", id, map[string]any{"code": o.Code, "name": o.Name,
			"is_customer": o.IsCustomer, "is_supplier": o.IsSupplier, "owner_id": owner})
	})
}

// orgSelect reads a partner with its contact summary and owner; placeholder n is the
// caller's owner filter (for can_manage_contacts).
func orgSelect(n int) string {
	return `SELECT o.id, o.code, o.name, o.is_customer, o.is_supplier, o.tax_code,
	o.invoice_company_name, o.invoice_tax_code, o.invoice_address, o.invoice_email,
	(SELECT count(*) FROM contacts ct WHERE ct.organization_id = o.id), pc.full_name, pc.phone, pc.email, o.owner_id, ou.full_name, ` + crm.Manageable(n)
}

const orgFrom = `FROM organizations o LEFT JOIN contacts pc ON pc.organization_id = o.id AND pc.is_primary LEFT JOIN users ou ON ou.id = o.owner_id`

func (o *Organization) dest() []any {
	return []any{&o.ID, &o.Code, &o.Name, &o.IsCustomer, &o.IsSupplier, &o.TaxCode, &o.InvoiceCompanyName, &o.InvoiceTaxCode, &o.InvoiceAddress, &o.InvoiceEmail,
		&o.Contacts, &o.PrimaryContact, &o.PrimaryPhone, &o.PrimaryEmail,
		&o.OwnerID, &o.OwnerName, &o.CanManageContacts}
}

// loadOrganization reads one partner as the caller sees it.
func loadOrganization(ctx context.Context, q pgx.Tx, p *access.Principal, id uuid.UUID) (*Organization, error) {
	var o Organization
	err := q.QueryRow(ctx, orgSelect(2)+` `+orgFrom+` WHERE o.id = $1`, id, p.OwnerFilter()).Scan(o.dest()...)
	return &o, err
}

// ListOrganizations handles GET /organizations?role=customer|supplier&q= (q also matches
// a contact's name, email or phone).
func (h *Handlers) ListOrganizations(c *gin.Context) {
	page, size := httpx.Page(c)
	role := c.Query("role")
	q := strings.TrimSpace(c.Query("q"))
	items := []Organization{}
	total := 0
	rows, err := h.Pool.Query(c.Request.Context(), orgSelect(5)+`, count(*) OVER () `+orgFrom+`
		WHERE ($1 = '' OR ($1 = 'customer' AND o.is_customer) OR ($1 = 'supplier' AND o.is_supplier) OR ($1 = 'prospect' AND (o.is_customer OR (o.lead_stage IS NOT NULL AND o.lead_stage <> 'lost'))))
		  AND ($2 = '' OR strpos(vn_fold(o.code), vn_fold($2)) > 0 OR strpos(vn_fold(o.name), vn_fold($2)) > 0
		       OR EXISTS (SELECT 1 FROM contacts ct WHERE ct.organization_id = o.id AND (strpos(vn_fold(ct.full_name), vn_fold($2)) > 0 OR strpos(vn_fold(ct.email), vn_fold($2)) > 0 OR strpos(vn_fold(ct.phone), vn_fold($2)) > 0)))
		ORDER BY vn_fold(o.name), o.name, o.id LIMIT $3 OFFSET $4`, role, q, size, (page-1)*size, auth.Principal(c).OwnerFilter())
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var o Organization
		if err := rows.Scan(append(o.dest(), &total)...); err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, o)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, page, size, total)
}

// Product is a catalogue item; documents keep their own snapshot of names (BR-01).
type Product struct {
	ID               uuid.UUID `json:"id"`
	Code             string    `json:"code"`
	Name             string    `json:"name"`
	Manufacturer     *string   `json:"manufacturer"`
	ManufacturerCode *string   `json:"manufacturer_code"`
	Unit             string    `json:"unit"`
	// Description is the specification in words; Origin is the country of manufacture.
	Description *string `json:"description"`
	Origin      *string `json:"origin"`
	// Supplier is who we usually buy it from (optional), with their code for the item.
	SupplierID       *uuid.UUID `json:"supplier_id"`
	SupplierName     *string    `json:"supplier_name"`
	SupplierItemCode *string    `json:"supplier_item_code"`
}

type productInput struct {
	// Code: typed by the user, or empty to number it (SP-00001…). Empty on edit keeps it.
	Code             *string `json:"code"`
	Name             string  `json:"name"`
	Manufacturer     *string `json:"manufacturer"`
	ManufacturerCode *string `json:"manufacturer_code"`
	Unit             string  `json:"unit"`
	Description      *string `json:"description"`
	Origin           *string `json:"origin"`
	SupplierID       *string `json:"supplier_id"`
	SupplierItemCode *string `json:"supplier_item_code"`
	supplier         *uuid.UUID
}

const productSelect = `SELECT p.id, p.code, p.name, p.manufacturer, p.manufacturer_code, p.unit, p.description, p.origin, p.supplier_id, s.name, p.supplier_item_code
	FROM products p LEFT JOIN organizations s ON s.id = p.supplier_id`

func (p *Product) dest() []any {
	return []any{&p.ID, &p.Code, &p.Name, &p.Manufacturer, &p.ManufacturerCode, &p.Unit, &p.Description, &p.Origin, &p.SupplierID, &p.SupplierName, &p.SupplierItemCode}
}

// save inserts (id nil) or updates a product and returns it as stored.
func (in *productInput) save(ctx context.Context, tx pgx.Tx, id *uuid.UUID) (*Product, error) {
	if in.supplier != nil {
		var isSupplier bool
		if err := tx.QueryRow(ctx, `SELECT is_supplier FROM organizations WHERE id = $1`, *in.supplier).Scan(&isSupplier); err != nil || !isSupplier {
			return nil, apperrors.Invalid("supplier_id", "not_supplier", nil)
		}
	}
	var out uuid.UUID
	var err error
	if id == nil {
		err = tx.QueryRow(ctx, `INSERT INTO products (name, manufacturer, manufacturer_code, unit, description, origin, supplier_id, supplier_item_code, code)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, next_product_code())) RETURNING id`, in.Name, in.Manufacturer, in.ManufacturerCode, in.Unit, in.Description, in.Origin, in.supplier, in.SupplierItemCode, in.Code).Scan(&out)
	} else {
		out = *id
		_, err = tx.Exec(ctx, `UPDATE products SET name = $2, manufacturer = $3, manufacturer_code = $4, unit = $5, description = $6, origin = $7,
			supplier_id = $8, supplier_item_code = $9, code = COALESCE($10, code), version = version + 1, updated_at = now() WHERE id = $1`,
			out, in.Name, in.Manufacturer, in.ManufacturerCode, in.Unit, in.Description, in.Origin, in.supplier, in.SupplierItemCode, in.Code)
	}
	if err != nil {
		if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.ConstraintName == "products_code_key" {
				return nil, apperrors.Invalid("code", "taken", nil)
			}
			return nil, apperrors.Invalid("manufacturer_code", "taken", nil)
		}
		return nil, err
	}
	var p Product
	if err := tx.QueryRow(ctx, productSelect+` WHERE p.id = $1`, out).Scan(p.dest()...); err != nil {
		return nil, err
	}
	return &p, nil
}

func (in *productInput) validate() error {
	in.Name, in.Unit = strings.TrimSpace(in.Name), strings.TrimSpace(in.Unit)
	var errs apperrors.FieldErrors
	if in.Code = blankToNil(in.Code); in.Code != nil {
		up := strings.ToUpper(*in.Code)
		in.Code = &up
		if !productCode.MatchString(up) {
			errs.Add("code", "format", map[string]any{"pattern": "2-30 letters A-Z, digits, . _ -"})
		}
	}
	if in.Name == "" {
		errs.Add("name", "required", nil)
	} else if len([]rune(in.Name)) > 300 {
		errs.Add("name", "too_long", map[string]any{"max": 300})
	}
	if in.Unit == "" {
		errs.Add("unit", "required", nil)
	} else if len([]rune(in.Unit)) > 20 {
		errs.Add("unit", "too_long", map[string]any{"max": 20})
	}
	in.Manufacturer, in.ManufacturerCode = blankToNil(in.Manufacturer), blankToNil(in.ManufacturerCode)
	in.Description, in.Origin, in.SupplierItemCode = blankToNil(in.Description), blankToNil(in.Origin), blankToNil(in.SupplierItemCode)
	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{{"manufacturer", in.Manufacturer, 120}, {"manufacturer_code", in.ManufacturerCode, 80}, {"description", in.Description, 2000}, {"origin", in.Origin, 80}, {"supplier_item_code", in.SupplierItemCode, 80}} {
		if f.v != nil && len([]rune(*f.v)) > f.max {
			errs.Add(f.name, "too_long", map[string]any{"max": f.max})
		}
	}
	in.supplier = nil
	if s := blankToNil(in.SupplierID); s != nil {
		id := httpx.UUID(&errs, "supplier_id", *s)
		in.supplier = &id
	}
	return errs.Err()
}

// CreateProduct handles POST /products.
func (h *Handlers) CreateProduct(c *gin.Context) {
	var in productInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "products.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		p, err := in.save(ctx, tx, nil)
		if err != nil {
			return nil, err
		}
		return p, audit.Record(ctx, tx, "create", "product", p.ID, map[string]any{"name": p.Name, "unit": p.Unit,
			"manufacturer": p.Manufacturer, "manufacturer_code": p.ManufacturerCode, "supplier_id": p.SupplierID})
	})
}

// ListProducts handles GET /products?q=.
func (h *Handlers) ListProducts(c *gin.Context) {
	page, size := httpx.Page(c)
	q := strings.TrimSpace(c.Query("q"))
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT p.id, p.code, p.name, p.manufacturer, p.manufacturer_code, p.unit, p.description, p.origin, p.supplier_id, s.name, p.supplier_item_code, count(*) OVER ()
		FROM products p LEFT JOIN organizations s ON s.id = p.supplier_id
		WHERE $1 = '' OR strpos(vn_fold(p.code), vn_fold($1)) > 0 OR strpos(vn_fold(p.name), vn_fold($1)) > 0 OR strpos(vn_fold(p.manufacturer_code), vn_fold($1)) > 0 OR strpos(vn_fold(p.manufacturer), vn_fold($1)) > 0 OR strpos(vn_fold(p.supplier_item_code), vn_fold($1)) > 0
		ORDER BY vn_fold(p.name), p.name, p.id LIMIT $2 OFFSET $3`, q, size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []Product{}
	total := 0
	for rows.Next() {
		var p Product
		if err := rows.Scan(append(p.dest(), &total)...); err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, page, size, total)
}

func blankToNil(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// UpdateOrganization handles PATCH /organizations/:id. Documents keep the names they
// were made with; a partner still used as customer (contracts) or supplier (orders)
// keeps that role.
func (h *Handlers) UpdateOrganization(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in organizationInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if len(in.Contacts) > 0 {
		response.Fail(c, apperrors.Invalid("contacts", "edit_contacts_separately", nil))
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	httpx.Command(c, h.Pool, "organizations.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id = $1 FOR UPDATE`, id); err != nil {
			return nil, err
		}
		before, err := loadOrganization(ctx, tx, p, id)
		if err != nil {
			return nil, err
		}
		owner, keepOwner, err := resolveOwner(ctx, tx, p, in, false)
		if err != nil {
			return nil, err
		}
		if keepOwner {
			owner = before.OwnerID
		}
		var usedAsCustomer, usedAsSupplier bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contracts WHERE customer_id = $1), EXISTS (SELECT 1 FROM purchase_orders WHERE supplier_id = $1)`, id).
			Scan(&usedAsCustomer, &usedAsSupplier); err != nil {
			return nil, err
		}
		var errs apperrors.FieldErrors
		if usedAsCustomer && !in.IsCustomer {
			errs.Add("is_customer", "in_use", nil)
		}
		if usedAsSupplier && !in.IsSupplier {
			errs.Add("is_supplier", "in_use", nil)
		}
		if err := errs.Err(); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET code = $2, name = $3, is_customer = $4, is_supplier = $5, tax_code = $6,
				invoice_company_name = $7, invoice_tax_code = $8, invoice_address = $9, invoice_email = $10, owner_id = $11,
				version = version + 1, updated_at = now() WHERE id = $1`, id, in.Code, in.Name, in.IsCustomer, in.IsSupplier, in.TaxCode,
			in.InvoiceCompanyName, in.InvoiceTaxCode, in.InvoiceAddress, in.InvoiceEmail, owner); err != nil {
			if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
				return nil, apperrors.Invalid("code", "taken", nil)
			}
			return nil, err
		}
		o, err := loadOrganization(ctx, tx, p, id)
		if err != nil {
			return nil, err
		}
		return o, audit.Record(ctx, tx, "update", "organization", id, map[string]any{"before": before, "after": o})
	})
}

// UpdateProduct handles PATCH /products/:id; documents keep their own snapshot (BR-01).
func (h *Handlers) UpdateProduct(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in productInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "products.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var before Product
		if err := tx.QueryRow(ctx, productSelect+` WHERE p.id = $1 FOR UPDATE OF p`, id).Scan(before.dest()...); err != nil {
			return nil, err
		}
		p, err := in.save(ctx, tx, &id)
		if err != nil {
			return nil, err
		}
		return p, audit.Record(ctx, tx, "update", "product", id, map[string]any{"before": before, "after": p})
	})
}

var productCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{1,29}$`)

var accents = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// suggestCode makes a free partner code from the name: initials of the words (Lọc hóa dầu
// Sông Trà → LHDST), or the start of a one-word name; KH/NCC when the name gives too little;
// a number is added when the code is taken (LHDST2).
func suggestCode(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string, customer bool) (string, error) {
	folded, _, _ := transform.String(accents, strings.ToUpper(name))
	folded = strings.NewReplacer("Đ", "D", "đ", "D").Replace(folded)
	words := strings.FieldsFunc(folded, func(r rune) bool { return !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') })
	// Company-form words carry no meaning in a code.
	skip := map[string]bool{"CONG": true, "TY": true, "CTY": true, "TNHH": true, "CP": true, "CO": true, "PHAN": true, "MTV": true, "JSC": true, "LTD": true, "CORP": true, "COMPANY": true}
	base := ""
	kept := []string{}
	for _, w := range words {
		if !skip[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 1 {
		base = kept[0]
	} else {
		for _, w := range kept {
			base += w[:1]
		}
	}
	if len(base) > 8 {
		base = base[:8]
	}
	if len(base) < 2 {
		base = "NCC"
		if customer {
			base = "KH"
		}
	}
	for n := 1; n < 10000; n++ {
		code := base
		if n > 1 || base == "KH" || base == "NCC" {
			suffix := strconv.Itoa(n)
			code = base[:min(len(base), 12-len(suffix))] + suffix
		}
		var taken bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE code = $1)`, code).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return code, nil
		}
	}
	return "", apperrors.Rule("code_unavailable", "No free code; type one.", nil)
}

// GenerateCode is suggestCode for other packages (leads get a code from their name too).
func GenerateCode(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	return suggestCode(ctx, tx, name, true)
}

// SuggestCode handles GET /organizations/suggest-code?name=&role=customer|supplier.
func (h *Handlers) SuggestCode(c *gin.Context) {
	code, err := suggestCode(c.Request.Context(), h.Pool, c.Query("name"), c.Query("role") != "supplier")
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"code": code})
}

// SuggestProductCode handles GET /products/suggest-code: the next free SP-number (it is used up).
func (h *Handlers) SuggestProductCode(c *gin.Context) {
	var code string
	if err := h.Pool.QueryRow(c.Request.Context(), `SELECT next_product_code()`).Scan(&code); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"code": code})
}
