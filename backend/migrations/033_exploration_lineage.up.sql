-- One task start per run, plus idempotent historical lineage.
-- Re-run with: SELECT backfill_exploration_lineage();

CREATE OR REPLACE FUNCTION try_uuid(p text) RETURNS uuid
LANGUAGE plpgsql AS $$
BEGIN
    RETURN p::uuid;
EXCEPTION WHEN others THEN
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION record_lineage_backfill_diagnostic(p_run uuid, p_node uuid, p_reason text)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    next_seq int;
BEGIN
    IF EXISTS (
        SELECT 1 FROM run_events
        WHERE run_id = p_run AND type = 'lineage_backfill_diagnostic'
          AND payload->>'node_id' = p_node::text
          AND payload->>'reason' = p_reason
    ) THEN
        RETURN;
    END IF;
    SELECT COALESCE(MAX(seq), 0) + 1 INTO next_seq FROM run_events WHERE run_id = p_run;
    INSERT INTO run_events (id, run_id, seq, type, payload)
    VALUES (
        gen_random_uuid(), p_run, next_seq, 'lineage_backfill_diagnostic',
        jsonb_build_object('node_id', p_node, 'reason', p_reason, 'source', 'historical_backfill')
    );
END $$;

CREATE OR REPLACE FUNCTION backfill_exploration_lineage()
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    r record;
    start_id uuid;
    intent_row record;
    src_elem jsonb;
    src uuid;
    invalid boolean;
    node_id uuid;
    reason text;
