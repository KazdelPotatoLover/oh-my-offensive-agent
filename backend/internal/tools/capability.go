package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

type AdapterContext struct {
	RunID, WorkItemID, AttemptID, SessionID uuid.UUID
	Service                                 *service.RunService
}

type Adapter struct {
	Name    string
	Version string
	Factory func(AdapterContext) Tool
}

var adapters = struct {
	sync.RWMutex
	items map[string]Adapter
}{items: map[string]Adapter{}}

// RegisterAdapter is the server/plugin extension boundary. It accepts code-owned
// factories, never executable strings supplied by a model or a task page.
func RegisterAdapter(a Adapter) error {
	if a.Name == "" || a.Version == "" || a.Factory == nil {
		return fmt.Errorf("invalid capability adapter")
	}
	adapters.Lock()
	defer adapters.Unlock()
	if _, exists := adapters.items[a.Name]; exists {
		return fmt.Errorf("adapter %q already registered", a.Name)
	}
	adapters.items[a.Name] = a
	model.RegisterCapabilityName(a.Name, a.Version)
	return nil
}

func init() {
	for _, a := range []Adapter{
		{Name: "register_asset", Version: "1", Factory: func(c AdapterContext) Tool { return &TypedAssetTool{scope: c} }},
		{Name: "get_endpoint", Version: "1", Factory: func(c AdapterContext) Tool { return NewGetEndpointTool(c.RunID, c.WorkItemID, c.Service) }},
		HTTPRequestAdapter(),
		{Name: "read_sample", Version: "1", Factory: func(c AdapterContext) Tool {
			return NewReadSampleToolWithScope(c.RunID, func(ctx context.Context, key string) (bool, error) {
				return c.Service.IsSampleInRunScope(ctx, c.RunID, key)
			})
		}},
		{Name: "extract_endpoint_links", Version: "1", Factory: func(c AdapterContext) Tool {
			return NewExtractEndpointLinksTool(c.RunID, c.WorkItemID, c.SessionID, c.Service)
		}},
		{Name: "run_security_check", Version: "1", Factory: func(c AdapterContext) Tool {
			return NewRunSecurityCheckTool(c.RunID, c.WorkItemID, c.AttemptID, c.SessionID, c.Service)
		}},
		{Name: "record_observation", Version: "1", Factory: func(c AdapterContext) Tool {
			return NewRecordObservationToolWithSession(c.RunID, c.AttemptID, c.SessionID, c.Service)
		}},
		{Name: "record_observation_batch", Version: "1", Factory: func(c AdapterContext) Tool {
			return NewRecordObservationBatchTool(c.RunID, c.AttemptID, c.SessionID, c.Service)
		}},
		{Name: "inspect_artifact", Version: "1", Factory: func(c AdapterContext) Tool { return NewArtifactTool("inspect_artifact", c) }},
	} {
		if err := RegisterAdapter(a); err != nil {
			panic(err)
		}
	}
}

type timedCapability struct {
	Tool
	timeout time.Duration
}

func (t timedCapability) Execute(ctx context.Context, args []byte) (string, error) {
	toolCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	return t.Tool.Execute(toolCtx, args)
}

func RegisterProfileTools(reg *Registry, p model.CapabilityProfile, c AdapterContext) error {
	adapters.RLock()
	defer adapters.RUnlock()
	for _, cfg := range p.Tools {
		if cfg.Disabled {
			continue
		}
		a, ok := adapters.items[cfg.Name]
		if !ok {
			return fmt.Errorf("adapter %s is not available", cfg.Name)
		}
		if cfg.Version != "" && cfg.Version != a.Version {
			return fmt.Errorf("adapter %s version mismatch", cfg.Name)
		}
		tool := a.Factory(c)
		if tool == nil || tool.Name() != cfg.Name {
			return fmt.Errorf("adapter identity mismatch")
		}
		if cfg.TimeoutSeconds > 0 {
			tool = timedCapability{Tool: tool, timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
		}
		reg.Register(tool)
	}
	return nil
}

func CapabilityCatalog(c AdapterContext) []map[string]any {
	adapters.RLock()
	defer adapters.RUnlock()
	res := []map[string]any{}
	for _, a := range adapters.items {
		t := a.Factory(c)
		res = append(res, map[string]any{"name": a.Name, "version": a.Version, "description": t.Description(), "parameters": json.RawMessage(t.Parameters()), "role": "worker"})
	}
	return res
}

func RegisterArtifactReaders(reg *Registry, c AdapterContext) {
	for _, name := range []string{"list_artifacts", "read_artifact", "get_worker_output"} {
		reg.Register(NewArtifactTool(name, c))
	}
	if c.WorkItemID == uuid.Nil {
		reg.Register(&ListCapabilityProfilesTool{scope: c})
	}
}

type ListCapabilityProfilesTool struct{ scope AdapterContext }

func (t *ListCapabilityProfilesTool) Name() string { return "list_capability_profiles" }
func (t *ListCapabilityProfilesTool) Description() string {
	return "List the immutable, authorized capability profiles for this Run. Use their names in intent input_data.profile."
}
func (t *ListCapabilityProfilesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *ListCapabilityProfilesTool) Validate(b []byte) error { return nil }
func (t *ListCapabilityProfilesTool) Execute(ctx context.Context, b []byte) (string, error) {
	run, err := t.scope.Service.Get(ctx, t.scope.RunID)
	if err != nil {
		return "", err
	}
	profiles, err := model.ProfilesFromInput(run.ExecutionInput)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(profiles)
	return string(out), err
}
