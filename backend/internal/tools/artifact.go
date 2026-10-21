package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type ArtifactTool struct {
	name  string
	scope AdapterContext
}

func NewArtifactTool(name string, c AdapterContext) *ArtifactTool {
	return &ArtifactTool{name: name, scope: c}
}
func (t *ArtifactTool) Name() string { return t.name }
func (t *ArtifactTool) Description() string {
	switch t.name {
	case "get_worker_output":
		return "Read a page of a worker's immutable full tool output by work_item_id and step_id within this Run."
	case "write_artifact":
		return "Save a named UTF-8 artifact in this work item's controlled workspace. Paths and executable commands are not accepted."
	case "inspect_artifact":
		return "Inspect a saved artifact as data: search text or parse JSON. This never executes its contents or makes a network request."
	default:
		return "List or read immutable artifacts from the current Run, using byte offsets and bounded pages."
	}
}
func (t *ArtifactTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"artifact_id":{"type":"string"},"work_item_id":{"type":"string"},"step_id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"},"name":{"type":"string"},"content":{"type":"string"},"query":{"type":"string"},"parse_json":{"type":"boolean"}}}`)
}
func (t *ArtifactTool) Validate(args []byte) error {
	var in map[string]any
	return json.Unmarshal(args, &in)
}
func (t *ArtifactTool) Execute(ctx context.Context, args []byte) (string, error) {
	var in struct {
		ArtifactID, WorkItemID, StepID, Name, Content, Query string
		Offset, Limit                                        int
		ParseJSON                                            bool `json:"parse_json"`
	}
	// Use explicit wire keys for UUID fields; Go's field matching does not remove underscores.
	var wire struct {
		ArtifactID string `json:"artifact_id"`
		WorkItemID string `json:"work_item_id"`
		StepID     string `json:"step_id"`
		Name       string `json:"name"`
		Content    string `json:"content"`
		Query      string `json:"query"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		ParseJSON  bool   `json:"parse_json"`
	}
	if err := json.Unmarshal(args, &wire); err != nil {
		return "", err
	}
	in.ArtifactID, in.WorkItemID, in.StepID, in.Name, in.Content, in.Query, in.Offset, in.Limit, in.ParseJSON = wire.ArtifactID, wire.WorkItemID, wire.StepID, wire.Name, wire.Content, wire.Query, wire.Offset, wire.Limit, wire.ParseJSON
	c := t.scope
	var out any
	var err error
	switch t.name {
	case "list_artifacts":
		out, err = c.Service.ListArtifacts(ctx, c.RunID, nil, in.Offset, in.Limit)
	case "write_artifact":
		out, err = c.Service.SaveArtifact(ctx, c.RunID, &c.WorkItemID, &c.SessionID, nil, in.Name, "text/plain", in.Content)
	case "get_worker_output":
		var workID, stepID uuid.UUID
		workID, err = uuid.Parse(in.WorkItemID)
		if err != nil {
			return "", err
		}
		stepID, err = uuid.Parse(in.StepID)
		if err != nil {
			return "", err
		}
		out, err = c.Service.GetWorkerOutput(ctx, c.RunID, workID, stepID, in.Offset, in.Limit)
	default:
		id, parseErr := uuid.Parse(in.ArtifactID)
		if parseErr != nil {
			return "", parseErr
		}
		page, readErr := c.Service.ReadArtifact(ctx, c.RunID, id, in.Offset, in.Limit)
		if readErr != nil {
			return "", readErr
		}
		if t.name == "inspect_artifact" {
			body, _ := page["content"].(string)
			if in.Query != "" {
				page["query_found"] = strings.Contains(body, in.Query)
				page["search_complete"] = in.Offset == 0 && page["has_more"] == false
			}
			if in.ParseJSON {
				if in.Offset != 0 || page["has_more"] == true {
					return "", fmt.Errorf("JSON inspection needs a complete bounded artifact")
				}
				var parsed any
				if err := json.Unmarshal([]byte(body), &parsed); err != nil {
					return "", err
				}
				page["parsed"] = parsed
			}
		}
		out = page
	}
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	return string(b), err
}
