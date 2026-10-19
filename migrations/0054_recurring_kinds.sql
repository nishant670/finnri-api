-- Subscriptions become the one home for every recurring payment: a kind
-- (subscription, loan, investment, bill) plus optional loan and investment
-- details. Additive: existing rows keep working and are filed by kind below.
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS kind VARCHAR(16) NOT NULL DEFAULT 'subscription',
    ADD COLUMN IF NOT EXISTS loan_type VARCHAR(24),
    ADD COLUMN IF NOT EXISTS lender VARCHAR(120),
    ADD COLUMN IF NOT EXISTS principal NUMERIC(19,2) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS annual_rate_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS processing_fee NUMERIC(19,2) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS foreclosure_charge_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS start_date VARCHAR(10),
    ADD COLUMN IF NOT EXISTS platform VARCHAR(120),
    ADD COLUMN IF NOT EXISTS step_up_pct DOUBLE PRECISION NOT NULL DEFAULT 0;

-- File what already exists. EMIs were created through the EMI tag, and a
-- fixed number of instalments only ever meant a loan.
UPDATE subscriptions SET kind = 'loan'
    WHERE kind = 'subscription' AND (transaction_tag = 'EMI' OR total_instalments > 0);
UPDATE subscriptions SET kind = 'investment'
    WHERE kind = 'subscription' AND (transaction_tag = 'Investment' OR purpose_type = 'investment');

CREATE INDEX IF NOT EXISTS idx_subscriptions_kind ON subscriptions (kind);

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_kind_check;
ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_kind_check
    CHECK (kind IN ('subscription', 'loan', 'investment', 'bill'));
