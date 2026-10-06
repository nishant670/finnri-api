-- Launch-offer orders record the promotion and the undiscounted amount, so
-- the offer's cap (500 buyers) and once-per-person rule are counted from
-- payments rather than from a separate counter that could drift.
ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS promotion_code VARCHAR(40) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS original_amount_minor BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_payments_promotion
    ON payments (promotion_code, status) WHERE promotion_code <> '';
