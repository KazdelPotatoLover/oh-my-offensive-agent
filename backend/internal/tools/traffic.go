package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

type TrafficTool struct {
	name  string
	scope AdapterContext
}

func (t *TrafficTool) Name() string { return t.name }
func (t *TrafficTool) Description() string {
	if t.name == "search_http_traffic" {
		return "Search redacted captured HTTP traffic shared within this Run, including authorized past epochs. No network request. Returns receipt_id for get_http_exchange, source_tool_call_id for original evidence, and artifact IDs. Default 5, max 20. Pass next_cursor as after. Optional origin/host/url_contains/method/status/epoch filters. Historical material is not a new verification."
	}
	return "Read a gateway exchange by receipt_id returned by search_http_traffic or http_request. Returns bounded redacted request/response pages and artifact IDs; continue bodies with read_artifact(artifact_id,offset=next_offset). Does not contact the target or create fresh empirical evidence."
}
func (t *TrafficTool) Parameters() json.RawMessage {
	if t.name == "search_http_traffic" {
		return json.RawMessage(`{"type":"object","properties":{"origin":{"type":"string"},"host":{"type":"string"},"url_contains":{"type":"string"},"method":{"type":"string"},"status":{"type":"integer"},"epoch":{"type":"integer"},"limit":{"type":"integer","minimum":1,"maximum":20},"after":{"type":"string","description":"next_cursor receipt UUID from the previous search"}}}`)
	}
	return json.RawMessage(`{"type":"object","properties":{"receipt_id":{"type":"string","format":"uuid"}},"required":["receipt_id"]}`)
}
func (t *TrafficTool) Validate(b []byte) error {
	if t.name == "search_http_traffic" {
		var f service.TrafficFilter
		return json.Unmarshal(b, &f)
	}
	var in struct {
		ID uuid.UUID `json:"receipt_id"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	if in.ID == uuid.Nil {
		return fmt.Errorf("receipt_id is required; use the receipt UUID, not request/artifact/call IDs")
	}
	return nil
}
func (t *TrafficTool) Execute(ctx context.Context, b []byte) (string, error) {
	c := t.scope
	if err := c.Service.AuthorizeTrafficReader(ctx, c.RunID, c.SessionID, c.WorkItemID); err != nil {
		return "", err
	}
	var out any
	var err error
	if t.name == "search_http_traffic" {
		var f service.TrafficFilter
		if err := json.Unmarshal(b, &f); err != nil {
			return "", err
		}
		out, err = c.Service.SearchHTTPTraffic(ctx, c.RunID, f)
	} else {
		var in struct {
			ID uuid.UUID `json:"receipt_id"`
		}
		if err := json.Unmarshal(b, &in); err != nil {
			return "", err
		}
		out, err = c.Service.ReadHTTPExchange(ctx, c.RunID, in.ID)
	}
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(out)
	return string(raw), err
}

func RegisterTrafficReaders(reg *Registry, c AdapterContext) {
	for _, name := range []string{"search_http_traffic", "get_http_exchange"} {
		reg.Register(&TrafficTool{name: name, scope: c})
	}
}
