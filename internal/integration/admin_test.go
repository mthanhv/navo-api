//go:build integration

package integration

import "testing"

// The day-to-day administration a live company needs: accounts, passwords,
// corrections to partners and products, and discarding mistaken drafts.
func TestAccountsAndCorrections(t *testing.T) {
	e := setup(t)
	e.user("nam@test.vn", []string{"admin"})
	e.user("khoa@test.vn", []string{"sales"})
	admin, sales := e.login("nam@test.vn"), e.login("khoa@test.vn")
	sales2 := e.login("khoa@test.vn")

	list := e.must(200, e.do(admin, "GET", "/api/v1/users", nil, nil), "users")["items"].([]any)
	ids := map[string]string{}
	for _, it := range list {
		u := it.(map[string]any)
		ids[u["email"].(string)] = u["id"].(string)
	}
	adminID, salesID := ids["nam@test.vn"], ids["khoa@test.vn"]

	// Admin cannot lock themselves out.
	if r := e.do(admin, "PATCH", "/api/v1/users/"+adminID, map[string]any{"full_name": "Nam", "roles": []string{"sales"}, "is_active": true}, nil); r.reason() != "cannot_lock_out_self" {
		t.Fatalf("self demotion must be refused: %v", r.Body)
	}
	// Admin cannot hand out purchase-cost access they do not hold.
	if r := e.do(admin, "PATCH", "/api/v1/users/"+salesID, map[string]any{"full_name": "Khoa", "roles": []string{"sales"}, "grants": []string{"purchase_cost.read"}, "is_active": true}, nil); r.Status != 403 {
		t.Fatalf("grant escalation must be refused: %d %v", r.Status, r.Body)
	}
	// Change own password: wrong current is a field error; success keeps this session and ends the others.
	if r := e.do(sales, "POST", "/api/v1/me/password", map[string]any{"current_password": "nope-nope-nope", "new_password": "new-horse-battery-2"}, nil); r.Status != 400 {
		t.Fatalf("wrong current password: %d %v", r.Status, r.Body)
	}
	e.must(200, e.do(sales, "POST", "/api/v1/me/password", map[string]any{"current_password": "correct-horse-1", "new_password": "new-horse-battery-2"}, nil), "change password")
	e.must(200, e.do(sales, "GET", "/api/v1/me", nil, nil), "this session stays")
	if r := e.do(sales2, "GET", "/api/v1/me", nil, nil); r.Status != 401 {
		t.Fatalf("other sessions end after a password change: %d", r.Status)
	}
	if r := e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": "khoa@test.vn", "password": "new-horse-battery-2", "client": "mobile"}, nil); r.Status != 200 {
		t.Fatalf("sign in with the new password: %v", r.Body)
	}
	// Admin reset ends every session; the temporary password works.
	e.must(200, e.do(admin, "POST", "/api/v1/users/"+salesID+"/password", map[string]any{"password": "temporary-pass-9"}, nil), "reset")
	if r := e.do(sales, "GET", "/api/v1/me", nil, nil); r.Status != 401 {
		t.Fatalf("reset ends sessions: %d", r.Status)
	}
	r := e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": "khoa@test.vn", "password": "temporary-pass-9", "client": "mobile"}, nil)
	sales = r.data()["token"].(string)
	// Deactivation ends sessions and blocks sign-in; reactivation restores it.
	upd := e.must(200, e.do(admin, "PATCH", "/api/v1/users/"+salesID, map[string]any{"full_name": "Khoa Nguyễn", "roles": []string{"sales", "leader"}, "is_active": false}, nil), "deactivate")
	if upd["full_name"] != "Khoa Nguyễn" || upd["is_active"] != false {
		t.Fatalf("update: %v", upd)
	}
	if r := e.do(sales, "GET", "/api/v1/me", nil, nil); r.Status != 401 {
		t.Fatalf("deactivated user is signed out: %d", r.Status)
	}
	if r := e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": "khoa@test.vn", "password": "temporary-pass-9", "client": "mobile"}, nil); r.reason() != "invalid_credentials" {
		t.Fatalf("deactivated user cannot sign in: %v", r.Body)
	}
	e.must(200, e.do(admin, "PATCH", "/api/v1/users/"+salesID, map[string]any{"full_name": "Khoa Nguyễn", "roles": []string{"sales"}, "is_active": true}, nil), "reactivate")
	sales = e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": "khoa@test.vn", "password": "temporary-pass-9", "client": "mobile"}, nil).data()["token"].(string)

	// Partner and product corrections.
	cust := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Song Tra", "is_customer": true}, nil), "customer")
	sup := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "DGA", "name": "Đông Á", "is_supplier": true}, nil), "supplier")
	prod := e.must(201, e.do(sales, "POST", "/api/v1/products", map[string]any{"name": "Van bi", "unit": "bo"}, nil), "product")
	fixed := e.must(200, e.do(sales, "PATCH", "/api/v1/organizations/"+cust["id"].(string), map[string]any{"code": "STRA", "name": "Lọc hóa dầu Sông Trà", "is_customer": true, "tax_code": "0101234567"}, nil), "fix customer")
	if fixed["name"] != "Lọc hóa dầu Sông Trà" {
		t.Fatalf("customer fix: %v", fixed)
	}
	if e.must(200, e.do(sales, "PATCH", "/api/v1/products/"+prod["id"].(string), map[string]any{"name": "Van bi DN50", "unit": "bộ"}, nil), "fix product")["unit"] != "bộ" {
		t.Fatal("product fix")
	}

	// A draft contract is deleted; afterwards the customer is in use and keeps its role.
	draft := e.must(201, e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": "Nhầm", "currency": "VND",
		"lines": []any{map[string]any{"product_id": prod["id"], "quantity": "1", "unit_price": "1"}}}, nil), "draft")
	if r := e.do(sales, "PATCH", "/api/v1/organizations/"+cust["id"].(string), map[string]any{"code": "STRA", "name": "x", "is_supplier": true}, nil); r.Status != 400 {
		t.Fatalf("a customer with contracts keeps the customer role: %d %v", r.Status, r.Body)
	}
	e.must(200, e.do(sales, "DELETE", "/api/v1/contracts/"+draft["id"].(string), nil, key("del")), "delete draft")
	if r := e.do(sales, "GET", "/api/v1/contracts/"+draft["id"].(string), nil, nil); r.Status != 404 {
		t.Fatalf("deleted draft is gone: %d", r.Status)
	}

	// Active contract: a mistaken draft PO (with an open request) and delivery are cancelled.
	k := e.must(201, e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": "Thật", "currency": "VND",
		"lines": []any{map[string]any{"product_id": prod["id"], "quantity": "5", "unit_price": "1"}}}, nil), "contract")
	kid := k["id"].(string)
	k = e.must(200, e.do(sales, "POST", "/api/v1/contracts/"+kid+"/activate", map[string]any{"signed_on": "2026-09-01"}, key("act")), "activate")
	if r := e.do(sales, "DELETE", "/api/v1/contracts/"+kid, nil, key("del2")); r.reason() != "document_not_draft" {
		t.Fatalf("an active contract cannot be deleted: %v", r.Body)
	}
	line := lineByNo(k, 1)["contract_line_id"]
	po := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+kid+"/purchase-orders", map[string]any{"supplier_id": sup["id"], "currency": "VND",
		"lines": []any{map[string]any{"contract_line_id": line, "quantity": "9", "unit_price": "1"}}}, nil), "po")
	poID := po["id"].(string)
	e.must(201, e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/override-request", map[string]any{"reason": "nhầm"}, nil), "request")
	if c := e.must(200, e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/cancel", nil, key("cpo")), "cancel po"); c["status"] != "cancelled" {
		t.Fatalf("cancel: %v", c)
	}
	if q := e.must(200, e.do(admin, "GET", "/api/v1/override-requests", nil, nil), "queue"); len(q["items"].([]any)) != 0 {
		t.Fatalf("a cancelled order leaves the approval queue: %v", q)
	}
	if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/confirm", nil, key("cpo2")); r.reason() != "document_not_draft" {
		t.Fatalf("a cancelled order cannot be confirmed: %v", r.Body)
	}
	if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/cancel", nil, key("cpo3")); r.reason() != "document_not_draft" {
		t.Fatalf("cancel twice: %v", r.Body)
	}
	do := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+kid+"/delivery-orders", map[string]any{"planned_on": "2026-12-01", "firmness": "tentative",
		"lines": []any{map[string]any{"contract_line_id": line, "quantity": "1"}}}, nil), "do")
	e.must(200, e.do(sales, "POST", "/api/v1/delivery-orders/"+do["id"].(string)+"/cancel", nil, key("cdo")), "cancel do")
	l := lineByNo(e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+kid, nil, nil), "contract"), 1)
	if l["ordered"] != "0" {
		t.Fatalf("cancelled documents do not count: %v", l)
	}
	if e.must(200, e.do(sales, "GET", "/api/v1/delivery-orders", nil, nil), "planned")["pagination"].(map[string]any)["total"].(float64) != 0 {
		t.Fatal("a cancelled delivery is no longer planned")
	}
}
