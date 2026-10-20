CREATE TABLE IF NOT EXISTS asset_discoveries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    source_asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source_tool_call_id UUID NOT NULL REFERENCES agent_tool_calls(id) ON DELETE CASCADE,
    target_asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    raw_href TEXT NOT NULL,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_asset_discoveries_run_source_target_href UNIQUE (run_id, source_asset_id, target_asset_id, raw_href)
);

CREATE INDEX IF NOT EXISTS idx_asset_discoveries_run_id ON asset_discoveries (run_id);
CREATE INDEX IF NOT EXISTS idx_asset_discoveries_source_asset_id ON asset_discoveries (source_asset_id);
CREATE INDEX IF NOT EXISTS idx_asset_discoveries_target_asset_id ON asset_discoveries (target_asset_id);
CREATE INDEX IF NOT EXISTS idx_asset_discoveries_tool_call_id ON asset_discoveries (source_tool_call_id);
