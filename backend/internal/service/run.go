package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RunService struct {
	pool *pgxpool.Pool
}

func NewRunService(pool *pgxpool.Pool) *RunService {
	return &RunService{pool: pool}
}

func (s *RunService) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *RunService) ListByTask(ctx context.Context, taskID uuid.UUID) ([]model.Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
		        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
		 FROM runs WHERE task_id = $1 ORDER BY created_at DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var runs []model.Run
	for rows.Next() {
		var r model.Run
		if err := rows.Scan(&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.ScopePolicyVersion, &r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	if runs == nil {
		runs = []model.Run{}
	}
	return runs, nil
}

func (s *RunService) Get(ctx context.Context, id uuid.UUID) (*model.Run, error) {
	var r model.Run
	err := s.pool.QueryRow(ctx,
		`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
		        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
		 FROM runs WHERE id = $1`, id).Scan(
		&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
		&r.ScopePolicyVersion, &r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query run: %w", err)
	}
	return &r, nil
}

// Create creates a new run for a task. Returns the run, whether it already existed (idempotent hit), and any error.
func (s *RunService) Create(ctx context.Context, taskID uuid.UUID, req model.CreateRunRequest, idempotencyKey *string) (*model.Run, bool, error) {
	// If idempotency key provided, check for existing run
	if idempotencyKey != nil && *idempotencyKey != "" {
		var existing model.Run
		err := s.pool.QueryRow(ctx,
			`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
			        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
			 FROM runs WHERE task_id = $1 AND idempotency_key = $2`,
			taskID, *idempotencyKey).Scan(
			&existing.ID, &existing.TaskID, &existing.IdempotencyKey, &existing.ConfigSnapshot, &existing.ExecutionInput,
			&existing.ScopePolicyVersion, &existing.Status, &existing.FinishReason, &existing.FailAtStep, &existing.FinalOutput,
			&existing.StartedAt, &existing.FinishedAt, &existing.CreatedAt, &existing.UpdatedAt)
		if err == nil {
			// Found existing - check if params match
			if !runRequestMatches(existing, req) {
				return nil, false, ErrConflict
			}
			return &existing, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("check idempotency: %w", err)
		}
	}

	// Get task to snapshot full execution input (objective, scope, config, and parameters)
	var taskName string
	var taskObjective string
	var taskScope json.RawMessage
	var taskConfig json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT name, objective, scope, config FROM tasks WHERE id = $1`, taskID).Scan(
		&taskName, &taskObjective, &taskScope, &taskConfig)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, fmt.Errorf("get task config: %w", err)
	}

	// Resolve executor configuration: defaults to "fake" if not specified
	executor := "fake"
	providerType := "mock"
	mockScenario := "normal"
	budget := model.DefaultBudgetConfig()

	// Check Task.Config
	if len(taskConfig) > 0 && string(taskConfig) != "{}" {
		var cfgMap map[string]interface{}
		if err := json.Unmarshal(taskConfig, &cfgMap); err == nil {
			if ex, ok := cfgMap["executor"].(string); ok && ex != "" {
				executor = ex
			}
			if prov, ok := cfgMap["provider"].(string); ok && prov != "" {
				providerType = prov
			}
			if scen, ok := cfgMap["mock_scenario"].(string); ok && scen != "" {
				mockScenario = scen
			}
			if bMap, ok := cfgMap["budget"].(map[string]interface{}); ok {
				if bBytes, err := json.Marshal(bMap); err == nil {
					json.Unmarshal(bBytes, &budget)
				}
			}
		}
	}

	// Request parameters override Task.Config
	if req.Executor != nil && *req.Executor != "" {
		executor = *req.Executor
	}
	if req.Provider != nil && *req.Provider != "" {
		providerType = *req.Provider
	}
	if req.MockScenario != nil && *req.MockScenario != "" {
		mockScenario = *req.MockScenario
	}
	if req.Budget != nil {
		if req.Budget.MaxModelCalls > 0 {
			budget.MaxModelCalls = req.Budget.MaxModelCalls
		}
		if req.Budget.MaxToolCalls > 0 {
			budget.MaxToolCalls = req.Budget.MaxToolCalls
		}
		if req.Budget.RunTimeoutSeconds > 0 {
			budget.RunTimeoutSeconds = req.Budget.RunTimeoutSeconds
		}
		if req.Budget.ModelTimeoutSeconds > 0 {
			budget.ModelTimeoutSeconds = req.Budget.ModelTimeoutSeconds
		}
		if req.Budget.ToolTimeoutSeconds > 0 {
			budget.ToolTimeoutSeconds = req.Budget.ToolTimeoutSeconds
		}
		if req.Budget.SummaryTimeoutSeconds > 0 {
			budget.SummaryTimeoutSeconds = req.Budget.SummaryTimeoutSeconds
		}
		if req.Budget.CleanupTimeoutSeconds > 0 {
			budget.CleanupTimeoutSeconds = req.Budget.CleanupTimeoutSeconds
		}
		if req.Budget.MaxToolOutputBytes > 0 {
			budget.MaxToolOutputBytes = req.Budget.MaxToolOutputBytes
		}
		if req.Budget.MaxPlannerRounds > 0 {
			budget.MaxPlannerRounds = req.Budget.MaxPlannerRounds
		}
		if req.Budget.MaxWorkItems > 0 {
			budget.MaxWorkItems = req.Budget.MaxWorkItems
		}
		if req.Budget.MaxParallelWorkers > 0 {
			budget.MaxParallelWorkers = req.Budget.MaxParallelWorkers
		}
	}

	var modelConfig provider.Config
	if executor == "agent" || executor == "orchestrator" || executor == "orchestration" {
		switch providerType {
		case "mock":
		case "openai", "remote":
			var configErr error
			modelConfig, configErr = provider.ConfigFromEnv().Normalized()
			if configErr != nil {
				return nil, false, configErr
			}
		default:
			return nil, false, fmt.Errorf("%w: unsupported provider", provider.ErrModelConfigInvalid)
		}
	}

	// Check if scope specifies discovery mode
	type discoveryScopeData struct {
		Mode            string                 `json:"mode"`
		Seeds           []string               `json:"seeds"`
		Endpoints       []string               `json:"endpoints"`
		DiscoveryPolicy *model.DiscoveryPolicy `json:"discovery_policy"`
	}
	var dScope discoveryScopeData
	_ = json.Unmarshal(taskScope, &dScope)

	var endpoints []string
	var seedCustomSpecs []BuiltinAssetSpec
	var isDiscovery bool
	var discoveryPolicy *model.DiscoveryPolicy

	if dScope.Mode == "discovery" {
		isDiscovery = true
		if len(dScope.Seeds) == 0 {
			return nil, false, errors.New("seeds cannot be empty in discovery mode")
		}

		discoveryPolicy = dScope.DiscoveryPolicy

		for _, s := range dScope.Seeds {
			sTrim := strings.TrimSpace(s)
			if strings.HasPrefix(sTrim, "http://") || strings.HasPrefix(sTrim, "https://") {
				u, err := url.Parse(sTrim)
				if err != nil {
					return nil, false, fmt.Errorf("invalid seed URL '%s': %w", sTrim, err)
				}
				if u.Scheme != "http" {
					return nil, false, errors.New("only http scheme is permitted for seed URL")
				}
				if u.Host == "" {
					return nil, false, errors.New("seed URL must contain host")
				}
				if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
					return nil, false, errors.New("seeds cannot contain query, fragment, or user credentials")
				}
				origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
				path := u.Path
				if path == "" {
					path = "/"
				}
				canonURL := origin + path

				if discoveryPolicy == nil {
					discoveryPolicy = &model.DiscoveryPolicy{
						Mode:              "same_origin_html",
						Origin:            origin,
						AllowedMethods:    []string{"GET"},
						MaxPages:          20,
						MaxDepth:          2,
						RequestIntervalMs: 500,
					}
				} else {
					if discoveryPolicy.Origin == "" {
						discoveryPolicy.Origin = origin
					}
					if discoveryPolicy.Origin != origin {
						return nil, false, fmt.Errorf("seed origin '%s' does not match discovery policy origin '%s'", origin, discoveryPolicy.Origin)
					}
					if discoveryPolicy.Mode == "" {
						if len(discoveryPolicy.AllowedPaths) > 0 {
							discoveryPolicy.Mode = "exact_paths"
						} else {
							discoveryPolicy.Mode = "same_origin_html"
						}
					}
					if len(discoveryPolicy.AllowedMethods) == 0 {
						discoveryPolicy.AllowedMethods = []string{"GET"}
					}
					if discoveryPolicy.MaxPages <= 0 {
						discoveryPolicy.MaxPages = 20
					}
					if discoveryPolicy.MaxDepth <= 0 {
						discoveryPolicy.MaxDepth = 2
					}
					if discoveryPolicy.RequestIntervalMs <= 0 {
						discoveryPolicy.RequestIntervalMs = 500
					}
				}

				ns := "target-range"
				if strings.Contains(origin, "127.0.0.1") {
					ns = "local-fixture-v1"
				}

				extKey := "GET " + canonURL
				name := "GET " + path
				spec := BuiltinAssetSpec{
					Namespace:   ns,
					Kind:        "http_endpoint",
					ExternalKey: extKey,
					Name:        name,
					Description: fmt.Sprintf("Seed endpoint %s", canonURL),
					Metadata: map[string]interface{}{
						"url":    canonURL,
						"method": "GET",
						"path":   path,
					},
				}
				seedCustomSpecs = append(seedCustomSpecs, spec)
			} else {
				// Relative seed e.g. "/" or "http_home"
				cKey, ok := ResolveEndpointKey(sTrim)
				if !ok {
					return nil, false, fmt.Errorf("unknown seed endpoint in scope: '%s'", sTrim)
				}
				endpoints = append(endpoints, cKey)

				fixturePort := os.Getenv("FIXTURE_PORT")
				if fixturePort == "" {
					fixturePort = "29090"
				}
				if discoveryPolicy == nil {
					discoveryPolicy = &model.DiscoveryPolicy{
						Mode:              "exact_paths",
						Origin:            fmt.Sprintf("http://127.0.0.1:%s", fixturePort),
						AllowedMethods:    []string{"GET"},
						AllowedPaths:      []string{"/", "/login", "/health"},
						MaxPages:          20,
						MaxDepth:          2,
						RequestIntervalMs: 500,
					}
				} else {
					if discoveryPolicy.Origin == "" {
						discoveryPolicy.Origin = fmt.Sprintf("http://127.0.0.1:%s", fixturePort)
					}
					if discoveryPolicy.Mode == "" {
						if len(discoveryPolicy.AllowedPaths) > 0 {
							discoveryPolicy.Mode = "exact_paths"
						} else {
							discoveryPolicy.Mode = "same_origin_html"
						}
					}
					if len(discoveryPolicy.AllowedMethods) == 0 {
						discoveryPolicy.AllowedMethods = []string{"GET"}
					}
					if discoveryPolicy.Mode != "same_origin_html" && len(discoveryPolicy.AllowedPaths) == 0 {
						discoveryPolicy.AllowedPaths = []string{"/", "/login", "/health"}
					}
				}
			}
		}
	} else {
		// Validate and parse scope before beginning transaction
		epList, err := ParseAndValidateScope(taskScope)
		if err != nil {
			return nil, false, err
		}
		endpoints = epList
	}

	executionInputMap := map[string]interface{}{
		"objective": taskObjective,
		"scope":     taskScope,
		"config":    taskConfig,
		"parameters": map[string]interface{}{
			"fail_at_step": req.FailAtStep,
		},
	}
	if isDiscovery && discoveryPolicy != nil {
		executionInputMap["discovery_policy"] = discoveryPolicy
	}
	if executor == "agent" || executor == "orchestrator" || executor == "orchestration" {
		agentConfig := map[string]interface{}{
			"executor":       executor,
			"provider":       providerType,
			"prompt_version": "v4",
			"tools_version":  "v2",
			"budget":         budget,
		}
		if providerType == "mock" {
			agentConfig["mock_scenario"] = mockScenario
		} else {
			agentConfig["model"] = modelConfig.Model
			agentConfig["endpoint"] = modelConfig.BaseURL
			agentConfig["api_format"] = modelConfig.APIFormat
		}
		executionInputMap["agent_config"] = agentConfig
		executionInputMap["budget"] = budget
	}

	executionInputBytes, err := json.Marshal(executionInputMap)
	if err != nil {
		return nil, false, fmt.Errorf("marshal execution input: %w", err)
	}

	// Create run in transaction
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var r model.Run
	err = tx.QueryRow(ctx,
		`INSERT INTO runs (task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, fail_at_step) 
		 VALUES ($1, $2, $3, $4, 'v1', 'queued', $5) 
		 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
		          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
		taskID, idempotencyKey, taskConfig, executionInputBytes, req.FailAtStep).Scan(
		&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
		&r.ScopePolicyVersion, &r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if idempotencyKey != nil && *idempotencyKey != "" {
				// Check if conflict is due to concurrent insert with same idempotency key
				var existing model.Run
				readErr := s.pool.QueryRow(ctx,
					`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
					        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
					 FROM runs WHERE task_id = $1 AND idempotency_key = $2`,
					taskID, *idempotencyKey).Scan(
					&existing.ID, &existing.TaskID, &existing.IdempotencyKey, &existing.ConfigSnapshot, &existing.ExecutionInput,
					&existing.ScopePolicyVersion, &existing.Status, &existing.FinishReason, &existing.FailAtStep, &existing.FinalOutput, &existing.StartedAt,
					&existing.FinishedAt, &existing.CreatedAt, &existing.UpdatedAt)
				if readErr == nil {
					if runRequestMatches(existing, req) {
						return &existing, true, nil
					}
					return nil, false, ErrConflict
				}
			}
			if pgErr.ConstraintName == "idx_runs_one_active_per_task" {
				return nil, false, ErrAlreadyActive
			}
			if pgErr.ConstraintName == "idx_runs_idempotency" {
				return nil, false, ErrConflict
			}
		}
		return nil, false, fmt.Errorf("insert run: %w", err)
	}

	// Insert initial event
	payload, _ := json.Marshal(map[string]interface{}{
		"from": nil,
		"to":   "queued",
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, 1, 'state_change', $2)`,
		r.ID, payload)
	if err != nil {
		return nil, false, fmt.Errorf("insert initial event: %w", err)
	}

	// Provision assets & run_assets for validated endpoints
	httpSpecs := GetHTTPEndpointSpecs("")
	var allProvisionSpecs []BuiltinAssetSpec
	allProvisionSpecs = append(allProvisionSpecs, seedCustomSpecs...)
	for _, epKey := range endpoints {
		if s, ok := BuiltinAssetSpecs[epKey]; ok {
			allProvisionSpecs = append(allProvisionSpecs, s)
		} else if s, ok := httpSpecs[epKey]; ok {
			allProvisionSpecs = append(allProvisionSpecs, s)
		}
	}

	for _, spec := range allProvisionSpecs {
		var assetID uuid.UUID
		metaJSON, _ := json.Marshal(spec.Metadata)

		err = tx.QueryRow(ctx,
			`INSERT INTO assets (id, namespace, kind, external_key, name, description, metadata, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, now(), now())
			 ON CONFLICT (namespace, kind, external_key) DO UPDATE
			 SET name = EXCLUDED.name,
			     description = EXCLUDED.description,
			     metadata = EXCLUDED.metadata,
			     updated_at = now()
			 RETURNING id`,
			spec.Namespace, spec.Kind, spec.ExternalKey, spec.Name, spec.Description, metaJSON).Scan(&assetID)
		if err != nil {
			return nil, false, fmt.Errorf("upsert asset %s: %w", spec.ExternalKey, err)
		}

		snapJSON, _ := json.Marshal(map[string]interface{}{
			"namespace":    spec.Namespace,
			"kind":         spec.Kind,
			"external_key": spec.ExternalKey,
			"name":         spec.Name,
			"description":  spec.Description,
			"metadata":     spec.Metadata,
			"is_seed":      true,
		})

		_, err = tx.Exec(ctx,
			`INSERT INTO run_assets (id, run_id, asset_id, snapshot, is_seed, depth, created_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, true, 0, now())
			 ON CONFLICT (run_id, asset_id) DO NOTHING`,
			r.ID, assetID, snapJSON)
		if err != nil {
			return nil, false, fmt.Errorf("link run_asset %s: %w", spec.ExternalKey, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit transaction: %w", err)
	}

	slog.Info("created run", "run_id", r.ID, "task_id", taskID, "task_name", taskName)
	return &r, false, nil
}

func (s *RunService) Stop(ctx context.Context, id uuid.UUID) (*model.Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var status model.RunStatus
	if err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if status.IsTerminal() {
		return nil, fmt.Errorf("%w: run is already %s", ErrTerminalState, status)
	}
	if status != model.RunStatusStopping {
		if !model.IsAllowedTransition(status, model.RunStatusStopping) {
			return nil, fmt.Errorf("%w: cannot stop run in status %s", ErrInvalidTransition, status)
		}
		if _, err = tx.Exec(ctx, `UPDATE runs SET status='stopping',updated_at=now() WHERE id=$1`, id); err != nil {
			return nil, err
		}
		var seq int
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM run_events WHERE run_id=$1`, id).Scan(&seq); err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(map[string]interface{}{"from": status, "to": model.RunStatusStopping})
		if _, err = tx.Exec(ctx, `INSERT INTO run_events(id,run_id,seq,type,payload) VALUES(gen_random_uuid(),$1,$2,'state_change',$3)`, id, seq, payload); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// TransitionState atomically updates run status and inserts a state_change event.
func (s *RunService) TransitionState(ctx context.Context, runID uuid.UUID, fromStatus, toStatus model.RunStatus, finishReason *string) (*model.Run, error) {
	if !model.IsAllowedTransition(fromStatus, toStatus) {
		return nil, fmt.Errorf("%w: transition from %s to %s is not permitted by state machine rules", ErrInvalidTransition, fromStatus, toStatus)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify current status and lock the row
	var currentStatus model.RunStatus
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get run for update: %w", err)
	}

	if currentStatus.IsTerminal() {
		return nil, fmt.Errorf("%w: run is already in terminal status %s", ErrTerminalState, currentStatus)
	}

	if currentStatus != fromStatus {
		return nil, fmt.Errorf("%w: expected status %s but got %s", ErrInvalidTransition, fromStatus, currentStatus)
	}

	if !model.IsAllowedTransition(currentStatus, toStatus) {
		return nil, fmt.Errorf("%w: transition from %s to %s is not permitted by state machine rules", ErrInvalidTransition, currentStatus, toStatus)
	}

	// Build update query
	now := time.Now()
	var r model.Run
	if toStatus.IsTerminal() {
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, finish_reason = $2, finished_at = $3, updated_at = $3 
			 WHERE id = $4 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			toStatus, finishReason, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.ScopePolicyVersion, &r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
	} else if toStatus == model.RunStatusRunning {
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, started_at = $2, updated_at = $2 
			 WHERE id = $3 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			toStatus, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.ScopePolicyVersion, &r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, updated_at = $2 
			 WHERE id = $3 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, scope_policy_version, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			toStatus, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.ScopePolicyVersion, &r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("update run status: %w", err)
	}

	// Get next seq
	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, fmt.Errorf("get next seq: %w", err)
	}

	// Insert state_change event
	payload, _ := json.Marshal(map[string]interface{}{
		"from": string(fromStatus),
		"to":   string(toStatus),
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, $2, 'state_change', $3)`,
		runID, nextSeq, payload)
	if err != nil {
		return nil, fmt.Errorf("insert state_change event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transition: %w", err)
	}

	slog.Info("run state transition", "run_id", runID, "from", fromStatus, "to", toStatus)
	return &r, nil
}

// InsertProgressEvent inserts a progress event for a run.
func (s *RunService) InsertProgressEvent(ctx context.Context, runID uuid.UUID, step, total int, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify run is still running
	var status model.RunStatus
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status)
	if err != nil {
		return fmt.Errorf("get run status: %w", err)
	}
	if status.IsTerminal() {
		slog.Warn("skipping progress event for terminal run", "run_id", runID, "status", status)
		return ErrTerminalState
	}
	if status != model.RunStatusRunning {
		slog.Warn("skipping progress event for non-running run", "run_id", runID, "status", status)
		return fmt.Errorf("%w: run is in status %s", ErrInvalidTransition, status)
	}

	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return fmt.Errorf("get next seq: %w", err)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"step":    step,
		"total":   total,
		"message": message,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, $2, 'progress', $3)`,
		runID, nextSeq, payload)
	if err != nil {
		return fmt.Errorf("insert progress event: %w", err)
	}

	return tx.Commit(ctx)
}

