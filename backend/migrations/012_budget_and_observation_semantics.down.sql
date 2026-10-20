-- Do not rewrite historical outcomes to fit an older application. Rollback is
-- deliberately refused if the new semantic state has already been persisted.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM work_items WHERE status='exhausted')
     OR EXISTS (SELECT 1 FROM agent_sessions WHERE status='exhausted')
     OR EXISTS (SELECT 1 FROM attempts WHERE status='exhausted') THEN
    RAISE EXCEPTION 'cannot downgrade: exhausted records require migration 012';
  END IF;
END $$;

DROP INDEX IF EXISTS idx_observations_run_asset;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_status_check;
ALTER TABLE work_items ADD CONSTRAINT work_items_status_check
  CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled', 'blocked'));
ALTER TABLE agent_sessions DROP CONSTRAINT IF EXISTS agent_sessions_status_check;
ALTER TABLE agent_sessions ADD CONSTRAINT agent_sessions_status_check
  CHECK (status IN ('running', 'completed', 'failed', 'stopped', 'interrupted'));
ALTER TABLE attempts DROP CONSTRAINT IF EXISTS attempts_status_check;
ALTER TABLE attempts ADD CONSTRAINT attempts_status_check
  CHECK (status IN ('running', 'completed', 'failed', 'stopped', 'interrupted'));
COMMENT ON COLUMN work_items.status IS NULL;
