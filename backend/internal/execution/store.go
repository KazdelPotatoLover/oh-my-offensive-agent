package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type admission struct {
	Epoch  int
	Status string
}

type jobRecord struct {
	ID          uuid.UUID
	RunID       uuid.UUID
	Epoch       int
	WorkItemID  uuid.UUID
	AttemptID   uuid.UUID
	State       string
	ExitCode    *int
	StdoutID    *uuid.UUID
	StderrID    *uuid.UUID
	ReceiptID   *uuid.UUID
	ContainerID string
	Truncated   bool
	ErrorCode   string
	Traffic     []uuid.UUID
}

func (s *Store) Admit(ctx context.Context, scope contract.ExecutionScope) (admission, error) {
	var out admission
	var attemptEpoch int
	err := s.pool.QueryRow(ctx, `
		SELECT r.current_epoch, r.status, a.epoch_number
		FROM attempts a
		JOIN work_items w ON w.id = a.work_item_id
		JOIN runs r ON r.id = w.run_id
		WHERE r.id = $1 AND w.id = $2 AND a.id = $3`,
		scope.RunID, scope.WorkItemID, scope.AttemptID).Scan(&out.Epoch, &out.Status, &attemptEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, scopeDenied("execution scope does not match this attempt")
	}
	if err != nil {
		return out, persistence("execution scope could not be read")
	}
	if out.Status != "queued" && out.Status != "running" {
		return out, invalidState("execution run is not accepting commands")
	}
	if attemptEpoch != out.Epoch || scope.EpochNumber != out.Epoch {
		return out, staleEpoch("execution epoch does not match the run")
	}
	if scope.SessionID != nil {
		var ok bool
		err = s.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM agent_sessions
				WHERE id=$1 AND run_id=$2 AND epoch_number=$3
				  AND (attempt_id IS NULL OR attempt_id=$4)
			)`, scope.SessionID, scope.RunID, out.Epoch, scope.AttemptID).Scan(&ok)
		if err != nil || !ok {
			return out, scopeDenied("execution session is not in this attempt")
		}
	}
	return out, nil
}

func (s *Store) InsertJob(ctx context.Context, id uuid.UUID, scope contract.ExecutionScope, image string, argv []string, timeout int, source *uuid.UUID) error {
	raw, err := json.Marshal(argv)
	if err != nil {
		return invalidInput("command argv is not valid")
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO execution_jobs(
			id, run_id, epoch_number, work_item_id, attempt_id, session_id, source_tool_call_id,
			image, argv, timeout_seconds, state
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'queued')`,
		id, scope.RunID, scope.EpochNumber, scope.WorkItemID, scope.AttemptID, scope.SessionID, source,
		image, raw, timeout)
	if err != nil {
		if strings.Contains(err.Error(), "execution_jobs_one_active_attempt") {
			return invalidState("this attempt already has an active command")
		}
		return persistence("execution job was not recorded")
	}
	return nil
}

func (s *Store) MarkRunning(ctx context.Context, id uuid.UUID, containerID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE execution_jobs
		SET state='running', container_id=$2, started_at=COALESCE(started_at, now()), updated_at=now()
		WHERE id=$1 AND state IN ('queued','running')`, id, containerID)
	return err
}

func (s *Store) Finish(ctx context.Context, id uuid.UUID, state string, exitCode *int, truncated bool, errorCode, containerID string, stdout, stderr, script, receipt *uuid.UUID) error {
	if state == string(contract.JobFinished) && exitCode == nil {
		return invalidState("finished execution job requires an exit code")
	}
	if state != string(contract.JobFinished) {
		exitCode = nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE execution_jobs SET
			state=$2,
			exit_code=$3,
			truncated=$4,
			error_code=$5,
			container_id=CASE WHEN $6 <> '' THEN $6 ELSE container_id END,
			stdout_artifact_id=COALESCE($7, stdout_artifact_id),
			stderr_artifact_id=COALESCE($8, stderr_artifact_id),
			script_artifact_id=COALESCE($9, script_artifact_id),
			execution_receipt_id=COALESCE($10, execution_receipt_id),
			finished_at=now(),
			updated_at=now()
		WHERE id=$1 AND state IN ('queued','running')`,
		id, state, exitCode, truncated, errorCode, containerID, stdout, stderr, script, receipt)
	if err != nil {
		return persistence("execution job result was not recorded")
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}

