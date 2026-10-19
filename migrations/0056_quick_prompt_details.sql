-- A quick prompt is the whole shape of a transaction the user repeats, not
-- just its title, amount, mode and category. The editor showed type,
-- merchant, tag and notes and dropped them on save, and had no way to say
-- which wallet or card to pay from — so every prompt fell back to the
-- default account for its mode.
ALTER TABLE quick_prompts
    ADD COLUMN IF NOT EXISTS type VARCHAR(10) NOT NULL DEFAULT 'expense',
    ADD COLUMN IF NOT EXISTS account_id BIGINT,
    ADD COLUMN IF NOT EXISTS merchant TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS notes TEXT NOT NULL DEFAULT '';

ALTER TABLE quick_prompts DROP CONSTRAINT IF EXISTS quick_prompts_type_check;
ALTER TABLE quick_prompts
    ADD CONSTRAINT quick_prompts_type_check CHECK (type IN ('expense', 'income'));

-- SET NULL, not a composite owned-account key like entries have: accounts
-- are hard-deleted, and a shortcut must never be the reason an account
-- cannot be removed. Ownership is checked when the prompt is saved.
ALTER TABLE quick_prompts DROP CONSTRAINT IF EXISTS fk_quick_prompts_account;
ALTER TABLE quick_prompts
    ADD CONSTRAINT fk_quick_prompts_account
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_quick_prompts_account_id ON quick_prompts (account_id);
