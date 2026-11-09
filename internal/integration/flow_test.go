//go:build integration

// Package integration exercises the HTTP API against a real PostgreSQL
// database. It needs TEST_DATABASE_URL pointing at a disposable database whose
// name contains "test"; the public schema is dropped and rebuilt.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/db/migrations"
	"navo-api/internal/config"
	"navo-api/internal/db"
	"navo-api/internal/router"
	"navo-api/internal/users"
)

type env struct {
	t    *testing.T
	srv  *httptest.Server
	pool *pgxpool.Pool
}

type result struct {
	Status int
	Header http.Header
	Body   map[string]any
}

func (r result) data() map[string]any { m, _ := r.Body["data"].(map[string]any); return m }
func (r result) reason() string       { s, _ := r.Body["reason"].(string); return s }

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil || !strings.Contains(name, "test") {
		t.Fatalf("refusing to reset database %q: its name must contain \"test\"", name)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	pool.Reset() // drop cached statement plans from the old schema
	if _, err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	srv := httptest.NewServer(router.New(config.Config{SessionTTL: time.Hour, Timezone: loc, FilesDir: t.TempDir()}, pool))
	t.Cleanup(func() { srv.Close(); pool.Close() })
	return &env{t: t, srv: srv, pool: pool}
}

func (e *env) user(email string, roles []string, grants ...string) {
	e.t.Helper()
	err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
		_, err := users.Create(context.Background(), tx, users.Input{Email: email, FullName: email, Password: "correct-horse-1", Roles: roles, Grants: grants}, nil)
		return err
	})
	if err != nil {
		e.t.Fatalf("create user %s: %v", email, err)
	}
}

// login returns a Bearer token (mobile client) for the user.
func (e *env) login(email string) string {
	e.t.Helper()
	r := e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": email, "password": "correct-horse-1", "client": "mobile"}, nil)
	if r.Status != 200 {
		e.t.Fatalf("login %s: %d %v", email, r.Status, r.Body)
	}
	return r.data()["token"].(string)
}

func (e *env) do(token, method, path string, body any, headers map[string]string) result {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return result{Status: resp.StatusCode, Header: resp.Header, Body: out}
}

func (e *env) must(status int, r result, what string) map[string]any {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("%s: got %d, want %d: %v", what, r.Status, status, r.Body)
	}
	return r.data()
}

func key(s string) map[string]string { return map[string]string{"Idempotency-Key": "test-key-" + s} }

func lineByNo(contract map[string]any, no int) map[string]any {
	for _, l := range contract["lines"].([]any) {
		m := l.(map[string]any)
		if int(m["line_no"].(float64)) == no {
			return m
		}
	}
	return nil
}

func flagCodes(line map[string]any) []string {
	var out []string
	for _, f := range line["flags"].([]any) {
		out = append(out, f.(map[string]any)["code"].(string))
	}
	return out
}

