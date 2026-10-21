DO $$
BEGIN
    RAISE EXCEPTION 'cannot rollback migration 015: audit retention policy forbids reverting to CASCADE delete foreign keys';
END $$;
