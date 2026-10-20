package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

// GenerateProgrammaticSummary aggregates persisted database evidence into a structured summary
// when model summary cannot be generated due to budget exhaustion, timeout, or model unavailability.
func GenerateProgrammaticSummary(ctx context.Context, runID uuid.UUID, runService *service.RunService, cause model.TerminationCause) string {
	var sb strings.Builder
	sb.WriteString("## Execution Wrap-up Summary\n\n")
	sb.WriteString(fmt.Sprintf("- **Termination Cause**: %s\n", cause))

	// Get work items
	workItems, err := runService.ListWorkItemsByRun(ctx, runID)
	if err == nil && len(workItems) > 0 {
		var completedItems, unfinishedItems []string
		for _, wi := range workItems {
			if wi.Status == model.WorkItemStatusCompleted {
				completedItems = append(completedItems, fmt.Sprintf("%s (%s)", wi.Title, wi.WorkType))
			} else {
				unfinishedItems = append(unfinishedItems, fmt.Sprintf("%s [status: %s]", wi.Title, wi.Status))
			}
		}

		sb.WriteString(fmt.Sprintf("- **Completed Work Items** (%d):\n", len(completedItems)))
		for _, item := range completedItems {
			sb.WriteString(fmt.Sprintf("  - %s\n", item))
		}
		if len(unfinishedItems) > 0 {
			sb.WriteString(fmt.Sprintf("- **Unfinished / Cancelled Work Items** (%d):\n", len(unfinishedItems)))
			for _, item := range unfinishedItems {
				sb.WriteString(fmt.Sprintf("  - %s\n", item))
			}
		}
	}
	if err != nil {
		sb.WriteString("- Work inventory unavailable; inspect persisted records after database recovery.\n")
	}

	// Discovered assets count
	assets, err := runService.ListRunAssets(ctx, runID)
	if err == nil {
		sb.WriteString(fmt.Sprintf("- **Discovered Assets** (%d):\n", len(assets)))
		for _, a := range assets {
			var snapshot struct {
				ExternalKey string `json:"external_key"`
			}
			_ = json.Unmarshal(a.Snapshot, &snapshot)
			sb.WriteString(fmt.Sprintf("  - %s (asset %s)\n", snapshot.ExternalKey, a.AssetID))
		}
	} else {
		sb.WriteString("- Asset inventory unavailable.\n")
	}

	// Observations count
	obs, err := runService.ListObservationsByRun(ctx, runID)
	if err == nil {
		sb.WriteString(fmt.Sprintf("- **Observations Recorded** (%d)\n", len(obs)))
		for i, o := range obs {
			if i >= 20 {
				sb.WriteString("  - Further observations remain available in the evidence view.\n")
				break
			}
			content := []rune(strings.ReplaceAll(o.Content, "\n", " "))
			if len(content) > 240 {
				content = append(content[:240], []rune("…")...)
			}
			sb.WriteString(fmt.Sprintf("  - [%s] %s (observation %s; source %s)\n", o.ObsType, string(content), o.ID, o.SourceToolCallID))
		}
	} else {
		sb.WriteString("- Observations unavailable.\n")
	}

	return sb.String()
}
