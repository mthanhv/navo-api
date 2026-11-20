//go:build integration

package integration

import (
	"testing"
	"time"
)

// A quotation is drafted, sent, revised when changed after sending, won by creating a
// contract from it, given back when that draft contract is deleted, and lost with a reason.
// Other salespeople and delivery never see it.
func TestQuotations(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("tuan@test.vn", []string{"delivery"})
	khoa, ha, tuan := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("tuan@test.vn")
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "QTC", "name": "Sông Trà", "is_customer": true}, nil), "customer")["id"].(string)
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van bi", "unit": "cái"}, nil), "product")["id"].(string)
	valid := time.Now().AddDate(0, 0, 30).Format(time.DateOnly)
	body := map[string]any{"customer_id": cust, "title": "Van cho bảo dưỡng 2027", "currency": "VND", "valid_until": valid, "terms": "Giao 6 tuần",
		"lines": []any{map[string]any{"product_id": prod, "name": "Van bi DN50", "quantity": "10", "unit_price": "1000000", "tax_rate": "0.08", "lead_time": "6 tuần"}}}
	q := e.must(201, e.do(khoa, "POST", "/api/v1/quotations", body, nil), "create")
	id := q["id"].(string)
	if q["status"] != "draft" || q["revision"].(float64) != 1 || q["subtotal"] != "10000000" || q["tax"] != "800000" || q["total"] != "10800000" {
		t.Fatalf("draft quotation: %v", q)
	}
	if n := q["number"].(string); len(n) != 10 || n[:3] != "QT-" {
		t.Fatalf("number: %s", n)
	}
	if r := e.do(ha, "GET", "/api/v1/quotations/"+id, nil, nil); r.Status != 404 {
		t.Fatalf("another salesperson cannot see it: %d", r.Status)
	}
	if r := e.do(tuan, "GET", "/api/v1/quotations", nil, nil); r.Status != 403 {
		t.Fatalf("delivery has no quotations: %d", r.Status)
	}

	// Sent, then changed: revision 2, back to draft until sent again.
	q = e.must(200, e.do(khoa, "POST", "/api/v1/quotations/"+id+"/send", nil, key("send1")), "send")
	if q["status"] != "sent" || q["sent_on"] == nil {
		t.Fatalf("sent: %v", q)
	}
	if r := e.do(khoa, "DELETE", "/api/v1/quotations/"+id, nil, key("del")); r.reason() != "quotation_was_sent" {
		t.Fatalf("a sent quotation is kept: %v", r.Body)
	}
	body["version"] = q["version"]
	body["lines"] = []any{map[string]any{"product_id": prod, "name": "Van bi DN50", "quantity": "12", "unit_price": "950000"}}
	q = e.must(200, e.do(khoa, "PATCH", "/api/v1/quotations/"+id, body, nil), "revise")
	if q["revision"].(float64) != 2 || q["status"] != "draft" || q["total"] != "11400000" {
		t.Fatalf("revision: %v", q)
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/quotations/"+id+"/send", nil, key("send2")), "send again")

	// Won: the contract form is filled from it; creating the contract marks it won.
	in := e.must(200, e.do(khoa, "GET", "/api/v1/quotations/"+id+"/contract-input", nil, nil), "contract input")
	if in["customer_reference"] == nil || len(in["lines"].([]any)) != 1 {
		t.Fatalf("contract input: %v", in)
	}
	in["quotation_id"] = id
	k := e.must(201, e.do(khoa, "POST", "/api/v1/contracts", in, key("from-qt")), "contract from quotation")
	if k["quotation"].(map[string]any)["id"] != id {
		t.Fatalf("contract names its quotation: %v", k["quotation"])
	}
	q = e.must(200, e.do(khoa, "GET", "/api/v1/quotations/"+id, nil, nil), "won")
	if q["status"] != "won" || q["contract_id"] != k["id"] {
		t.Fatalf("won: %v", q)
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts", in, key("twice")); r.reason() != "quotation_closed" {
		t.Fatalf("a won quotation makes one contract: %v", r.Body)
	}

	// Deleting the draft contract gives the quotation back.
	e.must(200, e.do(khoa, "DELETE", "/api/v1/contracts/"+k["id"].(string), nil, key("del-k")), "delete draft contract")
	if q = e.must(200, e.do(khoa, "GET", "/api/v1/quotations/"+id, nil, nil), "open again"); q["status"] != "sent" || q["contract_id"] != nil {
		t.Fatalf("open again: %v", q)
	}

	// Lost needs a reason; reopening clears it.
	if r := e.do(khoa, "POST", "/api/v1/quotations/"+id+"/lost", map[string]any{}, key("lost0")); r.Status != 400 {
		t.Fatalf("lost needs a reason: %d", r.Status)
	}
	q = e.must(200, e.do(khoa, "POST", "/api/v1/quotations/"+id+"/lost", map[string]any{"reason": "Giá cao hơn đối thủ"}, key("lost")), "lost")
	if q["status"] != "lost" || q["lost_reason"] != "Giá cao hơn đối thủ" {
		t.Fatalf("lost: %v", q)
	}
	list := e.do(khoa, "GET", "/api/v1/quotations?tab=lost", nil, nil)
	if list.data()["counts"].(map[string]any)["lost"].(float64) != 1 {
		t.Fatalf("lost tab: %v", list.Body)
	}
	q = e.must(200, e.do(khoa, "POST", "/api/v1/quotations/"+id+"/reopen", nil, key("reopen")), "reopen")
	if q["status"] != "draft" || q["lost_reason"] != nil {
		t.Fatalf("reopen: %v", q)
	}

	// A draft never sent can be deleted.
	d := e.must(201, e.do(khoa, "POST", "/api/v1/quotations", map[string]any{"customer_id": cust, "title": "Nháp", "currency": "VND", "valid_until": valid,
		"lines": []any{map[string]any{"product_id": prod, "quantity": "1", "unit_price": "1"}}}, nil), "second")
	e.must(200, e.do(khoa, "DELETE", "/api/v1/quotations/"+d["id"].(string), nil, key("del2")), "delete unsent draft")
}

