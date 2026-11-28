-- A recurring item made while saving a transaction can be sent twice when the
-- first answer was lost (the app gives each save step 30 s). The second create
-- carries the same Idempotency-Key and gets the first row back, not a copy.
-- internal/database/schema.go applies the same statements on boot.
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(128);

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_user_idempotency
    ON subscriptions (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
