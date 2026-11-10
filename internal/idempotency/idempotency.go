// Package idempotency makes confirm commands safe to retry: the same key and
// payload returns the first result; a different payload with the same key is refused.
// The record is written in the command's transaction, so a failed command
// leaves no record and can be retried.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/reqctx"
)

// Header is the HTTP header carrying the client's key.
const Header = "Idempotency-Key"

// Stored is a previously committed result.
type Stored struct {
	Status int
	Data   json.RawMessage
}

// Hash fingerprints a request so a reused key with another payload is detected.
func Hash(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method + " " + path + "\n"))
	h.Write(body)
	return h.Sum(nil)
}

// Begin claims the key. It returns the stored result when the command already
// ran. A concurrent request with the same key waits on the row lock until the
// first commits, then sees its result.
func Begin(ctx context.Context, tx pgx.Tx, actor uuid.UUID, scope, key string, hash []byte) (*Stored, error) {
	if l := len(key); l < 8 || l > 128 {
		return nil, apperrors.Invalid(Header, "idempotency_key", map[string]any{"min": 8, "max": 128})
	}
	tag, err := tx.Exec(ctx, `INSERT INTO idempotency_records (actor_id, scope, key, request_hash, authorization_hash)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, actor, scope, key, hash, reqctx.Authorization(ctx))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var storedHash []byte
	var status *int
	var data []byte
	var accessHash []byte
	if err := tx.QueryRow(ctx, `SELECT request_hash, status_code, response, authorization_hash FROM idempotency_records
		WHERE actor_id = $1 AND scope = $2 AND key = $3`, actor, scope, key).Scan(&storedHash, &status, &data, &accessHash); err != nil {
		return nil, err
	}
	if len(accessHash) == 0 || !bytes.Equal(accessHash, reqctx.Authorization(ctx)) {
		return nil, apperrors.Conflict("idempotency_authorization_changed", "Your access changed. Reload the document to check its current status.", nil)
	}
	if !bytes.Equal(storedHash, hash) {
		return nil, apperrors.Conflict("idempotency_payload_mismatch", "This retry key was already used for a different request.", nil)
	}
	if status == nil {
		return nil, apperrors.Conflict("idempotency_in_progress", "The same request is still being processed.", nil)
	}
	return &Stored{Status: *status, Data: data}, nil
}

// Finish stores the result for later retries.
func Finish(ctx context.Context, tx pgx.Tx, actor uuid.UUID, scope, key string, status int, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE idempotency_records SET status_code = $4, response = $5
		WHERE actor_id = $1 AND scope = $2 AND key = $3`, actor, scope, key, status, body)
	return err
}
