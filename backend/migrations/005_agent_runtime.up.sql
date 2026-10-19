-- Migration 005: Agent Runtime (messages, model calls, tool calls, and final output)

ALTER TABLE runs ADD COLUMN IF NOT EXISTS final_output TEXT;

CREATE TABLE IF NOT EXISTS agent_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq INT NOT NULL,
    role VARCHAR(50) NOT NULL,
    content TEXT NOT NULL DEFAULT '',
    tool_call_id VARCHAR(255),
    tool_calls JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_agent_messages_run_id ON agent_messages (run_id);

CREATE TABLE IF NOT EXISTS model_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round INT NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'started',
    finish_reason VARCHAR(100),
    request_id VARCHAR(255),
    prompt_tokens INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    total_tokens INT NOT NULL DEFAULT 0,
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_model_calls_run_id ON model_calls (run_id);

CREATE TABLE IF NOT EXISTS agent_tool_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    model_call_id UUID REFERENCES model_calls(id) ON DELETE CASCADE,
    call_id VARCHAR(255) NOT NULL,
    tool_name VARCHAR(100) NOT NULL,
    arguments JSONB NOT NULL DEFAULT '{}',
    output TEXT NOT NULL DEFAULT '',
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    error_message TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_agent_tool_calls_run_id ON agent_tool_calls (run_id);
CREATE INDEX IF NOT EXISTS idx_agent_tool_calls_model_call_id ON agent_tool_calls (model_call_id);
