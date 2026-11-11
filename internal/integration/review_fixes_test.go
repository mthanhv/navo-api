//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"navo-api/internal/auth"
	"navo-api/internal/db"
)

// fixture is an active contract with one line, plus the partners and product behind it.
type fixture struct {
	customer, supplier, product, contract, line string
}

// activeContract creates customer, supplier and product as `token`, then a contract with
// one line of `qty` units at price 1, activated on 2026-08-01. Codes get `tag` so several
// fixtures can live in one database.
func (e *env) activeContract(token, tag, qty string) fixture {
	e.t.Helper()
	var f fixture
	f.customer = e.must(201, e.do(token, "POST", "/api/v1/organizations", map[string]any{"code": "C" + tag, "name": "Khách " + tag, "is_customer": true}, nil), "customer")["id"].(string)
	f.supplier = e.must(201, e.do(token, "POST", "/api/v1/organizations", map[string]any{"code": "S" + tag, "name": "NCC " + tag, "is_supplier": true}, nil), "supplier")["id"].(string)
	f.product = e.must(201, e.do(token, "POST", "/api/v1/products", map[string]any{"name": "Van " + tag, "unit": "bộ"}, nil), "product")["id"].(string)
	c := e.must(201, e.do(token, "POST", "/api/v1/contracts", map[string]any{"customer_id": f.customer, "title": "HĐ " + tag, "currency": "VND",
		"lines": []any{map[string]any{"product_id": f.product, "quantity": qty, "unit_price": "1"}}}, nil), "contract")
	f.contract = c["id"].(string)
	c = e.must(200, e.do(token, "POST", "/api/v1/contracts/"+f.contract+"/activate", map[string]any{"signed_on": "2026-08-01"}, key("act-"+tag)), "activate")
	f.line = lineByNo(c, 1)["contract_line_id"].(string)
	return f
}