// InsertRunEventWithContext inserts a run event with optional session, work item, and attempt IDs.
func (s *RunService) InsertRunEventWithContext(ctx context.Context, runID uuid.UUID, sessionID, workItemID, attemptID *uuid.UUID, eventType string, payload json.RawMessage) (*model.RunEvent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	// Use the same Run-first lock order as the terminal transaction. Besides
	// serializing event sequence allocation, this prevents a cancelling session
	// from holding an event key while waiting on the finalizer's Run lock.
	if err := lockRunAdmission(ctx, tx, runID); err != nil {
		return nil, err
	}

	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, fmt.Errorf("get next seq: %w", err)
	}

	if payload == nil {
		payload = json.RawMessage(`{}`)
	}

	var ev model.RunEvent
	err = tx.QueryRow(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload, session_id, work_item_id, attempt_id)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, run_id, seq, type, payload, session_id, work_item_id, attempt_id, created_at`,
		runID, nextSeq, eventType, payload, sessionID, workItemID, attemptID).Scan(
		&ev.ID, &ev.RunID, &ev.Seq, &ev.Type, &ev.Payload, &ev.SessionID, &ev.WorkItemID, &ev.AttemptID, &ev.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert run_event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit run_event: %w", err)
	}
	return &ev, nil
}

func (s *RunService) GetEvents(ctx context.Context, runID uuid.UUID) ([]model.RunEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, seq, type, payload, session_id, work_item_id, attempt_id, created_at 
		 FROM run_events WHERE run_id = $1 ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []model.RunEvent
	for rows.Next() {
		var e model.RunEvent
		if err := rows.Scan(&e.ID, &e.RunID, &e.Seq, &e.Type, &e.Payload, &e.SessionID, &e.WorkItemID, &e.AttemptID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	if events == nil {
		events = []model.RunEvent{}
	}
	return events, nil
}

func intPtrEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func runRequestMatches(existing model.Run, req model.CreateRunRequest) bool {
	if !intPtrEqual(existing.FailAtStep, req.FailAtStep) {
		return false
	}
	if len(existing.ExecutionInput) > 0 {
		var inMap map[string]interface{}
		if err := json.Unmarshal(existing.ExecutionInput, &inMap); err == nil {
			if req.Executor != nil && *req.Executor != "" {
				if ex, _ := inMap["executor"].(string); ex != "" && ex != *req.Executor {
					return false
				}
			}
			if req.Provider != nil && *req.Provider != "" {
				if ac, ok := inMap["agent_config"].(map[string]interface{}); ok {
					if p, _ := ac["provider"].(string); p != "" && p != *req.Provider {
						return false
					}
				}
			}
			if req.MockScenario != nil && *req.MockScenario != "" {
				if ac, ok := inMap["agent_config"].(map[string]interface{}); ok {
					if sc, _ := ac["mock_scenario"].(string); sc != "" && sc != *req.MockScenario {
						return false
					}
				}
			}
		}
	}
	return true
}

// --- Phase 4: Asset Helpers & Service Methods ---

type BuiltinAssetSpec struct {
	Namespace   string
	Kind        string
	ExternalKey string
	Name        string
	Description string
	Metadata    map[string]interface{}
}

var BuiltinAssetSpecs = map[string]BuiltinAssetSpec{
	"sample_home": {
		Namespace:   "builtin-http-v1",
		Kind:        "sample_endpoint",
		ExternalKey: "sample_home",
		Name:        "Website Homepage",
		Description: "Website homepage (index)",
		Metadata: map[string]interface{}{
			"method":       "GET",
			"status_code":  200,
			"path":         "/",
			"content_type": "text/html; charset=utf-8",
		},
	},
	"sample_login": {
		Namespace:   "builtin-http-v1",
		Kind:        "sample_endpoint",
		ExternalKey: "sample_login",
		Name:        "Account Login Page",
		Description: "User login page with HTML form",
		Metadata: map[string]interface{}{
			"method":       "GET",
			"status_code":  200,
			"path":         "/login",
			"content_type": "text/html; charset=utf-8",
		},
	},
	"sample_health": {
		Namespace:   "builtin-http-v1",
		Kind:        "sample_endpoint",
		ExternalKey: "sample_health",
		Name:        "System Health Check",
		Description: "Service health check endpoint",
		Metadata: map[string]interface{}{
			"method":       "GET",
			"status_code":  200,
			"path":         "/health",
			"content_type": "application/json",
		},
	},
}

// GetFixturePort returns the configured fixture port from FIXTURE_PORT or defaults to 29090.
func GetFixturePort() string {
	if p := os.Getenv("FIXTURE_PORT"); p != "" {
		return p
	}
	return "29090"
}

// GetHTTPEndpointSpecs returns the canonical endpoint definitions for the local fixture server.
func GetHTTPEndpointSpecs(port string) map[string]BuiltinAssetSpec {
	if port == "" {
		port = GetFixturePort()
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%s", port)
	return map[string]BuiltinAssetSpec{
		"http_home": {
			Namespace:   "local-fixture-v1",
			Kind:        "http_endpoint",
			ExternalKey: fmt.Sprintf("GET %s/", baseURL),
			Name:        "Local Fixture Home",
			Description: "Root landing page of local fixture application",
			Metadata: map[string]interface{}{
				"method":       "GET",
				"url":          fmt.Sprintf("%s/", baseURL),
				"path":         "/",
				"port":         port,
				"content_type": "text/html; charset=utf-8",
			},
		},
		"http_login": {
			Namespace:   "local-fixture-v1",
			Kind:        "http_endpoint",
			ExternalKey: fmt.Sprintf("GET %s/login", baseURL),
			Name:        "Local Fixture Login",
			Description: "Login page of local fixture application",
			Metadata: map[string]interface{}{
				"method":       "GET",
				"url":          fmt.Sprintf("%s/login", baseURL),
				"path":         "/login",
				"port":         port,
				"content_type": "text/html; charset=utf-8",
			},
		},
		"http_health": {
			Namespace:   "local-fixture-v1",
			Kind:        "http_endpoint",
			ExternalKey: fmt.Sprintf("GET %s/health", baseURL),
			Name:        "Local Fixture Health Check",
			Description: "Health check endpoint of local fixture application",
			Metadata: map[string]interface{}{
				"method":       "GET",
				"url":          fmt.Sprintf("%s/health", baseURL),
				"path":         "/health",
				"port":         port,
				"content_type": "application/json",
			},
		},
	}
}

// ResolveEndpointKey resolves a candidate string to a canonical asset spec key.
// It matches builtin samples, HTTP canonical keys, aliases, and full URLs.
func ResolveEndpointKey(candidate string) (string, bool) {
	candidate = strings.TrimSpace(candidate)
	if _, ok := BuiltinAssetSpecs[candidate]; ok {
		return candidate, true
	}

	switch candidate {
	case "http_home", "http_root", "/", "home":
		return "http_home", true
	case "http_login", "/login", "login":
		return "http_login", true
	case "http_health", "/health", "health":
		return "http_health", true
	}

	// Also handle full URLs on 127.0.0.1
	if strings.HasPrefix(candidate, "http://") {
		u, err := url.Parse(candidate)
		if err == nil && u.Hostname() == "127.0.0.1" {
			switch u.Path {
			case "", "/":
				return "http_home", true
			case "/login":
				return "http_login", true
			case "/health":
				return "http_health", true
			}
		}
	}

	return "", false
}

// ParseAndValidateScope extracts and strictly validates endpoint keys from task scope.
// It accepts:
// 1. {"endpoints": ["..."]}
// 2. {"http_endpoints": ["..."]}
// 3. {"allowed_endpoints": ["..."]}
// 4. {"samples": ["..."]}
// 5. ["..."]
// If scope is empty, {} or null, returns empty slice and nil error (valid empty scope).
// If scope contains unrecognized endpoint keys, returns an explicit validation error.
func ParseAndValidateScope(scopeBytes []byte) ([]string, error) {
	if len(scopeBytes) == 0 || string(scopeBytes) == "{}" || string(scopeBytes) == "null" {
		return nil, nil
	}

	var candidates []string

	// Try array directly
	var strSlice []string
	if err := json.Unmarshal(scopeBytes, &strSlice); err == nil {
		candidates = strSlice
	} else {
		// Try map
		var m map[string]interface{}
		if err := json.Unmarshal(scopeBytes, &m); err != nil {
			return nil, fmt.Errorf("invalid scope JSON: %w", err)
		}
		for _, key := range []string{"endpoints", "http_endpoints", "allowed_endpoints", "samples"} {
			if val, ok := m[key]; ok {
				if list, ok := val.([]interface{}); ok {
					for _, it := range list {
						if s, ok := it.(string); ok {
							candidates = append(candidates, s)
						}
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	var normalizedKeys []string
	seen := make(map[string]bool)

	for _, c := range candidates {
		c = strings.TrimSpace(c)
		canonicalKey, ok := ResolveEndpointKey(c)
		if !ok {
			return nil, fmt.Errorf("unknown endpoint in scope: '%s'", c)
		}
		if !seen[canonicalKey] {
			seen[canonicalKey] = true
			normalizedKeys = append(normalizedKeys, canonicalKey)
		}
	}

	return normalizedKeys, nil
}

// ParseScopeEndpoints is a backward-compatible wrapper that returns the validated endpoint keys.
func ParseScopeEndpoints(scopeBytes []byte) []string {
	keys, err := ParseAndValidateScope(scopeBytes)
	if err != nil {
		return nil
	}
	return keys
}

// IsSampleInRunScope checks if a given sample ID is within the assets of a Run.
// In v1, empty run_assets means NO samples are permitted (empty scope does not allow all).
func (s *RunService) IsSampleInRunScope(ctx context.Context, runID uuid.UUID, sampleID string) (bool, error) {
	var scopePolicyVersion string
	err := s.pool.QueryRow(ctx, `SELECT scope_policy_version FROM runs WHERE id = $1`, runID).Scan(&scopePolicyVersion)
	if err != nil {
		return false, fmt.Errorf("query run scope policy: %w", err)
	}

	var count int
	err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_assets WHERE run_id = $1`, runID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query run_assets count: %w", err)
	}
	if count == 0 {
		if scopePolicyVersion == "legacy" {
			// Historical run without asset restrictions
			return true, nil
		}
		// In v1, empty scope means NO samples allowed!
		return false, nil
	}

	var exists bool
	err = s.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM run_assets ra
			JOIN assets a ON ra.asset_id = a.id
			WHERE ra.run_id = $1 AND a.kind = 'sample_endpoint' AND a.external_key = $2
		)`, runID, sampleID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check sample in run scope: %w", err)
	}
	return exists, nil
}

// ListAssets returns all assets with their associated run count and observation count.
func (s *RunService) ListAssets(ctx context.Context) ([]model.AssetSummary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.namespace, a.kind, a.external_key, a.name, a.description, a.metadata, a.created_at, a.updated_at,
		        COUNT(DISTINCT ra.run_id) AS run_count,
		        COUNT(DISTINCT o.id) AS observation_count,
		        COUNT(DISTINCT wia.work_item_id) AS work_item_count
		 FROM assets a
		 LEFT JOIN run_assets ra ON a.id = ra.asset_id
		 LEFT JOIN observations o ON a.id = o.asset_id
		 LEFT JOIN work_item_assets wia ON a.id = wia.asset_id
		 GROUP BY a.id
		 ORDER BY a.external_key ASC`)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()

	var summaries []model.AssetSummary
	for rows.Next() {
		var sm model.AssetSummary
		if err := rows.Scan(
			&sm.ID, &sm.Namespace, &sm.Kind, &sm.ExternalKey, &sm.Name, &sm.Description, &sm.Metadata, &sm.CreatedAt, &sm.UpdatedAt,
			&sm.RunCount, &sm.ObservationCount, &sm.WorkItemCount); err != nil {
			return nil, fmt.Errorf("scan asset summary: %w", err)
		}
		summaries = append(summaries, sm)
	}
	if summaries == nil {
		summaries = []model.AssetSummary{}
	}
	return summaries, nil
}

