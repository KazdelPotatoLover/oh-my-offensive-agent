CREATE TABLE control_commands (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    result JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id,kind,idempotency_key)
);
CREATE TABLE work_control_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    message TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    delivered_session_id UUID REFERENCES agent_sessions(id) ON DELETE SET NULL,
    delivered_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id,idempotency_key)
);
CREATE INDEX pending_work_controls ON work_control_messages(work_item_id,created_at) WHERE delivered_at IS NULL;
