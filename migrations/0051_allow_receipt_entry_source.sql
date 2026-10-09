-- Entries drafted from a scanned bill or receipt record source = 'receipt'.
-- Widening only: every existing row already satisfies the new check.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_source_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_source_check
    CHECK (source IN ('manual', 'text', 'voice', 'receipt'));
