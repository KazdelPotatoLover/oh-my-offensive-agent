DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM execution_commands)
       OR EXISTS (SELECT 1 FROM main_messages)
       OR EXISTS (SELECT 1 FROM main_turns)
       OR EXISTS (SELECT 1 FROM main_model_calls)
       OR EXISTS (SELECT 1 FROM main_conversations)
    THEN
        RAISE EXCEPTION 'cannot rollback migration 030: control and main conversation audit rows must be retained';
    END IF;
END $$;

DROP TABLE IF EXISTS main_model_calls;
DROP TABLE IF EXISTS main_messages;
DROP TABLE IF EXISTS main_turns;
DROP TABLE IF EXISTS main_conversations;
DROP TABLE IF EXISTS execution_commands;
