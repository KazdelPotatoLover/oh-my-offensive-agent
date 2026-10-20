-- Opaque provider items are session scoped through agent_messages.session_id.
ALTER TABLE agent_messages ADD COLUMN provider_payload JSONB;
ALTER TABLE model_calls ADD COLUMN response_id TEXT;
-- A missing usage object must not be interpreted as zero billable tokens.
ALTER TABLE model_calls ADD COLUMN usage_known BOOLEAN NOT NULL DEFAULT FALSE;
