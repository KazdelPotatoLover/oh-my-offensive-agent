-- Migration 005 down: Rollback Agent Runtime tables and columns

DROP TABLE IF EXISTS agent_tool_calls;
DROP TABLE IF EXISTS model_calls;
DROP TABLE IF EXISTS agent_messages;
ALTER TABLE runs DROP COLUMN IF EXISTS final_output;
