// Package imports loads a company's starting master data — customers and suppliers (with
// their main contact) and products — from an Excel or CSV file (REQUIREMENTS 5.7, N-10,
// AC-20). It is always preview then commit: every row gets an outcome and a reason, nothing
// is merged on a guess, and importing the same file twice creates nothing the second time.
package imports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/response"
)

// Handlers serve the master-data import.
type Handlers struct{ Pool *pgxpool.Pool }

const (
	maxBytes = 5 << 20
	maxRows  = 5000
)

var (
	orgCode = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)
	taxCode = regexp.MustCompile(`^[0-9]{10}(-[0-9]{3})?$`)
)

// column is one template column: its key, the header in the template, and the header
// spellings accepted when reading (compared without accents or case).
type column struct {
	key, header string
	accepts     []string
	required    bool
}

var templates = map[string][]column{
	"partners": {
		{"code", "Mã (Code)", []string{"ma", "code", "ma doi tac", "ma khach hang", "ma ncc"}, true},
		{"name", "Tên (Name)", []string{"ten", "name", "ten doi tac", "ten khach hang", "ten ncc"}, true},
		{"tax_code", "Mã số thuế (Tax code)", []string{"ma so thue", "mst", "tax code"}, false},
		{"is_customer", "Khách hàng (Customer) x", []string{"khach hang", "customer", "la khach hang"}, false},
		{"is_supplier", "Nhà cung cấp (Supplier) x", []string{"nha cung cap", "supplier", "ncc", "la nha cung cap"}, false},
		{"contact", "Người liên hệ (Contact)", []string{"nguoi lien he", "contact", "lien he"}, false},
		{"job_title", "Chức vụ (Role)", []string{"chuc vu", "role", "job title", "phong ban"}, false},
		{"email", "Email", []string{"email", "e-mail"}, false},
		{"phone", "Điện thoại (Phone)", []string{"dien thoai", "phone", "so dien thoai", "sdt"}, false},
	},
	"products": {
		{"name", "Tên sản phẩm (Name)", []string{"ten san pham", "ten hang", "ten", "name", "product"}, true},
		{"unit", "Đơn vị (Unit)", []string{"don vi", "dvt", "don vi tinh", "unit"}, true},
		{"manufacturer", "Hãng (Manufacturer)", []string{"hang", "hang san xuat", "manufacturer", "brand"}, false},
		{"manufacturer_code", "Mã hãng (Manufacturer code)", []string{"ma hang", "ma hang san xuat", "manufacturer code", "part number"}, false},
		{"origin", "Xuất xứ (Origin)", []string{"xuat xu", "origin"}, false},
		{"description", "Mô tả (Description)", []string{"mo ta", "thong so", "description"}, false},
		{"supplier_code", "Mã NCC (Supplier code)", []string{"ma ncc", "ma nha cung cap", "supplier code"}, false},
	},
}

var fold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func normalize(s string) string {
	s, _, _ = transform.String(fold, strings.ToLower(strings.TrimSpace(s)))
	s = strings.ReplaceAll(s, "đ", "d")
	s = strings.Map(func(r rune) rune {
		if r == '(' || r == ')' || r == '*' || r == ':' {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// Row is one data row and what importing it does.
type Row struct {
	Line   int               `json:"line"` // row number in the file, as the user sees it
	Values map[string]string `json:"values"`
	// Outcome: new (will be created), exists (already in Navo, unchanged), conflict (looks
	// like an existing record but differs: decide by hand), error (fix the file).
	Outcome string   `json:"outcome"`
	Reasons []string `json:"reasons"`
	// MatchName names the existing record a conflict or exists row points at.
	MatchName *string `json:"match_name,omitempty"`
}

// Result is a preview or a commit.
type Result struct {
	Kind     string         `json:"kind"`
	FileHash string         `json:"file_hash"`
	Counts   map[string]int `json:"counts"`
	Rows     []Row          `json:"rows"`
	// Created is set by commit: how many records were added.
	Created *int `json:"created,omitempty"`
}

// Template handles GET /imports/:kind/template: an empty workbook with the expected headers.
func (h *Handlers) Template(c *gin.Context) {
	kind := c.Param("kind")
	cols, ok := templates[kind]
	if !ok {
		response.Fail(c, apperrors.NotFound())
		return
	}
	f := excelize.NewFile()
	defer f.Close()
	sheet := f.GetSheetName(0)
	for i, col := range cols {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, col.header)
		_ = f.SetColWidth(sheet, string(rune('A'+i)), string(rune('A'+i)), 24)
	}
	style, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"EDF2FE"}, Pattern: 1}})
	last, _ := excelize.CoordinatesToCellName(len(cols), 1)
	_ = f.SetCellStyle(sheet, "A1", last, style)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="navo-`+kind+`-template.xlsx"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buf.Bytes())
}

