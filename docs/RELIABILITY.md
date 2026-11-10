# Reliability rules and API additions

These rules implement the conservative choices approved on 11 November 2026. All monetary inputs are explicit decimal strings; percentages describe a term and do not choose a net/gross or partial-acceptance basis.

## Corrections and permissions

| Command | Input | Guard and result |
|---|---|---|
| `PATCH /api/v1/contracts/:id` | Complete draft input plus `version` | Draft only, scoped owner, matching version. Lines are replaced atomically; payment terms are edited separately. Customer cannot change after linked notes/evidence; currency cannot change while terms exist. |
| `GET /api/v1/contracts/:id/draft` | — | Scoped draft editing input, including version, prices, units, taxes and owner. |
| `POST /api/v1/goods-receipts/:id/reverse` | `{reason}` | Confirmed only; remaining accepted stock must cover all confirmed deliveries. Original lines remain. |
| `POST /api/v1/delivery-orders/:id/reverse` | `{reason}` | Confirmed only; recorded acceptance and live delivery-dependent allocations must be corrected first. |
| `POST /api/v1/purchase-orders/:id/close` | `{reason}` | Confirmed only, no draft receipt. Cancels the entire **unreceived balance**; received quantities and original ordered quantity remain in history. Returned line `quantity` is the effective quantity, with `cancelled_quantity` separate. |
| `POST /api/v1/receipts/:id/reverse`, `/payments/:id/reverse` | `{reason}` | Posted only, with no live allocations. Original money record remains with reversed status. |
| `POST /api/v1/payments/:id/allocations` | `{purchase_invoice_id,amount}` | Posted payment, supplier/currency match, recorded invoice. Both available payment and outstanding invoice limits are checked after separate locks. |
| `GET /api/v1/payments/:id/allocations` | — | Includes reversed allocation history. |
| `POST /api/v1/payment-allocations/:id/reverse` | `{reason}` | Reverses an allocation without deleting the original. |
| `POST /api/v1/milestones/:id/trigger` | `{on,note}` | Acceptance/manual only; required evidence reference, nonfuture date, active contract, once only. Acceptance requires full confirmed delivery on/before the supplied date. |
| `POST /api/v1/milestones/:id/reset` | `{reason}` | Explicit correction of a recorded manual/acceptance event; blocks live allocations; versioned history preserved. |
| `PATCH /api/v1/milestones/:id` | Complete term input plus `version` | Matching version; untriggered, no invoice/allocation history; draft/active contract only. |
| `GET /api/v1/handovers/preview?from_user=:id` | — | Admin preview of draft/active contracts and open tasks. |
| `POST /api/v1/handovers` | `{from_user,to_user,reason,contracts:[{id,version}],task_ids:[]}` | Admin, active sales/admin successor, selected current assignments only. Linked tasks require successor contract access. Ownership transfers atomically, authorship is retained, old command replay is invalidated. |

Correction and handover commands require an `Idempotency-Key`. Reasons are required (1–500 characters). Route permissions continue to apply; full receipt/delivery reversal uses fulfillment confirmation permission, cash uses finance write, PO close uses procurement write. File upload/unlink requires contract write or finance write plus existing resource/field visibility.

Creation endpoints accept an optional `Idempotency-Key`. Contract creation can include `milestones` and creates header, lines and terms in one transaction. A new create request with the same key must have the same payload. Stored command results are bound to the caller's resolved permission/scope revision; changed or legacy authorization returns `idempotency_authorization_changed` without repeating the effect. Read the current resource after that conflict.

`POST /files` supports a UUID `client_id`. The same actor/identity with identical owner, kind, name and hash replays the original IDs; different input conflicts. `POST /notes` serializes `client_id`, replays the original note/task, and rejects changed business content. A successfully posted mobile note is immutable; photo checkpoints continue separately.

## Financial presentation and accounting exchange

Sales invoice `paid` is `null`: cash is allocated to obligations, not individually to split sales invoices. Do not copy the milestone's paid amount onto each invoice. Purchase invoices use live payment allocations. Supplier advances remain unallocated until explicitly assigned; cancelling an invoice with live payment allocations is blocked.

MISA export generation and marking occur in one transaction. Each new batch stores immutable workbook bytes, SHA-256 and mapping version. Subsequent download verifies and returns those exact bytes. Cancellation/reversal flags affected batches `correction_needed`; this flag is a review signal, not proof of an accounting adjustment in MISA. Legacy batches without frozen bytes answer `legacy_export_unavailable`; retrieve their original downloaded file rather than fabricating history. Real MISA SME template/account acceptance remains required.

## Operational boundaries

Readiness checks all required migration versions as well as database connectivity; additional expand-only versions are allowed for rollback. Apply migrations 011–014 before using these changes. Back up and reconcile existing money data before rollout: historic over-allocation is not silently rewritten.

AI extraction persists successful results by actor/source hash/model before reply, allows one running request per actor/two globally, and caps attempts at 50 per rolling day. Retries of completed sources reuse results. Expired running jobs fail after five minutes; provider retries are disabled. This is bounded synchronous extraction with recovery, not a general async worker. The source document is attached by the web after draft creation; a failed attachment remains visible/retryable.

SMTP recipients pass through `sending`. Uncertain outcomes or interrupted in-flight sends are `unknown`, not confirmed delivery and not automatically resent. A recorded SMTP success is not recipient delivery proof. New mailings snapshot product details; old mailings lack those historical fields.

ZIP files are fully prepared before a success response. Missing bytes or copy failures fail visibly; archives over 500 MB require individual downloads.