// GetAsset retrieves a single asset by ID.
func (s *RunService) GetAsset(ctx context.Context, assetID uuid.UUID) (*model.Asset, error) {
	var a model.Asset
	err := s.pool.QueryRow(ctx,
		`SELECT id, namespace, kind, external_key, name, description, metadata, created_at, updated_at
		 FROM assets WHERE id = $1`, assetID).Scan(
		&a.ID, &a.Namespace, &a.Kind, &a.ExternalKey, &a.Name, &a.Description, &a.Metadata, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get asset: %w", err)
	}
	return &a, nil
}

// ListAssetsByRun retrieves all assets associated with a Run.
func (s *RunService) ListAssetsByRun(ctx context.Context, runID uuid.UUID) ([]model.Asset, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.namespace, a.kind, a.external_key, a.name, a.description, a.metadata, a.created_at, a.updated_at
		 FROM assets a
		 JOIN run_assets ra ON a.id = ra.asset_id
		 WHERE ra.run_id = $1
		 ORDER BY a.external_key ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list assets by run: %w", err)
	}
	defer rows.Close()

	var assets []model.Asset
	for rows.Next() {
		var a model.Asset
		if err := rows.Scan(
			&a.ID, &a.Namespace, &a.Kind, &a.ExternalKey, &a.Name, &a.Description, &a.Metadata, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		assets = append(assets, a)
	}
	if assets == nil {
		assets = []model.Asset{}
	}
	return assets, nil
}

