-- How a settlement was paid. The person asked to confirm a payment was told
-- only who and how much; "by UPI" or "in cash" is what lets them check it
-- against their own records. Empty for rows recorded before it was asked.
ALTER TABLE split_settlements
    ADD COLUMN IF NOT EXISTS payment_mode VARCHAR(24) NOT NULL DEFAULT '';

ALTER TABLE split_settlements
    DROP CONSTRAINT IF EXISTS split_settlements_payment_mode_check;
ALTER TABLE split_settlements
    ADD CONSTRAINT split_settlements_payment_mode_check
    CHECK (payment_mode IN ('', 'cash', 'upi', 'bank_transfer', 'card', 'wallet', 'other'));