// BR-05 is a four-eyes control: the person who asked for an excess cannot also allow it,
// even when they hold the approval permission (Admin holds every permission).
func TestOverrideNeedsAnotherApprover(t *testing.T) {
	e := setup(t)
	e.user("nam@test.vn", []string{"admin"})
	e.user("lan@test.vn", []string{"admin"})
	nam, lan := e.login("nam@test.vn"), e.login("lan@test.vn")
	f := e.activeContract(nam, "SELF", "10")
	po := e.must(201, e.do(nam, "POST", "/api/v1/contracts/"+f.contract+"/purchase-orders", map[string]any{"supplier_id": f.supplier, "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "12", "unit_price": "1"}}}, nil), "po")["id"].(string)
	e.must(201, e.do(nam, "POST", "/api/v1/purchase-orders/"+po+"/override-request", map[string]any{"reason": "dự phòng"}, nil), "request")
	queue := e.must(200, e.do(nam, "GET", "/api/v1/override-requests", nil, nil), "queue")["items"].([]any)
	req := queue[0].(map[string]any)
	if req["requested_by_id"] == nil || req["requested_by_id"] == "" {
		t.Fatalf("the queue says who asked, so the web can hide the buttons: %v", req)
	}
	rid := req["id"].(string)
	for _, action := range []string{"approve", "reject"} {
		if r := e.do(nam, "POST", "/api/v1/override-requests/"+rid+"/"+action, map[string]any{"note": "tự duyệt"}, key("self-"+action)); r.reason() != "override_self_decision" {
			t.Fatalf("%s of one's own request must be refused: %d %v", action, r.Status, r.Body)
		}
	}
	if st := e.must(200, e.do(nam, "GET", "/api/v1/purchase-orders/"+po, nil, nil), "po")["status"]; st != "draft" {
		t.Fatalf("a refused self-approval leaves the PO a draft: %v", st)
	}
	e.must(200, e.do(lan, "POST", "/api/v1/override-requests/"+rid+"/approve", map[string]any{"note": "OK"}, key("other-approve")), "another admin approves")
	if st := e.must(200, e.do(nam, "GET", "/api/v1/purchase-orders/"+po, nil, nil), "po")["status"]; st != "confirmed" {
		t.Fatalf("approval by another admin confirms the PO: %v", st)
	}
}

// Shared read, owner writes (owner decision 2026-11-11): everyone can look a customer up,
// but only its owner, owners of its open contracts and whole-company roles change or email
// its contacts. Suppliers stay shared.
func TestContactOwnership(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	e.user("mai@test.vn", []string{"accounting"})
	e.user("nam@test.vn", []string{"admin"})
	khoa, ha, lan, mai, nam := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("lan@test.vn"), e.login("mai@test.vn"), e.login("nam@test.vn")

	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Sông Trà", "is_customer": true}, nil), "customer")
	if cust["owner_id"] == nil || cust["can_manage_contacts"] != true {
		t.Fatalf("a salesperson creating a customer becomes its owner: %v", cust)
	}
	cid := cust["id"].(string)
	supp := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DGA", "name": "Đông Á", "is_supplier": true}, nil), "supplier")["id"].(string)
	if r := e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "X1", "name": "X", "is_customer": true, "owner_id": ""}, nil); r.Status != 403 {
		t.Fatalf("a salesperson cannot choose an owner: %d", r.Status)
	}
	huy := e.must(201, e.do(khoa, "POST", "/api/v1/organizations/"+cid+"/contacts", map[string]any{"full_name": "Trần Huy", "email": "huy@stra.vn"}, nil), "owner adds")["id"].(string)

	// Everyone reads; the list tells each caller whether they may manage.
	for who, tok := range map[string]string{"ha": ha, "lan": lan, "mai": mai} {
		if n := len(e.do(tok, "GET", "/api/v1/organizations/"+cid+"/contacts", nil, nil).Body["data"].([]any)); n != 1 {
			t.Fatalf("%s reads the contacts: %d", who, n)
		}
	}
	row := e.must(200, e.do(ha, "GET", "/api/v1/organizations?role=customer", nil, nil), "list")["items"].([]any)[0].(map[string]any)
	if row["can_manage_contacts"] != false || row["owner_name"] != "khoa@test.vn" {
		t.Fatalf("another salesperson sees the owner and cannot manage: %v", row)
	}

	// Another salesperson cannot change, delete or email them.
	if r := e.do(ha, "PATCH", "/api/v1/contacts/"+huy, map[string]any{"full_name": "Đổi tên"}, nil); r.reason() != "not_customer_owner" {
		t.Fatalf("edit by a non-owner: %d %v", r.Status, r.Body)
	}
	if r := e.do(ha, "DELETE", "/api/v1/contacts/"+huy, nil, nil); r.reason() != "not_customer_owner" {
		t.Fatalf("delete by a non-owner: %d %v", r.Status, r.Body)
	}
	if n := len(e.do(ha, "GET", "/api/v1/contacts/emailable", nil, nil).Body["data"].([]any)); n != 0 {
		t.Fatalf("a non-owner has no one at this customer to email: %d", n)
	}
	mail := map[string]any{"subject": "s", "body": "b", "contact_ids": []string{huy}}
	if r := e.do(ha, "POST", "/api/v1/mailings/preview", mail, nil); r.Status != 400 {
		t.Fatalf("a non-owner cannot address this contact even by id: %d %v", r.Status, r.Body)
	}
	if r := e.do(mai, "POST", "/api/v1/organizations/"+cid+"/contacts", map[string]any{"full_name": "X"}, nil); r.Status != 403 {
		t.Fatalf("accounting reads but does not write contacts: %d", r.Status)
	}

	// Leaders and admin manage any customer; suppliers are shared.
	e.must(200, e.do(lan, "PATCH", "/api/v1/contacts/"+huy, map[string]any{"full_name": "Trần Quốc Huy", "email": "huy@stra.vn"}, nil), "leader edits")
	e.must(201, e.do(nam, "POST", "/api/v1/organizations/"+cid+"/contacts", map[string]any{"full_name": "Lê Mai", "email": "mai@stra.vn"}, nil), "admin adds")
	e.must(201, e.do(ha, "POST", "/api/v1/organizations/"+supp+"/contacts", map[string]any{"full_name": "Mr Kim"}, nil), "supplier contacts are shared")

	// Owning an open contract for the customer is enough.
	product := e.must(201, e.do(ha, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "bộ"}, nil), "product")["id"].(string)
	e.must(201, e.do(ha, "POST", "/api/v1/contracts", map[string]any{"customer_id": cid, "title": "HĐ của Hà", "currency": "VND",
		"lines": []any{map[string]any{"product_id": product, "quantity": "1", "unit_price": "1"}}}, nil), "ha's draft")
	e.must(200, e.do(ha, "PATCH", "/api/v1/contacts/"+huy, map[string]any{"full_name": "Trần Quốc Huy", "email": "huy@stra.vn", "phone": "0903"}, nil), "contract owner edits")
	if n := len(e.do(ha, "GET", "/api/v1/contacts/emailable", nil, nil).Body["data"].([]any)); n != 2 {
		t.Fatalf("the contract owner can now email this customer's people: %d", n)
	}

	// Whole-company users assign the owner; the change is checked.
	if r := e.do(nam, "PATCH", "/api/v1/organizations/"+cid, map[string]any{"code": "STRA", "name": "Sông Trà", "is_customer": true, "owner_id": supp}, nil); r.Status != 400 {
		t.Fatalf("the owner must be an active salesperson or admin: %d", r.Status)
	}
	users := e.must(200, e.do(nam, "GET", "/api/v1/users?page_size=100", nil, nil), "users")["items"].([]any)
	var haID string
	for _, u := range users {
		if u.(map[string]any)["email"] == "ha@test.vn" {
			haID = u.(map[string]any)["id"].(string)
		}
	}
	moved := e.must(200, e.do(nam, "PATCH", "/api/v1/organizations/"+cid, map[string]any{"code": "STRA", "name": "Sông Trà", "is_customer": true, "owner_id": haID}, nil), "reassign")
	if moved["owner_id"] != haID {
		t.Fatalf("owner reassigned: %v", moved)
	}
	kept := e.must(200, e.do(ha, "PATCH", "/api/v1/organizations/"+cid, map[string]any{"code": "STRA", "name": "Sông Trà JSC", "is_customer": true}, nil), "sales edits name")
	if kept["owner_id"] != haID {
		t.Fatalf("an edit without owner_id keeps the owner: %v", kept)
	}
}