// ListRunAssets retrieves all run_asset linkages with snapshots.
func (s *RunService) ListRunAssets(ctx context.Context, runID uuid.UUID) ([]model.RunAsset, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, asset_id, snapshot, created_at
		 FROM run_assets WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list run_assets: %w", err)
	}
	defer rows.Close()

	var runAssets []model.RunAsset
	for rows.Next() {
		var ra model.RunAsset
		if err := rows.Scan(&ra.ID, &ra.RunID, &ra.AssetID, &ra.Snapshot, &ra.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan run_asset: %w", err)
		}
		runAssets = append(runAssets, ra)
	}
	if runAssets == nil {
		runAssets = []model.RunAsset{}
	}
	return runAssets, nil
}

// GetAssetDetailsWithHistory retrieves the asset and its history grouped by Run.
func (s *RunService) GetAssetDetailsWithHistory(ctx context.Context, assetID uuid.UUID) (*model.AssetDetailsResponse, error) {
	asset, err := s.GetAsset(ctx, assetID)
	if err != nil {
		return nil, err
	}

	// Find distinct runs associated with this asset via run_assets OR observations OR work_item_assets
	runRows, err := s.pool.Query(ctx,
		`SELECT DISTINCT r.id, r.task_id, t.name, r.status, r.finish_reason, r.started_at, r.finished_at, r.created_at
		 FROM runs r
		 JOIN tasks t ON r.task_id = t.id
		 LEFT JOIN run_assets ra ON r.id = ra.run_id AND ra.asset_id = $1
		 LEFT JOIN observations o ON r.id = o.run_id AND o.asset_id = $1
		 LEFT JOIN work_items wi ON r.id = wi.run_id
		 LEFT JOIN work_item_assets wia ON wi.id = wia.work_item_id AND wia.asset_id = $1
		 WHERE ra.asset_id IS NOT NULL OR o.asset_id IS NOT NULL OR wia.asset_id IS NOT NULL
		 ORDER BY r.created_at DESC`, assetID)
	if err != nil {
		return nil, fmt.Errorf("query asset runs: %w", err)
	}
	defer runRows.Close()

	type runMeta struct {
		id           uuid.UUID
		taskID       uuid.UUID
		taskName     string
		status       model.RunStatus
		finishReason *string
		startedAt    *time.Time
		finishedAt   *time.Time
	}
	var runs []runMeta
	for runRows.Next() {
		var rm runMeta
		var createdAt time.Time
		if err := runRows.Scan(&rm.id, &rm.taskID, &rm.taskName, &rm.status, &rm.finishReason, &rm.startedAt, &rm.finishedAt, &createdAt); err != nil {
			return nil, fmt.Errorf("scan run meta: %w", err)
		}
		runs = append(runs, rm)
	}
	runRows.Close()

	var history []model.AssetRunHistory
	for _, rm := range runs {
		// Work items for this asset in this run
		wiRows, err := s.pool.Query(ctx,
			`SELECT wi.id, wi.run_id, wi.key, wi.title, wi.objective, wi.work_type, wi.status,
			        wi.blocked_reason, wi.input_data, wi.output_data, wi.current_attempt_id,
			        wi.created_by_planner_round_id, wi.created_at, wi.updated_at,
			        pr.round
			 FROM work_items wi
			 JOIN work_item_assets wia ON wi.id = wia.work_item_id
			 LEFT JOIN planner_rounds pr ON wi.created_by_planner_round_id = pr.id
			 WHERE wi.run_id = $1 AND wia.asset_id = $2
			 ORDER BY wi.created_at ASC`, rm.id, assetID)
		if err != nil {
			return nil, fmt.Errorf("query work items for asset: %w", err)
		}

		var workItems []model.WorkItemDetail
		for wiRows.Next() {
			var wid model.WorkItemDetail
			var roundNum *int
			if err := wiRows.Scan(
				&wid.ID, &wid.RunID, &wid.Key, &wid.Title, &wid.Objective, &wid.WorkType, &wid.Status,
				&wid.BlockedReason, &wid.InputData, &wid.OutputData, &wid.CurrentAttemptID,
				&wid.CreatedByPlannerRoundID, &wid.CreatedAt, &wid.UpdatedAt, &roundNum); err != nil {
				wiRows.Close()
				return nil, fmt.Errorf("scan work item detail: %w", err)
			}
			wid.CreatedRoundNum = roundNum
			workItems = append(workItems, wid)
		}
		wiRows.Close()

		// For each work item, populate TargetAssets, SourceObservations, and Dependencies
		for i := range workItems {
			// TargetAssets
			taRows, err := s.pool.Query(ctx,
				`SELECT a.id, a.namespace, a.kind, a.external_key, a.name, a.description, a.metadata, a.created_at, a.updated_at
				 FROM assets a
				 JOIN work_item_assets wia ON a.id = wia.asset_id
				 WHERE wia.work_item_id = $1`, workItems[i].ID)
			if err == nil {
				for taRows.Next() {
					var a model.Asset
					if err := taRows.Scan(&a.ID, &a.Namespace, &a.Kind, &a.ExternalKey, &a.Name, &a.Description, &a.Metadata, &a.CreatedAt, &a.UpdatedAt); err == nil {
						workItems[i].TargetAssets = append(workItems[i].TargetAssets, a)
					}
				}
				taRows.Close()
			}
			if workItems[i].TargetAssets == nil {
				workItems[i].TargetAssets = []model.Asset{}
			}

			// SourceObservations
			soRows, err := s.pool.Query(ctx,
				`SELECT o.id, o.run_id, o.attempt_id, o.asset_id, o.obs_type, o.content, o.data, o.source_tool_call_id, o.created_at
				 FROM observations o
				 JOIN work_item_sources wis ON o.id = wis.source_observation_id
				 WHERE wis.work_item_id = $1`, workItems[i].ID)
			if err == nil {
				for soRows.Next() {
					var o model.Observation
					if err := soRows.Scan(&o.ID, &o.RunID, &o.AttemptID, &o.AssetID, &o.ObsType, &o.Content, &o.Data, &o.SourceToolCallID, &o.CreatedAt); err == nil {
						workItems[i].SourceObservations = append(workItems[i].SourceObservations, o)
					}
				}
				soRows.Close()
			}
			if workItems[i].SourceObservations == nil {
				workItems[i].SourceObservations = []model.Observation{}
			}

			// Dependencies
			depRows, err := s.pool.Query(ctx,
				`SELECT dep.id, dep.run_id, dep.key, dep.title, dep.objective, dep.work_type, dep.status,
				        dep.blocked_reason, dep.input_data, dep.output_data, dep.current_attempt_id,
				        dep.created_by_planner_round_id, dep.created_at, dep.updated_at
				 FROM work_items dep
				 JOIN work_dependencies wd ON dep.id = wd.depends_on_work_item_id
				 WHERE wd.work_item_id = $1`, workItems[i].ID)
			if err == nil {
				for depRows.Next() {
					var dep model.WorkItem
					if err := depRows.Scan(&dep.ID, &dep.RunID, &dep.Key, &dep.Title, &dep.Objective, &dep.WorkType, &dep.Status,
						&dep.BlockedReason, &dep.InputData, &dep.OutputData, &dep.CurrentAttemptID,
						&dep.CreatedByPlannerRoundID, &dep.CreatedAt, &dep.UpdatedAt); err == nil {
						workItems[i].Dependencies = append(workItems[i].Dependencies, dep)
					}
				}
				depRows.Close()
			}
			if workItems[i].Dependencies == nil {
				workItems[i].Dependencies = []model.WorkItem{}
			}
		}

		if workItems == nil {
			workItems = []model.WorkItemDetail{}
		}

		// Observations for this asset in this run
		obsRows, err := s.pool.Query(ctx,
			`SELECT o.id, o.run_id, o.attempt_id, o.asset_id, o.obs_type, o.content, o.data, o.source_tool_call_id, o.created_at,
			        tc.id, tc.run_id, tc.session_id, tc.model_call_id, tc.call_id, tc.tool_name, tc.arguments, tc.output, tc.status, tc.error_message, tc.started_at, tc.finished_at, tc.created_at
			 FROM observations o
			 LEFT JOIN agent_tool_calls tc ON o.source_tool_call_id = tc.id
			 WHERE o.run_id = $1 AND o.asset_id = $2
			 ORDER BY o.created_at ASC`, rm.id, assetID)
		if err != nil {
			return nil, fmt.Errorf("query observations for asset: %w", err)
		}

		var observations []model.ObservationDetail
		for obsRows.Next() {
			var od model.ObservationDetail
			var tcID, tcRunID *uuid.UUID
			var tcSessionID, tcModelCallID *uuid.UUID
			var tcCallID, tcToolName, tcOutput, tcStatus *string
			var tcArgs []byte
			var tcErrMsg *string
			var tcStartedAt, tcFinishedAt, tcCreatedAt *time.Time

			err := obsRows.Scan(
				&od.ID, &od.RunID, &od.AttemptID, &od.AssetID, &od.ObsType, &od.Content, &od.Data, &od.SourceToolCallID, &od.CreatedAt,
				&tcID, &tcRunID, &tcSessionID, &tcModelCallID, &tcCallID, &tcToolName, &tcArgs, &tcOutput, &tcStatus, &tcErrMsg, &tcStartedAt, &tcFinishedAt, &tcCreatedAt)
			if err != nil {
				obsRows.Close()
				return nil, fmt.Errorf("scan observation detail: %w", err)
			}
			if tcID != nil {
				od.ToolCall = &model.AgentToolCall{
					ID:           *tcID,
					RunID:        *tcRunID,
					SessionID:    tcSessionID,
					ModelCallID:  tcModelCallID,
					CallID:       *tcCallID,
					ToolName:     *tcToolName,
					Arguments:    tcArgs,
					Output:       *tcOutput,
					Status:       *tcStatus,
					ErrorMessage: tcErrMsg,
					StartedAt:    tcStartedAt,
					FinishedAt:   tcFinishedAt,
					CreatedAt:    *tcCreatedAt,
				}
			}
			observations = append(observations, od)
		}
		obsRows.Close()

		if observations == nil {
			observations = []model.ObservationDetail{}
		}

		var discoveredBy *model.AssetDiscoveryDetail
		var disc model.AssetDiscoveryDetail
		err = s.pool.QueryRow(ctx,
			`SELECT ad.id, ad.run_id, ad.source_asset_id, ad.source_tool_call_id, ad.target_asset_id,
			        ad.raw_href, ad.discovered_at,
			        sa.name AS source_asset_name,
			        ta.name AS target_asset_name,
			        ta.external_key AS target_asset_key
			 FROM asset_discoveries ad
			 JOIN assets sa ON ad.source_asset_id = sa.id
			 JOIN assets ta ON ad.target_asset_id = ta.id
			 WHERE ad.run_id = $1 AND ad.target_asset_id = $2
			 LIMIT 1`, rm.id, assetID).Scan(
			&disc.ID, &disc.RunID, &disc.SourceAssetID, &disc.SourceToolCallID, &disc.TargetAssetID,
			&disc.RawHref, &disc.DiscoveredAt,
			&disc.SourceAssetName, &disc.TargetAssetName, &disc.TargetAssetKey)
		if err == nil {
			discoveredBy = &disc
		}

		history = append(history, model.AssetRunHistory{
			RunID:        rm.id,
			TaskID:       rm.taskID,
			TaskName:     rm.taskName,
			RunStatus:    rm.status,
			FinishReason: rm.finishReason,
			StartedAt:    rm.startedAt,
			FinishedAt:   rm.finishedAt,
			DiscoveredBy: discoveredBy,
			WorkItems:    workItems,
			Observations: observations,
		})
	}

	if history == nil {
		history = []model.AssetRunHistory{}
	}

	return &model.AssetDetailsResponse{
		Asset:      *asset,
		RunHistory: history,
	}, nil
}

