-- Guests prove themselves with a random secret; only its SHA-256 is stored.
--
-- Until now a guest was whoever sent the right device_id, which the app made
-- with Math.random and this table kept in the clear. The device id stays as the
-- key credits are counted against. The secret is what opens the account.
--
-- Historical record, like the other numbered files: the live statements are in
-- internal/database/schema.go and run through cmd/migrate.
ALTER TABLE users ADD COLUMN IF NOT EXISTS guest_secret_hash VARCHAR(64) NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_guest_secret_hash
    ON users (guest_secret_hash)
    WHERE guest_secret_hash <> '';
