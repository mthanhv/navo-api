//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"navo-api/internal/config"
	"navo-api/internal/notify"
)

// fakeZalo plays the Zalo Bot API: one token, queued incoming messages, recorded sends.
type fakeZalo struct {
	mu      sync.Mutex
	updates []map[string]any
	sent    []map[string]string
	fail    bool
}

func (f *fakeZalo) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/botGOOD-TOKEN-123/") {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized", "error_code": 401})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch strings.TrimPrefix(r.URL.Path, "/botGOOD-TOKEN-123/") {
	case "getMe":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": "b1", "display_name": "Bot Navo"}})
	case "getUpdates":
		out := f.updates
		f.updates = nil
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
	case "sendMessage":
		if f.fail {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "quota exceeded"})
			return
		}
		f.sent = append(f.sent, map[string]string{"chat": body["chat_id"].(string), "text": body["text"].(string)})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": "m"}})
	}
}

func (f *fakeZalo) message(id int64, chat, kind, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, map[string]any{"update_id": id, "event_name": "message.text.received",
		"message": map[string]any{"from": map[string]any{"id": "x", "display_name": "Khoa"}, "chat": map[string]any{"id": chat, "chat_type": kind}, "text": text}})
}

func (f *fakeZalo) to(chat string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, m := range f.sent {
		if m["chat"] == chat {
			out = append(out, m["text"])
		}
	}
	return out
}

