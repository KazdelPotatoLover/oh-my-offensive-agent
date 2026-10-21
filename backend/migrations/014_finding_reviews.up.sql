ALTER TABLE findings
    ADD COLUMN IF NOT EXISTS review_version INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_reviewed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_reviewer TEXT,
    ADD COLUMN IF NOT EXISTS last_review_comment TEXT;

CREATE TABLE IF NOT EXISTS finding_reviews (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    finding_id UUID NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    from_status TEXT NOT NULL CHECK (from_status IN ('pending_review', 'confirmed', 'false_positive', 'ignored')),
    to_status TEXT NOT NULL CHECK (to_status IN ('pending_review', 'confirmed', 'false_positive', 'ignored')),
    from_version INT NOT NULL,
    to_version INT NOT NULL,
    reviewer TEXT NOT NULL,
    comment TEXT NOT NULL,
    request_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_finding_reviews_finding_version UNIQUE (finding_id, to_version),
    CONSTRAINT uq_finding_reviews_finding_request UNIQUE (finding_id, request_id)
);

CREATE INDEX IF NOT EXISTS idx_finding_reviews_finding_id ON finding_reviews(finding_id);
CREATE INDEX IF NOT EXISTS idx_finding_reviews_run_id ON finding_reviews(run_id);
CREATE INDEX IF NOT EXISTS idx_finding_reviews_created_at ON finding_reviews(created_at);
