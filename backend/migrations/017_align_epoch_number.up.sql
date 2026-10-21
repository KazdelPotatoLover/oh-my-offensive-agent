ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;
ALTER TABLE agent_tool_calls ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;
ALTER TABLE model_calls ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;

ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN ('queued','running','paused','stopping','completed','failed','stopped','interrupted'));
