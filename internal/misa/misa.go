// Package misa exchanges data with MISA SME 2023 (offline) through Excel files
// (REQUIREMENTS 5.11, mockup Trao đổi MISA): money documents recorded in Navo are
// exported once, in batches; MISA's catalogue lists are imported to learn MISA codes.
//
// The sheet layouts follow MISA SME's "Nhập khẩu từ Excel" templates as far as known;
// they must be checked against the company's MISA installation before go-live (O-18),
// so column order and default accounts live in one place below.
package misa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// Default accounts (Vietnamese SME chart, Circular 133). Confirm with the accountant (O-18).
var accounts = map[string]string{
	"receivable": "131", "revenue": "5111", "vat_out": "33311",
	"payable": "331", "inventory": "156", "vat_in": "1331",
	"bank": "1121", "cash": "1111",
}

// Handlers serve the MISA exchange.
type Handlers struct{ Pool *pgxpool.Pool }

// Types of documents exported.
var docTypes = []string{"sales", "purchase", "cash"}

// doc is one money document ready for export.
type doc struct {
	Type, ID, Number, PartnerKind, PartnerID, PartnerName, PartnerCode string
	Date                                                               time.Time
	Description, LegalNumber, Currency, Method                         string
	Net, VAT, Total                                                    string
}

func period(raw string) (time.Time, time.Time, error) {
	start, err := time.Parse("2006-01", raw)
	if err != nil {
		return time.Time{}, time.Time{}, apperrors.Invalid("period", "format", nil)
	}
	return start, start.AddDate(0, 1, 0), nil
}

