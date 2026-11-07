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

Demo accounts (password = `NAVO_SEED_PASSWORD`): `admin@congty.vn` (admin), `lan@congty.vn`
(leader), `mai@congty.vn` (accounting), `khoa@congty.vn` and `ha@congty.vn` (sales). The demo
data reproduces the mockups: contract SC-26-0001 (Sông Trà) with the AC-01 line at 50/100 and no
plan, a late PO, an under-ordered line; SC-26-0002 (Đạm Phương Nam) overdue; one draft contract.
For a single real account use `NAVO_USER_PASSWORD=… make user EMAIL=… NAME=… ROLES=…`.
Start over: `dropdb navo_dev && createdb navo_dev && make migrate && make seed`.

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

### Error responses

`{request_id, status, success:false, error_code, error, reason?, params?, errors?[{field, code, params}]}`.
`error_code`: `validation.invalid_request` (400), `auth.unauthorized` (401), `auth.forbidden` (403),
`resource.not_found` (404, also for records outside your scope), `resource.conflict` /
`rule.violation` (409), `rate.limited` (429), `server.internal` (500).
Rule reasons so far: `contract_not_draft`, `contract_not_active`, `document_not_draft`,
`quantity_exceeds_contract`, `receipt_exceeds_ordered`, `delivery_exceeds_contract`,
`insufficient_stock`, `purchase_order_not_confirmed`, `invalid_credentials`,
`idempotency_payload_mismatch`, `idempotency_in_progress`, `csrf_header_missing`, `override_already_pending`, `override_already_decided`.
Field codes: `required`, `too_long`, `format`, `decimal`, `greater_than`, `less_or_equal`, `date`,
`uuid`, `currency`, `one_of`, `duplicate`, `taken`, `not_found`, `not_in_contract`,
`not_customer`, `not_supplier`, `email`, `length`, `tax_code`, `type`, `invalid_json`.

### Line health flags (REQUIREMENTS 5.4.1)

`delivery_overdue`, `over_delivered` (critical); `no_plan`, `stock_short`, `receipt_overdue`,
`over_ordered` (warning); `under_ordered` (info); `completed`. Window for `stock_short` is
`company_settings.alert_horizon_days` (default 14).

## Not yet built (next increments)

Returns and reversals, acceptance,
amendments, finance (invoices, receipts/payments, allocations), files and documents tree,
CRM notes/tasks, MISA export, AI drafts, OpenAPI file and backup runbook.
