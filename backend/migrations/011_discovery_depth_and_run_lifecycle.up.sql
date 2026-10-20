ALTER TABLE run_assets ADD COLUMN IF NOT EXISTS is_seed BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE run_assets ADD COLUMN IF NOT EXISTS depth INTEGER NOT NULL DEFAULT 0;

-- Recover original seeds from recorded input, even when navigation links have
-- added incoming edges to a seed. No existing observations or Run outcomes change.
UPDATE run_assets ra
SET is_seed = true, depth = 0
FROM runs r, assets a
WHERE ra.run_id = r.id AND ra.asset_id = a.id AND (
    ra.snapshot->>'is_seed' = 'true'
    OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            CASE WHEN jsonb_typeof(r.execution_input->'scope'->'seeds') = 'array'
                 THEN r.execution_input->'scope'->'seeds' ELSE '[]'::jsonb END
        ) AS seed(value)
        WHERE a.external_key = 'GET ' || seed.value
           OR a.metadata->>'path' = CASE seed.value
                WHEN 'http_home' THEN '/' WHEN 'http_login' THEN '/login'
                WHEN 'http_health' THEN '/health' ELSE seed.value END
    )
    OR NOT EXISTS (
        SELECT 1 FROM asset_discoveries ad
        WHERE ad.run_id = ra.run_id AND ad.target_asset_id = ra.asset_id
    )
);

CREATE INDEX IF NOT EXISTS idx_run_assets_run_id_is_seed ON run_assets (run_id, is_seed);
