CREATE TABLE exploration_changes (
    seq BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    entity TEXT NOT NULL,
    entity_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX exploration_changes_run_cursor ON exploration_changes(run_id,epoch_number,seq);
CREATE TABLE planner_cursors (
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    change_seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id,epoch_number)
);
CREATE FUNCTION publish_exploration_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
    IF TG_TABLE_NAME='work_items' AND TG_OP='UPDATE' AND to_jsonb(NEW)->>'status'='running' THEN RETURN NEW; END IF;
    INSERT INTO exploration_changes(run_id,epoch_number,entity,entity_id)
    SELECT NEW.run_id,current_epoch,TG_TABLE_NAME,NEW.id FROM runs WHERE id=NEW.run_id;
    PERFORM pg_notify('exploration_changes',NEW.run_id::text);
    RETURN NEW;
END $$;
CREATE TRIGGER exploration_node_changes AFTER INSERT OR UPDATE OF payload,state,content_version,delete_reason ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION publish_exploration_change();
CREATE TRIGGER exploration_edge_changes AFTER INSERT OR UPDATE OF valid ON exploration_edges FOR EACH ROW EXECUTE FUNCTION publish_exploration_change();
CREATE TRIGGER work_direction_changes AFTER INSERT OR UPDATE OF status ON work_items FOR EACH ROW EXECUTE FUNCTION publish_exploration_change();
CREATE TRIGGER finding_changes AFTER INSERT OR UPDATE OF status,verification_state,retest_verdict ON findings FOR EACH ROW EXECUTE FUNCTION publish_exploration_change();
CREATE TRIGGER scoped_asset_changes AFTER INSERT ON run_assets FOR EACH ROW EXECUTE FUNCTION publish_exploration_change();