// pending lists documents of the period not exported yet (exportID nil) or of one export.
func pending(ctx context.Context, q db.Querier, from, to time.Time, types []string, exportID *uuid.UUID) ([]doc, error) {
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	filter := `x.misa_export_id IS NULL AND x.d >= $1 AND x.d < $2`
	args := []any{from, to}
	if exportID != nil {
		filter, args = `x.misa_export_id = $1`, []any{*exportID}
	}
	sql := `SELECT * FROM (
		SELECT 'sales' AS t, i.id::text, i.number, 'customer' AS pk, o.id::text AS pid, o.name, coalesce(o.misa_code, '') AS code, i.issued_on AS d,
		       i.description, i.legal_number, i.currency::text, '' AS method, i.amount_net::text, i.vat_amount::text, i.amount_total::text, i.misa_export_id
		FROM sales_invoices i JOIN contracts c ON c.id = i.contract_id JOIN organizations o ON o.id = c.customer_id WHERE i.status = 'issued'
		UNION ALL
		SELECT 'purchase', i.id::text, i.number, 'supplier', o.id::text, o.name, coalesce(o.misa_code, ''), i.issued_on,
		       'Mua hàng theo hóa đơn ' || i.legal_number, i.legal_number, i.currency::text, '', i.amount_net::text, i.vat_amount::text, i.amount_total::text, i.misa_export_id
		FROM purchase_invoices i JOIN organizations o ON o.id = i.supplier_id WHERE i.status = 'recorded'
		UNION ALL
		SELECT 'receipt', r.id::text, r.number, 'customer', o.id::text, o.name, coalesce(o.misa_code, ''), r.received_on,
		       'Thu tiền ' || o.name || coalesce(' · ' || r.reference, ''), '', r.currency::text, r.method, '0', '0', r.amount::text, r.misa_export_id
		FROM receipts r JOIN organizations o ON o.id = r.customer_id WHERE r.status = 'posted'
		UNION ALL
		SELECT 'payment', p.id::text, p.number, 'supplier', o.id::text, o.name, coalesce(o.misa_code, ''), p.paid_on,
		       'Trả tiền ' || o.name || coalesce(' · ' || p.reference, ''), '', p.currency::text, p.method, '0', '0', p.amount::text, p.misa_export_id
		FROM payments p JOIN organizations o ON o.id = p.supplier_id WHERE p.status = 'posted'
	) x WHERE ` + filter + ` ORDER BY x.d, x.number`
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []doc{}
	for rows.Next() {
		var d doc
		var exp *uuid.UUID
		if err := rows.Scan(&d.Type, &d.ID, &d.Number, &d.PartnerKind, &d.PartnerID, &d.PartnerName, &d.PartnerCode, &d.Date,
			&d.Description, &d.LegalNumber, &d.Currency, &d.Method, &d.Net, &d.VAT, &d.Total, &exp); err != nil {
			return nil, err
		}
		d.Currency = strings.TrimSpace(d.Currency)
		group := map[string]string{"sales": "sales", "purchase": "purchase", "receipt": "cash", "payment": "cash"}[d.Type]
		if want[group] {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// Issue is a partner that has no MISA code yet; its documents wait until it has one.
type Issue struct {
	Kind string `json:"kind"` // customer | supplier
	ID   string `json:"id"`
	Name string `json:"name"`
	Docs int    `json:"docs"`
}

func types(c *gin.Context) []string {
	raw := c.DefaultQuery("types", strings.Join(docTypes, ","))
	out := []string{}
	for _, t := range strings.Split(raw, ",") {
		for _, k := range docTypes {
			if strings.TrimSpace(t) == k {
				out = append(out, k)
			}
		}
	}
	return out
}

// Preview handles GET /misa/preview?period=YYYY-MM&types=sales,purchase,cash: what is
// ready, what waits for a MISA code, counts per type and earlier exports.
func (h *Handlers) Preview(c *gin.Context) {
	ctx := c.Request.Context()
	from, to, err := period(c.DefaultQuery("period", time.Now().Format("2006-01")))
	if err != nil {
		response.Fail(c, err)
		return
	}
	all, err := pending(ctx, h.Pool, from, to, docTypes, nil)
	if err != nil {
		response.Fail(c, err)
		return
	}
	selected := map[string]bool{}
	for _, t := range types(c) {
		selected[t] = true
	}
	counts := map[string]int{"sales": 0, "purchase": 0, "cash": 0}
	ready := 0
	issues := map[string]*Issue{}
	order := []string{}
	for _, d := range all {
		g := map[string]string{"sales": "sales", "purchase": "purchase", "receipt": "cash", "payment": "cash"}[d.Type]
		counts[g]++
		if !selected[g] {
			continue
		}
		if d.PartnerCode == "" {
			if issues[d.PartnerID] == nil {
				issues[d.PartnerID] = &Issue{Kind: d.PartnerKind, ID: d.PartnerID, Name: d.PartnerName}
				order = append(order, d.PartnerID)
			}
			issues[d.PartnerID].Docs++
			continue
		}
		ready++
	}
	list := []Issue{}
	for _, id := range order {
		list = append(list, *issues[id])
	}
	history, err := h.history(ctx)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"period": from.Format("2006-01"), "counts": counts, "ready": ready, "issues": list, "history": history, "accounts": accounts})
}

// Export is one file sent to MISA.
type Export struct {
	ID               uuid.UUID      `json:"id"`
	Period           string         `json:"period"`
	Batch            int            `json:"batch"`
	FileName         string         `json:"file_name"`
	Counts           map[string]int `json:"counts"`
	CreatedBy        string         `json:"created_by"`
	CreatedAt        time.Time      `json:"created_at"`
	CorrectionNeeded bool           `json:"correction_needed"`
}

func (h *Handlers) history(ctx context.Context) ([]Export, error) {
	rows, err := h.Pool.Query(ctx, `SELECT e.id, e.period, e.batch, e.file_name, e.counts, u.full_name, e.created_at, e.correction_needed FROM misa_exports e
		JOIN users u ON u.id = e.created_by ORDER BY e.created_at DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Export{}
	for rows.Next() {
		var e Export
		var counts []byte
		if err := rows.Scan(&e.ID, &e.Period, &e.Batch, &e.FileName, &counts, &e.CreatedBy, &e.CreatedAt, &e.CorrectionNeeded); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(counts, &e.Counts)
		out = append(out, e)
	}
	return out, rows.Err()
}

type exportInput struct {
	Period string   `json:"period"`
	Types  []string `json:"types"`
}

// CreateExport handles POST /misa/exports (idempotent): takes every ready document of the
// period, marks it exported in this batch and records the batch. Documents of partners
// without a MISA code stay for the next batch.
func (h *Handlers) CreateExport(c *gin.Context) {
	p := auth.Principal(c)
	var in exportInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	from, to, err := period(in.Period)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if len(in.Types) == 0 {
		in.Types = docTypes
	}
	for _, typ := range in.Types {
		if typ != "sales" && typ != "purchase" && typ != "cash" {
			response.Fail(c, apperrors.Invalid("types", "one_of", nil))
			return
		}
	}
	httpx.Command(c, h.Pool, "misa.export", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('misa_export'))`); err != nil {
			return nil, err
		}
		docs, err := pending(ctx, tx, from, to, in.Types, nil)
		if err != nil {
			return nil, err
		}
		ready := docs[:0]
		for _, d := range docs {
			if d.PartnerCode != "" {
				ready = append(ready, d)
			}
		}
		if len(ready) == 0 {
			return nil, apperrors.Rule("nothing_to_export", "No document is ready to export for this period.", nil)
		}
		var batch int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(batch), 0) + 1 FROM misa_exports WHERE period = $1`, from.Format("2006-01")).Scan(&batch); err != nil {
			return nil, err
		}
		counts := map[string]int{}
		for _, d := range ready {
			counts[d.Type]++
		}
		content, err := workbook(ready)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		e := Export{ID: uuid.New(), Period: from.Format("2006-01"), Batch: batch, Counts: counts, CreatedBy: p.Name, CreatedAt: time.Now()}
		e.FileName = fmt.Sprintf("NAVO_MISA_%s_lo%d.xlsx", e.Period, batch)
		cj, _ := json.Marshal(counts)
		if _, err := tx.Exec(ctx, `INSERT INTO misa_exports (id, period, batch, file_name, counts, created_by, file_content, file_sha256, mapping_version) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			e.ID, e.Period, batch, e.FileName, cj, p.UserID, content, fmt.Sprintf("%x", sum), "sme2023-draft-v1"); err != nil {
			return nil, err
		}
		tables := map[string]string{"sales": "sales_invoices", "purchase": "purchase_invoices", "receipt": "receipts", "payment": "payments"}
		for _, d := range ready {
			if _, err := tx.Exec(ctx, `UPDATE `+tables[d.Type]+` SET misa_export_id = $1 WHERE id = $2 AND misa_export_id IS NULL`, e.ID, d.ID); err != nil {
				return nil, err
			}
		}
		return e, audit.Record(ctx, tx, "export", "misa_export", e.ID, map[string]any{"period": e.Period, "batch": batch, "counts": counts})
	})
}