BEGIN
    FOR r IN SELECT id, execution_input, current_epoch FROM runs LOOP
        start_id := NULL;
        SELECT id INTO start_id FROM exploration_nodes
        WHERE run_id = r.id AND kind = 'fact' AND state = 'origin' AND delete_reason IS NULL
        ORDER BY created_at LIMIT 1;
        IF start_id IS NULL THEN
            SELECT id INTO start_id FROM exploration_nodes
            WHERE run_id = r.id AND kind = 'begin' AND delete_reason IS NULL
            ORDER BY created_at LIMIT 1;
        END IF;
        IF start_id IS NULL THEN
            INSERT INTO exploration_nodes(run_id, kind, payload, state, origin, epoch_number)
            VALUES (
                r.id, 'fact',
                jsonb_build_object(
                    'summary', COALESCE(r.execution_input->>'objective', ''),
                    'objective', COALESCE(r.execution_input->>'objective', ''),
                    'scope', COALESCE(r.execution_input->'scope', '{}'::jsonb),
                    'source', 'historical_backfill',
                    'verified_source', false
                ),
                'origin', 'run_snapshot', COALESCE(r.current_epoch, 1)
            )
            RETURNING id INTO start_id;
        END IF;

        INSERT INTO exploration_edges(run_id, src_id, dst_id, rel, reason)
        SELECT r.id, start_id, g.id, 'spawns', 'historical_backfill'
        FROM exploration_nodes g
        WHERE g.run_id = r.id AND g.kind = 'goal' AND g.delete_reason IS NULL
          AND NOT EXISTS (
              SELECT 1 FROM exploration_edges e
              WHERE e.run_id = r.id AND e.dst_id = g.id AND e.rel = 'spawns'
          )
        ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING;

        FOR intent_row IN
            SELECT id, payload FROM exploration_nodes
            WHERE run_id = r.id AND kind = 'intent' AND delete_reason IS NULL
        LOOP
            IF EXISTS (
                SELECT 1 FROM exploration_edges e
                WHERE e.run_id = r.id AND e.dst_id = intent_row.id AND e.rel = 'derived_from'
            ) THEN
                CONTINUE;
            END IF;
            IF intent_row.payload ? 'source_fact_ids'
               AND jsonb_typeof(intent_row.payload->'source_fact_ids') IS DISTINCT FROM 'array' THEN
                PERFORM record_lineage_backfill_diagnostic(r.id, intent_row.id, 'source_fact_ids is not a valid array');
                CONTINUE;
            END IF;
            IF NOT (intent_row.payload ? 'source_fact_ids')
               OR jsonb_array_length(intent_row.payload->'source_fact_ids') = 0 THEN
                INSERT INTO exploration_edges(run_id, src_id, dst_id, rel, reason)
                VALUES (r.id, start_id, intent_row.id, 'derived_from', 'historical_backfill')
                ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING;
                CONTINUE;
            END IF;

            invalid := false;
            FOR src_elem IN SELECT jsonb_array_elements(intent_row.payload->'source_fact_ids') LOOP
                IF jsonb_typeof(src_elem) IS DISTINCT FROM 'string' THEN
                    invalid := true;
                    CONTINUE;
                END IF;
                src := try_uuid(src_elem #>> '{}');
                IF src IS NULL OR NOT EXISTS (
                    SELECT 1 FROM exploration_nodes n
                    WHERE n.id = src AND n.run_id = r.id
                      AND n.kind IN ('fact', 'finding')
                      AND n.delete_reason IS NULL
                      AND n.state NOT IN ('retracted', 'invalidated')
                ) THEN
                    invalid := true;
                    CONTINUE;
                END IF;
                INSERT INTO exploration_edges(run_id, src_id, dst_id, rel, reason)
                VALUES (r.id, src, intent_row.id, 'derived_from', 'historical_backfill')
                ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING;
            END LOOP;
            IF invalid THEN
                PERFORM record_lineage_backfill_diagnostic(r.id, intent_row.id, 'source_fact_ids contains an unusable source');
            END IF;
        END LOOP;

        INSERT INTO exploration_edges(run_id, src_id, dst_id, rel, reason)
        SELECT r.id, intent.id, fact.id, 'yields', 'historical_backfill'
        FROM exploration_nodes fact
        JOIN agent_tool_calls tc
          ON tc.run_id = r.id AND tc.id = try_uuid(fact.payload->>'source_tool_call_id')
        JOIN agent_sessions s
          ON s.id = tc.session_id AND s.run_id = r.id AND s.work_item_id IS NOT NULL
        JOIN exploration_nodes intent
          ON intent.run_id = r.id AND intent.kind = 'intent'
         AND intent.work_item_id = s.work_item_id AND intent.delete_reason IS NULL
        WHERE fact.run_id = r.id AND fact.kind = 'fact' AND fact.state <> 'origin'
          AND fact.delete_reason IS NULL
          AND jsonb_typeof(fact.payload->'source_tool_call_id') = 'string'
          AND NOT EXISTS (
              SELECT 1 FROM exploration_edges e
              WHERE e.run_id = r.id AND e.dst_id = fact.id AND e.rel = 'yields'
          )
          AND (
              SELECT count(*) FROM exploration_nodes i
              WHERE i.run_id = r.id AND i.kind = 'intent'
                AND i.work_item_id = s.work_item_id AND i.delete_reason IS NULL
          ) = 1
        ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING;

        INSERT INTO exploration_edges(run_id, src_id, dst_id, rel, reason)
        SELECT r.id, intent.id, fn.id, 'yields', 'historical_backfill'
        FROM exploration_nodes fn
        JOIN findings f ON f.id = fn.finding_id AND f.run_id = r.id AND f.source_tool_call_id IS NOT NULL
        JOIN agent_tool_calls tc ON tc.id = f.source_tool_call_id AND tc.run_id = r.id
        JOIN agent_sessions s ON s.id = tc.session_id AND s.run_id = r.id AND s.work_item_id IS NOT NULL
        JOIN exploration_nodes intent
          ON intent.run_id = r.id AND intent.kind = 'intent'
         AND intent.work_item_id = s.work_item_id AND intent.delete_reason IS NULL
        WHERE fn.run_id = r.id AND fn.kind = 'finding' AND fn.delete_reason IS NULL
          AND NOT EXISTS (
              SELECT 1 FROM exploration_edges e
              WHERE e.run_id = r.id AND e.dst_id = fn.id AND e.rel = 'yields'
          )
          AND (
              SELECT count(*) FROM exploration_nodes i
              WHERE i.run_id = r.id AND i.kind = 'intent'
                AND i.work_item_id = s.work_item_id AND i.delete_reason IS NULL
          ) = 1
        ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING;

        FOR node_id, reason IN
            SELECT fact.id, 'source_tool_call_id did not resolve to one intent'
            FROM exploration_nodes fact
            WHERE fact.run_id = r.id AND fact.kind = 'fact' AND fact.state <> 'origin'
              AND fact.delete_reason IS NULL
              AND fact.payload ? 'source_tool_call_id'
              AND NOT EXISTS (
                  SELECT 1 FROM exploration_edges e
                  WHERE e.run_id = r.id AND e.dst_id = fact.id AND e.rel = 'yields'
              )
        LOOP
            PERFORM record_lineage_backfill_diagnostic(r.id, node_id, reason);
        END LOOP;
    END LOOP;
END $$;

SELECT backfill_exploration_lineage();

CREATE UNIQUE INDEX IF NOT EXISTS exploration_nodes_one_origin
    ON exploration_nodes (run_id)
    WHERE kind = 'fact' AND state = 'origin' AND delete_reason IS NULL;
