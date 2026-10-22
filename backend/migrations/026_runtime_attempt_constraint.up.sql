-- One running attempt per work item. A late epoch return cannot open a second
-- active attempt, and two dispatchers cannot both win the same direction.
CREATE UNIQUE INDEX IF NOT EXISTS attempts_one_running_per_work_item
    ON attempts (work_item_id)
    WHERE status = 'running';

-- SubmitPlan inserts share transaction now(), and work item ids are random.
-- plan_seq is the insertion order used by claim.
CREATE SEQUENCE IF NOT EXISTS work_items_plan_seq;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS plan_seq bigint;
ALTER TABLE work_items ALTER COLUMN plan_seq SET DEFAULT nextval('work_items_plan_seq');
WITH ordered AS (
    SELECT id, row_number() OVER (ORDER BY created_at ASC, id ASC) AS n
    FROM work_items
    WHERE plan_seq IS NULL
)
UPDATE work_items AS wi
SET plan_seq = ordered.n
FROM ordered
WHERE wi.id = ordered.id;
SELECT setval(
    'work_items_plan_seq',
    GREATEST((SELECT COALESCE(MAX(plan_seq), 1) FROM work_items), 1)
);
ALTER TABLE work_items ALTER COLUMN plan_seq SET NOT NULL;
CREATE INDEX IF NOT EXISTS work_items_claim_order ON work_items (run_id, plan_seq);
