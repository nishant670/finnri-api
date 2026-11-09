-- Wallet balances and pay-later accounts.
--
-- balance_tracked: a running balance used to appear only when the opening
-- balance was non-zero, so a wallet logged from its very first load (opening
-- exactly ₹0) could never show one. The flag says the user gave the account a
-- balance, whatever it worked out to.
--
-- credit_kind: '' for a card, 'pay_later' for a pay-later credit line such as
-- CRED flash. Both are credit_card accounts with a limit, statements and a due
-- date; only how the app names them differs.
--
-- internal/database/schema.go runs the same statements on every migrate.
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS balance_tracked BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS credit_kind VARCHAR(16) NOT NULL DEFAULT '';

-- Every balance entered before the flag existed was a real one.
UPDATE accounts SET balance_tracked = TRUE WHERE balance <> 0 AND type <> 'credit_card';

ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_credit_kind_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_credit_kind_check
    CHECK (credit_kind IN ('', 'pay_later') AND (credit_kind = '' OR type = 'credit_card'));
