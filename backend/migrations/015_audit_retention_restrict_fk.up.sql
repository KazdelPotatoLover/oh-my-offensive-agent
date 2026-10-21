-- Migration 015: Audit Retention - Replace CASCADE with named RESTRICT foreign keys
-- This ensures tasks, runs, and findings with human review audit history cannot be deleted.

ALTER TABLE finding_reviews
    DROP CONSTRAINT IF EXISTS fk_finding_reviews_finding,
    DROP CONSTRAINT IF EXISTS fk_finding_reviews_run,
    DROP CONSTRAINT IF EXISTS finding_reviews_finding_id_fkey,
    DROP CONSTRAINT IF EXISTS finding_reviews_run_id_fkey;

ALTER TABLE finding_reviews
    ADD CONSTRAINT fk_finding_reviews_finding
        FOREIGN KEY (finding_id) REFERENCES findings(id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_finding_reviews_run
        FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE RESTRICT;
