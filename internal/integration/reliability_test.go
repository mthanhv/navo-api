//go:build integration

package integration

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// One isolated fixture exercises the exact regressions found in the assessment.
func TestReliabilityRegressions(t *testing.T) {
	e := setup(t)
	e.user("review-sales@test.vn", []string{"sales"})
	e.user("review-other@test.vn", []string{"sales"})
	e.user("review-admin@test.vn", []string{"admin"})
	sales, other, admin := e.login("review-sales@test.vn"), e.login("review-other@test.vn"), e.login("review-admin@test.vn")
	today := time.Now().In(time.FixedZone("VN", 7*3600)).Format(time.DateOnly)
	cust := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "RVC", "name": "Review Customer", "is_customer": true}, nil), "customer")
	sup := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "RVS", "name": "Review Supplier", "is_supplier": true}, nil), "supplier")
	prod := e.must(201, e.do(sales, "POST", "/api/v1/products", map[string]any{"name": "Review Valve", "unit": "unit"}, nil), "product")
	contractBody := map[string]any{"customer_id": cust["id"], "title": "Review contract", "currency": "VND", "lines": []any{map[string]any{"product_id": prod["id"], "quantity": "10", "unit_price": "10"}}}
	contract := e.must(201, e.do(sales, "POST", "/api/v1/contracts", contractBody, key("create-review")), "contract")
	cid := contract["id"].(string)
	e.must(201, e.do(sales, "POST", "/api/v1/contracts", contractBody, key("create-review")), "create replay")
	contract = e.must(200, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/activate", map[string]any{"signed_on": today}, key("activate-review")), "activate")
	term := func(name string) map[string]any {
		return e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/milestones", map[string]any{"name": name, "amount": "100", "trigger": "signing", "days_after": 0}, nil), "term")
	}
	m1, m2 := term("One"), term("Two")
	receipt := e.must(201, e.do(admin, "POST", "/api/v1/receipts", map[string]any{"customer_id": cust["id"], "received_on": today, "amount": "100", "currency": "VND", "method": "bank"}, key("receipt-review")), "receipt")
	rid := receipt["id"].(string)
	t.Run("receipt allocation lock snapshot", func(t *testing.T) {
		results := raceAfterLock(t, e, `SELECT id FROM receipts WHERE id=$1 FOR UPDATE`, rid, "%FROM receipts WHERE id%", []func() result{
			func() result {
				return e.do(admin, "POST", "/api/v1/receipts/"+rid+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": m1["id"], "amount": "80"}}}, key("race-r1"))
			},
			func() result {
				return e.do(admin, "POST", "/api/v1/receipts/"+rid+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": m2["id"], "amount": "80"}}}, key("race-r2"))
			},
		})
		successes := 0
		for _, r := range results {
			if r.Status == 200 {
				successes++
			}
		}
		r := e.must(200, e.do(admin, "GET", "/api/v1/receipts/"+rid, nil, nil), "receipt after race")
		if successes != 1 || r["allocated"] != "80" || r["unallocated"] != "20" {
			t.Fatalf("race: successes=%d balances=%v", successes, r)
		}
	})
	pi := e.must(201, e.do(admin, "POST", "/api/v1/purchase-invoices", map[string]any{"supplier_id": sup["id"], "legal_number": "REV-PI", "issued_on": today, "currency": "VND", "amount_net": "100", "vat_amount": "0"}, key("review-pi")), "purchase invoice")
	t.Run("supplier payment lock snapshot", func(t *testing.T) {
		pay := func(k string) func() result {
			return func() result {
				return e.do(admin, "POST", "/api/v1/payments", map[string]any{"supplier_id": sup["id"], "paid_on": today, "currency": "VND", "amount": "80", "method": "bank", "purchase_invoice_id": pi["id"]}, key(k))
			}
		}
		rs := raceAfterLock(t, e, `SELECT id FROM purchase_invoices WHERE id=$1 FOR UPDATE`, pi["id"].(string), "%FROM purchase_invoices WHERE id%", []func() result{pay("race-p1"), pay("race-p2")})
		successes := 0
		for _, r := range rs {
			if r.Status == 201 {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("payment race: %v", rs)
		}
		list := e.do(admin, "GET", "/api/v1/invoices?kind=purchase", nil, nil).Body["data"].([]any)
		if list[0].(map[string]any)["paid"] != "80" {
			t.Fatalf("invoice overpaid: %v", list)
		}
	})
	t.Run("advance allocation and partner validation", func(t *testing.T) {
		bad := e.do(admin, "POST", "/api/v1/payments", map[string]any{"supplier_id": cust["id"], "paid_on": today, "amount": "10", "currency": "VND", "method": "bank"}, key("bad-supplier"))
		if bad.Status != 400 {
			t.Fatalf("invalid supplier: %v", bad)
		}
		advance := e.must(201, e.do(admin, "POST", "/api/v1/payments", map[string]any{"supplier_id": sup["id"], "paid_on": today, "amount": "30", "currency": "VND", "method": "bank"}, key("advance")), "advance")
		a := e.must(201, e.do(admin, "POST", "/api/v1/payments/"+advance["id"].(string)+"/allocations", map[string]any{"purchase_invoice_id": pi["id"], "amount": "20"}, key("advance-allocate")), "allocate advance")
		e.must(200, e.do(admin, "POST", "/api/v1/payment-allocations/"+a["id"].(string)+"/reverse", map[string]any{"reason": "Wrong invoice"}, key("advance-reverse")), "reverse allocation")
		e.must(200, e.do(admin, "POST", "/api/v1/payments/"+advance["id"].(string)+"/reverse", map[string]any{"reason": "Wrong cash entry"}, key("advance-cash-reverse")), "reverse advance")
	})
	t.Run("task scope and replay revocation", func(t *testing.T) {
		if r := e.do(other, "POST", "/api/v1/tasks", map[string]any{"title": "Guess contract", "contract_id": cid, "customer_id": cust["id"]}, nil); r.Status != 404 {
			t.Fatalf("task leaked: %v", r.Body)
		}
		users := e.must(200, e.do(admin, "GET", "/api/v1/users", nil, nil), "users")["items"].([]any)
		var uid string
		for _, u := range users {
			m := u.(map[string]any)
			if m["email"] == "review-sales@test.vn" {
				uid = m["id"].(string)
			}
		}
		grants := func(g []string) {
			e.must(200, e.do(admin, "PATCH", "/api/v1/users/"+uid, map[string]any{"full_name": "Sales", "roles": []string{"sales"}, "grants": g, "is_active": true}, nil), "grants")
		}
		grants([]string{"purchase_cost.read"})
		po := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/purchase-orders", map[string]any{"supplier_id": sup["id"], "currency": "VND", "lines": []any{map[string]any{"contract_line_id": lineByNo(contract, 1)["contract_line_id"], "quantity": "10", "unit_price": "9"}}}, key("po-create")), "po")
		pid := po["id"].(string)
		e.must(200, e.do(sales, "POST", "/api/v1/purchase-orders/"+pid+"/confirm", nil, key("po-replay")), "confirm")
		grants([]string{})
		if r := e.do(sales, "POST", "/api/v1/purchase-orders/"+pid+"/confirm", nil, key("po-replay")); r.Status != 409 || r.reason() != "idempotency_authorization_changed" {
			t.Fatalf("unsafe replay: %v", r.Body)
		}
		var raw string
		if err := e.pool.QueryRow(context.Background(), `SELECT response::text FROM idempotency_records WHERE scope='purchase_orders.confirm' AND key='test-key-po-replay'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if raw == "" {
			t.Fatal("original command outcome must remain recorded")
		}
	})
	t.Run("immutable export and split invoices", func(t *testing.T) {
		var invoice map[string]any
		for i, k := range []string{"split-1", "split-2"} {
			_ = i
			invoice = e.must(201, e.do(admin, "POST", "/api/v1/contracts/"+cid+"/sales-invoices", map[string]any{"legal_number": k, "issued_on": today, "currency": "VND", "description": "Part", "amount_net": "50", "vat_amount": "0", "milestone_id": m1["id"]}, key(k)), "invoice")
			if invoice["paid"] != nil {
				t.Fatalf("invoice must not inherit obligation cash: %v", invoice)
			}
		}
		e.must(200, e.do(admin, "PUT", "/api/v1/misa/codes", map[string]any{"kind": "organization", "id": cust["id"], "code": "KH-OLD"}, nil), "code")
		batch := e.must(201, e.do(admin, "POST", "/api/v1/misa/exports", map[string]any{"period": today[:7], "types": []string{"sales"}}, key("export")), "export")
		path := "/api/v1/misa/exports/" + batch["id"].(string) + "/file"
		status, _, before := e.raw(admin, path)
		if status != 200 {
			t.Fatal(status)
		}
		e.must(200, e.do(admin, "PUT", "/api/v1/misa/codes", map[string]any{"kind": "organization", "id": cust["id"], "code": "KH-NEW"}, nil), "code change")
		e.must(200, e.do(admin, "POST", "/api/v1/invoices/sales/"+invoice["id"].(string)+"/cancel", nil, key("invoice-cancel")), "cancel exported invoice")
		status, _, after := e.raw(admin, path)
		if status != 200 || !bytes.Equal(before, after) {
			t.Fatal("historical workbook changed")
		}
		var correction bool
		if err := e.pool.QueryRow(context.Background(), `SELECT correction_needed FROM misa_exports WHERE id=$1`, batch["id"]).Scan(&correction); err != nil || !correction {
			t.Fatalf("missing correction flag: %v", err)
		}
	})
	t.Run("atomic contract terms", func(t *testing.T) {
		bad := map[string]any{}
		for k, v := range contractBody {
			bad[k] = v
		}
		bad["milestones"] = []any{map[string]any{"name": "Bad", "amount": "0", "trigger": "signing", "days_after": 0}}
		if r := e.do(sales, "POST", "/api/v1/contracts", bad, key("bad-aggregate")); r.Status != 400 {
			t.Fatalf("invalid term: %v", r.Body)
		}
		var count int
		e.pool.QueryRow(context.Background(), `SELECT count(*) FROM contracts`).Scan(&count)
		if count != 1 {
			t.Fatalf("partial/duplicate contracts: %d", count)
		}
	})
}

func raceAfterLock(t *testing.T, e *env, query, id, waitPattern string, actions []func() result) []result {
	t.Helper()
	ctx := context.Background()
	blocker, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, query, id); err != nil {
		t.Fatal(err)
	}
	rs := make([]result, len(actions))
	var wg sync.WaitGroup
	for i, fn := range actions {
		wg.Add(1)
		go func(i int, fn func() result) { defer wg.Done(); rs[i] = fn() }(i, fn)
	}
	deadline := time.Now().Add(5 * time.Second)
	waiting := 0
	for time.Now().Before(deadline) {
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1`, waitPattern).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= len(actions) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if waiting < len(actions) {
		t.Fatalf("race barrier was not reached: %d", waiting)
	}
	return rs
}

func TestOperationalCorrections(t *testing.T) {
	e := setup(t)
	e.user("ops@test.vn", []string{"sales"})
	e.user("read@test.vn", []string{"leader"})
	sales, leader := e.login("ops@test.vn"), e.login("read@test.vn")
	day := time.Now().In(time.FixedZone("VN", 7*3600)).Format(time.DateOnly)
	cust := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "OPC", "name": "Customer", "is_customer": true}, nil), "customer")
	sup := e.must(201, e.do(sales, "POST", "/api/v1/organizations", map[string]any{"code": "OPS", "name": "Supplier", "is_supplier": true}, nil), "supplier")
	prod := e.must(201, e.do(sales, "POST", "/api/v1/products", map[string]any{"name": "Valve", "unit": "unit"}, nil), "product")
	k := e.must(201, e.do(sales, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": "Corrections", "currency": "VND", "lines": []any{map[string]any{"product_id": prod["id"], "quantity": "10", "unit_price": "10"}}}, nil), "contract")
	cid := k["id"].(string)
	edit := map[string]any{"version": k["version"], "customer_id": cust["id"], "title": "Corrected draft", "currency": "VND", "lines": []any{map[string]any{"product_id": prod["id"], "quantity": "10", "unit_price": "10"}}}
	k = e.must(200, e.do(sales, "PATCH", "/api/v1/contracts/"+cid, edit, key("edit")), "edit draft")
	if r := e.do(sales, "PATCH", "/api/v1/contracts/"+cid, edit, key("edit-stale")); r.reason() != "version_conflict" {
		t.Fatalf("stale edit accepted: %v", r.Body)
	}
	file := e.must(201, e.uploadClient(sales, "contract", cid, "signed_contract", "contract.pdf", pdf, "726195de-48b2-4dd2-89ae-67665e071b16"), "upload")
	retry := e.must(201, e.uploadClient(sales, "contract", cid, "signed_contract", "contract.pdf", pdf, "726195de-48b2-4dd2-89ae-67665e071b16"), "upload retry")
	if file["file_id"] != retry["file_id"] {
		t.Fatal("upload duplicated")
	}
	if r := e.upload(leader, "contract", cid, "minutes", "bad.pdf", pdf); r.Status != 403 {
		t.Fatalf("reader attached: %v", r.Body)
	}
	k = e.must(200, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/activate", map[string]any{"signed_on": day}, key("op-act")), "activate")
	m := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/milestones", map[string]any{"name": "Acceptance", "amount": "100", "trigger": "acceptance", "days_after": 0}, nil), "term")
	mid := m["id"].(string)
	event := map[string]any{"on": day, "note": "Signed customer acceptance reference AC-1"}
	if r := e.do(sales, "POST", "/api/v1/milestones/"+mid+"/trigger", event, key("before-delivery")); r.reason() != "acceptance_requires_delivery" {
		t.Fatalf("accepted before delivery: %v", r.Body)
	}
	line := lineByNo(k, 1)["contract_line_id"]
	po := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/purchase-orders", map[string]any{"supplier_id": sup["id"], "currency": "VND", "lines": []any{map[string]any{"contract_line_id": line, "quantity": "10", "unit_price": "9"}}}, nil), "PO")
	pid := po["id"].(string)
	e.must(200, e.do(sales, "POST", "/api/v1/purchase-orders/"+pid+"/confirm", nil, key("op-po")), "PO confirm")
	gr := e.must(201, e.do(sales, "POST", "/api/v1/purchase-orders/"+pid+"/goods-receipts", map[string]any{"received_on": day, "lines": []any{map[string]any{"purchase_order_line_id": po["lines"].([]any)[0].(map[string]any)["id"], "accepted_quantity": "10"}}}, nil), "GR")
	gid := gr["id"].(string)
	e.must(200, e.do(sales, "POST", "/api/v1/goods-receipts/"+gid+"/confirm", nil, key("op-gr")), "GR confirm")
	d := e.must(201, e.do(sales, "POST", "/api/v1/contracts/"+cid+"/delivery-orders", map[string]any{"lines": []any{map[string]any{"contract_line_id": line, "quantity": "10"}}}, nil), "DO")
	did := d["id"].(string)
	e.must(200, e.do(sales, "POST", "/api/v1/delivery-orders/"+did+"/confirm", map[string]any{"delivered_on": day}, key("op-do")), "DO confirm")
	reason := map[string]any{"reason": "Correct a mistaken entry"}
	if r := e.do(sales, "POST", "/api/v1/goods-receipts/"+gid+"/reverse", reason, key("gr-dependents")); r.reason() != "reversal_has_dependents" {
		t.Fatalf("negative stock reversal: %v", r.Body)
	}
	e.must(200, e.do(sales, "POST", "/api/v1/milestones/"+mid+"/trigger", event, key("op-trigger")), "acceptance")
	if r := e.do(sales, "POST", "/api/v1/milestones/"+mid+"/trigger", event, key("trigger-again")); r.reason() != "milestone_already_triggered" {
		t.Fatalf("event overwritten: %v", r.Body)
	}
	if r := e.do(sales, "POST", "/api/v1/delivery-orders/"+did+"/reverse", reason, key("do-dependents")); r.reason() != "reversal_has_dependents" {
		t.Fatalf("acceptance erased: %v", r.Body)
	}
	e.must(200, e.do(sales, "POST", "/api/v1/milestones/"+mid+"/reset", reason, key("op-reset")), "reset acceptance")
	e.must(200, e.do(sales, "POST", "/api/v1/delivery-orders/"+did+"/reverse", reason, key("op-do-rev")), "reverse DO")
	e.must(200, e.do(sales, "POST", "/api/v1/goods-receipts/"+gid+"/reverse", reason, key("op-gr-rev")), "reverse GR")
	e.must(200, e.do(sales, "POST", "/api/v1/purchase-orders/"+pid+"/close", reason, key("op-close")), "close remaining PO")
	got := e.must(200, e.do(sales, "GET", "/api/v1/contracts/"+cid, nil, nil), "balances")
	l := lineByNo(got, 1)
	if l["ordered"] != "0" || l["received"] != "0" || l["delivered"] != "0" || l["remaining"] != "10" {
		t.Fatalf("bad corrected balances: %v", l)
	}
	var reversals, history int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM document_reversals`).Scan(&reversals)
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM payment_term_history`).Scan(&history)
	if reversals != 2 || history != 2 {
		t.Fatalf("missing correction history: %d,%d", reversals, history)
	}
	t.Run("note replay and assignment handover", func(t *testing.T) {
		e.user("handover-admin@test.vn", []string{"admin"})
		e.user("successor@test.vn", []string{"sales"})
		admin := e.login("handover-admin@test.vn")
		successor := e.login("successor@test.vn")
		oldMe := e.must(200, e.do(sales, "GET", "/api/v1/me", nil, nil), "old owner")
		newMe := e.must(200, e.do(successor, "GET", "/api/v1/me", nil, nil), "new owner")
		from := oldMe["user"].(map[string]any)["id"]
		to := newMe["user"].(map[string]any)["id"]
		body := map[string]any{"client_id": "7fa06b04-a46c-480a-9462-bc462cc8cf1b", "customer_id": cust["id"], "contract_id": cid, "body": "Customer requested follow-up", "written_at": "2026-11-10T01:00:00Z", "task": map[string]any{"title": "Call customer"}}
		var rs [2]result
		var wg sync.WaitGroup
		for i := range rs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); rs[i] = e.do(sales, "POST", "/api/v1/notes", body, nil) }(i)
		}
		wg.Wait()
		if rs[0].Status+rs[1].Status != 401 || rs[0].data()["id"] != rs[1].data()["id"] {
			t.Fatalf("duplicate note: %v", rs)
		}
		body["body"] = "Changed after uncertain response"
		if r := e.do(sales, "POST", "/api/v1/notes", body, nil); r.Status != 409 {
			t.Fatalf("changed replay accepted: %v", r.Body)
		}
		preview := e.must(200, e.do(admin, "GET", "/api/v1/handovers/preview?from_user="+from.(string), nil, nil), "preview")
		contracts := preview["contracts"].([]any)
		tasks := preview["tasks"].([]any)
		if len(contracts) != 1 || len(tasks) != 1 {
			t.Fatalf("bad preview: %v", preview)
		}
		taskID := tasks[0].(map[string]any)["id"]
		handover := map[string]any{"from_user": from, "to_user": to, "reason": "Staff handover", "contracts": contracts, "task_ids": []any{taskID}}
		// The command accepts only the preview's explicit ID and version.
		contractPreview := contracts[0].(map[string]any)
		handover["contracts"] = []any{map[string]any{"id": contractPreview["id"], "version": contractPreview["version"]}}
		e.must(201, e.do(admin, "POST", "/api/v1/handovers", handover, key("handover")), "handover")
		if r := e.do(sales, "GET", "/api/v1/contracts/"+cid, nil, nil); r.Status != 404 {
			t.Fatalf("former owner retained contract access: %v", r.Body)
		}
		e.must(200, e.do(successor, "GET", "/api/v1/contracts/"+cid, nil, nil), "successor contract")
		var author, taskOwner string
		if err := e.pool.QueryRow(context.Background(), `SELECT n.author_id,t.owner_id FROM notes n JOIN tasks t ON t.note_id=n.id WHERE t.id=$1`, taskID).Scan(&author, &taskOwner); err != nil {
			t.Fatal(err)
		}
		if author != from || taskOwner != to {
			t.Fatalf("handover changed history or missed task: %s %s", author, taskOwner)
		}
	})

}
