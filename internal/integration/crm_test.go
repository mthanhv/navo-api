//go:build integration

package integration

import "testing"

// Field notes are stored once even when an offline draft is sent twice; the task they
// create lands on the author's list; photos attach to the note; others' notes stay private.
func TestNotesAndTasks(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	khoa, ha, lan := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("lan@test.vn")
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DPN", "name": "Đạm Phương Nam", "is_customer": true}, nil), "c")
	note := map[string]any{"client_id": "7c9e6679-7425-40de-944b-e07fc1f90ae7", "customer_id": cust["id"], "body": "Anh Huy cần báo giá 6 van DN80",
		"written_at": "2026-11-07T10:42:00+07:00", "task": map[string]any{"title": "Gửi báo giá 6 van cầu DN80", "due_on": "2026-11-14"}}
	first := e.must(201, e.do(khoa, "POST", "/api/v1/notes", note, nil), "note")
	again := e.must(200, e.do(khoa, "POST", "/api/v1/notes", note, nil), "resend")
	if first["id"] != again["id"] {
		t.Fatal("a resent offline draft is stored once")
	}
	tasks := e.do(khoa, "GET", "/api/v1/tasks", nil, nil).Body["data"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["due_on"] != "2026-11-14" {
		t.Fatalf("task from the note: %v", tasks)
	}
	tid := tasks[0].(map[string]any)["id"].(string)
	if r := e.do(ha, "POST", "/api/v1/tasks/"+tid+"/done", nil, nil); r.Status != 404 {
		t.Fatalf("only the owner closes a task: %d", r.Status)
	}
	if e.must(200, e.do(khoa, "POST", "/api/v1/tasks/"+tid+"/done", nil, nil), "done")["done_at"] == nil {
		t.Fatal("task done")
	}
	e.must(201, e.upload(khoa, "note", first["id"].(string), "photo", "anh.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")), "photo")
	if r := e.upload(ha, "note", first["id"].(string), "photo", "x.png", []byte("\x89PNG\r\n\x1a\n")); r.Status != 404 {
		t.Fatalf("another salesperson cannot attach to this note: %d", r.Status)
	}
	if n := len(e.do(ha, "GET", "/api/v1/notes", nil, nil).Body["data"].([]any)); n != 0 {
		t.Fatalf("other sales do not see the note: %d", n)
	}
	if n := len(e.do(lan, "GET", "/api/v1/notes?customer_id="+cust["id"].(string), nil, nil).Body["data"].([]any)); n != 1 {
		t.Fatalf("leaders see notes: %d", n)
	}
	if r := e.do(khoa, "POST", "/api/v1/ai/note-parse", map[string]any{"text": "x"}, nil); r.reason() != "ai_disabled" {
		t.Fatalf("AI off without a key: %v", r.Body)
	}
}
