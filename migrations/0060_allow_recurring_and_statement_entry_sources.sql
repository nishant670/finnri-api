-- The server writes two entry sources the check never allowed: 'recurring'
-- (autopay, and recurring payments logged or marked paid) and 'statement'
-- (rows imported from a card statement). Every such insert failed in
-- PostgreSQL. Widening only: every existing row already satisfies the new
-- check. internal/database/schema.go builds the same list from
-- models.EntrySources.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_source_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_source_check
    CHECK (source IN ('manual', 'text', 'voice', 'receipt', 'recurring', 'statement'));
