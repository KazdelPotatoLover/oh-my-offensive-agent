DROP INDEX IF EXISTS idx_run_assets_run_id_is_seed;
ALTER TABLE run_assets DROP COLUMN IF EXISTS depth;
ALTER TABLE run_assets DROP COLUMN IF EXISTS is_seed;
