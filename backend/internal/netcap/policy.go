package netcap

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	TemplateLegacy   = "legacy"
	TemplateReadonly = "readonly"
	TemplateActive   = "active"

	defaultMaxBody = 64 * 1024
	hardMaxBody    = 1 << 20
)

// Management ports belong to this deployment. Authorizing a target origin never
// opens the API, the UI, or the database.
var managementPorts = map[string]struct{}{
	"25432": {},
	"28080": {},
	"23000": {},
}

var legacyFixturePaths = map[string]struct{}{
	"/": {}, "/login": {}, "/health": {}, "/slow": {}, "/large": {},
	"/500": {}, "/redirect": {}, "/target": {},
}

// OriginRule is one frozen origin grant. AllowPrivate is explicit; private
// addresses are not rejected globally and are not allowed by default.
type OriginRule struct {
	Scheme       string   `json:"scheme"`
	Host         string   `json:"host"`
	Port         string   `json:"port,omitempty"`
	PathMode     string   `json:"path_mode,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	AllowPrivate bool     `json:"allow_private,omitempty"`
}

func (r OriginRule) origin() string {
	return fmt.Sprintf("%s://%s", r.Scheme, r.hostPort())
}

func (r OriginRule) hostPort() string {
	port := r.Port
	if port == "" {
		if r.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(r.Host, port)
}

// Policy is the server-side authorization for one Run. It is loaded from the
// frozen execution snapshot. Tool arguments cannot change it.
type Policy struct {
	Template       string
	Origins        []OriginRule
	AllowedMethods []string
	MaxPages       int
	MaxRequests    int
	Interval       time.Duration
	MaxBodyBytes   int
	MaxRedirects   int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	FixtureCAs     []FixtureCA
	// EnforceLimits is false for historical non-discovery runs so existing
	// GET checks are not given a new implicit page cap.
	EnforceLimits bool
}

type FixtureCA struct {
	PEM     []byte
	Origins []string
}

type networkPolicyJSON struct {
	Template          string       `json:"template"`
	AllowedMethods    []string     `json:"allowed_methods"`
	Origins           []OriginRule `json:"origins"`
	MaxPages          int          `json:"max_pages"`
	MaxRequests       int          `json:"max_requests"`
	RequestIntervalMs int          `json:"request_interval_ms"`
	MaxBodyBytes      int          `json:"max_body_bytes"`
	MaxRedirects      int          `json:"max_redirects"`
	TimeoutSeconds    int          `json:"timeout_seconds"`
}

type discoveryPolicyJSON struct {
	Mode              string   `json:"mode"`
	Origin            string   `json:"origin"`
	AllowedMethods    []string `json:"allowed_methods"`
	AllowedPaths      []string `json:"allowed_paths"`
	MaxPages          int      `json:"max_pages"`
	RequestIntervalMs int      `json:"request_interval_ms"`
}

// PolicyFromExecutionInput reads the frozen snapshot. A missing network_policy
// keeps the historical HTTP GET constraints. It does not upgrade old tasks to
// HTTPS or extra methods.
func PolicyFromExecutionInput(raw []byte) (Policy, error) {
	p := Policy{
		Template:       TemplateLegacy,
		AllowedMethods: []string{"GET"},
		MaxBodyBytes:   defaultMaxBody,
		MaxRedirects:   5,
		DefaultTimeout: 5 * time.Second,
		MaxTimeout:     120 * time.Second,
	}
	if len(raw) == 0 {
		p.Origins = []OriginRule{legacyFixtureRule()}
		return p, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Policy{}, fmt.Errorf("execution input is not an object")
	}
	if np, ok := doc["network_policy"]; ok && string(np) != "null" {
		return policyFromNetwork(np)
	}
	if dp := findDiscovery(doc); dp != nil && dp.Origin != "" {
		return policyFromDiscovery(*dp), nil
	}
	p.Origins = []OriginRule{legacyFixtureRule()}
	return p, nil
}

func policyFromNetwork(raw json.RawMessage) (Policy, error) {
	var in networkPolicyJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return Policy{}, fmt.Errorf("network_policy is invalid")
	}
	template := strings.ToLower(strings.TrimSpace(in.Template))
	switch template {
	case TemplateReadonly, TemplateActive:
	default:
		return Policy{}, fmt.Errorf("network_policy.template must be readonly or active")
	}
	if len(in.Origins) == 0 {
		return Policy{}, fmt.Errorf("network_policy requires origins")
	}
	methods := normalizeMethods(in.AllowedMethods)
	if template == TemplateReadonly {
		methods = intersect(methods, []string{"GET", "HEAD"})
	}
	if len(methods) == 0 {
		return Policy{}, fmt.Errorf("network_policy allows no methods")
	}
	for i := range in.Origins {
		in.Origins[i].Scheme = strings.ToLower(in.Origins[i].Scheme)
		in.Origins[i].Host = strings.TrimSpace(in.Origins[i].Host)
		if in.Origins[i].Scheme != "http" && in.Origins[i].Scheme != "https" {
			return Policy{}, fmt.Errorf("origin scheme must be http or https")
		}
		if in.Origins[i].Host == "" || strings.Contains(in.Origins[i].Host, " ") {
			return Policy{}, fmt.Errorf("origin host is empty")
		}
		if in.Origins[i].PathMode == "" {
			if len(in.Origins[i].Paths) > 0 {
				in.Origins[i].PathMode = "exact"
			} else {
				in.Origins[i].PathMode = "same_origin"
			}
		}
	}
	body := in.MaxBodyBytes
	if body <= 0 {
		body = defaultMaxBody
	}
	if body > hardMaxBody {
		body = hardMaxBody
	}
	redirects := in.MaxRedirects
	if redirects <= 0 {
		redirects = 5
	}
	if redirects > 10 {
		redirects = 10
	}
	timeout := 30 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	maxPages := in.MaxPages
	maxRequests := in.MaxRequests
	enforce := maxPages > 0 || maxRequests > 0
	if enforce && maxPages <= 0 {
		maxPages = 20
	}
	if enforce && maxRequests <= 0 {
		maxRequests = maxPages * 4
	}
	return Policy{
		Template:       template,
		Origins:        in.Origins,
		AllowedMethods: methods,
		MaxPages:       maxPages,
		MaxRequests:    maxRequests,
		Interval:       time.Duration(in.RequestIntervalMs) * time.Millisecond,
		MaxBodyBytes:   body,
		MaxRedirects:   redirects,
		DefaultTimeout: timeout,
		MaxTimeout:     120 * time.Second,
		EnforceLimits:  enforce,
	}, nil
}

func policyFromDiscovery(dp discoveryPolicyJSON) Policy {
	u, err := url.Parse(dp.Origin)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return Policy{
			Template:       TemplateLegacy,
			AllowedMethods: []string{"GET"},
			MaxBodyBytes:   defaultMaxBody,
			MaxRedirects:   5,
			DefaultTimeout: 5 * time.Second,
			MaxTimeout:     120 * time.Second,
			Origins:        []OriginRule{legacyFixtureRule()},
		}
	}
	mode := dp.Mode
	if mode == "" {
		if len(dp.AllowedPaths) > 0 {
			mode = "exact"
		} else {
			mode = "same_origin"
		}
	}
	pathMode := "exact"
	if mode == "same_origin_html" || mode == "same_origin" {
		pathMode = "same_origin"
	}
	methods := normalizeMethods(dp.AllowedMethods)
	if len(methods) == 0 {
		methods = []string{"GET"}
	}
	// Historical discovery snapshots authorize GET. Extra methods require an
	// explicit network_policy template.
	methods = intersect(methods, []string{"GET", "HEAD"})
	if len(methods) == 0 {
		methods = []string{"GET"}
	}
	maxPages := dp.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}
	interval := dp.RequestIntervalMs
	if interval <= 0 {
		interval = 500
	}
	allowPrivate := u.Hostname() == "127.0.0.1" || net.ParseIP(u.Hostname()) != nil && isPrivateIP(net.ParseIP(u.Hostname()))
	return Policy{
		Template: TemplateLegacy,
		Origins: []OriginRule{{
			Scheme:       "http",
			Host:         u.Hostname(),
			Port:         portOrDefault(u),
			PathMode:     pathMode,
			Paths:        append([]string(nil), dp.AllowedPaths...),
			AllowPrivate: allowPrivate,
		}},
		AllowedMethods: methods,
		MaxPages:       maxPages,
		MaxRequests:    maxPages * 5,
		Interval:       time.Duration(interval) * time.Millisecond,
		MaxBodyBytes:   defaultMaxBody,
		MaxRedirects:   0,
		DefaultTimeout: 5 * time.Second,
		MaxTimeout:     5 * time.Second,
		EnforceLimits:  true,
	}
}

func legacyFixtureRule() OriginRule {
	port := os.Getenv("FIXTURE_PORT")
	if port == "" {
		port = "29090"
	}
	paths := make([]string, 0, len(legacyFixturePaths))
	for p := range legacyFixturePaths {
		paths = append(paths, p)
	}
	return OriginRule{
		Scheme:       "http",
		Host:         "127.0.0.1",
		Port:         port,
		PathMode:     "exact",
		Paths:        paths,
		AllowPrivate: true,
	}
}

func findDiscovery(doc map[string]json.RawMessage) *discoveryPolicyJSON {
	if raw, ok := doc["discovery_policy"]; ok && string(raw) != "null" {
		var dp discoveryPolicyJSON
		if json.Unmarshal(raw, &dp) == nil && dp.Origin != "" {
			return &dp
		}
	}
	if raw, ok := doc["scope"]; ok {
		var scope map[string]json.RawMessage
		if json.Unmarshal(raw, &scope) == nil {
			if inner, ok := scope["discovery_policy"]; ok {
				var dp discoveryPolicyJSON
				if json.Unmarshal(inner, &dp) == nil && dp.Origin != "" {
					return &dp
				}
			}
		}
	}
	return nil
}

func (p Policy) AllowsMethod(method string) error {
	method = strings.ToUpper(strings.TrimSpace(method))
	if !contains(p.AllowedMethods, method) {
		return fmt.Errorf("method %s is outside the frozen network policy", method)
	}
	if p.Template == TemplateReadonly && method != "GET" && method != "HEAD" {
		return fmt.Errorf("readonly template does not authorize %s", method)
	}
	if p.Template == TemplateLegacy && method != "GET" && method != "HEAD" {
		return fmt.Errorf("historical snapshot does not authorize %s", method)
	}
	return nil
}

func (p Policy) Match(u *url.URL) (OriginRule, error) {
	if u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return OriginRule{}, fmt.Errorf("url must be absolute http or https")
	}
	if u.User != nil {
		return OriginRule{}, fmt.Errorf("url must not embed credentials")
	}
	host := u.Hostname()
	port := portOrDefault(u)
	if _, denied := managementPorts[port]; denied {
		return OriginRule{}, fmt.Errorf("port %s is a management port and is not a target", port)
	}
	if ip := net.ParseIP(host); ip != nil && isMetadataIP(ip) {
		return OriginRule{}, fmt.Errorf("metadata addresses are not targets")
	}
	for _, rule := range p.Origins {
		if !strings.EqualFold(rule.Scheme, u.Scheme) || !strings.EqualFold(rule.Host, host) {
			continue
		}
		rulePort := rule.Port
		if rulePort == "" {
			rulePort = defaultPort(rule.Scheme)
		}
		if rulePort != port {
			continue
		}
		path := u.Path
		if path == "" {
			path = "/"
		}
		switch rule.PathMode {
		case "same_origin", "same_origin_html":
			return rule, nil
		default:
			for _, allowed := range rule.Paths {
				if allowed == path {
					return rule, nil
				}
			}
			return OriginRule{}, fmt.Errorf("path %s is outside the frozen origin grant", path)
		}
	}
	return OriginRule{}, fmt.Errorf("origin %s://%s is outside the frozen network policy", u.Scheme, u.Host)
}

func (p Policy) CAPool(origin string) [][]byte {
	var out [][]byte
	for _, ca := range p.FixtureCAs {
		if len(ca.Origins) == 0 {
			continue
		}
		for _, allowed := range ca.Origins {
			if allowed == origin || allowed == "*" {
				out = append(out, ca.PEM)
				break
			}
		}
	}
	return out
}

func DestinationAllowed(host, port string, rule OriginRule) error {
	if _, denied := managementPorts[port]; denied {
		return fmt.Errorf("port %s is a management port and is not a target", port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if isMetadataIP(ip) || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("address %s is not a target", ip)
	}
	if isPrivateIP(ip) && !rule.AllowPrivate {
		return fmt.Errorf("private address %s is not authorized for this origin", ip)
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func isMetadataIP(ip net.IP) bool {
	return ip != nil && ip.Equal(net.ParseIP("169.254.169.254"))
}

func portOrDefault(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return defaultPort(u.Scheme)
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func normalizeMethods(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range in {
		m = strings.ToUpper(strings.TrimSpace(m))
		switch m {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

func intersect(have, allow []string) []string {
	var out []string
	for _, m := range have {
		if contains(allow, m) {
			out = append(out, m)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func canonicalOrigin(u *url.URL) string {
	return fmt.Sprintf("%s://%s", u.Scheme, net.JoinHostPort(u.Hostname(), portOrDefault(u)))
}

func pageKey(method string, u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.ToUpper(method) + " " + canonicalOrigin(u) + path
}
