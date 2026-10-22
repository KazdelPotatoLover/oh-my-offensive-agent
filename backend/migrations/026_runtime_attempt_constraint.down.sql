DROP INDEX IF EXISTS work_items_claim_order;
ALTER TABLE work_items DROP COLUMN IF EXISTS plan_seq;
DROP SEQUENCE IF EXISTS work_items_plan_seq;
DROP INDEX IF EXISTS attempts_one_running_per_work_item;
