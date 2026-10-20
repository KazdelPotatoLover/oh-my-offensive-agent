package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/checker"
	"agent-workbench/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrRunTerminalCannotWriteChecks = errors.New("cannot record check results: run is stopping or in terminal state")
	ErrAssetNotInRunScope           = errors.New("target asset is not in run scope")
)

type CheckResultInput struct {
	AssetID             uuid.UUID
	CheckID             string
	CheckName           string
	CheckVersion        string
	Status              model.CheckStatus
	Explanation         string
	SourceToolCallID    *uuid.UUID
	SourceObservationID *uuid.UUID
	EvidenceSnapshot    json.RawMessage
	Details             json.RawMessage
	GenerateFinding     bool
	FindingTitle        string
	FindingCategory     string
	FindingSeverity     string
	FindingDescription  string
	FindingRemediation  string
}

type RecordCheckResultsOutput struct {
	RecordedCount     int                 `json:"recorded_count"`
	DeduplicatedCount int                 `json:"deduplicated_count"`
	CheckResults      []model.CheckResult `json:"check_results"`
	Findings          []model.Finding     `json:"findings"`
}

// RecordCheckResults records check results and candidate findings in an atomic transaction
// with strict Run terminal state protection and scope enforcement.
func (s *RunService) RecordCheckResults(
	ctx context.Context,
	runID uuid.UUID,
	attemptID *uuid.UUID,
	sessionID *uuid.UUID,
	workItemID *uuid.UUID,
	inputs []CheckResultInput,
) (*RecordCheckResultsOutput, error) {
	if len(inputs) == 0 {
		return &RecordCheckResultsOutput{}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock Run row and verify run is active (stopping and terminal states rejected)
	var runStatus model.RunStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus); err != nil {
		return nil, fmt.Errorf("lock run: %w", err)
	}
	if runStatus.IsTerminal() || runStatus == model.RunStatusStopping {
		return nil, fmt.Errorf("%w: current run status is %s", ErrRunTerminalCannotWriteChecks, runStatus)
	}

	// Bind the executing check to its current worker. Evidence may come from a
	// different inspection session, but never from a different Run or asset.
	if attemptID == nil || sessionID == nil || workItemID == nil {
		return nil, fmt.Errorf("check execution requires attempt, session and work item")
	}
	var plannedJSON json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT wi.input_data FROM attempts a JOIN work_items wi ON wi.id=a.work_item_id
 JOIN agent_sessions sess ON sess.id=a.session_id
 WHERE a.id=$1 AND sess.id=$2 AND wi.id=$3 AND wi.run_id=$4
 AND sess.run_id=wi.run_id AND sess.attempt_id=a.id AND sess.work_item_id=wi.id
 AND wi.work_type='run_security_check' AND wi.current_attempt_id=a.id AND wi.status='running' AND a.status='running' AND sess.status='running'`, *attemptID, *sessionID, *workItemID, runID).Scan(&plannedJSON); err != nil {
		return nil, err
	}
	var planned struct {
		AssetID uuid.UUID `json:"asset_id"`
		CheckID string    `json:"check_id"`
	}
	if err := json.Unmarshal(plannedJSON, &planned); err != nil {
		return nil, fmt.Errorf("invalid check work item: %w", err)
	}

	// 2. Fetch event sequence for this run
	var seq int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM run_events WHERE run_id = $1`, runID).Scan(&seq); err != nil {
		return nil, fmt.Errorf("get event sequence: %w", err)
	}

	output := &RecordCheckResultsOutput{
		CheckResults: make([]model.CheckResult, 0, len(inputs)),
		Findings:     make([]model.Finding, 0),
	}

	for i, in := range inputs {
		if in.AssetID == uuid.Nil {
			return nil, fmt.Errorf("item %d: asset_id is required", i)
		}
		if strings.TrimSpace(in.CheckID) == "" {
			return nil, fmt.Errorf("item %d: check_id is required", i)
		}
		if !in.Status.IsValid() {
			return nil, fmt.Errorf("item %d: invalid check status %q", i, in.Status)
		}

		// 3. Verify asset is in Run scope
		var inScope bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id = $1 AND asset_id = $2)`, runID, in.AssetID).Scan(&inScope)
		if err != nil {
			return nil, fmt.Errorf("check asset scope: %w", err)
		}
		if !inScope {
			return nil, fmt.Errorf("%w: asset %s", ErrAssetNotInRunScope, in.AssetID)
		}

		if planned.AssetID != in.AssetID || !checker.IncludesRule(planned.CheckID, in.CheckID) {
			return nil, fmt.Errorf("check outcome does not match planned rule or asset")
		}
		var inWork bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_item_assets wa JOIN assets a ON a.id=wa.asset_id WHERE wa.work_item_id=$1 AND wa.asset_id=$2 AND a.kind='http_endpoint')`, *workItemID, in.AssetID).Scan(&inWork); err != nil {
			return nil, err
		}
		if !inWork {
			return nil, fmt.Errorf("check asset is outside HTTP work item scope")
		}
		in, err = verifiedCheckInput(ctx, tx, runID, in)
		if err != nil {
			return nil, fmt.Errorf("check item %d: %w", i, err)
		}

		// Include the rule version and deduplicate missing-evidence results too.
		var existingCR model.CheckResult
		row := tx.QueryRow(ctx, `SELECT id,run_id,attempt_id,session_id,work_item_id,asset_id,
   check_id,check_name,check_version,status,explanation,source_tool_call_id,
   source_observation_id,evidence_snapshot,details,created_at FROM check_results
   WHERE run_id=$1 AND asset_id=$2 AND check_id=$3 AND check_version=$4
   AND source_tool_call_id IS NOT DISTINCT FROM $5`, runID, in.AssetID, in.CheckID, in.CheckVersion, in.SourceToolCallID)
		scanErr := row.Scan(&existingCR.ID, &existingCR.RunID, &existingCR.AttemptID, &existingCR.SessionID,
			&existingCR.WorkItemID, &existingCR.AssetID, &existingCR.CheckID, &existingCR.CheckName,
			&existingCR.CheckVersion, &existingCR.Status, &existingCR.Explanation, &existingCR.SourceToolCallID,
			&existingCR.SourceObservationID, &existingCR.EvidenceSnapshot, &existingCR.Details, &existingCR.CreatedAt)
		existingFound := scanErr == nil
		if scanErr != nil && !errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("dedup check: %w", scanErr)
		}

		if existingFound {
			output.DeduplicatedCount++
			output.CheckResults = append(output.CheckResults, existingCR)

			// Load associated finding if any
			var existingF model.Finding
			rowF := tx.QueryRow(ctx, `
				SELECT id, run_id, check_result_id, asset_id, title, category, severity,
				       status, description, remediation, source_tool_call_id, evidence_ref, created_at
				FROM findings
				WHERE check_result_id = $1`, existingCR.ID)
			if scanErr := rowF.Scan(
				&existingF.ID, &existingF.RunID, &existingF.CheckResultID, &existingF.AssetID,
				&existingF.Title, &existingF.Category, &existingF.Severity, &existingF.Status,
				&existingF.Description, &existingF.Remediation, &existingF.SourceToolCallID,
				&existingF.EvidenceRef, &existingF.CreatedAt,
			); scanErr == nil {
				output.Findings = append(output.Findings, existingF)
			} else if !errors.Is(scanErr, pgx.ErrNoRows) {
				return nil, fmt.Errorf("load deduplicated finding: %w", scanErr)
			}
			continue
		}

		// 5. Insert new CheckResult
		evidenceSnap := in.EvidenceSnapshot
		if len(evidenceSnap) == 0 {
			evidenceSnap = json.RawMessage(`{}`)
		}
		details := in.Details
		if len(details) == 0 {
			details = json.RawMessage(`{}`)
		}

		var cr model.CheckResult
		err = tx.QueryRow(ctx, `
			INSERT INTO check_results (
				id, run_id, attempt_id, session_id, work_item_id, asset_id,
				check_id, check_name, check_version, status, explanation,
				source_tool_call_id, source_observation_id, evidence_snapshot, details
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING id, run_id, attempt_id, session_id, work_item_id, asset_id,
			          check_id, check_name, check_version, status, explanation,
			          source_tool_call_id, source_observation_id, evidence_snapshot, details, created_at`,
			runID, attemptID, sessionID, workItemID, in.AssetID,
			in.CheckID, in.CheckName, in.CheckVersion, in.Status, in.Explanation,
			in.SourceToolCallID, in.SourceObservationID, evidenceSnap, details,
		).Scan(
			&cr.ID, &cr.RunID, &cr.AttemptID, &cr.SessionID, &cr.WorkItemID, &cr.AssetID,
			&cr.CheckID, &cr.CheckName, &cr.CheckVersion, &cr.Status, &cr.Explanation,
			&cr.SourceToolCallID, &cr.SourceObservationID, &cr.EvidenceSnapshot, &cr.Details, &cr.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("insert check result %d: %w", i, err)
		}

		output.RecordedCount++
		output.CheckResults = append(output.CheckResults, cr)

		// Record run_event for check_result
		seq++
		crPayload, _ := json.Marshal(map[string]interface{}{
			"check_result_id": cr.ID,
			"check_id":        cr.CheckID,
			"check_name":      cr.CheckName,
			"status":          cr.Status,
			"asset_id":        cr.AssetID,
			"explanation":     cr.Explanation,
		})
		if _, err = tx.Exec(ctx, `
			INSERT INTO run_events (id, run_id, seq, type, payload, session_id, work_item_id, attempt_id)
			VALUES (gen_random_uuid(), $1, $2, 'check_result', $3, $4, $5, $6)`,
			runID, seq, crPayload, sessionID, workItemID, attemptID); err != nil {
			return nil, fmt.Errorf("insert check event: %w", err)
		}

		// 6. If needs_attention, form candidate Finding for human review
		if in.Status == model.CheckStatusNeedsAttention && in.GenerateFinding {
			cat := in.FindingCategory
			if cat == "" {
				cat = "configuration_risk"
			}
			sev := in.FindingSeverity
			if sev == "" {
				sev = "low"
			}
			title := in.FindingTitle
			if title == "" {
				title = fmt.Sprintf("[配置关注] %s", in.CheckName)
			}

			var f model.Finding
			err = tx.QueryRow(ctx, `
				INSERT INTO findings (
					id, run_id, check_result_id, asset_id, title, category,
					severity, status, description, remediation, source_tool_call_id, evidence_ref
				) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'pending_review', $7, $8, $9, $10)
				RETURNING id, run_id, check_result_id, asset_id, title, category,
				          severity, status, description, remediation, source_tool_call_id, evidence_ref, created_at`,
				runID, cr.ID, cr.AssetID, title, cat,
				sev, in.FindingDescription, in.FindingRemediation, in.SourceToolCallID, evidenceSnap,
			).Scan(
				&f.ID, &f.RunID, &f.CheckResultID, &f.AssetID, &f.Title, &f.Category,
				&f.Severity, &f.Status, &f.Description, &f.Remediation, &f.SourceToolCallID,
				&f.EvidenceRef, &f.CreatedAt,
			)
			if err != nil {
				return nil, fmt.Errorf("insert candidate finding for check %d: %w", i, err)
			}
			output.Findings = append(output.Findings, f)

			// Record run_event for finding
			seq++
			fPayload, _ := json.Marshal(map[string]interface{}{
				"finding_id":      f.ID,
				"check_result_id": f.CheckResultID,
				"asset_id":        f.AssetID,
				"title":           f.Title,
				"category":        f.Category,
				"severity":        f.Severity,
				"status":          f.Status,
			})
			if _, err = tx.Exec(ctx, `
				INSERT INTO run_events (id, run_id, seq, type, payload, session_id, work_item_id, attempt_id)
				VALUES (gen_random_uuid(), $1, $2, 'finding', $3, $4, $5, $6)`,
				runID, seq, fPayload, sessionID, workItemID, attemptID); err != nil {
				return nil, fmt.Errorf("insert finding event: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit check results: %w", err)
	}

	return output, nil
}

