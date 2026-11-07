# navo-api

Go API for Navo (Gin, PostgreSQL via pgx). Product rules: `../REQUIREMENTS.md`; design:
`../TECHNICAL_DESIGN.md`. Conventions follow Lova's engineering guidelines (workflow, validation,
error contract, audit, tests), adapted for one company per installation (no tenant_id, no RLS).

## Commands

- `make check` (gofmt, vet incl. integration tag, race unit tests) must pass.
- `make test-integration` needs `TEST_DATABASE_URL` (database name must contain `test`; the
  test drops and rebuilds its public schema). Never point it at `DATABASE_URL`.
- New migration: `db/migrations/NNN_description.sql`. Applied files are immutable.

## Shared building blocks

- Errors: return `apperrors` values (`Invalid`, `FieldErrors.Add/Err`, `Rule(reason, …)`,
  `Conflict`, `Forbidden`, `NotFound`). `response.Fail` maps them and database constraint
  errors; server faults are logged with the request id and never leak detail.
- Requests: `httpx.Decode` (strict, unknown fields refused), `httpx.Page`, `httpx.PathID`,
  `httpx.UUID`, `httpx.Date`; numbers arrive as decimal strings via `decimalx.Positive` /
  `decimalx.NonNegative` (never float).
- Writes: `httpx.Command(c, pool, scope, idempotent, status, fn)` runs one transaction. Confirm,
  activate and other non-repeatable commands pass `idempotent=true` (Idempotency-Key header).
- Audit: `audit.Record(ctx, tx, …)` in the same transaction for every write. Never record
  passwords, tokens or file contents.
- Permissions: `auth.Require(access.X)` on the route; data scope via
  `Principal.OwnerFilter()` in every query of contracts and their documents (out of scope =
  404). Field-level secrets (purchase prices) are omitted server-side (`PurchaseCostRead`).
- Numbers: `numbering.NextYearly` (SC/PI/RV/PV/DRAFT) and `numbering.NextChild` (PO/GR/DO under a
  contract), REQUIREMENTS 5.12.
- Line progress and health flags: `progress` (pure, unit tested) and `core.QueryLines` (one
  query, each quantity aggregated separately). Never recompute these in a client.

## Rules

- Confirms lock the document, then affected lines in id order, then re-read totals.
- Confirmed documents are never hard-deleted (DB trigger); corrections are reversals.
- Composite foreign keys keep PO/GR/DO lines inside their own contract; keep that pattern.
- A business-logic change updates tests (`internal/progress`, `internal/integration`) and the
  docs in the same change; record it in `changes.txt`.
