-- Budget exhaustion is a bounded, useful stop. Worker-local exhaustion is
-- distinct from an execution error and is therefore a first-class work state.
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_status_check;
ALTER TABLE work_items ADD CONSTRAINT work_items_status_check
  CHECK (status IN ('pending', 'running', 'completed', 'failed', 'exhausted', 'cancelled', 'blocked'));

ALTER TABLE agent_sessions DROP CONSTRAINT IF EXISTS agent_sessions_status_check;
ALTER TABLE agent_sessions ADD CONSTRAINT agent_sessions_status_check
  CHECK (status IN ('running', 'completed', 'failed', 'exhausted', 'stopped', 'interrupted'));

ALTER TABLE attempts DROP CONSTRAINT IF EXISTS attempts_status_check;
ALTER TABLE attempts ADD CONSTRAINT attempts_status_check
  CHECK (status IN ('running', 'completed', 'failed', 'exhausted', 'stopped', 'interrupted'));

COMMENT ON COLUMN work_items.status IS
  'exhausted means the local worker budget ended; evidence is retained and the item was not completed';

CREATE INDEX IF NOT EXISTS idx_observations_run_asset ON observations(run_id, asset_id, created_at);
