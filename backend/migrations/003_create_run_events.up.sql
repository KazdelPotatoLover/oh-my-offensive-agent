CREATE TABLE run_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq INT NOT NULL,
    type VARCHAR(50) NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, seq)
);

CREATE INDEX idx_run_events_run_id ON run_events (run_id);