func permissionFor(kind string) access.Permission {
	if kind == "products" {
		return access.CatalogWrite
	}
	return access.PartnersWrite
}

// readFile returns the rows of the first sheet (xlsx) or the CSV, and the file's hash.
func readFile(c *gin.Context) ([][]string, string, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+1<<20)
	fh, err := c.FormFile("file")
	if err != nil {
		return nil, "", apperrors.Invalid("file", "required", nil)
	}
	if fh.Size > maxBytes {
		return nil, "", apperrors.Invalid("file", "file_too_large", map[string]any{"max_mb": maxBytes >> 20})
	}
	src, err := fh.Open()
	if err != nil {
		return nil, "", err
	}
	defer src.Close()
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if bytes.HasPrefix(raw, []byte("PK")) {
		f, err := excelize.OpenReader(bytes.NewReader(raw))
		if err != nil {
			return nil, "", apperrors.Invalid("file", "file_type", map[string]any{"type": "xlsx, csv"})
		}
		defer f.Close()
		rows, err := f.GetRows(f.GetSheetName(0))
		return rows, hash, err
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	if bytes.Count(raw[:min(len(raw), 2000)], []byte(";")) > bytes.Count(raw[:min(len(raw), 2000)], []byte(",")) {
		r.Comma = ';' // Excel in Vietnamese locale saves CSV with semicolons
	}
	rows, err := r.ReadAll()
	if err != nil {
		return nil, "", apperrors.Invalid("file", "file_type", map[string]any{"type": "xlsx, csv"})
	}
	return rows, hash, nil
}

// parse finds the header row (within the first 10 rows) and maps data rows to column keys.
func parse(kind string, rows [][]string) ([]Row, error) {
	cols := templates[kind]
	header, index := -1, map[string]int{}
	for i := 0; i < len(rows) && i < 10 && header < 0; i++ {
		found := map[string]int{}
		for j, cell := range rows[i] {
			n := normalize(cell)
			if n == "" {
				continue
			}
			for _, col := range cols {
				if _, done := found[col.key]; done {
					continue
				}
				if headerMatches(n, col) {
					found[col.key] = j
					break
				}
			}
		}
		ok := true
		for _, col := range cols {
			if _, has := found[col.key]; col.required && !has {
				ok = false
			}
		}
		if ok {
			header, index = i, found
		}
	}
	if header < 0 {
		return nil, apperrors.Rule("import_header_missing", "The file has no header row with the required columns. Start from the template.", nil)
	}
	out := []Row{}
	for i := header + 1; i < len(rows); i++ {
		r := Row{Line: i + 1, Values: map[string]string{}, Reasons: []string{}}
		empty := true
		for key, j := range index {
			if j < len(rows[i]) {
				v := strings.TrimSpace(rows[i][j])
				r.Values[key] = v
				if v != "" {
					empty = false
				}
			}
		}
		if empty {
			continue
		}
		out = append(out, r)
		if len(out) > maxRows {
			return nil, apperrors.Invalid("file", "too_many", map[string]any{"max": maxRows})
		}
	}
	return out, nil
}