// A contract's history shows who did what to it and its documents, newest first, with the
// person's own reason, within scope and without the raw audit payload (which can hold prices).
func TestContractHistory(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	khoa, ha, lan := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("lan@test.vn")
	f := e.activeContract(khoa, "HIS", "10")
	po := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/purchase-orders", map[string]any{"supplier_id": f.supplier, "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "10", "unit_price": "777"}}}, nil), "po")["id"].(string)
	e.must(200, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po+"/confirm", nil, key("his-c")), "confirm")
	e.must(200, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po+"/close", map[string]any{"reason": "NCC hết hàng"}, key("his-close")), "close")

	r := e.do(lan, "GET", "/api/v1/contracts/"+f.contract+"/history", nil, nil)
	if r.Status != 200 {
		t.Fatalf("history: %d %v", r.Status, r.Body)
	}
	got := r.Body["data"].([]any)
	seen := map[string]bool{}
	for _, x := range got {
		m := x.(map[string]any)
		seen[m["action"].(string)+":"+m["entity_type"].(string)] = true
		if m["action"] == "close_outstanding" && (m["detail"] != "NCC hết hàng" || m["actor"] != "khoa@test.vn") {
			t.Fatalf("the close shows who and why: %v", m)
		}
		if _, leaked := m["changes"]; leaked {
			t.Fatal("the raw audit payload is never returned")
		}
	}
	for _, want := range []string{"create:contract", "activate:contract", "create:purchase_order", "confirm:purchase_order", "close_outstanding:purchase_order"} {
		if !seen[want] {
			t.Fatalf("history misses %s: %v", want, got)
		}
	}
	if first := got[0].(map[string]any); first["action"] != "close_outstanding" {
		t.Fatalf("newest first: %v", first)
	}
	if r := e.do(ha, "GET", "/api/v1/contracts/"+f.contract+"/history", nil, nil); r.Status != 404 {
		t.Fatalf("another salesperson cannot read this contract's history: %d", r.Status)
	}
}

// receiveAndDeliver orders, receives and delivers on the fixture's line.
func (e *env) receiveAndDeliver(token string, f fixture, tag, ordered, delivered string) (po, poLine string) {
	e.t.Helper()
	p := e.must(201, e.do(token, "POST", "/api/v1/contracts/"+f.contract+"/purchase-orders", map[string]any{"supplier_id": f.supplier, "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": ordered, "unit_price": "1"}}}, nil), "po")
	po, poLine = p["id"].(string), p["lines"].([]any)[0].(map[string]any)["id"].(string)
	e.must(200, e.do(token, "POST", "/api/v1/purchase-orders/"+po+"/confirm", nil, key(tag+"-po")), "po confirm")
	gr := e.must(201, e.do(token, "POST", "/api/v1/purchase-orders/"+po+"/goods-receipts", map[string]any{"received_on": "2026-10-01",
		"lines": []any{map[string]any{"purchase_order_line_id": poLine, "accepted_quantity": ordered}}}, nil), "gr")["id"].(string)
	e.must(200, e.do(token, "POST", "/api/v1/goods-receipts/"+gr+"/confirm", nil, key(tag+"-gr")), "gr confirm")
	if delivered != "0" {
		e.deliver(token, f, tag, delivered)
	}
	return po, poLine
}

