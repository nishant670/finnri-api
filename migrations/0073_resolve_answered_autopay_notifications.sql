-- Autopay notifications that were already answered are done, not just read.
--
-- Confirm and Fix it on "Autopay transaction added" set read_at only, so the
-- app kept the notification in Needs you with its buttons (2026-11-11). The
-- handler now sets resolved_at; this clears the ones answered before that.
--
-- Applied on boot by internal/database/schema.go (runtimeSchemaStatements);
-- this file records the change. Idempotent.
UPDATE notifications SET
    resolved_at = COALESCE(o.confirmed_at, o.reverted_at, NOW()),
    read_at = COALESCE(notifications.read_at, o.confirmed_at, o.reverted_at, NOW())
FROM subscription_occurrences o
WHERE notifications.resolved_at IS NULL
    AND notifications.user_id = o.user_id
    AND o.status IN ('confirmed', 'reverted')
    AND (notifications.id = o.notification_id
        OR notifications.action_url = '/subscription-occurrences/' || o.id);
