-- Test users: accounts that are not real customers (Google Play pre-launch
-- robots, the owner's and family's accounts, the owner's own test installs).
-- They keep working; the admin console leaves them out of its numbers.
-- schema.go applies this on every deploy; keep the two in step.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS is_test BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS test_reason VARCHAR(16) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS test_marked_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_users_is_test ON users (is_test);
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_test_reason_check;
ALTER TABLE users
    ADD CONSTRAINT users_test_reason_check
    CHECK ((is_test AND test_reason IN ('test_lab', 'internal', 'manual')) OR (NOT is_test AND test_reason = ''));

-- One-time backfill of production rows found on 2026-11-19 (guarded by
-- schema_repairs in schema.go). Matching on id plus creation second means
-- no other database has a matching row.
UPDATE users SET is_test = TRUE, test_reason = tagged.reason, test_marked_at = NOW()
FROM (VALUES
    (2, TIMESTAMPTZ '2026-08-15 22:50:05+00', 'internal'),
    (3, TIMESTAMPTZ '2026-08-16 06:02:47+00', 'internal'),
    (4, TIMESTAMPTZ '2026-08-16 22:16:08+00', 'internal'),
    (5, TIMESTAMPTZ '2026-08-17 08:26:16+00', 'internal'),
    (6, TIMESTAMPTZ '2026-08-18 12:16:08+00', 'internal'),
    (8, TIMESTAMPTZ '2026-08-18 20:02:10+00', 'internal'),
    (9, TIMESTAMPTZ '2026-08-18 22:07:11+00', 'internal'),
    (10, TIMESTAMPTZ '2026-08-19 13:53:17+00', 'internal'),
    (11, TIMESTAMPTZ '2026-08-20 10:23:33+00', 'internal'),
    (13, TIMESTAMPTZ '2026-08-24 08:57:36+00', 'internal'),
    (14, TIMESTAMPTZ '2026-08-29 10:52:52+00', 'internal'),
    (15, TIMESTAMPTZ '2026-08-29 12:32:28+00', 'internal'),
    (16, TIMESTAMPTZ '2026-08-31 09:16:00+00', 'internal'),
    (17, TIMESTAMPTZ '2026-08-31 19:57:19+00', 'internal'),
    (18, TIMESTAMPTZ '2026-09-02 17:11:39+00', 'internal'),
    (19, TIMESTAMPTZ '2026-09-02 17:12:21+00', 'internal'),
    (20, TIMESTAMPTZ '2026-09-04 18:28:40+00', 'internal'),
    (21, TIMESTAMPTZ '2026-09-09 16:22:44+00', 'internal'),
    (22, TIMESTAMPTZ '2026-09-11 10:21:02+00', 'internal'),
    (23, TIMESTAMPTZ '2026-09-12 04:56:55+00', 'test_lab'),
    (24, TIMESTAMPTZ '2026-09-12 04:58:58+00', 'test_lab'),
    (25, TIMESTAMPTZ '2026-09-12 05:00:58+00', 'test_lab'),
    (26, TIMESTAMPTZ '2026-09-12 05:22:49+00', 'test_lab'),
    (29, TIMESTAMPTZ '2026-09-19 08:12:11+00', 'internal'),
    (31, TIMESTAMPTZ '2026-09-21 07:25:51+00', 'internal'),
    (32, TIMESTAMPTZ '2026-09-21 07:25:58+00', 'internal'),
    (33, TIMESTAMPTZ '2026-09-21 07:29:18+00', 'internal'),
    (34, TIMESTAMPTZ '2026-09-21 07:55:05+00', 'test_lab'),
    (35, TIMESTAMPTZ '2026-09-21 10:18:48+00', 'test_lab'),
    (36, TIMESTAMPTZ '2026-09-21 11:01:13+00', 'test_lab'),
    (37, TIMESTAMPTZ '2026-09-21 11:02:13+00', 'test_lab'),
    (38, TIMESTAMPTZ '2026-09-21 11:05:21+00', 'test_lab'),
    (39, TIMESTAMPTZ '2026-09-21 11:33:57+00', 'test_lab'),
    (41, TIMESTAMPTZ '2026-09-26 02:49:31+00', 'internal'),
    (42, TIMESTAMPTZ '2026-09-26 02:55:33+00', 'internal'),
    (43, TIMESTAMPTZ '2026-09-26 03:17:24+00', 'internal'),
    (44, TIMESTAMPTZ '2026-10-03 10:30:58+00', 'test_lab'),
    (45, TIMESTAMPTZ '2026-10-03 10:32:31+00', 'test_lab'),
    (46, TIMESTAMPTZ '2026-10-03 10:38:01+00', 'test_lab'),
    (47, TIMESTAMPTZ '2026-10-03 11:06:56+00', 'test_lab'),
    (49, TIMESTAMPTZ '2026-10-19 08:41:08+00', 'test_lab'),
    (50, TIMESTAMPTZ '2026-10-19 08:46:49+00', 'test_lab'),
    (51, TIMESTAMPTZ '2026-10-19 08:51:46+00', 'test_lab'),
    (52, TIMESTAMPTZ '2026-10-19 10:31:28+00', 'test_lab'),
    (53, TIMESTAMPTZ '2026-10-19 10:58:57+00', 'test_lab'),
    (54, TIMESTAMPTZ '2026-10-21 16:04:08+00', 'internal'),
    (55, TIMESTAMPTZ '2026-10-21 16:23:58+00', 'test_lab'),
    (56, TIMESTAMPTZ '2026-10-21 16:24:11+00', 'test_lab'),
    (57, TIMESTAMPTZ '2026-10-21 17:14:38+00', 'test_lab'),
    (58, TIMESTAMPTZ '2026-10-21 19:23:18+00', 'internal'),
    (60, TIMESTAMPTZ '2026-10-23 14:13:01+00', 'test_lab'),
    (61, TIMESTAMPTZ '2026-10-23 14:47:09+00', 'test_lab'),
    (65, TIMESTAMPTZ '2026-10-23 16:39:44+00', 'test_lab'),
    (66, TIMESTAMPTZ '2026-10-26 18:00:46+00', 'test_lab'),
    (67, TIMESTAMPTZ '2026-10-26 18:01:32+00', 'test_lab'),
    (68, TIMESTAMPTZ '2026-10-26 18:35:51+00', 'test_lab'),
    (69, TIMESTAMPTZ '2026-10-26 20:38:59+00', 'test_lab'),
    (76, TIMESTAMPTZ '2026-11-05 04:20:07+00', 'test_lab'),
    (77, TIMESTAMPTZ '2026-11-05 04:52:17+00', 'test_lab'),
    (78, TIMESTAMPTZ '2026-11-05 05:31:41+00', 'test_lab'),
    (79, TIMESTAMPTZ '2026-11-05 12:29:51+00', 'test_lab'),
    (81, TIMESTAMPTZ '2026-11-10 02:01:53+00', 'test_lab'),
    (82, TIMESTAMPTZ '2026-11-10 02:49:16+00', 'test_lab'),
    (83, TIMESTAMPTZ '2026-11-10 02:50:49+00', 'test_lab'),
    (90, TIMESTAMPTZ '2026-11-18 03:59:41+00', 'test_lab'),
    (91, TIMESTAMPTZ '2026-11-18 04:00:26+00', 'test_lab'),
    (92, TIMESTAMPTZ '2026-11-18 04:34:09+00', 'test_lab'),
    (93, TIMESTAMPTZ '2026-11-18 04:34:29+00', 'test_lab')
) AS tagged(id, created_second, reason)
WHERE users.id = tagged.id
    AND date_trunc('second', users.created_at) = tagged.created_second
    AND NOT users.is_test;
