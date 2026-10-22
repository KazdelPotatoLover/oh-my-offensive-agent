CREATE TABLE main_tool_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    turn_id UUID NOT NULL REFERENCES main_turns(id) ON DELETE CASCADE,
    model_call_id UUID NOT NULL REFERENCES main_model_calls(id),
    epoch_number INT NOT NULL,
    call_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running','success','failed')),
    output TEXT NOT NULL DEFAULT '',
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    UNIQUE(turn_id,call_id)
);
