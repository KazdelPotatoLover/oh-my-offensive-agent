ALTER TABLE agent_sessions DROP COLUMN IF EXISTS epoch_number;
ALTER TABLE attempts DROP COLUMN IF EXISTS epoch_number;
ALTER TABLE agent_tool_calls DROP COLUMN IF EXISTS epoch_number;
ALTER TABLE model_calls DROP COLUMN IF EXISTS epoch_number;
