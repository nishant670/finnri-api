-- A recurring payment can now have an end: a loan EMI is N payments, not forever.
-- total_instalments = 0 keeps the old open-ended behaviour.
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS total_instalments INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS instalments_paid INTEGER NOT NULL DEFAULT 0;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_instalments_check;
ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_instalments_check
    CHECK (total_instalments BETWEEN 0 AND 600 AND instalments_paid >= 0
        AND (total_instalments = 0 OR instalments_paid <= total_instalments));