func (e *env) deliver(token string, f fixture, tag, qty string) string {
	e.t.Helper()
	d := e.must(201, e.do(token, "POST", "/api/v1/contracts/"+f.contract+"/delivery-orders", map[string]any{"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": qty}}}, nil), "do")["id"].(string)
	e.must(200, e.do(token, "POST", "/api/v1/delivery-orders/"+d+"/confirm", map[string]any{"delivered_on": "2026-10-05"}, key(tag+"-do-"+qty)), "do confirm")
	return d
}

func (e *env) line(token string, f fixture) map[string]any {
	e.t.Helper()
	return lineByNo(e.must(200, e.do(token, "GET", "/api/v1/contracts/"+f.contract, nil, nil), "contract"), 1)
}

// Acceptance and returns keep net figures right (AC-05, AC-07, AC-10): accepted ≤ delivered;
// a customer return lowers delivered and puts goods back in stock; a supplier return lowers
// received; reversing either is refused when the goods have moved on. "After acceptance"
// money falls due only on full acceptance.
func TestAcceptanceAndReturns(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("mai@test.vn", []string{"accounting"})
	khoa, mai := e.login("khoa@test.vn"), e.login("mai@test.vn")
	f := e.activeContract(khoa, "ACC", "10")
	term := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/milestones", map[string]any{"name": "Sau nghiệm thu", "amount": "10", "trigger": "acceptance", "days_after": 0}, nil), "term")["id"].(string)
	po, poLine := e.receiveAndDeliver(khoa, f, "acc", "10", "6")
	pa := func(qty string) map[string]any {
		return map[string]any{"on": "2026-10-06", "evidence": "BB nghiệm thu", "reason": "Hàng lỗi", "lines": []any{map[string]any{"contract_line_id": f.line, "quantity": qty}}}
	}
	acc1 := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/acceptances", pa("4"), key("acc1")), "accept 4")
	if acc1["number"] != "AC-26-0001-01" {
		t.Fatalf("acceptance number: %v", acc1["number"])
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/acceptances", pa("3"), key("acc-over")); r.Status != 400 {
		t.Fatalf("only 2 delivered but not accepted: %d %v", r.Status, r.Body)
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/acceptances", map[string]any{"on": "2099-01-01", "evidence": "x", "lines": pa("1")["lines"]}, key("acc-future")); r.Status != 400 {
		t.Fatalf("a future date is refused: %d", r.Status)
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/customer-returns", pa("3"), key("cr-over")); r.Status != 400 {
		t.Fatalf("accepted goods cannot be returned without reversing acceptance: %d", r.Status)
	}
	cr := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/customer-returns", pa("2"), key("cr")), "return 2")
	l := e.line(khoa, f)
	if l["delivered"] != "4" || l["accepted"] != "4" || l["available"] != "6" || l["remaining"] != "6" {
		t.Fatalf("delivered 6 − returned 2 = 4; returned goods back in stock: %v", l)
	}
	if r := e.do(khoa, "POST", "/api/v1/milestones/"+term+"/trigger", map[string]any{"on": "2026-10-06", "note": "x"}, key("hand")); r.reason() != "acceptance_from_documents" {
		t.Fatalf("no hand-entered acceptance: %v", r.Body)
	}

	// Supplier return: at most what is in stock and not delivered (10 received − 4 delivered).
	sr := func(qty string) map[string]any {
		return map[string]any{"on": "2026-10-07", "reason": "Lỗi kỹ thuật", "lines": []any{map[string]any{"purchase_order_line_id": poLine, "quantity": qty}}}
	}
	if r := e.do(khoa, "POST", "/api/v1/purchase-orders/"+po+"/supplier-returns", sr("7"), key("sr-over")); r.Status != 400 {
		t.Fatalf("cannot return goods already delivered: %d %v", r.Status, r.Body)
	}
	srDoc := e.must(201, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po+"/supplier-returns", sr("5"), key("sr")), "supplier return 5")
	if l = e.line(khoa, f); l["received"] != "5" || l["available"] != "1" {
		t.Fatalf("received 10 − 5 returned: %v", l)
	}
	if po := e.must(200, e.do(khoa, "GET", "/api/v1/purchase-orders/"+po, nil, nil), "po")["lines"].([]any)[0].(map[string]any); po["received"] != "5" {
		t.Fatalf("the PO line expects 5 more after the return: %v", po)
	}
	reason := map[string]any{"reason": "Nhập nhầm"}
	if r := e.do(khoa, "POST", "/api/v1/customer-returns/"+cr["id"].(string)+"/reverse", reason, key("cr-rev-early")); r.reason() != "insufficient_stock" {
		t.Fatalf("the returned goods went back to the supplier, so the customer return stays: %v", r.Body)
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/supplier-returns/"+srDoc["id"].(string)+"/reverse", reason, key("sr-rev")), "reverse supplier return")
	e.must(200, e.do(khoa, "POST", "/api/v1/customer-returns/"+cr["id"].(string)+"/reverse", reason, key("cr-rev")), "reverse customer return")
	if l = e.line(khoa, f); l["delivered"] != "6" || l["received"] != "10" {
		t.Fatalf("both reversals restore the figures: %v", l)
	}

	// Full acceptance makes the term due; partial never does.
	terms := func() map[string]any {
		return e.must(200, e.do(mai, "GET", "/api/v1/receivables?tab=all", nil, nil), "receivables")["items"].([]any)[0].(map[string]any)
	}
	if terms()["status"] != "wait" {
		t.Fatalf("4 of 10 accepted: not due: %v", terms())
	}
	e.deliver(khoa, f, "acc-rest", "4")
	acc2 := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/acceptances", pa("6"), key("acc2")), "accept the rest")
	if m := terms(); m["status"] == "wait" || m["due_on"] != "2026-10-06" {
		t.Fatalf("fully accepted: the term falls due: %v", m)
	}
	if r := e.do(khoa, "POST", "/api/v1/delivery-orders/"+e.must(200, e.do(khoa, "GET", "/api/v1/contracts/"+f.contract+"/documents", nil, nil), "docs")["delivery_orders"].([]any)[0].(map[string]any)["id"].(string)+"/reverse", reason, key("do-rev")); r.reason() != "reversal_has_dependents" {
		t.Fatalf("accepted deliveries cannot be reversed: %v", r.Body)
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/acceptances/"+acc2["id"].(string)+"/reverse", reason, key("acc2-rev")), "reverse acceptance")
	if m := terms(); m["status"] != "wait" {
		t.Fatalf("no longer fully accepted: the term waits again: %v", m)
	}
	docs := e.must(200, e.do(khoa, "GET", "/api/v1/contracts/"+f.contract+"/documents", nil, nil), "docs")
	if len(docs["acceptances"].([]any)) != 2 || len(docs["customer_returns"].([]any)) != 1 || len(docs["supplier_returns"].([]any)) != 1 {
		t.Fatalf("every document is kept, reversed ones marked: %v", docs)
	}
}

// Amendments change an active contract with history (BR-06); close, cancel and reopen
// carry a reason; a closed contract takes no new orders but can still be accepted.
func TestAmendCloseCancel(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	khoa := e.login("khoa@test.vn")
	f := e.activeContract(khoa, "AMD", "10")
	e.receiveAndDeliver(khoa, f, "amd", "10", "6")
	version := e.must(200, e.do(khoa, "GET", "/api/v1/contracts/"+f.contract, nil, nil), "c")["version"]
	amend := func(v any, qty string) map[string]any {
		return map[string]any{"version": v, "signed_on": "2026-10-10", "reason": "Khách giảm số lượng", "reference": "PL-01",
			"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": qty, "unit_price": "1"}}}
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/amendments", amend(version, "5"), key("am-low")); r.Status != 400 {
		t.Fatalf("cannot go below the 6 delivered: %d %v", r.Status, r.Body)
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/amendments", amend(float64(0), "8"), key("am-stale")); r.reason() != "version_conflict" {
		t.Fatalf("stale version: %v", r.Body)
	}
	am := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/amendments", amend(version, "8"), key("am")), "amend to 8")
	ch := am["lines"].([]any)[0].(map[string]any)
	if am["number"] != "AM-26-0001-01" || ch["before"].(map[string]any)["quantity"] != "10" || ch["after"].(map[string]any)["quantity"] != "8" {
		t.Fatalf("the amendment keeps before and after: %v", am)
	}
	if l := e.line(khoa, f); l["contract_quantity"] != "8" || l["remaining"] != "2" || l["over"] != "0" {
		t.Fatalf("line follows the amendment: %v", l)
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/close", map[string]any{"reason": "Xong"}, key("close-early")); r.reason() != "contract_not_delivered" {
		t.Fatalf("2 still to deliver: %v", r.Body)
	}
	e.deliver(khoa, f, "amd-rest", "2")
	closed := e.must(200, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/close", map[string]any{"reason": "Đã giao đủ"}, key("close")), "close")
	if closed["status"] != "closed" {
		t.Fatalf("closed: %v", closed["status"])
	}
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/purchase-orders", map[string]any{"supplier_id": f.supplier, "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "1", "unit_price": "1"}}}, nil); r.reason() != "contract_not_active" {
		t.Fatalf("a closed contract takes no new orders: %v", r.Body)
	}
	e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/acceptances", map[string]any{"on": "2026-10-11", "evidence": "BB", "lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "8"}}}, key("acc-closed")), "accept after closing")
	ended := e.must(200, e.do(khoa, "GET", "/api/v1/contracts?tab=ended", nil, nil), "ended")
	if ended["counts"].(map[string]any)["ended"].(float64) != 1 || len(ended["items"].([]any)) != 1 {
		t.Fatalf("the ended tab lists it: %v", ended)
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/contracts/"+f.contract+"/reopen", map[string]any{"reason": "Khách đặt thêm"}, key("reopen")), "reopen")

	// Cancel: refused while goods are still on order; allowed once that balance is closed.
	g := e.activeContract(khoa, "CXL", "5")
	po := e.must(201, e.do(khoa, "POST", "/api/v1/contracts/"+g.contract+"/purchase-orders", map[string]any{"supplier_id": g.supplier, "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": g.line, "quantity": "5", "unit_price": "1"}}}, nil), "po")["id"].(string)
	e.must(200, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po+"/confirm", nil, key("cxl-po")), "confirm")
	if r := e.do(khoa, "POST", "/api/v1/contracts/"+g.contract+"/cancel", map[string]any{"reason": "Khách hủy"}, key("cxl-early")); r.reason() != "contract_cancel_blocked" {
		t.Fatalf("goods on order: %v", r.Body)
	}
	e.must(200, e.do(khoa, "POST", "/api/v1/purchase-orders/"+po+"/close", map[string]any{"reason": "Khách hủy"}, key("cxl-close-po")), "close PO balance")
	if c := e.must(200, e.do(khoa, "POST", "/api/v1/contracts/"+g.contract+"/cancel", map[string]any{"reason": "Khách hủy"}, key("cxl")), "cancel"); c["status"] != "cancelled" {
		t.Fatalf("cancelled: %v", c["status"])
	}
}

