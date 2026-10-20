package checker

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"agent-workbench/backend/internal/model"
)

type httpToolOutput struct {
	Error     map[string]string `json:"error"`
	Integrity struct {
		SavedBytes  int    `json:"saved_bytes"`
		IsTruncated bool   `json:"is_truncated"`
		Note        string `json:"note"`
	} `json:"integrity"`
	Request struct {
		Method    string `json:"method"`
		URL       string `json:"url"`
		AssetID   string `json:"asset_id"`
		StartedAt string `json:"started_at"`
	} `json:"request"`
	Response *struct {
		StatusCode       int               `json:"status_code"`
		Headers          map[string]string `json:"headers"`
		ReceivedAt       string            `json:"received_at"`
		DurationMs       int64             `json:"duration_ms"`
		HeaderDurationMs int64             `json:"header_duration_ms"`
		Body             string            `json:"body"`
	} `json:"response"`
}

// getHeaderCaseInsensitive searches for a header in a case-insensitive manner.
func getHeaderCaseInsensitive(headers map[string]string, key string) (string, bool) {
	if headers == nil {
		return "", false
	}
	target := strings.ToLower(strings.TrimSpace(key))
	for k, v := range headers {
		if strings.ToLower(strings.TrimSpace(k)) == target {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// EvaluateHTTPResponseChecks executes deterministic checks against persisted get_endpoint evidence.
// It supports running a specific check rule or the entire suite ("http_security_headers").
func EvaluateHTTPResponseChecks(checkID string, evidenceJSON []byte) []RuleResult {
	checkID = strings.TrimSpace(checkID)

	// 1. Missing evidence check
	if len(evidenceJSON) == 0 || strings.TrimSpace(string(evidenceJSON)) == "" {
		return []RuleResult{
			makeInsufficientEvidenceResult(checkID, "No persisted get_endpoint evidence found for asset in this run. Fetch the endpoint first."),
		}
	}

	// 2. Corrupted / unparseable payload check (error must never be pass)
	var out httpToolOutput
	if err := json.Unmarshal(evidenceJSON, &out); err != nil {
		return []RuleResult{
			makeErrorResult(checkID, fmt.Sprintf("Corrupted HTTP evidence payload: %v", err)),
		}
	}

	// 3. Truncated response check
	if out.Integrity.IsTruncated {
		return []RuleResult{
			makeInsufficientEvidenceResult(checkID, "Response was truncated (integrity.is_truncated=true); headers or body incomplete for security check."),
		}
	}

	// 4. Incomplete response / connection error
	if out.Error != nil && out.Error["type"] != "" {
		return []RuleResult{
			makeInsufficientEvidenceResult(checkID, fmt.Sprintf("HTTP request did not yield a valid response: %s (%s)", out.Error["type"], out.Error["message"])),
		}
	}
	if out.Response == nil || out.Response.StatusCode <= 0 {
		return []RuleResult{
			makeInsufficientEvidenceResult(checkID, "HTTP response is missing or status code is invalid in evidence."),
		}
	}

	parsedRequest, parseErr := url.Parse(out.Request.URL)
	if parseErr != nil || parsedRequest.Hostname() == "" || (parsedRequest.Scheme != "http" && parsedRequest.Scheme != "https") || out.Response.Headers == nil {
		return []RuleResult{makeInsufficientEvidenceResult(checkID, "HTTP request URL or response headers are missing or invalid.")}
	}

	// Build evidence snapshot
	snapMap := map[string]interface{}{
		"url":         out.Request.URL,
		"method":      out.Request.Method,
		"status_code": out.Response.StatusCode,
		"headers":     out.Response.Headers,
		"truncated":   out.Integrity.IsTruncated,
		"saved_bytes": out.Integrity.SavedBytes,
	}
	snapBytes, _ := json.Marshal(snapMap)

	var results []RuleResult

	// Determine rules to run
	runXContentType := checkID == "http_security_headers" || checkID == "http_x_content_type_options"
	runCSP := checkID == "http_security_headers" || checkID == "http_content_security_policy"
	runHSTS := checkID == "http_security_headers" || checkID == "http_strict_transport_security"

	// Rule 1: X-Content-Type-Options
	if runXContentType {
		results = append(results, evaluateXContentTypeOptions(out, snapBytes))
	}

	// Rule 2: Content-Security-Policy
	if runCSP {
		results = append(results, evaluateContentSecurityPolicy(out, snapBytes))
	}

	// Rule 3: Strict-Transport-Security
	if runHSTS {
		results = append(results, evaluateStrictTransportSecurity(out, snapBytes))
	}

	if len(results) == 0 {
		return []RuleResult{
			makeErrorResult(checkID, fmt.Sprintf("Unknown rule or suite %q", checkID)),
		}
	}

	return results
}

func evaluateXContentTypeOptions(out httpToolOutput, snapBytes []byte) RuleResult {
	const ruleID = "http_x_content_type_options"
	const ruleName = "X-Content-Type-Options 响应头检查"
	const ruleVersion = "1.0.0"

	// Condition: Applicable to HTTP 200 responses with content
	if out.Response.StatusCode != 200 {
		return RuleResult{
			CheckID:          ruleID,
			CheckName:        ruleName,
			CheckVersion:     ruleVersion,
			Status:           model.CheckStatusNotApplicable,
			Explanation:      fmt.Sprintf("X-Content-Type-Options 检查仅适用于 HTTP 200 响应，当前响应状态码为 %d。", out.Response.StatusCode),
			EvidenceSnapshot: snapBytes,
		}
	}

	val, found := getHeaderCaseInsensitive(out.Response.Headers, "X-Content-Type-Options")
	if found && strings.EqualFold(val, "nosniff") {
		return RuleResult{
			CheckID:          ruleID,
			CheckName:        ruleName,
			CheckVersion:     ruleVersion,
			Status:           model.CheckStatusPass,
			Explanation:      "X-Content-Type-Options 响应头已正确配置为 'nosniff'。",
			EvidenceSnapshot: snapBytes,
		}
	}

	var explanation string
	if !found {
		explanation = "HTTP 响应缺少 X-Content-Type-Options 响应头，浏览器可能对响应内容执行 MIME 嗅探。"
	} else {
		explanation = fmt.Sprintf("X-Content-Type-Options 响应头配置为 %q，非预期的 'nosniff'。", val)
	}

	return RuleResult{
		CheckID:            ruleID,
		CheckName:          ruleName,
		CheckVersion:       ruleVersion,
		Status:             model.CheckStatusNeedsAttention,
		Explanation:        explanation,
		EvidenceSnapshot:   snapBytes,
		GenerateFinding:    true,
		FindingTitle:       "[配置关注] X-Content-Type-Options 未配置为 nosniff",
		FindingCategory:    "configuration_risk",
		FindingSeverity:    "low",
		FindingDescription: "HTTP 响应未声明有效的 X-Content-Type-Options: nosniff。浏览器可能对返回的文本或媒体内容执行 MIME 嗅探，在特定场景下增加脚本注入风险。",
		FindingRemediation: "在 Web 服务器或反向代理配置中添加响应头: X-Content-Type-Options: nosniff。",
	}
}

func evaluateContentSecurityPolicy(out httpToolOutput, snapBytes []byte) RuleResult {
	const ruleID = "http_content_security_policy"
	const ruleName = "Content-Security-Policy 响应头存在性检查"
	const ruleVersion = "1.0.0"

	// Condition: Applicable only to HTML responses
	contentType, _ := getHeaderCaseInsensitive(out.Response.Headers, "Content-Type")
	contentTypeLower := strings.ToLower(contentType)
	if !strings.Contains(contentTypeLower, "text/html") {
		return RuleResult{
			CheckID:          ruleID,
			CheckName:        ruleName,
			CheckVersion:     ruleVersion,
			Status:           model.CheckStatusNotApplicable,
			Explanation:      fmt.Sprintf("Content-Security-Policy 策略检查仅适用于 HTML 页面资源，当前响应 Content-Type 为 %q。", contentType),
			EvidenceSnapshot: snapBytes,
		}
	}

	val, found := getHeaderCaseInsensitive(out.Response.Headers, "Content-Security-Policy")
	if found && strings.TrimSpace(val) != "" {
		return RuleResult{
			CheckID:          ruleID,
			CheckName:        ruleName,
			CheckVersion:     ruleVersion,
			Status:           model.CheckStatusPass,
			Explanation:      "Content-Security-Policy 响应头存在；本规则不评价策略语法、强度或实际浏览器防护效果。",
			EvidenceSnapshot: snapBytes,
		}
	}

	return RuleResult{
		CheckID:            ruleID,
		CheckName:          ruleName,
		CheckVersion:       ruleVersion,
		Status:             model.CheckStatusNeedsAttention,
		Explanation:        "HTML 页面未配置 Content-Security-Policy 响应头。",
		EvidenceSnapshot:   snapBytes,
		GenerateFinding:    true,
		FindingTitle:       "[配置风险] 缺失 Content-Security-Policy 响应头",
		FindingCategory:    "configuration_risk",
		FindingSeverity:    "low",
		FindingDescription: "HTML 页面未声明 Content-Security-Policy (CSP)。缺少对脚本、样式、图片等外部资源加载来源的限制，无法获得浏览器原生 XSS 纵深防护能力。",
		FindingRemediation: "结合业务资源加载需求，配置符合最小权限原则的 Content-Security-Policy 响应头。",
	}
}

func evaluateStrictTransportSecurity(out httpToolOutput, snapBytes []byte) RuleResult {
	const ruleID = "http_strict_transport_security"
	const ruleName = "Strict-Transport-Security (HSTS) 检查"
	const ruleVersion = "1.0.0"

	// Condition: Applicable only to HTTPS scheme (RFC 6797 §7.2)
	parsedURL, err := url.Parse(out.Request.URL)
	if err != nil || strings.ToLower(parsedURL.Scheme) != "https" {
		return RuleResult{
			CheckID:          ruleID,
			CheckName:        ruleName,
			CheckVersion:     ruleVersion,
			Status:           model.CheckStatusNotApplicable,
			Explanation:      fmt.Sprintf("Strict-Transport-Security (HSTS) 检查仅适用于 HTTPS 传输，当前请求为明文 HTTP (%s)。", out.Request.URL),
			EvidenceSnapshot: snapBytes,
		}
	}

	if net.ParseIP(parsedURL.Hostname()) != nil {
		return RuleResult{CheckID: ruleID, CheckName: ruleName, CheckVersion: ruleVersion, Status: model.CheckStatusNotApplicable,
			Explanation: "HSTS 主机策略不适用于 IP 字面量目标。", EvidenceSnapshot: snapBytes}
	}
	val, found := getHeaderCaseInsensitive(out.Response.Headers, "Strict-Transport-Security")
	if found && hasActiveHSTSMaxAge(val) {
		return RuleResult{
			CheckID:          ruleID,
			CheckName:        ruleName,
			CheckVersion:     ruleVersion,
			Status:           model.CheckStatusPass,
			Explanation:      fmt.Sprintf("HSTS 响应头包含正数 max-age 指令: %s。本规则不验证浏览器已有策略、预加载或其他指令的全部语义。", val),
			EvidenceSnapshot: snapBytes,
		}
	}

	return RuleResult{
		CheckID:            ruleID,
		CheckName:          ruleName,
		CheckVersion:       ruleVersion,
		Status:             model.CheckStatusNeedsAttention,
		Explanation:        "HTTPS 响应未声明启用状态的有效 HSTS max-age，可能缺失、格式无效或设置为 0。",
		EvidenceSnapshot:   snapBytes,
		GenerateFinding:    true,
		FindingTitle:       "[配置关注] 未启用有效的 Strict-Transport-Security 策略",
		FindingCategory:    "configuration_risk",
		FindingSeverity:    "low",
		FindingDescription: "此响应未声明启用状态的有效 HSTS 策略。这是响应配置观察，不代表已验证可利用漏洞，也未检查浏览器已有策略或预加载状态。",
		FindingRemediation: "根据业务验证 HTTPS 部署后配置正数 max-age；是否包含子域应按子域 HTTPS 支持情况决定。",
	}
}

func makeInsufficientEvidenceResult(checkID, reason string) RuleResult {
	def, err := GetCheckDefinition(checkID)
	name := checkID
	version := "1.0.0"
	if err == nil {
		name = def.Name
		version = def.Version
	}
	return RuleResult{
		CheckID:          checkID,
		CheckName:        name,
		CheckVersion:     version,
		Status:           model.CheckStatusInsufficientEvidence,
		Explanation:      reason,
		EvidenceSnapshot: json.RawMessage(`{}`),
	}
}

func makeErrorResult(checkID, reason string) RuleResult {
	def, err := GetCheckDefinition(checkID)
	name := checkID
	version := "1.0.0"
	if err == nil {
		name = def.Name
		version = def.Version
	}
	return RuleResult{
		CheckID:          checkID,
		CheckName:        name,
		CheckVersion:     version,
		Status:           model.CheckStatusError,
		Explanation:      reason,
		EvidenceSnapshot: json.RawMessage(`{}`),
	}
}

var hstsMaxAge = regexp.MustCompile(`(?i)^max-age\s*=\s*(?:"([0-9]+)"|([0-9]+))$`)

// RFC 6797 sections 6.1 and 6.1.1: zero disables policy; a substring
// containing max-age is not a valid directive. Repeated directives are invalid.
func hasActiveHSTSMaxAge(value string) bool {
	seen := map[string]bool{}
	active := false
	for _, raw := range strings.Split(value, ";") {
		directive := strings.TrimSpace(raw)
		if directive == "" {
			continue
		}
		name, _, _ := strings.Cut(directive, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if seen[name] {
			return false
		}
		seen[name] = true
		if name == "max-age" {
			matches := hstsMaxAge.FindStringSubmatch(directive)
			if len(matches) != 3 {
				return false
			}
			active = strings.TrimLeft(matches[1]+matches[2], "0") != ""
		}
		if name == "includesubdomains" && strings.Contains(directive, "=") {
			return false
		}
	}
	return active
}
