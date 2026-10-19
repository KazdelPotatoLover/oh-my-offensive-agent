CREATE TABLE runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    idempotency_key VARCHAR(255),
    config_snapshot JSONB NOT NULL DEFAULT '{}',
    status VARCHAR(20) NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued','running','stopping','completed','failed','stopped','interrupted')),
    finish_reason TEXT,
    fail_at_step INT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_runs_one_active_per_task
    ON runs (task_id)
    WHERE status IN ('queued', 'running', 'stopping');

CREATE UNIQUE INDEX idx_runs_idempotency
    ON runs (task_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_runs_task_id ON runs (task_id);
CREATE INDEX idx_runs_status ON runs (status);
