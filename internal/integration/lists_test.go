//go:build integration

package integration

import "testing"

// The list screens' tabs, filters, counts and the top-bar search (S-03, 9.1).
func TestListTabsFiltersAndSearch(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	khoa, ha, lan := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("lan@test.vn")

	stra := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Sông Trà", "is_customer": true}, nil), "c1")
	dpn := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DPN", "name": "Đạm Phương Nam", "is_customer": true}, nil), "c2")
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "bộ"}, nil), "p")
	mk := func(tok string, cust map[string]any, title, signed string, activate bool) string {
		k := e.must(201, e.do(tok, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": title, "currency": "VND", "delivery_location": "Kho A",
			"lines": []any{map[string]any{"product_id": prod["id"], "quantity": "10", "unit_price": "1000", "due_on": "2027-01-15"}}}, nil), title)
		id := k["id"].(string)
		if activate {
			e.must(200, e.do(tok, "POST", "/api/v1/contracts/"+id+"/activate", map[string]any{"signed_on": signed}, key(title)), "activate")
		}
		return id
	}
	k1 := mk(khoa, stra, "Khoa active", "2026-09-01", true)
	mk(khoa, dpn, "Khoa draft", "", false)
	mk(ha, dpn, "Ha active", "2026-09-02", true)

	got := e.must(200, e.do(khoa, "GET", "/api/v1/contracts/"+k1, nil, nil), "contract")
	if got["value"] != "10000" || got["final_due_on"] != "2027-01-15" || got["delivery_location"] != "Kho A" {
		t.Fatalf("contract header fields: value=%v due=%v loc=%v", got["value"], got["final_due_on"], got["delivery_location"])
	}

	list := e.must(200, e.do(khoa, "GET", "/api/v1/contracts?tab=all", nil, nil), "list")
	counts := list["counts"].(map[string]any)
	if counts["all"].(float64) != 1 || counts["draft"].(float64) != 1 || counts["action"].(float64) != 1 {
		t.Fatalf("sales counts: %v", counts)
	}
	item := list["items"].([]any)[0].(map[string]any)
	if item["progress"].(map[string]any)["lines"].(float64) != 1 || item["attention"].(map[string]any)["flag"].(map[string]any)["code"] != "no_plan" {
		t.Fatalf("list aggregates: %v", item)
	}
	if n := len(e.must(200, e.do(lan, "GET", "/api/v1/contracts?tab=all&customer_id="+dpn["id"].(string), nil, nil), "by customer")["items"].([]any)); n != 1 {
		t.Fatalf("customer filter: %d", n)
	}
	owners := e.must(200, e.do(lan, "GET", "/api/v1/contracts/owners", nil, nil), "owners")
	_ = owners
	lines := e.must(200, e.do(lan, "GET", "/api/v1/reports/contract-lines?tab=action", nil, nil), "lines")
	if lines["counts"].(map[string]any)["all"].(float64) != 2 || lines["pagination"].(map[string]any)["total"].(float64) != 2 {
		t.Fatalf("leader line tabs: %v", lines["counts"])
	}
	if e.must(200, e.do(lan, "GET", "/api/v1/reports/contract-lines?tab=soon", nil, nil), "soon")["pagination"].(map[string]any)["total"].(float64) != 0 {
		t.Fatal("nothing planned in 7 days")
	}

	s := e.must(200, e.do(ha, "GET", "/api/v1/search?q=active", nil, nil), "search")
	cs := s["contracts"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["detail"] != "Ha active · Đạm Phương Nam" {
		t.Fatalf("search is scoped to the caller's contracts: %v", cs)
	}
	if cust := e.must(200, e.do(ha, "GET", "/api/v1/search?q=trà", nil, nil), "search customers")["customers"].([]any); len(cust) != 1 {
		t.Fatalf("customer search: %v", cust)
	}
	if one := e.must(200, e.do(ha, "GET", "/api/v1/search?q=a", nil, nil), "short")["contracts"].([]any); len(one) != 0 {
		t.Fatal("one character searches nothing")
	}
}
