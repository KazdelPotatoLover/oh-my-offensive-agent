package contract

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

const (
	CaptureDirect       = "direct"
	CaptureTLSIntercept = "tls_intercept"
	CaptureConnectMeta  = "connect_metadata"
	CredentialRefPrefix = "ref:"
)

// HTTPRequestInput is the model-facing request. The server checks scope, method,
// origin, and redirects. A model cannot set an allow flag on this struct.
type HTTPRequestInput struct {
	AssetID        *uuid.UUID        `json:"asset_id,omitempty"`
	URL            string            `json:"url,omitempty"`
	Method         string            `json:"method"`
	Query          map[string]string `json:"query,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	BodyJSON       json.RawMessage   `json:"body_json,omitempty"`
	BodyText       string            `json:"body_text,omitempty"`
	Form           map[string]string `json:"form,omitempty"`
	CredentialRef  string            `json:"credential_ref,omitempty"`
	SessionRef     string            `json:"session_ref,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

func (in HTTPRequestInput) Validate() error {
	if in.AssetID == nil && strings.TrimSpace(in.URL) == "" {
		return invalidInput("http request requires asset_id or url")
	}
	if in.AssetID != nil && *in.AssetID == uuid.Nil {
		return invalidInput("asset_id is empty")
	}
	switch strings.ToUpper(strings.TrimSpace(in.Method)) {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return invalidInput("unsupported http method")
	}
	bodies := 0
	if len(in.BodyJSON) > 0 && string(in.BodyJSON) != "null" {
		bodies++
	}
	if in.BodyText != "" {
		bodies++
	}
	if len(in.Form) > 0 {
		bodies++
	}
	if bodies > 1 {
		return invalidInput("body_json, body_text, and form are mutually exclusive")
	}
	if in.URL != "" {
		parsed, err := url.Parse(in.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return invalidInput("url must be an absolute http or https URL")
		}
		if parsed.User != nil {
			return invalidInput("url must not embed credentials")
		}
	}
	if in.CredentialRef != "" && !IsCredentialReference(in.CredentialRef) {
		return invalidInput("credential_ref must be a ref: alias")
	}
	if in.SessionRef != "" && !IsCredentialReference(in.SessionRef) {
		return invalidInput("session_ref must be a ref: alias")
	}
	for name, value := range in.Headers {
		if isSensitiveHeader(name) && !IsCredentialReference(value) {
			return invalidInput("sensitive header values must be ref: aliases")
		}
	}
	if in.TimeoutSeconds < 0 {
		return invalidInput("timeout_seconds must be non-negative")
	}
	return nil
}

func IsCredentialReference(value string) bool {
	if !strings.HasPrefix(value, CredentialRefPrefix) {
		return false
	}
	alias := strings.TrimPrefix(value, CredentialRefPrefix)
	return alias != "" && !strings.ContainsAny(alias, " \r\n\t")
}

func isSensitiveHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "cookie", "set-cookie", "x-api-key", "proxy-authorization":
		return true
	default:
		return false
	}
}

// HTTPReceipt is minted by the capture gateway. Model JSON that looks like a
// receipt is not evidence. CaptureConnectMeta does not prove a response body.
type HTTPReceipt struct {
	RequestID          uuid.UUID      `json:"request_id"`
	ReceiptID          uuid.UUID      `json:"receipt_id"`
	AssetID            *uuid.UUID     `json:"asset_id,omitempty"`
	Method             string         `json:"method"`
	URL                string         `json:"url"`
	StatusCode         int            `json:"status_code"`
	ResponsePreview    string         `json:"response_preview"`
	RequestArtifactID  *uuid.UUID     `json:"request_artifact_id,omitempty"`
	ResponseArtifactID *uuid.UUID     `json:"response_artifact_id,omitempty"`
	Truncated          bool           `json:"truncated"`
	RequestHash        string         `json:"request_hash,omitempty"`
	ResponseHash       string         `json:"response_hash,omitempty"`
	CredentialRef      string         `json:"credential_ref,omitempty"`
	CaptureMode        string         `json:"capture_mode"`
	Scope              ExecutionScope `json:"-"`
}

func (r HTTPReceipt) Validate() error {
	if r.RequestID == uuid.Nil || r.ReceiptID == uuid.Nil {
		return invalidInput("receipt requires request_id and receipt_id")
	}
	switch r.CaptureMode {
	case CaptureDirect, CaptureTLSIntercept, CaptureConnectMeta:
	default:
		return invalidInput("unknown capture_mode")
	}
	if r.CaptureMode == CaptureConnectMeta && r.ResponseArtifactID != nil {
		return invalidInput("connect metadata is not a response body")
	}
	return nil
}

// EmpiricalSource reports whether a receipt can back an empirical finding.
// CONNECT metadata and command stdout cannot.
func (r HTTPReceipt) EmpiricalSource() bool {
	if err := r.Validate(); err != nil {
		return false
	}
	return r.CaptureMode == CaptureDirect || r.CaptureMode == CaptureTLSIntercept
}

// RejectModelMintedReceipt refuses a receipt assembled by a model or tool output.
func RejectModelMintedReceipt(_ json.RawMessage) error {
	return &Error{
		Code:            CodeUnauthorized,
		Scope:           ScopeRequest,
		Message:         "capture receipts are minted by the gateway",
		SuggestedAction: ActionNone,
	}
}

// TargetTransport is the direct scoped HTTP client used by http_request and the
// legacy get_endpoint adapter.
type TargetTransport interface {
	Do(ctx context.Context, scope ExecutionScope, in HTTPRequestInput) (*HTTPReceipt, error)
}

// CaptureGateway mints and looks up receipts. Job egress uses the same gateway.
// Setting HTTP_PROXY is not an implementation of this interface.
type CaptureGateway interface {
	Exchange(ctx context.Context, scope ExecutionScope, in HTTPRequestInput) (*HTTPReceipt, error)
	Lookup(ctx context.Context, scope ExecutionScope, receiptID uuid.UUID) (*HTTPReceipt, error)
}
