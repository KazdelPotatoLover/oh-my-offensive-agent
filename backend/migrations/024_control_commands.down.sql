DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM control_commands) OR EXISTS (SELECT 1 FROM work_control_messages) THEN
        RAISE EXCEPTION 'cannot rollback migration 024: execution control audit records exist';
    END IF;
END $$;

DROP TABLE work_control_messages;
DROP TABLE control_commands;
