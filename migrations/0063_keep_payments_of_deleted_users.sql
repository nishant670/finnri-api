-- Deleting the account of anyone who had ever paid failed in PostgreSQL:
-- AutoMigrate creates payments, so its foreign keys are GORM's
-- fk_payments_user and fk_payments_subscription with no ON DELETE clause
-- (NO ACTION), not the CASCADE / SET NULL that schema.go's CREATE TABLE
-- declares. The user delete, and the user_subscriptions delete before it,
-- hit those constraints.
--
-- Payments are kept as the financial record (GST, Razorpay reconciliation)
-- and lose only the link to the person: user_id becomes nullable and is set
-- to NULL when the user is deleted, as subscription_id already is meant to be
-- when the subscription goes. internal/database/schema.go runs the same
-- statements on every migrate.
ALTER TABLE payments ALTER COLUMN user_id DROP NOT NULL;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS fk_payments_user;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_user_id_fkey;
ALTER TABLE payments ADD CONSTRAINT fk_payments_user
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS fk_payments_subscription;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_subscription_id_fkey;
ALTER TABLE payments ADD CONSTRAINT fk_payments_subscription
    FOREIGN KEY (subscription_id) REFERENCES user_subscriptions(id) ON DELETE SET NULL;
