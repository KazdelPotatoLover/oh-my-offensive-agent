CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- 1. Assets table (Stable object identity across Runs)
CREATE TABLE IF NOT EXISTS assets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace VARCHAR(100) NOT NULL,
    kind VARCHAR(50) NOT NULL,
    external_key VARCHAR(100) NOT NULL,
    name VARCHAR(255) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_assets_identity UNIQUE (namespace, kind, external_key)
);

CREATE INDEX IF NOT EXISTS idx_assets_identity ON assets (namespace, kind, external_key);
CREATE INDEX IF NOT EXISTS idx_assets_kind ON assets (kind);

-- 2. Run-Asset linkage (Scope of assets for a Run and non-sensitive snapshot)
CREATE TABLE IF NOT EXISTS run_assets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    snapshot JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_run_assets UNIQUE (run_id, asset_id)
);

CREATE INDEX IF NOT EXISTS idx_run_assets_run_id ON run_assets (run_id);
CREATE INDEX IF NOT EXISTS idx_run_assets_asset_id ON run_assets (asset_id);

-- 3. WorkItem-Asset linkage (Target assets for work items)
CREATE TABLE IF NOT EXISTS work_item_assets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_work_item_assets UNIQUE (work_item_id, asset_id)
);

CREATE INDEX IF NOT EXISTS idx_work_item_assets_work_item_id ON work_item_assets (work_item_id);
CREATE INDEX IF NOT EXISTS idx_work_item_assets_asset_id ON work_item_assets (asset_id);

-- 4. WorkItem-Source Observation linkage (Causal source observations)
CREATE TABLE IF NOT EXISTS work_item_sources (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    source_observation_id UUID NOT NULL REFERENCES observations(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_work_item_sources UNIQUE (work_item_id, source_observation_id)
);

CREATE INDEX IF NOT EXISTS idx_work_item_sources_work_item_id ON work_item_sources (work_item_id);
CREATE INDEX IF NOT EXISTS idx_work_item_sources_source_obs_id ON work_item_sources (source_observation_id);

-- 5. Add asset_id to observations (nullable, historical data retained as NULL)
ALTER TABLE observations ADD COLUMN IF NOT EXISTS asset_id UUID REFERENCES assets(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_observations_asset_id ON observations (asset_id);

-- 6. Add created_by_planner_round_id to work_items (nullable, set by server)
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS created_by_planner_round_id UUID REFERENCES planner_rounds(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_work_items_created_by_planner_round_id ON work_items (created_by_planner_round_id);
