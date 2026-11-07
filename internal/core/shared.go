// Package core holds the contract → PO → goods receipt → delivery order flow
// (N-04..N-08). These modules share one quantity model and one transaction
// discipline, so they live together; finance and CRM will be separate packages.
package core

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/access"
	"navo-api/internal/db"
)

// Handlers serve the core endpoints.
type Handlers struct {
	Pool *pgxpool.Pool
	TZ   *time.Location
}

// today is the business date in the company's timezone, as a UTC midnight to
// compare with DATE columns.
func (h *Handlers) today() time.Time {
	n := time.Now().In(h.TZ)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

// contractRef is what commands need to know about a contract.
type contractRef struct {
	ID       uuid.UUID
	Number   *string
	Status   string
	Currency string
	OwnerID  uuid.UUID
}

// lockContract loads a contract the caller may see, optionally locking it.
// A contract outside the caller's scope is reported as not found.
func lockContract(ctx context.Context, q db.Querier, id uuid.UUID, p *access.Principal, forUpdate bool) (*contractRef, error) {
	sql := `SELECT id, number, status, currency, owner_id FROM contracts WHERE id = $1 AND ($2::uuid IS NULL OR owner_id = $2)`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var c contractRef
	if err := q.QueryRow(ctx, sql, id, p.OwnerFilter()).Scan(&c.ID, &c.Number, &c.Status, &c.Currency, &c.OwnerID); err != nil {
		return nil, err
	}
	return &c, nil
}

// touchLines records the latest business change on contract lines (5.4 "last updated").
func touchLines(ctx context.Context, tx pgx.Tx, lineIDs []uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE contract_lines SET last_business_change_at = now() WHERE id = ANY($1)`, lineIDs)
	return err
}

// horizonDays reads the alert window (O-14).
func horizonDays(ctx context.Context, q db.Querier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT alert_horizon_days FROM company_settings`).Scan(&n)
	return n, err
}
