-- What a parse turned out to be. A typed question (Ask Finnri, or a question
-- typed into Home's capture field) reserves transaction_parse_text before the
-- provider says it is a question, so the usage list showed every answer as
-- "Text capture". The action code and its price stay as reserved; this column
-- only records 'question' for parses that resolved to an answer. Empty for
-- everything else, including every existing row.
-- internal/database/schema.go applies the same column and check on boot.
ALTER TABLE ai_usage_events
    ADD COLUMN IF NOT EXISTS intent VARCHAR(16) NOT NULL DEFAULT '';

ALTER TABLE ai_usage_events
    DROP CONSTRAINT IF EXISTS ai_usage_events_intent_check;

ALTER TABLE ai_usage_events
    ADD CONSTRAINT ai_usage_events_intent_check
    CHECK (intent IN ('', 'question'));