// AttachArtifacts records child artifact ids without changing a terminal state.
// Cancel can finish the row before the original Run persists its output.
func (s *Store) AttachArtifacts(ctx context.Context, id uuid.UUID, stdout, stderr, script, receipt *uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE execution_jobs SET
			stdout_artifact_id=COALESCE(stdout_artifact_id, $2),
			stderr_artifact_id=COALESCE(stderr_artifact_id, $3),
			script_artifact_id=COALESCE(script_artifact_id, $4),
			execution_receipt_id=COALESCE(execution_receipt_id, $5),
			updated_at=now()
		WHERE id=$1`, id, stdout, stderr, script, receipt)
	if err != nil {
		return persistence("execution artifact link was not recorded")
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (*jobRecord, error) {
	var rec jobRecord
	err := s.pool.QueryRow(ctx, `
		SELECT id, run_id, epoch_number, work_item_id, attempt_id, state, exit_code,
		       stdout_artifact_id, stderr_artifact_id, execution_receipt_id,
		       container_id, truncated, error_code
		FROM execution_jobs WHERE id=$1`, id).Scan(
		&rec.ID, &rec.RunID, &rec.Epoch, &rec.WorkItemID, &rec.AttemptID, &rec.State, &rec.ExitCode,
		&rec.StdoutID, &rec.StderrID, &rec.ReceiptID, &rec.ContainerID, &rec.Truncated, &rec.ErrorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("execution job was not found")
	}
	if err != nil {
		return nil, persistence("execution job could not be read")
	}
	rows, err := s.pool.Query(ctx, `SELECT receipt_id FROM execution_job_traffic WHERE job_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, persistence("execution traffic could not be read")
	}
	defer rows.Close()
	for rows.Next() {
		var rid uuid.UUID
		if err := rows.Scan(&rid); err != nil {
			return nil, persistence("execution traffic could not be read")
		}
		rec.Traffic = append(rec.Traffic, rid)
	}
	return &rec, rows.Err()
}

func (s *Store) ListNonTerminal(ctx context.Context) ([]jobRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, epoch_number, work_item_id, attempt_id, state, container_id
		FROM execution_jobs WHERE state IN ('queued','running')`)
	if err != nil {
		return nil, persistence("execution jobs could not be listed")
	}
	defer rows.Close()
	var out []jobRecord
	for rows.Next() {
		var rec jobRecord
		if err := rows.Scan(&rec.ID, &rec.RunID, &rec.Epoch, &rec.WorkItemID, &rec.AttemptID, &rec.State, &rec.ContainerID); err != nil {
			return nil, persistence("execution jobs could not be listed")
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) ListContainerRefs(ctx context.Context) ([]jobRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, state, container_id FROM execution_jobs WHERE container_id <> ''`)
	if err != nil {
		return nil, persistence("execution jobs could not be listed")
	}
	defer rows.Close()
	var out []jobRecord
	for rows.Next() {
		var rec jobRecord
		if err := rows.Scan(&rec.ID, &rec.State, &rec.ContainerID); err != nil {
			return nil, persistence("execution jobs could not be listed")
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) ListAttemptJobs(ctx context.Context, attemptID uuid.UUID) ([]jobRecord, error) {
	return s.ListScopeJobs(ctx, uuid.Nil, 0, &attemptID)
}

// ListScopeJobs returns non-terminal jobs for a run epoch. A nil attempt lists
// every active job in that epoch. A nil run lists by attempt only.
func (s *Store) ListScopeJobs(ctx context.Context, runID uuid.UUID, epoch int, attemptID *uuid.UUID) ([]jobRecord, error) {
	query := `
		SELECT id, run_id, epoch_number, work_item_id, attempt_id, state, container_id
		FROM execution_jobs
		WHERE state IN ('queued','running')`
	args := []any{}
	if runID != uuid.Nil {
		args = append(args, runID)
		query += fmt.Sprintf(" AND run_id=$%d", len(args))
		if epoch >= 1 {
			args = append(args, epoch)
			query += fmt.Sprintf(" AND epoch_number=$%d", len(args))
		}
	}
	if attemptID != nil && *attemptID != uuid.Nil {
		args = append(args, *attemptID)
		query += fmt.Sprintf(" AND attempt_id=$%d", len(args))
	}
	if len(args) == 0 {
		return nil, invalidInput("job revoke requires a run or attempt")
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, persistence("execution jobs could not be listed")
	}
	defer rows.Close()
	var out []jobRecord
	for rows.Next() {
		var rec jobRecord
		if err := rows.Scan(&rec.ID, &rec.RunID, &rec.Epoch, &rec.WorkItemID, &rec.AttemptID, &rec.State, &rec.ContainerID); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) SaveChildArtifact(ctx context.Context, scope contract.ExecutionScope, name, mediaType, content string) (uuid.UUID, error) {
	content = strings.ToValidUTF8(content, "\uFFFD")
	if len(content) > service.MaxArtifactBytes {
		content = content[:service.MaxArtifactBytes]
		content = strings.ToValidUTF8(content, "")
	}
	if !utf8.ValidString(content) {
		return uuid.Nil, invalidInput("execution artifact is not valid text")
	}
	if name == "" || len(name) > 128 {
		return uuid.Nil, invalidInput("artifact name is not valid")
	}
	sum := sha256.Sum256([]byte(content))
	id := uuid.New()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO artifacts(id, run_id, work_item_id, session_id, source_tool_call_id, epoch_number, name, media_type, content, sha256, byte_length)
		VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,$9,$10)`,
		id, scope.RunID, scope.WorkItemID, scope.SessionID, scope.EpochNumber, name, mediaType, content, hex.EncodeToString(sum[:]), len(content))
	if err != nil {
		return uuid.Nil, persistence("execution artifact was not recorded")
	}
	return id, nil
}

func (s *Store) LinkArtifact(ctx context.Context, jobID, artifactID uuid.UUID, role, path string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO execution_job_artifacts(job_id, artifact_id, role, path)
		VALUES ($1,$2,$3,$4)`, jobID, artifactID, role, path)
	if err != nil {
		return persistence("execution artifact link was not recorded")
	}
	return nil
}

