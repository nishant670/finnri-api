# FINNRI Backlog

Last audited: 2026-07-11

This is the working checklist for FINNRI. Pick the first unchecked task in the
highest-priority section, complete it, verify it, then update this file in the
same change.

Legend:
- `[x]` Done in the current codebase and/or docs.
- `[ ]` Not done, not verified, or still conflicting with the docs.

## Deferred, scoped elsewhere

- [ ] Push notification delivery — `createNotification` writes a row and nothing
      sends it, so every notification is in-app only. Scoped in
      `docs/PUSH_NOTIFICATION_DELIVERY_PLAN.md`.

## Current Status

- Mobile app is the active MVP surface: `finnri-app/`.
- Backend API is active and tested: `finnri-api/`.
- Web dashboard is active in `finnri-web/` with live overview, insights,
  transactions, accounts, notifications, budgets, recurring payments, and EMI
  API integrations documented in `docs/WEB_DASHBOARD.md`.
- Backend tests pass with `GOCACHE=/private/tmp/finnri-go-build-cache go test ./...` when local `httptest` networking is allowed.
- Mobile lint errors and warnings have been addressed per the P1 quality gates.
- Web lint, TypeScript checking, and the production build pass.

## Docs And Planning

- [x] MVP scope documented in `docs/MVP_SCOPE.md`.
- [x] Product direction documented in `docs/PRD.md` and `docs/FINNRI_PRD_v2_Codex_Ready.md`.
- [x] Technical architecture documented in `docs/ARCHITECTURE.md` and `docs/technical-spec.md`.
- [x] API expectations documented in `docs/API_SPEC.md`.
- [x] AI parsing rules documented in `docs/AI_PARSING.md`.
- [x] Data model expectations documented in `docs/DATA_MODEL.md`.
- [x] Code review and PR-sized implementation plan documented in `docs/review-report.md`.
- [x] Reconcile docs with implementation route names: `/v1/entries` and `/v1/parse` are the canonical Phase 1.2 API routes.
- [x] Update `docs/review-report.md` so old baseline findings that are now fixed are clearly separated from current remaining gaps.
- [x] Decide whether `BACKLOG.md` should remain at workspace root or be copied into one of the three Git repos: canonical planning docs are copied into `finnri-api/` so they can be versioned with backend/API/security work.

## Completed MVP Foundations

- [x] Expo mobile app exists with Expo Router navigation.
- [x] Go/Gin backend exists with GORM and PostgreSQL.
- [x] Guest user creation exists at `POST /v1/auth/guest`.
- [x] Backend creates or ensures a default Cash account for new/existing guests.
- [x] Voice/text parse endpoint exists at `POST /v1/parse`.
- [x] Parse endpoint returns a draft only; it does not persist transactions.
- [x] Parser provider interface exists in `finnri-api/internal/ai`.
- [x] Parser normalizer exposes `confidence`, `needs_confirmation`, `missing_fields`, and `clarifications`.
- [x] Mobile confirmation modal displays AI review prompts and clarifications.
- [x] Manual transaction entry exists.
- [x] Explicit `Confirm & Save` flow exists before transaction persistence.
- [x] Backend entry CRUD exists at `/v1/entries`.
- [x] Backend transaction validation uses DTO-style input instead of binding directly to the GORM model.
- [x] Backend uses fixed-point `Money` for entries.
- [x] Backend defaults/validates INR for transaction currency.
- [x] Backend supports idempotency keys for transaction creation.
- [x] Backend transaction listing supports pagination and filters/search.
- [x] Backend account CRUD exists at `/v1/accounts`.
- [x] Account deletion is blocked when transactions reference the account.
- [x] Mobile account list/create/edit/default/delete flows exist.
- [x] Mobile confirmation modal can use API accounts.
- [x] Backend dashboard exists at `GET /v1/dashboard`.
- [x] Mobile dashboard/insight screen uses backend dashboard data.
- [x] Dashboard includes monthly/period spend, income, daily average, top categories, top merchants, account spending, recent transactions, and insight cards.
- [x] Backend has deterministic insight templates for period comparison, category increase, top merchant, account usage, and unusual spending.
- [x] Backend unit tests exist for money, parser normalization, parse handler behavior, entry validation/query/ownership, account validation, OpenAI client behavior, and dashboard insights.

## P0 - Fix Contract And Trust Gaps

- [x] Make `account_id` truly required for every new transaction, including Cash transactions.
- [x] Update `TransactionFormModal` validation so `accountId` is required before save.
- [x] Show/select the default Cash account instead of hiding account selection when `mode === "Cash"`.
- [x] Align backend validation with the product rule that create/update payloads require an owned `account_id`.
- [x] Resolve migration conflict where `0002_lock_transaction_contract.sql` makes `account_id` non-null but `0003_make_entry_account_optional.sql` makes it nullable again.
- [x] Decide the canonical API contract: keep `/v1/entries` as the MVP contract and align docs/OpenAPI accordingly.
- [x] Remove or defer receipt/document upload from the MVP flow, or harden it before external testing.

## P0 - External Beta Security

