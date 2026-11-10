# Navo product and architecture assessment

Historical baseline, recorded before implementation. See [remediation status](REMEDIATION.md) for subsequent changes and current verification.

Assessment date: 10 November 2026, Asia/Ho_Chi_Minh. Repository root: `/Users/mthanhv/Projects/Navo`.

## Executive assessment

**Navo is a functional early operational product with a credible contract-to-delivery core. It is not ready to serve as the authoritative production record for money, corrections, and accounting exchange.** A restricted, supervised pilot is plausible after the critical defects below are fixed. Broad production rollout should wait for correction workflows, opening-data reconciliation, native recovery verification, and a measured restore drill.

The product's value is specific: help Nhật Duy SGN know what each industrial-equipment contract line still needs, where its goods are, what delivery is next, and what money is due. Sales, accounting, leadership, and administrators have different jobs. The strongest implementation choice is using ContractLine identity throughout purchasing and fulfillment; the same product can correctly appear twice with separate prices, quantities, and deadlines.

The intended commercial model is **one company per installation**. REQUIREMENTS O-16 explicitly excludes multi-tenancy, a SaaS back office, and SaaS billing. Their absence is not a defect. Operational professionalism is the immediate goal. The code deliberately gives Admin every permission, citing an owner decision dated 10 November in `internal/access/access.go`; this differs from the older requirements. I have not classified that deliberate behavior as an authorization bug. The decision needs reconciliation in the specification.

The main product risk remains the one identified by the requirements: this customer previously abandoned software because it was too complex. Finish reliable daily work and recovery before expanding campaigns, analytics, or AI scope. No rewrite or microservice split is justified by this review.

## Scope, method, and evidence limits

Read repository instructions for API, web, native, and infrastructure; product requirements and technical design; current routes, schemas/migrations, business handlers, shared clients, screens, integration tests, automation, CI/CD, and deployment/backup scripts. Reviewed all five code repositories at the subsystem level. This is not a proof that every line or possible interleaving is correct.

No application source was modified. Existing tests/builds generated ignored output. Targeted probes used temporary scripts under `/tmp`, new disposable local databases, generated test accounts, and isolated servers. Those databases were dropped. No cloud resources were created, no deployment ran, and no real customer email or paid AI request was sent.

Evidence labels:

- **Reproduced:** executed against the real current API/PostgreSQL or built web UI during this assessment.
- **Source verified:** directly established by current code; the described device, failure, or production scenario was not executed.
- **Incomplete:** required behavior is absent from the current schema/routes/UI.
- **Unverified:** depends on an external environment or acceptance evidence unavailable here.

### Checks executed

| Check | Result and qualification |
|---|---|
| API `make check` | Passed: formatting, vet, integration-tag vet, race unit tests. |
| API `make -e test-integration` | Passed against a new disposable database; nine integration test functions, including fulfillment, overrides, account changes, documents, finance, notes, contacts, and recorded mail sender. Package ran in about 148 seconds. |
| Web typecheck, lint, unit tests, production build | Passed; eight formatting tests. Build warns about a 587.88 kB minified JS chunk. |
| Mobile typecheck and lint | Passed. |
| Mobile Jest and iOS export | Passed, five formatting tests. Initial Jest attempt hit a sandbox Watchman permission error; retry used `--watchman=false`. Export is not native E2E. |
| Automation preparation/typecheck/harness checks | Passed; two harness safety/redaction tests. |
| Existing web/API regression | 10 passed, 13 failed: all six API cases passed; four of 17 web cases passed. Many web failures are stale selectors/assertions. This is not 13 established product bugs. |
| Current web quantity probe | Passed on an API-created active fixture using current UI controls: two POs, receipts, delivery, reload, and persisted API balance checks. Existing web vertical test separately reached customer/product/contract creation and activation before failing on an obsolete PO selector. No claim that the entire original UI suite passed. |
| Targeted finance/security/recovery probes | Reproduced the defects described in C1, C2, C3, C5, H1, and H2. |
| Infrastructure | Terraform format passed. Initial full `make check` was blocked by registry DNS/provider sandbox limits. Both Terraform configurations subsequently validated using installed providers outside the sandbox. Shell syntax and production/local Compose config checks completed. No infrastructure runtime claim. |

Current browser evidence: [regression results](../../navo-at/artifacts/runs/1794270764489-91471/results.json). Temporary probe evidence: [quantity balances](/tmp/navo-review-oAL6zA/ui-evidence/balances.json), [finance/API requests](/tmp/navo-review-oAL6zA/evidence/api-requests.json), [security/API requests](/tmp/navo-review-oAL6zA/security-evidence/api-requests.json), [receipt recovery/API requests](/tmp/navo-review-oAL6zA/recovery-evidence/api-requests.json). Each probe directory has a cleanup receipt.

Not executed: native device journeys on current source, Android/release binaries, real MISA SME import, real SMTP delivery/restart ambiguity, live AI accuracy/cost/latency, production upgrade/rollback, backup restore, load testing, or current advisory scans of dependencies. Old execution reports are historical evidence only. The old customer-field accessibility defect is fixed in current code: AT-W09 now passes.

