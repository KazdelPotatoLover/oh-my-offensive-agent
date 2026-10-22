-- Refuse to drop capture evidence. An empty database may roll the schema back.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM http_receipts) OR EXISTS (SELECT 1 FROM http_credentials) THEN
        RAISE EXCEPTION 'http capture evidence exists; refusing to drop migration 028';
    END IF;
END $$;

DROP TABLE IF EXISTS http_job_capabilities;
DROP TABLE IF EXISTS http_request_ledger;
DROP TABLE IF EXISTS http_pages;
DROP TABLE IF EXISTS http_origin_pace;
DROP TABLE IF EXISTS http_receipts;
DROP TABLE IF EXISTS http_capture_artifacts;
DROP TABLE IF EXISTS http_sessions;
DROP TABLE IF EXISTS http_credentials;
