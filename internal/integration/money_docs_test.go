//go:build integration

package integration

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func (e *env) upload(token, ownerType, ownerID, kind, name string, content []byte) result {
	return e.uploadClient(token, ownerType, ownerID, kind, name, content, "")
}

func (e *env) uploadClient(token, ownerType, ownerID, kind, name string, content []byte, clientID string) result {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if clientID != "" {
		_ = w.WriteField("client_id", clientID)
	}
	_ = w.WriteField("owner_type", ownerType)
	_ = w.WriteField("owner_id", ownerID)
	_ = w.WriteField("doc_kind", kind)
	fw, _ := w.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = w.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/files", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return result{Status: resp.StatusCode, Header: resp.Header, Body: out}
}

func (e *env) raw(token, path string) (int, http.Header, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

var pdf = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

// Files land in the right folder from the document they are attached to, and only
// people allowed to see a document (and its prices or money) see its files (5.12, AC-11).
func TestDocumentsTreeAndPermissions(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("mai@test.vn", []string{"accounting"})
	khoa, ha, mai := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("mai@test.vn")
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Lọc hóa dầu Sông Trà", "is_customer": true}, nil), "c")
	sup := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DGA", "name": "Đông Á", "is_supplier": true}, nil), "s")
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "bộ"}, nil), "p")
	k := e.must(201, e.do(khoa, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": "Van", "currency": "VND",
		"lines": []any{map[string]any{"product_id": prod["id"], "quantity": "10", "unit_price": "1000000"}}}, nil), "k")
	kid := k["id"].(string)
	k = e.must(200, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/activate", map[string]any{"signed_on": "2026-09-01"}, key("a")), "act")
	line := lineByNo(k, 1)["contract_line_id"]
	po := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/purchase-orders", map[string]any{"supplier_id": sup["id"], "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": line, "quantity": "10", "unit_price": "800000"}}}, nil), "po")

	signed := e.must(201, e.upload(khoa, "contract", kid, "signed_contract", "Hợp đồng bản ký.pdf", pdf), "upload contract")
	if r := e.upload(khoa, "contract", kid, "signed_contract", "virus.exe", []byte("MZ\x90\x00\x03binary")); r.Status != 400 {
		t.Fatalf("executables are refused: %d %v", r.Status, r.Body)
	}
	if r := e.upload(khoa, "purchase_order", po["id"].(string), "po_signed", "PO.pdf", pdf); r.Status != 403 {
		t.Fatalf("a PO with prices needs purchase-price access: %d", r.Status)
	}
	e.must(201, e.upload(mai, "purchase_order", po["id"].(string), "po_signed", "PO đã ký.pdf", pdf), "accounting uploads the priced PO")
	e.must(201, e.upload(khoa, "purchase_order", po["id"].(string), "cq", "CQ lô 1.pdf", pdf), "CQ")
	if r := e.upload(ha, "contract", kid, "minutes", "x.pdf", pdf); r.Status != 404 {
		t.Fatalf("another salesperson cannot attach to this contract: %d", r.Status)
	}

	tree := e.do(khoa, "GET", "/api/v1/documents/tree", nil, nil)
	tc := tree.Body["data"].([]any)[0].(map[string]any)["contracts"].([]any)[0].(map[string]any)
	if tc["files"].(float64) != 2 {
		t.Fatalf("sales count only files they may see (2, not the priced PO): %v", tc)
	}
	docs := e.must(200, e.do(khoa, "GET", "/api/v1/documents/contracts/"+kid, nil, nil), "docs")
	secs := docs["sections"].([]any)
	if len(secs) != 4 {
		t.Fatalf("sales see sections 1,2,3,6 (no money sections): %d", len(secs))
	}
	if n := len(secs[0].(map[string]any)["groups"].([]any)[0].(map[string]any)["files"].([]any)); n != 1 {
		t.Fatalf("signed contract sits in section 1: %d", n)
	}
	if e.must(200, e.do(mai, "GET", "/api/v1/documents/contracts/"+kid, nil, nil), "docs mai")["files_total"].(float64) != 3 {
		t.Fatal("accounting sees the priced PO too")
	}
	if r := e.do(ha, "GET", "/api/v1/documents/contracts/"+kid, nil, nil); r.Status != 404 {
		t.Fatalf("other sales cannot open the folder: %d", r.Status)
	}

	fileID, linkID := signed["file_id"].(string), signed["link_id"].(string)
	st, h, body := e.raw(khoa, "/api/v1/files/"+fileID+"?link="+linkID)
	if st != 200 || !bytes.Equal(body, pdf) || !strings.Contains(h.Get("Content-Disposition"), "STRA_SC-26-0001_HopDong_") {
		t.Fatalf("download: %d %q", st, h.Get("Content-Disposition"))
	}
	if st, _, _ := e.raw(ha, "/api/v1/files/"+fileID+"?link="+linkID); st != 404 {
		t.Fatalf("other sales cannot download: %d", st)
	}
	st, _, body = e.raw(khoa, "/api/v1/documents/contracts/"+kid+"/zip")
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if st != 200 || err != nil || len(zr.File) != 2 {
		t.Fatalf("zip: %d %v", st, err)
	}
	if !strings.HasPrefix(zr.File[0].Name, "SC-26-0001 - Van/1. Hop dong & phu luc/") {
		t.Fatalf("zip folders mirror the tree without diacritics: %s", zr.File[0].Name)
	}
	e.must(200, e.do(khoa, "DELETE", "/api/v1/attachment-links/"+linkID, nil, nil), "unlink")
	if e.must(200, e.do(khoa, "GET", "/api/v1/documents/contracts/"+kid, nil, nil), "after unlink")["files_total"].(float64) != 1 {
		t.Fatal("unlinked file leaves the folder")
	}
}

