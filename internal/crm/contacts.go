package crm

import (
	"context"
	"net/mail"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/db"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// Contact is a person at a customer or supplier. A partner can have many.
type Contact struct {
	ID               uuid.UUID `json:"id"`
	OrganizationID   uuid.UUID `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	FullName         string    `json:"full_name"`
	JobTitle         *string   `json:"job_title"`
	Email            *string   `json:"email"`
	Phone            *string   `json:"phone"`
	Note             *string   `json:"note"`
	IsPrimary        bool      `json:"is_primary"`
	AcceptsEmail     bool      `json:"accepts_email"`
}

type contactInput struct {
	FullName     string  `json:"full_name"`
	JobTitle     *string `json:"job_title"`
	Email        *string `json:"email"`
	Phone        *string `json:"phone"`
	Note         *string `json:"note"`
	IsPrimary    bool    `json:"is_primary"`
	AcceptsEmail *bool   `json:"accepts_email"`
}

func trimOpt(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

func (in *contactInput) validate() error {
	in.FullName = strings.TrimSpace(in.FullName)
	in.JobTitle, in.Email, in.Phone, in.Note = trimOpt(in.JobTitle), trimOpt(in.Email), trimOpt(in.Phone), trimOpt(in.Note)
	var errs apperrors.FieldErrors
	if in.FullName == "" {
		errs.Add("full_name", "required", nil)
	} else if len([]rune(in.FullName)) > 200 {
		errs.Add("full_name", "too_long", map[string]any{"max": 200})
	}
	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{{"job_title", in.JobTitle, 120}, {"phone", in.Phone, 40}, {"note", in.Note, 500}} {
		if f.v != nil && len([]rune(*f.v)) > f.max {
			errs.Add(f.name, "too_long", map[string]any{"max": f.max})
		}
	}
	if in.Email != nil {
		a, err := mail.ParseAddress(*in.Email)
		// A bare address only: no display name, no spaces, and a dot in the domain.
		if err != nil || a.Address != *in.Email || len(*in.Email) > 254 || !strings.Contains((*in.Email)[strings.LastIndex(*in.Email, "@")+1:], ".") {
			errs.Add("email", "email", nil)
		} else {
			lower := strings.ToLower(*in.Email)
			in.Email = &lower
		}
	}
	return errs.Err()
}

const contactSelect = `SELECT ct.id, ct.organization_id, o.name, ct.full_name, ct.job_title, ct.email, ct.phone, ct.note, ct.is_primary, ct.accepts_email
	FROM contacts ct JOIN organizations o ON o.id = ct.organization_id`

func scanContacts(rows pgx.Rows) ([]Contact, error) {
	defer rows.Close()
	out := []Contact{}
	for rows.Next() {
		var ct Contact
		if err := rows.Scan(&ct.ID, &ct.OrganizationID, &ct.OrganizationName, &ct.FullName, &ct.JobTitle, &ct.Email, &ct.Phone, &ct.Note, &ct.IsPrimary, &ct.AcceptsEmail); err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	return out, rows.Err()
}

func loadContact(ctx context.Context, q db.Querier, id uuid.UUID) (*Contact, error) {
	rows, err := q.Query(ctx, contactSelect+` WHERE ct.id = $1`, id)
	if err != nil {
		return nil, err
	}
	list, err := scanContacts(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, apperrors.NotFound()
	}
	return &list[0], nil
}

// ListContacts handles GET /organizations/:id/contacts: the main contact first, then by name.
func (h *Handlers) ListContacts(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	rows, err := h.Pool.Query(c.Request.Context(), contactSelect+` WHERE ct.organization_id = $1 ORDER BY ct.is_primary DESC, ct.full_name, ct.id`, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	list, err := scanContacts(rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, list)
}

// ListEmailContacts handles GET /contacts/emailable: customer contacts that have an
// address and have not asked to stop receiving product news — who a mailing may go to.
func (h *Handlers) ListEmailContacts(c *gin.Context) {
	rows, err := h.Pool.Query(c.Request.Context(), contactSelect+` WHERE o.is_customer AND ct.email IS NOT NULL AND ct.accepts_email
		ORDER BY o.name, ct.is_primary DESC, ct.full_name, ct.id LIMIT 2000`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	list, err := scanContacts(rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, list)
}

func contactWriteErr(err error) error {
	if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
		return apperrors.Invalid("email", "taken", nil)
	}
	return err
}

// CreateContact handles POST /organizations/:id/contacts. The first contact of a
// partner becomes the main one; marking another as main moves the mark.
func (h *Handlers) CreateContact(c *gin.Context) {
	orgID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in contactInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contacts.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var existing int
		// Lock the partner so two people adding contacts at once agree on who is the main one.
		if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id = $1 FOR UPDATE`, orgID).Scan(new(uuid.UUID)); err != nil {
			return nil, err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM contacts WHERE organization_id = $1`, orgID).Scan(&existing); err != nil {
			return nil, err
		}
		primary := in.IsPrimary || existing == 0
		if primary {
			if _, err := tx.Exec(ctx, `UPDATE contacts SET is_primary = false, updated_at = now() WHERE organization_id = $1 AND is_primary`, orgID); err != nil {
				return nil, err
			}
		}
		accepts := in.AcceptsEmail == nil || *in.AcceptsEmail
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO contacts (organization_id, full_name, job_title, email, phone, note, is_primary, accepts_email)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`, orgID, in.FullName, in.JobTitle, in.Email, in.Phone, in.Note, primary, accepts).Scan(&id); err != nil {
			return nil, contactWriteErr(err)
		}
		ct, err := loadContact(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		return ct, audit.Record(ctx, tx, "create", "contact", id, map[string]any{"organization_id": orgID, "full_name": ct.FullName, "is_primary": primary})
	})
}