## Architecture and model

| Layer | Current implementation | Assessment |
|---|---|---|
| Web | React/TypeScript/Vite, shared UI, bilingual locales, route permission guards, cookie session | Coherent small application. Shared field/drawer/client primitives are the right boundaries; recovery logic is still spread across screens. |
| Native | Expo/React Native, secure session token, shared API meanings, local note drafts | Appropriate separate native UI. Business types/formatters/locales are duplicated; offline drafts and freshness have important gaps. |
| API | Go/Gin modular monolith: auth/access, master data, core fulfillment, finance, documents, CRM, MISA, AI | Fits the deployment and team size. Transactions, explicit decimal strings, and domain-specific errors are valuable existing foundations. |
| Persistence | PostgreSQL; numbered migrations; contract/document lines, audit, idempotency, files/attachment links, milestones/allocations, notes/tasks, contacts/mailings | Strong composite foreign keys in fulfillment. Later finance and CRM paths do not consistently inherit the same invariants. Version columns largely lack optimistic preconditions. |
| Files | Private filesystem, UUID paths, MIME/size checks, scoped download, document tree and ZIP | Useful controls exist. Uploads are not idempotent; ZIP can silently omit missing file bytes. |
| Integrations | Synchronous Anthropic extraction, SMTP background goroutine, generated MISA workbook | Each has a different reliability model; none has full real-external end-to-end acceptance evidence here. |
| Operations | One EC2/Compose host, Caddy, app/migration DB roles, SSM delivery, off-instance DB/file backup, basic alarms | Reasonable starting topology, conditional on demonstrated restore and coordinated deployments. Availability/SLO claims remain unverified. |

## Core workflow map

“Verified” below applies only to the explicitly tested path, not all edge cases.

| Workflow | Frontend → backend → persistence/integration | Status |
|---|---|---|
| Account bootstrap, login, revoke/reset/logout | CLI first Admin or web users → auth/users → bcrypt hashes, hashed opaque sessions, roles/grants | API integration verified; browser auth passed. Invitation delivery/forced initial-password change are not implemented; current flow is administrator-issued passwords. |
| Customer/supplier/product/contact setup | Web master data → masterdata/contacts → organizations/products/contacts | Basic CRUD and validation verified in API/integration; web invalid-input test passed. Plant/department hierarchy, aliases, opening-data onboarding incomplete. |
| Manual contract draft and activation | New-contract form → create contract, separately create terms and upload file → contract/lines/milestones/files → activate with official numbering | Creation/activation observed in existing web run and API tests. Aggregate save/recovery is incomplete; no draft edit/amend/close/handover workflow. |
| AI-assisted contract | File chooser → synchronous extraction → proposed matches → human review → same contract save | Source verified, external AI unverified. Timeouts, usage controls, durable provenance incomplete. |
| Purchase and quantity approval | Contract → PO draft/confirm or override request → locked contract lines, composite FKs, quantities, audit/idempotency | Ordinary quantity path verified through current web; over-order approval/rejection verified in Go integration. Lost create responses and editable retry state remain gaps. |
| Receive accepted/rejected goods | PO → GR draft then confirm → GR lines; accepted quantities drive availability | Happy path verified. Lost confirmation response recovery reproduced broken. Returns/reversal missing. |
| Plan and confirm delivery | Contract line → DO draft/confirm → stock/contract checks → DO lines and shared progress | Current UI verified at 100 ordered / 70 accepted / 50 delivered / 50 remaining / 20 available. Last-stock competition and replay API tests pass. Actual dates lack future-event checks; returns and correction absent. |
| Acceptance → collectible obligation | Contract money tab → manually trigger milestone date/note → payment_milestones | Manual trigger reproduced. Not an acceptance document: no accepted quantities/delivery linkage or partial acceptance rule. Can trigger without any delivery or source note. |
| Invoice → receipt → allocation → correction | Finance UI/API → invoices, receipts, allocations/reversals → reports | Sequential Go integration verified. Concurrent receipt allocations broken; split-invoice paid presentation wrong; receipt/payment reversal incomplete. Full finance browser E2E unverified. |
| Supplier invoice/payment/advance | Finance UI → purchase invoice/optional invoice-linked payment → payments | Source and targeted real-API probes verified. Concurrent overpayment reproduced; unlinked payment can name a non-supplier; later allocation of an advance absent. |
| MISA mapping/import/export/download | MISA UI → code matching/batch marking → workbook rebuilt from live document/catalog rows | Workbook generation tested in Go integration, real MISA unverified. Historical batch selection changes after cancellation/code changes; opening catalog/balances import incomplete. |
| Document file/ZIP | Documents UI → owner/kind permission check → private bytes and attachment metadata → scoped download/ZIP | API integration verified for selected file permissions/download/tree/ZIP. Read-only user mutation policy and missing-file ZIP recovery require work. |
| Mobile note/task/photos | Local draft → note/client ID + optional task → sequential photo uploads → remove local draft | Source verified, current native journey unverified. Cross-account drafts, unsaved edits, and photo retry/checkpoint integrity are incomplete. |
| Leadership/mobile progress | Native API hook → same core reports → retained screen data/pull refresh | Shared backend verified; current native display/recovery unverified. Failed refresh can silently retain stale data; list is truncated. |
| Customer product email | Preview/send UI → store mailing/recipients → goroutine → SMTP → record status | Recorded sender integration passed. Live SMTP and crash recovery unverified; restart status semantics are unsafe. |
| Release/restore | Repo CI → images → SSM deploy → backup/migrate/restart → readiness; restore instructions | Configuration validates. Runtime release, migration compatibility, and full restore unverified. Shared host deployment coordination missing. |

