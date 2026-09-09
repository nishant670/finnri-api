# FINNRI MVP Codebase Review

> Baseline review: 2026-07-09. Product authority: `/docs/PRD.md` and
> `/docs/MVP_SCOPE.md`. Findings below describe the pre-implementation baseline;
> the implementation update and per-PR statuses record subsequent progress.

> Implementation update: PR 1, PR 2, PR 3, PR 4, and PR 5 are implemented in
> the current working tree. This report now separates fixed baseline findings
> from current remaining gaps. Completed baseline gaps remain listed for
> traceability and should not be selected again.

## 1. Executive assessment

The repository has a useful prototype of most visible parts of the FINNRI habit loop: Expo mobile screens, guest creation, voice/text parsing, a review modal, transaction CRUD, account creation, and an insights endpoint. The existing Go/Gin/PostgreSQL stack is viable and should be improved rather than replaced.

The MVP is not yet external-beta ready. Account-linked transactions, visible
parser uncertainty, server-side transaction validation, INR consistency,
server-backed account management, pagination/search/filtering, and the real
dashboard are now implemented in the working tree. The highest-impact remaining
issues are logging and CORS/rate-limit hardening,
data-retention policy, QA coverage, and mobile lint failures.

The correct Phase 1 response is to finish and harden the core mobile flow. The Next.js dashboard, EMI-derived metrics, receipt uploads, advanced analytics, full subscriptions, bill splitting, imports, and open-ended advice must not displace that work.

## 2. Current codebase state

### Mobile: `finnri-app`

Present:

- Expo 54 / React Native 0.81 / Expo Router application.
- Multi-screen onboarding and a working “Continue as Guest” API path.
- Voice recording and typed input submitted to `/v1/parse`.
- Parsed draft mapped into a reusable transaction confirmation modal.
- Manual entry and an explicit “Confirm & Save” action.
- Transaction list, client-side search, server-backed filters, detail, edit, and delete flows.
- Account-creation form.
- Insights screen calling the backend.
- Zustand/AsyncStorage session persistence.

Incomplete or misleading:

- Onboarding requires five marketing screens before guest access; guest-first exists but first-value time is longer than necessary.
- Receipt/document upload is deferred from the MVP mobile flow; hardened upload support remains later-phase work.
- Mobile lint currently fails: 10 errors and 60 warnings in the inspected working tree.
- No automated mobile tests were found.

### Backend: `finnri-api`

Present:

- Go 1.23, Gin, GORM, and PostgreSQL.
- Guest user creation/reuse by device ID.
- Protected parse, entry, account, and insight routes.
- Text parsing and temporary in-memory audio forwarding to STT.
- JSON-schema validation of parser output.
- Owner-scoped entry/account reads and mutations in most handlers.
- Entry CRUD, idempotent create, pagination, and server search/filtering.
- Deterministic aggregate calculations and rule-based insight cards.
- Backend unit tests cover money, parser normalization, parse handler behavior, entry validation/query/ownership, account validation, OpenAI client behavior, and dashboard insights.

Incomplete or unsafe:

- Runtime `AutoMigrate` still exists in server startup; checked-in migrations are present, but production migration execution remains an operational gate.
- Parse attempts, normalized confidence history, categories, and reusable insight records/templates are not modeled.
- Insights load raw rows and aggregate in memory. This is acceptable for tiny prototypes but should move to bounded SQL queries before scale.
- Guest bearer auth now uses opaque, expiring, server-side sessions.
- OTP verification now uses random expiring OTP challenges and opaque one-time claim tokens.
- The upload endpoint still exists server-side and should stay inaccessible from MVP clients until hardened or removed.
- Database connection logging no longer emits DB env values or DSNs.
- Rate limiting is applied to auth endpoints and the AI parse endpoint.
- Request body limits and request context timeouts are applied to auth, parse, and upload paths.
- CORS now uses an explicit exact-origin allow list and does not default to `*`.
- The local `.env` contains secrets but is ignored by Git; the committed `.env.example` now lists required variables with secrets redacted.

### Web: `finnri-web`

