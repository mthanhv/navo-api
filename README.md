# navo-api

Backend for Navo: contracts → purchase orders → goods receipts → delivery orders, with line
progress and health flags. One company per installation (Nhật Duy SGN).

## Run locally

Needs Go and PostgreSQL (Homebrew `postgresql@18` listens on the `/tmp` socket).

```sh
cp .env.example .env            # adjust DATABASE_URL if needed
createdb navo_dev && createdb navo_test
make migrate
NAVO_SEED_PASSWORD='dev-password-123' make seed   # demo users + data, empty dev DB only
make run                        # http://localhost:8080
make check && make test-integration
```

Demo accounts (password = `NAVO_SEED_PASSWORD`): `admin@congty.vn` (admin), `tuan@congty.vn`
(delivery), `mai@congty.vn` (accounting), `khoa@congty.vn` and `ha@congty.vn` (sales). The demo
data reproduces the mockups: contract SC-26-0001 (Sông Trà) with the AC-01 line at 50/100 and no
plan, a late PO, an under-ordered line; SC-26-0002 (Đạm Phương Nam) overdue; one draft contract.
For a single real account use `NAVO_USER_PASSWORD=… make user EMAIL=… NAME=… ROLES=…`.
Start over: `dropdb navo_dev && createdb navo_dev && make migrate && make seed`.

To clear transaction records while preserving users, partners, products, settings, document
number sequences, and audit history, run `make clear-transaction-data` first to preview row
counts. The script removes quotations, contracts and their lifecycle documents, purchase and
delivery documents, receipts, invoices, payments, MISA exports, transaction-only attachments,
and idempotency records. It also removes stored file bytes when a file is no longer linked to
any non-transaction record.

Deletion is opt-in and limited to database names containing a `dev`, `test`, or `local` marker:

```sh
make clear-transaction-data ARGS="--execute --confirm-db navo_dev"
make clear-transaction-data ARGS="--url-env TEST_DATABASE_URL --execute --confirm-db navo_test"
```

The script refuses production/live names and requires the exact database name as confirmation.
Stop the API before execution. It does not clear CRM activities, mailings, notifications, or
audit history. Run it from a database owner account so the cleanup can temporarily disable
history-protection triggers inside the transaction.

### Local email with Mailpit

From `../navo-infra`, run `make local-mailpit`, then set these values in the API `.env`
and restart `make run`:

```dotenv
NAVO_SMTP_HOST=127.0.0.1
NAVO_SMTP_PORT=1025
NAVO_SMTP_USERNAME=
NAVO_SMTP_PASSWORD=
NAVO_SMTP_ALLOW_INSECURE=true
NAVO_MAIL_FROM=navo@example.test
```

Send a product-news email from the app and inspect recipients, subject and body at
http://localhost:8025. Mailpit captures messages instead of delivering them to customers.
The full Docker stack (`../navo-infra: make local-up`) configures this automatically.
Use `NAVO_SMTP_ALLOW_INSECURE` only for local capture servers; it defaults to false.
Existing integration tests use an in-memory sender and do not need Mailpit.

## API (v1, increment A1–A2)

All bodies are JSON; quantities and money are decimal strings (`"60"`, `"980.50"`).
Web clients sign in with a cookie and must send `X-Navo-Client: web` on writes; mobile clients
send `client: "mobile"` at login and use `Authorization: Bearer <token>`. Confirm and activate
need an `Idempotency-Key` header (8–128 chars); a retry with the same key returns the first
result (`Idempotent-Replayed: true`).

