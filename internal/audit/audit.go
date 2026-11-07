// Package audit records who changed what, inside the same transaction as the
// change, so a failed audit write rolls the change back (AC-13).
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/reqctx"
)

// Record writes one audit entry. changes must never hold passwords, tokens or file contents.
func Record(ctx context.Context, tx pgx.Tx, action, entityType string, entityID uuid.UUID, changes map[string]any) error {
	body, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	var actor *uuid.UUID
	if id := reqctx.Actor(ctx); id != uuid.Nil {
		actor = &id
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_logs (actor_id, request_id, action, entity_type, entity_id, changes)
		VALUES ($1, $2, $3, $4, $5, $6)`, actor, reqctx.RequestID(ctx), action, entityType, entityID, body)
	return err
}
