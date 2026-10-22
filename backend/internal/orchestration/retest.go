package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RetestExecutor executes independent verification sessions for reported findings.
type RetestExecutor struct {
	runService      *service.RunService
	provider        provider.Provider
	registry        *tools.Registry
	requestLimiter  *provider.RequestLimiter
	maxOutputTokens int
	recovery        provider.RecoveryPolicy
}

func NewRetestExecutor(runService *service.RunService, p provider.Provider, reg *tools.Registry) *RetestExecutor {
	return &RetestExecutor{
		runService: runService,
		provider:   p,
		registry:   reg,
	}
}

// ApplyProcessSettings attaches the same upstream cap and recovery policy used
// by planner and worker sessions. A nil limiter does not invent a serial fallback.
func (e *RetestExecutor) ApplyProcessSettings(limiter *provider.RequestLimiter, tokens int, recovery provider.RecoveryPolicy) {
	if e == nil {
		return
	}
	e.requestLimiter = limiter
	e.maxOutputTokens = tokens
	e.recovery = recovery
}

func (e *RetestExecutor) sessionOptions(opts ...agent.SessionRunnerOption) []agent.SessionRunnerOption {
	return modelSessionOptions(e.requestLimiter, e.maxOutputTokens, e.recovery, opts...)
}

// ExecuteFindingRetest independently reproduces or verifies fixes for a finding.
func (e *RetestExecutor) ExecuteFindingRetest(
	ctx context.Context,
	runID, findingID, retestID uuid.UUID,
) (*model.FindingRetest, error) {
	pool := e.runService.Pool()
	if pool == nil {
		return nil, errors.New("database pool unavailable")
	}

	var title, category, severity, sourceKind, verificationState string
	var assetID uuid.UUID
	var detailsBytes, evidenceRefBytes []byte

	err := pool.QueryRow(ctx, `
		SELECT f.title, f.category, f.severity, f.source_kind, f.verification_state,
		       f.asset_id, cr.details, f.evidence_ref
		FROM findings f
		LEFT JOIN check_results cr ON f.check_result_id = cr.id
		WHERE f.id = $1 AND f.run_id = $2`,
		findingID, runID,
	).Scan(&title, &category, &severity, &sourceKind, &verificationState,
		&assetID, &detailsBytes, &evidenceRefBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, service.ErrNotFound
		}
		return nil, fmt.Errorf("query finding for retest: %w", err)
	}

	var sessID uuid.UUID
	var retestStatus string
	err = pool.QueryRow(ctx, `
		SELECT session_id, status FROM finding_retests
		WHERE id = $1 AND finding_id = $2 AND run_id = $3`,
		retestID, findingID, runID,
	).Scan(&sessID, &retestStatus)
	if err != nil {
		return nil, fmt.Errorf("query retest session: %w", err)
	}
	if retestStatus != "running" {
		return nil, fmt.Errorf("%w: retest status is %s", service.ErrInvalidState, retestStatus)
	}

	unsafe, reason, scheme, replayErr := e.runService.FindingReplayUnsafe(ctx, findingID)
	if replayErr != nil {
		return e.runService.FinishFindingRetest(ctx, runID, findingID, retestID, "inconclusive", "", "Inconclusive: replay safety could not be read")
	}
	if unsafe {
		if reason == "" {
			reason = "original exchange cannot be safely auto-replayed"
		}
		return e.runService.FinishFindingRetest(ctx, runID, findingID, retestID, "inconclusive", "", "Inconclusive: "+reason)
	}

	verdict := "inconclusive"
	summary := "Independent retest completed"
	evidenceStr := ""

	// If a model provider and registry are available, execute an active verifier agent session
	if e.provider != nil {
		reg := e.registry
		if reg == nil {
			reg = tools.NewRegistry()
		}
		tools.RegisterArtifactReaders(reg, tools.AdapterContext{RunID: runID, SessionID: sessID, Service: e.runService})
		evidenceTool := "get_endpoint"
		if scheme == "https" {
			evidenceTool = "http_request"
			if _, exists := reg.Get("http_request"); !exists {
				transport := netcap.Shared()
				if transport == nil {
					return e.runService.FinishFindingRetest(ctx, runID, findingID, retestID, "inconclusive", "", "Inconclusive: HTTPS auto retest requires http_request")
				}
				reg.Register(tools.NewHTTPRequestTool(runID, uuid.Nil, uuid.Nil, sessID, e.runService, transport))
			}
		}
		if evidenceTool == "get_endpoint" {
			if _, exists := reg.Get("get_endpoint"); !exists {
				reg.Register(tools.NewGetEndpointTool(runID, uuid.Nil, e.runService))
			}
		}

		verifierPrompt := fmt.Sprintf(
			"You are an Independent Security Verifier. Your task is to verify whether the following finding is still reproducible or fixed.\n"+
				"Finding: %s (%s, %s)\nAsset ID: %s\nDetails: %s\nPrior Evidence: %s\n"+
				"Run verification tools against this exact target. The server evaluates the stored verification conditions against your current response. Missing or ambiguous conditions are inconclusive; your final text is only a note.",
			title, category, severity, assetID.String(), string(detailsBytes), string(evidenceRefBytes))

		verifierMessages := []provider.ChatMessage{
			{Role: provider.RoleSystem, Content: "You are an Independent Security Verifier. Faithfully reproduce or verify findings using tools."},
			{Role: provider.RoleUser, Content: verifierPrompt},
		}

		budget := model.BudgetConfig{
			MaxModelCalls: 5,
			MaxToolCalls:  10,
		}
		sr := agent.NewSessionRunner(sessID, runID, "verifier", e.runService, e.provider, reg, budget, e.sessionOptions()...)
		res, runErr := sr.Run(ctx, verifierMessages)

		if runErr == nil && res != nil {
			var sourceID uuid.UUID
			err := pool.QueryRow(ctx, `SELECT id FROM agent_tool_calls
				WHERE session_id=$1 AND tool_name=$2 AND status='success'
				AND arguments->>'asset_id'=$3 ORDER BY created_at DESC,id DESC LIMIT 1`, sessID, evidenceTool, assetID.String()).Scan(&sourceID)
			if err == nil {
				evidence, _ := json.Marshal(map[string]any{"source_tool_call_id": sourceID, "model_notes": res.FinalContent})
				return e.runService.FinishFindingRetestFromEvidence(ctx, runID, findingID, retestID, string(evidence), "Evaluated the original verification conditions against this retest's target response")
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("load retest evidence: %w", err)
			}
			summary = "Inconclusive: no successful target verification in this retest session"
		} else {
			summary = "Inconclusive: verification failed or incomplete"
		}

	} else {
		// provider == nil: cannot execute verification tools, strictly inconclusive
		verdict = "inconclusive"
		summary = fmt.Sprintf("Independent retest could not verify finding %s: no verifier provider configured", title)
	}

	return e.runService.FinishFindingRetest(ctx, runID, findingID, retestID, verdict, evidenceStr, summary)
}
