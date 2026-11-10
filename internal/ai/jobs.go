package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/db"
	"navo-api/internal/reqctx"
)

// extractCached persists the result before replying. An uncertain HTTP response
// can retry the same file without purchasing another successful extraction.
func (h *Handlers) readCached(ctx context.Context, data []byte, model string, run func(context.Context) ([]byte, error)) ([]byte, error) {
	actor := reqctx.Actor(ctx)
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	var id uuid.UUID
	var cached []byte
	err := db.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ai_extraction_quota'))`); err != nil {
			return err
		}
		// Expired leases are failures, never fabricated successes.
		if _, err := tx.Exec(ctx, `UPDATE ai_extractions SET status='failed',finished_at=now() WHERE status='running' AND created_at<now()-interval '5 minutes'`); err != nil {
			return err
		}
		var status string
		err := tx.QueryRow(ctx, `SELECT id,status,result FROM ai_extractions WHERE actor_id=$1 AND source_hash=$2 AND model=$3 FOR UPDATE`, actor, hash, model).Scan(&id, &status, &cached)
		if err == nil {
			if status == "completed" {
				return nil
			}
			if status == "running" {
				return apperrors.Rule("ai_busy", "This file is already being read. Retry shortly.", nil)
			}
			// Failed requests require an explicit retry; this request is that retry.
		} else if err != pgx.ErrNoRows {
			return err
		}
		var concurrent, personal, daily int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='running'),count(*) FILTER(WHERE status='running' AND actor_id=$1),(SELECT count(*) FROM ai_extraction_attempts WHERE actor_id=$1 AND created_at>=now()-interval '24 hours') FROM ai_extractions`, actor).Scan(&concurrent, &personal, &daily); err != nil {
			return err
		}
		if concurrent >= 2 || personal >= 1 {
			return apperrors.Rule("ai_busy", "The reading service is busy. Retry shortly.", nil)
		}
		if daily >= 50 {
			return apperrors.Rule("ai_daily_limit", "Your daily document-reading limit was reached.", nil)
		}
		if id == uuid.Nil {
			if err := tx.QueryRow(ctx, `INSERT INTO ai_extractions(actor_id,source_hash,model,status) VALUES($1,$2,$3,'running') RETURNING id`, actor, hash, model).Scan(&id); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE ai_extractions SET status='running',result=NULL,created_at=now(),finished_at=NULL WHERE id=$1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO ai_extraction_attempts(extraction_id,actor_id) VALUES($1,$2)`, id, actor)
		return err
	})
	if err != nil {
		return nil, err
	}
	if cached != nil {
		return cached, nil
	}
	raw, extractErr := run(ctx)
	finish, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if extractErr != nil {
		_, _ = h.Pool.Exec(finish, `UPDATE ai_extractions SET status='failed',finished_at=now() WHERE id=$1`, id)
		return nil, extractErr
	}

	if _, err := h.Pool.Exec(finish, `UPDATE ai_extractions SET status='completed',result=$2,finished_at=now() WHERE id=$1`, id, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (h *Handlers) extractCached(ctx context.Context, media string, data []byte) (*Draft, error) {
	raw, err := h.readCached(ctx, data, "contract-v1:"+h.AI.model, func(ctx context.Context) ([]byte, error) {
		d, err := h.AI.Extract(ctx, media, data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(d)
	})
	if err != nil {
		return nil, err
	}
	var d Draft
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
func (h *Handlers) noteCached(ctx context.Context, text string, today time.Time) (*NoteTask, error) {
	if !h.AI.Enabled() {
		return nil, apperrors.Rule("ai_disabled", "AI drafts are not turned on.", nil)
	}
	raw, err := h.readCached(ctx, []byte(text), "note-v1:"+today.Format(time.DateOnly)+":"+h.AI.model, func(ctx context.Context) ([]byte, error) {
		d, err := h.AI.ParseNote(ctx, text, today)
		if err != nil {
			return nil, err
		}
		return json.Marshal(d)
	})
	if err != nil {
		return nil, err
	}
	var d NoteTask
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