// UpdateContact handles PATCH /contacts/:id (the whole contact is sent).
func (h *Handlers) UpdateContact(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in contactInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contacts.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var orgID uuid.UUID
		var wasPrimary, accepted bool
		if err := tx.QueryRow(ctx, `SELECT organization_id, is_primary, accepts_email FROM contacts WHERE id = $1 FOR UPDATE`, id).Scan(&orgID, &wasPrimary, &accepted); err != nil {
			return nil, err
		}
		if in.IsPrimary && !wasPrimary {
			if _, err := tx.Exec(ctx, `UPDATE contacts SET is_primary = false, updated_at = now() WHERE organization_id = $1 AND is_primary`, orgID); err != nil {
				return nil, err
			}
		}
		accepts := accepted
		if in.AcceptsEmail != nil {
			accepts = *in.AcceptsEmail
		}
		if _, err := tx.Exec(ctx, `UPDATE contacts SET full_name = $2, job_title = $3, email = $4, phone = $5, note = $6, is_primary = $7, accepts_email = $8,
			version = version + 1, updated_at = now() WHERE id = $1`, id, in.FullName, in.JobTitle, in.Email, in.Phone, in.Note, in.IsPrimary, accepts); err != nil {
			return nil, contactWriteErr(err)
		}
		ct, err := loadContact(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		return ct, audit.Record(ctx, tx, "update", "contact", id, map[string]any{"full_name": ct.FullName, "is_primary": ct.IsPrimary, "accepts_email": ct.AcceptsEmail})
	})
}

// DeleteContact handles DELETE /contacts/:id. Emails already sent keep the name and
// address they went to.
func (h *Handlers) DeleteContact(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "contacts.delete", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var orgID uuid.UUID
		var name string
		if err := tx.QueryRow(ctx, `DELETE FROM contacts WHERE id = $1 RETURNING organization_id, full_name`, id).Scan(&orgID, &name); err != nil {
			return nil, err
		}
		return gin.H{"id": id}, audit.Record(ctx, tx, "delete", "contact", id, map[string]any{"organization_id": orgID, "full_name": name})
	})
}