| Method | Path | Permission |
|---|---|---|
| POST | `/api/v1/auth/login`, `/auth/logout`; GET `/me` | — |
| GET, POST | `/api/v1/users` | users.manage |
| GET, POST | `/api/v1/organizations` (`?role=customer\|supplier&q=`) | contracts.read / partners.write |
| GET, POST | `/api/v1/products` | contracts.read / catalog.write |
| GET, POST | `/api/v1/contracts`; GET `/contracts/:id` | contracts.read / contracts.write |
| POST | `/api/v1/contracts/:id/activate` `{signed_on}` → `SC-yy-nnnn` | contracts.write |
| POST | `/api/v1/contracts/:id/purchase-orders`; GET `/purchase-orders/:id`; POST `/purchase-orders/:id/confirm` | procurement.write |
| POST | `/api/v1/purchase-orders/:id/goods-receipts`; POST `/goods-receipts/:id/confirm` | fulfillment.write / .confirm |
| POST | `/api/v1/contracts/:id/delivery-orders`; POST `/delivery-orders/:id/confirm` | fulfillment.write / .confirm |
| GET | `/api/v1/reports/contract-lines` (`?flag=no_plan\|needs_action…&contract_id=`) | contracts.read |
| GET | `/api/v1/purchase-orders` (`?view=awaiting\|draft\|all`), `/delivery-orders` (`?view=planned\|delivered`) | contracts.read |
| POST | `/api/v1/purchase-orders/:id/override-request` `{reason}`; `/delivery-orders/:id/override-request` `{reason, delivered_on}` | procurement / fulfillment.write |
| GET, POST | `/api/v1/override-requests` (`?status=pending\|decided`); `/:id/approve` `{note?}`; `/:id/reject` `{note}` | quantity.override (Admin) |
| GET | `/api/v1/reports/overview`, `/contracts/:id/documents` | contracts.read |
| GET, POST | `/api/v1/organizations/:id/contacts`; PATCH, DELETE `/contacts/:id` (many contacts per customer/supplier, one main) | contracts.read / partners.write |
| GET | `/api/v1/contacts/emailable` (customer contacts with an address who accept product news) | partners.write |
| GET | `/api/v1/mailings`, `/mailings/:id`, `/mailings/status` | contracts.read (sales: own) |
| GET | `/api/v1/contracts/:id/history` (who changed the contract and its documents, and why) | contracts.read |
| POST | `/api/v1/contracts/:id/close`, `/cancel`, `/reopen` `{reason}` (Idempotency-Key) | contracts.write |
| POST | `/api/v1/contracts/:id/amendments` `{version, signed_on, reference?, reason, lines[], new_lines[]}` | contracts.write |
| POST | `/api/v1/contracts/:id/acceptances`, `/contracts/:id/customer-returns` `{on, evidence\|reason, lines[{contract_line_id, quantity}]}`; `/purchase-orders/:id/supplier-returns` `{on, reason, lines[{purchase_order_line_id, quantity}]}`; `/acceptances\|customer-returns\|supplier-returns/:id/reverse` `{reason}` | fulfillment.confirm |
| POST | `/api/v1/me/mfa/setup`, `/me/mfa/enable {code}`, `/me/mfa/disable {password, code}`; `/users/:id/mfa/reset` (users.manage, not yourself) | signed in |
| GET, POST | `/api/v1/imports/partners\|products/template`, `/preview`, `/commit` (multipart `file`; commit needs `file_hash`) | partners.write / catalog.write |
| POST | `/api/v1/mailings/preview`; `/mailings` `{subject, body, product_ids, contact_ids}` (Idempotency-Key; one copy per contact, sent in the background) | partners.write |

Admin holds every permission and sees the whole company, cannot decide their own
over-quantity request, and with `NAVO_REQUIRE_ADMIN_MFA=true` (production) must turn on
two-step sign-in before anything else. Login accepts `code` or `recovery_code` and answers
reason `mfa_required` when one is needed. Contacts: everyone reads; changing and emailing a
customer's contacts needs `contacts.write` and being its owner, an owner of one of its open
contracts, or a whole-company role. Net received/delivered/accepted come from SQL functions
(migration 016); see `docs/REVIEW_2026-11-11.md`. Products also carry `description`,
`origin`, `supplier_id` (optional usual supplier) and `supplier_item_code`. Email needs
`NAVO_SMTP_HOST` and `NAVO_MAIL_FROM` (see `.env.example`); without them sending answers
`email_disabled` and everything else works.

