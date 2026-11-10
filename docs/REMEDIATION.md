# Remediation status — 11 November 2026

Read this with the [baseline assessment](PRODUCT_REVIEW_2026-11-10.md) and [implemented rules/API](RELIABILITY.md). The assessment was performed before the changes. This status deliberately separates implemented safeguards from workflows and external acceptance that remain outstanding.

Navo remains a single-business operational application, not a multi-tenant subscription SaaS. The current changes make the existing contract, finance and field-note workflows safer for a controlled pilot; they do not establish production readiness for every requirement.

## Changes against the findings

| Finding | Current status | Implementation/evidence |
|---|---|---|
| C1 money races | Fixed for reproduced paths | `finance/receipts.go`, `finance/payment_allocations.go`: parent lock then fresh sums; deterministic competing 80-of-100 integration regressions. |
| C2 replay after access change | Fixed | `auth/auth.go`, `idempotency/idempotency.go`: resolved-access fingerprint and user revision; replay refuses changed/legacy authorization without reapplying the mutation. Revocation regression passes. |
| C3 tasks/files permissions | Fixed | `crm/crm.go`, `documents/*`, router: scoped contract/customer validation, transactional task audit, mutation capability gate; reader upload and guessed-contract regressions. |
| C4 correction dead ends | Approved minimum implemented; broader lifecycle incomplete | Full GR/DO/cash reversals, outstanding PO cancellation, draft editing, trigger reset, supplier allocation reversals. Reasons/history/dependency guards tested. Partial physical returns, active commercial amendments and contract close/cancel remain separate missing workflows; do not simulate these with a full reversal of a partly valid document. |
| C5 interrupted multistep saves | Fixed for tested open-form retries | Contract/lines/terms save atomically; stable create identities and resumable attachment; GR retry uses the same document/key after lost confirmation. Browser fault injection proves one GR/effect. Keys are not persisted across closing/reloading a web form; inspect existing drafts before starting a fresh create after an uncertain interruption. |
| C6 account-shared drafts | Fixed | Mobile installation/user namespace, session reset, owner checks and serialization. Old unowned drafts stay quarantined, never attributed to the next login. Jest isolation tests pass. |
| H1 export history | Frozen new batches implemented; real-MISA acceptance outstanding | Immutable workbook bytes/hash/version, visible correction flag, corrected import counts. Real SME template/account mapping and proof of import remain unverified. Legacy original bytes cannot be reconstructed. |
| H2 settlement/advances | Reproduced defects fixed; no invented sales invoice settlement | Sales `paid=null` rather than duplicated milestone cash; explicit supplier advance allocations/residual/history and supplier validation. Invoice cancellation blocks live allocations. Invoice-driven obligation generation requires a separate agreed commercial model. |
| H3 acceptance/term integrity | Approved minimum implemented | Full-delivery guard, required evidence, nonfuture/once-only event, explicit reset/history, version preconditions and dependent-term guards. No partial acceptance or inferred amounts. |
| H4 continuity/onboarding | Handover implemented; opening data incomplete | Selected versioned assignments transfer atomically, authorship stays intact; old owner access revoked. Catalog/opening-balance preview/commit and reconciliation need real starting data and agreed mappings. |
| H5 native recovery | Logic fixed and unit/build checked; device verification outstanding | Durable copied photo assets, checkpoint deduplication, autosave on background/back/unmount, independent request errors/freshness, pagination. No booted simulator was available; kill/relaunch, camera permissions and radio-loss behavior remain unverified on device. |
| H6 shared interaction | Implemented; focused browser tests | Field descriptions/errors, dirty dismissal, modal focus trap/inertness/return, keyboard tabs/radios. Browser regression covers focus and retained edits. Full assistive-technology testing remains outstanding. |
| H7 extraction bounds | Safeguards implemented; provider quality/provenance incomplete | Aligned timeouts, persistent result replay, concurrency/attempt limits, no SDK paid retries, ambiguous fields left empty. Local fake-provider result/cache/quota integration passes. Still synchronous; no cancel/poll worker, page-level source coordinates, real-provider timing/accuracy/privacy acceptance. |
| H8 release/recovery | Local safeguards tested; production exercise outstanding | Host-wide deploy/backup lock, full manifest rollback, strict SSM failures, HTTPS/image smoke, schema readiness, checksummed backup/file manifest, prebuilt ZIP. Fault injection covers config/backup/migrate/restart/health; local restore covers data/files/permissions/audit. No EC2/S3 deployment/restore or alert delivery was exercised. |
| H9 documentation/testing | Updated baseline/status/readmes and current browser tests | Current selectors retain business assertions; new concurrency/security/recovery tests. Machine-readable OpenAPI and cross-repo immutable release manifest remain future work. |
| L1 reporting bounds | Partial | Selector pagination and size clamping fixed. API reports still aggregate broad datasets in memory; benchmark and paginate SQL before larger deployments. |
| L2 mail restart truth | Implemented; live SMTP unverified | New campaign product snapshots, claimed `sending`, ambiguous/interrupted sends `unknown`, persistence errors logged. Unknown recipients are not automatically retried. Actual SMTP acceptance is not delivery proof. |
| L3 customer validation | Outstanding | Validate the corrected daily workflow with sales/accounting on anonymized customer documents before expanding scope. |

