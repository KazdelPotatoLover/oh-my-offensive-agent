-- Migration 029: isolated execution jobs.
-- Child outputs hang off execution_job_artifacts. The artifacts.source_tool_call_id
-- unique constraint is intentionally left unchanged.

CREATE TABLE execution_jobs (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE RESTRICT,
    epoch_number INT NOT NULL CHECK (epoch_number >= 1),
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE RESTRICT,
    attempt_id UUID NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    session_id UUID REFERENCES agent_sessions(id) ON DELETE RESTRICT,
    source_tool_call_id UUID REFERENCES agent_tool_calls(id) ON DELETE RESTRICT,
    image TEXT NOT NULL,
    argv JSONB NOT NULL,
    timeout_seconds INT NOT NULL CHECK (timeout_seconds > 0 AND timeout_seconds <= 300),
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'finished', 'killed', 'interrupted', 'outcome_unknown')),
    exit_code INT,
    stdout_artifact_id UUID REFERENCES artifacts(id) ON DELETE RESTRICT,
    stderr_artifact_id UUID REFERENCES artifacts(id) ON DELETE RESTRICT,
    script_artifact_id UUID REFERENCES artifacts(id) ON DELETE RESTRICT,
    execution_receipt_id UUID,
    container_id TEXT NOT NULL DEFAULT '',
    truncated BOOLEAN NOT NULL DEFAULT false,
    error_code TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT execution_jobs_exit_only_finished CHECK (
        (state = 'finished' AND exit_code IS NOT NULL)
        OR (state <> 'finished' AND exit_code IS NULL)
    )
);

CREATE INDEX execution_jobs_by_run ON execution_jobs(run_id, created_at);
CREATE INDEX execution_jobs_by_attempt ON execution_jobs(attempt_id, created_at);
CREATE INDEX execution_jobs_by_work_item ON execution_jobs(work_item_id, created_at);
CREATE INDEX execution_jobs_nonterminal ON execution_jobs(state) WHERE state IN ('queued', 'running');
CREATE UNIQUE INDEX execution_jobs_one_active_attempt
    ON execution_jobs(attempt_id) WHERE state IN ('queued', 'running');

CREATE TABLE execution_job_artifacts (
    job_id UUID NOT NULL REFERENCES execution_jobs(id) ON DELETE RESTRICT,
    artifact_id UUID NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
    role TEXT NOT NULL CHECK (role IN ('stdout', 'stderr', 'script', 'file', 'output')),
    path TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, role, artifact_id)
);

CREATE INDEX execution_job_artifacts_by_artifact ON execution_job_artifacts(artifact_id);

CREATE TABLE execution_job_traffic (
    job_id UUID NOT NULL REFERENCES execution_jobs(id) ON DELETE RESTRICT,
    receipt_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, receipt_id)
);

CREATE TABLE execution_workspaces (
    work_item_id UUID PRIMARY KEY REFERENCES work_items(id) ON DELETE RESTRICT,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE RESTRICT,
    restored_attempt_id UUID REFERENCES attempts(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- File tools keep a path the artifacts table cannot store (names reject slashes).
-- These rows are workspace history, not a second source_tool_call_id owner.
CREATE TABLE execution_files (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE RESTRICT,
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE RESTRICT,
    attempt_id UUID NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    artifact_id UUID NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
    path TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX execution_files_by_work_item ON execution_files(work_item_id, path, created_at DESC);
