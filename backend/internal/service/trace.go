package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListWorkerTraces retrieves an overview of all executed or running work items for a run.
func (s *RunService) ListWorkerTraces(ctx context.Context, runID uuid.UUID, q string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	query := `
		SELECT wi.id, wi.key, wi.title, wi.objective, wi.work_type, wi.status,
		       en.id as intent_id,
		       count(tc.id) as step_count
		FROM work_items wi
		LEFT JOIN exploration_nodes en ON en.work_item_id = wi.id
		LEFT JOIN agent_sessions sess ON sess.work_item_id = wi.id
		LEFT JOIN agent_tool_calls tc ON tc.session_id = sess.id
		WHERE wi.run_id = $1 AND wi.status IN ('running', 'completed', 'exhausted', 'failed', 'blocked')
	`
	args := []any{runID}
	if strings.TrimSpace(q) != "" {
		args = append(args, "%"+strings.TrimSpace(q)+"%")
		query += fmt.Sprintf(" AND (wi.title ILIKE $%d OR wi.objective ILIKE $%d OR wi.key ILIKE $%d)", len(args), len(args), len(args))
	}
	query += fmt.Sprintf(" GROUP BY wi.id, wi.key, wi.title, wi.objective, wi.work_type, wi.status, en.id ORDER BY wi.created_at ASC LIMIT $%d", len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query worker traces: %w", err)
	}
	defer rows.Close()

	var traces []map[string]any
	for rows.Next() {
		var wiID uuid.UUID
		var key, title, objective, workType, status string
		var intentID *uuid.UUID
		var stepCount int
		if err := rows.Scan(&wiID, &key, &title, &objective, &workType, &status, &intentID, &stepCount); err != nil {
			return nil, fmt.Errorf("scan worker trace: %w", err)
		}
		item := map[string]any{
			"work_item_id": wiID,
			"key":          key,
			"title":        title,
			"objective":    objective,
			"work_type":    workType,
			"status":       status,
			"step_count":   stepCount,
		}
		if intentID != nil {
			item["intent_id"] = *intentID
		}
		traces = append(traces, item)
	}
	if traces == nil {
		traces = []map[string]any{}
	}
	return traces, nil
}

// SearchAllWorkerTraces keyword-searches tool outputs and arguments across all workers in a run.
func (s *RunService) SearchAllWorkerTraces(
	ctx context.Context,
	runID uuid.UUID,
	excludeWorkItemID *uuid.UUID,
	q string,
	limit int,
) ([]map[string]any, error) {
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("search query q cannot be empty")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	query := `
		SELECT tc.id, tc.tool_name, tc.output, tc.status, tc.created_at,
		       wi.id as work_item_id, wi.key as work_item_key,
		       en.id as intent_id
		FROM agent_tool_calls tc
		JOIN agent_sessions sess ON tc.session_id = sess.id
		JOIN work_items wi ON sess.work_item_id = wi.id
		LEFT JOIN exploration_nodes en ON en.work_item_id = wi.id
		WHERE tc.run_id = $1
		  AND (tc.output ILIKE $2 OR tc.tool_name ILIKE $2 OR tc.arguments::text ILIKE $2)
	`
	pattern := "%" + strings.TrimSpace(q) + "%"
	args := []any{runID, pattern}

	if excludeWorkItemID != nil && *excludeWorkItemID != uuid.Nil {
		args = append(args, *excludeWorkItemID)
		query += fmt.Sprintf(" AND wi.id != $%d", len(args))
	}

	query += fmt.Sprintf(" ORDER BY tc.created_at DESC LIMIT $%d", len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search all worker traces: %w", err)
	}
	defer rows.Close()

	var hits []map[string]any
	for rows.Next() {
		var tcID uuid.UUID
		var toolName, output, status string
		var ca any
		var wiID uuid.UUID
		var wiKey string
		var intentID *uuid.UUID

		if err := rows.Scan(&tcID, &toolName, &output, &status, &ca, &wiID, &wiKey, &intentID); err != nil {
			return nil, fmt.Errorf("scan trace hit: %w", err)
		}

		// Truncate output summary to 100 chars
		summary := strings.TrimSpace(output)
		if len(summary) > 100 {
			summary = summary[:100] + "..."
		}

		hit := map[string]any{
			"step_id":       tcID,
			"tool_name":     toolName,
			"status":        status,
			"summary":       summary,
			"work_item_id":  wiID,
			"work_item_key": wiKey,
		}
		if intentID != nil {
			hit["intent_id"] = *intentID
		}
		hits = append(hits, hit)
	}
	if hits == nil {
		hits = []map[string]any{}
	}
	return hits, nil
}