// The PIC (owner) defaults to the creator; only whole-company roles assign someone else, who then
// sees the quotation and owns the contract made from it.
func TestQuotationOwner(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("tuan@test.vn", []string{"delivery"})
	e.user("nam@test.vn", []string{"admin"})
	khoa, ha, nam := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("nam@test.vn")
	cust := e.must(201, e.do(nam, "POST", "/api/v1/organizations", map[string]any{"code": "QTO", "name": "Customer", "is_customer": true}, nil), "customer")["id"].(string)
	prod := e.must(201, e.do(nam, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "cái"}, nil), "product")["id"].(string)
	body := func(owner string) map[string]any {
		return map[string]any{"customer_id": cust, "title": "PIC", "currency": "VND", "valid_until": time.Now().AddDate(0, 0, 10).Format(time.DateOnly), "owner_id": owner,
			"lines": []any{map[string]any{"product_id": prod, "quantity": "1", "unit_price": "100"}}}
	}
	if r := e.do(ha, "POST", "/api/v1/quotations", body(e.userID(khoa)), nil); r.Status != 403 {
		t.Fatalf("a salesperson cannot give a quotation to someone else: %d", r.Status)
	}
	if r := e.do(nam, "POST", "/api/v1/quotations", body(e.userID(e.login("tuan@test.vn"))), nil); r.Status != 400 {
		t.Fatalf("the PIC must be sales or admin: %d", r.Status)
	}
	q := e.must(201, e.do(nam, "POST", "/api/v1/quotations", body(e.userID(khoa)), nil), "admin assigns khoa")
	if q["owner_name"] != "khoa@test.vn" {
		t.Fatalf("PIC: %v", q["owner_name"])
	}
	id := q["id"].(string)
	e.must(200, e.do(khoa, "GET", "/api/v1/quotations/"+id, nil, nil), "the PIC sees it")
	if r := e.do(ha, "GET", "/api/v1/quotations/"+id, nil, nil); r.Status != 404 {
		t.Fatalf("others do not: %d", r.Status)
	}
	in := e.must(200, e.do(khoa, "GET", "/api/v1/quotations/"+id+"/contract-input", nil, nil), "contract input")
	if in["owner_id"] != e.userID(khoa) {
		t.Fatalf("the contract goes to the PIC: %v", in["owner_id"])
	}
	in["quotation_id"] = id
	k := e.must(201, e.do(khoa, "POST", "/api/v1/contracts", in, key("pic")), "PIC makes the contract")
	if k["owner_id"] != e.userID(khoa) {
		t.Fatalf("contract owner: %v", k["owner_id"])
	}
	// Changing the PIC on an open quotation is also admin-only.
	q2 := e.must(201, e.do(khoa, "POST", "/api/v1/quotations", body(""), nil), "own quotation")
	b := body(e.userID(ha))
	b["version"] = q2["version"]
	if r := e.do(khoa, "PATCH", "/api/v1/quotations/"+q2["id"].(string), b, nil); r.Status != 403 {
		t.Fatalf("sales cannot hand a quotation over: %d", r.Status)
	}
	if q2 = e.must(200, e.do(nam, "PATCH", "/api/v1/quotations/"+q2["id"].(string), b, nil), "admin hands over"); q2["owner_name"] != "ha@test.vn" {
		t.Fatalf("handed over: %v", q2["owner_name"])
	}
}

