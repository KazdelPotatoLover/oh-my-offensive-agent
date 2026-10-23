DROP INDEX IF EXISTS exploration_nodes_one_origin;
DROP FUNCTION IF EXISTS backfill_exploration_lineage();
DROP FUNCTION IF EXISTS record_lineage_backfill_diagnostic(uuid, uuid, text);
DROP FUNCTION IF EXISTS try_uuid(text);
