-- Migration 030: shared execution commands and the Main Agent transcript.
-- Control effects still go through the existing run/work-item services.
-- These tables record who asked, the idempotent result, and the main transcript.
-- They are not a second work-item state machine.

CREATE TABLE execution_commands (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    command_type TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('accepted', 'applied', 'rejected')),
    principal_kind TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    explicit_user_command BOOLEAN NOT NULL DEFAULT FALSE,
    epoch_number INT NOT NULL,
    work_item_id UUID,
    attempt_id UUID,
    source_tool_call_id UUID,
    delivered BOOLEAN NOT NULL DEFAULT FALSE,
    result JSONB NOT NULL DEFAULT '{}',
    error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, idempotency_key)
);

CREATE INDEX execution_commands_run_created ON execution_commands (run_id, created_at);

-- Stable conversation anchor. The public transcript is main_messages, not agent_messages.
-- session status stays completed so an epoch transition, which stops running sessions,
-- does not destroy the conversation row.
CREATE TABLE main_conversations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    session_id UUID NOT NULL REFERENCES agent_sessions(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id)
);

CREATE TABLE main_turns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES main_conversations(id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    expected_epoch INT NOT NULL,
    actor_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    model_calls INT NOT NULL DEFAULT 0,
    runtime_snapshot JSONB NOT NULL DEFAULT '{}',
    error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (run_id, idempotency_key)
);

CREATE INDEX main_turns_run_status ON main_turns (run_id, status, created_at);

CREATE TABLE main_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES main_conversations(id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq INT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'action')),
    text TEXT NOT NULL DEFAULT '',
    turn_id UUID REFERENCES main_turns(id) ON DELETE SET NULL,
    command_id UUID REFERENCES execution_commands(id) ON DELETE SET NULL,
    model_call_id UUID,
    tool_call_id UUID,
    work_item_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);

CREATE INDEX main_messages_run_seq ON main_messages (run_id, seq);

-- Separate from model_calls so a main turn cannot silently consume RunExecutionUsage.
CREATE TABLE main_model_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    turn_id UUID NOT NULL REFERENCES main_turns(id) ON DELETE CASCADE,
    session_id UUID NOT NULL REFERENCES agent_sessions(id) ON DELETE RESTRICT,
    epoch_number INT NOT NULL,
    status TEXT NOT NULL,
    finish_reason TEXT,
    prompt_tokens INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    total_tokens INT NOT NULL DEFAULT 0,
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX main_model_calls_run ON main_model_calls (run_id, created_at);
