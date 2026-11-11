package finance

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
)

// SyncAcceptanceTerms brings a contract's "after acceptance" payment terms in line with its
// acceptance documents (owner decision 2026-11-11): a term falls due only when every line
// is fully accepted, dated by the latest confirmed acceptance. Partial acceptance is
// recorded but never makes money due and no amount is inferred.
//
// It is called in the same transaction as every acceptance change, after the contract's
// lines and terms are locked. A term that loses its acceptance while money is allocated to
// it refuses the change, so collected cash is never left against an event that did not happen.
func SyncAcceptanceTerms(ctx context.Context, tx pgx.Tx, contractID, actor uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM payment_milestones WHERE contract_id = $1 ORDER BY id FOR UPDATE`, contractID); err != nil {
		return err
	}
	var full bool
	var completing *uuid.UUID
	var on *time.Time
	var number *string
	if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM contract_lines WHERE contract_id = $1)
			AND NOT EXISTS (SELECT 1 FROM contract_lines cl WHERE cl.contract_id = $1 AND line_accepted(cl.id) < cl.quantity),
			a.id, a.accepted_on, a.number
		FROM (SELECT 1) one LEFT JOIN LATERAL (
			SELECT id, accepted_on, number FROM acceptances WHERE contract_id = $1 AND status = 'confirmed'
			ORDER BY accepted_on DESC, created_at DESC LIMIT 1) a ON true`, contractID).Scan(&full, &completing, &on, &number); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT m.id, m.triggered_on IS NOT NULL, m.acceptance_id,
			EXISTS (SELECT 1 FROM receipt_allocations a WHERE a.milestone_id = m.id AND a.reversed_at IS NULL)
		FROM payment_milestones m WHERE m.contract_id = $1 AND m.trigger = 'acceptance' ORDER BY m.id`, contractID)
	if err != nil {
		return err
	}
	type term struct {
		id        uuid.UUID
		triggered bool
		from      *uuid.UUID
		collected bool
	}
	var terms []term
	for rows.Next() {
		var t term
		if err := rows.Scan(&t.id, &t.triggered, &t.from, &t.collected); err != nil {
			rows.Close()
			return err
		}
		terms = append(terms, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range terms {
		switch {
		case t.triggered && t.from == nil:
			// A date entered by hand before acceptance documents existed stays until reset.
			continue
		case full && completing != nil && (t.from == nil || *t.from != *completing):
			if err := setAcceptanceEvent(ctx, tx, t.id, actor, on, completing, number); err != nil {
				return err
			}
		case !full && t.from != nil:
			if t.collected {
				return apperrors.Rule("acceptance_has_payments", "Money is allocated to a term that falls due on acceptance. Reverse those allocations first.", nil)
			}
			if err := setAcceptanceEvent(ctx, tx, t.id, actor, nil, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func setAcceptanceEvent(ctx context.Context, tx pgx.Tx, id, actor uuid.UUID, on *time.Time, acceptance *uuid.UUID, number *string) error {
	if _, err := tx.Exec(ctx, `UPDATE payment_milestones SET triggered_on = $2, trigger_note = $3, acceptance_id = $4, version = version + 1 WHERE id = $1`,
		id, on, number, acceptance); err != nil {
		return err
	}
	if err := recordTermHistory(ctx, tx, id, actor); err != nil {
		return err
	}
	action, changes := "trigger", map[string]any{"note": number, "acceptance_id": acceptance}
	if on == nil {
		action, changes = "reset_event", map[string]any{"reason": "acceptance reversed"}
	} else {
		changes["on"] = on.Format(time.DateOnly)
	}
	return audit.Record(ctx, tx, action, "payment_milestone", id, changes)
}
