//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"navo-api/internal/config"
	"navo-api/internal/router"
)

func TestAIJobRecovery(t *testing.T) {
	e := setup(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"{\"body\":\"Follow up\",\"task_title\":null,\"due_on\":null}"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer provider.Close()
	t.Setenv("ANTHROPIC_BASE_URL", provider.URL)
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	server := httptest.NewServer(router.New(config.Config{SessionTTL: time.Hour, Timezone: loc, FilesDir: t.TempDir(), AnthropicAPIKey: "test-key", AIModel: "test-model"}, e.pool))
	defer server.Close()
	e.srv = server
	e.user("ai@test.vn", []string{"sales"})
	actor := e.login("ai@test.vn")
	for i := 0; i < 2; i++ {
		got := e.must(200, e.do(actor, "POST", "/api/v1/ai/note-parse", map[string]any{"text": "Follow up"}, nil), "read cached note")
		if got["body"] != "Follow up" {
			t.Fatalf("wrong extraction: %v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("successful retry purchased another extraction: %d", calls.Load())
	}
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO ai_extraction_attempts(extraction_id,actor_id) SELECT j.id,j.actor_id FROM ai_extractions j CROSS JOIN generate_series(1,49)`); err != nil {
		t.Fatal(err)
	}
	if r := e.do(actor, "POST", "/api/v1/ai/note-parse", map[string]any{"text": "Different source"}, nil); r.reason() != "ai_daily_limit" {
		t.Fatalf("quota not enforced: %v", r.Body)
	}
	e.must(200, e.do(actor, "POST", "/api/v1/ai/note-parse", map[string]any{"text": "Follow up"}, nil), "cached result remains recoverable at quota")
	if calls.Load() != 1 {
		t.Fatal("quota called provider")
	}
}
