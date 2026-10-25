-- A full refund ended the pass but left its unspent credits spendable: the
-- period grant has its own expiry, so a refunded Monthly still showed 3,525
-- credits while the daily cap had fallen back to the free tier's 50.
-- applyRefund now revokes the remainder at refund time; this takes back what
-- earlier refunds left behind. Credits already spent stay spent. Each revoked
-- grant gets an 'expiry' ledger row, the same as a grant running out.
--
-- Historical record only: internal/database/schema.go runs the same statement
-- once through cmd/migrate, keyed on schema_repairs.
WITH targets AS (
    SELECT grant_row.id, grant_row.user_id, grant_row.guest_device_id_hash, grant_row.credits_remaining
    FROM credit_grants AS grant_row
    WHERE grant_row.source = 'subscription_period'
        AND grant_row.credits_remaining > 0
        AND EXISTS (
            SELECT 1 FROM payments AS payment
            WHERE payment.subscription_id = grant_row.subscription_id
                AND payment.status = 'refunded'
        )
    FOR UPDATE
), cleared AS (
    UPDATE credit_grants AS grant_row
    SET credits_remaining = 0, updated_at = NOW()
    FROM targets
    WHERE grant_row.id = targets.id
    RETURNING grant_row.id
)
INSERT INTO credit_ledger
    (user_id, guest_device_id_hash, grant_id, direction, credits, balance_after,
     reason_code, idempotency_key, created_at)
SELECT targets.user_id, targets.guest_device_id_hash, targets.id, 'expiry',
    targets.credits_remaining, 0, 'subscription_refund_revoked',
    'refund:grant:' || targets.id, NOW()
FROM targets JOIN cleared ON cleared.id = targets.id
ON CONFLICT DO NOTHING;
