// Package users lets Admin create accounts and manage roles and grants (N-01).
package users

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// Handlers serve user management.
type Handlers struct{ Pool *pgxpool.Pool }

// User is an account as Admin sees it (never the password hash).
type User struct {
	ID       uuid.UUID `json:"id"`
	Email    string    `json:"email"`
	FullName string    `json:"full_name"`
	IsActive bool      `json:"is_active"`
	Roles    []string  `json:"roles"`
	Grants   []string  `json:"grants"`
	// LastSignInAt is when the user last signed in (any device), nil if never.
	LastSignInAt *time.Time `json:"last_sign_in_at"`
}

// Input creates a user.
type Input struct {
	Email    string   `json:"email"`
	FullName string   `json:"full_name"`
	Password string   `json:"password"`
	Roles    []string `json:"roles"`
	Grants   []string `json:"grants"`
}

// Validate checks an input; shared by the API and the CLI.
func (in *Input) Validate() error {
	in.Email, in.FullName = strings.TrimSpace(in.Email), strings.TrimSpace(in.FullName)
	var errs apperrors.FieldErrors
	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email || len(in.Email) > 254 {
		errs.Add("email", "email", nil)
	}
	if in.FullName == "" {
		errs.Add("full_name", "required", nil)
	} else if len([]rune(in.FullName)) > 200 {
		errs.Add("full_name", "too_long", map[string]any{"max": 200})
	}
	if l := len(in.Password); l < 10 || l > 72 {
		errs.Add("password", "length", map[string]any{"min": 10, "max": 72})
	}
	if len(in.Roles) == 0 {
		errs.Add("roles", "required", map[string]any{"min": 1})
	}
	for _, r := range in.Roles {
		if !access.ValidRole(r) {
			errs.Add("roles", "one_of", map[string]any{"values": []string{"sales", "leader", "accounting", "admin"}})
			break
		}
	}
	for _, g := range in.Grants {
		if !access.Grantable[access.Permission(g)] {
			errs.Add("grants", "not_grantable", map[string]any{"grant": g})
		}
	}
	return errs.Err()
}

// Create inserts the user with roles and grants and audits it, in tx.
func Create(ctx context.Context, tx pgx.Tx, in Input, grantedBy *uuid.UUID) (uuid.UUID, error) {
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO users (email, full_name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
		in.Email, in.FullName, hash).Scan(&id); err != nil {
		if appErr := apperrors.From(err); appErr.Reason == "duplicate" {
			return uuid.Nil, apperrors.Invalid("email", "taken", nil)
		}
		return uuid.Nil, err
	}
	for _, r := range in.Roles {
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, r); err != nil {
			return uuid.Nil, err
		}
	}
	for _, g := range in.Grants {
		if _, err := tx.Exec(ctx, `INSERT INTO user_grants (user_id, grant_code, granted_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id, g, grantedBy); err != nil {
			return uuid.Nil, err
		}
	}
	return id, audit.Record(ctx, tx, "create", "user", id, map[string]any{"email": in.Email, "full_name": in.FullName, "roles": in.Roles, "grants": in.Grants})
}

// CreateUser handles POST /users.
func (h *Handlers) CreateUser(c *gin.Context) {
	var in Input
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.Validate(); err != nil {
		response.Fail(c, err)
		return
	}
	p := auth.Principal(c)
	// Admin cannot hand out finance or purchase-price access by default (REQUIREMENTS 2);
	// granting those needs the permission itself.
	for _, g := range in.Grants {
		if !p.Can(access.Permission(g)) {
			response.Fail(c, apperrors.Forbidden("grant_"+g))
			return
		}
	}
	httpx.Command(c, h.Pool, "users.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		id, err := Create(c.Request.Context(), tx, in, &p.UserID)
		if err != nil {
			return nil, err
		}
		return User{ID: id, Email: in.Email, FullName: in.FullName, IsActive: true, Roles: in.Roles, Grants: nonNil(in.Grants)}, nil
	})
}

// ListUsers handles GET /users.
func (h *Handlers) ListUsers(c *gin.Context) {
	page, size := httpx.Page(c)
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT u.id, u.email, u.full_name, u.is_active,
			coalesce((SELECT array_agg(role ORDER BY role) FROM user_roles WHERE user_id = u.id), '{}'),
			coalesce((SELECT array_agg(grant_code ORDER BY grant_code) FROM user_grants WHERE user_id = u.id), '{}'),
			(SELECT max(created_at) FROM sessions WHERE user_id = u.id),
			count(*) OVER ()
		FROM users u ORDER BY u.full_name, u.id LIMIT $1 OFFSET $2`, size, (page-1)*size)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []User{}
	total := 0
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.FullName, &u.IsActive, &u.Roles, &u.Grants, &u.LastSignInAt, &total); err != nil {
			response.Fail(c, err)
			return
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, err)
		return
	}
	response.List(c, items, page, size, total)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// UpdateInput replaces a user's name, roles, grants and active flag (email is the