## Critical blockers

Estimates are engineering person-days including focused regression tests, assuming current architecture and no hidden data migration complexity. They are planning ranges, not commitments; related work overlaps.

### C1. Finance locks do not prevent concurrent over-allocation or overpayment

**Evidence — reproduced.** [receipts.go:292](../internal/finance/receipts.go:292) reads the receipt and an allocation sum in the same statement that waits for its row lock. [invoices.go:409](../internal/finance/invoices.go:409) does the analogous sum for supplier payments. Under a controlled lock barrier, both requests start with the pre-commit statement snapshot. Two allocations of 80 against a receipt of 100 both returned 200: allocated 160, unallocated −60. Two payments of 80 against an invoice of 100 both returned 201: paid 160.

**Impact/workflow:** accounting can allocate money that does not exist or overpay a supplier despite apparent locking and valid individual inputs. AC-12 is not met.

**Fix:** acquire parent locks in a separate statement, then read dependent totals using a fresh statement after the lock. Define a consistent lock order covering allocate, reverse, term edit, payment, and invoice cancellation. Revalidate status, customer/supplier, currency, and available balances inside that transaction.

**Effort:** 2–4 days. **Acceptance:** deterministic barrier tests with distinct idempotency keys allow at most one of two competing 80-of-100 writes; persisted allocated/paid never exceeds the parent amount; rejected transactions leave no business/audit side effects. Include two receipts competing for one milestone and reversal/edit races.

### C2. Idempotent replay bypasses current field permissions

**Evidence — reproduced.** [httpx.go:152](../internal/httpx/httpx.go:152) returns previously stored JSON before rerunning the scoped handler. A salesperson with purchase-cost access confirmed a PO; Admin removed that grant; a fresh GET omitted prices, but replaying the old confirm key still returned `unit_price: 980.5`.

**Impact/workflow:** revocation is inconsistent; sensitive information remains accessible from stored command responses. A current route-level permission check alone is insufficient.

**Fix:** store canonical command outcome identifiers, then authorize resource access and shape the replay response for the current principal. Alternatively reauthorize/redact stored output before returning it, without rerunning the mutation.

**Effort:** 2–4 days. **Acceptance:** grant revocation and scope changes apply to old keys on every relevant command; effects still occur once; same-key/different-payload conflict continues to work.

### C3. Task creation crosses contract ownership boundaries

**Evidence — reproduced.** [crm.go:294](../internal/crm/crm.go:294) inserts supplied customer/contract IDs without checking scope or their relationship; its task query joins contract numbers. Another salesperson created a task against the fixture owner's contract and received `SC-26-0001` in the response. Routes have no task capability gate beyond authentication. Task create/done/reopen also omit the expected audit record.

**Impact/workflow:** unauthorized metadata disclosure, misleading customer/contract links, and incomplete traceability in field work. This differs from intentionally shared customer master data.

**Fix:** resolve the contract inside the caller's scope, enforce customer/contract consistency, require the intended task capability, and audit state changes transactionally. Also review file mutations: [router.go:123](../internal/router/router.go:123) requires only contracts.read for upload, so a nominally read-only leader can attach files; this latter observation is source verified.

**Effort:** 1–3 days. **Acceptance:** out-of-scope contract task creation returns 404 with no metadata/write; mismatched customer fails; permitted task create/done/reopen records audit; read-only roles cannot mutate document attachments unless explicitly granted.

### C4. Real operational corrections are missing

**Evidence — incomplete.** [cancel.go:66](../internal/core/cancel.go:66) only cancels drafts. Current routes have no confirmed receipt/delivery reversal, supplier/customer returns, partial cancellation of outstanding confirmed PO quantities, contract amendments, or contract close/cancel action. Receipt/payment creation posts immediately; no route reverses those money documents. Only receipt allocations can be reversed. Status columns and comments are not implementations of those workflows.

**Impact/workflow:** a mistaken confirmation, actual customer return, supplier short-close, or incorrect cash entry cannot be corrected through supported business actions. The customer would need off-system records or direct database intervention, undermining AC-05/06/07/13 and the product's authoritative totals.

**Fix:** implement the minimum linked correction documents and explicit contract/PO lifecycle commands. Preserve originals, require reason/source, calculate net quantities/money from original plus corrections, and enforce downstream dependency limits. Add draft edit before implementing more input automation. Version commercial amendments rather than overwriting confirmed terms.

