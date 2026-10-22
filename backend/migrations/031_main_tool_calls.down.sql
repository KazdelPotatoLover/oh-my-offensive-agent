DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM main_tool_calls) THEN
        RAISE EXCEPTION 'main tool call history must be retained';
    END IF;
END $$;
DROP TABLE main_tool_calls;
