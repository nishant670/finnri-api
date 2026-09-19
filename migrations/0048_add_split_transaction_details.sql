-- Standalone split bills carry the same descriptive fields as transactions.
ALTER TABLE split_bills
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS merchant TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS time TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS attachment TEXT NOT NULL DEFAULT '';

UPDATE split_bills AS bill SET
    mode = COALESCE(entry.mode, ''), category = COALESCE(entry.category, ''), merchant = COALESCE(entry.merchant, ''),
    tag = COALESCE(entry.tag, ''), time = COALESCE(entry.time, ''), attachment = COALESCE(entry.attachment, '')
FROM entries AS entry WHERE bill.entry_id = entry.id;
