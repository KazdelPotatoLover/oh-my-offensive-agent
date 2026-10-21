-- Migration 016 Down: ARTEX Alignment Core Schema

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM finding_retests) OR EXISTS (SELECT 1 FROM budget_grants)
       OR EXISTS (SELECT 1 FROM execution_epochs) OR EXISTS (SELECT 1 FROM exploration_nodes)
       OR EXISTS (SELECT 1 FROM planner_todos) OR EXISTS (SELECT 1 FROM asset_relations)
       OR EXISTS (SELECT 1 FROM findings WHERE source_kind != 'check_rule' OR verification_state != 'unverified' OR retest_verdict IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot rollback migration 016: exploration, authorization or verification history exists';
    END IF;
END $$;

DROP TABLE IF EXISTS finding_retests;

ALTER TABLE findings
    DROP COLUMN IF EXISTS source_kind,
    DROP COLUMN IF EXISTS verification_state,
    DROP COLUMN IF EXISTS retest_verdict,
    DROP COLUMN IF EXISTS retest_notes,
    DROP COLUMN IF EXISTS retest_at;

DROP TABLE IF EXISTS asset_relations;
DROP TABLE IF EXISTS exploration_anchors;
DROP TABLE IF EXISTS exploration_edges;
DROP TABLE IF EXISTS exploration_nodes;
DROP TABLE IF EXISTS planner_todos;

ALTER TABLE model_calls DROP COLUMN IF EXISTS epoch;
ALTER TABLE agent_tool_calls DROP COLUMN IF EXISTS epoch;
ALTER TABLE attempts DROP COLUMN IF EXISTS epoch;
ALTER TABLE agent_sessions DROP COLUMN IF EXISTS epoch;

DROP TABLE IF EXISTS budget_grants;
DROP TABLE IF EXISTS execution_epochs;

ALTER TABLE runs DROP COLUMN IF EXISTS current_epoch;
