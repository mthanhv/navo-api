//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
	"navo-api/db/migrations"
	"navo-api/internal/db"
	"navo-api/internal/notify"
)

func TestAcceptanceTermsAfterCommercialChanges(t *testing.T) {
	e := setup(t)
	e.user("review@test.vn", []string{"admin"})
	actor := e.login("review@test.vn")
	f := e.activeContract(actor, "SYNC", "10")
	e.receiveAndDeliver(actor, f, "sync", "10", "10")
	term := func(name, trigger string) map[string]any {
		return e.must(201, e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/milestones", map[string]any{"name": name, "trigger": trigger, "amount": "3", "days_after": 0}, nil), "term")
	}
	first := term("Acceptance", "acceptance")
	e.must(201, e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/acceptances", map[string]any{"on": "2026-10-06", "evidence": "Signed", "lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "10"}}}, key("sync-accept")), "accept")
	terms := func() []any {
		return e.do(actor, "GET", "/api/v1/contracts/"+f.contract+"/milestones", nil, nil).Body["data"].([]any)
	}
	if m := term("Added after acceptance", "acceptance"); m["triggered_on"] != "2026-10-06" {
		t.Fatalf("new acceptance term did not inherit event: %v", m)
	}
	manual := term("Change to acceptance", "manual")
	e.must(200, e.do(actor, "PATCH", "/api/v1/milestones/"+manual["id"].(string), map[string]any{"version": manual["version"], "name": "Changed", "trigger": "acceptance", "amount": "3", "days_after": 0}, nil), "change trigger")
	for _, v := range terms() {
		if v.(map[string]any)["triggered_on"] != "2026-10-06" {
			t.Fatalf("term missing event: %v", v)
		}
	}
	amend := func(qty, tag string) result {
		c := e.must(200, e.do(actor, "GET", "/api/v1/contracts/"+f.contract, nil, nil), "contract")
		return e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/amendments", map[string]any{"version": c["version"], "signed_on": "2026-10-07", "reason": "Change quantity", "lines": []any{map[string]any{"contract_line_id": f.line, "quantity": qty, "unit_price": "1"}}}, key(tag))
	}
	e.must(201, amend("11", "sync-up"), "increase")
	for _, v := range terms() {
		if v.(map[string]any)["status"] != "wait" {
			t.Fatalf("incomplete acceptance remained due: %v", v)
		}
	}
	e.must(201, amend("10", "sync-down"), "decrease")
	r := e.must(201, e.do(actor, "POST", "/api/v1/receipts", map[string]any{"customer_id": f.customer, "received_on": "2026-10-08", "amount": "1", "currency": "VND", "method": "bank"}, key("sync-cash")), "cash")
	e.must(200, e.do(actor, "POST", "/api/v1/receipts/"+r["id"].(string)+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": first["id"], "amount": "1"}}}, key("sync-allocate")), "allocate")
	if r := amend("11", "sync-paid"); r.reason() != "acceptance_has_payments" {
		t.Fatalf("paid acceptance lost its source: %d %v", r.Status, r.Body)
	}
	if e.line(actor, f)["contract_quantity"] != "10" {
		t.Fatal("rejected amendment changed contract")
	}
}

func TestCancelledAllocationAndCustomerSummary(t *testing.T) {
	e := setup(t)
	e.user("cash-review@test.vn", []string{"admin"})
	actor := e.login("cash-review@test.vn")
	f := e.activeContract(actor, "CASH", "10")
	other := e.activeContract(actor, "OTHER", "20")
	term := e.must(201, e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/milestones", map[string]any{"name": "Signing", "amount": "10", "trigger": "signing"}, nil), "signing")
	receipt := func(customer, amount, tag string) string {
		return e.must(201, e.do(actor, "POST", "/api/v1/receipts", map[string]any{"customer_id": customer, "received_on": "2026-10-08", "amount": amount, "currency": "VND", "method": "bank"}, key(tag)), "receipt")["id"].(string)
	}
	rid := receipt(f.customer, "5", "cash-one")
	receipt(other.customer, "9", "cash-other")
	filtered := e.must(200, e.do(actor, "GET", "/api/v1/receivables?tab=all&customer_id="+f.customer, nil, nil), "filtered")
	summary := filtered["summary"].([]any)[0].(map[string]any)
	if summary["uncollected"] != "10" || summary["unallocated"] != "5" || summary["due_open"] != "10" {
		t.Fatalf("mixed customer/company scope: %v", summary)
	}
	e.must(200, e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/cancel", map[string]any{"reason": "Customer cancelled"}, key("cancel-cash")), "cancel")
	if r := e.do(actor, "POST", "/api/v1/receipts/"+rid+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": term["id"], "amount": "1"}}}, key("cancelled-allocation")); r.Status != 400 {
		t.Fatalf("cancelled contract accepted cash: %d %v", r.Status, r.Body)
	}
	if r := e.must(200, e.do(actor, "GET", "/api/v1/receipts/"+rid, nil, nil), "receipt"); r["allocated"] != "0" {
		t.Fatalf("failed allocation changed balance: %v", r)
	}
}

func TestCancellationRacesCollection(t *testing.T) {
	e := setup(t)
	e.user("cancel-race@test.vn", []string{"admin"})
	actor := e.login("cancel-race@test.vn")
	f := e.activeContract(actor, "RACECASH", "10")
	term := e.must(201, e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/milestones", map[string]any{"name": "Signing", "amount": "10", "trigger": "signing"}, nil), "term")
	cash := e.must(201, e.do(actor, "POST", "/api/v1/receipts", map[string]any{"customer_id": f.customer, "received_on": "2026-10-08", "amount": "1", "currency": "VND", "method": "bank"}, key("cancel-race-cash")), "cash")
	results := raceAfterLock(t, e, `SELECT id FROM contracts WHERE id=$1 FOR UPDATE`, f.contract, "%FROM contracts%", []func() result{
		func() result {
			return e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/cancel", map[string]any{"reason": "Cancel"}, key("cancel-race"))
		},
		func() result {
			return e.do(actor, "POST", "/api/v1/receipts/"+cash["id"].(string)+"/allocations", map[string]any{"allocations": []any{map[string]any{"milestone_id": term["id"], "amount": "1"}}}, key("cancel-race-allocation"))
		},
	})
	successes := 0
	for _, r := range results {
		if r.Status == 200 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("cancel and collection must serialize: %v", results)
	}
	current := e.must(200, e.do(actor, "GET", "/api/v1/contracts/"+f.contract, nil, nil), "contract")
	receipt := e.must(200, e.do(actor, "GET", "/api/v1/receipts/"+cash["id"].(string), nil, nil), "cash")
	if current["status"] == "cancelled" && receipt["allocated"] != "0" {
		t.Fatal("cash was hidden on a cancelled contract")
	}
}

func TestServiceContractsAreGated(t *testing.T) {
	e := setup(t)
	e.user("service-review@test.vn", []string{"sales"})
	actor := e.login("service-review@test.vn")
	f := e.activeContract(actor, "GATE", "1")
	r := e.do(actor, "POST", "/api/v1/contracts", map[string]any{"customer_id": f.customer, "title": "Service", "kind": "service", "currency": "VND", "lines": []any{map[string]any{"product_id": f.product, "quantity": "1", "unit_price": "1", "line_type": "service"}}}, nil)
	if r.Status != 400 || !strings.Contains(mustJSON(r.Body), "service_unavailable") {
		t.Fatalf("unsupported service contract accepted: %d %v", r.Status, r.Body)
	}
}

func TestDeliveryEvidencePermissions(t *testing.T) {
	e := setup(t)
	e.user("evidence-admin@test.vn", []string{"admin"})
	e.user("evidence-delivery@test.vn", []string{"delivery"})
	admin, delivery := e.login("evidence-admin@test.vn"), e.login("evidence-delivery@test.vn")
	f := e.activeContract(admin, "FILES", "1")
	e.receiveAndDeliver(admin, f, "evidence", "1", "0")
	do := e.deliver(delivery, f, "delivery-evidence", "1")
	docs := e.must(200, e.do(delivery, "GET", "/api/v1/contracts/"+f.contract+"/documents", nil, nil), "documents")
	gr := docs["goods_receipts"].([]any)[0].(map[string]any)["id"].(string)
	for _, x := range []struct{ kind, id, doc string }{{"delivery_order", do, "delivery_note"}, {"goods_receipt", gr, "receipt_note"}} {
		file := e.must(201, e.upload(delivery, x.kind, x.id, x.doc, "signed.txt", []byte("Signed evidence")), "delivery uploads evidence")
		e.must(200, e.do(delivery, "DELETE", "/api/v1/attachment-links/"+file["link_id"].(string), nil, nil), "remove own evidence")
	}
	if r := e.upload(delivery, "contract", f.contract, "signed_contract", "contract.txt", []byte("No contract edits")); r.Status != 403 {
		t.Fatalf("fulfillment permission allowed contract files: %d", r.Status)
	}
	file := e.must(201, e.upload(admin, "delivery_order", do, "delivery_note", "other.txt", []byte("Admin evidence")), "admin evidence")
	if r := e.do(delivery, "DELETE", "/api/v1/attachment-links/"+file["link_id"].(string), nil, nil); r.Status != 403 {
		t.Fatalf("delivery removed someone else's evidence: %d", r.Status)
	}
}

func (e *env) misaCatalogue(token string, content []byte, hash string) result {
	e.t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	f, _ := w.CreateFormFile("file", "catalogue.xlsx")
	f.Write(content)
	if hash != "" {
		w.WriteField("confirm", "true")
		w.WriteField("preview_hash", hash)
	}
	w.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/misa/import", &b)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return result{Status: resp.StatusCode, Body: body}
}
func TestMisaCatalogueReview(t *testing.T) {
	e := setup(t)
	e.user("misa-review@test.vn", []string{"admin"})
	actor := e.login("misa-review@test.vn")
	org := func(code, name, tax string) string {
		return e.must(201, e.do(actor, "POST", "/api/v1/organizations", map[string]any{"code": code, "name": name, "tax_code": tax, "is_customer": true}, nil), "customer")["id"].(string)
	}
	wrong := org("WRONG", "Same legal name", "1234567890")
	right := org("RIGHT", "Reviewed name", "")
	exact := org("EXACT", "Actual legal name", "1111111111")
	f := excelize.NewFile()
	defer f.Close()
	for i, row := range [][]any{{"Mã khách hàng", "Tên khách hàng", "Mã số thuế"}, {"BAD", "Same legal name", "9876543210"}, {"GOOD", "Reviewed name", ""}, {"EXACTCODE", "Outdated name", "1111111111"}} {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		f.SetSheetRow("Sheet1", cell, &row)
	}
	var b bytes.Buffer
	f.Write(&b)
	preview := e.must(200, e.misaCatalogue(actor, b.Bytes(), ""), "preview")
	if preview["matched"] != float64(2) || preview["preview"] != true {
		t.Fatalf("preview: %v", preview)
	}
	var count int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM organizations WHERE misa_code IS NOT NULL`).Scan(&count)
	if count != 0 {
		t.Fatal("preview mutated codes")
	}
	if r := e.misaCatalogue(actor, b.Bytes(), "stale-hash"); r.Status != 409 {
		t.Fatalf("commit without current review: %d", r.Status)
	}
	e.must(200, e.misaCatalogue(actor, b.Bytes(), preview["preview_hash"].(string)), "confirmed import")
	for id, want := range map[string]string{wrong: "", right: "GOOD", exact: "EXACTCODE"} {
		var got string
		if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(misa_code,'') FROM organizations WHERE id=$1`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("code %s = %q want %q (%v)", id, got, want, err)
		}
	}
	if r := e.misaCatalogue(actor, b.Bytes(), preview["preview_hash"].(string)); r.Status != 409 {
		t.Fatalf("changed matches accepted stale preview: %d", r.Status)
	}
}

func TestNotificationRevocation(t *testing.T) {
	e := setup(t)
	e.user("notify-review@test.vn", []string{"sales"})
	e.user("notify-new@test.vn", []string{"sales"})
	actor := e.login("notify-review@test.vn")
	f := e.activeContract(actor, "REVOKE", "1")
	fake := &fakeZalo{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	ctx := context.Background()
	var user string
	e.pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='notify-review@test.vn'`).Scan(&user)
	e.pool.Exec(ctx, `UPDATE notification_settings SET zalo_token='GOOD-TOKEN-123',digest_time='08:00',quiet_from='20:00',quiet_to='07:00'`)
	e.pool.Exec(ctx, `INSERT INTO zalo_chats(chat_id,kind,title,user_id) VALUES('review-person','person','Reviewer',$1)`, user)
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, loc)
	svc := notify.New(e.pool, server.URL, "", loc)
	svc.Now = func() time.Time { return now }
	// A failed network send leaves a private digest queued for retry.
	fake.mu.Lock()
	fake.fail = true
	fake.mu.Unlock()
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE status='pending'`).Scan(&pending)
	if pending == 0 {
		t.Fatal("expected a queued digest")
	}
	// Change ownership without changing the user's version: the DB invalidates the rendered scope too.
	if _, err := e.pool.Exec(ctx, `UPDATE contracts SET owner_id=(SELECT id FROM users WHERE email='notify-new@test.vn') WHERE id=$1`, f.contract); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.fail = false
	fake.mu.Unlock()
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.to("review-person"); len(got) != 0 {
		t.Fatalf("old owner received handed-over details: %v", got)
	}
	// A role downgrade invalidates a digest rendered with company-wide access.
	if _, err := e.pool.Exec(ctx, `UPDATE user_roles SET role='sales_manager' WHERE user_id=$1 AND role='sales'`, user); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.fail = true
	fake.mu.Unlock()
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE status='pending'`).Scan(&pending); err != nil || pending == 0 {
		t.Fatal("manager digest was not queued", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE user_roles SET role='sales' WHERE user_id=$1 AND role='sales_manager'`, user); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.fail = false
	fake.mu.Unlock()
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.to("review-person"); len(got) != 0 {
		t.Fatalf("role downgrade leaked a company digest: %v", got)
	}
	// A manually queued message must also be cancelled when the person is deactivated.
	e.pool.Exec(ctx, `INSERT INTO notifications(dedupe_key,rule,chat_id,body) SELECT 'offboard','attention_digest',id,'Private' FROM zalo_chats WHERE chat_id='review-person'`)
	e.pool.Exec(ctx, `UPDATE users SET is_active=false WHERE id=$1`, user)
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.to("review-person"); len(got) != 0 {
		t.Fatalf("inactive recipient received details: %v", got)
	}
}

func TestLegacyCRMUpgradeAndMigrationAtomicity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	e.pool.Reset()
	old := fstest.MapFS{}
	names, _ := fs.Glob(migrations.FS, "*.sql")
	for _, name := range names {
		if name < "023" {
			b, _ := fs.ReadFile(migrations.FS, name)
			old[name] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := db.Migrate(ctx, e.pool, old); err != nil {
		t.Fatal(err)
	}
	user, org, note, task := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := e.pool.Exec(ctx, `INSERT INTO users(id,email,full_name,password_hash) VALUES($1,'legacy@test.vn','Legacy','unused')`, user); err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `INSERT INTO organizations(id,code,name,is_customer) VALUES($1,'LEGACY','Legacy',true)`, org)
	if _, err := e.pool.Exec(ctx, `INSERT INTO notes(id,client_id,customer_id,body,written_at,author_id) VALUES($1,$1,$2,'Before upgrade',now(),$3)`, note, org, user); err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `INSERT INTO tasks(id,title,owner_id,customer_id,note_id,created_by) VALUES($1,'Follow up',$2,$3,$4,$2)`, task, user, org, note)
	if _, err := db.Migrate(ctx, e.pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx, e.pool, old); err != nil {
		t.Fatal("old schema contract failed", err)
	}
	var body string
	if err := e.pool.QueryRow(ctx, `SELECT body FROM notes WHERE id=$1`, note).Scan(&body); err != nil || body != "Before upgrade" {
		t.Fatalf("lost legacy note: %q %v", body, err)
	}
	// Old images continue to use real INSERT ... ON CONFLICT tables and update tasks.
	next := uuid.New()
	if _, err := e.pool.Exec(ctx, `INSERT INTO notes(id,client_id,customer_id,body,written_at,author_id) VALUES($1,$1,$2,'Old image write',now(),$3) ON CONFLICT(client_id) DO NOTHING`, next, org, user); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT summary FROM activities WHERE id=$1`, next).Scan(&body); err != nil || body != "Old image write" {
		t.Fatalf("legacy write not mirrored: %q %v", body, err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE tasks SET done_at=now() WHERE id=$1`, task); err != nil {
		t.Fatal(err)
	}
	var status string
	e.pool.QueryRow(ctx, `SELECT status FROM activities WHERE id=$1`, task).Scan(&status)
	if status != "done" {
		t.Fatal("legacy task completion not mirrored")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE activities SET summary='New image write' WHERE id=$1`, next); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT body FROM notes WHERE id=$1`, next).Scan(&body); err != nil || body != "New image write" {
		t.Fatal("new write not visible to old image", body, err)
	}
	bad := fstest.MapFS{"900_temporary.sql": {Data: []byte(`CREATE TABLE batch_should_rollback(id int)`)}, "901_failure.sql": {Data: []byte(`SELECT missing_migration_function()`)}}
	if _, err := db.Migrate(ctx, e.pool, bad); err == nil {
		t.Fatal("invalid batch succeeded")
	}
	var leftovers bool
	e.pool.QueryRow(ctx, `SELECT to_regclass('batch_should_rollback') IS NOT NULL OR EXISTS(SELECT 1 FROM schema_migrations WHERE version='900_temporary')`).Scan(&leftovers)
	if leftovers {
		t.Fatal("failed batch exposed an intermediate migration")
	}
}
