//go:build integration

package integration

import (
	"net/url"
	"testing"
)

// Codes are typed or generated: partners from the name's initials (numbered when taken),
// products as SP-00001…; a typed code that is taken is refused at the field.
func TestCodes(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	k := e.login("khoa@test.vn")
	if c := e.must(200, e.do(k, "GET", "/api/v1/organizations/suggest-code?name="+url.QueryEscape("Công ty TNHH Lọc hóa dầu Sông Trà")+"&role=customer", nil, nil), "suggest")["code"]; c != "LHDST" {
		t.Fatalf("initials: %v", c)
	}
	a := e.must(201, e.do(k, "POST", "/api/v1/organizations", map[string]any{"code": "", "name": "Lọc hóa dầu Sông Trà", "is_customer": true}, nil), "generated")
	b := e.must(201, e.do(k, "POST", "/api/v1/organizations", map[string]any{"name": "Lọc Hóa Dầu Sơn Tây", "is_customer": true}, nil), "generated again")
	if a["code"] != "LHDST" || b["code"] != "LHDST2" {
		t.Fatalf("generated codes: %v %v", a["code"], b["code"])
	}
	if c := e.must(201, e.do(k, "POST", "/api/v1/organizations", map[string]any{"name": "Petrolimex", "is_supplier": true}, nil), "one word")["code"]; c != "PETROLIM" {
		t.Fatalf("one-word name: %v", c)
	}
	p1 := e.must(201, e.do(k, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "cái"}, nil), "auto product")
	p2 := e.must(201, e.do(k, "POST", "/api/v1/products", map[string]any{"name": "Bơm", "unit": "bộ", "code": "bom-01"}, nil), "typed product")
	if p1["code"] != "SP-00001" || p2["code"] != "BOM-01" {
		t.Fatalf("product codes: %v %v", p1["code"], p2["code"])
	}
	if r := e.do(k, "POST", "/api/v1/products", map[string]any{"name": "Bơm 2", "unit": "bộ", "code": "BOM-01"}, nil); r.Status != 400 {
		t.Fatalf("a taken code is refused: %d %v", r.Status, r.Body)
	}
	if c := e.must(200, e.do(k, "GET", "/api/v1/products/suggest-code", nil, nil), "next")["code"]; c != "SP-00002" {
		t.Fatalf("next product code: %v", c)
	}
	if l := e.do(k, "GET", "/api/v1/products?q=bom-01", nil, nil).data()["items"].([]any); len(l) != 1 {
		t.Fatalf("search by code: %v", l)
	}
}
