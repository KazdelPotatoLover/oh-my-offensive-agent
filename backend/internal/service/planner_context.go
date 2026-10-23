package service

import (
	"context"
	"encoding/json"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/netcap"
	"github.com/google/uuid"
)

// PlannerExecutionContext exposes persisted blockers and the actual capability
// names to each fresh planning session. It never changes limits or authorizes a
// retry. The bounded digest points back to the original tool-call records.
func (s *RunService) PlannerExecutionContext(ctx context.Context, runID uuid.UUID) (map[string]any, error) {
	var raw json.RawMessage
	var epoch int
	if err := s.pool.QueryRow(ctx, `SELECT execution_input,current_epoch FROM runs WHERE id=$1`, runID).Scan(&raw, &epoch); err != nil {
		return nil, err
	}
	if err := validateContextEpoch(ctx, epoch); err != nil {
		return nil, err
	}
	policy, err := netcap.PolicyFromExecutionInput(raw)
	if err != nil {
		return nil, err
	}
	profiles, err := model.ProfilesFromInput(raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		names = append(names, p.Name)
	}
	rows, err := s.pool.Query(ctx, `WITH origins AS (
 SELECT origin FROM http_request_ledger WHERE run_id=$1 AND epoch_number=$2
 UNION SELECT origin FROM http_pages WHERE run_id=$1 AND epoch_number=$2)
 SELECT origin,
 (SELECT count(*) FROM http_request_ledger WHERE run_id=$1 AND epoch_number=$2 AND origin=origins.origin AND state IN ('reserved','completed','outcome_unknown','cancelled','timeout')),
 (SELECT count(*) FROM http_pages WHERE run_id=$1 AND epoch_number=$2 AND origin=origins.origin)
 FROM origins ORDER BY origin`, runID, epoch)
	if err != nil {
		return nil, err
	}
	origins := make([]map[string]any, 0)
	for rows.Next() {
		var origin string
		var requests, pages int
		if err := rows.Scan(&origin, &requests, &pages); err != nil {
			rows.Close()
			return nil, err
		}
		origins = append(origins, map[string]any{
			"origin": origin, "requests_used": requests, "pages_used": pages,
			"requests_blocked":  policy.EnforceLimits && requests >= policy.MaxRequests,
			"new_pages_blocked": policy.EnforceLimits && pages >= policy.MaxPages,
		})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT tool_name,left(error_message,400),count(*),(array_agg(id ORDER BY created_at DESC))[1]
 FROM (SELECT id,tool_name,error_message,created_at FROM agent_tool_calls
 WHERE run_id=$1 AND epoch_number=$2 AND status IN ('failed','rejected') AND error_message IS NOT NULL
 ORDER BY created_at DESC LIMIT 100) recent
 GROUP BY tool_name,error_message ORDER BY max(created_at) DESC LIMIT 12`, runID, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	errors := make([]map[string]any, 0)
	for rows.Next() {
		var name, message string
		var count int
		var id uuid.UUID
		if err := rows.Scan(&name, &message, &count, &id); err != nil {
			return nil, err
		}
		errors = append(errors, map[string]any{"tool": name, "error": message, "count": count, "source_tool_call_id": id})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{
		"epoch_number": epoch, "authorized_profiles": names,
		"network_limits":       map[string]any{"enforced": policy.EnforceLimits, "max_requests_per_origin": policy.MaxRequests, "max_pages_per_origin": policy.MaxPages, "origins": origins},
		"recent_tool_failures": errors,
		"guidance":             "Limits are frozen per origin and epoch. Waiting, changing work keys or asset kinds does not reset them. Reuse existing material after a limit refusal. A binding/schema error requires correcting the identity/arguments, not repeating the same network request. Inspect recent_done_intents and recent_findings before dispatch; require a material new hypothesis or changed precondition for a revisit. Use partial/blocked if no executable route remains; never prove goals with unverified evidence.",
	}, nil
}