Historical Phase 1 baseline:

- A substantial Next.js dashboard prototype existed with login, transactions,
  accounts, insights, and API wrappers.

Current later-phase status:

- The web dashboard is active outside the original mobile MVP boundary.
- Overview, insights, transactions, accounts, notifications, budgets,
  recurring payments, and EMI use live backend APIs.
- Web lint, TypeScript checking, and the production build pass.
- Remaining web scope and deliberate deferrals are tracked in
  `WEB_DASHBOARD.md`.

## 3. What aligns with the updated PRD

- The chosen Expo mobile and Go/Gin/PostgreSQL stack matches the recommended direction.
- The backend exposes a guest endpoint and mobile can continue as a guest.
- Mobile supports both voice and text capture.
- `/v1/parse` returns a draft and does not directly insert a transaction.
- Mobile opens an editable review modal and requires an explicit save action.
- Transaction list, filters, detail, update, and delete foundations exist.
- Account CRUD routes and account-creation UI exist.
- Audio is held in memory for transcription rather than deliberately persisted by the parse endpoint.
- Parser instructions include INR, Indian payment modes, relative-date context, uncertainty metadata, and confirm-first language.
- Insight calculations are primarily deterministic rather than an open-ended AI advisor.
- Ownership predicates are present on entry/account reads, updates, and deletes.

These are valuable foundations, not proof that the MVP acceptance criteria are met.

## 4. Baseline findings now resolved

The following 2026-07-09 findings have been fixed in the current working tree
or intentionally deferred from MVP. Keep them here for traceability, but do not
select them again as open backlog work.

| Original baseline finding | Current resolution |
| --- | --- |
| Accounts are not linked to transactions. | Entries now require an owned `account_id`; Cash transactions also use a real account, and account deletion is blocked while entries reference it. |
| Confirmation account choices are hardcoded. | Mobile confirmation uses API accounts and requires account selection before saving. |
| Parser uncertainty is invisible. | Mobile confirmation surfaces AI review prompts, missing fields, clarifications, and confidence-driven review states. |
| Create entry binds directly to the GORM model. | Backend create/update use DTO-style inputs with validation and ownership checks. |
| Amounts use floating point for entries. | Backend entries use fixed-point `Money`; INR defaults/validation are in place. |
| Transaction listing lacks pagination/search/account filtering. | Backend listing supports pagination plus type/category/mode/account/date/tag/search and amount filters. |
| Account management is incomplete on mobile. | Mobile account list/create/edit/default/delete flows are implemented. |
| Dashboard endpoint and deterministic insights are missing or hardcoded. | `GET /v1/dashboard` exists and mobile insight screen uses backend dashboard data. |
| API docs use unimplemented product route names. | `/v1/entries` and `/v1/parse` are the canonical Phase 1.2 routes in docs and OpenAPI. |
| Receipt upload is visible in MVP flow. | Mobile upload UI and `/v1/upload` client calls are removed; hardened uploads are deferred to a later phase. |
| Parse retention is undefined. | `docs/AI_PARSING.md` now defines transient audio handling, no MVP parse-attempt persistence, confirmed-entry `source_text` retention, and deletion behavior. |
| Account/data deletion is missing. | `DELETE /v1/user` now deletes the authenticated user's profile, owned data, sessions, matching verification rows, and safe local upload references. |

## 5. Current conflicts with the updated PRD

### Phase 1 conflicts

1. **Guest-first is implemented but not optimized.** A new user is routed through a lengthy onboarding sequence before guest creation.
2. **Mobile quality gates are not met.** Mobile lint still fails, and no automated mobile component/flow tests were found.
3. **Manual QA remains undocumented.** Voice capture, accessibility, and real-device/simulator flows still need explicit QA records.
4. **Some data-model hardening remains deferred.** Categories, SQL-bounded dashboard queries, and stronger database constraints remain future backend hardening work.

### Trust and security conflicts

1. The server-side upload endpoint still exists and must remain deferred, removed, or hardened before public exposure.
2. Local upload storage remains deferred and must stay hardened or unavailable before public exposure.

