
# Finance Parser API (Go 1.21 + Gin)

Accepts **audio** (or `hint_text`), transcribes with Whisper, parses with an LLM using a confirm-first policy, validates against JSON Schema, and returns structured JSON.

## Quick Start

```bash
go version
go mod tidy
cp .env.example .env
# edit .env and fill required redacted values such as OPENAI_API_KEY and DB_PASSWORD
go run ./cmd/server
```

If `cp .env.example .env` doesn't work, create it manually from the committed
template. `.env.example` lists every required variable with secrets redacted.

## Rotate the OpenAI key

The key is read only by the backend from `finnri-api/.env`.

1. Open `.env` and replace only the value after `OPENAI_API_KEY=`.
2. Do not add quotes or spaces, and never put the key in the mobile or web app.
3. If `OPENAI_API_KEY` was previously exported in your shell, run
   `unset OPENAI_API_KEY`; exported variables take precedence over `.env`.
4. Restart the backend; environment variables are loaded only at startup.
5. Confirm that the key's OpenAI project has billing/credits and access to
   `gpt-4o-mini` and `gpt-4o-mini-transcribe`.

`.env` is ignored by Git. Commit only `.env.example`, which contains
configuration names and safe defaults but no real secrets.

## Development cost controls

- Speech-to-text defaults to `gpt-4o-mini-transcribe`.
- Transaction parsing uses `gpt-4o-mini`; it is a small, low-cost model suited
  to focused JSON extraction.
- AI output is capped at 600 tokens with `OPENAI_MAX_OUTPUT_TOKENS`.
- Input is capped at 1,000 characters with `MAX_TRANSCRIPT_CHARS`.
- Auth JSON bodies are capped with `MAX_JSON_KB`; parse/upload request bodies
  are capped with `MAX_UPLOAD_MB`.
- Each parse logs token counts, not prompt content or the API key.
- The API does not retry failed OpenAI calls, preventing duplicate charges.

## Source text and transcript retention

- Uploaded voice audio is held in memory only long enough to transcribe it.
- `/v1/parse` returns a draft and does not persist parse attempts, transcripts,
  provider prompts, or raw provider responses.
- Confirmed entries may store `source_text` as transaction provenance. Editing
  or deleting the entry edits or deletes that stored text.
- A future `ParseAttempt` audit store must define a short retention window,
  access controls, and deletion job before it is enabled.

## Database migration

Schema changes have one production entry point: `cmd/migrate`. Railway runs the
compiled `/app/migrate` binary as a pre-deploy command, and a failure stops the
deployment before the new server receives traffic. The server process never
mutates schema during boot.

```bash
go run ./cmd/migrate
```

The numbered SQL files remain historical records; do not apply them separately
to a live database. Their current equivalents live in
`internal/database/schema.go` and execute only through the migration command.
The `0043` repair is keyed on a `schema_repairs` row. Its destructive half is
guarded for a reason:
it deletes settlements for friends with no expense history, which is right for a
leftover from a deleted group and wrong for a settlement somebody records before
their first expense.

## Auth verification

OTP codes are randomly generated and stored only as hashes. Verified OTPs issue
opaque, expiring, one-time `claim_token` values for registration or contact
updates; the token does not contain the email or phone number. For local manual
testing only, set `OTP_DEBUG_RESPONSE=true` to include `dev_otp` in the
`POST /v1/auth/otp/send` response. To force a predictable local-only code, also
set `OTP_DEV_CODE=123456`; this static code is ignored unless
`OTP_DEBUG_RESPONSE=true`.

Google login verifies Google ID tokens against the comma-separated
`GOOGLE_CLIENT_IDS` audience allowlist, then creates, links, or upgrades the
same Finnri user session returned by the email/mobile login flow.

## Account deletion

`DELETE /v1/user` permanently deletes the authenticated user's profile and owned
data: entries, accounts, budgets, subscriptions, split ledger records, quick
prompts, notifications, auth sessions, and matching OTP/claim verification rows.
Legacy local upload files referenced by entries are removed only when they
resolve safely under `uploads/`.

## Test
```bash
curl -X POST http://localhost:8080/v1/parse   -H "Authorization: Bearer test"   -F "hint_text=I spent 500 rupees today with my Amex card for my wife's birthday gift"
```

## FAQ
- **What does `cp .env.example .env` do?** Copies the template env file so you can edit secrets.
- **Which models are used?** `gpt-4o-mini` parses transactions and
  `gpt-4o-mini-transcribe` transcribes voice. Both are configurable in `.env`.
- **How do I replace the OpenAI key?** Replace the `OPENAI_API_KEY` value in
  `.env`, then restart the backend.
