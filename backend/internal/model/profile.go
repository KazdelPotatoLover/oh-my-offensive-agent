package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrInvalidProfile = errors.New("invalid capability profile")

type CapabilityTool struct {
	Name           string `json:"name"`
	Version        string `json:"version,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Disabled       bool   `json:"disabled,omitempty"`
}

type CapabilityProfile struct {
	Name           string           `json:"name"`
	Version        string           `json:"version"`
	Tools          []CapabilityTool `json:"tools"`
	MaxModelCalls  int              `json:"max_model_calls,omitempty"`
	MaxToolCalls   int              `json:"max_tool_calls,omitempty"`
	TimeoutSeconds int              `json:"timeout_seconds,omitempty"`
}

var capabilityNames = struct {
	sync.RWMutex
	names map[string]string
}{names: map[string]string{
	"get_endpoint": "1", "read_sample": "1", "extract_endpoint_links": "1", "extract_api_endpoints": "1",
	"run_security_check": "1", "record_observation": "1", "record_observation_batch": "1",
	"inspect_artifact": "1", "register_asset": "1",
	"http_request": "1", "search_http_traffic": "1", "get_http_exchange": "1",
	"list_skills": "1", "read_skill": "1",
	"run_command": "1", "write_file": "1", "read_file": "1", "inspect_command": "1",
}}

// RegisterCapabilityName is server-owned. Task configuration cannot invent an
// executable or broaden network/file permissions by changing a profile.
func RegisterCapabilityName(name string, versions ...string) {
	capabilityNames.Lock()
	defer capabilityNames.Unlock()
	version := "1"
	if len(versions) > 0 {
		version = versions[0]
	}
	capabilityNames.names[name] = version
}

func DefaultProfiles() []CapabilityProfile {
	return []CapabilityProfile{
		{
			Name:    "explore",
			Version: "1",
			Tools: []CapabilityTool{
				{Name: "get_endpoint"},
				{Name: "http_request"},
				{Name: "search_http_traffic"},
				{Name: "get_http_exchange"},
				{Name: "read_sample"},
				{Name: "extract_endpoint_links"},
				{Name: "extract_api_endpoints"},
				{Name: "record_observation"},
				{Name: "record_observation_batch"},
				{Name: "inspect_artifact"},
				{Name: "register_asset"},
				{Name: "list_skills"},
				{Name: "read_skill"},
			},
		},
		{
			Name:    "api_recon",
			Version: "1",
			Tools: []CapabilityTool{
				{Name: "get_endpoint"},
				{Name: "extract_api_endpoints"},
				{Name: "extract_endpoint_links"},
				{Name: "search_http_traffic"},
				{Name: "get_http_exchange"},
				{Name: "record_observation"},
				{Name: "record_observation_batch"},
				{Name: "inspect_artifact"},
				{Name: "register_asset"},
				{Name: "list_skills"},
				{Name: "read_skill"},
			},
		},
		{
			Name:    "active_probe",
			Version: "1",
			Tools: []CapabilityTool{
				{Name: "get_endpoint"},
				{Name: "http_request"},
				{Name: "search_http_traffic"},
				{Name: "get_http_exchange"},
				{Name: "extract_api_endpoints"},
				{Name: "extract_endpoint_links"},
				{Name: "record_observation"},
				{Name: "record_observation_batch"},
				{Name: "inspect_artifact"},
				{Name: "register_asset"},
				{Name: "list_skills"},
				{Name: "read_skill"},
			},
		},
		{
			Name:    "browser",
			Version: "1",
			Tools: []CapabilityTool{
				{Name: "get_endpoint"},
				{Name: "http_request"},
				{Name: "search_http_traffic"},
				{Name: "get_http_exchange"},
				{Name: "extract_api_endpoints"},
				{Name: "extract_endpoint_links"},
				{Name: "record_observation"},
				{Name: "record_observation_batch"},
				{Name: "inspect_artifact"},
				{Name: "register_asset"},
				{Name: "list_skills"},
				{Name: "read_skill"},
			},
		},
		{
			Name:    "http_readonly",
			Version: "1",
			Tools: []CapabilityTool{
				{Name: "get_endpoint"},
				{Name: "extract_endpoint_links"},
				{Name: "record_observation"},
				{Name: "inspect_artifact"},
			},
		},
		{
			Name:    "sample",
			Version: "1",
			Tools: []CapabilityTool{
				{Name: "read_sample"},
				{Name: "record_observation"},
				{Name: "inspect_artifact"},
			},
		},
	}
}

func ValidateProfiles(profiles []CapabilityProfile) error {
	if len(profiles) > 32 {
		return fmt.Errorf("at most 32 capability profiles are allowed")
	}
	seen := map[string]bool{}
	capabilityNames.RLock()
	defer capabilityNames.RUnlock()
	for _, p := range profiles {
		if strings.TrimSpace(p.Name) == "" || len(p.Name) > 64 || seen[p.Name] || strings.TrimSpace(p.Version) == "" || len(p.Tools) == 0 || len(p.Tools) > 64 {
			return fmt.Errorf("invalid or duplicate capability profile %q", p.Name)
		}
		seen[p.Name] = true
		if p.MaxModelCalls < 0 || p.MaxToolCalls < 0 || p.TimeoutSeconds < 0 || p.TimeoutSeconds > 3600 {
			return fmt.Errorf("invalid profile limits")
		}
		tools := map[string]bool{}
		for _, tool := range p.Tools {
			if capabilityNames.names[tool.Name] == "" || (tool.Version != "" && tool.Version != capabilityNames.names[tool.Name]) || tools[tool.Name] || tool.TimeoutSeconds < 0 || tool.TimeoutSeconds > 300 {
				return fmt.Errorf("unknown, duplicate or invalid tool %q in profile %s", tool.Name, p.Name)
			}
			tools[tool.Name] = true
		}
	}
	return nil
}

// ProfilesFromInput reads frozen capabilities. Old snapshots use built-ins.
func ProfilesFromInput(input json.RawMessage) ([]CapabilityProfile, error) {
	var in struct {
		Profiles []CapabilityProfile `json:"profiles"`
		Config   struct {
			Profiles []CapabilityProfile `json:"profiles"`
		} `json:"config"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	profiles := in.Profiles
	if profiles == nil {
		profiles = in.Config.Profiles
	}
	if profiles == nil {
		profiles = DefaultProfiles()
	}
	capabilityNames.RLock()
	for i := range profiles {
		for j := range profiles[i].Tools {
			if profiles[i].Tools[j].Version == "" {
				profiles[i].Tools[j].Version = capabilityNames.names[profiles[i].Tools[j].Name]
			}
		}
	}
	capabilityNames.RUnlock()
	if err := ValidateProfiles(profiles); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	return profiles, nil
}

