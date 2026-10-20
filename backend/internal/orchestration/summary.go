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
	if err == nil {
		var completedItems, unfinishedItems, exhaustedItems, interruptedItems []string
		for _, wi := range workItems {
			if wi.Status == model.WorkItemStatusCompleted {
				completedItems = append(completedItems, fmt.Sprintf("%s (%s)", wi.Title, wi.WorkType))
			} else if wi.Status == model.WorkItemStatusRunning {
				interruptedItems = append(interruptedItems, fmt.Sprintf("%s [interrupted at closeout]", wi.Title))
			} else if wi.Status == model.WorkItemStatusPending || wi.Status == model.WorkItemStatusBlocked {
				unfinishedItems = append(unfinishedItems, fmt.Sprintf("%s [unstarted; cancelled at closeout]", wi.Title))
			} else if wi.Status == model.WorkItemStatusExhausted {
				exhaustedItems = append(exhaustedItems, fmt.Sprintf("%s [status: %s]", wi.Title, wi.Status))
			} else {
				unfinishedItems = append(unfinishedItems, fmt.Sprintf("%s [status: %s]", wi.Title, wi.Status))
			}
		}

		sb.WriteString(fmt.Sprintf("- **Completed Work Items** (%d):\n", len(completedItems)))
		for _, item := range completedItems {
			sb.WriteString(fmt.Sprintf("  - %s\n", item))
		}
		if len(unfinishedItems) > 0 {
			sb.WriteString(fmt.Sprintf("- **Unstarted / Cancelled / Failed Work Items** (%d):\n", len(unfinishedItems)))
			for _, item := range unfinishedItems {
				sb.WriteString(fmt.Sprintf("  - %s\n", item))
			}
		}
		if len(interruptedItems) > 0 {
			sb.WriteString(fmt.Sprintf("- **Interrupted Work Items** (%d):\n", len(interruptedItems)))
			for _, item := range interruptedItems {
				sb.WriteString(fmt.Sprintf("  - %s\n", item))
			}
		}
		if len(exhaustedItems) > 0 {
			sb.WriteString(fmt.Sprintf("- **Budget Exhausted Work Items** (%d):\n", len(exhaustedItems)))
			for _, item := range exhaustedItems {
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
		sb.WriteString(fmt.Sprintf("- **Discovered Assets** (%d; closeout snapshot):\n", len(assets)))
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

	coverage, coverageErr := runService.ListAssetCoverage(ctx, runID)
	if coverageErr == nil {
		planned, visited := 0, 0
		for _, a := range coverage {
			if a.Planned {
				planned++
			}
			if a.Visited {
				visited++
			}
		}
		sb.WriteString(fmt.Sprintf("- **Coverage snapshot before closeout**: discovered=%d, planned=%d, visited=%d, unvisited=%d\n", len(coverage), planned, visited, len(coverage)-visited))
		sb.WriteString("  - Visited means a successful get_endpoint/read_sample result; it does not require an observation or completed work item.\n")
		for _, a := range coverage {
			if !a.Visited {
				sb.WriteString(fmt.Sprintf("  - Unvisited: %s (asset %s)\n", a.ExternalKey, a.AssetID))
			}
		}
	} else {
		sb.WriteString("- Coverage inventory unavailable.\n")
	}

	// Observations count
	obs, err := runService.ListObservationsByRun(ctx, runID)
	if err == nil {
		sb.WriteString(fmt.Sprintf("- **Observations Recorded** (%d; each source remains traceable)\n", len(obs)))
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
	if calls, callErr := runService.ListAgentToolCalls(ctx, runID); callErr == nil {
		failed := 0
		for _, call := range calls {
			if call.Status == "failed" || call.Status == "rejected" {
				failed++
			}
		}
		sb.WriteString(fmt.Sprintf("- **Tool failures**: %d\n", failed))
	} else {
		sb.WriteString("- Tool failure inventory unavailable.\n")
	}
	sb.WriteString("- **Scope limits**: counts above describe persisted discoveries and planned/visited assets; completed work items do not imply complete security coverage.\n")
	if run, runErr := runService.Get(ctx, runID); runErr == nil {
		var input struct {
			Scope  json.RawMessage `json:"scope"`
			Policy json.RawMessage `json:"discovery_policy"`
		}
		if json.Unmarshal(run.ExecutionInput, &input) == nil {
			sb.WriteString(fmt.Sprintf("  - Configured scope: %s\n", input.Scope))
			if len(input.Policy) > 0 {
				sb.WriteString(fmt.Sprintf("  - Discovery limits: %s\n", input.Policy))
			}
		}
	}
	// Security Check Results & Candidate Findings
	checkResults, crErr := runService.ListCheckResultsByRun(ctx, runID)
	findings, fErr := runService.ListFindingsByRun(ctx, runID)
	if crErr == nil {
		passCount, attnCount, naCount, insuffCount, errCount := 0, 0, 0, 0, 0
		checkedAssets := make(map[uuid.UUID]bool)
		for _, cr := range checkResults {
			checkedAssets[cr.AssetID] = true
			switch cr.Status {
			case model.CheckStatusPass:
				passCount++
			case model.CheckStatusNeedsAttention:
				attnCount++
			case model.CheckStatusNotApplicable:
				naCount++
			case model.CheckStatusInsufficientEvidence:
				insuffCount++
			case model.CheckStatusError:
				errCount++
			}
		}

		sb.WriteString(fmt.Sprintf("\n### Deterministic Check Results (%d total: pass=%d, needs_attention=%d, not_applicable=%d, insufficient_evidence=%d, error=%d)\n",
			len(checkResults), passCount, attnCount, naCount, insuffCount, errCount))
		for _, cr := range checkResults {
			sourceRef := "no_evidence"
			if cr.SourceToolCallID != nil {
				sourceRef = cr.SourceToolCallID.String()
			}
			sb.WriteString(fmt.Sprintf("  - [%s] %s v%s (asset %s): %s (evidence %s)\n",
				cr.Status, cr.CheckName, cr.CheckVersion, cr.AssetID, cr.Explanation, sourceRef))
		}

		// Candidate Findings for Human Review
		if fErr == nil {
			sb.WriteString(fmt.Sprintf("\n### Candidate Findings (Pending Review) (%d):\n", len(findings)))
			if len(findings) == 0 {
				sb.WriteString("  - None recorded.\n")
			}
			for _, f := range findings {
				srcRef := "none"
				if f.SourceToolCallID != nil {
					srcRef = f.SourceToolCallID.String()
				}
				sb.WriteString(fmt.Sprintf("  - [%s|%s] %s (finding %s, check_result %s, evidence %s): %s\n",
					f.Severity, f.Status, f.Title, f.ID, f.CheckResultID, srcRef, f.Description))
			}
		}

		// Unchecked Assets (未检查资产)
		runAssets, _ := runService.ListRunAssets(ctx, runID)
		var unchecked []model.RunAsset
		for _, ra := range runAssets {
			if !checkedAssets[ra.AssetID] {
				unchecked = append(unchecked, ra)
			}
		}
		sb.WriteString(fmt.Sprintf("\n### Unchecked Assets (%d):\n", len(unchecked)))
		if len(unchecked) == 0 {
			sb.WriteString("  - All in-scope assets received security checks.\n")
		} else {
			for _, ra := range unchecked {
				var snapshot struct {
					ExternalKey string `json:"external_key"`
				}
				_ = json.Unmarshal(ra.Snapshot, &snapshot)
				sb.WriteString(fmt.Sprintf("  - Unchecked: %s (asset %s)\n", snapshot.ExternalKey, ra.AssetID))
			}
		}
	}

	sb.WriteString("- **Snapshot note**: counts are a snapshot before the terminal cleanup transaction; pending/blocked work is unstarted and cancelled, and running work is interrupted at closeout. Tool call counts are not page counts.\n")

	return sb.String()
}
