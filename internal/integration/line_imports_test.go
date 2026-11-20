//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func (e *env) postFile(token, path, name string, content []byte) result {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = w.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return result{Status: resp.StatusCode, Body: out}
}

// A customer's item list in Excel (title rows above the header, Vietnamese headers, real
// numbers and dates) is read into lines matched by part number or name; nothing is saved.
func TestLineImport(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("tuan@test.vn", []string{"delivery"})
	khoa, tuan := e.login("khoa@test.vn"), e.login("tuan@test.vn")
	e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van bi DN50", "unit": "cái", "manufacturer_code": "VB-50"}, nil), "valve")
	e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Transmitter áp suất 0-40 bar", "unit": "chiếc"}, nil), "transmitter")

	f := excelize.NewFile()
	s := f.GetSheetName(0)
	due := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	rows := [][]any{
		{"YÊU CẦU BÁO GIÁ — Lọc hóa dầu Sông Trà"},
		{},
		{"STT", "Mã hãng", "Tên hàng hóa", "ĐVT", "Số lượng", "Đơn giá", "Thuế suất", "Hạn giao"},
		{1, "vb-50", "Van bi DN 50 (theo bản vẽ)", "", 20, 4850000, "8%", due},
		{2, "", "Transmitter áp suất 0-40 bar", "chiếc", 100, 12400000, 10, "15/12/2026"},
		{3, "", "Bơm định lượng", "bộ", "hai", "", "", ""},
	}
	for i, r := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		_ = f.SetSheetRow(s, cell, &r)
	}
	var buf bytes.Buffer
	_ = f.Write(&buf)

	if r := e.postFile(tuan, "/api/v1/line-imports", "rfq.xlsx", buf.Bytes()); r.Status != 403 {
		t.Fatalf("delivery does not write contracts: %d", r.Status)
	}
	got := e.must(200, e.postFile(khoa, "/api/v1/line-imports", "rfq.xlsx", buf.Bytes()), "read")["lines"].([]any)
	if len(got) != 3 {
		t.Fatalf("three item rows: %v", got)
	}
	one, two, three := got[0].(map[string]any), got[1].(map[string]any), got[2].(map[string]any)
	if one["match"].(map[string]any)["name"] != "Van bi DN50" || one["unit"] != "cái" || one["quantity"] != "20" || one["unit_price"] != "4850000" || one["tax_rate"] != "0.08" || one["due_on"] != "2026-12-15" {
		t.Fatalf("part number match keeps the customer's wording: %v", one)
	}
	if one["name"] != "Van bi DN 50 (theo bản vẽ)" {
		t.Fatalf("name: %v", one["name"])
	}
	if two["match"] == nil || two["tax_rate"] != "0.1" || two["due_on"] != "2026-12-15" {
		t.Fatalf("exact name match: %v", two)
	}
	if three["match"] != nil || len(three["problems"].([]any)) != 1 || three["problems"].([]any)[0] != "quantity" {
		t.Fatalf("unknown product and a bad quantity are flagged: %v", three)
	}
	if r := e.postFile(khoa, "/api/v1/line-imports", "x.csv", []byte("a;b\n1;2\n")); r.reason() != "import_header_missing" {
		t.Fatalf("a file without the columns is refused: %v", r.Body)
	}
}
