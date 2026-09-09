# FINNRI MVP Technical Specification

> Product authority: `/docs/PRD.md` and `/docs/MVP_SCOPE.md`. Supporting contracts live in `/docs/ARCHITECTURE.md`, `/docs/DATA_MODEL.md`, `/docs/API_SPEC.md`, `/docs/AI_PARSING.md`, and `/docs/UI_UX_GUIDELINES.md`. If this document conflicts with them, the updated product docs take priority.

## 1. Purpose and scope

FINNRI is an AI-first personal finance companion for young Indians. The MVP must prove one habit loop:

1. Start as a guest.
2. Speak or type a financial event.
3. Receive an AI-produced draft.
4. Review and edit every important field.
5. Explicitly confirm the transaction.
6. Save it against an account/payment source.
7. See it in the transaction list and basic dashboard.
8. Receive a simple, useful insight.

The mobile app is the MVP product surface. The existing Next.js client is retained, but active web-dashboard product work is deferred. Improve the existing Expo, Go/Gin, GORM, and PostgreSQL stack; do not rewrite it unless a measured blocker is documented.

## 2. Architecture

### 2.1 Intended MVP architecture

```text
Expo mobile app
  ├─ guest session and optional later account upgrade
  ├─ voice/text capture
  ├─ editable confirmation sheet
  ├─ accounts and transaction management
  └─ dashboard and insight cards
          │ HTTPS JSON/multipart
          ▼
Go/Gin API
  ├─ authentication and ownership boundary
  ├─ AI parse orchestration (draft only)
  ├─ accounts, transactions, dashboard, and insights
  ├─ validation and deterministic calculations
  └─ provider interfaces for AI and speech-to-text
          │
          ├────────► AI/STT providers
          │          no direct database access
          ▼
PostgreSQL through GORM
```

Parsing and persistence are separate operations. No parser, model provider, background task, or client-side convenience path may create a transaction. Only an explicit user confirmation may call the transaction-create endpoint.

### 2.2 Repository boundaries

- `finnri-app/`: primary Expo/React Native MVP client.
- `finnri-api/`: Go/Gin API and PostgreSQL persistence.
- `finnri-web/`: existing Next.js code, maintained only for security or low-cost compatibility fixes during MVP; not a Phase 1 deliverable.
- `docs/`: product and engineering source of truth.

### 2.3 Backend module boundaries

Keep the current service and introduce clearer internal boundaries incrementally:

- `auth`: guest identity, session issuance, optional account upgrade.
- `accounts`: account/payment-source CRUD, default selection, ownership checks.
- `transactions`: validation, CRUD, search/filter, and account linkage.
- `ai_parser`: provider-neutral transcription, parsing, normalization, confidence, and missing-field reporting.
- `dashboard`: deterministic monthly aggregates and recent activity.
- `insights`: deterministic templates; optional AI wording after facts are computed.
- `categories`: deferred for MVP; entries keep category names as strings until
  category taxonomy, icons, budgets, and user customization stabilize.
- `audit`: parse-attempt metadata and security-relevant events, with retention controls.

Handlers should accept request DTOs rather than binding database models directly. Services enforce ownership and validation; repositories perform persistence. This separation can be introduced within the current Go application without a service rewrite.

## 3. Frontend architecture

Use Expo Router for navigation, React Query for server-state caching/invalidation, and Zustand only for small local/session state. AsyncStorage may retain the guest session token and non-sensitive preferences; it is not the financial ledger.

### 3.1 Required MVP screens and flows

- Guest-first onboarding with a direct “Continue as guest” path.
- Home capture surface with both voice and text entry.
- Mandatory confirmation bottom sheet for AI drafts and manual entry.
- Account list plus create/edit/default-account actions.
- Transaction list with search and filters.
- Transaction detail with edit and delete.
- Basic dashboard with monthly spend, daily average, top categories, account-wise spend, recent transactions, and insight cards.

The confirmation sheet must:

- Display amount, type, category, merchant, account, date/time, note, and tags.
- Use real accounts returned by the API, not hardcoded names.
- Highlight low-confidence or missing fields and show concise clarification prompts.
- Require a valid amount, type, date, category, and owned account before save.
- Default currency to INR for the MVP while preserving a currency field.
- Keep the final “Confirm & save” action visually and technically distinct from parsing.

Network loading, empty, retry, validation, and destructive-action confirmation states are required. UI must follow the accessibility requirements in `/docs/UI_UX_GUIDELINES.md`.

## 4. Backend architecture and rules

- All protected reads and writes derive `user_id` from a verified session, never from request JSON.
- All entity lookups and mutations include ownership predicates.
- Account IDs submitted with transactions must belong to the authenticated user.
- Create and update use allowlisted DTOs plus server-side validation.
- Money uses an exact decimal representation in PostgreSQL (for example `numeric(14,2)`), not floating point.
- Dates are stored as a date and optional time/timestamp with explicit timezone semantics; relative dates are resolved using the client-provided IANA timezone, defaulting to `Asia/Kolkata`.
- Database changes use reviewed, reversible migrations. Runtime `AutoMigrate` is acceptable only during local prototyping and must not be the production migration strategy.
- Dashboard and insight calculations are deterministic database queries or tested domain functions.

## 5. Database and data model

The canonical MVP model follows `/docs/DATA_MODEL.md`. Product and domain
language should use "Transaction" for the saved user-facing record, while the
Phase 1.2 API route remains `/v1/entries` for compatibility with the
implemented contract.

### User

- `id`
- `guest_id` or public UUID
- optional phone/email and display name
- locale and currency, defaulting to India/INR
- guest/registered status
- timestamps

Guest records own data exactly like registered users. Upgrading a guest account must retain the same ownership relationship and transaction history.

### Account

- `id`, `user_id`
- `name`
- `type`: `cash`, `upi`, `bank`, `credit_card`, `debit_card`, `wallet`, or `other`.
  Legacy client aliases `credit`, `debit`, and `wallets` are normalized by the
  backend to the canonical values above.
- optional institution name and masked identifier/last four
- `credit_limit` and `balance` use the same fixed-point money storage as
  transaction amounts (`numeric(19,2)` in PostgreSQL, `models.Money` in Go).
- `is_default`
- timestamps

Accounts are user-defined payment sources, not bank connections. Store only data needed to identify the source; never store full card numbers, PINs, CVVs, or banking credentials. A default Cash account may be created for a new guest to keep first capture fast.

### Category

Deferred as a standalone table for MVP. The backend stores the user-confirmed
category name on `entries.category`, validates it as required, and uses that
string for filters and dashboard rollups.

Future normalized shape:

- `id`
- nullable `user_id` for system categories
- name, transaction type, icon/color, and `is_system`
- timestamps

Migration plan:

- Create `categories` with seeded system rows and optional user-defined rows.
- Backfill categories from distinct entry category strings per user and type.
- Add nullable `entries.category_id` while preserving `entries.category`.
- Dual-read and dual-write during one client compatibility window.
- Make `category_id` required only after mobile clients can edit and display the
  normalized category model.

### Transaction

- `id`, `user_id`, non-null `account_id`, required string `category`
- exact-decimal `amount`, `currency`, and type (`expense` or `income`)
- merchant, occurred date/time, note, tags
- source (`voice`, `text`, or `manual`)
- optional source text and AI confidence metadata under the retention policy in
  `docs/AI_PARSING.md`
- timestamps

`account_id` is the source of truth. A free-text payment mode or account label must not substitute for the foreign key. Historical transactions should retain a stable account reference; deleting a used account should be restricted or soft-deleted.

The database mirrors the API validation where PostgreSQL can enforce it:
`entries.amount > 0`, `entries.type IN ('expense', 'income')`,
`entries.source IN ('manual', 'text', 'voice')`, canonical account types, and a
composite owned-account foreign key from `entries(user_id, account_id)` to
`accounts(user_id, id)`.

### ParseAttempt