// Two-step sign-in: set up with an authenticator code, then every login needs a fresh code
// or a one-time recovery code; a code never works twice; another admin resets a lost phone.
func TestTwoStepSignIn(t *testing.T) {
	e := setup(t)
	e.user("nam@test.vn", []string{"admin"})
	e.user("lan@test.vn", []string{"admin"})
	nam, lan := e.login("nam@test.vn"), e.login("lan@test.vn")
	login := func(extra map[string]any) result {
		body := map[string]any{"email": "nam@test.vn", "password": "correct-horse-1", "client": "mobile"}
		for k, v := range extra {
			body[k] = v
		}
		return e.do("", "POST", "/api/v1/auth/login", body, nil)
	}
	set := e.must(200, e.do(nam, "POST", "/api/v1/me/mfa/setup", nil, nil), "setup")
	secret := set["secret"].(string)
	if !strings.HasPrefix(set["uri"].(string), "otpauth://totp/Navo:nam@test.vn?") {
		t.Fatalf("authenticator link: %v", set["uri"])
	}
	if r := login(nil); r.Status != 200 {
		t.Fatalf("nothing changes until a code is confirmed: %d", r.Status)
	}
	if r := e.do(nam, "POST", "/api/v1/me/mfa/enable", map[string]any{"code": "000000"}, nil); r.Status != 400 {
		t.Fatalf("a wrong code does not enable: %d", r.Status)
	}
	enableCode := auth.CodeAt(secret, time.Now())
	on := e.must(200, e.do(nam, "POST", "/api/v1/me/mfa/enable", map[string]any{"code": enableCode}, nil), "enable")
	codes := on["recovery_codes"].([]any)
	if len(codes) != 8 {
		t.Fatalf("eight recovery codes, shown once: %v", codes)
	}
	if me := e.must(200, e.do(nam, "GET", "/api/v1/me", nil, nil), "me")["user"].(map[string]any); me["mfa_enabled"] != true {
		t.Fatalf("me shows two-step is on: %v", me)
	}
	if r := login(nil); r.reason() != "mfa_required" {
		t.Fatalf("password alone no longer signs in: %v", r.Body)
	}
	// The exact code used to enable, whatever time it is now (a slow run may cross a step).
	if r := login(map[string]any{"code": enableCode}); r.reason() != "invalid_mfa_code" {
		t.Fatalf("the code used to enable cannot be replayed: %v", r.Body)
	}
	if r := login(map[string]any{"code": auth.CodeAt(secret, time.Now().Add(30*time.Second))}); r.Status != 200 {
		t.Fatalf("the next code signs in: %d %v", r.Status, r.Body)
	}
	if r := login(map[string]any{"recovery_code": codes[0]}); r.Status != 200 {
		t.Fatalf("a recovery code signs in: %d", r.Status)
	}
	if r := login(map[string]any{"recovery_code": codes[0]}); r.reason() != "invalid_mfa_code" {
		t.Fatalf("a recovery code works once: %v", r.Body)
	}
	if r := e.do(nam, "POST", "/api/v1/users/"+e.userID(nam)+"/mfa/reset", nil, nil); r.reason() != "mfa_reset_self" {
		t.Fatalf("nobody resets their own second step: %v", r.Body)
	}
	e.must(200, e.do(lan, "POST", "/api/v1/users/"+e.userID(nam)+"/mfa/reset", nil, nil), "another admin resets")
	if r := e.do(nam, "GET", "/api/v1/me", nil, nil); r.Status != 401 {
		t.Fatalf("a reset ends the person's sessions: %d", r.Status)
	}
	if r := login(nil); r.Status != 200 {
		t.Fatalf("after the reset the password signs in again: %d", r.Status)
	}
}