**Effort:** 10–20 days, split into separately releasable receipt/delivery, cash, and contract/PO increments. **Acceptance:** receive 8 then return 3 yields 5; deliver 50 then customer-return 10 yields net 40; replacement delivery does not inflate contract completion; paid/allocated money reverses coherently; partial PO close cannot erase received goods; historical evidence remains traceable; retries and races cannot apply a correction twice.

### C5. Multi-step saves fail at interruption boundaries

**Evidence — reproduced for goods receipts; source verified for other forms.** [ContractDrawers.tsx:242](../../navo/src/pages/ContractDrawers.tsx:242) creates a new GR on every attempt but retains one confirm key. The catch branch attempts cancellation after any error, including a lost response after commit. Dropping the first successful confirm response and retrying in the same drawer produced `409 idempotency_payload_mismatch`; database state was one confirmed receipt plus one cancelled receipt. The UI remained in error.

[ContractNew.tsx:114](../../navo/src/pages/ContractNew.tsx:114) separately creates the contract, each milestone, and a file. Later failure leaves earlier writes; retry starts a new contract. Attachment failure is swallowed. PO creation retains a known created ID, which helps when confirm fails, but cannot recover a lost creation response; edited fields after that known creation are not PATCHed before confirmation.

**Impact/workflow:** users cannot tell whether stock or commercial records were saved; retries can create duplicate drafts or confirm data different from the form. Silent missing source attachments undermine trust.

**Fix:** use idempotent aggregate create commands where appropriate, or persist a resumable command/draft identity. Retain the same resource and same action key across uncertain outcomes; check status before retry/cancel; distinguish rejected commands from unknown results. Return partial-save recovery explicitly. Surface attachment failure with retry.

**Effort:** 4–7 days. **Acceptance:** lose the response after each create/confirm/term/file step, reload or retry, and get one intended business record with a clear final state. Changed input is either saved explicitly or rejected as a conflict. A failed attachment remains visibly pending.

### C6. Mobile drafts are shared across signed-in accounts

**Evidence — source verified; not reproduced on a device.** [drafts.ts:25](../../navo-mobile/src/lib/drafts.ts:25) uses one `navo.noteDrafts` key; Draft contains no installation/user identity. [session.tsx:19](../../navo-mobile/src/lib/session.tsx:19) removes only the token on logout. The next salesperson's home reads the same draft list and can send it with their own credentials. Notes without a contract can still be sent for shared customer records.

**Impact/workflow:** confidential unsent customer notes can appear under another account and be attributed to the wrong author on shared devices.

**Fix:** namespace local data by installation and immutable user ID; enforce ownership before reading/sending; make logout retention explicit; migrate/quarantine existing unowned drafts rather than assigning them to the next login. Include the account boundary in upload checkpoints.

**Effort:** 2–4 days. **Acceptance:** user A's drafts/photos are invisible and unsendable as user B, including after process restart, expired session, and switching server URL; returning as A follows the agreed retention policy.

## High-priority improvements

### H1. MISA exports are mutable and have no proven real-MISA contract

**Evidence — reproduced selection drift; source-verified integration limits.** [misa.go:67](../internal/misa/misa.go:67) joins current partner codes and filters live statuses even for historical export IDs. [misa.go:286](../internal/misa/misa.go:286) rebuilds the file from those rows. An exported two-invoice batch became one eligible row after invoice cancellation, and its partner code changed from KH-OLD to KH-NEW. The code comment claiming the same content every time is inaccurate. Cancellation only audits an exported flag; it creates no accounting correction queue. Exports are marked before workbook download/build, not after confirmed MISA import.

Accounts and column layouts are hardcoded at [misa.go:38](../internal/misa/misa.go:38), with a comment that customer validation is still required. Workbook amounts convert decimals to float64 despite input allowing up to 18 integer digits; no safe-export precision bound is enforced. No real template/import acceptance evidence was available.

**Impact/workflow:** historical accounting handoffs cannot reliably be reproduced; exported does not mean imported; recovery and month-end reconciliation can diverge. Treat this as a blocker before customers rely on MISA export, even if rollout initially excludes it.

**Fix:** immutable export items/payload or stored workbook+hash, versioned customer-approved mapping, separate prepared/downloaded/import-confirmed/correction-needed states, explicit re-export reason and reconciliation. Validate numeric limits rather than silently losing precision.

**Effort:** 5–10 days plus accountant/template acceptance. **Acceptance:** same batch yields the same logical workbook/hash after partner edits/cancellation; corrections appear separately; missing mappings are actionable; generated golden files import into a copy of the customer's exact MISA installation and reconcile by partner/currency.

### H2. Invoice settlement and supplier advances are not a coherent ledger

**Evidence — reproduced and incomplete.** [invoices.go:214](../internal/finance/invoices.go:214) assigns all milestone allocations to every sales invoice linked to that milestone. Two invoices of 50 each both reported paid 80 for one milestone allocated 80. Invoice creation also permits no milestone; it does not itself create an invoice-triggered obligation. Thus invoice entries and receivable balances can diverge unless users separately create consistent milestones.