- [x] Replace forgeable `mock_token_*` bearer tokens with signed or opaque expiring sessions.
- [x] Store sessions server-side or make them revocable.
- [x] Replace static/mock OTP verification and plaintext `claim_*` tokens.
- [x] Stop logging database credentials and DSNs in `database.Connect()` and `cmd/server/main.go`.
- [x] Add restrictive production CORS configuration instead of defaulting to `*`.
- [x] Apply rate limiting to auth and AI endpoints.
- [x] Add request size/time limits consistently to upload/auth/parse paths.
- [x] Add `.env.example` with redacted required variables.
- [x] Document source-text/transcript retention and deletion policy.
- [x] Add account/data deletion path before any public beta.
- [x] Expose destructive mobile account deletion from Security & Privacy after
  backend deletion covers AI/billing records.
- [ ] Migrate Google sign-in from `expo-auth-session` to the native
  `@react-native-google-signin/google-signin` SDK before any release wider than
  friends testing. The browser flow depends on "Enable custom URI scheme" on the
  Android OAuth client, which Google marks as not recommended: any app can
  register the same `finnri://` scheme and intercept the redirect. PKCE limits
  the damage but the native SDK avoids the redirect entirely. Note this changes
  the ID-token audience, so `GOOGLE_CLIENT_IDS` must switch from the Android
  client ID to the Web client ID at the same time as the app ships.

## P1 - Quality Gates

- [x] Backend full Go test suite passes when local `httptest` networking is available.
- [x] Fix mobile lint errors.
- [x] Reduce or intentionally document mobile lint warnings.
- [x] Add mobile component tests for confirmation editing, uncertainty display, account selection, validation, and disabled double-submit.
- [x] Add mobile flow tests for guest first transaction, text parse, manual entry, edit/delete, and dashboard refresh.
- [x] Add API contract tests against `finnri-api/openapi.yaml`.
- [x] Add an end-to-end smoke test for guest capture -> parse -> confirm -> save -> dashboard update.
- [x] Document manual mobile QA for voice capture on a real device/simulator.
- [x] Document accessibility QA for labels, touch targets, font scaling, contrast, and reduced motion.

## P1 - Product Flow Polish

- [x] Shorten onboarding path so "Continue as guest" is reachable faster.
- [x] Restore or expose typed natural-language input on the home capture card, because text capture is part of MVP scope.
- [x] Improve empty/error/retry states across home, transactions, accounts, and dashboard.
- [x] Make server field-level validation errors user-friendly in mobile forms.
- [x] Ensure dashboard and transaction lists refresh after edits/deletes, not only creates.
- [x] Confirm date/time behavior with Asia/Kolkata defaults and client timezone handling.

## P1 - Data Model And Backend Hardening

- [x] Add persistent `ParseAttempt` model or explicitly defer it with a privacy rationale.
- [x] Add `Category` model or explicitly keep string categories for MVP with a migration plan.
- [x] Move larger dashboard aggregations from in-memory scans to bounded SQL queries before scale.
- [x] Add database constraints for positive amounts, supported transaction types, supported sources, and owned account references where feasible.
- [x] Decide whether account balances/credit limits should use fixed-point money instead of `float64`.
- [x] Standardize account type enum names across docs, backend, and mobile (`credit_card` vs `credit`, `wallet` vs `wallets`).

## P2 - Deferred / Later Phases

- [x] Improve recurring candidate detection and weekly review.
- [x] Add budget alerts after the core habit loop is reliable.
- [x] Add optional login/sync after secure sessions are implemented.
- [x] Add full bill splitting, friend balances, and settlements later; not MVP.
- [x] Add EMI tools later; not MVP.
- [x] Add full subscription manager later; not MVP.
- [x] Resume active web dashboard feature work with live API-backed insights,
  transactions, accounts, split ledgers, notifications, budgets, recurring
  payments, and EMI.
- [x] Add authenticated, filtered CSV export for transaction entries.
- [x] Add authenticated transaction summary report rollups.
- [ ] Add bulk editing later.
- [x] Add merchant history-backed autocomplete later: remember past merchants with category associations and suggest merchants when users create or edit transactions.
- [x] Add hardened receipt/document uploads with MIME/size validation and retention controls: content-sniffed allowlist (JPEG/PNG/HEIC/WebP/PDF), random filenames, and file cleanup on entry delete, attachment replacement, and account deletion.
- [ ] Move receipts to private storage. They are currently served from an unauthenticated static route and protected only by unguessable filenames, which is a capability URL rather than real access control. Needs a per-file owner record and an authenticated handler replacing `r.Static`.
- [ ] Reap orphaned uploads: a file uploaded through `/v1/upload` whose entry save then fails is never referenced and never cleaned up. Add a sweep to the existing maintenance job.
- [ ] Add statement imports/reconciliation later.
- [ ] Add bank/UPI/Account Aggregator integrations later.
- [ ] Consider open-ended AI financial advice only after separate product, safety, and compliance review.

## Maintenance Notes

- [x] End the Phase 1 web freeze and document the active web surface in
  `docs/WEB_DASHBOARD.md`.
- [x] Fix web lint and TypeScript/build compatibility for the resumed dashboard.
- [ ] Avoid adding new deferred features until P0 and P1 MVP items are done.
- [x] After completed privacy/account-deletion work, update the relevant code,
  tests, docs, and this checklist together.
