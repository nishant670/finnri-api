-- The card catalogue, phase 1 ("Know my card") of
-- docs/CREDIT_CARD_SYSTEM_PLAN_2026-11-13.md.
--
-- card_products holds one row per real card a bank sells (HDFC Millennia, Axis
-- Flipkart), so Add card can offer the exact card instead of only the bank.
-- issuer_id is the app's provider id (hdfc, sbi, axis…), not a foreign key:
-- providers are a list in code (internal/http/account_providers.go).
--
-- The art is a sketch (§4.1): `drawing` names a pen drawing the app ships, the
-- real card's idea drawn fresh with no logos, coloured from the app's mood in
-- one of four tones. Fees are optional and filled only from the issuer's own
-- published terms, with source_urls and last_verified_at. Reward rules are
-- phase 2.
--
-- accounts.card_product_id links a card to its product. Deleting a product
-- clears the link (ON DELETE SET NULL) instead of being blocked. The network
-- is printed as plain text on the card, never as a logo.
--
-- schema.go repeats every statement here except the seed, which Migrate
-- inserts from internal/database/card_products_seed.json so admin edits are
-- never overwritten (ON CONFLICT DO NOTHING).
CREATE TABLE IF NOT EXISTS card_products (
  id VARCHAR(64) PRIMARY KEY,
  issuer_id VARCHAR(64) NOT NULL,
  name VARCHAR(80) NOT NULL,
  tone VARCHAR(8) NOT NULL,
  drawing VARCHAR(32) NOT NULL,
  orientation VARCHAR(10) NOT NULL DEFAULT 'landscape',
  art_idea VARCHAR(120) NOT NULL DEFAULT '',
  art_drawn_as VARCHAR(120) NOT NULL DEFAULT '',
  art_reviewed BOOLEAN NOT NULL DEFAULT FALSE,
  joining_fee NUMERIC(19,2),
  annual_fee NUMERIC(19,2),
  fee_waiver_spend NUMERIC(19,2),
  source_urls TEXT NOT NULL DEFAULT '[]',
  last_verified_at TIMESTAMPTZ,
  sort_order INTEGER NOT NULL DEFAULT 0,
  hidden BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_card_products_issuer_id
  ON card_products (issuer_id, sort_order);

ALTER TABLE card_products DROP CONSTRAINT IF EXISTS card_products_tone_check;
ALTER TABLE card_products ADD CONSTRAINT card_products_tone_check
  CHECK (tone IN ('deep', 'solid', 'tint', 'pale'));
ALTER TABLE card_products DROP CONSTRAINT IF EXISTS card_products_orientation_check;
ALTER TABLE card_products ADD CONSTRAINT card_products_orientation_check
  CHECK (orientation IN ('landscape', 'portrait'));
ALTER TABLE card_products DROP CONSTRAINT IF EXISTS card_products_fees_non_negative_check;
ALTER TABLE card_products ADD CONSTRAINT card_products_fees_non_negative_check
  CHECK ((joining_fee IS NULL OR joining_fee >= 0)
    AND (annual_fee IS NULL OR annual_fee >= 0)
    AND (fee_waiver_spend IS NULL OR fee_waiver_spend >= 0));

ALTER TABLE accounts
  ADD COLUMN IF NOT EXISTS card_product_id VARCHAR(64),
  ADD COLUMN IF NOT EXISTS card_network VARCHAR(16) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_accounts_card_product_id
  ON accounts (card_product_id) WHERE card_product_id IS NOT NULL;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS fk_accounts_card_product;
ALTER TABLE accounts ADD CONSTRAINT fk_accounts_card_product
  FOREIGN KEY (card_product_id) REFERENCES card_products(id) ON DELETE SET NULL;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_card_product_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_card_product_check
  CHECK ((card_product_id IS NULL AND card_network = '') OR type = 'credit_card');
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_card_network_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_card_network_check
  CHECK (card_network IN ('', 'visa', 'mastercard', 'rupay', 'amex', 'diners'));

-- The launch catalogue: 36 cards drawn in Round 19, their art approved by the
-- owner on 13 Nov 2026.
INSERT INTO card_products (id, issuer_id, name, tone, drawing, orientation, art_idea, art_drawn_as, art_reviewed, sort_order) VALUES
  ('hdfc-millennia', 'hdfc', 'Millennia', 'solid', 'dotgrid', 'landscape', 'Fine texture on blue', 'a dot field with one sweep', TRUE, 1),
  ('hdfc-regalia-gold', 'hdfc', 'Regalia Gold', 'deep', 'wedge', 'landscape', 'Black with a gold diagonal', 'a hatched wedge', TRUE, 2),
  ('hdfc-moneyback-plus', 'hdfc', 'MoneyBack+', 'tint', 'crossarrows', 'landscape', 'Two crossing arrows', 'two pen chevrons', TRUE, 3),
  ('hdfc-freedom', 'hdfc', 'Freedom', 'solid', 'arrow', 'landscape', 'An arrow pointing on', 'one chevron with speed lines', TRUE, 4),
  ('hdfc-swiggy', 'hdfc', 'Swiggy', 'solid', 'stripes', 'landscape', 'Diagonal stripes', 'our own hatching', TRUE, 5),
  ('hdfc-infinia', 'hdfc', 'Infinia', 'deep', 'shards', 'landscape', 'Angular shards', 'pen-drawn planes', TRUE, 6),
  ('hdfc-diners-club-black', 'hdfc', 'Diners Club Black', 'pale', 'globe', 'landscape', 'A world map', 'a wire globe', TRUE, 7),
  ('hdfc-indianoil', 'hdfc', 'IndianOil', 'solid', 'drop', 'landscape', 'Liquid fuel', 'a drop and flow lines', TRUE, 8),
  ('hdfc-pixel-play', 'hdfc', 'Pixel Play', 'tint', 'playdots', 'landscape', 'A play arrow of particles', 'a triangle of dots', TRUE, 9),
  ('hdfc-tata-neu-infinity', 'hdfc', 'Tata Neu Infinity', 'solid', 'coins', 'portrait', 'A brand letter (logo)', 'a coin stack instead', TRUE, 10),
  ('sbi-simplyclick', 'sbi', 'SimplyCLICK', 'solid', 'tree', 'landscape', 'A tree of fine dots', 'a branching pen tree', TRUE, 1),
  ('sbi-simplysave', 'sbi', 'SimplySAVE', 'deep', 'constellation', 'landscape', 'Night lights', 'a constellation', TRUE, 2),
  ('sbi-cashback', 'sbi', 'Cashback', 'solid', 'tick', 'portrait', 'A big bold stroke', 'a tick and coins', TRUE, 3),
  ('sbi-prime', 'sbi', 'PRIME', 'deep', 'burst', 'landscape', 'A burst of light', 'radiating pen rays', TRUE, 4),
  ('sbi-elite', 'sbi', 'ELITE', 'deep', 'mandala', 'landscape', 'Ornate filigree', 'our own mandala', TRUE, 5),
  ('sbi-bpcl', 'sbi', 'BPCL', 'deep', 'splash', 'landscape', 'A liquid splash', 'a pen swirl', TRUE, 6),
  ('sbi-bpcl-octane', 'sbi', 'BPCL Octane', 'deep', 'octane', 'landscape', 'The same splash', 'the swirl plus speed lines', TRUE, 7),
  ('sbi-irctc', 'sbi', 'IRCTC', 'deep', 'train', 'landscape', 'A train by a skyline', 'a line train and city', TRUE, 8),
  ('sbi-pulse', 'sbi', 'Pulse', 'solid', 'pulse', 'landscape', 'Pulse rings', 'rings and a heartbeat', TRUE, 9),
  ('icici-amazon-pay', 'icici', 'Amazon Pay', 'deep', 'parcel', 'landscape', 'A brand logo', 'a parcel instead', TRUE, 1),
  ('icici-coral', 'icici', 'Coral', 'solid', 'sphere', 'landscape', 'A glowing sphere', 'a contoured planet', TRUE, 2),
  ('icici-rubyx', 'icici', 'Rubyx', 'deep', 'gem', 'landscape', 'A ruby', 'a line-cut gem', TRUE, 3),
  ('icici-sapphiro', 'icici', 'Sapphiro', 'solid', 'crystal', 'landscape', 'A sapphire crystal', 'a faceted prism', TRUE, 4),
  ('axis-flipkart', 'axis', 'Flipkart', 'deep', 'ribbons', 'landscape', 'Flowing ribbons', 'two pen ribbons', TRUE, 1),
  ('axis-ace', 'axis', 'ACE', 'deep', 'silk', 'landscape', 'A silk wave', 'a bundle of wave lines', TRUE, 2),
  ('axis-my-zone', 'axis', 'MY Zone', 'deep', 'headphones', 'landscape', 'Headphones', 'line headphones', TRUE, 3),
  ('axis-atlas', 'axis', 'Atlas', 'deep', 'globe+route', 'landscape', 'A dotted globe', 'a wire globe with a flight path', TRUE, 4),
  ('axis-magnus', 'axis', 'Magnus', 'deep', 'feather', 'portrait', 'A feather', 'a pen feather', TRUE, 5),
  ('axis-airtel', 'axis', 'Airtel', 'deep', 'signal', 'landscape', 'A brand mark (logo)', 'a signal tower instead', TRUE, 6),
  ('idfc-first-power-plus', 'idfc-first', 'Power+', 'deep', 'car', 'landscape', 'A car outline', 'a pen car', TRUE, 1),
  ('idfc-first-wealth', 'idfc-first', 'Wealth', 'deep', 'aurora', 'landscape', 'Northern lights', 'aurora strokes over hills', TRUE, 2),
  ('au-lit', 'au-small-finance', 'LIT', 'pale', 'doodlering', 'landscape', 'A ring of doodles', 'our own doodle ring', TRUE, 1),
  ('au-zenith', 'au-small-finance', 'Zenith', 'solid', 'fern', 'landscape', 'Swirling fronds', 'radiating pen fronds', TRUE, 2),
  ('amex-membership-rewards', 'amex', 'Membership Rewards', 'tint', 'laurel', 'landscape', 'A portrait mark (logo)', 'a laurel wreath instead', TRUE, 1),
  ('standard-chartered-smart', 'standard-chartered', 'Smart', 'pale', 'orbits', 'landscape', 'A brand swirl (logo)', 'orbits instead', TRUE, 1),
  ('indusind-cred', 'indusind', 'CRED', 'pale', 'blocks', 'landscape', 'Silver with a colour block', 'stacked outline blocks', TRUE, 1)
ON CONFLICT (id) DO NOTHING;