// sign-in identity and stays).
type UpdateInput struct {
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
	Grants   []string `json:"grants"`
	IsActive bool     `json:"is_active"`
}

func (in *UpdateInput) validate() error {
	probe := Input{Email: "probe@navo.local", FullName: in.FullName, Password: "0123456789", Roles: in.Roles, Grants: in.Grants}
	err := probe.Validate()
	in.FullName = probe.FullName
	return err
}

// UpdateUser handles PATCH /users/:id. Admin cannot lock themselves out (deactivate
// or drop their own admin role), may only add grants they hold themselves, and a
// deactivated user's sessions end at once.
func (h *Handlers) UpdateUser(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in UpdateInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := in.validate(); err != nil {
		response.Fail(c, err)
		return
	}
	if id == p.UserID && (!in.IsActive || !contains(in.Roles, "admin")) {
		response.Fail(c, apperrors.Rule("cannot_lock_out_self", "You cannot deactivate yourself or remove your own Admin role.", nil))
		return
	}
	httpx.Command(c, h.Pool, "users.update", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var u User
		if err := tx.QueryRow(ctx, `SELECT id, email, full_name, is_active,
				coalesce((SELECT array_agg(role ORDER BY role) FROM user_roles WHERE user_id = u.id), '{}'),
				coalesce((SELECT array_agg(grant_code ORDER BY grant_code) FROM user_grants WHERE user_id = u.id), '{}')
			FROM users u WHERE id = $1 FOR UPDATE`, id).Scan(&u.ID, &u.Email, &u.FullName, &u.IsActive, &u.Roles, &u.Grants); err != nil {
			return nil, err
		}
		for _, g := range in.Grants {
			if !contains(u.Grants, g) && !p.Can(access.Permission(g)) {
				return nil, apperrors.Forbidden("grant_" + g)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET full_name = $2, is_active = $3, version = version + 1, updated_at = now() WHERE id = $1`, id, in.FullName, in.IsActive); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND NOT (role = ANY($2))`, id, in.Roles); err != nil {
			return nil, err
		}
		for _, r := range in.Roles {
			if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, r); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_grants WHERE user_id = $1 AND NOT (grant_code = ANY($2))`, id, nonNil(in.Grants)); err != nil {
			return nil, err
		}
		for _, g := range in.Grants {
			if _, err := tx.Exec(ctx, `INSERT INTO user_grants (user_id, grant_code, granted_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id, g, p.UserID); err != nil {
				return nil, err
			}
		}
		if !in.IsActive {
			if err := auth.RevokeSessions(ctx, tx, id, ""); err != nil {
				return nil, err
			}
		}
		changes := map[string]any{"full_name": []string{u.FullName, in.FullName}, "roles": []any{u.Roles, in.Roles},
			"grants": []any{u.Grants, nonNil(in.Grants)}, "is_active": []bool{u.IsActive, in.IsActive}}
		return User{ID: id, Email: u.Email, FullName: in.FullName, IsActive: in.IsActive, Roles: in.Roles, Grants: nonNil(in.Grants)},
			audit.Record(ctx, tx, "update", "user", id, changes)
	})
}

type passwordInput struct {
	Password string `json:"password"`
}

// ResetPassword handles POST /users/:id/password: Admin sets a temporary password
// (told to the user in person) and every session of that user ends.
func (h *Handlers) ResetPassword(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in passwordInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if err := auth.ValidatePassword("password", in.Password); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "users.reset_password", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if err := auth.SetPassword(ctx, tx, id, in.Password); err != nil {
			return nil, err
		}
		if err := auth.RevokeSessions(ctx, tx, id, ""); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "password_reset": true}, audit.Record(ctx, tx, "reset_password", "user", id, nil)
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
