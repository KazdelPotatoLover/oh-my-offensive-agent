DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM execution_jobs)
       OR EXISTS (SELECT 1 FROM execution_job_artifacts)
       OR EXISTS (SELECT 1 FROM execution_job_traffic)
       OR EXISTS (SELECT 1 FROM execution_files) THEN
        RAISE EXCEPTION 'cannot rollback migration 029: execution jobs and their artifacts are retained evidence';
    END IF;
END $$;

DROP TABLE IF EXISTS execution_files;
DROP TABLE IF EXISTS execution_workspaces;
DROP TABLE IF EXISTS execution_job_traffic;
DROP TABLE IF EXISTS execution_job_artifacts;
DROP TABLE IF EXISTS execution_jobs;
