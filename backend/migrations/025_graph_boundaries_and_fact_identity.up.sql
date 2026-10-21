ALTER TABLE exploration_nodes ADD COLUMN fact_fingerprint TEXT;
CREATE UNIQUE INDEX exploration_fact_identity ON exploration_nodes(run_id,fact_fingerprint) WHERE fact_fingerprint IS NOT NULL;
ALTER TABLE exploration_nodes ADD CONSTRAINT exploration_nodes_run_identity UNIQUE(run_id,id);
ALTER TABLE work_items ADD CONSTRAINT work_items_run_identity UNIQUE(run_id,id);
ALTER TABLE agent_sessions ADD CONSTRAINT agent_sessions_run_identity UNIQUE(run_id,id);
ALTER TABLE agent_tool_calls ADD CONSTRAINT agent_tool_calls_run_identity UNIQUE(run_id,id);
ALTER TABLE findings ADD CONSTRAINT findings_run_identity UNIQUE(run_id,id);
-- Enforce all new writes without rewriting or fabricating historical graph data.
ALTER TABLE exploration_edges ADD CONSTRAINT exploration_edge_source_scope FOREIGN KEY(run_id,src_id) REFERENCES exploration_nodes(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE exploration_edges ADD CONSTRAINT exploration_edge_target_scope FOREIGN KEY(run_id,dst_id) REFERENCES exploration_nodes(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE exploration_anchors ADD CONSTRAINT exploration_anchor_node_scope FOREIGN KEY(run_id,node_id) REFERENCES exploration_nodes(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE exploration_anchors ADD CONSTRAINT exploration_anchor_asset_scope FOREIGN KEY(run_id,asset_id) REFERENCES run_assets(run_id,asset_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE exploration_nodes ADD CONSTRAINT exploration_work_scope FOREIGN KEY(run_id,work_item_id) REFERENCES work_items(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE exploration_nodes ADD CONSTRAINT exploration_finding_scope FOREIGN KEY(run_id,finding_id) REFERENCES findings(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE artifacts ADD CONSTRAINT artifact_work_scope FOREIGN KEY(run_id,work_item_id) REFERENCES work_items(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE artifacts ADD CONSTRAINT artifact_session_scope FOREIGN KEY(run_id,session_id) REFERENCES agent_sessions(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE artifacts ADD CONSTRAINT artifact_call_scope FOREIGN KEY(run_id,source_tool_call_id) REFERENCES agent_tool_calls(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE context_checkpoints ADD CONSTRAINT checkpoint_session_scope FOREIGN KEY(run_id,session_id) REFERENCES agent_sessions(run_id,id) ON DELETE CASCADE NOT VALID;
ALTER TABLE work_control_messages ADD CONSTRAINT control_work_scope FOREIGN KEY(run_id,work_item_id) REFERENCES work_items(run_id,id) ON DELETE CASCADE NOT VALID;
