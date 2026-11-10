package crm

import (
	"context"
	"log/slog"
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
	"navo-api/internal/mailer"
	"navo-api/internal/response"
)

// Limits keep one mailing a personal note to known contacts, not a bulk campaign.
const (
	maxRecipients      = 200
	maxMailingProducts = 20
)

// MailingProduct is a catalogue item named in an email, as it was when sent.
type MailingProduct struct {
	ProductID        uuid.UUID `json:"product_id"`
	Name             string    `json:"name"`
	Manufacturer     *string   `json:"manufacturer"`
	ManufacturerCode *string   `json:"manufacturer_code"`
	description      *string
	origin           *string
}

// Recipient is one person a mailing went to and what happened.
type Recipient struct {
	ID               uuid.UUID  `json:"id"`
	ContactID        *uuid.UUID `json:"contact_id"`
	OrganizationID   uuid.UUID  `json:"organization_id"`
	OrganizationName string     `json:"organization_name"`
	FullName         string     `json:"full_name"`
	Email            string     `json:"email"`
	Status           string     `json:"status"`
	Error            *string    `json:"error"`
	SentAt           *time.Time `json:"sent_at"`
}

// Mailing is one email sent to several contacts, each receiving their own copy.
type Mailing struct {
	ID         uuid.UUID        `json:"id"`
	Subject    string           `json:"subject"`
	Body       string           `json:"body"`
	Author     string           `json:"author"`
	CreatedAt  time.Time        `json:"created_at"`
	FinishedAt *time.Time       `json:"finished_at"`
	Total      int              `json:"total"`
	Sent       int              `json:"sent"`
	Failed     int              `json:"failed"`
	Products   []MailingProduct `json:"products,omitempty"`
	Recipients []Recipient      `json:"recipients,omitempty"`
}

type mailingInput struct {
	Subject    string   `json:"subject"`
	Body       string   `json:"body"`
	ProductIDs []string `json:"product_ids"`
	ContactIDs []string `json:"contact_ids"`
}

type target struct {
	contactID uuid.UUID
	orgID     uuid.UUID
	name      string
	email     string
}

// MailStatus handles GET /mailings/status so the web can say when email is not set up.
func (h *Handlers) MailStatus(c *gin.Context) {
	response.OK(c, gin.H{"enabled": h.Mail != nil && h.Mail.Enabled(), "from": h.from(), "max_recipients": maxRecipients})
}

func (h *Handlers) from() string {
	if h.Mail == nil {
		return ""
	}
	return h.Mail.From()
}

// Compose builds the text one recipient receives: greeting, the writer's message, the
// products, the writer's name, and how to stop receiving these emails.
func Compose(toName, body string, products []MailingProduct, senderName, senderEmail string) string {
	var b strings.Builder
	b.WriteString("Kính gửi Anh/Chị " + toName + ",\n\n")
	b.WriteString(strings.TrimSpace(body) + "\n")
	if len(products) > 0 {
		b.WriteString("\nSản phẩm giới thiệu:\n")
		for _, p := range products {
			b.WriteString("• " + p.Name)
			var extra []string
			if p.Manufacturer != nil {
				extra = append(extra, "Hãng: "+*p.Manufacturer)
			}
			if p.ManufacturerCode != nil {
				extra = append(extra, "Mã: "+*p.ManufacturerCode)
			}
			if p.origin != nil {
				extra = append(extra, "Xuất xứ: "+*p.origin)
			}
			if len(extra) > 0 {
				b.WriteString(" (" + strings.Join(extra, " · ") + ")")
			}
			b.WriteString("\n")
			if p.description != nil {
				b.WriteString("  " + strings.ReplaceAll(strings.TrimSpace(*p.description), "\n", "\n  ") + "\n")
			}
		}
	}
	b.WriteString("\nTrân trọng,\n" + senderName + "\n" + senderEmail + "\n")
	b.WriteString("\n--\nNếu Anh/Chị không muốn nhận email giới thiệu sản phẩm, vui lòng trả lời email này để chúng tôi dừng gửi.\n")
	return b.String()
}

