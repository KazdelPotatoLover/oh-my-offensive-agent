-- Migration 016: ARTEX Alignment Core Schema
-- Dual Graph (exploration_nodes, exploration_edges, exploration_anchors),
-- Epochs & Grants, PlannerTodos, Asset Relations, and Finding Retests.

-- 1. Run Epoch & Grants
ALTER TABLE runs ADD COLUMN IF NOT EXISTS current_epoch INT NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS execution_epochs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed', 'stopped', 'interrupted')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    finish_reason TEXT,
    summary TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, epoch_number)
);

CREATE INDEX IF NOT EXISTS idx_execution_epochs_run ON execution_epochs(run_id);

CREATE TABLE IF NOT EXISTS budget_grants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    granted_by TEXT NOT NULL DEFAULT 'user',
    additional_model_calls INT NOT NULL DEFAULT 0,
    additional_tool_calls INT NOT NULL DEFAULT 0,
    additional_seconds INT NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_budget_grants_run ON budget_grants(run_id);

-- Attach epoch to sessions, attempts, and tool/model calls
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;
ALTER TABLE agent_tool_calls ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;
ALTER TABLE model_calls ADD COLUMN IF NOT EXISTS epoch_number INT NOT NULL DEFAULT 1;

-- 2. Planner Shared Todos (survives wakeups & restarts)
CREATE TABLE IF NOT EXISTS planner_todos (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    item_key TEXT NOT NULL,
    description TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'in_progress', 'completed', 'dropped')),
    depends_on TEXT[] NOT NULL DEFAULT '{}',
    version INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, item_key)
);

CREATE INDEX IF NOT EXISTS idx_planner_todos_run ON planner_todos(run_id);

-- 3. Exploration Graph (Goal, Fact, Hint, Intent, Finding, Digest)
CREATE TABLE IF NOT EXISTS exploration_nodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('begin', 'goal', 'intent', 'fact', 'finding', 'hint', 'digest')),
    payload JSONB NOT NULL DEFAULT '{}',
    priority INT NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'open',
    origin TEXT,
    owner TEXT,
    blocked_reason TEXT,
    delete_reason TEXT,
    work_item_id UUID UNIQUE REFERENCES work_items(id) ON DELETE SET NULL,
    finding_id UUID REFERENCES findings(id) ON DELETE SET NULL,
    content_version INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_expnodes_run_kind ON exploration_nodes(run_id, kind);
CREATE INDEX IF NOT EXISTS idx_expnodes_run_state ON exploration_nodes(run_id, state);
CREATE INDEX IF NOT EXISTS idx_expnodes_work_item ON exploration_nodes(work_item_id);

CREATE TABLE IF NOT EXISTS exploration_edges (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    src_id UUID NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    dst_id UUID NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    rel TEXT NOT NULL CHECK (rel IN ('spawns', 'derived_from', 'yields', 'proves', 'covers')),
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, src_id, dst_id, rel)
);

CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges(dst_id, rel);

CREATE TABLE IF NOT EXISTS exploration_anchors (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    node_id UUID NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(node_id, asset_id)
);

CREATE INDEX IF NOT EXISTS idx_expanchors_node ON exploration_anchors(node_id);
CREATE INDEX IF NOT EXISTS idx_expanchors_asset ON exploration_anchors(asset_id);

-- 4. Asset Relations (Typed asset graph edges: subdomain_of, endpoint_of, etc.)
CREATE TABLE IF NOT EXISTS asset_relations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    target_asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    relation_type TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(source_asset_id, target_asset_id, relation_type)
);

CREATE INDEX IF NOT EXISTS idx_asset_relations_source ON asset_relations(source_asset_id);
CREATE INDEX IF NOT EXISTS idx_asset_relations_target ON asset_relations(target_asset_id);

-- 5. Finding Semantic Extensions & Retests
ALTER TABLE findings
    ADD COLUMN IF NOT EXISTS source_kind TEXT NOT NULL DEFAULT 'check_rule'
        CHECK (source_kind IN ('check_rule', 'speculative', 'empirical')),
    ADD COLUMN IF NOT EXISTS verification_state TEXT NOT NULL DEFAULT 'unverified'
        CHECK (verification_state IN ('unverified', 'verified', 'retested')),
    ADD COLUMN IF NOT EXISTS retest_verdict TEXT,
    ADD COLUMN IF NOT EXISTS retest_notes TEXT,
    ADD COLUMN IF NOT EXISTS retest_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS finding_retests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    finding_id UUID NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    status VARCHAR(20) NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed', 'stopped')),
    verdict TEXT CHECK (verdict IN ('still_reproducible', 'fixed', 'inconclusive')),
    notes TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    session_id UUID REFERENCES agent_sessions(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_finding_retests_finding ON finding_retests(finding_id);
CREATE INDEX IF NOT EXISTS idx_finding_retests_run ON finding_retests(run_id);