[invoices.go:372](../internal/finance/invoices.go:372) only validates a supplier through an invoice when an invoice is supplied. An unlinked payment to a customer-only organization returned 201. Supplier advances have no later allocation command; payment has one optional invoice FK, rather than a separate allocation relation. Cancelling an invoice does not reconcile existing payments/obligations.

**Impact/workflow:** misleading paid/open indicators, unaccounted supplier advances, missing or duplicated obligations, and inaccurate cash reconciliation.

**Fix:** distinguish obligation settlement from invoice settlement. Show milestone paid at milestone level unless explicit invoice allocations exist. Define invoice-triggered obligations, support supplier advance allocations, validate partner role consistently, and enforce cancellation/correction effects.

**Effort:** 5–10 days. **Acceptance:** split invoices cannot each inherit the same cash; combined settled money never exceeds actual allocated money; an advance later allocates across two supplier obligations with residual preserved; invalid supplier is rejected; invoice and obligation totals reconcile without double counting.

### H3. Acceptance and payment terms can change commercial truth without sufficient evidence

**Evidence — reproduced/incomplete.** [milestones.go:378](../internal/finance/milestones.go:378) accepts an acceptance date without checking deliveries, accepted quantity, or required source note. A contract with no delivery successfully became collectible with an empty source. Triggering again with a new key replaces the date. [milestones.go:295](../internal/finance/milestones.go:295) allows term changes without a client version precondition; active term triggers/amounts can be rewritten. Creation has a contract-status guard that update/delete do not share. The percent/net/gross/partial-event decisions O-04/O-05 remain open in documentation; current contract value is net quantity × price.

**Impact/workflow:** accounting deadlines may change without a defensible acceptance event or commercial version, and concurrent edits can silently overwrite one another.

**Fix:** minimum acceptance document with delivery-line quantities and source evidence; immutable trigger occurrence plus explicit correction; version preconditions for terms/master/user edits; explicit agreed calculation basis. Preserve simple UI while putting the invariants in the service.

**Effort:** 5–10 days, overlapping C4/H2. **Acceptance:** cannot accept above delivered-unaccepted quantity; partial acceptance triggers only the agreed obligation amount; source/date are traceable; repeated submission is once-only; stale updates return 409 with user input retained; contractual changes preserve previous versions.

### H4. Onboarding and employee handover do not cover the customer's starting state

**Evidence — incomplete.** Current [router.go](../internal/router/router.go) exposes no handover or opening-balances workflow. Deactivating a salesperson revokes access but does not transfer their contracts/open tasks. Organization owner_id exists but customer assignment/team hierarchy is not implemented. [misa.go:453](../internal/misa/misa.go:453) only learns codes for already-existing entries; unmatched MISA rows are not imported as new catalog entries, and there is no opening receivable/payable import or preview/commit pipeline. Its matched counter increments even if an UPDATE affected zero rows because the code is already used.

**Impact/workflow:** the distributor must re-enter starting catalogs and keep legacy balances externally; salesperson departure can strand work. These directly affect adoption and continuity, unlike optional CRM expansion.

**Fix:** preview/commit import of necessary catalogs and opening balances, duplicate/ambiguous match review, accurate row outcomes, and an atomic handover preview transferring selected contracts/customers/tasks while preserving authorship. Implement plant/department detail only to the degree required by real initial data.

**Effort:** 7–12 days. **Acceptance:** anonymized customer starting files import/retry without duplicates and reconcile by currency; zero-row updates are not reported matched; successor sees selected work and old user loses access; historical author remains unchanged; transfer has actor, reason, and audit.

### H5. Mobile recovery can lose work or present stale data as current

**Evidence — source verified.** [contract/[id].tsx:68](<../../navo-mobile/src/app/(app)/contract/[id].tsx:68>) only exposes fetch errors when no data exists. Its independent documents request has no loading/error/retry state. NetInfo connectivity alone does not establish that the API is reachable. Contract list reads one page of 50 with no continuation; quick-note requests page_size=200 although the server caps valid page sizes at 100 and falls back to 25 for 200. The same invalid page size occurs in web Users.

[note.tsx:137](<../../navo-mobile/src/app/(app)/note.tsx:137>) closes without autosave/dirty confirmation. Photo URIs are picker locations, not durable copied draft assets. [drafts.ts:68](../../navo-mobile/src/lib/drafts.ts:68) uploads without a per-photo idempotency identity; a lost successful upload response duplicates the attachment on retry. Progress is saved internally, but retry from the still-open screen can save the older in-memory draft over that checkpoint. Editing a draft after its note has been posted does not update the existing server note.

**Impact/workflow:** factory visits on weak networks can lose notes/photos, duplicate uploads, omit selectable customers/contracts, or show old delivery figures without warning.

**Fix:** one tested freshness state for API failure/offline/stale data, timestamp and independent retries; searchable paginated selectors; durable user-scoped autosaved drafts and copied assets; explicit post-submit immutability/update policy; per-photo deduplication and resumable checkpoints.

