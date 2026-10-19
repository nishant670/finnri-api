-- Screenshots or files a user attaches to feedback, as upload URLs. Up to
-- three; enforced by the API. Admins read them through
-- GET /v1/admin/feedback/:id/attachments/:index.
ALTER TABLE feedbacks
    ADD COLUMN IF NOT EXISTS attachments JSONB NOT NULL DEFAULT '[]'::jsonb;
