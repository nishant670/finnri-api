-- Translates a shared group's slots into friend rows each member actually owns.
--
-- A group roster is written in the owner's namespace, and split_participants
-- .friend_id must name a row its author owns. Without this table a member could
-- not record a debt against the group's owner at all.
CREATE TABLE IF NOT EXISTS split_group_member_links (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
    -- 'owner', or an owner-side split_friends.id as text.
    slot VARCHAR(32) NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    friend_id BIGINT NOT NULL REFERENCES split_friends(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One row per person per viewer. The unique index is what makes the sync
-- idempotent: it runs on every roster change and must never double-provision.
CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_member_links_unique
    ON split_group_member_links (group_id, slot, user_id);

-- Reading a bill back asks "which slot is this friend row of mine?".
CREATE INDEX IF NOT EXISTS idx_split_group_member_links_friend
    ON split_group_member_links (user_id, friend_id);