Parse attempts are explicitly deferred for MVP to avoid retaining raw financial
text unless the product has a clear privacy basis for doing so. The current
backend must not persist parse-attempt rows, raw transcripts, provider prompts,
or raw provider responses. If audit storage is added later, it must define:

- `id`, `user_id`, input type, source text/transcript
- provider/model metadata
- normalized draft, confidence, missing fields, status, and timestamp
- raw provider response only when justified, access-controlled, and covered by retention limits

Parse attempts never imply saved transactions. Avoid logging raw financial text.

### Insight

- `id`, `user_id`, template/type, title/body/severity
- optional related entity, dismissed timestamp, created timestamp

Persist insights only if dismissal/history requires it; otherwise compute them on request.

### Lightweight candidate metadata

`RecurringCandidate` and `SplitMetadata` are optional, lightweight annotations only. They must not grow into a subscription manager, group ledger, balance settlement system, or bill-splitting workflow in MVP.

## 6. AI parsing flow

1. The client captures text or temporary audio.
2. For voice, the API sends audio to the configured STT provider and receives a transcript.
3. The API sends transcript/text, current date, timezone, allowed enums, and the output schema to the parser provider.
4. A normalization layer validates types and enums, removes unsupported fields, resolves safe defaults, and marks uncertainty.
5. `POST /v1/parse` returns a draft only.
6. The mobile app maps the draft to the confirmation sheet and displays confidence, missing fields, and clarifications.
7. The user edits and selects an owned account.
8. Only “Confirm & save” calls `POST /v1/entries`.
9. The API validates independently and persists the confirmed payload.
10. Transaction, dashboard, and insight queries are invalidated/refetched.

The normalized draft contract contains:

- `amount`, `currency`, `type`, `merchant`, `category`
- `account_hint` only; the server must not invent or resolve it to an account without user confirmation
- `date`, optional time, note, and tags
- lightweight recurring/split candidate flags when confidently inferred
- field-level confidence, `missing_fields`, and concise clarifications
- source text/transcript in the draft response, and only on a saved entry when
  the user confirms persistence

Rules:

- Never hallucinate a merchant, category, or account.
- Unknown values remain null/missing and visible.
- The parsing endpoint has no transaction repository dependency.
- AI may word a deterministic insight but may not invent calculations.
- Raw voice is transient and deleted after transcription by default.
- Parse requests are not retained server-side in MVP. Confirmed entries may
  retain `source_text`; editing or deleting the entry edits or deletes that text.
- The provider is accessed through internal parser and transcription interfaces. Provider/model selection belongs in configuration, not handlers or UI.

## 7. Confirm transaction flow

The trust boundary is explicit:

```text
capture → parse draft → review/edit → explicit confirmation → validate → save
```

- Parse success must never trigger save automatically.
- Manual entries use the same final validation and explicit-save path.
- The create request contains only confirmed values, including `account_id`.
- The server does not trust a client `confirmed` boolean as proof; the API design itself separates draft parsing from persistence.
- Duplicate taps are prevented client-side and transaction creation should support an idempotency key.
- Validation failures return field-level errors without discarding the draft.

## 8. API expectations

`/docs/API_SPEC.md` defines the authoritative Phase 1.2 API contract. The MVP
keeps the implemented versioned routes: transaction persistence uses
`/v1/entries`, and draft parsing uses `/v1/parse`. Product language may call
saved records "transactions", but route names, clients, and OpenAPI should stay
aligned to this versioned contract.

### Required MVP capabilities

- `POST /v1/auth/guest`
- `POST /v1/parse` — draft only; text and audio supported
- `POST /v1/entries`
- `GET /v1/entries`
- `GET /v1/entries/:id`
- `PUT /v1/entries/:id`
- `DELETE /v1/entries/:id`
- `DELETE /v1/user`
- `GET /v1/accounts`
- `POST /v1/accounts`
- `PUT /v1/accounts/:id`
- `DELETE /v1/accounts/:id` with used-account safeguards
- `GET /v1/dashboard`
- `GET /v1/insights`