func (s *Store) AddTraffic(ctx context.Context, jobID, receiptID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO execution_job_traffic(job_id, receipt_id) VALUES ($1,$2)
		ON CONFLICT DO NOTHING`, jobID, receiptID)
	if err != nil {
		return persistence("execution traffic was not recorded")
	}
	return nil
}

func (s *Store) InsertFile(ctx context.Context, scope contract.ExecutionScope, artifactID uuid.UUID, path string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO execution_files(id, run_id, work_item_id, attempt_id, artifact_id, path)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		uuid.New(), scope.RunID, scope.WorkItemID, scope.AttemptID, artifactID, path)
	if err != nil {
		return persistence("execution file was not recorded")
	}
	return nil
}

func (s *Store) LatestFiles(ctx context.Context, scope contract.ExecutionScope) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT path, content FROM (
			SELECT f.path, a.content,
			       row_number() OVER (PARTITION BY f.path ORDER BY f.created_at DESC) AS n
			FROM execution_files f
			JOIN artifacts a ON a.id = f.artifact_id
			WHERE f.work_item_id=$1 AND f.run_id=$2
		) s WHERE n=1`, scope.WorkItemID, scope.RunID)
	if err != nil {
		return nil, persistence("execution workspace files could not be read")
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			return nil, persistence("execution workspace files could not be read")
		}
		out[path] = content
	}
	return out, rows.Err()
}

func (s *Store) Origins(ctx context.Context, scope contract.ExecutionScope) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.external_key, ra.snapshot
		FROM work_item_assets wia
		JOIN assets a ON a.id = wia.asset_id
		JOIN run_assets ra ON ra.asset_id = a.id AND ra.run_id = $2
		WHERE wia.work_item_id = $1 AND a.kind = 'http_endpoint'`, scope.WorkItemID, scope.RunID)
	if err != nil {
		return nil, persistence("execution origins could not be read")
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	var out []string
	for rows.Next() {
		var key string
		var snapshot []byte
		if err := rows.Scan(&key, &snapshot); err != nil {
			return nil, persistence("execution origins could not be read")
		}
		origin := originFromAsset(key, snapshot)
		if origin == "" {
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	return out, rows.Err()
}

func (s *Store) TouchWorkspace(ctx context.Context, scope contract.ExecutionScope) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO execution_workspaces(work_item_id, run_id, restored_attempt_id)
		VALUES ($1,$2,$3)
		ON CONFLICT (work_item_id) DO UPDATE
		SET restored_attempt_id=EXCLUDED.restored_attempt_id, updated_at=now()`,
		scope.WorkItemID, scope.RunID, scope.AttemptID)
	if err != nil {
		return persistence("execution workspace was not recorded")
	}
	return nil
}

func originFromAsset(externalKey string, snapshot []byte) string {
	var snap map[string]any
	if json.Unmarshal(snapshot, &snap) == nil {
		if meta, ok := snap["metadata"].(map[string]any); ok {
			if raw, ok := meta["url"].(string); ok {
				if origin := originOf(raw); origin != "" {
					return origin
				}
			}
		}
	}
	parts := strings.SplitN(externalKey, " ", 2)
	candidate := externalKey
	if len(parts) == 2 {
		candidate = parts[1]
	}
	return originOf(candidate)
}

func originOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return originKey(parsed)
}
