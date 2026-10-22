-- 028: trusted HTTP capture, credential references, and origin pacing.
-- Receipts are minted only by the capture gateway. This does not rewrite 001-027.

CREATE TABLE http_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alias TEXT NOT NULL,
    version INT NOT NULL,
    target_origin TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('token', 'cookie', 'login')),
    header_name TEXT NOT NULL DEFAULT '',
    secret TEXT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    login_path TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (alias, version)
);

CREATE TABLE http_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ref TEXT NOT NULL,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    owner_work_item_id UUID,
    owner_attempt_id UUID,
    shared BOOLEAN NOT NULL DEFAULT false,
    target_origin TEXT NOT NULL,
    credential_alias TEXT NOT NULL DEFAULT '',
    credential_version INT NOT NULL DEFAULT 0,
    cookie_header TEXT NOT NULL DEFAULT '',
    authorization_header TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, ref)
);

CREATE TABLE http_capture_artifacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('request', 'response')),
    media_type TEXT NOT NULL,
    headers JSONB NOT NULL DEFAULT '{}',
    content BYTEA NOT NULL,
    sha256 TEXT NOT NULL,
    byte_length INT NOT NULL,
    truncated BOOLEAN NOT NULL DEFAULT false,
    is_binary BOOLEAN NOT NULL DEFAULT false,
    sensitive BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE http_receipts (
    id UUID PRIMARY KEY,
    request_id UUID NOT NULL UNIQUE,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    role TEXT NOT NULL,
    work_item_id UUID,
    attempt_id UUID,
    session_id UUID,
    source_tool_call_id UUID,
    job_id UUID,
    asset_id UUID,
    method TEXT NOT NULL,
    url TEXT NOT NULL,
    final_url TEXT NOT NULL,
    status_code INT NOT NULL DEFAULT 0,
    capture_mode TEXT NOT NULL CHECK (capture_mode IN ('direct', 'tls_intercept', 'connect_metadata')),
    request_hash TEXT NOT NULL,
    response_hash TEXT NOT NULL DEFAULT '',
    credential_ref TEXT NOT NULL DEFAULT '',
    credential_version INT NOT NULL DEFAULT 0,
    session_ref TEXT NOT NULL DEFAULT '',
    truncated BOOLEAN NOT NULL DEFAULT false,
    outcome TEXT NOT NULL,
    preview TEXT NOT NULL DEFAULT '',
    request_artifact_id UUID REFERENCES http_capture_artifacts(id),
    response_artifact_id UUID REFERENCES http_capture_artifacts(id),
    replay_safe BOOLEAN NOT NULL DEFAULT false,
    minted_by TEXT NOT NULL DEFAULT 'gateway' CHECK (minted_by = 'gateway'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX http_receipts_scope ON http_receipts (run_id, epoch_number, created_at);
CREATE INDEX http_receipts_call ON http_receipts (source_tool_call_id) WHERE source_tool_call_id IS NOT NULL;

CREATE TABLE http_origin_pace (
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    origin TEXT NOT NULL,
    next_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, epoch_number, origin)
);

CREATE TABLE http_pages (
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    origin TEXT NOT NULL,
    page_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, epoch_number, page_key)
);

CREATE TABLE http_request_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    origin TEXT NOT NULL,
    state TEXT NOT NULL,
    receipt_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX http_request_ledger_origin ON http_request_ledger (run_id, epoch_number, origin, state);

CREATE TABLE http_job_capabilities (
    token_hash TEXT PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    epoch_number INT NOT NULL,
    job_id UUID NOT NULL,
    work_item_id UUID,
    attempt_id UUID,
    session_id UUID,
    role TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
