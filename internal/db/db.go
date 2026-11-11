// Package db opens the PostgreSQL pool, runs transactions and applies migrations.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects and verifies the database is reachable.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// InTx runs fn in one transaction; any error rolls everything back, so a
// document, its audit entry and its idempotency record commit together.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, fn)
}

// Ready accepts later additive migrations but refuses a schema missing anything
// required by this binary. A DB ping alone is not application readiness.
func Ready(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	versions := make([]string, len(names))
	for i, name := range names {
		versions[i] = strings.TrimSuffix(name, ".sql")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=ANY($1)`, versions).Scan(&count); err != nil {
		return err
	}
	if count != len(versions) {
		return fmt.Errorf("schema incompatible: %d of %d required migrations", count, len(versions))
	}
	return pool.Ping(ctx)
}

// migrationLockID serialises migration runners (pg_advisory_xact_lock key).
const migrationLockID = 7_428_001

// Migrate applies every *.sql file in files not yet recorded, in name order,
// each in its own transaction. Applied files are immutable: never edit one,
// add a new numbered file instead.
func Migrate(ctx context.Context, pool *pgxpool.Pool, files fs.FS) ([]string, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	var applied []string
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return applied, err
		}
		done := false
		err = InTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				return err
			}
			done = true
			return nil
		})
		if err != nil {
			return applied, err
		}
		if done {
			applied = append(applied, version)
		}
	}
	return applied, nil
}

// Retention is how long finished sessions and stored command results are kept. A retry of
// a command arrives within minutes; ninety days is far beyond any real retry.
const Retention = 90 * 24 * time.Hour

// Purge deletes sessions that ended more than Retention ago (keeping each person's most
// recent one, which is their "last signed in") and command results older than Retention.
// Business records and the audit log are never purged.
func Purge(ctx context.Context, pool *pgxpool.Pool) (sessions, results int64, err error) {
	cutoff := time.Now().Add(-Retention)
	tag, err := pool.Exec(ctx, `DELETE FROM sessions s WHERE coalesce(s.revoked_at, s.expires_at) < $1
		AND s.created_at < (SELECT max(created_at) FROM sessions x WHERE x.user_id = s.user_id)`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	sessions = tag.RowsAffected()
	tag, err = pool.Exec(ctx, `DELETE FROM idempotency_records WHERE created_at < $1`, cutoff)
	if err != nil {
		return sessions, 0, err
	}
	return sessions, tag.RowsAffected(), nil
}
