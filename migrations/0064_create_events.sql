-- Events group a user's own transactions by occasion ("Diwali party", "Goa
-- trip") so they can see later what it cost them. They are private: who owes
-- whom stays with split groups, and an event only says what a transaction was
-- for. An entry belongs to at most one event, so event totals never count the
-- same money twice.
--
-- Dates are for display and suggestions; an entry outside them may still
-- belong. Deleting an event unlinks its entries and never deletes them.
CREATE TABLE IF NOT EXISTS events (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name VARCHAR(60) NOT NULL,
  emoji VARCHAR(16) NOT NULL DEFAULT '',
  start_date DATE,
  end_date DATE,
  budget NUMERIC(19,2),
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_events_user_id ON events (user_id);

ALTER TABLE events DROP CONSTRAINT IF EXISTS events_dates_check;
ALTER TABLE events ADD CONSTRAINT events_dates_check
  CHECK (start_date IS NULL OR end_date IS NULL OR start_date <= end_date);
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_budget_check;
ALTER TABLE events ADD CONSTRAINT events_budget_check
  CHECK (budget IS NULL OR budget > 0);

ALTER TABLE entries ADD COLUMN IF NOT EXISTS event_id BIGINT;
CREATE INDEX IF NOT EXISTS idx_entries_event_id ON entries (event_id) WHERE event_id IS NOT NULL;
ALTER TABLE entries DROP CONSTRAINT IF EXISTS fk_entries_event;
ALTER TABLE entries ADD CONSTRAINT fk_entries_event
  FOREIGN KEY (event_id) REFERENCES events(id) ON DELETE SET NULL;