func (e *env) userID(token string) string {
	e.t.Helper()
	return e.must(200, e.do(token, "GET", "/api/v1/me", nil, nil), "me")["user"].(map[string]any)["id"].(string)
}

// Housekeeping removes long-finished sessions and old command results, never a live
// session or a person's most recent one.
func TestPurgeKeepsWhatMatters(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	live := e.login("khoa@test.vn")
	ctx := context.Background()
	old := time.Now().Add(-db.Retention - 24*time.Hour)
	uid := e.userID(live)
	for i, created := range []time.Time{old.Add(-2 * time.Hour), old.Add(-time.Hour)} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, client, created_at, expires_at) VALUES ($1, $2, 'web', $3, $4)`,
			uid, []byte{byte(i), 1, 2, 3}, created, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO idempotency_records (actor_id, scope, key, request_hash, created_at) VALUES ($1, 'x', 'old-key-1', '\x00', $2), ($1, 'x', 'new-key-1', '\x00', now())`, uid, old); err != nil {
		t.Fatal(err)
	}
	sessions, results, err := db.Purge(ctx, e.pool)
	if err != nil || sessions != 2 || results != 1 {
		t.Fatalf("purged %d sessions, %d results, err %v; want 2, 1", sessions, results, err)
	}
	if r := e.do(live, "GET", "/api/v1/me", nil, nil); r.Status != 200 {
		t.Fatalf("the live session survives: %d", r.Status)
	}
}