## Verification

- API `make check`: formatting, vet (including integration tag) and race unit checks pass.
- All existing Go integration tests plus `TestReliabilityRegressions` and `TestOperationalCorrections` pass with race detection in a new disposable database (180.330 s). The latter includes concurrent note replay and versioned handover.
- `TestAIJobRecovery` passes against PostgreSQL and a local fake provider (8.937 s): result reuse and rolling-attempt quota. No paid provider calls.
- Web typecheck/lint/unit/build pass; the final browser/API run and its artifact path are recorded below after completion.
- Mobile typecheck/lint, 10 Jest tests and iOS export pass before compatible dependency refresh; final recheck recorded below.
- Infrastructure configuration checks pass. Deployment fault tests cover five failure stages, web redirects and successful independent service updates. Local pg_dump/pg_restore drill recovers receipt amount/status, all audited record counts, authorization and SHA-256-identical file download; missing file bytes fail ZIP visibly. This is not a production RPO/RTO measurement.
- No cloud deployment, real customer email, real AI extraction, real MISA import or native-device E2E was performed.

## Next steps in risk/dependency order

1. **Before rollout (accounting + operator, 1–3 days):** back up the actual installation and inspect historic receipt allocations/payments for amounts above parent limits, duplicate notes/files, and exports with no original workbook. Reconcile with the accountant; preserve corrections and never silently rewrite history. Apply migrations 011–014 through the normal deployment path; verify readiness and old/new image compatibility in staging.
2. **External acceptance (2–4 days plus access):** use the customer's MISA SME export/import templates and anonymized examples to validate columns, accounts, currencies, round trips, cancellation adjustments and duplicate handling. Run SMTP/AI failure trials only after approved configuration/data-use decisions. Success means accountant-approved reconciliation and recoverable failure outcomes, not merely a generated file or 200 response.
3. **Operational acceptance (1–3 days):** run the documented S3 restore on a separate host, compare all referenced file hashes, money totals, audit and access; measure RTO against an agreed target. Exercise simultaneous API/web releases with real flock, image start failure and rollback. Name an alert responder and verify app/disk/backup alerts reach them.
4. **Native acceptance (2–3 days):** run current Maestro flows on SDK-57 iOS and supported Android/release builds; add draft account-switch, back/kill/relaunch, >100 selector entries, photo response-loss and independent documents failure. Pass criteria: no cross-account content, no lost draft, each photo once, stale data clearly labeled.
5. **Complete adoption gaps (5–10 days after starting files/rules):** implement catalog/opening balances preview/commit with row-level mapping/duplicate errors and reconciled currency totals. Add contract closure/cancellation and versioned active amendments only after obligations/exception rules are explicit. Physical partial returns need their own documents and net-quantity accounting, not mutation of confirmed lines.
6. **Sustain confidence (3–6 days, incremental):** publish API schema, pin tested repository commits for release, automate locale/type compatibility, close mobile advisory paths with a supported Expo migration, benchmark query/pagination boundaries and split web bundles only when measured value justifies it.

## Recommended acceptance matrix

Keep API interleaving tests separate from UI and native tests. Every failure should retain input, show a recoverable outcome, and leave no partial business/audit write.

| Journey | Required cases |
|---|---|
| Accounts/continuity | First admin, create/login/logout/reset/deactivate; scoped reads/writes; grant revoke then old-key replay; handover stale preview and selected tasks, author retained. |
| Draft to activation | Manual and AI-assisted input; duplicate product lines; explicit terms; invalid term rolls back whole draft; version conflict; attachment failure and retry; future signed date rejected. |
| Fulfillment | Split suppliers; accepted/rejected receipts; partial deliveries; actual dates; last-stock race; approval permission; same/different-key duplicates; lost create/confirm response; full reversal in dependency order; partial PO close preserves received quantity. |
| Customer money | Acceptance before/after full delivery with required evidence; repeated event/reset; explicit amounts; concurrent receipts competing for one milestone and allocations competing for one receipt; reverse allocations before cash; split invoice shows no invented settlement. |
| Supplier money | Invalid supplier/currency; advance across invoices/residual; concurrent payments and allocations; invoice cancellation with active allocations; reverse then reallocate and verify report sums. |
| Evidence/MISA | Readers cannot upload/unlink; out-of-scope links invisible; identical upload UUID replays and changed hash conflicts; missing file ZIP fails; frozen batch unchanged after edits/cancel/reversal, correction flag; real-MISA round trip and old-file policy. |
| Mobile field work | API down while internet up, stale timestamp, independent docs retry, pagination, incomplete-note autosave, background/kill/relaunch, account switch, note response loss, partial photo retry/dedup, quota/provider errors. |
| Release/restore | Every failure stage and concurrent deploy; required migrations missing; old image after additive migrations; dump and file corruption; restore financial/audit/scope checks; S3 availability and alert delivery. |
