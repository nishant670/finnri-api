-- Recurring income: a salary, rent received or a retainer runs on the same
-- schedule machinery as recurring payments, as kind 'income'. Its payments
-- are written as income entries and it is never summed into commitments.
-- schema.go re-applies this constraint on boot; keep the two in step.
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_kind_check;
ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_kind_check
    CHECK (kind IN ('subscription', 'loan', 'investment', 'bill', 'income'));