// GetWorkerTrace gets the step summary stream or full step details for a specific work item.
func (s *RunService) GetWorkerTrace(
	ctx context.Context,
	runID uuid.UUID,
	targetWorkOrIntentID uuid.UUID,
	q string,
	stepIDs []uuid.UUID,
	limit int,
) (map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	// 1. Resolve work_item_id (caller might have passed intent exploration node ID or work_item_id)
	var resolvedWorkItemID uuid.UUID
	var wiRunID uuid.UUID
	var wiKey, wiTitle, wiStatus string

	err := s.pool.QueryRow(ctx,
		`SELECT id, run_id, key, title, status FROM work_items WHERE id = $1`,
		targetWorkOrIntentID,
	).Scan(&resolvedWorkItemID, &wiRunID, &wiKey, &wiTitle, &wiStatus)

	if errors.Is(err, pgx.ErrNoRows) {
		// Check exploration_nodes by intent ID
		var expRunID uuid.UUID
		var wiID *uuid.UUID
		err2 := s.pool.QueryRow(ctx,
			`SELECT run_id, work_item_id FROM exploration_nodes WHERE id = $1`,
			targetWorkOrIntentID,
		).Scan(&expRunID, &wiID)
		if err2 != nil {
			if errors.Is(err2, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: work item or intent %s not found", ErrNodeNotFound, targetWorkOrIntentID)
			}
			return nil, fmt.Errorf("query exploration node: %w", err2)
		}
		if expRunID != runID {
			return nil, fmt.Errorf("%w: requested trace belongs to run %s (current %s)", ErrCrossRunBoundary, expRunID, runID)
		}
		if wiID == nil {
			return nil, fmt.Errorf("exploration node has no associated execution work item")
		}
		resolvedWorkItemID = *wiID
		_ = s.pool.QueryRow(ctx, `SELECT run_id, key, title, status FROM work_items WHERE id = $1`, resolvedWorkItemID).Scan(&wiRunID, &wiKey, &wiTitle, &wiStatus)
	} else if err != nil {
		return nil, fmt.Errorf("query work item: %w", err)
	}

	// Cross-run isolation check
	if wiRunID != runID {
		return nil, fmt.Errorf("%w: work item %s belongs to run %s (current %s)", ErrCrossRunBoundary, resolvedWorkItemID, wiRunID, runID)
	}

	// Mode 3: Drill down into specific step_ids (up to 5 steps max per call, like ARTEX)
	if len(stepIDs) > 0 {
		const maxSteps = 5
		var selectedIDs []uuid.UUID
		var omittedIDs []uuid.UUID
		seen := make(map[uuid.UUID]bool)

		for _, sid := range stepIDs {
			if sid != uuid.Nil && !seen[sid] {
				seen[sid] = true
				if len(selectedIDs) < maxSteps {
					selectedIDs = append(selectedIDs, sid)
				} else {
					omittedIDs = append(omittedIDs, sid)
				}
			}
		}

		rows, err := s.pool.Query(ctx,
			`SELECT tc.id, tc.tool_name, tc.arguments, tc.output, tc.status, tc.error_message, tc.created_at
			 FROM agent_tool_calls tc
			 JOIN agent_sessions sess ON tc.session_id = sess.id
			 WHERE sess.work_item_id = $1 AND tc.run_id = $2 AND tc.id = ANY($3)
			 ORDER BY tc.created_at ASC`,
			resolvedWorkItemID, runID, selectedIDs,
		)
		if err != nil {
			return nil, fmt.Errorf("query trace steps by id: %w", err)
		}
		defer rows.Close()

		var steps []map[string]any
		for rows.Next() {
			var id uuid.UUID
			var toolName, output, status string
			var args []byte
			var errMsg *string
			var ca any
			if err := rows.Scan(&id, &toolName, &args, &output, &status, &errMsg, &ca); err != nil {
				return nil, err
			}
			steps = append(steps, map[string]any{
				"step_id":       id,
				"tool_name":     toolName,
				"arguments":     string(args),
				"output":        output,
				"status":        status,
				"error_message": errMsg,
			})
		}

		res := map[string]any{
			"work_item_id":      resolvedWorkItemID,
			"work_item_key":     wiKey,
			"returned_step_ids": selectedIDs,
			"steps":             steps,
		}
		if len(omittedIDs) > 0 {
			res["omitted_step_ids"] = omittedIDs
			res["notice"] = fmt.Sprintf("Maximum %d steps returned per call. %d steps omitted: %v", maxSteps, len(omittedIDs), omittedIDs)
		}
		return res, nil
	}

	// Mode 1 & 2: Summary stream of steps (optionally filtered by keyword q)
	query := `
		SELECT tc.id, tc.tool_name, tc.output, tc.status, tc.created_at
		FROM agent_tool_calls tc
		JOIN agent_sessions sess ON tc.session_id = sess.id
		WHERE sess.work_item_id = $1 AND tc.run_id = $2
	`
	qArgs := []any{resolvedWorkItemID, runID}
	if strings.TrimSpace(q) != "" {
		qArgs = append(qArgs, "%"+strings.TrimSpace(q)+"%")
		query += fmt.Sprintf(" AND (tc.output ILIKE $%d OR tc.tool_name ILIKE $%d OR tc.arguments::text ILIKE $%d)", len(qArgs), len(qArgs), len(qArgs))
	}
	query += fmt.Sprintf(" ORDER BY tc.created_at ASC LIMIT $%d", len(qArgs)+1)
	qArgs = append(qArgs, limit)

	rows, err := s.pool.Query(ctx, query, qArgs...)
	if err != nil {
		return nil, fmt.Errorf("query trace summary: %w", err)
	}
	defer rows.Close()

	var steps []map[string]any
	for rows.Next() {
		var id uuid.UUID
		var toolName, output, status string
		var ca any
		if err := rows.Scan(&id, &toolName, &output, &status, &ca); err != nil {
			return nil, err
		}
		summary := strings.TrimSpace(output)
		if len(summary) > 100 {
			summary = summary[:100] + "..."
		}
		steps = append(steps, map[string]any{
			"step_id":   id,
			"tool_name": toolName,
			"status":    status,
			"summary":   summary,
		})
	}
	if steps == nil {
		steps = []map[string]any{}
	}

	return map[string]any{
		"work_item_id":  resolvedWorkItemID,
		"work_item_key": wiKey,
		"title":         wiTitle,
		"status":        wiStatus,
		"total_steps":   len(steps),
		"steps":         steps,
	}, nil
}
