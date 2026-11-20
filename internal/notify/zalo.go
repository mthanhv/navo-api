// Package notify sends Navo's notifications through a Zalo Bot (Zalo Bot Platform,
// https://bot.zapps.me/docs; REQUIREMENTS 5.15 / 1.9): to people who linked their Zalo, and to
// groups the bot was @mentioned in. Messages never carry purchase prices or money amounts.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultAPIBase is the Zalo Bot API host (sendMessage doc, 12/10/2026).
const DefaultAPIBase = "https://bot-api.zaloplatforms.com"

// Zalo calls the Bot API: https://{base}/bot{token}/{method}.
type Zalo struct {
	Base string
	HTTP *http.Client
}

// ErrZalo is a refusal from Zalo (ok:false), as opposed to a network failure.
var ErrZalo = errors.New("zalo refused")

func (z Zalo) call(ctx context.Context, token, method string, body any, out any) error {
	base := z.Base
	if base == "" {
		base = DefaultAPIBase
	}
	client := z.HTTP
	if client == nil {
		client = &http.Client{Timeout: 40 * time.Second}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/bot"+token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		// The URL holds the token: report the method only.
		return fmt.Errorf("zalo %s: request failed", method)
	}
	defer res.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return fmt.Errorf("zalo %s: unreadable reply (HTTP %d)", method, res.StatusCode)
	}
	if !env.OK {
		msg := env.Description
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return fmt.Errorf("%w: %s: %s", ErrZalo, method, msg)
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// GetMe checks a token and returns the bot's display name.
func (z Zalo) GetMe(ctx context.Context, token string) (string, error) {
	var me map[string]any
	if err := z.call(ctx, token, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	for _, k := range []string{"display_name", "account_name", "name"} {
		if v, ok := me[k].(string); ok && v != "" {
			return v, nil
		}
	}
	return "Zalo Bot", nil
}

// Send posts a text message to a person or group (1–2000 characters).
func (z Zalo) Send(ctx context.Context, token, chatID, text string) error {
	return z.call(ctx, token, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// Update is one incoming message, reduced to what Navo uses.
type Update struct {
	ID       int64
	ChatID   string
	ChatType string // PRIVATE for a person; anything else is treated as a group
	FromName string
	Text     string
}

type rawUpdate struct {
	UpdateID  int64  `json:"update_id"`
	EventName string `json:"event_name"`
	Message   struct {
		From struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"from"`
		Chat struct {
			ID       string `json:"id"`
			ChatType string `json:"chat_type"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// Updates long-polls for new messages (getUpdates; used when no webhook is set). Zalo's docs
// show one update per reply; a list is accepted too.
func (z Zalo) Updates(ctx context.Context, token string, offset int64, wait time.Duration) ([]Update, error) {
	var raw json.RawMessage
	if err := z.call(ctx, token, "getUpdates", map[string]any{"offset": offset, "timeout": int(wait.Seconds())}, &raw); err != nil {
		return nil, err
	}
	var list []rawUpdate
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
	} else if len(raw) > 0 && raw[0] == '{' {
		var one rawUpdate
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		list = []rawUpdate{one}
	}
	out := []Update{}
	for _, u := range list {
		if u.Message.Chat.ID == "" {
			continue
		}
		out = append(out, Update{ID: u.UpdateID, ChatID: u.Message.Chat.ID, ChatType: u.Message.Chat.ChatType, FromName: u.Message.From.DisplayName, Text: u.Message.Text})
	}
	return out, nil
}
