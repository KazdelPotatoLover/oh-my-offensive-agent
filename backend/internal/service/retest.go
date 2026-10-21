package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrRetestActiveConflict = errors.New("an active retest is already running for this finding")
	ErrInvalidVerdict       = errors.New("invalid retest verdict: must be still_reproducible, fixed, or inconclusive")
	ErrRetestNotFound       = errors.New("finding retest not found")
)

type ReportFindingInput struct {
	AssetID          uuid.UUID            `json:"asset_id"`
	Title            string               `json:"title"`
	Category         string               `json:"category"`
	Severity         string               `json:"severity"` // 'info', 'low', 'medium', 'high', 'critical'
	Description      string               `json:"description"`
	Remediation      string               `json:"remediation"`
	SourceKind       string               `json:"source_kind"` // 'check_rule', 'speculative', 'empirical'
	EvidenceRef      json.RawMessage      `json:"evidence_ref"`
	TriggerCondition string               `json:"trigger_condition"`
	ReproduceSteps   string               `json:"reproduce_steps"`
	SourceToolCallID *uuid.UUID           `json:"source_tool_call_id,omitempty"`
	Verification     *FindingVerification `json:"verification,omitempty"`
}

// ReportFinding creates a first-class finding with full evidence and exploration node linkage.
func (s *RunService) ReportFinding(ctx context.Context, runID uuid.UUID, in ReportFindingInput) (*model.Finding, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("finding title cannot be empty")
	}
	if in.AssetID == uuid.Nil {
		return nil, errors.New("finding asset_id is required")
	}

	sourceKind := strings.TrimSpace(in.SourceKind)
	if sourceKind == "" {
		sourceKind = "empirical"
	}
	if sourceKind != "check_rule" && sourceKind != "speculative" && sourceKind != "empirical" {
		return nil, fmt.Errorf("invalid source_kind: %s", sourceKind)
	}

	verificationState := "unverified"
	if sourceKind == "empirical" {
		verificationState = "verified"
	}

	severity := strings.ToLower(strings.TrimSpace(in.Severity))
	if severity == "" {
		severity = "medium"
	}

	evidenceRef := in.EvidenceRef
	if len(evidenceRef) == 0 {
		evidenceRef = json.RawMessage(`{}`)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify run exists
	var currentEpoch int
	err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("verify run: %w", err)
	}

	if err := validateContextEpoch(ctx, currentEpoch); err != nil {
		return nil, err
	}
	// Verify asset belongs to run scope
	var inScope bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM run_assets WHERE run_id = $1 AND asset_id = $2)`,
		runID, in.AssetID).Scan(&inScope)
	if err != nil {
		return nil, fmt.Errorf("verify asset scope: %w", err)
	}
	if !inScope {
		return nil, fmt.Errorf("%w: asset %s is not in run scope", ErrAssetOutOfScope, in.AssetID)
	}

	if sourceKind == "empirical" {
		if in.SourceToolCallID == nil || *in.SourceToolCallID == uuid.Nil {
			return nil, fmt.Errorf("%w: empirical finding requires verified source_tool_call_id", ErrInvalidEvidence)
		}
		response, err := targetEvidence(ctx, tx, runID, in.AssetID, *in.SourceToolCallID, currentEpoch, nil)
		if err != nil {
			return nil, err
		}
		if in.Verification != nil && (!in.Verification.Vulnerable.valid() || !in.Verification.Fixed.valid() || !in.Verification.Vulnerable.matches(response) || in.Verification.Fixed.matches(response)) {
			return nil, fmt.Errorf("%w: initial evidence must uniquely match the vulnerable verification condition", ErrInvalidEvidence)
		}

		verificationState = "verified"
	}

	// Create underlying check_result for consistent reporting
	checkID := "empirical_vuln"
	if sourceKind == "speculative" {
		checkID = "speculative_lead"
	}
	var checkResultID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO check_results (
			id, run_id, asset_id, check_id, check_name, check_version,
			status, explanation, source_tool_call_id, evidence_snapshot, details
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, 'v1',
			'needs_attention', $5, $6, $7, $8
		) RETURNING id`,
		runID, in.AssetID, checkID, in.Title,
		in.Description, in.SourceToolCallID, evidenceRef,
		map[string]any{"trigger": in.TriggerCondition, "steps": in.ReproduceSteps, "verification": in.Verification},
	).Scan(&checkResultID)
	if err != nil {
		return nil, fmt.Errorf("create check_result for finding: %w", err)
	}

	// Insert into findings
	var f model.Finding
	err = tx.QueryRow(ctx, `
		INSERT INTO findings (
			id, run_id, check_result_id, asset_id, title, category,
			severity, status, description, remediation, source_tool_call_id,
			evidence_ref, review_version, source_kind, verification_state, epoch_number
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5,
			$6, 'pending_review', $7, $8, $9,
			$10, 0, $11, $12, $13
		) RETURNING id, run_id, check_result_id, asset_id, title, category,
		            severity, status, description, remediation, source_tool_call_id,
		            evidence_ref, created_at, review_version, last_reviewed_at,
		            last_reviewer, last_review_comment, source_kind, verification_state,
		            retest_verdict, retest_notes, retest_at`,
		runID, checkResultID, in.AssetID, in.Title, in.Category,
		severity, in.Description, in.Remediation, in.SourceToolCallID,
		evidenceRef, sourceKind, verificationState, currentEpoch,
	).Scan(
		&f.ID, &f.RunID, &f.CheckResultID, &f.AssetID, &f.Title, &f.Category,
		&f.Severity, &f.Status, &f.Description, &f.Remediation, &f.SourceToolCallID,
		&f.EvidenceRef, &f.CreatedAt, &f.ReviewVersion, &f.LastReviewedAt,
		&f.LastReviewer, &f.LastReviewComment, &f.SourceKind, &f.VerificationState,
		&f.RetestVerdict, &f.RetestNotes, &f.RetestAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert finding: %w", err)
	}

	// Link into exploration_nodes as kind='finding'
	findingPayload, _ := json.Marshal(map[string]any{
		"title":              f.Title,
		"category":           f.Category,
		"severity":           f.Severity,
		"source_kind":        f.SourceKind,
		"verification_state": f.VerificationState,
		"description":        f.Description,
	})
	var nodeID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO exploration_nodes (run_id, kind, payload, state, origin, finding_id, epoch_number)
		VALUES ($1, 'finding', $2, $3, $4, $5, $6)
		RETURNING id`,
		runID, findingPayload, verificationState, sourceKind, f.ID, currentEpoch,
	).Scan(&nodeID)
	if err != nil {
		return nil, fmt.Errorf("link finding into exploration_nodes: %w", err)
	}

	// Link anchor to target asset
	_, _ = tx.Exec(ctx, `
		INSERT INTO exploration_anchors (run_id, node_id, asset_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`,
		runID, nodeID, f.AssetID)

	// Record run_event
	eventPayload, _ := json.Marshal(map[string]any{
		"finding_id":         f.ID,
		"title":              f.Title,
		"category":           f.Category,
		"severity":           f.Severity,
		"source_kind":        f.SourceKind,
		"verification_state": f.VerificationState,
		"node_id":            nodeID,
	})
	_, _ = tx.Exec(ctx, `
		INSERT INTO run_events (id, run_id, seq, type, payload)
		VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'finding_reported', $2)`,
		runID, eventPayload)

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit finding report: %w", err)
	}
	return &f, nil
}

// CreateFindingRetest starts an independent verification session to independently reproduce or verify fix.
func (s *RunService) CreateFindingRetest(ctx context.Context, runID, findingID uuid.UUID, notes string) (*model.FindingRetest, *model.AgentSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock run first
	var currentEpoch int
	err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("verify run: %w", err)
	}

	// Verify finding exists in run scope
	var fExists bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM findings WHERE id = $1 AND run_id = $2 FOR UPDATE)`,
		findingID, runID,
	).Scan(&fExists)
	if err != nil || !fExists {
		return nil, nil, ErrNotFound
	}

	// 2. Concurrency rule: at most one active retest per finding
	var activeCount int
	err = tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM finding_retests
		WHERE finding_id = $1 AND status = 'running'`,
		findingID,
	).Scan(&activeCount)
	if err != nil {
		return nil, nil, fmt.Errorf("check active retests: %w", err)
	}
	if activeCount > 0 {
		return nil, nil, ErrRetestActiveConflict
	}

	// 3. Create independent verifier session
	sessID := uuid.New()
	var sess model.AgentSession
	snapshot, _ := json.Marshal(map[string]any{
		"finding_id": findingID,
		"notes":      notes,
	})
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_sessions (id, run_id, role, status, input_snapshot, epoch_number)
		VALUES ($1, $2, 'verifier', 'running', $3, $4)
		RETURNING id, run_id, role, round, work_item_id, attempt_id, status, input_snapshot, finish_reason, created_at, updated_at`,
		sessID, runID, snapshot, currentEpoch,
	).Scan(
		&sess.ID, &sess.RunID, &sess.Role, &sess.Round, &sess.WorkItemID, &sess.AttemptID,
		&sess.Status, &sess.InputSnapshot, &sess.FinishReason, &sess.CreatedAt, &sess.UpdatedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create verifier session: %w", err)
	}

	// 4. Create retest record
	var retest model.FindingRetest
	err = tx.QueryRow(ctx, `
		INSERT INTO finding_retests (id, finding_id, run_id, status, notes, session_id, created_at)
		VALUES (gen_random_uuid(), $1, $2, 'running', $3, $4, now())
		RETURNING id, finding_id, run_id, status, verdict, notes, evidence, summary, session_id, created_at, finished_at`,
		findingID, runID, notes, sess.ID,
	).Scan(
		&retest.ID, &retest.FindingID, &retest.RunID, &retest.Status, &retest.Verdict,
		&retest.Notes, &retest.Evidence, &retest.Summary, &retest.SessionID,
		&retest.CreatedAt, &retest.FinishedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("insert finding_retest: %w", err)
	}

	// 5. Run event
	evPayload, _ := json.Marshal(map[string]any{
		"retest_id":  retest.ID,
		"finding_id": findingID,
		"session_id": sess.ID,
	})
	_, _ = tx.Exec(ctx, `
		INSERT INTO run_events (id, run_id, seq, type, payload)
		VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'finding_retest_started', $2)`,
		runID, evPayload)

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit retest creation: %w", err)
	}
	return &retest, &sess, nil
}

