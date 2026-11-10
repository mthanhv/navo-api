//go:build integration

package integration

import (
	"testing"
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