// headerMatches compares a normalised header cell with a column: the template header, or
// one of the accepted spellings (exact, or as a "… (english)" prefix).
func headerMatches(n string, col column) bool {
	if n == normalize(col.header) {
		return true
	}
	for _, a := range col.accepts {
		if n == a || strings.HasPrefix(n, a+" ") && len(strings.Fields(n)) <= len(strings.Fields(a))+2 {
			return true
		}
	}
	return false
}

func yes(v string) bool {
	switch normalize(v) {
	case "x", "1", "yes", "y", "true", "co", "c":
		return true
	}
	return false
}

func tooLong(r *Row, key string, max int) {
	if len([]rune(r.Values[key])) > max {
		r.Reasons = append(r.Reasons, key+":too_long")
	}
}

// classify validates each row and compares it with what Navo already holds. It reads only;
// commit calls it again inside its transaction.
func classify(ctx context.Context, q db.Querier, kind string, rows []Row) error {
	seen := map[string]int{}
	for i := range rows {
		r := &rows[i]
		v := r.Values
		if kind == "partners" {
			v["code"] = strings.ToUpper(v["code"])
			if !orgCode.MatchString(v["code"]) {
				r.Reasons = append(r.Reasons, "code:format")
			}
			if v["name"] == "" {
				r.Reasons = append(r.Reasons, "name:required")
			}
			tooLong(r, "name", 300)
			if v["tax_code"] != "" && !taxCode.MatchString(v["tax_code"]) {
				r.Reasons = append(r.Reasons, "tax_code:tax_code")
			}
			if !yes(v["is_customer"]) && !yes(v["is_supplier"]) {
				r.Reasons = append(r.Reasons, "is_customer:customer_or_supplier")
			}
			if v["email"] != "" {
				if a, err := mail.ParseAddress(v["email"]); err != nil || a.Address != v["email"] {
					r.Reasons = append(r.Reasons, "email:email")
				}
			}
			if (v["email"] != "" || v["phone"] != "" || v["job_title"] != "") && v["contact"] == "" {
				r.Reasons = append(r.Reasons, "contact:required")
			}
			tooLong(r, "contact", 200)
			tooLong(r, "job_title", 120)
			tooLong(r, "phone", 40)
			if prev, dup := seen["code:"+v["code"]]; dup && v["code"] != "" {
				r.Reasons = append(r.Reasons, "code:duplicate_in_file:"+itoa(prev))
			}
			seen["code:"+v["code"]] = r.Line
			if v["tax_code"] != "" {
				if prev, dup := seen["tax:"+v["tax_code"]]; dup {
					r.Reasons = append(r.Reasons, "tax_code:duplicate_in_file:"+itoa(prev))
				}
				seen["tax:"+v["tax_code"]] = r.Line
			}
		} else {
			if v["name"] == "" {
				r.Reasons = append(r.Reasons, "name:required")
			}
			if v["unit"] == "" {
				r.Reasons = append(r.Reasons, "unit:required")
			}
			tooLong(r, "name", 300)
			tooLong(r, "unit", 20)
			tooLong(r, "manufacturer", 120)
			tooLong(r, "manufacturer_code", 80)
			tooLong(r, "origin", 80)
			tooLong(r, "description", 2000)
			key := normalize(v["manufacturer"]) + "|" + normalize(v["manufacturer_code"])
			if v["manufacturer_code"] == "" {
				key = "name:" + normalize(v["name"])
			}
			if prev, dup := seen[key]; dup {
				r.Reasons = append(r.Reasons, "name:duplicate_in_file:"+itoa(prev))
			}
			seen[key] = r.Line
		}
		if len(r.Reasons) > 0 {
			r.Outcome = "error"
			continue
		}
		if err := compare(ctx, q, kind, r); err != nil {
			return err
		}
	}
	return nil
}