func (e *env) importFile(token, kind, step, name string, content []byte, hash string) result {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if hash != "" {
		_ = w.WriteField("file_hash", hash)
	}
	fw, _ := w.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = w.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/imports/"+kind+"/"+step, &buf)
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

// Starting data loads by preview then commit (AC-20): each row has an outcome and reason;
// near-matches are conflicts for a person, never merged; the same file twice creates nothing.
func TestMasterDataImport(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	khoa, lan := e.login("khoa@test.vn"), e.login("lan@test.vn")
	e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Lọc hóa dầu Sông Trà", "is_customer": true, "tax_code": "0101234567"}, nil), "existing")
	csv := []byte("Mã;Tên;Mã số thuế;Khách hàng;Nhà cung cấp;Người liên hệ;Email\n" +
		"STRA;Lọc hóa dầu Sông Trà;;x;;;\n" + // exists (no tax code given: matches by code)
		"DPN;Đạm Phương Nam;0309999999;x;;Trần Huy;huy@dpn.vn\n" + // new, with contact
		"STR2;Sông Trà khác mã;0101234567;x;;;\n" + // conflict: tax code used by STRA
		"KDH;Khí Đông Hải;;;;;\n" + // error: neither customer nor supplier
		"DPN;Trùng mã;;x;;;\n" + // error: duplicate code in file
		"DA;Thiết bị Đông Á;;;x;Mr Kim;not-an-email\n") // error: email
	if r := e.importFile(lan, "partners", "preview", "p.csv", csv, ""); r.Status != 403 {
		t.Fatalf("leaders do not import: %d", r.Status)
	}
	pv := e.importFile(khoa, "partners", "preview", "p.csv", csv, "")
	if pv.Status != 200 {
		t.Fatalf("preview: %d %v", pv.Status, pv.Body)
	}
	d := pv.data()
	c := d["counts"].(map[string]any)
	if c["new"].(float64) != 1 || c["exists"].(float64) != 1 || c["conflict"].(float64) != 1 || c["error"].(float64) != 3 {
		t.Fatalf("outcomes: %v rows %v", c, d["rows"])
	}
	var orgs int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM organizations`).Scan(&orgs)
	if orgs != 1 {
		t.Fatalf("preview saves nothing: %d", orgs)
	}
	if r := e.importFile(khoa, "partners", "commit", "p.csv", csv, "not-the-hash"); r.reason() != "import_file_changed" {
		t.Fatalf("commit only the previewed file: %v", r.Body)
	}
	hash := d["file_hash"].(string)
	done := e.must(200, e.importFile(khoa, "partners", "commit", "p.csv", csv, hash), "commit")
	if done["created"].(float64) != 1 {
		t.Fatalf("only the new row is created: %v", done)
	}
	dpn := e.must(200, e.do(khoa, "GET", "/api/v1/organizations?q=DPN", nil, nil), "dpn")["items"].([]any)[0].(map[string]any)
	if dpn["primary_contact"] != "Trần Huy" || dpn["owner_name"] != "khoa@test.vn" {
		t.Fatalf("contact and owner come with the import: %v", dpn)
	}
	again := e.must(200, e.importFile(khoa, "partners", "commit", "p.csv", csv, hash), "again")
	if again["created"].(float64) != 0 || again["counts"].(map[string]any)["exists"].(float64) != 2 {
		t.Fatalf("the same file twice creates nothing: %v", again)
	}

	// Products: a maker code identifies an item; same name with a code is a conflict.
	e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van cầu DN50", "unit": "bộ", "manufacturer": "KSB", "manufacturer_code": "BOA-50"}, nil), "product")
	pcsv := []byte("Tên sản phẩm,Đơn vị,Hãng,Mã hãng,Mã NCC\n" +
		"Van cầu DN50,bộ,KSB,BOA-50,\n" + // exists
		"Van cầu DN50,bộ,ARI,ARI-50,\n" + // conflict: same name, other maker
		"Gioăng DN100,cái,,,\n" + // new
		"Bơm,,,,\n" + // error: unit
		"Van bi,cái,,,NOPE\n") // error: unknown supplier
	pp := e.importFile(khoa, "products", "preview", "p.csv", pcsv, "").data()
	if c := pp["counts"].(map[string]any); c["new"].(float64) != 1 || c["exists"].(float64) != 1 || c["conflict"].(float64) != 1 || c["error"].(float64) != 2 {
		t.Fatalf("product outcomes: %v", pp["rows"])
	}
	if got := e.must(200, e.importFile(khoa, "products", "commit", "p.csv", pcsv, pp["file_hash"].(string)), "products"); got["created"].(float64) != 1 {
		t.Fatalf("one product created: %v", got)
	}
	if r := e.importFile(khoa, "products", "preview", "x.csv", []byte("a,b\n1,2\n"), ""); r.reason() != "import_header_missing" {
		t.Fatalf("a file without the columns is refused: %v", r.Body)
	}
}