// GetLatestHTTPEvidence retrieves the most recent successful get_endpoint tool call for an asset in a Run.
func (s *RunService) GetLatestHTTPEvidence(ctx context.Context, runID, assetID uuid.UUID) (*model.AgentToolCall, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, run_id, session_id, model_call_id, call_id, tool_name,
		       arguments, output, status, error_message, started_at, finished_at, created_at
		FROM agent_tool_calls
		WHERE run_id = $1
		  AND tool_name = 'get_endpoint'
		  AND status = 'success'
		  AND arguments->>'asset_id' = $2
		ORDER BY created_at DESC
		LIMIT 1`, runID, assetID.String())

	var tc model.AgentToolCall
	err := row.Scan(
		&tc.ID, &tc.RunID, &tc.SessionID, &tc.ModelCallID, &tc.CallID, &tc.ToolName,
		&tc.Arguments, &tc.Output, &tc.Status, &tc.ErrorMessage, &tc.StartedAt, &tc.FinishedAt, &tc.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query latest http evidence: %w", err)
	}
	return &tc, nil
}

// ListCheckResultsByRun retrieves all check results for a run.
func (s *RunService) ListCheckResultsByRun(ctx context.Context, runID uuid.UUID) ([]model.CheckResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, attempt_id, session_id, work_item_id, asset_id,
		       check_id, check_name, check_version, status, explanation,
		       source_tool_call_id, source_observation_id, evidence_snapshot, details, created_at
		FROM check_results
		WHERE run_id = $1
		ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list check results: %w", err)
	}
	defer rows.Close()

	results := make([]model.CheckResult, 0)
	for rows.Next() {
		var cr model.CheckResult
		if err := rows.Scan(
			&cr.ID, &cr.RunID, &cr.AttemptID, &cr.SessionID, &cr.WorkItemID, &cr.AssetID,
			&cr.CheckID, &cr.CheckName, &cr.CheckVersion, &cr.Status, &cr.Explanation,
			&cr.SourceToolCallID, &cr.SourceObservationID, &cr.EvidenceSnapshot, &cr.Details, &cr.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan check result: %w", err)
		}
		results = append(results, cr)
	}
	return results, rows.Err()
}

// ListCheckResultsByAttempt includes original results and verified result reuse
// recorded by successful check tool calls in this Attempt. Original ownership stays intact.
func (s *RunService) ListCheckResultsByAttempt(ctx context.Context, attemptID uuid.UUID) ([]model.CheckResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cr.id, cr.run_id, cr.attempt_id, cr.session_id, cr.work_item_id, cr.asset_id,
         cr.check_id, cr.check_name, cr.check_version, cr.status, cr.explanation,
         cr.source_tool_call_id, cr.source_observation_id, cr.evidence_snapshot, cr.details, cr.created_at
  FROM check_results cr
  WHERE cr.attempt_id=$1 OR EXISTS (
   SELECT 1 FROM agent_tool_calls tc JOIN attempts a ON a.session_id=tc.session_id
   JOIN work_items wi ON wi.id=a.work_item_id
   WHERE a.id=$1 AND wi.run_id=cr.run_id AND tc.run_id=cr.run_id
   AND tc.tool_name='run_security_check' AND tc.status='success'
   AND (tc.output::jsonb->'check_results') @> jsonb_build_array(jsonb_build_object('id',cr.id::text))
  ) ORDER BY cr.created_at ASC`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("list check results by attempt: %w", err)
	}
	defer rows.Close()

	results := make([]model.CheckResult, 0)
	for rows.Next() {
		var cr model.CheckResult
		if err := rows.Scan(
			&cr.ID, &cr.RunID, &cr.AttemptID, &cr.SessionID, &cr.WorkItemID, &cr.AssetID,
			&cr.CheckID, &cr.CheckName, &cr.CheckVersion, &cr.Status, &cr.Explanation,
			&cr.SourceToolCallID, &cr.SourceObservationID, &cr.EvidenceSnapshot, &cr.Details, &cr.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan check result: %w", err)
		}
		results = append(results, cr)
	}
	return results, rows.Err()
}

