-- A card's annual fee and the yearly spend that waives it, read from its
-- statement or entered by the user. Zero means unknown.
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS annual_fee NUMERIC(19,2) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS fee_waiver_spend NUMERIC(19,2) NOT NULL DEFAULT 0;

ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS accounts_annual_fee_non_negative_check;
ALTER TABLE accounts
    ADD CONSTRAINT accounts_annual_fee_non_negative_check
    CHECK (annual_fee >= 0 AND fee_waiver_spend >= 0);
