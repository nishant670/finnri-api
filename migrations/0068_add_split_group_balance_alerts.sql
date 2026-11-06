-- Group balance alerts.
--
-- split_groups.balance_alert_amount is the group's limit (0 = off). When any
-- one person's balance with a member in the group reaches it, that member gets
-- a notification and a push. split_group_balance_alerts remembers who has
-- already been told about which person, so it fires once per crossing; the
-- row is removed when the balance drops back under the limit.
--
-- internal/database/schema.go runs the same statements on every migrate.
ALTER TABLE split_groups
    ADD COLUMN IF NOT EXISTS balance_alert_amount NUMERIC(19,2) NOT NULL DEFAULT 0;

ALTER TABLE split_groups DROP CONSTRAINT IF EXISTS split_groups_balance_alert_amount_check;
ALTER TABLE split_groups ADD CONSTRAINT split_groups_balance_alert_amount_check
    CHECK (balance_alert_amount >= 0);

CREATE TABLE IF NOT EXISTS split_group_balance_alerts (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    friend_id BIGINT NOT NULL REFERENCES split_friends(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_balance_alert_pair
    ON split_group_balance_alerts (group_id, user_id, friend_id);