// Files on a quotation: listed with the kinds allowed, copied into the contract's folder when
// it is won, and the copies go away again if that draft contract is deleted.
func TestQuotationFiles(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	khoa, ha := e.login("khoa@test.vn"), e.login("ha@test.vn")
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "QTF", "name": "Customer", "is_customer": true}, nil), "customer")["id"].(string)
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "cái"}, nil), "product")["id"].(string)
	q := e.must(201, e.do(khoa, "POST", "/api/v1/quotations", map[string]any{"customer_id": cust, "title": "Files", "currency": "VND", "valid_until": time.Now().AddDate(0, 0, 10).Format(time.DateOnly),
		"lines": []any{map[string]any{"product_id": prod, "quantity": "1", "unit_price": "100"}}}, nil), "quotation")
	id := q["id"].(string)
	kinds := e.must(200, e.do(khoa, "GET", "/api/v1/files?owner_type=quotation", nil, nil), "kinds")["kinds"].([]any)
	if len(kinds) == 0 || kinds[0] != "quotation" {
		t.Fatalf("quotation kinds: %v", kinds)
	}
	e.must(201, e.upload(khoa, "quotation", id, "customer_request", "rfq.pdf", pdf), "upload request")
	if r := e.upload(ha, "quotation", id, "customer_request", "x.pdf", pdf); r.Status != 404 {
		t.Fatalf("another salesperson cannot attach to it: %d", r.Status)
	}
	list := e.must(200, e.do(khoa, "GET", "/api/v1/files?owner_type=quotation&owner_id="+id, nil, nil), "list")["items"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["kind"] != "customer_request" {
		t.Fatalf("listed: %v", list)
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/quotations/"+id+"/send", nil, key("fs")), "send")
	in := e.must(200, e.do(khoa, "GET", "/api/v1/quotations/"+id+"/contract-input", nil, nil), "input")
	in["quotation_id"] = id
	k := e.must(201, e.do(khoa, "POST", "/api/v1/contracts", in, key("fk")), "contract")["id"].(string)
	cf := e.must(200, e.do(khoa, "GET", "/api/v1/files?owner_type=contract&owner_id="+k, nil, nil), "contract files")["items"].([]any)
	if len(cf) != 1 || cf[0].(map[string]any)["kind"] != "customer_request" {
		t.Fatalf("the quotation's file is in the contract folder: %v", cf)
	}
	e.must(200, e.do(khoa, "DELETE", "/api/v1/contracts/"+k, nil, key("fdel")), "delete draft contract")
	if l := e.must(200, e.do(khoa, "GET", "/api/v1/files?owner_type=quotation&owner_id="+id, nil, nil), "kept")["items"].([]any); len(l) != 1 {
		t.Fatalf("the quotation keeps its file: %v", l)
	}
}