Transaction listing supports pagination plus search/filter by merchant, category, note, account, date range, type, and tags. Error responses use stable machine codes and field details. OpenAPI must cover implemented request/response schemas, authentication, pagination, and errors.

`GET /v1/dashboard` is a separate coarse-grained mobile payload. It should include monthly spend, daily average, top categories, account-wise spend, recent transactions, and a small set of insights. `GET /v1/insights` is a compatibility alias for the deterministic dashboard response. Both must honor requested date ranges consistently.

## 9. Security and privacy

- Keep guest bearer auth on opaque, expiring, server-side sessions; replace plaintext OTP claim tokens before any external beta.
- Guest access is anonymous, not unauthenticated: issue an opaque, revocable, expiring session bound to a server-side guest identity.
- Keep provider and database secrets outside source control; maintain a redacted `.env.example`.
- Never print DSNs, tokens, provider responses, transcripts, or transaction bodies in logs.
- Use HTTPS, restrictive production CORS, request-size limits, timeouts, and rate limits on auth and AI endpoints.
- Validate uploaded content by detected MIME type, extension, size, and generated filename; receipts are optional and may be deferred if safe storage is not ready.
- Minimize personal and financial data. Follow `docs/AI_PARSING.md` for
  source-text/transcript retention, and use `DELETE /v1/user` for account/data
  deletion.
- Store only masked payment identifiers. Encrypt sensitive fields/backups where appropriate.
- Apply authorization tests to every account and transaction route.
- Treat insights as spending analysis, not investment, tax, legal, credit, or regulated financial advice.

## 10. Testing and quality gates

### Backend

- Unit tests for parser normalization, enum/date handling, missing fields, deterministic insights, and transaction validation.
- Handler/integration tests for guest session, parse-without-save, transaction CRUD, filters, pagination, account ownership, and cross-user denial.
- Migration tests and database constraints for positive amounts, supported types, and account foreign keys.
  Current migrations enforce positive entry amounts, supported entry/source
  enums, canonical account type values, and owned account references.
- Contract tests against OpenAPI and malformed/provider-failure responses.

### Mobile

- Component tests for confirmation editing, uncertainty display, account selection, validation, and disabled double-submit.
- Flow tests for guest first transaction, voice/text parse, manual entry, edit/delete, and dashboard refresh.
- Manual voice capture QA on a real device/simulator is documented in
  `docs/MANUAL_MOBILE_QA.md`.
- Accessibility checks for labels, focus, font scaling, contrast, reduced motion,
  and 44×44 touch targets are documented in `docs/ACCESSIBILITY_QA.md`.

### End-to-end acceptance

- A new user saves a first INR transaction as a guest without registration.
- Parsing alone leaves transaction count unchanged.
- A parsed draft cannot save until required fields and an owned account are confirmed.
- Saved data appears in list, dashboard, and account aggregates.
- Search/filter/edit/delete work for the owner and fail for another user.
- At least five deterministic, fixture-tested insight templates are available.
- Raw audio is not retained by default.

Required CI gates are Go tests/vet, mobile lint/typecheck/tests, API contract tests, and a focused end-to-end smoke suite. Manual QA steps are documented where automation is not yet present.

## 11. Deferred technical work

The following are not Phase 1 deliverables:

- Full Splitwise-style groups with friend-to-friend ledgers beyond the current user-owned split ledger API.
- EMI planning workflow beyond the stateless calculator API.
- Full subscription manager.
- Web dashboard parity was not a Phase 1 deliverable. The later-phase API-backed
  dashboard foundation is now active; advanced reports, full-history exports,
  and bulk editing remain deferred. See `WEB_DASHBOARD.md`.
- Bank/UPI/Account Aggregator integrations.
- Statement import, OCR ingestion, reconciliation, or automated transaction creation.
- Open-ended conversational financial advisor.
- Investment, tax, legal, or credit advice.
- Offline ledger and multi-device conflict resolution.

Existing code for these areas may remain if isolated, secure, and low-maintenance, but it must not shape the MVP architecture, navigation, API contracts, or delivery sequence.
