DROP TRIGGER scoped_asset_changes ON run_assets;
DROP TRIGGER finding_changes ON findings;
DROP TRIGGER work_direction_changes ON work_items;
DROP TRIGGER exploration_edge_changes ON exploration_edges;
DROP TRIGGER exploration_node_changes ON exploration_nodes;
DROP FUNCTION publish_exploration_change();
DROP TABLE planner_cursors;
DROP TABLE exploration_changes;
