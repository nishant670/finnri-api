-- Release notes for each store build, shown on the app's "What's new" screen.
--
-- They live on the server because the update card is drawn by the build the
-- user already has, which cannot know what the next build contains. Admins
-- write them as drafts and publish them; the app reads only published ones.
--
-- min_supported marks a critical fix: builds below the highest published
-- release with it set must update before they can be used.
CREATE TABLE IF NOT EXISTS app_releases (
  id BIGSERIAL PRIMARY KEY,
  platform VARCHAR(10) NOT NULL,
  version_code INTEGER NOT NULL,
  version_name VARCHAR(20) NOT NULL,
  headline VARCHAR(140) NOT NULL,
  status VARCHAR(12) NOT NULL DEFAULT 'draft',
  min_supported BOOLEAN NOT NULL DEFAULT FALSE,
  published_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_app_releases_platform_version
  ON app_releases (platform, version_code);

ALTER TABLE app_releases DROP CONSTRAINT IF EXISTS app_releases_platform_check;
ALTER TABLE app_releases ADD CONSTRAINT app_releases_platform_check
  CHECK (platform IN ('android', 'ios'));
ALTER TABLE app_releases DROP CONSTRAINT IF EXISTS app_releases_status_check;
ALTER TABLE app_releases ADD CONSTRAINT app_releases_status_check
  CHECK (status IN ('draft', 'published'));
ALTER TABLE app_releases DROP CONSTRAINT IF EXISTS app_releases_version_code_check;
ALTER TABLE app_releases ADD CONSTRAINT app_releases_version_code_check
  CHECK (version_code > 0);

-- One card on the screen. Which columns apply depends on kind: problem and fix
-- belong to a fix, before_image and after_image to a new look, deep_link to a
-- feature. Images are names in the release-media store, not URLs.
CREATE TABLE IF NOT EXISTS app_release_items (
  id BIGSERIAL PRIMARY KEY,
  release_id BIGINT NOT NULL REFERENCES app_releases(id) ON DELETE CASCADE,
  position INTEGER NOT NULL DEFAULT 0,
  kind VARCHAR(12) NOT NULL,
  title VARCHAR(80) NOT NULL,
  body VARCHAR(400) NOT NULL DEFAULT '',
  problem VARCHAR(300) NOT NULL DEFAULT '',
  fix VARCHAR(300) NOT NULL DEFAULT '',
  image VARCHAR(64) NOT NULL DEFAULT '',
  before_image VARCHAR(64) NOT NULL DEFAULT '',
  after_image VARCHAR(64) NOT NULL DEFAULT '',
  deep_link VARCHAR(200) NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_app_release_items_release_id
  ON app_release_items (release_id, position);

ALTER TABLE app_release_items DROP CONSTRAINT IF EXISTS app_release_items_kind_check;
ALTER TABLE app_release_items ADD CONSTRAINT app_release_items_kind_check
  CHECK (kind IN ('new', 'ui', 'fix', 'improved'));