// Admin connects the bot; a salesperson links their Zalo with a code; a group registers by
// @mentioning the bot; the morning digest and expiring quotations reach the right chats once,
// never during quiet hours, and a refused send is retried then marked failed.
func TestZaloNotifications(t *testing.T) {
	fake := &fakeZalo{}
	zs := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer zs.Close()
	e := setup(t, func(c *config.Config) { c.ZaloAPIBase = zs.URL })
	e.user("nam@test.vn", []string{"admin"})
	e.user("khoa@test.vn", []string{"sales"})
	nam, khoa := e.login("nam@test.vn"), e.login("khoa@test.vn")
	if r := e.do(khoa, "GET", "/api/v1/settings", nil, nil); r.Status != 403 {
		t.Fatalf("settings are for admin: %d", r.Status)
	}
	if r := e.do(nam, "PUT", "/api/v1/settings/zalo", map[string]any{"token": "WRONG-TOKEN-1"}, nil); r.Status != 400 {
		t.Fatalf("a wrong token is refused: %d %v", r.Status, r.Body)
	}
	z := e.must(200, e.do(nam, "PUT", "/api/v1/settings/zalo", map[string]any{"token": "GOOD-TOKEN-123"}, nil), "connect")
	if z["bot_name"] != "Bot Navo" || z["token_hint"] != "••••-123" {
		t.Fatalf("connected: %v", z)
	}
	if s := e.must(200, e.do(nam, "GET", "/api/v1/settings", nil, nil), "settings"); strings.Contains(mustJSON(s), "GOOD-TOKEN-123") {
		t.Fatal("the token is never returned")
	}

	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	today := time.Now().In(loc)
	if today.Weekday() == time.Sunday { // no digest on Sundays: use Saturday
		today = today.AddDate(0, 0, -1)
	}
	now := time.Date(today.Year(), today.Month(), today.Day(), 6, 30, 0, 0, loc) // quiet hours until 07:00
	svc := notify.New(e.pool, zs.URL, "https://navo.test", loc)
	svc.Now = func() time.Time { return now }
	run := func() {
		t.Helper()
		if err := svc.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	code := e.must(200, e.do(khoa, "POST", "/api/v1/me/zalo/code", nil, nil), "code")["code"].(string)
	fake.message(1, "u-khoa", "PRIVATE", "mã của tôi "+code)
	fake.message(2, "g-sales", "GROUP", "@Bot Navo đăng ký")
	run()
	if me := e.must(200, e.do(khoa, "GET", "/api/v1/me/zalo", nil, nil), "me"); me["linked"] != true {
		t.Fatalf("khoa linked: %v", me)
	}
	fake.message(3, "u-khoa", "PRIVATE", code) // the code is used up
	run()
	if got := fake.to("u-khoa"); len(got) != 2 || !strings.Contains(got[0], "Đã kết nối") || !strings.Contains(got[1], "hết hạn") {
		t.Fatalf("link replies: %v", got)
	}
	settings := e.must(200, e.do(nam, "GET", "/api/v1/settings", nil, nil), "settings")
	var group string
	for _, c := range settings["chats"].([]any) {
		if m := c.(map[string]any); m["kind"] == "group" {
			group = m["id"].(string)
		}
	}
	if group == "" {
		t.Fatalf("group registered: %v", settings["chats"])
	}
	e.must(200, e.do(nam, "PATCH", "/api/v1/zalo-chats/"+group, map[string]any{"title": "Nhóm Sales"}, nil), "rename")
	e.must(200, e.do(nam, "PATCH", "/api/v1/settings/notifications", map[string]any{"digest_time": "08:00", "quiet_from": "20:00", "quiet_to": "07:00",
		"rules": []any{map[string]any{"key": "attention_digest", "enabled": true, "to_people": true, "group_id": group}}}, nil), "rules")

	// Khoa's active contract has a line due soon with no delivery plan; his quotation expires in 2 days.
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "ZLC", "name": "Customer", "is_customer": true}, nil), "customer")["id"].(string)
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van", "unit": "cái"}, nil), "product")["id"].(string)
	due := now.AddDate(0, 0, 5).Format(time.DateOnly)
	k := e.must(201, e.do(khoa, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust, "title": "Zalo", "currency": "VND",
		"lines": []any{map[string]any{"product_id": prod, "name_on_contract": "Van bi DN50", "quantity": "10", "unit_price": "1", "due_on": due}}}, nil), "contract")
	e.must(200, e.do(khoa, "POST", "/api/v1/contracts/"+k["id"].(string)+"/activate", map[string]any{"signed_on": now.Format(time.DateOnly)}, key("zact")), "activate")
	q := e.must(201, e.do(khoa, "POST", "/api/v1/quotations", map[string]any{"customer_id": cust, "title": "Van 2027", "currency": "VND", "valid_until": now.AddDate(0, 0, 2).Format(time.DateOnly),
		"lines": []any{map[string]any{"product_id": prod, "quantity": "1", "unit_price": "1"}}}, nil), "quote")
	e.must(200, e.do(khoa, "POST", "/api/v1/quotations/"+q["id"].(string)+"/send", nil, key("zsend")), "send quote")

	run() // 06:30: before the digest time, nothing new
	if n := len(fake.to("u-khoa")); n != 2 {
		t.Fatalf("nothing before 08:00: %d", n)
	}
	now = now.Add(2 * time.Hour) // 08:30
	run()
	run() // a second run the same morning sends nothing again
	mine := fake.to("u-khoa")[2:]
	if len(mine) != 2 || !strings.Contains(strings.Join(mine, "\n"), "chưa có kế hoạch giao") || !strings.Contains(strings.Join(mine, "\n"), q["number"].(string)) {
		t.Fatalf("khoa's morning messages: %v", mine)
	}
	if g := fake.to("g-sales"); len(g) != 2 || !strings.Contains(g[1], "Toàn công ty") || !strings.Contains(g[1], "khoa@test.vn: 1") {
		t.Fatalf("group digest (after the welcome): %v", g)
	}
	for _, m := range append(mine, fake.to("g-sales")...) {
		if strings.Contains(m, "VND") || strings.Contains(m, "₫") {
			t.Fatalf("no money in Zalo: %s", m)
		}
	}

	// Sending fails three times, then the message is marked failed and shown in the log.
	fake.mu.Lock()
	fake.fail = true
	fake.mu.Unlock()
	e.must(200, e.do(nam, "PATCH", "/api/v1/settings/notifications", map[string]any{"digest_time": "08:00", "quiet_from": "20:00", "quiet_to": "07:00",
		"rules": []any{map[string]any{"key": "attention_digest", "enabled": true, "to_people": true, "group_id": nil}}}, nil), "rules again")
	if r := e.do(nam, "POST", "/api/v1/zalo-chats/"+group+"/test", nil, nil); r.reason() != "zalo_send_failed" {
		t.Fatalf("test message reports Zalo's refusal: %v", r.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO notifications (dedupe_key, rule, chat_id, body) SELECT 'x', 'attention_digest', id, 'thử' FROM zalo_chats WHERE chat_id = 'u-khoa'`); err != nil {
		t.Fatal(err)
	}
	run()
	run()
	run()
	var status string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM notifications WHERE dedupe_key = 'x'`).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("failed after three tries: %s %v", status, err)
	}
	if s := e.must(200, e.do(nam, "GET", "/api/v1/settings", nil, nil), "settings"); s["zalo"].(map[string]any)["last_error"] == nil {
		t.Fatal("the last error is shown")
	}
	e.must(200, e.do(khoa, "DELETE", "/api/v1/me/zalo", nil, nil), "unlink")
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
