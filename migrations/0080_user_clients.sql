-- Which apps and browsers people use, for the admin console's Audience page.
-- One row per user per app install kind or browser; see internal/http/client_info.go.
-- AutoMigrate creates the table from models.UserClient; this file documents it.
CREATE TABLE IF NOT EXISTS user_clients (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    client_key VARCHAR(64) NOT NULL,
    platform VARCHAR(16) NOT NULL DEFAULT '',
    form_factor VARCHAR(16) NOT NULL DEFAULT '',
    device VARCHAR(80) NOT NULL DEFAULT '',
    os VARCHAR(40) NOT NULL DEFAULT '',
    app_version VARCHAR(24) NOT NULL DEFAULT '',
    app_build VARCHAR(16) NOT NULL DEFAULT '',
    variant VARCHAR(16) NOT NULL DEFAULT '',
    is_test_device BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_clients_key ON user_clients (user_id, client_key);
CREATE INDEX IF NOT EXISTS idx_user_clients_user_id ON user_clients (user_id);
CREATE INDEX IF NOT EXISTS idx_user_clients_platform ON user_clients (platform);
CREATE INDEX IF NOT EXISTS idx_user_clients_last_seen_at ON user_clients (last_seen_at);

-- Where an account was made (android, ios, web). Empty before 2026-11.
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_platform VARCHAR(16) NOT NULL DEFAULT '';
