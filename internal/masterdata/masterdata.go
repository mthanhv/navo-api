// Package masterdata manages customers, suppliers and the product catalogue (N-02, N-03).
package masterdata

import (
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

// ListOrganizations handles GET /organizations?role=customer|supplier&q=.
func (h *Handlers) ListOrganizations(c *gin.Context) {
	page, size := httpx.Page(c)
	role := c.Query("role")
	q := strings.TrimSpace(c.Query("q"))
	items := []Organization{}
	total := 0
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT id, code, name, is_customer, is_supplier, tax_code, count(*) OVER ()
		FROM organizations
		WHERE ($1 = '' OR ($1 = 'customer' AND is_customer) OR ($1 = 'supplier' AND is_supplier))
		  AND ($2 = '' OR code ILIKE '%' || $2 || '%' OR name ILIKE '%' || $2 || '%')
		ORDER BY name, id LIMIT $3 OFFSET $4`, role, q, size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Code, &o.Name, &o.IsCustomer, &o.IsSupplier, &o.TaxCode, &total); err != nil {
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
}

type productInput struct {
	Name             string  `json:"name"`
	Manufacturer     *string `json:"manufacturer"`
	ManufacturerCode *string `json:"manufacturer_code"`
	Unit             string  `json:"unit"`
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
		p := Product{Name: in.Name, Manufacturer: in.Manufacturer, ManufacturerCode: in.ManufacturerCode, Unit: in.Unit}
		err := tx.QueryRow(ctx, `INSERT INTO products (name, manufacturer, manufacturer_code, unit) VALUES ($1, $2, $3, $4) RETURNING id`,
			p.Name, p.Manufacturer, p.ManufacturerCode, p.Unit).Scan(&p.ID)
		if err != nil {
			if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
				return nil, apperrors.Invalid("manufacturer_code", "taken", nil)
			}
			return nil, err
		}
		return p, audit.Record(ctx, tx, "create", "product", p.ID, map[string]any{"name": p.Name, "unit": p.Unit,
			"manufacturer": p.Manufacturer, "manufacturer_code": p.ManufacturerCode})
	})
}

// ListProducts handles GET /products?q=.
func (h *Handlers) ListProducts(c *gin.Context) {
	page, size := httpx.Page(c)
	q := strings.TrimSpace(c.Query("q"))
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT id, name, manufacturer, manufacturer_code, unit, count(*) OVER ()
		FROM products WHERE $1 = '' OR name ILIKE '%' || $1 || '%' OR manufacturer_code ILIKE '%' || $1 || '%'
		ORDER BY name, id LIMIT $2 OFFSET $3`, q, size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []Product{}
	total := 0
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Manufacturer, &p.ManufacturerCode, &p.Unit, &total); err != nil {
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
		if err := tx.QueryRow(ctx, `SELECT id, name, manufacturer, manufacturer_code, unit FROM products WHERE id = $1 FOR UPDATE`, id).
			Scan(&before.ID, &before.Name, &before.Manufacturer, &before.ManufacturerCode, &before.Unit); err != nil {
			return nil, err
		}
		p := Product{ID: id, Name: in.Name, Manufacturer: in.Manufacturer, ManufacturerCode: in.ManufacturerCode, Unit: in.Unit}
		if _, err := tx.Exec(ctx, `UPDATE products SET name = $2, manufacturer = $3, manufacturer_code = $4, unit = $5,
				version = version + 1, updated_at = now() WHERE id = $1`, id, p.Name, p.Manufacturer, p.ManufacturerCode, p.Unit); err != nil {
			if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
				return nil, apperrors.Invalid("manufacturer_code", "taken", nil)
			}
			return nil, err
		}
		return p, audit.Record(ctx, tx, "update", "product", id, map[string]any{"before": before, "after": p})
	})
}
