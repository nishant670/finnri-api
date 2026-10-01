-- Standalone split bills carry the same descriptive fields as transactions.
ALTER TABLE split_bills
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS merchant TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS time TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS attachment TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS schema_repairs (
    name TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_repairs WHERE name = '0049_add_split_transaction_details') THEN
        RETURN;
    END IF;
    UPDATE split_bills AS bill SET
        mode = COALESCE(entry.mode, ''), category = COALESCE(entry.category, ''),
        merchant = COALESCE(entry.merchant, ''), tag = COALESCE(entry.tag, ''),
        time = COALESCE(entry.time, ''), attachment = COALESCE(entry.attachment, '')
    FROM entries AS entry WHERE bill.entry_id = entry.id AND bill.user_id = entry.user_id;
    INSERT INTO schema_repairs (name) VALUES ('0049_add_split_transaction_details');
END
$$;
