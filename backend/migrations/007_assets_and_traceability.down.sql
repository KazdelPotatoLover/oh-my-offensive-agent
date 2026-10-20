DROP TABLE IF EXISTS work_item_sources;
DROP TABLE IF EXISTS work_item_assets;
DROP TABLE IF EXISTS run_assets;
ALTER TABLE observations DROP COLUMN IF EXISTS asset_id;
ALTER TABLE work_items DROP COLUMN IF EXISTS created_by_planner_round_id;
DROP TABLE IF EXISTS assets;