// GetAgentToolCallBySessionAndCallID retrieves a tool call within a specific session by its call_id.
func (s *RunService) GetAgentToolCallBySessionAndCallID(ctx context.Context, sessionID uuid.UUID, callID string) (*model.AgentToolCall, error) {
	var tc model.AgentToolCall
	err := s.pool.QueryRow(ctx,
		`SELECT id, run_id, session_id, model_call_id, call_id, tool_name, arguments, output, status, error_message, started_at, finished_at, created_at
		 FROM agent_tool_calls
		 WHERE session_id = $1 AND call_id = $2
		 ORDER BY created_at DESC LIMIT 1`, sessionID, callID).Scan(
		&tc.ID, &tc.RunID, &tc.SessionID, &tc.ModelCallID, &tc.CallID, &tc.ToolName,
		&tc.Arguments, &tc.Output, &tc.Status, &tc.ErrorMessage, &tc.StartedAt, &tc.FinishedAt, &tc.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get tool call by session and call_id: %w", err)
	}
	return &tc, nil
}

// ListWorkItemDetailsByRun retrieves all work items for a run with enriched metadata (target assets, sources, dependencies).
func (s *RunService) ListWorkItemDetailsByRun(ctx context.Context, runID uuid.UUID) ([]model.WorkItemDetail, error) {
	wiRows, err := s.pool.Query(ctx,
		`SELECT wi.id, wi.run_id, wi.key, wi.title, wi.objective, wi.work_type, wi.status,
		        wi.blocked_reason, wi.input_data, wi.output_data, wi.current_attempt_id,
		        wi.created_by_planner_round_id, wi.created_at, wi.updated_at,
		        pr.round
		 FROM work_items wi
		 LEFT JOIN planner_rounds pr ON wi.created_by_planner_round_id = pr.id
		 WHERE wi.run_id = $1
		 ORDER BY wi.created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	defer wiRows.Close()

	var items []model.WorkItemDetail
	for wiRows.Next() {
		var wid model.WorkItemDetail
		var roundNum *int
		if err := wiRows.Scan(
			&wid.ID, &wid.RunID, &wid.Key, &wid.Title, &wid.Objective, &wid.WorkType, &wid.Status,
			&wid.BlockedReason, &wid.InputData, &wid.OutputData, &wid.CurrentAttemptID,
			&wid.CreatedByPlannerRoundID, &wid.CreatedAt, &wid.UpdatedAt, &roundNum); err != nil {
			return nil, fmt.Errorf("scan work item: %w", err)
		}
		wid.CreatedRoundNum = roundNum
		items = append(items, wid)
	}
	wiRows.Close()

	for i := range items {
		// TargetAssets
		taRows, err := s.pool.Query(ctx,
			`SELECT a.id, a.namespace, a.kind, a.external_key, a.name, a.description, a.metadata, a.created_at, a.updated_at
			 FROM assets a
			 JOIN work_item_assets wia ON a.id = wia.asset_id
			 WHERE wia.work_item_id = $1`, items[i].ID)
		if err == nil {
			for taRows.Next() {
				var a model.Asset
				if err := taRows.Scan(&a.ID, &a.Namespace, &a.Kind, &a.ExternalKey, &a.Name, &a.Description, &a.Metadata, &a.CreatedAt, &a.UpdatedAt); err == nil {
					items[i].TargetAssets = append(items[i].TargetAssets, a)
				}
			}
			taRows.Close()
		}
		if items[i].TargetAssets == nil {
			items[i].TargetAssets = []model.Asset{}
		}

		// SourceObservations
		soRows, err := s.pool.Query(ctx,
			`SELECT o.id, o.run_id, o.attempt_id, o.asset_id, o.obs_type, o.content, o.data, o.source_tool_call_id, o.created_at
			 FROM observations o
			 JOIN work_item_sources wis ON o.id = wis.source_observation_id
			 WHERE wis.work_item_id = $1`, items[i].ID)
		if err == nil {
			for soRows.Next() {
				var o model.Observation
				if err := soRows.Scan(&o.ID, &o.RunID, &o.AttemptID, &o.AssetID, &o.ObsType, &o.Content, &o.Data, &o.SourceToolCallID, &o.CreatedAt); err == nil {
					items[i].SourceObservations = append(items[i].SourceObservations, o)
				}
			}
			soRows.Close()
		}
		if items[i].SourceObservations == nil {
			items[i].SourceObservations = []model.Observation{}
		}

		// Dependencies
		depRows, err := s.pool.Query(ctx,
			`SELECT dep.id, dep.run_id, dep.key, dep.title, dep.objective, dep.work_type, dep.status,
			        dep.blocked_reason, dep.input_data, dep.output_data, dep.current_attempt_id,
			        dep.created_by_planner_round_id, dep.created_at, dep.updated_at
			 FROM work_items dep
			 JOIN work_dependencies wd ON dep.id = wd.depends_on_work_item_id
			 WHERE wd.work_item_id = $1`, items[i].ID)
		if err == nil {
			for depRows.Next() {
				var dep model.WorkItem
				if err := depRows.Scan(&dep.ID, &dep.RunID, &dep.Key, &dep.Title, &dep.Objective, &dep.WorkType, &dep.Status,
					&dep.BlockedReason, &dep.InputData, &dep.OutputData, &dep.CurrentAttemptID,
					&dep.CreatedByPlannerRoundID, &dep.CreatedAt, &dep.UpdatedAt); err == nil {
					items[i].Dependencies = append(items[i].Dependencies, dep)
				}
			}
			depRows.Close()
		}
		if items[i].Dependencies == nil {
			items[i].Dependencies = []model.WorkItem{}
		}
	}

	if items == nil {
		items = []model.WorkItemDetail{}
	}
	return items, nil
}

// ListDiscoveriesByRun retrieves all asset discoveries made in a run.
func (s *RunService) ListDiscoveriesByRun(ctx context.Context, runID uuid.UUID) ([]model.AssetDiscoveryDetail, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ad.id, ad.run_id, ad.source_asset_id, ad.source_tool_call_id, ad.target_asset_id,
		        ad.raw_href, ad.discovered_at,
		        sa.name AS source_asset_name,
		        sa.external_key AS source_asset_key,
		        ta.name AS target_asset_name,
		        ta.external_key AS target_asset_key
		 FROM asset_discoveries ad
		 JOIN assets sa ON ad.source_asset_id = sa.id
		 JOIN assets ta ON ad.target_asset_id = ta.id
		 WHERE ad.run_id = $1
		 ORDER BY ad.discovered_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list discoveries: %w", err)
	}
	defer rows.Close()

	var list []model.AssetDiscoveryDetail
	for rows.Next() {
		var d model.AssetDiscoveryDetail
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.SourceAssetID, &d.SourceToolCallID, &d.TargetAssetID,
			&d.RawHref, &d.DiscoveredAt,
			&d.SourceAssetName, &d.SourceAssetKey, &d.TargetAssetName, &d.TargetAssetKey); err != nil {
			return nil, fmt.Errorf("scan discovery: %w", err)
		}
		list = append(list, d)
	}
	if list == nil {
		list = []model.AssetDiscoveryDetail{}
	}
	return list, nil
}
