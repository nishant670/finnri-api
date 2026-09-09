# Railway deployment (closed Android test)

Target: 5 testers, ~3 weeks, Android APKs distributed directly. Expected cost is
the $5/mo Railway Hobby plan, which includes $5 of usage credit — this workload
should stay inside that credit.

## Why this shape

- `cmd/server/main.go` starts `StartMaintenanceJobs` and
  `StartSubscriptionAutomation` as in-process tickers, so the service must stay
  running. Do not put this behind anything that scales to zero.
- `internal/http/handlers.go` writes receipt uploads to `./uploads` and serves
  them with `r.Static`. That path needs a volume or files vanish on redeploy.
- `numReplicas` is pinned to 1 in `railway.json`. A second replica would run a
  second copy of the subscription ticker and duplicate occurrences.

## 1. Push the deploy files

`Dockerfile`, `.dockerignore`, and `railway.json` are checked in. The 25 MB
`main` binary is currently tracked in Git; drop it so image builds stay small:

```bash
git rm --cached main && git commit -m "Stop tracking built binary; add Railway deploy config"
```

## 2. Create the Railway project

1. New Project → Deploy from GitHub repo → `nishant670/finnri-api`, branch `phase-1.0.2`.
2. Railway reads `railway.json` and builds with the Dockerfile. No build config needed.
3. Add a Postgres database to the same project (New → Database → PostgreSQL).
4. Service → Settings → Networking → Generate Domain. Note the
   `https://<name>.up.railway.app` URL.

## 3. Attach the uploads volume

Service → Settings → Volumes → New Volume, mount path `/app/uploads`.
Do this before the first real use; adding it later starts the directory empty.

After uploading a receipt from the app, confirm the returned `url` starts with
`https://`. The upload handler derives the origin from `X-Forwarded-Proto`
because Railway terminates TLS at the edge, and that URL is persisted on the
entry — an `http://` value would be stored permanently and then blocked by
Android's cleartext policy.

## 4. Environment variables

Set these on the **service** (not the database). Everything not listed has a
working default in `internal/config/config.go`.

| Variable | Value | Notes |
|---|---|---|
| `DATABASE_URL` | `${{Postgres.DATABASE_URL}}` | Railway reference variable. `internal/database/db.go` prefers this over the `DB_*` vars. Append `?sslmode=disable` if the internal connection errors on TLS. |
| `OPENAI_API_KEY` | your key | The only hard-required secret besides the DB. |
| `TZ_DEFAULT` | `Asia/Kolkata` | |
| `ALLOW_ORIGINS` | finnri-web origin, comma-separated | Browser-only. Leave empty if you are testing the APK alone. Never `*`. |
| `OTP_DEBUG_RESPONSE` | `true` | See the warning below. |
| `OTP_DEV_CODE` | `123456` | Optional: forces a fixed OTP so testers don't have to read it from the response. |
| `GOOGLE_CLIENT_IDS` | OAuth client IDs | Only if you enable Google login. |
| `TRUSTED_PROXIES` | leave unset | Defaults to private ranges, which is correct behind Railway's edge. Set to `none` only if the server is ever exposed directly with no proxy. |

`PORT` is injected by Railway and read by `config.Load()`. Do not set it manually.

> **`OTP_DEBUG_RESPONSE=true` returns the login code in the API response body.**
> There is no SMTP or SMS integration in this codebase, so this is the only way
> email/mobile login works today. It means anyone who knows a tester's email can
> log in as them. Acceptable for 5 known people on an unlisted URL for 3 weeks;
> it must be turned off before anyone else gets the URL, and real delivery has to
> be wired in before a public build.

## 5. Verify

```bash
curl https://<your-app>.up.railway.app/health
```

Expect `{"ok":true}`. Then check the deploy logs for
`connected to PostgreSQL successfully` and `listening on :8080`.

No manual migration step is needed. `AutoMigrate` builds the schema and
`EnsureRuntimeSchema` (`internal/database/schema.go`) then replays every
constraint the numbered files in `migrations/` apply — the `numeric(19,2)`
money columns, `account_id NOT NULL`, and the amount/type/source CHECK
constraints — idempotently on each boot.

## 6. Build the APK against this URL

The app falls back to `http://127.0.0.1:8080` when `EXPO_PUBLIC_API_URL` is
unset (`finnri-app/lib/transactions.ts`), and Android 9+ blocks cleartext HTTP —
so the variable must be set at build time and must be `https://`.

The `preview` profile in `finnri-app/eas.json` now carries it; replace the
placeholder with your real Railway domain, then:

```bash
cd finnri-app && eas build --profile preview --platform android
```

EAS returns a download link you can send to your testers.

## After the test

- Turn off `OTP_DEBUG_RESPONSE` and wire real OTP delivery. This is the one
  known gap being carried into the test deliberately.
