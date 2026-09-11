ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS refundable_amount NUMERIC(19,2),
    ADD COLUMN IF NOT EXISTS refund_expected_on TEXT,
    ADD COLUMN IF NOT EXISTS refund_reminder_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS refund_status VARCHAR(16);

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_refundable_amount_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_refundable_amount_check
    CHECK (refundable_amount IS NULL OR (refundable_amount > 0 AND refundable_amount <= amount));

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_refund_status_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_refund_status_check
    CHECK (refund_status IS NULL OR refund_status IN ('pending', 'received', 'written_off'));

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_refund_tracking_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_refund_tracking_check
    CHECK (
        (refundable_amount IS NULL AND refund_expected_on IS NULL AND refund_reminder_at IS NULL AND refund_status IS NULL)
        OR
        (refundable_amount IS NOT NULL AND refund_expected_on IS NOT NULL AND refund_status IS NOT NULL)
    );

-- Stored as the API's own YYYY-MM-DD string, exactly like entries.date beside
-- it. A SQL `date` comes back from Postgres as a timestamp, so the value the
-- API returned could not be handed back to it — editing a refundable entry was
-- rejected by its own validator. Recast here for databases built before this.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'entries'
          AND column_name = 'refund_expected_on'
          AND data_type = 'date'
    ) THEN
        ALTER TABLE entries
            ALTER COLUMN refund_expected_on TYPE TEXT
            USING to_char(refund_expected_on, 'YYYY-MM-DD');
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_entries_pending_refunds
    ON entries (refund_expected_on, refund_reminder_at)
    WHERE refund_status = 'pending';

-- The reminder ticker can run on more than one API replica. The database, not
-- an in-process read-before-write check, is the final idempotency boundary.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_refund_due_unique
    ON notifications (user_id, type, action_url)
    WHERE type = 'refund.due';
