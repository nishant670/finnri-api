-- An "other" group's own label and icon.
--
-- The four built-in kinds (trip, home, couple, other) stay as they are; a
-- group of kind 'other' may now carry the creator's own name for it ("Office",
-- "Diwali") and an icon name from the app's icon set. Both describe the group,
-- so every member sees the same pair. Empty on every other kind.
--
-- internal/database/schema.go runs the same statements on every migrate.
ALTER TABLE split_groups
    ADD COLUMN IF NOT EXISTS kind_label VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS kind_icon VARCHAR(48) NOT NULL DEFAULT '';
