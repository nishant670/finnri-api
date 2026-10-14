-- Categories a user named themselves, so the picker offers them again.
-- Until now a typed category ("Dividend") lived on its one entry and the
-- picker forgot it. Names the user's entries already carry are offered
-- without a row here; a row records one added from the picker before any
-- entry used it, or one the user removed (removed = TRUE). name_key is the
-- lowercased name, so "dividend" and "Dividend" are one category.
CREATE TABLE IF NOT EXISTS user_categories (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type VARCHAR(10) NOT NULL,
  name VARCHAR(40) NOT NULL,
  name_key VARCHAR(80) NOT NULL,
  removed BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_categories_user_type_key
  ON user_categories (user_id, type, name_key);

ALTER TABLE user_categories
  DROP CONSTRAINT IF EXISTS user_categories_type_check;

ALTER TABLE user_categories
  ADD CONSTRAINT user_categories_type_check
  CHECK (type IN ('expense', 'income'));
