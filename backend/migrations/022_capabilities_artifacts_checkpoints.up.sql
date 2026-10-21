ALTER TABLE work_items ADD COLUMN intent_fingerprint TEXT;
CREATE UNIQUE INDEX work_items_intent_identity ON work_items(run_id,intent_fingerprint) WHERE intent_fingerprint IS NOT NULL;

CREATE TABLE artifacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    work_item_id UUID REFERENCES work_items(id) ON DELETE CASCADE,
    session_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE,
    source_tool_call_id UUID REFERENCES agent_tool_calls(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    name TEXT NOT NULL,
    media_type TEXT NOT NULL DEFAULT 'text/plain',
    content TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    byte_length INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(source_tool_call_id)
);
CREATE INDEX artifacts_by_run ON artifacts(run_id,created_at,id);

CREATE TABLE context_checkpoints (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    work_item_id UUID REFERENCES work_items(id) ON DELETE CASCADE,
    session_id UUID NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    version INT NOT NULL DEFAULT 1,
    covered_messages INT NOT NULL,
    source_sha256 TEXT NOT NULL,
    strategy TEXT NOT NULL,
    summary TEXT NOT NULL,
    view_messages JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(session_id,covered_messages,source_sha256)
);

CREATE TABLE exploration_revisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    node_id UUID NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    version INT NOT NULL,
    payload JSONB NOT NULL,
    state TEXT NOT NULL,
    reason TEXT NOT NULL,
    actor TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(node_id,version)
);
ALTER TABLE exploration_edges ADD COLUMN valid BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE exploration_edges ADD COLUMN source_version INT;
ALTER TABLE exploration_edges ADD COLUMN target_version INT;
ALTER TABLE exploration_edges ADD COLUMN invalid_reason TEXT;
ALTER TABLE exploration_edges ADD COLUMN invalidated_at TIMESTAMPTZ;
