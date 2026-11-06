-- AI usage rows that open the entry they made (P57), and bug reports that
-- carry the app and phone they came from (P59).
--
-- ai_usage_events.entry_id is set when an entry saved from an AI draft names
-- the read's request_id. No foreign key: deleting an entry must not be blocked
-- by, or cascade into, the usage ledger. The app shows a row whose entry is
-- gone as such.
--
-- feedback.app_version / device / os_version / screen are filled in by the app
-- when the person leaves "Include app and phone details" on. Empty otherwise.
--
-- internal/database/schema.go runs the same statements on every migrate.
ALTER TABLE ai_usage_events
    ADD COLUMN IF NOT EXISTS entry_id BIGINT;
CREATE INDEX IF NOT EXISTS idx_ai_usage_events_entry
    ON ai_usage_events (entry_id)
    WHERE entry_id IS NOT NULL;

ALTER TABLE feedback
    ADD COLUMN IF NOT EXISTS app_version VARCHAR(40) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS device VARCHAR(120) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS os_version VARCHAR(40) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS screen VARCHAR(80) NOT NULL DEFAULT '';
