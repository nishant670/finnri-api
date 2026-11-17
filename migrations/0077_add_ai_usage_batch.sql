-- Bill reads that belong to one "scan several bills" run (P95–P102).
--
-- The app sends X-Finnri-Batch with each read of a batch. The id lets the
-- failure cooldown count a batch's unreadable photos as one failure, caps a
-- batch at five charged reads, and lets admin AI usage group the reads.
-- Empty for every read made on its own.
--
-- internal/database/schema.go runs the same statements on every migrate.
ALTER TABLE ai_usage_events
    ADD COLUMN IF NOT EXISTS batch_id VARCHAR(64) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_ai_usage_events_batch
    ON ai_usage_events (batch_id)
    WHERE batch_id <> '';
