package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/gin-gonic/gin"

	"navo-api/internal/apperrors"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// NoteTask is the follow-up a note asks for, if any.
type NoteTask struct {
	Body      string  `json:"body"`
	TaskTitle *string `json:"task_title"`
	DueOn     *string `json:"due_on"`
}

var noteSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"body", "task_title", "due_on"},
	"properties": map[string]any{"body": map[string]any{"type": "string"}, "task_title": nullableString, "due_on": nullableString},
}

const noteSystem = `A salesperson dictated a note after meeting a customer. Return:
- body: the note, cleaned of filler words and dictation slips, in the same language, nothing added.
- task_title: the follow-up they must do, as a short imperative (e.g. "Gửi báo giá 6 van cầu DN80"), or null if none.
- due_on: the date of that follow-up as YYYY-MM-DD, resolving words like "thứ Sáu", "tuần sau", "20/11" against today's date given below; null if none.`

// ParseNote reads a dictated note into body, follow-up and date.
func (e *Extractor) ParseNote(ctx context.Context, text string, today time.Time) (*NoteTask, error) {
	if !e.Enabled() {
		return nil, apperrors.Rule("ai_disabled", "AI drafts are not turned on for this installation.", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	msg, err := e.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model: e.model, MaxTokens: 2000,
		System:       []anthropic.BetaTextBlockParam{{Text: noteSystem}},
		Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("Today is " + today.Format("Monday 2006-01-02") + ".\n\nNote:\n" + text))},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortLow, Format: anthropic.BetaJSONOutputFormatParam{Schema: noteSchema}},
		Fallbacks:    anthropic.BetaFallbacksParamUnion{OfDefault: "default"},
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	})
	if err != nil {
		return nil, apperrors.Rule("ai_unavailable", "Reading the note is not available right now. Fill in the task yourself.", nil)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return nil, apperrors.Rule("ai_refused", "This note could not be read automatically.", nil)
	}
	var out bytes.Buffer
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	var n NoteTask
	if err := json.Unmarshal(out.Bytes(), &n); err != nil {
		return nil, apperrors.Rule("ai_unavailable", "Reading the note is not available right now. Fill in the task yourself.", nil)
	}
	return &n, nil
}

// NoteParse handles POST /ai/note-parse {text}.
func (h *Handlers) NoteParse(c *gin.Context) {
	var in struct {
		Text string `json:"text"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || len([]rune(in.Text)) > 5000 {
		response.Fail(c, apperrors.Invalid("text", "required", nil))
		return
	}
	n, err := h.AI.ParseNote(c.Request.Context(), in.Text, time.Now().In(h.TZ))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, n)
}
