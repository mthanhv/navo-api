package notify

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// Handlers serve the Settings page (Admin) and each person's own Zalo link.
type Handlers struct{ Svc *Service }

var (
	taxCode  = regexp.MustCompile(`^[0-9]{10}(-[0-9]{3})?$`)
	clock    = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	ruleKeys = map[string]bool{"attention_digest": true, "override_requested": true, "deliveries_tomorrow": true, "quotation_expiring": true}
)

type company struct {
	Name    string  `json:"name"`
	TaxCode *string `json:"tax_code"`
	Address *string `json:"address"`
	Phone   *string `json:"phone"`
	Email   *string `json:"email"`
}

type ruleView struct {
	Key      string     `json:"key"`
	Enabled  bool       `json:"enabled"`
	ToPeople bool       `json:"to_people"`
	GroupID  *uuid.UUID `json:"group_id"`
}

type chatView struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	UserName  *string   `json:"user_name"`
	CreatedAt time.Time `json:"created_at"`
}

type logView struct {
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at"`
	Rule      string     `json:"rule"`
	ChatTitle string     `json:"chat_title"`
	Status    string     `json:"status"`
	Error     *string    `json:"error"`
	Body      string     `json:"body"`
}

// Get handles GET /settings: company, Zalo connection, rules, chats and the last messages.
func (h *Handlers) Get(c *gin.Context) {
	ctx := c.Request.Context()
	pool := h.Svc.Pool
	var co company
	if err := pool.QueryRow(ctx, `SELECT name, tax_code, address, phone, email FROM company_settings`).Scan(&co.Name, &co.TaxCode, &co.Address, &co.Phone, &co.Email); err != nil {
		response.Fail(c, err)
		return
	}
	var token, bot, lastErr *string
	var connected, errAt *time.Time
	var digest, qf, qt string
	if err := pool.QueryRow(ctx, `SELECT zalo_token, zalo_bot_name, zalo_connected_at, to_char(digest_time,'HH24:MI'), to_char(quiet_from,'HH24:MI'), to_char(quiet_to,'HH24:MI'), last_error, last_error_at
		FROM notification_settings`).Scan(&token, &bot, &connected, &digest, &qf, &qt, &lastErr, &errAt); err != nil {
		response.Fail(c, err)
		return
	}
	hint := ""
	if token != nil {
		hint = "••••" + lastN(*token, 4)
	}
	rows, err := pool.Query(ctx, `SELECT key, enabled, to_people, group_id FROM notification_rules ORDER BY array_position(ARRAY['attention_digest','override_requested','deliveries_tomorrow','quotation_expiring'], key)`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	rules, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ruleView])
	if err != nil {
		response.Fail(c, err)
		return
	}
	rows, err = pool.Query(ctx, `SELECT c.id, c.kind, c.title, u.full_name, c.created_at FROM zalo_chats c LEFT JOIN users u ON u.id = c.user_id ORDER BY c.kind DESC, c.title`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	chats, err := pgx.CollectRows(rows, pgx.RowToStructByPos[chatView])
	if err != nil {
		response.Fail(c, err)
		return
	}
	rows, err = pool.Query(ctx, `SELECT n.created_at, n.sent_at, n.rule, c.title, n.status, n.error, n.body FROM notifications n JOIN zalo_chats c ON c.id = n.chat_id ORDER BY n.created_at DESC LIMIT 100`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	logs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[logView])
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"company":     co,
		"zalo":        gin.H{"connected": token != nil, "bot_name": bot, "token_hint": hint, "connected_at": connected, "last_error": lastErr, "last_error_at": errAt},
		"digest_time": digest, "quiet_from": qf, "quiet_to": qt, "public_url_set": h.Svc.PublicURL != "",
		"rules": rules, "chats": chats, "log": logs,
	})
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// UpdateCompany handles PATCH /settings/company.
func (h *Handlers) UpdateCompany(c *gin.Context) {
	var in company
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.TaxCode, in.Address, in.Phone, in.Email = trimmed(in.TaxCode), trimmed(in.Address), trimmed(in.Phone), trimmed(in.Email)
	var errs apperrors.FieldErrors
	if in.Name == "" {
		errs.Add("name", "required", nil)
	} else if len([]rune(in.Name)) > 200 {
		errs.Add("name", "too_long", map[string]any{"max": 200})
	}
	if in.TaxCode != nil && !taxCode.MatchString(*in.TaxCode) {
		errs.Add("tax_code", "tax_code", nil)
	}
	if in.Address != nil && len([]rune(*in.Address)) > 300 {
		errs.Add("address", "too_long", map[string]any{"max": 300})
	}
	if in.Phone != nil && len([]rune(*in.Phone)) > 40 {
		errs.Add("phone", "too_long", map[string]any{"max": 40})
	}
	if in.Email != nil {
		if a, err := mail.ParseAddress(*in.Email); err != nil || a.Address != *in.Email {
			errs.Add("email", "email", nil)
		}
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Svc.Pool, "settings.company", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `UPDATE company_settings SET name = $1, tax_code = $2, address = $3, phone = $4, email = $5`, in.Name, in.TaxCode, in.Address, in.Phone, in.Email); err != nil {
			return nil, err
		}
		return in, audit.Record(ctx, tx, "update", "company_settings", uuid.Nil, map[string]any{"name": in.Name, "tax_code": in.TaxCode})
	})
}

