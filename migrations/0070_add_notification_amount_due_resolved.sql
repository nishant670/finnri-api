-- Notifications that say their amount and date, and know when their job is
-- done (P48, P49, board R10-Notif-Proposals).
--
-- amount / due_date: what a reminder is for — a subscription's charge, a card
-- statement's total due, a refund expected back — so the list can show the
-- figure on the right without parsing the body.
-- resolved_at: set by the server when the job behind a notification is done
-- (the payment marked paid, the statement added, the settle-up decided, the
-- account set up). The app clears resolved items from "Needs you".
--
-- models.Notification is also auto-migrated; this file records the change.
ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS amount NUMERIC(19,2),
    ADD COLUMN IF NOT EXISTS due_date VARCHAR(10),
    ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_notifications_resolved_at
    ON notifications (resolved_at);
