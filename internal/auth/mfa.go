package auth

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/db"
	"navo-api/internal/httpx"
	"navo-api/internal/reqctx"
	"navo-api/internal/response"
)

const recoveryCodeCount = 8

// enrollmentAllowed lists what an admin who must still turn on two-step sign-in may do.
func enrollmentAllowed(method, route string) bool {
	switch method + " " + route {
	case "GET /api/v1/me", "POST /api/v1/auth/logout", "POST /api/v1/me/password",
		"POST /api/v1/me/mfa/setup", "POST /api/v1/me/mfa/enable":
		return true
	}
	return false
}

// secondFactor checks the code when the account has two-step sign-in on. It runs after the
// password matched, and is rate-limited with it.
func (s *Service) secondFactor(ctx context.Context, userID uuid.UUID, code, recovery string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var secret *string
		var last int64
		if err := tx.QueryRow(ctx, `SELECT totp_secret, totp_last_step FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&secret, &last); err != nil {
			return err
		}
		if secret == nil {
			return nil
		}
		code, recovery = strings.TrimSpace(code), strings.TrimSpace(recovery)
		switch {
		case code != "":
			step, ok := verifyTOTP(*secret, code, time.Now(), last)
			if !ok {
				return apperrors.Rule("invalid_mfa_code", "The code is wrong or has expired.", nil)
			}
			_, err := tx.Exec(ctx, `UPDATE users SET totp_last_step = $2 WHERE id = $1`, userID, step)
			return err
		case recovery != "":
			tag, err := tx.Exec(ctx, `UPDATE user_recovery_codes SET used_at = now() WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, hashRecovery(recovery))
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apperrors.Rule("invalid_mfa_code", "The code is wrong or has expired.", nil)
			}
			ctx = reqctx.WithActor(ctx, userID)
			return audit.Record(ctx, tx, "use_recovery_code", "user", userID, nil)
		default:
			return apperrors.Rule("mfa_required", "Enter the code from your authenticator app.", nil)
		}
	})
}

// SetupMFA handles POST /me/mfa/setup: a new secret to add to an authenticator app. Nothing
// changes until a code from it is confirmed with /me/mfa/enable.
func (s *Service) SetupMFA(c *gin.Context) {
	p := Principal(c)
	if p.MFAEnabled {
		response.Fail(c, apperrors.Rule("mfa_already_enabled", "Two-step sign-in is already on.", nil))
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		response.Fail(c, err)
		return
	}
	if _, err := s.Pool.Exec(c.Request.Context(), `UPDATE users SET totp_pending = $2 WHERE id = $1`, p.UserID, secret); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"secret": secret, "uri": totpURI(secret, p.Email)})
}

type mfaInput struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

// EnableMFA handles POST /me/mfa/enable {code}: turns two-step sign-in on and returns the
// recovery codes, shown once.
func (s *Service) EnableMFA(c *gin.Context) {
	p := Principal(c)
	var in mfaInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var codes []string
	err := db.InTx(c.Request.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := c.Request.Context()
		var pending *string
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT totp_pending, totp_enabled_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`, p.UserID).Scan(&pending, &enabled); err != nil {
			return err
		}
		if enabled {
			return apperrors.Rule("mfa_already_enabled", "Two-step sign-in is already on.", nil)
		}
		if pending == nil {
			return apperrors.Rule("mfa_not_set_up", "Start the setup again.", nil)
		}
		step, ok := verifyTOTP(*pending, in.Code, time.Now(), 0)
		if !ok {
			return apperrors.Invalid("code", "invalid_mfa_code", nil)
		}
		var hashes [][]byte
		var err error
		if codes, hashes, err = newRecoveryCodes(recoveryCodeCount); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET totp_secret = totp_pending, totp_pending = NULL, totp_enabled_at = now(), totp_last_step = $2,
			version = version + 1 WHERE id = $1`, p.UserID, step); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = $1`, p.UserID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.Exec(ctx, `INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, p.UserID, h); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, "enable_mfa", "user", p.UserID, nil)
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"recovery_codes": codes})
}

// DisableMFA handles POST /me/mfa/disable {password, code}. When admins must use two-step
// sign-in, an admin cannot turn it off; another admin resets it if the phone is lost.
func (s *Service) DisableMFA(c *gin.Context) {
	p := Principal(c)
	var in mfaInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	if s.RequireAdminMFA && p.HasRole(access.Admin) {
		response.Fail(c, apperrors.Rule("mfa_required_for_admin", "Administrators must keep two-step sign-in on.", nil))
		return
	}
	err := db.InTx(c.Request.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := c.Request.Context()
		var hash string
		var secret *string
		var last int64
		if err := tx.QueryRow(ctx, `SELECT password_hash, totp_secret, totp_last_step FROM users WHERE id = $1 FOR UPDATE`, p.UserID).Scan(&hash, &secret, &last); err != nil {
			return err
		}
		if secret == nil {
			return apperrors.Rule("mfa_not_enabled", "Two-step sign-in is off.", nil)
		}
		var errs apperrors.FieldErrors
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
			errs.Add("password", "wrong_password", nil)
		}
		if _, ok := verifyTOTP(*secret, in.Code, time.Now(), last); !ok {
			errs.Add("code", "invalid_mfa_code", nil)
		}
		if err := errs.Err(); err != nil {
			return err
		}
		if err := clearMFA(ctx, tx, p.UserID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, "disable_mfa", "user", p.UserID, nil)
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"mfa_enabled": false})
}

// ResetMFA handles POST /users/:id/mfa/reset (users.manage): for a lost phone. Another
// administrator must do it, and the person's sessions end.
func (s *Service) ResetMFA(c *gin.Context) {
	p := Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if id == p.UserID {
		response.Fail(c, apperrors.Rule("mfa_reset_self", "Another administrator must reset your two-step sign-in.", nil))
		return
	}
	httpx.Command(c, s.Pool, "users.mfa_reset", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, id); err != nil {
			return nil, err
		}
		if err := clearMFA(ctx, tx, id); err != nil {
			return nil, err
		}
		if err := RevokeSessions(ctx, tx, id, ""); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "mfa_enabled": false}, audit.Record(ctx, tx, "reset_mfa", "user", id, nil)
	})
}

func clearMFA(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_enabled_at = NULL, version = version + 1 WHERE id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = $1`, id)
	return err
}
