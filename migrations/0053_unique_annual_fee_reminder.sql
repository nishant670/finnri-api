-- One annual-fee reminder per card per renewal year. The renewal year is part
-- of action_url, so this is the delivery guarantee for the reminder job.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_annual_fee_unique
    ON notifications (user_id, type, action_url)
    WHERE type = 'card.annual_fee';
