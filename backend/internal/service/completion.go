package service

import (
	"context"
	"strings"

	"agent-workbench/backend/internal/model"
	"github.com/google/uuid"
)

// CompletionCause is shared by the explicit completion tool and Engine fallback.
// Running out of frontier or returning prose cannot bypass live goal proofs.
func (s *RunService) CompletionCause(ctx context.Context, runID uuid.UUID, requested string) (model.TerminationCause, error) {
	switch strings.ToLower(requested) {
	case "partial":
		return model.CausePartial, nil
	case "blocked", "needs_input":
		return model.CauseNeedsInput, nil
	}
	var goals, proved, intents, incomplete int
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM exploration_nodes WHERE run_id=$1 AND kind='goal'),
		(SELECT count(*) FROM exploration_nodes g WHERE g.run_id=$1 AND g.kind='goal' AND g.state='met' AND EXISTS(SELECT 1 FROM exploration_edges e JOIN exploration_nodes n ON n.id=e.src_id WHERE e.dst_id=g.id AND e.rel='proves' AND e.valid AND n.state='verified' AND n.delete_reason IS NULL AND e.source_version=n.content_version AND e.target_version=g.content_version)),
		(SELECT count(*) FROM exploration_nodes WHERE run_id=$1 AND kind='intent'),
		(SELECT count(*) FROM work_items WHERE run_id=$1 AND status!='completed')`, runID).Scan(&goals, &proved, &intents, &incomplete)
	if err != nil {
		return model.CauseInternalError, err
	}
	if (goals > 0 && goals != proved) || (goals == 0 && intents > 0) {
		return model.CauseNeedsInput, nil
	}
	if incomplete > 0 {
		return model.CausePartial, nil
	}
	return model.CauseCompleted, nil
}