// ConnectZalo handles PUT /settings/zalo {token}: checks the token with Zalo, then keeps it.
func (h *Handlers) ConnectZalo(c *gin.Context) {
	var in struct {
		Token string `json:"token"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.Token = strings.TrimSpace(in.Token)
	if len(in.Token) < 10 || len(in.Token) > 300 || strings.ContainsAny(in.Token, " /?#") {
		response.Fail(c, apperrors.Invalid("token", "format", nil))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	name, err := h.Svc.Zalo.GetMe(ctx, in.Token)
	if err != nil {
		if errors.Is(err, ErrZalo) {
			response.Fail(c, apperrors.Invalid("token", "zalo_token_rejected", nil))
			return
		}
		response.Fail(c, apperrors.Rule("zalo_unreachable", "Cannot reach Zalo right now. Try again in a moment.", nil))
		return
	}
	httpx.Command(c, h.Svc.Pool, "settings.zalo", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		// A different bot cannot reach the old chats: they are forgotten with the old token.
		var old *string
		if err := tx.QueryRow(ctx, `SELECT zalo_token FROM notification_settings FOR UPDATE`).Scan(&old); err != nil {
			return nil, err
		}
		if old != nil && *old != in.Token {
			if _, err := tx.Exec(ctx, `DELETE FROM zalo_chats`); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE notification_settings SET zalo_token = $1, zalo_bot_name = $2, zalo_connected_at = now(), update_offset = 0, last_error = NULL, last_error_at = NULL, updated_at = now()`, in.Token, name); err != nil {
			return nil, err
		}
		return gin.H{"connected": true, "bot_name": name, "token_hint": "••••" + lastN(in.Token, 4)}, audit.Record(ctx, tx, "connect", "zalo_bot", uuid.Nil, map[string]any{"bot": name})
	})
}

// DisconnectZalo handles DELETE /settings/zalo: stops sending; linked chats are kept for a reconnect of the same bot.
func (h *Handlers) DisconnectZalo(c *gin.Context) {
	httpx.Command(c, h.Svc.Pool, "settings.zalo_off", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `UPDATE notification_settings SET zalo_token = NULL, zalo_connected_at = NULL, updated_at = now()`); err != nil {
			return nil, err
		}
		return gin.H{"connected": false}, audit.Record(ctx, tx, "disconnect", "zalo_bot", uuid.Nil, nil)
	})
}