// File downloads the exact bytes frozen when the batch was prepared.
func (h *Handlers) File(c *gin.Context) {
	ctx := c.Request.Context()
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var name string
	var b []byte
	var checksum *string
	if err := h.Pool.QueryRow(ctx, `SELECT file_name, file_content, file_sha256 FROM misa_exports WHERE id = $1`, id).Scan(&name, &b, &checksum); err != nil {
		response.Fail(c, err)
		return
	}
	if b == nil || checksum == nil {
		response.Fail(c, apperrors.Rule("legacy_export_unavailable", "This older batch has no frozen file. Use the originally downloaded workbook; it cannot safely be reconstructed.", nil))
		return
	}
	sum := sha256.Sum256(b)
	if fmt.Sprintf("%x", sum) != *checksum {
		response.Fail(c, fmt.Errorf("export checksum mismatch"))
		return
	}
	c.Header("ETag", `"`+*checksum+`"`)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	c.Header("Cache-Control", "private, no-store")
	c.Data(200, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", b)
}

// workbook lays the documents out one sheet per MISA voucher type.
func workbook(docs []doc) ([]byte, error) {
	// Excel numeric cells have at most 15 significant decimal digits. Reject
	// amounts outside that envelope instead of silently rounding customer money.
	for _, d := range docs {
		for _, raw := range []string{d.Net, d.VAT, d.Total} {
			v := decimalx.MustParse(raw)
			if v.Abs().GreaterThan(decimalx.MustParse("9999999999999.99")) {
				return nil, apperrors.Rule("export_amount_precision", "An amount is too large for an exact Excel currency export.", map[string]any{"number": d.Number})
			}
		}
	}
	f := excelize.NewFile()
	defer f.Close()
	sheets := []struct {
		name, typ string
		head      []string
		row       func(d doc) []any
	}{
		{"Ban hang", "sales", []string{"Ngày hạch toán", "Ngày chứng từ", "Số chứng từ", "Mã khách hàng", "Tên khách hàng", "Diễn giải", "Số hóa đơn", "Ngày hóa đơn",
			"Loại tiền", "Thành tiền", "Tiền thuế GTGT", "Tổng tiền thanh toán", "TK nợ", "TK doanh thu", "TK thuế GTGT"},
			func(d doc) []any {
				return []any{d.Date, d.Date, d.Number, d.PartnerCode, d.PartnerName, d.Description, d.LegalNumber, d.Date, d.Currency, num(d.Net), num(d.VAT), num(d.Total),
					accounts["receivable"], accounts["revenue"], accounts["vat_out"]}
			}},
		{"Mua hang", "purchase", []string{"Ngày hạch toán", "Ngày chứng từ", "Số chứng từ", "Mã nhà cung cấp", "Tên nhà cung cấp", "Diễn giải", "Số hóa đơn", "Ngày hóa đơn",
			"Loại tiền", "Giá trị hàng", "Tiền thuế GTGT", "Tổng tiền thanh toán", "TK kho", "TK thuế GTGT", "TK công nợ"},
			func(d doc) []any {
				return []any{d.Date, d.Date, d.Number, d.PartnerCode, d.PartnerName, d.Description, d.LegalNumber, d.Date, d.Currency, num(d.Net), num(d.VAT), num(d.Total),
					accounts["inventory"], accounts["vat_in"], accounts["payable"]}
			}},
		{"Thu tien", "receipt", []string{"Ngày hạch toán", "Ngày chứng từ", "Số chứng từ", "Mã đối tượng", "Tên đối tượng", "Diễn giải", "Loại tiền", "Số tiền", "TK nợ", "TK có"},
			func(d doc) []any {
				return []any{d.Date, d.Date, d.Number, d.PartnerCode, d.PartnerName, d.Description, d.Currency, num(d.Total), cashAccount(d.Method), accounts["receivable"]}
			}},
		{"Chi tien", "payment", []string{"Ngày hạch toán", "Ngày chứng từ", "Số chứng từ", "Mã đối tượng", "Tên đối tượng", "Diễn giải", "Loại tiền", "Số tiền", "TK nợ", "TK có"},
			func(d doc) []any {
				return []any{d.Date, d.Date, d.Number, d.PartnerCode, d.PartnerName, d.Description, d.Currency, num(d.Total), accounts["payable"], cashAccount(d.Method)}
			}},
	}
	dateStyle, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr("dd/mm/yyyy")})
	headStyle, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	first := true
	for _, s := range sheets {
		if first {
			if err := f.SetSheetName("Sheet1", s.name); err != nil {
				return nil, err
			}
			first = false
		} else if _, err := f.NewSheet(s.name); err != nil {
			return nil, err
		}
		for i, hcell := range s.head {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(s.name, cell, hcell)
			_ = f.SetCellStyle(s.name, cell, cell, headStyle)
			col, _ := excelize.ColumnNumberToName(i + 1)
			_ = f.SetColWidth(s.name, col, col, 16)
		}
		r := 2
		for _, d := range docs {
			if d.Type != s.typ {
				continue
			}
			for i, v := range s.row(d) {
				cell, _ := excelize.CoordinatesToCellName(i+1, r)
				_ = f.SetCellValue(s.name, cell, v)
				if _, ok := v.(time.Time); ok {
					_ = f.SetCellStyle(s.name, cell, cell, dateStyle)
				}
			}
			r++
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ptr[T any](v T) *T { return &v }

func num(s string) float64 {
	// Excel stores numbers as doubles; amounts have 2 decimals and stay exact well beyond 10^12.
	f, _ := decimalx.MustParse(s).Float64()
	return f
}

func cashAccount(method string) string {
	if method == "cash" {
		return accounts["cash"]
	}
	return accounts["bank"]
}

type codeInput struct {
	Kind string `json:"kind"` // organization | product
	ID   string `json:"id"`
	Code string `json:"code"`
}

// SetCode handles PUT /misa/codes: the MISA code of a customer, supplier or product.
func (h *Handlers) SetCode(c *gin.Context) {
	var in codeInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	table := map[string]string{"organization": "organizations", "product": "products"}[in.Kind]
	if table == "" {
		errs.Add("kind", "one_of", map[string]any{"values": []string{"organization", "product"}})
	}
	id := httpx.UUID(&errs, "id", in.ID)
	in.Code = strings.TrimSpace(in.Code)
	if in.Code == "" || len(in.Code) > 50 {
		errs.Add("code", "required", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "misa.code", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('misa_catalogue'))`); err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `UPDATE `+table+` SET misa_code = $2 WHERE id = $1`, id, in.Code)
		if err != nil {
			if apperrors.From(err).Reason == "duplicate" {
				return nil, apperrors.Invalid("code", "taken", nil)
			}
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, apperrors.NotFound()
		}
		return gin.H{"id": id, "code": in.Code}, audit.Record(ctx, tx, "misa_code", in.Kind, id, map[string]any{"code": in.Code})
	})
}

var fold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func normalize(s string) string {
	s, _, _ = transform.String(fold, strings.ToLower(strings.TrimSpace(s)))
	s = strings.ReplaceAll(s, "đ", "d")
	return strings.Join(strings.Fields(s), " ")
}

// Import handles POST /misa/import (multipart "file"): a catalogue exported from MISA
// (customers, suppliers or products). Preview first; commit requires the exact preview
// hash and rechecks current matches. A conflicting tax identity is never matched by name.
func (h *Handlers) Import(c *gin.Context) {
	ctx := c.Request.Context()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 11<<20)
	fh, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, apperrors.Invalid("file", "required", nil))
		return
	}
	if fh.Size > 10<<20 {
		response.Fail(c, apperrors.Invalid("file", "file_too_large", map[string]any{"max_mb": 10}))
		return
	}
	src, err := fh.Open()
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer src.Close()
	f, err := excelize.OpenReader(src)
	if err != nil {
		response.Fail(c, apperrors.Invalid("file", "file_type", map[string]any{"type": "xlsx"}))
		return
	}
	defer f.Close()
	rows, err := f.GetRows(f.GetSheetName(0))
	if err != nil {
		response.Fail(c, err)
		return
	}
	// Find the header: a row with a "mã …" and a "tên …" column.
	codeCol, nameCol, taxCol, header := -1, -1, -1, -1
	kind := ""
	for i, r := range rows {
		if i > 20 {
			break
		}
		for j, cell := range r {
			n := normalize(cell)
			switch {
			case strings.HasPrefix(n, "ma so thue"):
				taxCol = j
			case strings.HasPrefix(n, "ma ") && codeCol < 0:
				codeCol = j
				switch {
				case strings.Contains(n, "khach"):
					kind = "customer"
				case strings.Contains(n, "nha cung cap") || strings.Contains(n, "ncc"):
					kind = "supplier"
				case strings.Contains(n, "hang") || strings.Contains(n, "vat tu"):
					kind = "product"
				}
			case strings.HasPrefix(n, "ten ") && nameCol < 0:
				nameCol = j
			}
		}
		if codeCol >= 0 && nameCol >= 0 {
			header = i
			break
		}
		codeCol, nameCol, taxCol = -1, -1, -1
	}
	if header < 0 || kind == "" {
		response.Fail(c, apperrors.Rule("misa_file_unknown", "This file does not look like a MISA customer, supplier or product list.", nil))
		return
	}

	commit := c.PostForm("confirm") == "true"
	expected := c.PostForm("preview_hash")
	var result catalogueResult
	err = db.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		// Serialize catalogue code assignments, including manual code changes.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('misa_catalogue'))`); err != nil {
			return err
		}
		var err error
		result, err = previewCatalogue(ctx, tx, kind, rows[header+1:], header+2, codeCol, nameCol, taxCol)
		if err != nil {
			return err
		}
		if !commit {
			return nil
		}
		if expected == "" || expected != result.PreviewHash {
			return apperrors.Conflict("misa_preview_changed", "Preview this file again before confirming its matches.", nil)
		}
		table := "organizations"
		if kind == "product" {
			table = "products"
		}
		for _, row := range result.Rows {
			if row.Outcome != "new" {
				continue
			}
			tag, err := tx.Exec(ctx, `UPDATE `+table+` SET misa_code=$2 WHERE id=$1 AND misa_code IS NULL`, row.MatchID, row.Code)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return apperrors.Conflict("misa_preview_changed", "A match changed. Preview again.", nil)
			}
			if err := audit.Record(ctx, tx, "misa_code", map[bool]string{true: "product", false: "organization"}[kind == "product"], *row.MatchID, map[string]any{"code": row.Code, "basis": row.Basis}); err != nil {
				return err
			}
		}
		result.Preview = false
		return audit.Record(ctx, tx, "import", "misa_catalogue", uuid.Nil, map[string]any{"kind": kind, "matched": result.Matched, "unmatched": len(result.Unmatched)})
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

type catalogueRow struct {
	Line        int        `json:"line"`
	Code        string     `json:"code"`
	Name        string     `json:"name"`
	TaxCode     string     `json:"tax_code"`
	Outcome     string     `json:"outcome"` // new, exists, conflict, unmatched
	Basis       string     `json:"basis"`   // tax_code, name
	MatchID     *uuid.UUID `json:"match_id,omitempty"`
	MatchName   string     `json:"match_name,omitempty"`
	CurrentCode *string    `json:"current_code,omitempty"`
}
type catalogueResult struct {
	Kind        string         `json:"kind"`
	Matched     int            `json:"matched"`
	Already     int            `json:"already"`
	Unmatched   []string       `json:"unmatched"`
	Rows        []catalogueRow `json:"rows"`
	Preview     bool           `json:"preview"`
	PreviewHash string         `json:"preview_hash"`
}

func previewCatalogue(ctx context.Context, tx pgx.Tx, kind string, rows [][]string, firstLine, codeCol, nameCol, taxCol int) (catalogueResult, error) {
	result := catalogueResult{Kind: kind, Preview: true, Rows: []catalogueRow{}, Unmatched: []string{}}
	table := "organizations"
	if kind == "product" {
		table = "products"
	}
	type hit struct {
		id        uuid.UUID
		name      string
		tax, code *string
	}
	for i, cells := range rows {
		get := func(col int) string {
			if col >= 0 && col < len(cells) {
				return strings.TrimSpace(cells[col])
			}
			return ""
		}
		row := catalogueRow{Line: firstLine + i, Code: get(codeCol), Name: get(nameCol), TaxCode: get(taxCol), Outcome: "unmatched"}
		if row.Code == "" && row.Name == "" {
			continue
		}
		if row.Code == "" || row.Name == "" || len(row.Code) > 50 {
			row.Outcome = "conflict"
			result.Rows = append(result.Rows, row)
			continue
		}
		find := func(byTax bool) ([]hit, error) {
			query := `SELECT id,name,NULL::text,misa_code FROM products WHERE lower(name)=lower($1) ORDER BY id LIMIT 2 FOR UPDATE`
			arg := row.Name
			if kind != "product" {
				flag := map[string]string{"customer": "is_customer", "supplier": "is_supplier"}[kind]
				condition := `lower(name)=lower($1)`
				if byTax {
					condition = `tax_code=$1`
					arg = row.TaxCode
				}
				query = `SELECT id,name,tax_code,misa_code FROM organizations WHERE ` + flag + ` AND ` + condition + ` ORDER BY id LIMIT 2 FOR UPDATE`
			}
			rs, err := tx.Query(ctx, query, arg)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rs, func(r pgx.CollectableRow) (hit, error) {
				var h hit
				err := r.Scan(&h.id, &h.name, &h.tax, &h.code)
				return h, err
			})
		}
		var hits []hit
		var err error
		if kind != "product" && row.TaxCode != "" {
			hits, err = find(true)
			row.Basis = "tax_code"
		}
		if err != nil {
			return result, err
		}
		if len(hits) == 0 {
			hits, err = find(false)
			row.Basis = "name"
		}
		if err != nil {
			return result, err
		}
		if len(hits) == 1 {
			h := hits[0]
			row.MatchID = &h.id
			row.MatchName = h.name
			row.CurrentCode = h.code
			switch {
			case row.TaxCode != "" && h.tax != nil && *h.tax != "" && *h.tax != row.TaxCode:
				row.Outcome = "conflict"
			case h.code != nil && *h.code == row.Code:
				row.Outcome = "exists"
			case h.code != nil:
				row.Outcome = "conflict"
			default:
				var used bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE misa_code=$1)`, row.Code).Scan(&used); err != nil {
					return result, err
				}
				if used {
					row.Outcome = "conflict"
				} else {
					row.Outcome = "new"
				}
			}
		} else if len(hits) > 1 {
			row.Outcome = "conflict"
		}
		result.Rows = append(result.Rows, row)
	}
	// Two proposed assignments must never compete for an identity or code.
	ids, codes := map[uuid.UUID]int{}, map[string]int{}
	for _, r := range result.Rows {
		if r.Outcome == "new" {
			ids[*r.MatchID]++
			codes[r.Code]++
		}
	}
	for i := range result.Rows {
		r := &result.Rows[i]
		if r.Outcome == "new" && (ids[*r.MatchID] > 1 || codes[r.Code] > 1) {
			r.Outcome = "conflict"
		}
		switch r.Outcome {
		case "new":
			result.Matched++
		case "exists":
			result.Already++
		default:
			result.Unmatched = append(result.Unmatched, r.Name)
		}
	}
	raw, err := json.Marshal(result.Rows)
	if err != nil {
		return result, err
	}
	result.PreviewHash = fmt.Sprintf("%x", sha256.Sum256(append([]byte(kind+"\n"), raw...)))
	return result, nil
}
