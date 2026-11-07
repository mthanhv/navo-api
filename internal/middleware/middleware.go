// Package middleware adds the request id, access logs and panic recovery.
package middleware

import (
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"navo-api/internal/apperrors"
	"navo-api/internal/reqctx"
	"navo-api/internal/response"
)

// RequestID keeps a well-formed X-Request-ID from the client or generates one,
// echoes it in the response header and puts it on the request context.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := reqctx.NormalizeRequestID(c.GetHeader("X-Request-ID"))
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(reqctx.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

// AccessLog writes one structured line per request. It never logs bodies,
// tokens or auth headers.
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		ctx := c.Request.Context()
		slog.InfoContext(ctx, "request", "request_id", reqctx.RequestID(ctx), "user_id", reqctx.Actor(ctx),
			"method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds())
	}
}

// Recover turns a panic into the standard 500 envelope.
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				ctx := c.Request.Context()
				slog.ErrorContext(ctx, "panic", "request_id", reqctx.RequestID(ctx), "panic", r)
				response.Fail(c, &apperrors.Error{Status: http.StatusInternalServerError, Code: apperrors.CodeInternal, Message: "Something went wrong. Please try again."})
			}
		}()
		c.Next()
	}
}

// CORS allows only configured origins. Credentials are allowed so the web app
// can send its cookie; the custom client header is required on writes (auth.ClientHeader).
func CORS(origins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && slices.Contains(origins, origin) {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, X-Navo-Client, Idempotency-Key")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Expose-Headers", "X-Request-ID, Idempotent-Replayed")
			h.Add("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
