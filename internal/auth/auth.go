// Package auth signs users in and resolves the Principal for each request.
// Web uses an HttpOnly cookie; mobile sends the same opaque token as a Bearer
// header and keeps it in secure storage. Only the token's SHA-256 is stored.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/httpx"
	"navo-api/internal/reqctx"
	"navo-api/internal/response"
)

// CookieName is the web session cookie.
const CookieName = "navo_session"

// ClientHeader must accompany cookie-authenticated writes. Browsers cannot
// send a custom header cross-site without a CORS preflight we do not allow,
// so this blocks CSRF.
const ClientHeader = "X-Navo-Client"

const principalKey = "navo.principal"

// HashPassword hashes a new password.
func HashPassword(password string) (string, error) {
	if l := len(password); l < 10 || l > 72 {
		return "", apperrors.Invalid("password", "length", map[string]any{"min": 10, "max": 72})
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(h), err
}

// Service holds auth dependencies.
type Service struct {
	Pool         *pgxpool.Pool
	TTL          time.Duration
	CookieSecure bool
	limiter      *limiter
}

// NewService builds the service.
func NewService(pool *pgxpool.Pool, ttl time.Duration, cookieSecure bool) *Service {
	return &Service{Pool: pool, TTL: ttl, CookieSecure: cookieSecure, limiter: newLimiter(10, time.Minute)}
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// dummyHash keeps login timing similar whether or not the email exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("navo-timing-equaliser"), 12)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Client   string `json:"client"`
}

// Login handles POST /auth/login.
func (s *Service) Login(c *gin.Context) {
	if !s.limiter.allow(c.ClientIP()) {
		response.Fail(c, apperrors.TooManyRequests())
		return
	}
	var req loginRequest
	if err := httpx.Decode(c, &req); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	if strings.TrimSpace(req.Email) == "" {
		errs.Add("email", "required", nil)
	}
	if req.Password == "" {
		errs.Add("password", "required", nil)
	}
	if req.Client == "" {
		req.Client = "web"
	}
	if req.Client != "web" && req.Client != "mobile" {
		errs.Add("client", "one_of", map[string]any{"values": []string{"web", "mobile"}})
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	ctx := c.Request.Context()
	var userID uuid.UUID
	var hash string
	err := s.Pool.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE lower(email) = lower($1) AND is_active`,
		strings.TrimSpace(req.Email)).Scan(&userID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		response.Fail(c, apperrors.Rule("invalid_credentials", "Email or password is incorrect.", nil))
		return
	}
	if err != nil {
		response.Fail(c, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		response.Fail(c, apperrors.Rule("invalid_credentials", "Email or password is incorrect.", nil))
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		response.Fail(c, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(s.TTL)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, client, expires_at) VALUES ($1, $2, $3, $4)`,
		userID, hashToken(token), req.Client, expires); err != nil {
		response.Fail(c, err)
		return
	}
	principal, err := s.loadPrincipal(ctx, userID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	body := gin.H{"user": principal, "permissions": principal.Permissions(), "expires_at": expires}
	if req.Client == "mobile" {
		body["token"] = token
	} else {
		http.SetCookie(c.Writer, &http.Cookie{Name: CookieName, Value: token, Path: "/", Expires: expires,
			HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteStrictMode})
	}
	response.OK(c, body)
}

// Logout handles POST /auth/logout by revoking the current session.
func (s *Service) Logout(c *gin.Context) {
	if token := tokenFrom(c); token != "" {
		if _, err := s.Pool.Exec(c.Request.Context(), `UPDATE sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, hashToken(token)); err != nil {
			response.Fail(c, err)
			return
		}
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteStrictMode})
	response.OK(c, gin.H{"signed_out": true})
}

// Me handles GET /me.
func (s *Service) Me(c *gin.Context) {
	p := Principal(c)
	response.OK(c, gin.H{"user": p, "permissions": p.Permissions()})
}

// Authenticate resolves the Principal from the cookie or Bearer token on
// every request, so role and grant changes apply immediately (AC-22).
func (s *Service) Authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := tokenFrom(c)
		if token == "" {
			response.Fail(c, apperrors.Unauthorized())
			return
		}
		usesCookie := !strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ")
		if usesCookie && c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.GetHeader(ClientHeader) == "" {
			response.Fail(c, apperrors.Forbidden("csrf_header_missing"))
			return
		}
		ctx := c.Request.Context()
		var userID uuid.UUID
		err := s.Pool.QueryRow(ctx, `SELECT s.user_id FROM sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now() AND u.is_active`, hashToken(token)).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, apperrors.Unauthorized())
			return
		}
		if err != nil {
			response.Fail(c, err)
			return
		}
		p, err := s.loadPrincipal(ctx, userID)
		if err != nil {
			response.Fail(c, err)
			return
		}
		c.Set(principalKey, p)
		perms := p.Permissions()
		sort.Slice(perms, func(i, j int) bool { return perms[i] < perms[j] })
		encoded, _ := json.Marshal([]any{p.Version, perms, p.OwnerFilter()})
		fingerprint := sha256.Sum256(encoded)
		c.Request = c.Request.WithContext(reqctx.WithAuthorization(reqctx.WithActor(ctx, userID), fingerprint[:]))
		c.Next()
	}
}

func (s *Service) loadPrincipal(ctx context.Context, userID uuid.UUID) (*access.Principal, error) {
	var name, email string
	var roles, grants []string
	var version int64
	err := s.Pool.QueryRow(ctx, `SELECT u.full_name, u.email, u.version,
			coalesce((SELECT array_agg(role ORDER BY role) FROM user_roles WHERE user_id = u.id), '{}'),
			coalesce((SELECT array_agg(grant_code ORDER BY grant_code) FROM user_grants WHERE user_id = u.id), '{}')
		FROM users u WHERE u.id = $1`, userID).Scan(&name, &email, &version, &roles, &grants)
	if err != nil {
		return nil, err
	}
	rs := make([]access.Role, len(roles))
	for i, r := range roles {
		rs[i] = access.Role(r)
	}
	p := access.NewPrincipal(userID, name, email, rs, grants)
	p.Version = version
	return p, nil
}

// Principal returns the signed-in user; Authenticate must have run.
func Principal(c *gin.Context) *access.Principal {
	p, _ := c.Get(principalKey)
	principal, _ := p.(*access.Principal)
	return principal
}

// Require refuses requests from users without perm.
func Require(perm access.Permission) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Principal(c).Can(perm) {
			response.Fail(c, apperrors.Forbidden(string(perm)))
			return
		}
		c.Next()
	}
}

func tokenFrom(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if ck, err := c.Cookie(CookieName); err == nil {
		return ck
	}
	return ""
}

// limiter is a fixed-window counter per client IP; enough for one instance (TECHNICAL_DESIGN 8.1).
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*window
}

type window struct {
	start time.Time
	count int
}

func newLimiter(max int, w time.Duration) *limiter {
	return &limiter{max: max, window: w, hits: map[string]*window{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 10_000 {
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) > l.window {
		l.hits[key] = &window{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.max
}

// ValidatePassword applies the password rule to one field (10–72 bytes; bcrypt's limit).
func ValidatePassword(field, password string) error {
	if l := len(password); l < 10 || l > 72 {
		return apperrors.Invalid(field, "length", map[string]any{"min": 10, "max": 72})
	}
	return nil
}

// SetPassword stores a new password hash for an existing user.
func SetPassword(ctx context.Context, tx pgx.Tx, userID uuid.UUID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RevokeSessions ends a user's sessions, except the one whose token is keepToken.
func RevokeSessions(ctx context.Context, tx pgx.Tx, userID uuid.UUID, keepToken string) error {
	var keep []byte
	if keepToken != "" {
		keep = hashToken(keepToken)
	}
	_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL AND ($2::bytea IS NULL OR token_hash <> $2)`, userID, keep)
	return err
}

type changePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword handles POST /me/password: the current password is required, and
// the user's other sessions (other browsers, the phone) end.
func (s *Service) ChangePassword(c *gin.Context) {
	p := Principal(c)
	if !s.limiter.allow("pw:" + p.UserID.String()) {
		response.Fail(c, apperrors.TooManyRequests())
		return
	}
	var in changePasswordInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	if in.CurrentPassword == "" {
		errs.Add("current_password", "required", nil)
	}
	if l := len(in.NewPassword); l < 10 || l > 72 {
		errs.Add("new_password", "length", map[string]any{"min": 10, "max": 72})
	} else if in.NewPassword == in.CurrentPassword {
		errs.Add("new_password", "same_as_current", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, s.Pool, "me.password", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var hash string
		if err := tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1 FOR UPDATE`, p.UserID).Scan(&hash); err != nil {
			return nil, err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.CurrentPassword)) != nil {
			return nil, apperrors.Invalid("current_password", "incorrect", nil)
		}
		if err := SetPassword(ctx, tx, p.UserID, in.NewPassword); err != nil {
			return nil, err
		}
		if err := RevokeSessions(ctx, tx, p.UserID, tokenFrom(c)); err != nil {
			return nil, err
		}
		return gin.H{"password_changed": true}, audit.Record(ctx, tx, "change_password", "user", p.UserID, nil)
	})
}