### Explicitly deferred scope found in the repository

- Next.js web dashboard was deferred during Phase 1; its later-phase foundation
  is now active and documented in `WEB_DASHBOARD.md`.
- EMI summary: remove from the MVP dashboard contract or hide behind future scope.
- Advanced/placeholder analytics and financial-health claims: replace with correct basic metrics.
- Receipt/document ingestion: defer unless it can be secured without delaying capture.

No Phase 1 work should add full bill splitting, settlements, an EMI calculator, a subscription manager, statement imports, bank integrations, advanced reports, or an open-ended AI advisor.

## 6. Critical MVP gaps

These block the stated acceptance criteria or user trust.

| Priority | Gap | Required outcome |
| --- | --- | --- |
| P0 | Secure guest/session boundary | Replace forgeable tokens and claims before external beta; preserve guest data on upgrade. |
| P0 | External-beta security baseline | Stop credential logging, add restrictive CORS, wire rate limits, add request limits, publish `.env.example`, document retention, and add deletion path. |
| P0 | Deferred upload exposure | Keep receipt/document upload out of MVP clients, and remove or harden the backend endpoint before external testing. |
| P1 | Automated core-flow coverage | Add backend, mobile, contract, and focused end-to-end tests. |
| P1 | Mobile lint and QA | Fix mobile lint errors, ratchet warnings down, and document real-device voice/accessibility QA. |
| P1 | First-value speed | Shorten onboarding so guest capture is reachable faster. |

## 7. Important but non-blocking gaps

- Reduce onboarding length and measure time to first confirmed transaction.
- Database constraints now enforce positive entry amounts, supported
  transaction/source enums, canonical account type values, and owned account
  references.
- Keep entry categories as required strings for MVP; normalize to a category
  table later after taxonomy, icons, budgets, and user customization stabilize.
- Add parse-attempt audit metadata with privacy-aware retention.
- Move growing dashboard aggregations into bounded SQL queries.
- Apply request IDs, redacted structured logs, timeouts, and rate limits.
- Add API contract tests against OpenAPI to prevent client/server drift.
- Resolve mobile lint errors and then ratchet warnings down.
- Resolve web lint only as maintenance; it is not an MVP launch blocker.
- Improve empty, error, offline, and accessibility states.

## 8. Recommended next pull requests

PR 1 through PR 5 below are completed baseline work kept for audit history.
The next active implementation sequence starts at PR 6.

### PR 1 — Lock the transaction/account contract

Status: implemented in the working tree; migration execution remains a deployment step.

- Define canonical MVP request/response DTOs and OpenAPI.
- Add `account_id`, exact-decimal amount, currency, source, and validated date fields.
- Add explicit migrations and a safe backfill/default Cash-account strategy.
- Add ownership and validation tests.

### PR 2 — Complete account management on mobile

Status: implemented in the working tree; manual mobile QA remains.

- Fetch and render real accounts.
- Create/edit/set-default/delete with used-account safeguards.
- Replace hardcoded confirmation choices with API accounts.
- Require an owned account on confirmed saves.

### PR 3 — Make AI confirmation honest

Status: implemented in the working tree; manual voice QA remains.

- Align prompt, JSON schema, normalizer, and mobile types with `/docs/AI_PARSING.md`.
- Add `missing_fields`, confidence, and clarifications to the UI.
- Keep account hints untrusted until user selection.
- Add parser fixtures for Indian phrasing, UPI/card/cash, dates, ambiguity, and provider failures.
- Prove in tests that parse requests do not change transaction count.

### PR 4 — Harden transaction CRUD

Status: implemented in the working tree; database migration and manual mobile QA remain.

- Replace model binding with create/update DTOs.
- Add idempotent create and stable field-error responses.
- Add server pagination and required search/filter behavior.
- Preserve edit/delete ownership checks and test cross-user attempts.
- Remove USD defaults and standardize INR.

### PR 5 — Ship a real basic dashboard and insights

Status: implemented in the working tree; manual mobile QA remains.

