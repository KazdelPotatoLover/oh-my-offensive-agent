DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM finding_reviews) THEN
        RAISE EXCEPTION 'cannot rollback migration 014: finding_reviews contains audit records';
    END IF;
    IF EXISTS (SELECT 1 FROM findings WHERE review_version > 0) THEN
        RAISE EXCEPTION 'cannot rollback migration 014: findings contains reviewed records';
    END IF;
END $$;

DROP TABLE IF EXISTS finding_reviews;

ALTER TABLE findings
    DROP COLUMN IF EXISTS last_review_comment,
    DROP COLUMN IF EXISTS last_reviewer,
    DROP COLUMN IF EXISTS last_reviewed_at,
    DROP COLUMN IF EXISTS review_version;