func ProfileByName(input json.RawMessage, name string) (CapabilityProfile, error) {
	if name == "" {
		name = "explore"
	}
	profiles, err := ProfilesFromInput(input)
	if err != nil {
		return CapabilityProfile{}, err
	}
	for _, p := range profiles {
		if p.Name == name {
			return p, nil
		}
	}
	return CapabilityProfile{}, fmt.Errorf("%w: profile %q is not authorized in this run", ErrInvalidProfile, name)
}

func (p CapabilityProfile) Fingerprint() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p CapabilityProfile) ApplyBudget(b BudgetConfig) BudgetConfig {
	if p.MaxModelCalls > 0 && (b.WorkerMaxModelCalls <= 0 || p.MaxModelCalls < b.WorkerMaxModelCalls) {
		b.WorkerMaxModelCalls = p.MaxModelCalls
	}
	if p.MaxToolCalls > 0 && (b.WorkerMaxToolCalls <= 0 || p.MaxToolCalls < b.WorkerMaxToolCalls) {
		b.WorkerMaxToolCalls = p.MaxToolCalls
	}
	if p.TimeoutSeconds > 0 && (b.WorkerTimeoutSeconds <= 0 || p.TimeoutSeconds < b.WorkerTimeoutSeconds) {
		b.WorkerTimeoutSeconds = p.TimeoutSeconds
	}
	return b
}
