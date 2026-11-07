-- Release notes are text only (finnri-app lib/whats-new). Each build draws its
-- own live previews from its real screens, in the viewer's theme, so nobody
-- uploads screenshots and the API no longer stores or serves them.
--
-- Files already uploaded to uploads/release-media/ are not referenced by
-- anything after this and can be deleted from the volume by hand.
ALTER TABLE app_release_items DROP COLUMN IF EXISTS image;
ALTER TABLE app_release_items DROP COLUMN IF EXISTS before_image;
ALTER TABLE app_release_items DROP COLUMN IF EXISTS after_image;
