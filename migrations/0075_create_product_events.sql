-- Product analytics: what people do in the app, as a name and one short label.
-- No amounts, merchants, notes or free text are stored. See
-- internal/models/product_event.go and internal/http/product_events.go.
--
-- Historical record, like the other numbered files: AutoMigrate creates this
-- table through cmd/migrate.
CREATE TABLE IF NOT EXISTS product_events (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    name VARCHAR(48) NOT NULL,
    label VARCHAR(32) NOT NULL DEFAULT '',
    platform VARCHAR(16) NOT NULL DEFAULT '',
    app_version VARCHAR(24) NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_product_events_user_id ON product_events (user_id);
CREATE INDEX IF NOT EXISTS idx_product_events_name_at ON product_events (name, occurred_at);
