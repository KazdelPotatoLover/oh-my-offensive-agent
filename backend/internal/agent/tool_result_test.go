package agent

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEvidenceToolMessageRetainsReferencesWithinBudget(t *testing.T) {
	body := strings.Repeat("<p>页面正文</p>\n", 1000)
	raw, _ := json.Marshal(map[string]interface{}{"integrity": map[string]interface{}{"is_truncated": false}, "request": map[string]interface{}{"asset_id": "asset-id"}, "response": map[string]interface{}{"status_code": 200, "headers": map[string]string{"Content-Type": "text/html"}, "body": body}})
	for _, limit := range []int{256, 1024, 8192, len(raw) + 1024} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			msg := evidenceToolMessage(string(raw), "evidence-record-uuid", "call-real-id", limit)
			if !json.Valid([]byte(msg)) || !utf8.ValidString(msg) || len(msg) > limit {
				t.Fatalf("invalid or oversized model result: len=%d limit=%d", len(msg), limit)
			}
			var parsed struct {
				Execution struct {
					Source    string `json:"source_tool_call_id"`
					Call      string `json:"call_id"`
					Truncated bool   `json:"result_truncated"`
				} `json:"_execution"`
				Response struct {
					Status int    `json:"status_code"`
					Body   string `json:"body"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(msg), &parsed); err != nil {
				t.Fatal(err)
			}
			if parsed.Execution.Source != "evidence-record-uuid" || parsed.Execution.Call != "call-real-id" {
				t.Fatal("lost or fabricated evidence identity")
			}
			if parsed.Execution.Truncated != (limit < len(raw)) {
				t.Fatal("incorrect preview truncation flag")
			}
			if limit >= 1024 && parsed.Response.Status != 200 {
				t.Fatal("lost HTTP status during body truncation")
			}
			if limit > len(raw) && parsed.Response.Body != body {
				t.Fatal("body changed without truncation")
			}
		})
	}
}