### Error responses

`{request_id, status, success:false, error_code, error, reason?, params?, errors?[{field, code, params}]}`.
`error_code`: `validation.invalid_request` (400), `auth.unauthorized` (401), `auth.forbidden` (403),
`resource.not_found` (404, also for records outside your scope), `resource.conflict` /
`rule.violation` (409), `rate.limited` (429), `server.internal` (500).
Rule reasons so far: `contract_not_draft`, `contract_not_active`, `document_not_draft`,
`quantity_exceeds_contract`, `receipt_exceeds_ordered`, `delivery_exceeds_contract`,
`insufficient_stock`, `purchase_order_not_confirmed`, `invalid_credentials`,
`idempotency_payload_mismatch`, `idempotency_in_progress`, `csrf_header_missing`, `override_already_pending`, `override_already_decided`, `email_disabled`.
Field codes: `required`, `too_long`, `format`, `decimal`, `greater_than`, `less_or_equal`, `date`,
`uuid`, `currency`, `one_of`, `duplicate`, `taken`, `not_found`, `not_in_contract`,
`not_customer`, `not_supplier`, `email`, `length`, `tax_code`, `type`, `invalid_json`.

### Line health flags (REQUIREMENTS 5.4.1)

`delivery_overdue`, `over_delivered` (critical); `no_plan`, `stock_short`, `receipt_overdue`,
`over_ordered` (warning); `under_ordered` (info); `completed`. Window for `stock_short` is
`company_settings.alert_horizon_days` (default 14).

## Current workflows and limits

Implemented: contract drafts/activation and draft editing; PO/GR/DO creation, confirmation and quantity approvals; full GR/DO reversal and closing outstanding PO quantities; versioned payment terms, evidenced full-delivery acceptance, invoices, receipts/payments and reversible allocations; scoped files/ZIP, notes/tasks, contacts/mailings, MISA code exchange and frozen export files, AI-assisted drafts, and selected assignment handover.

See [reliability and API details](docs/RELIABILITY.md), [assessment baseline](docs/PRODUCT_REVIEW_2026-11-10.md), [remediation status and release limits](docs/REMEDIATION.md), and [the 11 November review and fixes](docs/REVIEW_2026-11-11.md).

Opening balances, in-progress contract onboarding, real-MISA template acceptance, and a maintained OpenAPI schema remain incomplete. Returns, active amendments and contract close/cancel are implemented. Do not infer these from status columns or historical requirements. There is no subscription billing or tenant model: one business uses each installation.


## Product review fixes (20 November 2026)

- Acceptance terms synchronize when quantities or payment terms change, including terms
  added after acceptance. Changes that invalidate an already allocated acceptance are refused.
- Receipt allocation locks the source contracts and accepts only active/closed contracts.
  Receivable summaries consistently honor `customer_id`.
- `POST /misa/import` previews catalogue matches without writing. Confirm by reuploading the
  file with multipart `confirm=true` and the returned `preview_hash`. The server rechecks
  the matches; changed previews return `misa_preview_changed`. Conflicting tax IDs never
  fall back to a name match. Name-only matches require the same explicit review.
- Service-contract creation is gated with field error `service_unavailable` until the service
  fulfillment workflow is released. Supply contracts remain supported.
- Delivery staff may upload GR/DO evidence and remove their own attachments, without gaining
  contract/finance file writes. The legacy notes endpoint returns manual notes only.
- Pending Zalo messages are invalidated after permission, assignment, or rule changes. The
  next tick rebuilds daily messages and rechecks eligibility before sending queued content.
- Pending migrations apply as one transaction. Migration 025 restores and synchronizes the
  legacy `notes`/`tasks` tables for rollback images; 023 remains immutable. Keep these adapters
  while any supported image uses the legacy schema. A deployment from before 023 must use
  this migration runner so the temporary dropped-table schema is never committed.

Run `make check` and `make -e test-integration` with a dedicated UTF-8 test database.
Regression coverage is in `internal/integration/product_review_test.go`.