**Effort:** 5–9 days. **Acceptance:** native tests cover API-down with internet up, documents-only failure, >50 contracts and >100 customers, back/kill/relaunch, partial photo completion and lost upload response. Content and ownership remain correct, stale values are labeled, and each intended attachment exists once.

### H6. Shared form behavior still risks lost input and inaccessible interactions

**Evidence — source verified.** [ui.tsx:164](../../navo/src/components/ui.tsx:164) restores focus and handles Escape but has no focus trap, background inertness, or dirty-state dismissal guard. Backdrop/Escape dismiss edited forms. [ui.tsx:99](../../navo/src/components/ui.tsx:99) renders error/hint IDs but does not consistently connect ordinary input children to them. The new Combobox handles its error association, explaining the now-passing AT-W09; that one passing control does not establish form-wide accessibility. Tabs/radio-style groups also lack the full keyboard interaction model.

**Impact/workflow:** long operational forms can be lost accidentally, and keyboard/screen-reader users have inconsistent access to fields and modal actions.

**Fix:** implement these behaviors once in shared primitives and apply them across forms. Preserve user input on navigation/session expiry where appropriate.

**Effort:** 2–4 days. **Acceptance:** keyboard focus stays in an open modal and returns to the opener; dirty dismissal is recoverable; every invalid field exposes its message; shared controls work with keyboard and screen reader in both languages at 375/768/1280 and breakpoint edges.

### H7. AI extraction has conflicting time budgets and weak operational bounds

**Evidence — source verified; live provider unverified.** API [main.go:50](../cmd/api/main.go:50) has WriteTimeout=60s, extraction [extract.go:145](../internal/ai/extract.go:145) permits three minutes, the web allows four, and Caddy allows five. A successful extraction after the API's write deadline cannot be assumed deliverable. There is no durable extraction job, per-user usage budget/concurrency cap, or persistent field/page provenance. Uploaded content is not retained as extraction provenance; final source attachment errors are swallowed. The prompt asks for a best reading on ambiguous numbers, with a UI confirmation flag, rather than leaving unreadable values empty as the design proposes.

**Impact/workflow:** long files can consume provider work and then fail to reach the user; repeated retries increase cost, and business source traceability is weak.

**Fix:** align bounded timeout behavior immediately; add cancellable/idempotent jobs if long extraction is required, quotas/concurrency caps, schema/domain validation, and retained source references. Keep manual entry available. Finalize O-17 data-use/provider choices before real customer documents.

**Effort:** 3–6 days plus real-document evaluation. **Acceptance:** delayed-provider tests at 59/61/180 seconds have deterministic UI outcomes; retries do not duplicate paid work; ambiguous values require review; source document/field provenance survives save; quota/provider failures preserve the user's work.

### H8. Release and recovery safeguards need executable acceptance

**Evidence — source verified/unverified runtime.** [deploy.sh](../../navo-infra/deploy/deploy.sh:8) has no host-wide lock. API and web live in separate repositories whose GitHub concurrency groups do not serialize one another; both rewrite shared images.env/.env. The API image pointer changes before backup/migration success, and early failure does not restore that pointer. Web health accepts HTTP redirects from port 80, which is weaker than checking the actual new HTTPS app/API. [router.go:51](../internal/router/router.go:51) readiness only pings the DB, not schema compatibility.

[backup.sh](../../navo-infra/deploy/backup.sh) streams a dump then copies files and emits a heartbeat; this is useful but not restore proof. Backup/script instances are not mutually excluded. ZIP export [documents.go](../internal/documents/documents.go:432) skips a missing stored file after starting a successful download. Existing monitoring covers backup and instance checks, with no demonstrated application availability/error/disk alert coverage or tested escalation. Do not claim the proposed RPO/RTO is achieved.

**Impact/workflow:** overlapping releases can corrupt release metadata; a failed migration can poison later rollback selection; backup/file failures may only emerge during an incident.

**Fix:** shared host deployment/backup locks, atomic release manifests, early-failure rollback of metadata, schema-aware readiness, HTTPS/version smoke checks, explicit ZIP failure reporting, and a separate-machine DB+files restore drill with checksum/ref checks. Add application/disk alerts and a responsible responder.

**Effort:** 4–8 days plus restore exercise. **Acceptance:** concurrent API/web deployment is serialized; failure injected at backup/migration/start/health stages preserves a known-good release; restore recovers money, permissions, files, and audit within an agreed measured target; missing bytes fail visibly; alerts reach an owner.

### H9. Documentation and automation no longer describe the current product

**Evidence — executed/source verified.** API/mobile READMEs list finance/files/notes as not built even though they exist. Requirements still describe the old Admin model; access.go records a later decision. Browser suite expects obsolete controls (`#contract-create-po` versus current `#contract-order-missing`, a button where New contract is a link, links named by contract title instead of customer). The suite's 13 failures therefore cannot be treated as either a green product or 13 product defects. Old DEFECTS.md says customer error association is broken; current AT-W09 passes. There is no maintained machine-readable OpenAPI contract; web/native manually port types/locales/formatters.