func TestContractToDeliveryFlow(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	e.user("mai@test.vn", []string{"accounting"})
	e.user("nam@test.vn", []string{"admin"})
	sales, other, leader, acct, admin := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("lan@test.vn"), e.login("mai@test.vn"), e.login("nam@test.vn")

	customer := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Lọc hóa dầu Sông Trà", "is_customer": true}, nil), "customer")
	supplier := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "DGA", "name": "Thiết bị Đông Á", "is_supplier": true}, nil), "supplier")
	product := e.must(201, e.do(sales, "POST", "/api/v1/products", map[string]any{"name": "Transmitter áp suất 0–40 bar", "unit": "chiếc", "manufacturer_code": "PT-4020-A"}, nil), "product")

	// Validation: every bad field is reported at once, with JSON paths; numbers must be decimal strings.
	bad := e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": customer["id"], "title": "", "currency": "vnd1",
		"lines": []any{map[string]any{"product_id": product["id"], "quantity": "-1", "unit_price": "1e5"}}}, nil)
	if bad.Status != 400 || len(bad.Body["errors"].([]any)) != 4 {
		t.Fatalf("expected 4 field errors, got %d %v", bad.Status, bad.Body)
	}
	if e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": customer["id"], "title": "x", "currency": "VND", "sneaky": 1,
		"lines": []any{map[string]any{"product_id": product["id"], "quantity": "1", "unit_price": "1"}}}, nil).Status != 400 {
		t.Fatal("unknown fields must be refused")
	}

	// AC-02: the same product on two lines with different prices stays two lines; the customer's wording is kept.
	contract := e.must(201, e.do(sales, "POST", "/api/v1/contracts", map[string]any{
		"customer_id": customer["id"], "title": "Thiết bị đo BDTT 2027", "currency": "VND", "customer_reference": "PR-5520/2026",
		"lines": []any{
			map[string]any{"product_id": product["id"], "name_on_contract": "Bộ chuyển đổi áp suất 0-40bar, HART", "quantity": "100", "unit_price": "24000000", "due_on": "2027-06-30"},
			map[string]any{"product_id": product["id"], "quantity": "16", "unit_price": "25500000.50", "due_on": "2027-06-30"},
		}}, nil), "create contract")
	contractID := contract["id"].(string)
	if contract["status"] != "draft" || contract["number"] != nil || !strings.HasPrefix(contract["draft_number"].(string), "DRAFT-") {
		t.Fatalf("new contract should be an unnumbered draft: %v", contract)
	}
	if l := lineByNo(contract, 1); l["name_on_contract"] != "Bộ chuyển đổi áp suất 0-40bar, HART" || lineByNo(contract, 2)["name_on_contract"] != "Transmitter áp suất 0–40 bar" {
		t.Fatalf("name snapshots wrong: %v", contract["lines"])
	}
	if r := e.do(sales, "POST", "/api/v1/contracts/"+contractID+"/purchase-orders", map[string]any{"supplier_id": supplier["id"], "currency": "USD",
		"lines": []any{map[string]any{"contract_line_id": lineByNo(contract, 1)["contract_line_id"], "quantity": "1", "unit_price": "1"}}}, nil); r.reason() != "contract_not_active" {
		t.Fatalf("PO on a draft contract must be refused: %v", r.Body)
	}

	contract = e.must(200, e.do(sales, "POST", "/api/v1/contracts/"+contractID+"/activate", map[string]any{"signed_on": "2026-08-12"}, key("activate")), "activate")
	if contract["number"] != "SC-26-0001" {
		t.Fatalf("contract number = %v, want SC-26-0001", contract["number"])
	}
	line1 := lineByNo(contract, 1)["contract_line_id"].(string)

	newPO := func(qty string, lineID string) map[string]any {
		return e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+contractID+"/purchase-orders", map[string]any{"supplier_id": supplier["id"], "currency": "USD",
			"lines": []any{map[string]any{"contract_line_id": lineID, "quantity": qty, "unit_price": "980.50", "supplier_item_name": "Pressure transmitter PT-4020-A", "expected_on": "2026-09-20"}}}, nil), "create PO "+qty)
	}
	po1, po2 := newPO("60", line1), newPO("40", line1)
	if po1["number"] != "PO-26-0001-01" || po2["number"] != "PO-26-0001-02" {
		t.Fatalf("PO numbers %v %v", po1["number"], po2["number"])
	}
	e.must(200, e.do(sales, "POST", "/api/v1/purchase-orders/"+po1["id"].(string)+"/confirm", nil, key("po1")), "confirm PO1")
	e.must(200, e.do(sales, "POST", "/api/v1/purchase-orders/"+po2["id"].(string)+"/confirm", nil, key("po2")), "confirm PO2")

	// AC-03 / BR-05: a third PO for the same line would exceed 100 and is refused at confirmation.
	po3 := newPO("1", line1)
	if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+po3["id"].(string)+"/confirm", nil, key("po3")); r.Status != 409 || r.reason() != "quantity_exceeds_contract" {
		t.Fatalf("over-order must be refused: %d %v", r.Status, r.Body)
	}

	// AC-03: a line of another contract cannot be put on this contract's PO.
	otherContract := e.must(201, e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": customer["id"], "title": "Khác", "currency": "VND",
		"lines": []any{map[string]any{"product_id": product["id"], "quantity": "5", "unit_price": "1"}}}, nil), "other contract")
	foreignLine := lineByNo(otherContract, 1)["contract_line_id"]
	if r := e.do(sales, "POST", "/api/v1/contracts/"+contractID+"/purchase-orders", map[string]any{"supplier_id": supplier["id"], "currency": "USD",
		"lines": []any{map[string]any{"contract_line_id": foreignLine, "quantity": "1", "unit_price": "1"}}}, nil); r.Status != 400 {
		t.Fatalf("foreign contract line must be refused: %d %v", r.Status, r.Body)
	}

	// AC-05: rejected goods do not count as received.
	receive := func(po map[string]any, accepted, rejected, k string) {
		poLine := po["lines"].([]any)[0].(map[string]any)["id"]
		gr := e.must(201, e.do(sales, "POST", "/api/v1/purchase-orders/"+po["id"].(string)+"/goods-receipts", map[string]any{"received_on": "2026-09-25",
			"lines": []any{map[string]any{"purchase_order_line_id": poLine, "accepted_quantity": accepted, "rejected_quantity": rejected}}}, nil), "create GR")
		e.must(200, e.do(sales, "POST", "/api/v1/goods-receipts/"+gr["id"].(string)+"/confirm", nil, key(k)), "confirm GR")
	}
	receive(po1, "50", "0", "gr1")
	receive(po2, "20", "2", "gr2")

	// AC-04: a draft delivery does not count; confirming counts once even when retried.
	newDO := func(qty string) map[string]any {
		return e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+contractID+"/delivery-orders", map[string]any{
			"lines": []any{map[string]any{"contract_line_id": line1, "quantity": qty}}}, nil), "create DO "+qty)
	}
	do1 := newDO("20")
	report := func() map[string]any {
		return lineByNo(e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+contractID, nil, nil), "get contract"), 1)
	}
	if l := report(); l["delivered"] != "0" {
		t.Fatalf("draft delivery must not count: %v", l["delivered"])
	}
	confirm := map[string]any{"delivered_on": "2026-10-20"}
	e.must(200, e.do(sales, "POST", "/api/v1/delivery-orders/"+do1["id"].(string)+"/confirm", confirm, key("do1")), "confirm DO1")
	retry := e.do(sales, "POST", "/api/v1/delivery-orders/"+do1["id"].(string)+"/confirm", confirm, key("do1"))
	if retry.Status != 200 || retry.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry must replay the first result: %d %v", retry.Status, retry.Header)
	}
	if r := e.do(sales, "POST", "/api/v1/delivery-orders/"+do1["id"].(string)+"/confirm", map[string]any{"delivered_on": "2026-10-21"}, key("do1")); r.reason() != "idempotency_payload_mismatch" {
		t.Fatalf("same key, different payload must be refused: %v", r.Body)
	}
	do2 := newDO("30")
	e.must(200, e.do(sales, "POST", "/api/v1/delivery-orders/"+do2["id"].(string)+"/confirm", confirm, key("do2")), "confirm DO2")

	// AC-01: contract 100, ordered 60+40, received 70 accepted, delivered 50 → remaining 50, no plan.
	l := report()
	got := fmt.Sprintf("%v/%v/%v/%v/%v/%v", l["contract_quantity"], l["ordered"], l["received"], l["delivered"], l["remaining"], l["available"])
	if got != "100/100/70/50/50/20" {
		t.Fatalf("AC-01 quantities contract/ordered/received/delivered/remaining/available = %s", got)
	}
	// No plan for the remaining 50; PO-02 still owes 20 and its ETA (2026-09-20) has passed (BR-13).
	if codes := strings.Join(flagCodes(l), ","); codes != "no_plan,receipt_overdue" {
		t.Fatalf("AC-01 flags = %v, want no_plan,receipt_overdue", codes)
	}
	if l2 := lineByNo(e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+contractID, nil, nil), "contract"), 2); l2["ordered"] != "0" || l2["contract_quantity"] != "16" {
		t.Fatalf("line 2 must stay separate: %v", l2)
	}

	// Delivering more than has been received is refused.
	tooMuch := newDO("21")
	if r := e.do(sales, "POST", "/api/v1/delivery-orders/"+tooMuch["id"].(string)+"/confirm", confirm, key("too-much")); r.reason() != "insufficient_stock" {
		t.Fatalf("delivery beyond stock must be refused: %v", r.Body)
	}

	// AC-12: two confirmations racing for the last 20 units: exactly one wins.
	a, b := newDO("20"), newDO("20")
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, d := range []map[string]any{a, b} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			statuses[i] = e.do(sales, "POST", "/api/v1/delivery-orders/"+id+"/confirm", confirm, key(fmt.Sprintf("race-%d", i))).Status
		}(i, d["id"].(string))
	}
	wg.Wait()
	if !(statuses[0] == 200 && statuses[1] == 409 || statuses[0] == 409 && statuses[1] == 200) {
		t.Fatalf("race outcome %v, want one 200 and one 409", statuses)
	}
	if l := report(); l["delivered"] != "70" || l["available"] != "0" {
		t.Fatalf("after race delivered=%v available=%v, want 70 and 0", l["delivered"], l["available"])
	}

	// Overview and documents come from the server, in the caller's scope.
	ov := e.must(200, e.do(sales, "GET", "/api/v1/reports/overview", nil, nil), "overview")
	if ov["active_contracts"].(float64) != 1 || ov["needs_action"].(float64) < 1 || len(ov["attention"].([]any)) < 1 {
		t.Fatalf("overview: %v", ov)
	}
	if ov2 := e.must(200, e.do(other, "GET", "/api/v1/reports/overview", nil, nil), "other overview"); ov2["active_contracts"].(float64) != 0 {
		t.Fatalf("another salesperson's overview must be empty: %v", ov2)
	}
	docs := e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+contractID+"/documents", nil, nil), "documents")
	if len(docs["purchase_orders"].([]any)) != 3 || len(docs["goods_receipts"].([]any)) != 2 || len(docs["delivery_orders"].([]any)) != 5 {
		t.Fatalf("documents: %d POs, %d GRs, %d DOs", len(docs["purchase_orders"].([]any)), len(docs["goods_receipts"].([]any)), len(docs["delivery_orders"].([]any)))
	}
	if r := e.do(other, "GET", "/api/v1/contracts/"+contractID+"/documents", nil, nil); r.Status != 404 {
		t.Fatalf("documents outside scope must be 404: %d", r.Status)
	}
	if sum := e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+contractID, nil, nil), "summary")["summary"].(map[string]any); sum["lines"].(float64) != 2 || sum["fully_ordered"].(float64) != 1 {
		t.Fatalf("summary: %v", sum)
	}

	// AC-11 / AC-22: scope and permissions.
	if r := e.do(other, "GET", "/api/v1/contracts/"+contractID, nil, nil); r.Status != 404 {
		t.Fatalf("another salesperson must not see the contract: %d", r.Status)
	}
	if r := e.do(other, "GET", "/api/v1/reports/contract-lines", nil, nil); r.data()["pagination"].(map[string]any)["total"].(float64) != 0 {
		t.Fatalf("another salesperson's report must be empty: %v", r.Body)
	}
	e.must(200, e.do(leader, "GET", "/api/v1/contracts/"+contractID, nil, nil), "leader reads all")
	if r := e.do(leader, "POST", "/api/v1/contracts", map[string]any{}, nil); r.Status != 403 {
		t.Fatalf("leader must not create contracts by default: %d", r.Status)
	}
	if r := e.do(admin, "GET", "/api/v1/contracts", nil, nil); r.Status != 403 {
		t.Fatalf("admin must not read business data by default: %d", r.Status)
	}
	salesPO := e.must(200, e.do(sales, "GET", "/api/v1/purchase-orders/"+po1["id"].(string), nil, nil), "sales PO")
	if _, ok := salesPO["lines"].([]any)[0].(map[string]any)["unit_price"]; ok {
		t.Fatal("purchase price must be omitted without purchase_cost.read")
	}
	acctPO := e.must(200, e.do(acct, "GET", "/api/v1/purchase-orders/"+po1["id"].(string), nil, nil), "accounting PO")
	if acctPO["lines"].([]any)[0].(map[string]any)["unit_price"] != "980.5" {
		t.Fatalf("accounting should see the purchase price: %v", acctPO["lines"])
	}

	// Report filter: the line shows under "needs action" (no plan) and not under "completed".
	needs := e.must(200, e.do(leader, "GET", "/api/v1/reports/contract-lines?flag=needs_action", nil, nil), "report")
	if needs["pagination"].(map[string]any)["total"].(float64) < 1 {
		t.Fatalf("needs_action report empty: %v", needs)
	}

	// AC-13: confirmed documents cannot be hard-deleted, and confirmations are audited.
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM delivery_orders WHERE id = $1`, do1["id"]); err == nil || !strings.Contains(err.Error(), "document_not_draft") {
		t.Fatalf("deleting a confirmed DO must fail, got %v", err)
	}
	var audits int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE entity_type = 'delivery_order' AND action = 'confirm'`).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("delivery confirmations audited = %d (%v), want 3 (do1, do2, race winner)", audits, err)
	}
}