// FinishFindingRetest completes an independent retest with an explicit verdict.
// Verdict must be 'still_reproducible', 'fixed', or 'inconclusive'.
func (s *RunService) FinishFindingRetest(
	ctx context.Context,
	runID, findingID, retestID uuid.UUID,
	verdict, evidence, summary string,
) (*model.FindingRetest, error) {
	return s.finishFindingRetest(ctx, runID, findingID, retestID, verdict, evidence, summary, false)
}

// FinishFindingRetestFromEvidence derives the verdict from the stored condition
// and this retest's response; model prose never selects the verdict.
func (s *RunService) FinishFindingRetestFromEvidence(ctx context.Context, runID, findingID, retestID uuid.UUID, evidence, summary string) (*model.FindingRetest, error) {
	return s.finishFindingRetest(ctx, runID, findingID, retestID, "inconclusive", evidence, summary, true)
}

func (s *RunService) finishFindingRetest(ctx context.Context, runID, findingID, retestID uuid.UUID, verdict, evidence, summary string, derive bool) (*model.FindingRetest, error) {
	v := strings.TrimSpace(verdict)
	if v != "still_reproducible" && v != "fixed" && v != "inconclusive" {
		return nil, fmt.Errorf("%w: '%s'", ErrInvalidVerdict, verdict)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock run first
	var currentEpoch int
	err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRetestNotFound
		}
		return nil, fmt.Errorf("verify run: %w", err)
	}

	var retest model.FindingRetest
	err = tx.QueryRow(ctx, `
		SELECT id, finding_id, run_id, status, verdict, notes, evidence, summary, session_id, created_at, finished_at
		FROM finding_retests
		WHERE id = $1 AND finding_id = $2 AND run_id = $3
		FOR UPDATE`,
		retestID, findingID, runID,
	).Scan(
		&retest.ID, &retest.FindingID, &retest.RunID, &retest.Status, &retest.Verdict,
		&retest.Notes, &retest.Evidence, &retest.Summary, &retest.SessionID,
		&retest.CreatedAt, &retest.FinishedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRetestNotFound
		}
		return nil, fmt.Errorf("query retest: %w", err)
	}
	if retest.Status != "running" {
		return nil, fmt.Errorf("%w: retest is already in status %s", ErrInvalidState, retest.Status)
	}

	if derive || v != "inconclusive" {
		determined, err := evaluateRetest(ctx, tx, runID, findingID, retest.SessionID, evidence, currentEpoch)
		if err != nil {
			if !derive {
				return nil, err
			}
			determined = "inconclusive"
		}
		if derive {
			v = determined
		} else if determined != v {
			return nil, fmt.Errorf("%w: requested verdict is not supported by current verification evidence", ErrInvalidEvidence)
		}
	}

	now := time.Now()

	// Update retest
	err = tx.QueryRow(ctx, `
		UPDATE finding_retests
		SET status = 'completed', verdict = $1, evidence = $2, summary = $3, finished_at = $4
		WHERE id = $5
		RETURNING id, finding_id, run_id, status, verdict, notes, evidence, summary, session_id, created_at, finished_at`,
		v, evidence, summary, now, retestID,
	).Scan(
		&retest.ID, &retest.FindingID, &retest.RunID, &retest.Status, &retest.Verdict,
		&retest.Notes, &retest.Evidence, &retest.Summary, &retest.SessionID,
		&retest.CreatedAt, &retest.FinishedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("update finding_retest: %w", err)
	}

	// Update session if attached
	if retest.SessionID != nil {
		_, err = tx.Exec(ctx, `
			UPDATE agent_sessions
			SET status = 'completed', finish_reason = $1, updated_at = $2
			WHERE id = $3 AND status = 'running'`,
			v, now, *retest.SessionID)
		if err != nil {
			return nil, fmt.Errorf("update session finish: %w", err)
		}
	}

	// Update findings verification state and retest columns
	// NOTE: Original finding human review status is PRESERVED!
	_, err = tx.Exec(ctx, `
		UPDATE findings
		SET verification_state = 'retested',
		    retest_verdict = $1,
		    retest_notes = $2,
		    retest_at = $3
		WHERE id = $4`,
		v, summary, now, findingID)
	if err != nil {
		return nil, fmt.Errorf("update finding retest status: %w", err)
	}

	if v == "fixed" {
		if err := invalidateFindingProofs(ctx, tx, runID, findingID, "independent_retest_fixed"); err != nil {
			return nil, err
		}
	}

	// Run event
	evPayload, _ := json.Marshal(map[string]any{
		"retest_id":  retestID,
		"finding_id": findingID,
		"verdict":    v,
		"summary":    summary,
	})
	_, err = tx.Exec(ctx, `
		INSERT INTO run_events (id, run_id, seq, type, payload)
		VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'finding_retest_finished', $2)`,
		runID, evPayload)
	if err != nil {
		return nil, fmt.Errorf("insert retest finished event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit finish retest: %w", err)
	}
	return &retest, nil
}

