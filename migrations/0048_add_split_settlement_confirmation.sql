-- Gives a recorded payment a second side.
--
-- A settlement used to be a unilateral claim: whoever typed it moved both
-- ledgers, and the person it said had paid — or been paid — was never asked and
-- never told. The row now carries who has to agree to it and whether they have.
--
-- 'confirmed' is the default on purpose. Every settlement that already exists
-- was recorded under the old rule and has been living in both balances since;
-- defaulting them to 'pending' would raise a decision prompt for every payment
-- anybody has ever recorded.
ALTER TABLE split_settlements
    ADD COLUMN IF NOT EXISTS status VARCHAR(16) NOT NULL DEFAULT 'confirmed';

-- The Finnri account on the other side of this payment, when there is one. A
-- friend row with no `linked_user_id` stands for somebody who is not on Finnri,
-- so there is nobody to ask and the settlement is confirmed on creation.
ALTER TABLE split_settlements
    ADD COLUMN IF NOT EXISTS counterparty_user_id BIGINT REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE split_settlements
    ADD COLUMN IF NOT EXISTS responded_at TIMESTAMPTZ;

ALTER TABLE split_settlements
    DROP CONSTRAINT IF EXISTS split_settlements_status_check;
ALTER TABLE split_settlements
    ADD CONSTRAINT split_settlements_status_check
    CHECK (status IN ('pending', 'confirmed', 'denied'));

-- "What am I being asked to decide on?" is a question the app asks on every
-- launch, so it gets the index rather than a scan of everybody's payments.
CREATE INDEX IF NOT EXISTS idx_split_settlements_counterparty_status
    ON split_settlements (counterparty_user_id, status)
    WHERE counterparty_user_id IS NOT NULL;
