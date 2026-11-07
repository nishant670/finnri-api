-- "Use this card for…": phrases a user tied to one of their accounts, so a new
-- expense that mentions the phrase (fuel, BPCL, Swiggy) is pre-filled with that
-- account when the bill or the sentence did not say how it was paid.
--
-- phrase_key is the phrase lowercased with its spacing collapsed, and is unique
-- per user: one phrase picks one account, and tying it to another card moves it.
-- Both foreign keys cascade — a rule means nothing without its account.
CREATE TABLE IF NOT EXISTS account_rules (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  phrase VARCHAR(40) NOT NULL,
  phrase_key VARCHAR(40) NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_account_rules_user_phrase
  ON account_rules (user_id, phrase_key);

CREATE INDEX IF NOT EXISTS idx_account_rules_account
  ON account_rules (account_id);
