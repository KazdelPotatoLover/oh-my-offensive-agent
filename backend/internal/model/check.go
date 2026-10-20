package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type CheckStatus string

const (
	CheckStatusPass                 CheckStatus = "pass"
	CheckStatusNeedsAttention       CheckStatus = "needs_attention"
	CheckStatusNotApplicable        CheckStatus = "not_applicable"
	CheckStatusInsufficientEvidence CheckStatus = "insufficient_evidence"
	CheckStatusError                CheckStatus = "error"
)

func (s CheckStatus) IsValid() bool {
	switch s {
	case CheckStatusPass, CheckStatusNeedsAttention, CheckStatusNotApplicable,
		CheckStatusInsufficientEvidence, CheckStatusError:
		return true
	default:
		return false
	}
}

type FindingStatus string

const (
	FindingStatusPendingReview FindingStatus = "pending_review"
	FindingStatusConfirmed     FindingStatus = "confirmed"
	FindingStatusFalsePositive FindingStatus = "false_positive"
	FindingStatusIgnored       FindingStatus = "ignored"
)

type CheckResult struct {
	ID                  uuid.UUID       `json:"id"`
	RunID               uuid.UUID       `json:"run_id"`
	AttemptID           *uuid.UUID      `json:"attempt_id,omitempty"`
	SessionID           *uuid.UUID      `json:"session_id,omitempty"`
	WorkItemID          *uuid.UUID      `json:"work_item_id,omitempty"`
	AssetID             uuid.UUID       `json:"asset_id"`
	CheckID             string          `json:"check_id"`
	CheckName           string          `json:"check_name"`
	CheckVersion        string          `json:"check_version"`
	Status              CheckStatus     `json:"status"`
	Explanation         string          `json:"explanation"`
	SourceToolCallID    *uuid.UUID      `json:"source_tool_call_id,omitempty"`
	SourceObservationID *uuid.UUID      `json:"source_observation_id,omitempty"`
	EvidenceSnapshot    json.RawMessage `json:"evidence_snapshot"`
	Details             json.RawMessage `json:"details"`
	CreatedAt           time.Time       `json:"created_at"`
}

type Finding struct {
	ID               uuid.UUID       `json:"id"`
	RunID            uuid.UUID       `json:"run_id"`
	CheckResultID    uuid.UUID       `json:"check_result_id"`
	AssetID          uuid.UUID       `json:"asset_id"`
	Title            string          `json:"title"`
	Category         string          `json:"category"`
	Severity         string          `json:"severity"`
	Status           FindingStatus   `json:"status"`
	Description      string          `json:"description"`
	Remediation      string          `json:"remediation"`
	SourceToolCallID *uuid.UUID      `json:"source_tool_call_id,omitempty"`
	EvidenceRef      json.RawMessage `json:"evidence_ref"`
	CreatedAt        time.Time       `json:"created_at"`
}
