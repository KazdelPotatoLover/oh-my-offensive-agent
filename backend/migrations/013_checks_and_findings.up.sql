CREATE TABLE IF NOT EXISTS check_results (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    attempt_id UUID REFERENCES attempts(id) ON DELETE SET NULL,
    session_id UUID REFERENCES agent_sessions(id) ON DELETE SET NULL,
    work_item_id UUID REFERENCES work_items(id) ON DELETE SET NULL,
    asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    check_id TEXT NOT NULL,
    check_name TEXT NOT NULL,
    check_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pass', 'needs_attention', 'not_applicable', 'insufficient_evidence', 'error')),
    explanation TEXT NOT NULL,
    source_tool_call_id UUID REFERENCES agent_tool_calls(id) ON DELETE SET NULL,
    source_observation_id UUID REFERENCES observations(id) ON DELETE SET NULL,
    evidence_snapshot JSONB NOT NULL DEFAULT '{}',
    details JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_check_results_run_asset ON check_results(run_id, asset_id);
CREATE INDEX IF NOT EXISTS idx_check_results_status ON check_results(status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_check_results_dedup ON check_results(run_id, asset_id, check_id, check_version, source_tool_call_id) WHERE source_tool_call_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS findings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    check_result_id UUID NOT NULL UNIQUE REFERENCES check_results(id) ON DELETE CASCADE,
    asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    category TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'low', 'medium', 'high', 'critical')),
    status TEXT NOT NULL DEFAULT 'pending_review' CHECK (status IN ('pending_review', 'confirmed', 'false_positive', 'ignored')),
    description TEXT NOT NULL,
    remediation TEXT NOT NULL DEFAULT '',
    source_tool_call_id UUID REFERENCES agent_tool_calls(id) ON DELETE SET NULL,
    evidence_ref JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_findings_run_id ON findings(run_id);
CREATE INDEX IF NOT EXISTS idx_findings_asset_id ON findings(asset_id);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings(status);
