package tools_test

import (
	"testing"

	"agent-workbench/backend/internal/tools"
)

func TestGetNativeToolCatalog(t *testing.T) {
	catalog := tools.GetNativeToolCatalog()
	if len(catalog) == 0 {
		t.Fatalf("expected non-empty tool catalog")
	}

	foundEndpoint := false
	foundFinding := false
	foundSteer := false
	for _, item := range catalog {
		if item.Name == "" {
			t.Errorf("tool item missing name: %+v", item)
		}
		if item.Category == "" || item.CategoryName == "" {
			t.Errorf("tool item %s missing category info", item.Name)
		}
		if item.Description == "" {
			t.Errorf("tool item %s missing description", item.Name)
		}
		if len(item.Roles) == 0 {
			t.Errorf("tool item %s missing roles", item.Name)
		}
		if item.Name == "get_endpoint" {
			foundEndpoint = true
		}
		if item.Name == "report_finding" {
			foundFinding = true
		}
		if item.Name == "steer_work" {
			foundSteer = true
		}
	}

	if !foundEndpoint {
		t.Errorf("expected get_endpoint in catalog")
	}
	if !foundFinding {
		t.Errorf("expected report_finding in catalog")
	}
	if !foundSteer {
		t.Errorf("expected steer_work in catalog")
	}
}