// compare decides new / exists / conflict against existing records. A match is only ever
// by code (partners) or maker + maker code (products); names alone never merge — a
// similar name is a conflict for a person to decide.
func compare(ctx context.Context, q db.Querier, kind string, r *Row) error {
	v := r.Values
	var name string
	if kind == "partners" {
		var tax *string
		var cust, supp bool
		err := q.QueryRow(ctx, `SELECT name, tax_code, is_customer, is_supplier FROM organizations WHERE code = $1`, v["code"]).Scan(&name, &tax, &cust, &supp)
		switch {
		case err == nil:
			r.MatchName = &name
			same := normalize(name) == normalize(v["name"]) && (v["tax_code"] == "" || (tax != nil && *tax == v["tax_code"]))
			if same && (!yes(v["is_customer"]) || cust) && (!yes(v["is_supplier"]) || supp) {
				r.Outcome = "exists"
			} else {
				r.Outcome, r.Reasons = "conflict", append(r.Reasons, "code_used_differently")
			}
			return nil
		case err != pgx.ErrNoRows:
			return err
		}
		if v["tax_code"] != "" {
			var code string
			if err := q.QueryRow(ctx, `SELECT code, name FROM organizations WHERE tax_code = $1 LIMIT 1`, v["tax_code"]).Scan(&code, &name); err == nil {
				r.MatchName = &name
				r.Outcome, r.Reasons = "conflict", append(r.Reasons, "tax_code_used:"+code)
				return nil
			} else if err != pgx.ErrNoRows {
				return err
			}
		}
		if err := q.QueryRow(ctx, `SELECT name FROM organizations WHERE lower(name) = lower($1) LIMIT 1`, v["name"]).Scan(&name); err == nil {
			r.MatchName = &name
			r.Outcome, r.Reasons = "conflict", append(r.Reasons, "same_name")
			return nil
		} else if err != pgx.ErrNoRows {
			return err
		}
		r.Outcome = "new"
		return nil
	}
	if v["supplier_code"] != "" {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE code = upper($1) AND is_supplier)`, v["supplier_code"]).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			r.Outcome, r.Reasons = "error", append(r.Reasons, "supplier_code:not_supplier")
			return nil
		}
	}
	if v["manufacturer_code"] != "" {
		var unit string
		err := q.QueryRow(ctx, `SELECT name, unit FROM products WHERE lower(coalesce(manufacturer, '')) = lower($1) AND lower(manufacturer_code) = lower($2)`,
			v["manufacturer"], v["manufacturer_code"]).Scan(&name, &unit)
		if err == nil {
			r.MatchName = &name
			if normalize(unit) == normalize(v["unit"]) {
				r.Outcome = "exists"
			} else {
				r.Outcome, r.Reasons = "conflict", append(r.Reasons, "unit_differs:"+unit)
			}
			return nil
		} else if err != pgx.ErrNoRows {
			return err
		}
	}
	err := q.QueryRow(ctx, `SELECT name FROM products WHERE lower(name) = lower($1) LIMIT 1`, v["name"]).Scan(&name)
	switch {
	case err == nil:
		r.MatchName = &name
		if v["manufacturer_code"] == "" {
			r.Outcome = "exists" // same name, no maker code to tell them apart: treat as the same item
		} else {
			r.Outcome, r.Reasons = "conflict", append(r.Reasons, "same_name")
		}
	case err == pgx.ErrNoRows:
		r.Outcome = "new"
	default:
		return err
	}
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }

func count(rows []Row) map[string]int {
	c := map[string]int{"new": 0, "exists": 0, "conflict": 0, "error": 0}
	for _, r := range rows {
		c[r.Outcome]++
	}
	return c
}

func (h *Handlers) load(c *gin.Context) (string, []Row, string, bool) {
	kind := c.Param("kind")
	if _, ok := templates[kind]; !ok {
		response.Fail(c, apperrors.NotFound())
		return "", nil, "", false
	}
	if !auth.Principal(c).Can(permissionFor(kind)) {
		response.Fail(c, apperrors.Forbidden("missing_permission"))
		return "", nil, "", false
	}
	raw, hash, err := readFile(c)
	if err != nil {
		response.Fail(c, err)
		return "", nil, "", false
	}
	rows, err := parse(kind, raw)
	if err != nil {
		response.Fail(c, err)
		return "", nil, "", false
	}
	return kind, rows, hash, true
}

// Preview handles POST /imports/:kind/preview (multipart "file"). Nothing is saved.
func (h *Handlers) Preview(c *gin.Context) {
	kind, rows, hash, ok := h.load(c)
	if !ok {
		return
	}
	if err := classify(c.Request.Context(), h.Pool, kind, rows); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, Result{Kind: kind, FileHash: hash, Counts: count(rows), Rows: rows})
}

// Commit handles POST /imports/:kind/commit (multipart "file", form "file_hash"): re-checks
// every row inside one transaction and creates only the "new" rows. Conflicts and errors are
// left for a person; the file must be the one previewed (same hash).
func (h *Handlers) Commit(c *gin.Context) {
	kind, rows, hash, ok := h.load(c)
	if !ok {
		return
	}
	if c.PostForm("file_hash") != hash {
		response.Fail(c, apperrors.Conflict("import_file_changed", "The file is not the one you previewed. Preview it again.", nil))
		return
	}
	p := auth.Principal(c)
	created := 0
	err := db.InTx(c.Request.Context(), h.Pool, func(tx pgx.Tx) error {
		ctx := c.Request.Context()
		// One import at a time, so two people committing the same file create it once.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7428101)`); err != nil {
			return err
		}
		if err := classify(ctx, tx, kind, rows); err != nil {
			return err
		}
		for _, r := range rows {
			if r.Outcome != "new" {
				continue
			}
			if err := insert(ctx, tx, p, kind, r); err != nil {
				return err
			}
			created++
		}
		return audit.Record(ctx, tx, "import", kind, uuid.Nil, map[string]any{"file_hash": hash, "created": created, "rows": len(rows), "counts": count(rows)})
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	for i := range rows {
		if rows[i].Outcome == "new" {
			rows[i].Outcome = "created"
		}
	}
	response.OK(c, Result{Kind: kind, FileHash: hash, Counts: count(rows), Rows: rows, Created: &created})
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func insert(ctx context.Context, tx pgx.Tx, p *access.Principal, kind string, r Row) error {
	v := r.Values
	if kind == "partners" {
		var owner *uuid.UUID
		if yes(v["is_customer"]) && p.OwnerFilter() != nil {
			id := p.UserID
			owner = &id
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO organizations (code, name, is_customer, is_supplier, tax_code, owner_id) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			v["code"], v["name"], yes(v["is_customer"]), yes(v["is_supplier"]), opt(v["tax_code"]), owner).Scan(&id); err != nil {
			return err
		}
		if v["contact"] != "" {
			email := opt(strings.ToLower(v["email"]))
			if _, err := tx.Exec(ctx, `INSERT INTO contacts (organization_id, full_name, job_title, email, phone, is_primary) VALUES ($1, $2, $3, $4, $5, true)`,
				id, v["contact"], opt(v["job_title"]), email, opt(v["phone"])); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, "create", "organization", id, map[string]any{"code": v["code"], "name": v["name"], "source": "import"})
	}
	var supplier *uuid.UUID
	if v["supplier_code"] != "" {
		var sid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE code = upper($1)`, v["supplier_code"]).Scan(&sid); err != nil {
			return err
		}
		supplier = &sid
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO products (name, unit, manufacturer, manufacturer_code, origin, description, supplier_id) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		v["name"], v["unit"], opt(v["manufacturer"]), opt(v["manufacturer_code"]), opt(v["origin"]), opt(v["description"]), supplier).Scan(&id); err != nil {
		return err
	}
	return audit.Record(ctx, tx, "create", "product", id, map[string]any{"name": v["name"], "unit": v["unit"], "source": "import"})
}