- Add `GET /v1/dashboard` for monthly spend, daily average, top categories, account-wise spend, recent transactions, and insight cards.
- Honor date ranges consistently.
- Remove hardcoded mobile metrics and hide EMI/advanced placeholder sections.
- Add at least five fixture-tested deterministic insights: month comparison, category increase, top merchant, account usage, and unusual spending.

### PR 6 — External-beta security baseline

- Replace static OTP and plaintext OTP-claim tokens with expiring, revocable verification claims. Status: implemented in the working tree.
- Stop credential/financial-data logging. Status: database credential and DSN startup logging is removed.
- Add rate limiting and restrictive production CORS. Status: implemented in the working tree.
- Add request size and timeout limits to auth, parse, and upload paths. Status: implemented in the working tree.
- Keep uploads deferred, or validate content and use private object storage/signed access before re-enabling.
- Add secret template, rotation procedure, retention policy, and data deletion path. Status: redacted `.env.example`, rotation procedure, source-text/transcript retention policy, and `DELETE /v1/user` are implemented.

### PR 7 — Quality gate and MVP acceptance suite

- Fix mobile lint errors and critical warnings.
- Add mobile component/flow tests and API contract tests.
- Add an end-to-end guest capture-confirm-save-dashboard smoke path.
- Document manual voice QA and accessibility checks.

## 9. Future roadmap items

Only after the Phase 1 habit loop is measured and reliable:

- Phase 2: better recurring-candidate detection, weekly review, budget alerts, improved deterministic insights, optional login/sync.
- Phase 3: full bill splitting, friend balances/settlements, credit-card reminders, EMI tools.
- Phase 4: active web dashboard development, exports, advanced reports/filters, bulk editing, statement imports.
- Phase 5: bank/UPI/Account Aggregator integrations, reconciliation, automated imports.
- Open-ended AI financial advice requires a separate product, safety, and compliance decision; it is not implicitly approved by any phase above.

Lightweight recurring or split candidate metadata may remain in MVP only when it is passive, editable, and does not add a management workflow.

## 10. Risks and technical debt

### Product risks

- A long onboarding flow may prevent users from reaching the core capture value.
- Any return of hardcoded or mathematically weak “financial health” statements would destroy trust faster than having fewer insights.
- Reintroducing optional account selection would make account-wise insight data unreliable.
- Too much visible future-scope UI can obscure the capture-confirm habit loop.

### Security/privacy risks

- OTP verification depends on an external delivery channel before public beta; local `dev_otp` responses must stay disabled outside development.
- The dormant public upload endpoint creates malware/XSS/privacy exposure if re-exposed without hardening.
- Future logging changes must continue to avoid DSNs, tokens, transcripts, and transaction bodies.
- Confirmed-entry `source_text` can still contain sensitive personal details; client copy and edit controls must make this visible to users.
- Local secrets are ignored by Git, but accidental sharing and historical exposure still need operational controls.

### Engineering risks

- Sparse mobile and contract tests mean confirmation, account-selection, ownership, and dashboard regressions are likely.
- OpenAPI is more complete, but contract tests are still needed to keep clients and server aligned.
- String dates/times and some free-text enums still create correctness and
  migration debt outside the fixed transaction amount path. Account balances and
  credit limits now use fixed-point money.
- Runtime `AutoMigrate` cannot safely represent reviewed production migrations.
- Both frontend lint suites fail, reducing the value of CI until errors are brought to zero.

## 11. MVP release gate

Do not call the MVP ready until all of the following are demonstrably true:

- A new user can create a guest session and save a first transaction without registration.
- Voice and text produce editable drafts.
- Parsing cannot save, and uncertainty is visible before confirmation.
- Every saved transaction links to an owned account/payment source.
- Transaction list/search/filter/edit/delete work.
- Dashboard values come entirely from persisted data and update after save/edit/delete.
- At least five deterministic insight templates pass fixtures.
- Raw voice is not stored by default.
- Session, ownership, validation, secret handling, and logging meet the external-beta baseline.
- Core acceptance flows have automated coverage and documented manual QA.