**Impact/workflow:** release confidence erodes, onboarding guidance is misleading, and independent repositories can drift unnoticed. Existing pass counts overstate confidence in new finance concurrency and mobile recovery paths.

**Fix:** update approved decision history and feature-status matrix; repair selectors to current semantic controls without weakening business assertions; add the reproduced defects as regression cases; maintain a schema/API contract; run the critical cross-repository suite as a release gate tied to exact commits.

**Effort:** 3–6 days initially, then part of each change. **Acceptance:** current documentation matches routes and permissions; critical UI/API regressions pass against an explicit API/web/mobile version set; known failures have owners and evidence; no historical defect is labeled current without reproduction.

## Longer-term opportunities

### L1. Bound reporting work and remove silent list truncation

**Evidence:** [contracts.go:286](../internal/core/contracts.go:286) loads all matching contracts and company-scoped line facts before slicing a page; receivables/receipt lists similarly aggregate in memory. Documents tree loads the whole hierarchy and notes perform repeated detail reads. This is a growth concern, not an observed current latency incident.

**Customer need:** fast daily status and reliable search as history grows. **Fix:** benchmark a representative customer dataset first, then push bounded filtering/pagination and aggregation into queries, add measured indexes, and share API-generated contracts rather than duplicate types. Do not add caches or services without measurements. **Effort:** 3–7 days after measurement. **Acceptance:** agreed p95/row/file workload is met with stable query counts/memory and no truncated user-visible results; web startup chunk splitting is measured on the customer's network.

### L2. Make email delivery status honest under restart

**Evidence:** [mailings.go:294](../internal/crm/mailings.go:294) starts an in-process goroutine. SMTP send happens before the sent-status write, whose error is ignored. Startup marks queued recipients failed/interrupted. A crash after SMTP acceptance but before status persistence leaves ambiguous delivery; there is no durable claim/lease or explicit retry command. This is source verified, not a reproduced external email failure.

**Customer need:** avoid resending duplicate messages and know what actually happened. **Fix:** keep this optional capability secondary to core operations; when used operationally, introduce durable recipient claims/attempt history, bounded retry and an explicit unknown-delivery state. SMTP cannot by itself guarantee exactly-once remote delivery. **Effort:** 3–5 days. **Acceptance:** restart before/after SMTP acceptance produces truthful statuses, safe operator recovery, stable content snapshots, and no automatic resend of uncertain deliveries.

### L3. Complete only the next workflows customers actually need

**Evidence:** [REQUIREMENTS sections 3 and 7.1](/Users/mthanhv/Projects/Navo/REQUIREMENTS.md:62) deliberately stage opportunities/quotes, warranty, and advanced analytics later and require real-user usability validation. Those workflows are absent rather than broken current capabilities.

**Customer need:** preserve the core's simplicity while eventually tracing equipment after delivery. **Fix:** run the documented daily tasks with the customer's sales/accounting/leader roles; add plant/contact hierarchy or warranty serial lineage when real data/tasks require it; keep campaigns and broader AI behind this evidence. **Effort:** 3–5 days for usability/data validation; subsequent features estimated separately. **Acceptance:** users complete daily workflows without spreadsheets/IDs or facilitator rescue, explain balances from source records, and meet the agreed ease/time-to-value targets before new scope is approved.

## Practical roadmap

| Order | Outcome | Dependencies and release gate |
|---|---|---|
| 1 — Contain correctness/security risk | C1 finance races; C2 replay authorization; C3 task boundaries; C6 draft ownership | Add deterministic regression cases first. Do not authorize production money or shared-device notes until these pass. Roughly 1–2 weeks for a small experienced team, depending on overlap. |
| 2 — Make daily writes recoverable | C5 aggregate/resumable saves; H2 ledger semantics; minimum cash and stock corrections from C4 | Resolve O-04/O-05 with accounting using example documents. Gate on response-loss, correction, and reconciliation scenarios. Roughly 2–4 further weeks. |
| 3 — Complete operational continuity | Remaining C4 lifecycle; H3 acceptance; H4 opening data/handover; H5/H6 native/forms | Rehearse the customer's actual starting data and a departing salesperson. Run current native journeys on intended platforms. Roughly 2–4 further weeks; some work can overlap step 2. |
| 4 — Prove integration and recovery | H1 MISA template/snapshots/reconciliation; H8 release/restore | MISA requires the actual customer template/test installation. Restore requires a separate environment and agreed RPO/RTO. No accounting handoff claim until external acceptance. |
| 5 — Controlled rollout and simplify | H9 living regression/docs; customer usability study; monitored pilot | Named owners for incidents/import/reconciliation; staged users/contracts; compare source documents and end-of-day quantities/money. Promote only after defects close and users can recover independently. |
| 6 — Optimize/extend from measurements | L1 reporting, L2 email robustness, L3 selected later workflows | Only after real workload and usage demonstrate value. No architecture rewrite or subscription/tenant platform work is needed for the current product. |

