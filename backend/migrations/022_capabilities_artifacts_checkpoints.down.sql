DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM artifacts) OR EXISTS (SELECT 1 FROM context_checkpoints)
       OR EXISTS (SELECT 1 FROM exploration_revisions)
       OR EXISTS (SELECT 1 FROM work_items WHERE intent_fingerprint IS NOT NULL)
       OR EXISTS (SELECT 1 FROM exploration_edges WHERE NOT valid OR source_version > 1 OR target_version > 1) THEN
        RAISE EXCEPTION 'cannot rollback migration 022: preserved artifacts, checkpoints, intent identities or proof history exist';
    END IF;
END $$;

ALTER TABLE exploration_edges DROP COLUMN invalidated_at,DROP COLUMN invalid_reason,DROP COLUMN target_version,DROP COLUMN source_version,DROP COLUMN valid;
DROP TABLE exploration_revisions;
DROP TABLE context_checkpoints;
DROP TABLE artifacts;
DROP INDEX work_items_intent_identity;
ALTER TABLE work_items DROP COLUMN intent_fingerprint;
