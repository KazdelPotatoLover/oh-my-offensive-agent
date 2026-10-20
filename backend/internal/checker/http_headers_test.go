package checker

import (
	"encoding/json"
	"testing"

	"agent-workbench/backend/internal/model"
)

func TestHSTSPolicyIsNotJustASubstring(t *testing.T) {
	for _, tc := range []struct {
		header string
		active bool
	}{
		{`max-age=31536000; includeSubDomains`, true},
		{`max-age="3600"`, true},
		{`MAX-AGE = 3600`, true},
		{`max-age=0`, false},
		{`max-age=000`, false},
		{`x-max-age=3600`, false},
		{`max-age=invalid`, false},
		{`max-age=-1`, false},
		{`max-age=10; max-age=20`, false},
		{`max-age=10; includeSubDomains=no`, false},
	} {
		t.Run(tc.header, func(t *testing.T) {
			evidence, _ := json.Marshal(map[string]any{
				"request":  map[string]any{"url": "https://example.test/", "method": "GET"},
				"response": map[string]any{"status_code": 200, "headers": map[string]string{"Strict-Transport-Security": tc.header}},
			})
			results := EvaluateHTTPResponseChecks("http_strict_transport_security", evidence)
			if len(results) != 1 || (results[0].Status == model.CheckStatusPass) != tc.active {
				t.Fatalf("header=%q results=%+v", tc.header, results)
			}
		})
	}
}

func TestIncompleteHeaderEvidenceIsNotAConfigurationFinding(t *testing.T) {
	for _, evidence := range []string{
		`{"request":{"url":"https://example.test/"},"response":{"status_code":200}}`,
		`{"request":{},"response":{"status_code":200,"headers":{}}}`,
	} {
		results := EvaluateHTTPResponseChecks("http_security_headers", []byte(evidence))
		if len(results) != 1 || results[0].Status != model.CheckStatusInsufficientEvidence || results[0].GenerateFinding {
			t.Fatalf("results=%+v", results)
		}
	}
	results := EvaluateHTTPResponseChecks("http_strict_transport_security", []byte(`{"request":{"url":"https://127.0.0.1/"},"response":{"status_code":200,"headers":{}}}`))
	if len(results) != 1 || results[0].Status != model.CheckStatusNotApplicable {
		t.Fatalf("IP policy results=%+v", results)
	}
}