// ListFindingsByRun retrieves all findings for a run.
func (s *RunService) ListFindingsByRun(ctx context.Context, runID uuid.UUID) ([]model.Finding, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, check_result_id, asset_id, title, category,
		       severity, status, description, remediation, source_tool_call_id, evidence_ref, created_at
		FROM findings
		WHERE run_id = $1
		ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()

	findings := make([]model.Finding, 0)
	for rows.Next() {
		var f model.Finding
		if err := rows.Scan(
			&f.ID, &f.RunID, &f.CheckResultID, &f.AssetID, &f.Title, &f.Category,
			&f.Severity, &f.Status, &f.Description, &f.Remediation, &f.SourceToolCallID,
			&f.EvidenceRef, &f.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		findings = append(findings, f)
	}
	return findings, rows.Err()
}

// Recompute from the stored response rather than accepting caller-authored
// outcomes or finding severity. Only source identities cross this trust boundary.
func verifiedCheckInput(ctx context.Context, tx pgx.Tx, runID uuid.UUID, in CheckResultInput) (CheckResultInput, error) {
	if !checker.IsRegisteredCheck(in.CheckID) {
		return in, ErrUnknownCheckID
	}
	if in.SourceObservationID != nil {
		var source uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT source_tool_call_id FROM observations WHERE id=$1 AND run_id=$2 AND asset_id=$3`, *in.SourceObservationID, runID, in.AssetID).Scan(&source); err != nil {
			return in, fmt.Errorf("invalid check observation source: %w", err)
		}
		if in.SourceToolCallID != nil && *in.SourceToolCallID != source {
			return in, ErrInvalidObservationSource
		}
		in.SourceToolCallID = &source
	}
	var evidence []byte
	if in.SourceToolCallID != nil {
		var output string
		if err := tx.QueryRow(ctx, `SELECT output FROM agent_tool_calls WHERE id=$1 AND run_id=$2 AND status='success' AND tool_name='get_endpoint' AND lower(btrim(arguments->>'asset_id'))=$3`, *in.SourceToolCallID, runID, in.AssetID.String()).Scan(&output); err != nil {
			return in, fmt.Errorf("invalid check HTTP source: %w", ErrInvalidObservationSource)
		}
		evidence = []byte(output)
		var payload struct {
			Request struct {
				AssetID string `json:"asset_id"`
			} `json:"request"`
		}
		if json.Unmarshal(evidence, &payload) == nil && payload.Request.AssetID != "" && !strings.EqualFold(payload.Request.AssetID, in.AssetID.String()) {
			return in, ErrAssetSampleMismatch
		}
	}
	evaluated := checker.EvaluateHTTPResponseChecks(in.CheckID, evidence)
	if len(evaluated) != 1 || evaluated[0].CheckID != in.CheckID {
		return in, fmt.Errorf("record individual check results rather than a suite outcome")
	}
	r := evaluated[0]
	return CheckResultInput{AssetID: in.AssetID, CheckID: r.CheckID, CheckName: r.CheckName, CheckVersion: r.CheckVersion,
		Status: r.Status, Explanation: r.Explanation, SourceToolCallID: in.SourceToolCallID, SourceObservationID: in.SourceObservationID,
		EvidenceSnapshot: r.EvidenceSnapshot, Details: r.Details, GenerateFinding: r.GenerateFinding, FindingTitle: r.FindingTitle,
		FindingCategory: r.FindingCategory, FindingSeverity: r.FindingSeverity, FindingDescription: r.FindingDescription, FindingRemediation: r.FindingRemediation}, nil
}
