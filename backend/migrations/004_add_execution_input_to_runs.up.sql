ALTER TABLE runs ADD COLUMN execution_input JSONB NOT NULL DEFAULT '{}';

-- For historical runs, explicitly record captured information and clearly mark missing
-- fields rather than backfilling with current Task contents.
UPDATE runs
SET execution_input = jsonb_build_object(
    'objective', '[historical_unavailable]',
    'scope', jsonb_build_object('_unavailable', true, 'reason', 'not captured prior to migration 004'),
    'config', COALESCE(config_snapshot, '{}'::jsonb),
    'parameters', jsonb_build_object('fail_at_step', fail_at_step),
    '_historical_missing', true,
    '_note', 'Historical execution input was not captured at run creation time prior to migration 004; current Task content was deliberately not backfilled to maintain audit integrity'
);
