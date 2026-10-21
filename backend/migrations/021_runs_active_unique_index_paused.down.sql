DROP INDEX IF EXISTS idx_runs_one_active_per_task;
CREATE UNIQUE INDEX idx_runs_one_active_per_task ON runs (task_id) WHERE status IN ('queued', 'running', 'stopping');