// UpdateNotifications handles PATCH /settings/notifications: times and rules.
func (h *Handlers) UpdateNotifications(c *gin.Context) {
	var in struct {
		DigestTime string     `json:"digest_time"`
		QuietFrom  string     `json:"quiet_from"`
		QuietTo    string     `json:"quiet_to"`
		Rules      []ruleView `json:"rules"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	for k, v := range map[string]string{"digest_time": in.DigestTime, "quiet_from": in.QuietFrom, "quiet_to": in.QuietTo} {
		if !clock.MatchString(v) {
			errs.Add(k, "format", map[string]any{"pattern": "HH:MM"})
		}
	}
	for i, r := range in.Rules {
		if !ruleKeys[r.Key] {
			errs.Add(fmt.Sprintf("rules[%d].key", i), "one_of", nil)
		}
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Svc.Pool, "settings.notifications", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `UPDATE notification_settings SET digest_time = $1::time, quiet_from = $2::time, quiet_to = $3::time, updated_at = now()`, in.DigestTime, in.QuietFrom, in.QuietTo); err != nil {
			return nil, err
		}
		for i, r := range in.Rules {
			if r.GroupID != nil {
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM zalo_chats WHERE id = $1 AND kind = 'group')`, *r.GroupID).Scan(&ok); err != nil || !ok {
					return nil, apperrors.Invalid(fmt.Sprintf("rules[%d].group_id", i), "not_found", nil)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE notification_rules SET enabled = $2, to_people = $3, group_id = $4 WHERE key = $1`, r.Key, r.Enabled, r.ToPeople, r.GroupID); err != nil {
				return nil, err
			}
		}
		return gin.H{"saved": true}, audit.Record(ctx, tx, "update", "notification_settings", uuid.Nil, map[string]any{"digest_time": in.DigestTime, "rules": len(in.Rules)})
	})
}

// RenameChat handles PATCH /zalo-chats/:id {title} (groups get a name people recognise).
func (h *Handlers) RenameChat(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len([]rune(in.Title)) > 120 {
		response.Fail(c, apperrors.Invalid("title", "required", nil))
		return
	}
	tag, err := h.Svc.Pool.Exec(c.Request.Context(), `UPDATE zalo_chats SET title = $2 WHERE id = $1`, id, in.Title)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.Fail(c, apperrors.NotFound())
		return
	}
	response.OK(c, gin.H{"id": id, "title": in.Title})
}

// RemoveChat handles DELETE /zalo-chats/:id: no more messages to that person or group.
func (h *Handlers) RemoveChat(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if _, err := h.Svc.Pool.Exec(c.Request.Context(), `DELETE FROM zalo_chats WHERE id = $1`, id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "removed": true})
}

// TestChat handles POST /zalo-chats/:id/test: sends one message now and reports Zalo's answer.
func (h *Handlers) TestChat(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	ctx := c.Request.Context()
	var token *string
	var chat, title string
	if err := h.Svc.Pool.QueryRow(ctx, `SELECT s.zalo_token, c.chat_id, c.title FROM zalo_chats c, notification_settings s WHERE c.id = $1`, id).Scan(&token, &chat, &title); err != nil {
		response.Fail(c, err)
		return
	}
	if token == nil {
		response.Fail(c, apperrors.Rule("zalo_not_connected", "Connect the Zalo bot first.", nil))
		return
	}
	if err := h.Svc.Zalo.Send(ctx, *token, chat, "🔔 Tin thử từ Navo: kênh thông báo này đang hoạt động."); err != nil {
		h.Svc.recordError(ctx, err)
		response.Fail(c, apperrors.Rule("zalo_send_failed", err.Error(), nil))
		return
	}
	response.OK(c, gin.H{"sent": true, "title": title})
}

// MyZalo handles GET /me/zalo: whether notifications reach the signed-in person on Zalo.
func (h *Handlers) MyZalo(c *gin.Context) {
	p := auth.Principal(c)
	ctx := c.Request.Context()
	var bot *string
	var configured bool
	if err := h.Svc.Pool.QueryRow(ctx, `SELECT zalo_token IS NOT NULL, zalo_bot_name FROM notification_settings`).Scan(&configured, &bot); err != nil {
		response.Fail(c, err)
		return
	}
	var since *time.Time
	err := h.Svc.Pool.QueryRow(ctx, `SELECT created_at FROM zalo_chats WHERE user_id = $1`, p.UserID).Scan(&since)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"configured": configured, "bot_name": bot, "linked": since != nil, "linked_at": since})
}

// MyZaloCode handles POST /me/zalo/code: a 6-digit code to send to the bot (15 minutes).
func (h *Handlers) MyZaloCode(c *gin.Context) {
	p := auth.Principal(c)
	ctx := c.Request.Context()
	var configured bool
	if err := h.Svc.Pool.QueryRow(ctx, `SELECT zalo_token IS NOT NULL FROM notification_settings`).Scan(&configured); err != nil {
		response.Fail(c, err)
		return
	}
	if !configured {
		response.Fail(c, apperrors.Rule("zalo_not_connected", "Zalo notifications are not set up yet. Ask the administrator.", nil))
		return
	}
	var code string
	var expires time.Time
	err := db.InTx(ctx, h.Svc.Pool, func(tx pgx.Tx) error {
		for range 5 {
			n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
			code = fmt.Sprintf("%06d", n.Int64())
			err := tx.QueryRow(ctx, `INSERT INTO zalo_link_codes (code, user_id, expires_at) VALUES ($1, $2, now() + interval '15 minutes')
				ON CONFLICT (user_id) DO UPDATE SET code = EXCLUDED.code, expires_at = EXCLUDED.expires_at RETURNING expires_at`, code, p.UserID).Scan(&expires)
			if err == nil {
				return nil
			}
			if apperrors.From(err).Reason != "duplicate" {
				return err
			}
		}
		return apperrors.Rule("try_again", "Try again.", nil)
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"code": code, "expires_at": expires})
}

// UnlinkMyZalo handles DELETE /me/zalo.
func (h *Handlers) UnlinkMyZalo(c *gin.Context) {
	p := auth.Principal(c)
	if _, err := h.Svc.Pool.Exec(c.Request.Context(), `DELETE FROM zalo_chats WHERE user_id = $1`, p.UserID); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"linked": false})
}