// prepare validates the input and resolves products and recipients.
func (h *Handlers) prepare(ctx context.Context, q db.Querier, in *mailingInput, needRecipients bool) ([]MailingProduct, []target, error) {
	in.Subject, in.Body = strings.TrimSpace(in.Subject), strings.TrimSpace(in.Body)
	var errs apperrors.FieldErrors
	switch {
	case in.Subject == "":
		errs.Add("subject", "required", nil)
	case len([]rune(in.Subject)) > 200:
		errs.Add("subject", "too_long", map[string]any{"max": 200})
	case strings.ContainsAny(in.Subject, "\r\n"):
		errs.Add("subject", "format", nil)
	}
	if in.Body == "" {
		errs.Add("body", "required", nil)
	} else if len([]rune(in.Body)) > 10000 {
		errs.Add("body", "too_long", map[string]any{"max": 10000})
	}
	if len(in.ProductIDs) > maxMailingProducts {
		errs.Add("product_ids", "too_many", map[string]any{"max": maxMailingProducts})
	}
	if needRecipients && len(in.ContactIDs) == 0 {
		errs.Add("contact_ids", "required", nil)
	} else if len(in.ContactIDs) > maxRecipients {
		errs.Add("contact_ids", "too_many", map[string]any{"max": maxRecipients})
	}
	parse := func(field string, raw []string) []uuid.UUID {
		seen := map[uuid.UUID]bool{}
		out := []uuid.UUID{}
		for _, s := range raw {
			id, err := uuid.Parse(s)
			if err != nil {
				errs.Add(field, "uuid", nil)
				return nil
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return out
	}
	productIDs, contactIDs := parse("product_ids", in.ProductIDs), parse("contact_ids", in.ContactIDs)
	if err := errs.Err(); err != nil {
		return nil, nil, err
	}
	products := []MailingProduct{}
	if len(productIDs) > 0 {
		rows, err := q.Query(ctx, `SELECT p.id, p.name, p.manufacturer, p.manufacturer_code, p.description, p.origin
			FROM products p JOIN unnest($1::uuid[]) WITH ORDINALITY AS want(id, pos) ON want.id = p.id ORDER BY want.pos`, productIDs)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var p MailingProduct
			if err := rows.Scan(&p.ProductID, &p.Name, &p.Manufacturer, &p.ManufacturerCode, &p.description, &p.origin); err != nil {
				return nil, nil, err
			}
			products = append(products, p)
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		if len(products) != len(productIDs) {
			return nil, nil, apperrors.Invalid("product_ids", "not_found", nil)
		}
	}
	targets := []target{}
	if len(contactIDs) > 0 {
		rows, err := q.Query(ctx, `SELECT ct.id, ct.organization_id, ct.full_name, ct.email
			FROM contacts ct JOIN organizations o ON o.id = ct.organization_id
			WHERE ct.id = ANY($1) AND o.is_customer AND ct.email IS NOT NULL AND ct.accepts_email
			ORDER BY o.name, ct.full_name, ct.id`, contactIDs)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		seen := map[string]bool{}
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.contactID, &t.orgID, &t.name, &t.email); err != nil {
				return nil, nil, err
			}
			if !seen[t.email] { // the same person listed under two customers gets one copy
				seen[t.email] = true
				targets = append(targets, t)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		// Every chosen contact lost its address or opted out since the list was loaded.
		if len(targets) == 0 {
			return nil, nil, apperrors.Invalid("contact_ids", "not_emailable", nil)
		}
	}
	return products, targets, nil
}

// PreviewMailing handles POST /mailings/preview: the exact text the first recipient
// would receive. Nothing is saved or sent.
func (h *Handlers) PreviewMailing(c *gin.Context) {
	p := auth.Principal(c)
	var in mailingInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	products, targets, err := h.prepare(c.Request.Context(), h.Pool, &in, false)
	if err != nil {
		response.Fail(c, err)
		return
	}
	name := "…"
	if len(targets) > 0 {
		name = targets[0].name
	}
	response.OK(c, gin.H{"subject": in.Subject, "text": Compose(name, in.Body, products, p.Name, p.Email), "recipients": len(targets), "from": h.from(), "reply_to": p.Email})
}

// CreateMailing handles POST /mailings: stores the email and its recipients, then sends
// each copy in the background. The same Idempotency-Key never sends twice.
func (h *Handlers) CreateMailing(c *gin.Context) {
	p := auth.Principal(c)
	if h.Mail == nil || !h.Mail.Enabled() {
		response.Fail(c, apperrors.Rule("email_disabled", "Email is not set up for this installation.", nil))
		return
	}
	var in mailingInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var created uuid.UUID
	httpx.Command(c, h.Pool, "mailings.create", true, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		products, targets, err := h.prepare(ctx, tx, &in, true)
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO mailings (subject, body, created_by) VALUES ($1, $2, $3) RETURNING id`, in.Subject, in.Body, p.UserID).Scan(&id); err != nil {
			return nil, err
		}
		for i, pr := range products {
			if _, err := tx.Exec(ctx, `INSERT INTO mailing_products (mailing_id, product_id, position, name, manufacturer, manufacturer_code) VALUES ($1, $2, $3, $4, $5, $6)`,
				id, pr.ProductID, i+1, pr.Name, pr.Manufacturer, pr.ManufacturerCode); err != nil {
				return nil, err
			}
		}
		for _, t := range targets {
			if _, err := tx.Exec(ctx, `INSERT INTO mailing_recipients (mailing_id, contact_id, organization_id, full_name, email) VALUES ($1, $2, $3, $4, $5)`,
				id, t.contactID, t.orgID, t.name, t.email); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, "create", "mailing", id, map[string]any{"subject": in.Subject, "recipients": len(targets), "products": len(products)}); err != nil {
			return nil, err
		}
		m, err := h.loadMailing(ctx, tx, id, nil)
		if err != nil {
			return nil, err
		}
		created = id
		return m, nil
	})
	// Only a committed, first-time request reaches here with an id (a replay never runs fn).
	if created != uuid.Nil && c.Writer.Status() == httpx.Created {
		go h.deliver(created, p.Name, p.Email)
	}
}

// deliver sends every queued copy of a mailing, one recipient at a time, and records
// each result. It runs after the request has answered.
func (h *Handlers) deliver(id uuid.UUID, senderName, senderEmail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var subject, body string
	if err := h.Pool.QueryRow(ctx, `SELECT subject, body FROM mailings WHERE id = $1`, id).Scan(&subject, &body); err != nil {
		slog.Error("mailing load failed", "mailing_id", id, "error", err)
		return
	}
	products, err := h.mailingProducts(ctx, h.Pool, id, true)
	if err != nil {
		slog.Error("mailing products failed", "mailing_id", id, "error", err)
		return
	}
	rows, err := h.Pool.Query(ctx, `SELECT id, full_name, email FROM mailing_recipients WHERE mailing_id = $1 AND status = 'queued' ORDER BY id`, id)
	if err != nil {
		slog.Error("mailing recipients failed", "mailing_id", id, "error", err)
		return
	}
	type rcpt struct {
		id          uuid.UUID
		name, email string
	}
	var list []rcpt
	for rows.Next() {
		var r rcpt
		if err := rows.Scan(&r.id, &r.name, &r.email); err == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	for _, r := range list {
		sendCtx, done := context.WithTimeout(ctx, 45*time.Second)
		err := h.Mail.Send(sendCtx, mailer.Message{ToName: r.name, ToEmail: r.email, ReplyToName: senderName, ReplyToEmail: senderEmail,
			Subject: subject, Text: Compose(r.name, body, products, senderName, senderEmail)})
		done()
		if err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			slog.Warn("mailing copy failed", "mailing_id", id, "recipient_id", r.id, "error", msg)
			_, _ = h.Pool.Exec(ctx, `UPDATE mailing_recipients SET status = 'failed', error = $2 WHERE id = $1`, r.id, msg)
			continue
		}
		_, _ = h.Pool.Exec(ctx, `UPDATE mailing_recipients SET status = 'sent', sent_at = now() WHERE id = $1`, r.id)
	}
	_, _ = h.Pool.Exec(ctx, `UPDATE mailings SET finished_at = now() WHERE id = $1`, id)
}

// CloseInterrupted marks copies left queued by a restart as not sent, so the list never
// shows an email as "sending" forever. Called once at start-up.
func (h *Handlers) CloseInterrupted(ctx context.Context) {
	if _, err := h.Pool.Exec(ctx, `WITH stale AS (UPDATE mailings SET finished_at = now() WHERE finished_at IS NULL RETURNING id)
		UPDATE mailing_recipients r SET status = 'failed', error = 'interrupted' FROM stale WHERE r.mailing_id = stale.id AND r.status = 'queued'`); err != nil {
		slog.Warn("closing interrupted mailings failed", "error", err)
	}
}

func (h *Handlers) mailingProducts(ctx context.Context, q db.Querier, id uuid.UUID, withDetails bool) ([]MailingProduct, error) {
	rows, err := q.Query(ctx, `SELECT mp.product_id, mp.name, mp.manufacturer, mp.manufacturer_code, p.description, p.origin
		FROM mailing_products mp JOIN products p ON p.id = mp.product_id WHERE mp.mailing_id = $1 ORDER BY mp.position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailingProduct{}
	for rows.Next() {
		var p MailingProduct
		if err := rows.Scan(&p.ProductID, &p.Name, &p.Manufacturer, &p.ManufacturerCode, &p.description, &p.origin); err != nil {
			return nil, err
		}
		if !withDetails {
			p.description, p.origin = nil, nil
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const mailingSelect = `SELECT m.id, m.subject, m.body, u.full_name, m.created_at, m.finished_at,
	(SELECT count(*) FROM mailing_recipients r WHERE r.mailing_id = m.id),
	(SELECT count(*) FROM mailing_recipients r WHERE r.mailing_id = m.id AND r.status = 'sent'),
	(SELECT count(*) FROM mailing_recipients r WHERE r.mailing_id = m.id AND r.status = 'failed')
	FROM mailings m JOIN users u ON u.id = m.created_by`

func scanMailing(row pgx.Row, m *Mailing, extra ...any) error {
	return row.Scan(append([]any{&m.ID, &m.Subject, &m.Body, &m.Author, &m.CreatedAt, &m.FinishedAt, &m.Total, &m.Sent, &m.Failed}, extra...)...)
}

// loadMailing returns a mailing with products and recipients; owner limits it to the
// writer's own (sales), nil shows any (leaders, accounting).
func (h *Handlers) loadMailing(ctx context.Context, q db.Querier, id uuid.UUID, owner *uuid.UUID) (*Mailing, error) {
	var m Mailing
	if err := scanMailing(q.QueryRow(ctx, mailingSelect+` WHERE m.id = $1 AND ($2::uuid IS NULL OR m.created_by = $2)`, id, owner), &m); err != nil {
		return nil, err
	}
	var err error
	if m.Products, err = h.mailingProducts(ctx, q, id, false); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT r.id, r.contact_id, r.organization_id, o.name, r.full_name, r.email, r.status, r.error, r.sent_at
		FROM mailing_recipients r JOIN organizations o ON o.id = r.organization_id WHERE r.mailing_id = $1 ORDER BY o.name, r.full_name, r.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m.Recipients = []Recipient{}
	for rows.Next() {
		var r Recipient
		if err := rows.Scan(&r.ID, &r.ContactID, &r.OrganizationID, &r.OrganizationName, &r.FullName, &r.Email, &r.Status, &r.Error, &r.SentAt); err != nil {
			return nil, err
		}
		m.Recipients = append(m.Recipients, r)
	}
	return &m, rows.Err()
}

// GetMailing handles GET /mailings/:id.
func (h *Handlers) GetMailing(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	m, err := h.loadMailing(c.Request.Context(), h.Pool, id, auth.Principal(c).OwnerFilter())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, m)
}

// ListMailings handles GET /mailings: newest first; sales see their own.
func (h *Handlers) ListMailings(c *gin.Context) {
	page, size := httpx.Page(c)
	rows, err := h.Pool.Query(c.Request.Context(), mailingSelect+` WHERE ($1::uuid IS NULL OR m.created_by = $1)
		ORDER BY m.created_at DESC, m.id LIMIT $2 OFFSET $3`, auth.Principal(c).OwnerFilter(), size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []Mailing{}
	for rows.Next() {
		var m Mailing
		if err := scanMailing(rows, &m); err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	var total int
	if err := h.Pool.QueryRow(c.Request.Context(), `SELECT count(*) FROM mailings m WHERE ($1::uuid IS NULL OR m.created_by = $1)`, auth.Principal(c).OwnerFilter()).Scan(&total); err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, page, size, total)
}
