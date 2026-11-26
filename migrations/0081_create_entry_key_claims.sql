-- Lets a client cancel a create it never got an answer to, so the same money
-- can safely be saved another way; see internal/http/entry_key_cancel.go.
-- AutoMigrate creates the table from models.EntryKeyClaim; this file documents it.
CREATE TABLE IF NOT EXISTS entry_key_claims (
    user_id BIGINT NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    cancelled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, idempotency_key)
);