func TestCookieWritesNeedClientHeader(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	r := e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": "khoa@test.vn", "password": "correct-horse-1"}, nil)
	var cookie string
	for _, c := range strings.Split(r.Header.Get("Set-Cookie"), ";") {
		if strings.HasPrefix(c, "navo_session=") {
			cookie = c
		}
	}
	if cookie == "" || !strings.Contains(r.Header.Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("web login must set an HttpOnly cookie: %v", r.Header)
	}
	body := map[string]any{"code": "ABC", "name": "X", "is_customer": true}
	if r := e.do("", "POST", "/api/v1/organizations", body, map[string]string{"Cookie": cookie}); r.Status != 403 || r.reason() != "csrf_header_missing" {
		t.Fatalf("cookie write without client header must be refused: %d %v", r.Status, r.Body)
	}
	e.must(201, e.do("", "POST", "/api/v1/organizations", body, map[string]string{"Cookie": cookie, "X-Navo-Client": "web"}), "cookie write with header")
	if r := e.do("", "POST", "/api/v1/auth/login", map[string]any{"email": "khoa@test.vn", "password": "wrong-password"}, nil); r.reason() != "invalid_credentials" {
		t.Fatalf("wrong password: %v", r.Body)
	}
}

func TestOverrideApprovalAndLists(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("nam@test.vn", []string{"admin"})
	sales, other, admin := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("nam@test.vn")

	customer := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "STRA", "name": "Sông Trà", "is_customer": true}, nil), "customer")
	supplier := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "DGA", "name": "Đông Á", "is_supplier": true}, nil), "supplier")
	product := e.must(201, e.do(sales, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "bộ"}, nil), "product")
	contract := e.must(201, e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": customer["id"], "title": "X", "currency": "VND",
		"lines": []any{map[string]any{"product_id": product["id"], "quantity": "10", "unit_price": "1"}}}, nil), "contract")
	cid := contract["id"].(string)
	contract = e.must(200, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/activate", map[string]any{"signed_on": "2026-08-01"}, key("act")), "activate")
	line := lineByNo(contract, 1)["contract_line_id"]

	po := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/purchase-orders", map[string]any{"supplier_id": supplier["id"], "currency": "USD",
		"lines": []any{map[string]any{"contract_line_id": line, "quantity": "12", "unit_price": "5"}}}, nil), "po")
	poID := po["id"].(string)
	if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/confirm", nil, key("c1")); r.reason() != "quantity_exceeds_contract" {
		t.Fatalf("over-order must be refused first: %v", r.Body)
	}
	if r := e.do(other, "POST", "/api/v1/purchase-orders/"+poID+"/override-request", map[string]any{"reason": "x"}, nil); r.Status != 404 {
		t.Fatalf("another salesperson cannot request on this PO: %d", r.Status)
	}
	if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/override-request", map[string]any{"reason": " "}, nil); r.Status != 400 {
		t.Fatalf("a reason is required: %d", r.Status)
	}
	e.must(201, e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/override-request", map[string]any{"reason": "Khách xin thêm 2 bộ dự phòng"}, nil), "request")
	if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/override-request", map[string]any{"reason": "again"}, nil); r.reason() != "override_already_pending" {
		t.Fatalf("one open request per document: %v", r.Body)
	}
	if r := e.do(sales, "GET", "/api/v1/override-requests", nil, nil); r.Status != 403 {
		t.Fatalf("sales cannot see the approval queue: %d", r.Status)
	}
	if pending := e.must(200, e.do(sales, "GET", "/api/v1/purchase-orders/"+poID, nil, nil), "po")["override"].(map[string]any); pending["status"] != "pending" {
		t.Fatalf("the requester sees the pending request: %v", pending)
	}

	queue := e.must(200, e.do(admin, "GET", "/api/v1/override-requests", nil, nil), "queue")
	items := queue["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("queue: %v", queue)
	}
	req := items[0].(map[string]any)
	l0 := req["lines"].([]any)[0].(map[string]any)
	if l0["over"] != "2" || l0["requested"] != "12" {
		t.Fatalf("admin sees the excess: %v", l0)
	}
	if _, leaked := l0["unit_price"]; leaked {
		t.Fatal("admin must not see purchase prices")
	}
	rid := req["id"].(string)
	if r := e.do(admin, "POST", "/api/v1/override-requests/"+rid+"/reject", map[string]any{}, key("rej")); r.Status != 400 {
		t.Fatalf("rejecting needs a note: %d", r.Status)
	}
	approved := e.must(200, e.do(admin, "POST", "/api/v1/override-requests/"+rid+"/approve", map[string]any{"note": "OK, có phụ lục sau"}, key("app")), "approve")
	if approved["status"] != "approved" || approved["decided_by"] != "nam@test.vn" {
		t.Fatalf("approval: %v", approved)
	}
	if r := e.do(admin, "POST", "/api/v1/override-requests/"+rid+"/approve", map[string]any{}, key("app2")); r.reason() != "override_already_decided" {
		t.Fatalf("decided once: %v", r.Body)
	}
	if st := e.must(200, e.do(sales, "GET", "/api/v1/purchase-orders/"+poID, nil, nil), "po")["status"]; st != "confirmed" {
		t.Fatalf("approval confirms the PO: %v", st)
	}
	l := lineByNo(e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+cid, nil, nil), "contract"), 1)
	if l["ordered"] != "12" || !strings.Contains(strings.Join(flagCodes(l), ","), "over_ordered") {
		t.Fatalf("line after approved over-order: ordered=%v flags=%v", l["ordered"], flagCodes(l))
	}

	// Lists across contracts.
	awaiting := e.must(200, e.do(sales, "GET", "/api/v1/purchase-orders?view=awaiting", nil, nil), "po list")
	if awaiting["pagination"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("awaiting goods: %v", awaiting)
	}
	if e.must(200, e.do(other, "GET", "/api/v1/purchase-orders?view=all", nil, nil), "other list")["pagination"].(map[string]any)["total"].(float64) != 0 {
		t.Fatal("another salesperson's PO list must be empty")
	}

	// Over-delivery with approval; stock is still required.
	poLine := e.must(200, e.do(sales, "GET", "/api/v1/purchase-orders/"+poID, nil, nil), "po")["lines"].([]any)[0].(map[string]any)["id"]
	gr := e.must(201, e.do(sales, "POST", "/api/v1/purchase-orders/"+poID+"/goods-receipts", map[string]any{"received_on": "2026-09-01",
		"lines": []any{map[string]any{"purchase_order_line_id": poLine, "accepted_quantity": "12"}}}, nil), "gr")
	e.must(200, e.do(sales, "POST", "/api/v1/goods-receipts/"+gr["id"].(string)+"/confirm", nil, key("gr")), "confirm gr")
	newDO := func(q string) string {
		return e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/delivery-orders", map[string]any{"lines": []any{map[string]any{"contract_line_id": line, "quantity": q}}}, nil), "do")["id"].(string)
	}
	do1 := newDO("11")
	if r := e.do(sales, "POST", "/api/v1/delivery-orders/"+do1+"/confirm", map[string]any{"delivered_on": "2026-09-05"}, key("d1")); r.reason() != "delivery_exceeds_contract" {
		t.Fatalf("over-delivery refused first: %v", r.Body)
	}
	if r := e.do(sales, "POST", "/api/v1/delivery-orders/"+do1+"/override-request", map[string]any{"reason": "Hao hụt khi cắt"}, nil); r.Status != 400 {
		t.Fatalf("a delivery request needs the delivery date: %d", r.Status)
	}
	e.must(201, e.do(sales, "POST", "/api/v1/delivery-orders/"+do1+"/override-request", map[string]any{"reason": "Hao hụt khi cắt", "delivered_on": "2026-09-05"}, nil), "do request")
	q := e.must(200, e.do(admin, "GET", "/api/v1/override-requests", nil, nil), "queue")["items"].([]any)[0].(map[string]any)
	e.must(200, e.do(admin, "POST", "/api/v1/override-requests/"+q["id"].(string)+"/approve", map[string]any{}, key("d-app")), "approve do")
	do2 := newDO("5")
	e.must(201, e.do(sales, "POST", "/api/v1/delivery-orders/"+do2+"/override-request", map[string]any{"reason": "thêm", "delivered_on": "2026-09-06"}, nil), "do2 request")
	q2 := e.must(200, e.do(admin, "GET", "/api/v1/override-requests", nil, nil), "queue")["items"].([]any)[0].(map[string]any)
	if r := e.do(admin, "POST", "/api/v1/override-requests/"+q2["id"].(string)+"/approve", map[string]any{}, key("d-app2")); r.reason() != "insufficient_stock" {
		t.Fatalf("approval never delivers goods that were not received: %v", r.Body)
	}
	if left := e.must(200, e.do(admin, "GET", "/api/v1/override-requests", nil, nil), "queue")["items"].([]any); len(left) != 1 {
		t.Fatalf("a failed approval keeps the request pending: %d", len(left))
	}
	delivered := e.must(200, e.do(sales, "GET", "/api/v1/delivery-orders?view=delivered", nil, nil), "do list")
	if delivered["pagination"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("delivered list: %v", delivered)
	}
	if l := lineByNo(e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+cid, nil, nil), "contract"), 1); l["delivered"] != "11" || flagCodes(l)[0] != "over_delivered" {
		t.Fatalf("over-delivered line: %v %v", l["delivered"], flagCodes(l))
	}
}
