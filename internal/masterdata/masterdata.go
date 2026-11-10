// Package masterdata manages customers, suppliers and the product catalogue (N-02, N-03).
package masterdata

import (
	"context"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
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
	ID         uuid.UUID `json:"id"`
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	IsCustomer bool      `json:"is_customer"`
	IsSupplier bool      `json:"is_supplier"`
	TaxCode    *string   `json:"tax_code"`
	// Contacts is how many people are recorded; PrimaryContact is the main one, for lists.
	Contacts       int     `json:"contacts"`
	PrimaryContact *string `json:"primary_contact"`
	PrimaryPhone   *string `json:"primary_phone"`
	PrimaryEmail   *string `json:"primary_email"`
}

type organizationInput struct {
	Code       string  `json:"code"`
	Name       string  `json:"name"`
	IsCustomer bool    `json:"is_customer"`
	IsSupplier bool    `json:"is_supplier"`
	TaxCode    *string `json:"tax_code"`
}

func (in *organizationInput) validate() error {
	in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
	in.Name = strings.TrimSpace(in.Name)
	var errs apperrors.FieldErrors
	if !orgCode.MatchString(in.Code) {
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
	return errs.Err()
}

// CreateOrganization handles POST /organizations.
func (h *Handlers) CreateOrganization(c *gin.Context) {
	var in organizationInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "organizations.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		o := Organization{Code: in.Code, Name: in.Name, IsCustomer: in.IsCustomer, IsSupplier: in.IsSupplier, TaxCode: in.TaxCode}
		err := tx.QueryRow(ctx, `INSERT INTO organizations (code, name, is_customer, is_supplier, tax_code)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, o.Code, o.Name, o.IsCustomer, o.IsSupplier, o.TaxCode).Scan(&o.ID)
		if err != nil {
			if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
				return nil, apperrors.Invalid("code", "taken", nil)
			}
			return nil, err
		}
		return o, audit.Record(ctx, tx, "create", "organization", o.ID, map[string]any{"code": o.Code, "name": o.Name,
			"is_customer": o.IsCustomer, "is_supplier": o.IsSupplier})
	})
}

// ListOrganizations handles GET /organizations?role=customer|supplier&q= (q also matches
// a contact's name, email or phone).
func (h *Handlers) ListOrganizations(c *gin.Context) {
	page, size := httpx.Page(c)
	role := c.Query("role")
	q := strings.TrimSpace(c.Query("q"))
	items := []Organization{}
	total := 0
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT o.id, o.code, o.name, o.is_customer, o.is_supplier, o.tax_code,
			(SELECT count(*) FROM contacts ct WHERE ct.organization_id = o.id), pc.full_name, pc.phone, pc.email, count(*) OVER ()
		FROM organizations o LEFT JOIN contacts pc ON pc.organization_id = o.id AND pc.is_primary
		WHERE ($1 = '' OR ($1 = 'customer' AND o.is_customer) OR ($1 = 'supplier' AND o.is_supplier))
		  AND ($2 = '' OR o.code ILIKE '%' || $2 || '%' OR o.name ILIKE '%' || $2 || '%'
		       OR EXISTS (SELECT 1 FROM contacts ct WHERE ct.organization_id = o.id AND (ct.full_name ILIKE '%' || $2 || '%' OR ct.email ILIKE '%' || $2 || '%' OR ct.phone ILIKE '%' || $2 || '%')))
		ORDER BY o.name, o.id LIMIT $3 OFFSET $4`, role, q, size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Code, &o.Name, &o.IsCustomer, &o.IsSupplier, &o.TaxCode, &o.Contacts, &o.PrimaryContact, &o.PrimaryPhone, &o.PrimaryEmail, &total); err != nil {
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

const productSelect = `SELECT p.id, p.name, p.manufacturer, p.manufacturer_code, p.unit, p.description, p.origin, p.supplier_id, s.name, p.supplier_item_code
	FROM products p LEFT JOIN organizations s ON s.id = p.supplier_id`

func (p *Product) dest() []any {
	return []any{&p.ID, &p.Name, &p.Manufacturer, &p.ManufacturerCode, &p.Unit, &p.Description, &p.Origin, &p.SupplierID, &p.SupplierName, &p.SupplierItemCode}
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
		err = tx.QueryRow(ctx, `INSERT INTO products (name, manufacturer, manufacturer_code, unit, description, origin, supplier_id, supplier_item_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`, in.Name, in.Manufacturer, in.ManufacturerCode, in.Unit, in.Description, in.Origin, in.supplier, in.SupplierItemCode).Scan(&out)
	} else {
		out = *id
		_, err = tx.Exec(ctx, `UPDATE products SET name = $2, manufacturer = $3, manufacturer_code = $4, unit = $5, description = $6, origin = $7,
			supplier_id = $8, supplier_item_code = $9, version = version + 1, updated_at = now() WHERE id = $1`,
			out, in.Name, in.Manufacturer, in.ManufacturerCode, in.Unit, in.Description, in.Origin, in.supplier, in.SupplierItemCode)
	}
	if err != nil {
		if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
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
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT p.id, p.name, p.manufacturer, p.manufacturer_code, p.unit, p.description, p.origin, p.supplier_id, s.name, p.supplier_item_code, count(*) OVER ()
		FROM products p LEFT JOIN organizations s ON s.id = p.supplier_id
		WHERE $1 = '' OR p.name ILIKE '%' || $1 || '%' OR p.manufacturer_code ILIKE '%' || $1 || '%' OR p.manufacturer ILIKE '%' || $1 || '%' OR p.supplier_item_code ILIKE '%' || $1 || '%'
		ORDER BY p.name, p.id LIMIT $2 OFFSET $3`, q, size, (page-1)*size)
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
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "organizations.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var before Organization
		if err := tx.QueryRow(ctx, `SELECT id, code, name, is_customer, is_supplier, tax_code FROM organizations WHERE id = $1 FOR UPDATE`, id).
			Scan(&before.ID, &before.Code, &before.Name, &before.IsCustomer, &before.IsSupplier, &before.TaxCode); err != nil {
			return nil, err
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
		o := Organization{ID: id, Code: in.Code, Name: in.Name, IsCustomer: in.IsCustomer, IsSupplier: in.IsSupplier, TaxCode: in.TaxCode}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET code = $2, name = $3, is_customer = $4, is_supplier = $5, tax_code = $6,
				version = version + 1, updated_at = now() WHERE id = $1`, id, o.Code, o.Name, o.IsCustomer, o.IsSupplier, o.TaxCode); err != nil {
			if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
				return nil, apperrors.Invalid("code", "taken", nil)
			}
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