// StopFindingRetest aborts an active retest without marking it complete.
func (s *RunService) StopFindingRetest(ctx context.Context, runID, findingID, retestID uuid.UUID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// 1. Lock run first
	var runStatus model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRetestNotFound
		}
		return fmt.Errorf("verify run: %w", err)
	}

	var sessID *uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT session_id FROM finding_retests
		WHERE id = $1 AND finding_id = $2 AND run_id = $3 AND status = 'running'
		FOR UPDATE`,
		retestID, findingID, runID,
	).Scan(&sessID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRetestNotFound
		}
		return err
	}

	now := time.Now()
	_, err = tx.Exec(ctx, `
		UPDATE finding_retests
		SET status = 'stopped', summary = $1, finished_at = $2
		WHERE id = $3`,
		reason, now, retestID)
	if err != nil {
		return fmt.Errorf("stop retest: %w", err)
	}

	if sessID != nil {
		_, _ = tx.Exec(ctx, `
			UPDATE agent_sessions
			SET status = 'stopped', finish_reason = $1, updated_at = $2
			WHERE id = $3 AND status = 'running'`,
			reason, now, *sessID)
	}

	return tx.Commit(ctx)
}

// ListFindingRetests lists all retests for a finding.
func (s *RunService) ListFindingRetests(ctx context.Context, runID, findingID uuid.UUID) ([]model.FindingRetest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, finding_id, run_id, status, verdict, notes, evidence, summary, session_id, created_at, finished_at
		FROM finding_retests
		WHERE run_id = $1 AND finding_id = $2
		ORDER BY created_at ASC`,
		runID, findingID)
	if err != nil {
		return nil, fmt.Errorf("query finding retests: %w", err)
	}
	defer rows.Close()

	var retests []model.FindingRetest
	for rows.Next() {
		var rt model.FindingRetest
		if err := rows.Scan(
			&rt.ID, &rt.FindingID, &rt.RunID, &rt.Status, &rt.Verdict,
			&rt.Notes, &rt.Evidence, &rt.Summary, &rt.SessionID,
			&rt.CreatedAt, &rt.FinishedAt,
		); err != nil {
			return nil, err
		}
		retests = append(retests, rt)
	}
	if retests == nil {
		retests = []model.FindingRetest{}
	}
	return retests, nil
}