// Money: milestones become collectable on their event; receipts are allocated without
// exceeding what is open or what was received; reversals free the money (AC-08).
func TestReceivablesAndAllocation(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("mai@test.vn", []string{"accounting"})
	khoa, mai := e.login("khoa@test.vn"), e.login("mai@test.vn")
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DPN", "name": "Đạm Phương Nam", "is_customer": true}, nil), "c")
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "bộ"}, nil), "p")
	k := e.must(201, e.do(khoa, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": "Van", "currency": "VND",
		"lines": []any{map[string]any{"product_id": prod["id"], "quantity": "10", "unit_price": "100000000"}}}, nil), "k")
	kid := k["id"].(string)
	adv := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/milestones", map[string]any{"name": "Tạm ứng 30%", "percent": "30", "amount": "300000000", "trigger": "signing", "days_after": 14}, nil), "m1")
	acc := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/milestones", map[string]any{"name": "70% sau nghiệm thu", "percent": "70", "amount": "700000000", "trigger": "acceptance", "days_after": 30}, nil), "m2")
	if _, ok := adv["paid"]; ok {
		t.Fatal("sales do not see money received")
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/activate", map[string]any{"signed_on": "2026-09-01"}, key("a")), "act")
	if r := e.do(khoa, "GET", "/api/v1/receivables", nil, nil); r.Status != 403 {
		t.Fatalf("receivables need finance access: %d", r.Status)
	}
	rec := e.must(200, e.do(mai, "GET", "/api/v1/receivables?tab=all", nil, nil), "receivables")
	items := rec["items"].([]any)
	if items[0].(map[string]any)["status"] != "late" || items[0].(map[string]any)["due_on"] != "2026-09-15" || items[1].(map[string]any)["status"] != "wait" {
		t.Fatalf("advance is due 14 days after signing and late; acceptance not yet: %v", items)
	}

	rv := e.must(201, e.do(mai, "POST", "/api/v1/receipts", map[string]any{"customer_id": cust["id"], "received_on": "2026-10-01", "amount": "400000000", "currency": "VND", "method": "bank"}, key("rv")), "receipt")
	if rv["number"] != "RV-26-0001" {
		t.Fatalf("receipt number: %v", rv["number"])
	}
	rid := rv["id"].(string)
	if r := e.do(mai, "POST", "/api/v1/receipts/"+rid+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": acc["id"], "amount": "1"}}}, key("x1")); r.Status != 400 {
		t.Fatalf("a milestone not yet collectable cannot take money: %d %v", r.Status, r.Body)
	}
	if r := e.do(mai, "POST", "/api/v1/receipts/"+rid+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": adv["id"], "amount": "300000001"}}}, key("x2")); r.Status != 400 {
		t.Fatalf("cannot allocate more than is open: %d", r.Status)
	}
	alloc := map[string]any{"allocations": []any{map[string]any{"milestone_id": adv["id"], "amount": "300000000"}}}
	got := e.must(200, e.do(mai, "POST", "/api/v1/receipts/"+rid+"/allocations", alloc, key("ok")), "allocate")
	e.must(200, e.do(mai, "POST", "/api/v1/receipts/"+rid+"/allocations", alloc, key("ok")), "retry")
	if got["unallocated"] != "100000000" {
		t.Fatalf("100M stays unallocated: %v", got["unallocated"])
	}
	if e.must(200, e.do(mai, "GET", "/api/v1/receipts/"+rid, nil, nil), "receipt again")["unallocated"] != "100000000" {
		t.Fatal("a retried allocation is recorded once")
	}
	// Acceptance is a document with quantities and evidence; full acceptance makes the 70% due.
	sup := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "FINSUP", "name": "Supplier", "is_supplier": true}, nil), "supplier")
	po := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/purchase-orders", map[string]any{"supplier_id": sup["id"], "currency": "VND", "lines": []any{map[string]any{"contract_line_id": lineByNo(k, 1)["contract_line_id"], "quantity": "10", "unit_price": "1"}}}, nil), "finance PO")
	e.must(200, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po["id"].(string)+"/confirm", nil, key("fpo")), "finance PO confirm")
	gr := e.must(201, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po["id"].(string)+"/goods-receipts", map[string]any{"received_on": "2026-10-19", "lines": []any{map[string]any{"purchase_order_line_id": po["lines"].([]any)[0].(map[string]any)["id"], "accepted_quantity": "10"}}}, nil), "finance GR")
	e.must(200, e.do(khoa, "POST", "/api/v1/goods-receipts/"+gr["id"].(string)+"/confirm", nil, key("fgr")), "finance GR confirm")
	delivery := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/delivery-orders", map[string]any{"lines": []any{map[string]any{"contract_line_id": lineByNo(k, 1)["contract_line_id"], "quantity": "10"}}}, nil), "finance DO")
	e.must(200, e.do(khoa, "POST", "/api/v1/delivery-orders/"+delivery["id"].(string)+"/confirm", map[string]any{"delivered_on": "2026-10-20"}, key("fdo")), "finance DO confirm")
	e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+kid+"/acceptances", map[string]any{"on": "2026-10-20", "evidence": "Biên bản nghiệm thu 20/10",
		"lines": []any{map[string]any{"contract_line_id": lineByNo(k, 1)["contract_line_id"], "quantity": "10"}}}, key("t")), "acceptance")
	got = e.must(200, e.do(mai, "POST", "/api/v1/receipts/"+rid+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": acc["id"], "amount": "100000000"}}}, key("ok2")), "allocate 2")
	if got["unallocated"] != "0" {
		t.Fatalf("all allocated: %v", got)
	}
	allocs := got["allocations"].([]any)
	e.must(200, e.do(mai, "POST", "/api/v1/allocations/"+allocs[1].(map[string]any)["id"].(string)+"/reverse", map[string]any{"reason": "Nhầm khoản"}, key("rev")), "reverse")
	sum := e.must(200, e.do(mai, "GET", "/api/v1/receivables?tab=all", nil, nil), "after")["summary"].([]any)[0].(map[string]any)
	if sum["unallocated"] != "100000000" || sum["due_open"] != "700000000" || sum["uncollected"] != "700000000" {
		t.Fatalf("summary after reversal: %v", sum)
	}
	if r := e.do(khoa, "DELETE", "/api/v1/milestones/"+adv["id"].(string), nil, key("d")); r.reason() != "milestone_in_use" {
		t.Fatalf("a paid milestone cannot be deleted: %v", r.Body)
	}

	si := e.must(201, e.do(mai, "POST", "/api/v1/contracts/"+kid+"/sales-invoices", map[string]any{"legal_number": "1C26TND 0000123", "issued_on": "2026-09-02",
		"description": "Tạm ứng 30%", "currency": "VND", "amount_net": "277777778", "vat_amount": "22222222", "milestone_id": adv["id"]}, key("si")), "sales invoice")
	if si["number"] != "SI-26-0001-01" || si["amount_total"] != "300000000" {
		t.Fatalf("sales invoice: %v", si)
	}
	if r := e.do(mai, "POST", "/api/v1/contracts/"+kid+"/sales-invoices", map[string]any{"legal_number": "1C26TND 0000123", "issued_on": "2026-09-02",
		"description": "x", "currency": "VND", "amount_net": "1", "vat_amount": "0"}, key("si2")); r.Status != 400 {
		t.Fatalf("an e-invoice number is recorded once: %d", r.Status)
	}
	if n := len(e.must(200, e.do(mai, "GET", "/api/v1/documents/contracts/"+kid, nil, nil), "docs")["sections"].([]any)); n != 6 {
		t.Fatalf("accounting sees all six sections: %d", n)
	}

	// MISA: a customer without a MISA code holds its documents back; one batch exports once.
	if r := e.do(khoa, "GET", "/api/v1/misa/preview?period=2026-09", nil, nil); r.Status != 403 {
		t.Fatalf("MISA exchange is for accounting: %d", r.Status)
	}
	pv := e.must(200, e.do(mai, "GET", "/api/v1/misa/preview?period=2026-09", nil, nil), "preview")
	if pv["ready"].(float64) != 0 || len(pv["issues"].([]any)) != 1 {
		t.Fatalf("missing MISA code blocks the invoice: %v", pv)
	}
	if r := e.do(mai, "POST", "/api/v1/misa/exports", map[string]any{"period": "2026-09"}, key("ex0")); r.reason() != "nothing_to_export" {
		t.Fatalf("nothing ready yet: %v", r.Body)
	}
	e.must(200, e.do(mai, "PUT", "/api/v1/misa/codes", map[string]any{"kind": "organization", "id": cust["id"], "code": "KH0001"}, nil), "code")
	ex := e.must(201, e.do(mai, "POST", "/api/v1/misa/exports", map[string]any{"period": "2026-09"}, key("ex1")), "export")
	if ex["file_name"] != "NAVO_MISA_2026-09_lo1.xlsx" {
		t.Fatalf("export: %v", ex)
	}
	st, _, body := e.raw(mai, "/api/v1/misa/exports/"+ex["id"].(string)+"/file")
	xf, err := excelize.OpenReader(bytes.NewReader(body))
	if st != 200 || err != nil {
		t.Fatalf("xlsx: %d %v", st, err)
	}
	if v, _ := xf.GetCellValue("Ban hang", "C2"); v != "SI-26-0001-01" {
		t.Fatalf("sales sheet row: %q", v)
	}
	if v, _ := xf.GetCellValue("Ban hang", "D2"); v != "KH0001" {
		t.Fatalf("customer MISA code: %q", v)
	}
	if r := e.do(mai, "POST", "/api/v1/misa/exports", map[string]any{"period": "2026-09"}, key("ex2")); r.reason() != "nothing_to_export" {
		t.Fatalf("exported documents are not exported again: %v", r.Body)
	}

	// Import a MISA customer list: codes are learnt by name.
	e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "KDH", "name": "Khí Đông Hải", "is_customer": true}, nil), "kdh")
	nf := excelize.NewFile()
	_ = nf.SetSheetRow("Sheet1", "A1", &[]any{"DANH SÁCH KHÁCH HÀNG"})
	_ = nf.SetSheetRow("Sheet1", "A3", &[]any{"Mã khách hàng", "Tên khách hàng", "Mã số thuế"})
	_ = nf.SetSheetRow("Sheet1", "A4", &[]any{"KH0002", "KHÍ ĐÔNG HẢI", ""})
	_ = nf.SetSheetRow("Sheet1", "A5", &[]any{"KH0099", "Không có trong Navo", ""})
	var xb bytes.Buffer
	_ = nf.Write(&xb)
	var mb bytes.Buffer
	mw := multipart.NewWriter(&mb)
	fw, _ := mw.CreateFormFile("file", "khach_hang.xlsx")
	_, _ = fw.Write(xb.Bytes())
	_ = mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/misa/import", &mb)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+mai)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var imp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&imp)
	resp.Body.Close()
	d := imp["data"].(map[string]any)
	if d["kind"] != "customer" || d["matched"].(float64) != 1 || len(d["unmatched"].([]any)) != 1 {
		t.Fatalf("import: %v", imp)
	}
}
