-- 1. AgentSession table
CREATE TABLE IF NOT EXISTS agent_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role VARCHAR(50) NOT NULL, -- 'planner', 'worker', 'legacy_agent'
    round INT,
    work_item_id UUID,
    attempt_id UUID,
    status VARCHAR(20) NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed', 'stopped', 'interrupted')),
    input_snapshot JSONB NOT NULL DEFAULT '{}',
    finish_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_agent_sessions_run_id ON agent_sessions(run_id);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_role ON agent_sessions(role);

-- 2. PlannerRound table
CREATE TABLE IF NOT EXISTS planner_rounds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round INT NOT NULL,
    trigger_reason VARCHAR(100) NOT NULL DEFAULT 'initial',
    session_id UUID REFERENCES agent_sessions(id) ON DELETE SET NULL,
    plan_summary TEXT,
    status VARCHAR(20) NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed', 'stopped')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    UNIQUE(run_id, round)
);

CREATE INDEX IF NOT EXISTS idx_planner_rounds_run_id ON planner_rounds(run_id);

-- 3. WorkItem table
CREATE TABLE IF NOT EXISTS work_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    key VARCHAR(100) NOT NULL,
    title VARCHAR(255) NOT NULL,
    objective TEXT NOT NULL DEFAULT '',
    work_type VARCHAR(50) NOT NULL DEFAULT 'analyze_sample',
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled', 'blocked')),
    blocked_reason TEXT,
    input_data JSONB NOT NULL DEFAULT '{}',
    output_data JSONB NOT NULL DEFAULT '{}',
    current_attempt_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, key)
);

CREATE INDEX IF NOT EXISTS idx_work_items_run_id ON work_items(run_id);
CREATE INDEX IF NOT EXISTS idx_work_items_status ON work_items(status);

-- 4. WorkDependency table
CREATE TABLE IF NOT EXISTS work_dependencies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    depends_on_work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(work_item_id, depends_on_work_item_id),
    CONSTRAINT check_no_self_dependency CHECK (work_item_id != depends_on_work_item_id)
);

CREATE INDEX IF NOT EXISTS idx_work_dependencies_work_item ON work_dependencies(work_item_id);
CREATE INDEX IF NOT EXISTS idx_work_dependencies_depends_on ON work_dependencies(depends_on_work_item_id);

-- 5. Attempt table
CREATE TABLE IF NOT EXISTS attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    attempt_number INT NOT NULL DEFAULT 1,
    session_id UUID REFERENCES agent_sessions(id) ON DELETE SET NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed', 'stopped', 'interrupted')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    finish_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(work_item_id, attempt_number)
);

CREATE INDEX IF NOT EXISTS idx_attempts_work_item ON attempts(work_item_id);

-- 6. Add references to agent_sessions and run_events
ALTER TABLE agent_messages ADD COLUMN IF NOT EXISTS session_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE;
ALTER TABLE model_calls ADD COLUMN IF NOT EXISTS session_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE;
ALTER TABLE agent_tool_calls ADD COLUMN IF NOT EXISTS session_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE;

ALTER TABLE run_events ADD COLUMN IF NOT EXISTS session_id UUID;
ALTER TABLE run_events ADD COLUMN IF NOT EXISTS work_item_id UUID;
ALTER TABLE run_events ADD COLUMN IF NOT EXISTS attempt_id UUID;

CREATE INDEX IF NOT EXISTS idx_agent_messages_session_id ON agent_messages(session_id);
CREATE INDEX IF NOT EXISTS idx_model_calls_session_id ON model_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_agent_tool_calls_session_id ON agent_tool_calls(session_id);

-- 7. Observation table
CREATE TABLE IF NOT EXISTS observations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    attempt_id UUID NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,
    obs_type VARCHAR(50) NOT NULL DEFAULT 'http_observation',
    content TEXT NOT NULL DEFAULT '',
    data JSONB NOT NULL DEFAULT '{}',
    source_tool_call_id UUID NOT NULL REFERENCES agent_tool_calls(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_observations_run_id ON observations(run_id);
CREATE INDEX IF NOT EXISTS idx_observations_attempt_id ON observations(attempt_id);

-- 8. Backfill legacy sessions for existing messages / model_calls / tool_calls
DO $$
DECLARE
    r RECORD;
    sess_id UUID;
BEGIN
    FOR r IN SELECT DISTINCT run_id FROM agent_messages WHERE session_id IS NULL
             UNION
             SELECT DISTINCT run_id FROM model_calls WHERE session_id IS NULL
             UNION
             SELECT DISTINCT run_id FROM agent_tool_calls WHERE session_id IS NULL
    LOOP
        sess_id := gen_random_uuid();
        INSERT INTO agent_sessions (id, run_id, role, status, finish_reason)
        VALUES (sess_id, r.run_id, 'legacy_agent', 'completed', 'legacy_backfill');

        UPDATE agent_messages SET session_id = sess_id WHERE run_id = r.run_id AND session_id IS NULL;
        UPDATE model_calls SET session_id = sess_id WHERE run_id = r.run_id AND session_id IS NULL;
        UPDATE agent_tool_calls SET session_id = sess_id WHERE run_id = r.run_id AND session_id IS NULL;
    END LOOP;
END $$;

-- 9. Scope unique constraints to session_id
ALTER TABLE agent_messages DROP CONSTRAINT IF EXISTS agent_messages_run_id_seq_key;
ALTER TABLE agent_messages ADD CONSTRAINT agent_messages_session_id_seq_key UNIQUE(session_id, seq);

ALTER TABLE model_calls DROP CONSTRAINT IF EXISTS model_calls_run_id_round_key;
ALTER TABLE model_calls ADD CONSTRAINT model_calls_session_id_round_key UNIQUE(session_id, round);

ALTER TABLE agent_tool_calls DROP CONSTRAINT IF EXISTS agent_tool_calls_run_id_call_id_key;
ALTER TABLE agent_tool_calls ADD CONSTRAINT agent_tool_calls_session_id_call_id_key UNIQUE(session_id, call_id);