Do not add individual estimates together as a calendar forecast: correction, acceptance, ledger, and import work share schemas and tests. A realistic delivery plan needs team capacity and the customer's example files, which were not necessary to perform this assessment but are necessary to accept those implementations.

## Recommended end-to-end test plan

Use disposable real PostgreSQL and real built clients for happy paths. Faults should interrupt actual requests/transactions; do not return fabricated successful business responses. Assert persisted quantities, money, links, permissions, and audit—not only UI text or HTTP 200.

| Priority | Journey and variations | Required assertions |
|---|---|---|
| P0 | First Admin → create user → login web/native → role change → deactivate/reset/logout | Current permissions apply immediately; no stale-price replay; no cross-user drafts; sessions actually revoke; password reset and recovery are understandable. |
| P0 | Customer/product → draft contract with two same-product lines → terms/file → activate | Snapshot names and separate line IDs survive reload; numbering unique; each interrupted save resumes once; no silent missing attachment or duplicated contract. |
| P0 | Contract 100 → PO60+PO40 → accepted70/rejected2 → draft delivery → confirm50 | Ordered100/received70/delivered50/remaining50/available20; rejected goods never become available; draft delivery changes no actuals; second line unaffected; web/native/API agree. |
| P0 | Wrong-contract IDs, foreign sales IDs, all roles, restricted file kinds, task links, export/replay | 403/404 as intended; no hidden fields or metadata; no mutation by read-only roles; denied actions leave no rows/audit effects. |
| P0 | Over-order/delivery → request Admin → approve/reject/withdraw → concurrent decision | Exact quantities checked at decision/confirmation; one decision; no stock override; pending/rejected state clear; approval and business write atomic. |
| P0 | Lose response after contract/PO/GR/DO creation or confirmation; double-click; timeout; reload | One intended effect, stable action identity, truthful pending/saved state, clear next action, changed payload conflict handled without discarding input. Include the reproduced GR mismatch. |
| P0 | Concurrent last-stock deliveries, receipt allocations, milestone allocations, supplier payments | Barrier-controlled races; totals never exceed stock/cash/open obligations; audit/idempotency rollback together; different keys cannot bypass business limits. |
| P0 | Receipt100 → allocate80 → allocate only20 from later excess; reverse/reallocate; supplier advance then invoices | Residuals belong to correct partner/currency; no negative balance; split invoices do not duplicate paid amounts; wrong supplier/currency rejected; money corrections trace original entry. |
| P0 | Confirmed receipt correction, supplier return, customer return, replacement, PO short-close, amendment | Net figures and chronology correct; no hard delete; downstream quantities protect history; serial lineage when required; repeated corrections once-only. |
| P0 | Partial/full acceptance → payment trigger → correction | Accepted quantities cannot exceed eligible deliveries; delivery alone does not imply acceptance; source evidence/date required; obligations and due dates follow agreed rules without duplication. |
| P0 | Existing customer catalogs and opening balances → preview → resolve ambiguity → commit/retry | Accurate row outcomes, no duplicates/silent merges, per-currency reconciliation, validation errors recoverable. |
| P0 | MISA prepare → download → import into customer test copy → reconcile → cancel/correct/re-export | Frozen batch, correct mapping/precision, no unintended repeat, exact partner/currency totals, rejected external import recoverable; prepared versus imported distinguished. |
| P0 | Production-shaped upgrade/rollback and DB+file restore on a separate host | Non-owner app DB role; old/new schema compatibility; no concurrent release race; login/stock/money/files/audit survive; measured recovery and alerted failures. |
| P1 | Mobile note/photos offline → back/kill/relaunch → reconnect → partial upload → user switch | Durable assets, drafts owned by original account, one note/task/photo each, no unsaved loss or stale checkpoints. |
| P1 | Mobile reports with internet up/API down, documents-only failure, expiry, refresh recovery | Retained values visibly stale with timestamp; independent requests show errors/retry; successful refresh clears warnings; >50 contracts and larger catalogs remain reachable. |
| P1 | EN/VI, 375/639/640/767/768/1023/1024/1280+, keyboard and screen reader | No page overflow; contained table scroll; modal focus/dirty recovery; associated field errors; correct dates/decimals; all essential actions reachable. Current legacy selector failures must first be repaired. |
| P1 | AI delayed/error/ambiguous output, unavailable provider, oversized/unreadable file | Deterministic bounded job/request, preserved work, no duplicate paid extraction, no automatic confirmation, source traceability and reviewable uncertain values. |
| P1 | SMTP success/bounce/timeout/restart at each delivery boundary | Honest sent/failed/unknown state, consent respected, recipient isolation, explicit recovery without indiscriminate resend. |

Run a fast P0 API/quantity/security suite on every relevant change; run full current web/finance suites against the exact release commits; run native journeys on designated devices before release; run restore and MISA acceptance before go-live and after material storage/template changes. Treat unit, HTTP integration, browser, native, external-integration, and restore evidence as distinct layers.
