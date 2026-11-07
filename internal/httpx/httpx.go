// Package httpx holds request helpers shared by every module: strict JSON
// decoding, pagination, IDs, dates and the idempotent command runner.
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/apperrors"
	"navo-api/internal/db"
	"navo-api/internal/idempotency"
	"navo-api/internal/reqctx"
	"navo-api/internal/response"
)

const maxBody = 1 << 20 // 1 MiB of JSON is far beyond any form

// Body reads the request body once (needed for idempotency hashing).
func Body(c *gin.Context) ([]byte, error) {
	if b, ok := c.Get("navo.body"); ok {
		return b.([]byte), nil
	}
	b, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, apperrors.Invalid("body", "too_large", map[string]any{"max_bytes": maxBody})
	}
	c.Set("navo.body", b)
	return b, nil
}

// Decode parses JSON strictly: unknown or read-only fields are refused, never ignored.
func Decode(c *gin.Context, dst any) error {
	b, err := Body(c)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return apperrors.Invalid(typeErr.Field, "type", map[string]any{"expected": typeErr.Type.String()})
		}
		return apperrors.Invalid("body", "invalid_json", map[string]any{"detail": err.Error()})
	}
	return nil
}

// Page reads page and page_size (1..100, default 25).
func Page(c *gin.Context) (page, size int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ = strconv.Atoi(c.DefaultQuery("page_size", "25"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 25
	}
	return page, size
}

// PathID parses a UUID path parameter; a malformed id is simply not found.
func PathID(c *gin.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, apperrors.NotFound()
	}
	return id, nil
}

// UUID parses a required UUID field.
func UUID(errs *apperrors.FieldErrors, field, raw string) uuid.UUID {
	if raw == "" {
		errs.Add(field, "required", nil)
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		errs.Add(field, "uuid", nil)
	}
	return id
}

// Date parses an optional YYYY-MM-DD field.
func Date(errs *apperrors.FieldErrors, field string, raw *string) *time.Time {
	if raw == nil || *raw == "" {
		return nil
	}
	t, err := time.Parse(time.DateOnly, *raw)
	if err != nil {
		errs.Add(field, "date", map[string]any{"format": "YYYY-MM-DD"})
		return nil
	}
	return &t
}

// Command runs a write in one transaction. With idempotent set, the client's
// Idempotency-Key is claimed in the same transaction and the result is
// stored, so a retry returns the first result instead of acting twice.
func Command(c *gin.Context, pool *pgxpool.Pool, scope string, idempotent bool, status int, fn func(tx pgx.Tx) (any, error)) {
	ctx := c.Request.Context()
	body, err := Body(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	key := c.GetHeader(idempotency.Header)
	if idempotent && key == "" {
		response.Fail(c, apperrors.Invalid(idempotency.Header, "required", nil))
		return
	}
	var result any
	var replay *idempotency.Stored
	actor := reqctx.Actor(ctx)
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		if idempotent {
			stored, err := idempotency.Begin(ctx, tx, actor, scope, key, idempotency.Hash(c.Request.Method, c.Request.URL.Path, body))
			if err != nil || stored != nil {
				replay = stored
				return err
			}
		}
		out, err := fn(tx)
		if err != nil {
			return err
		}
		result = out
		if idempotent {
			return idempotency.Finish(ctx, tx, actor, scope, key, status, out)
		}
		return nil
	})
	switch {
	case err != nil:
		response.Fail(c, err)
	case replay != nil:
		c.Header("Idempotent-Replayed", "true")
		response.JSON(c, replay.Status, replay.Data)
	default:
		response.JSON(c, status, result)
	}
}

// Created is the usual status for create commands.
const Created = http.StatusCreated
