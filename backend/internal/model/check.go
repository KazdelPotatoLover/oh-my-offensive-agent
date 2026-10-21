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

func (s FindingStatus) IsValid() bool {
	switch s {
	case FindingStatusPendingReview, FindingStatusConfirmed, FindingStatusFalsePositive, FindingStatusIgnored:
		return true
	default:
		return false
	}
}

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
	ID                uuid.UUID       `json:"id"`
	RunID             uuid.UUID       `json:"run_id"`
	CheckResultID     uuid.UUID       `json:"check_result_id"`
	AssetID           uuid.UUID       `json:"asset_id"`
	Title             string          `json:"title"`
	Category          string          `json:"category"`
	Severity          string          `json:"severity"`
	Status            FindingStatus   `json:"status"`
	Description       string          `json:"description"`
	Remediation       string          `json:"remediation"`
	SourceToolCallID  *uuid.UUID      `json:"source_tool_call_id,omitempty"`
	EvidenceRef       json.RawMessage `json:"evidence_ref"`
	CreatedAt         time.Time       `json:"created_at"`
	ReviewVersion     int             `json:"review_version"`
	LastReviewedAt    *time.Time      `json:"last_reviewed_at,omitempty"`
	LastReviewer      *string         `json:"last_reviewer,omitempty"`
	LastReviewComment *string         `json:"last_review_comment,omitempty"`
}

type FindingReview struct {
	ID          uuid.UUID     `json:"id"`
	FindingID   uuid.UUID     `json:"finding_id"`
	RunID       uuid.UUID     `json:"run_id"`
	FromStatus  FindingStatus `json:"from_status"`
	ToStatus    FindingStatus `json:"to_status"`
	FromVersion int           `json:"from_version"`
	ToVersion   int           `json:"to_version"`
	Reviewer    string        `json:"reviewer"`
	Comment     string        `json:"comment"`
	RequestID   uuid.UUID     `json:"request_id"`
	CreatedAt   time.Time     `json:"created_at"`
}

type FindingReportSummary struct {
	Total         int `json:"total"`
	PendingReview int `json:"pending_review"`
	Confirmed     int `json:"confirmed"`
	FalsePositive int `json:"false_positive"`
	Ignored       int `json:"ignored"`
}

type FindingReportItem struct {
	Finding     Finding           `json:"finding"`
	CheckResult CheckResultHeader `json:"check_result"`
}

type CheckResultHeader struct {
	ID               uuid.UUID       `json:"id"`
	CheckID          string          `json:"check_id"`
	CheckName        string          `json:"check_name"`
	CheckVersion     string          `json:"check_version"`
	Status           CheckStatus     `json:"status"`
	Explanation      string          `json:"explanation"`
	SourceToolCallID *uuid.UUID      `json:"source_tool_call_id,omitempty"`
	EvidenceSnapshot json.RawMessage `json:"evidence_snapshot,omitempty"`
}

type FindingReport struct {
	RunID       uuid.UUID            `json:"run_id"`
	GeneratedAt time.Time            `json:"generated_at"`
	Summary     FindingReportSummary `json:"summary"`
	Findings    []FindingReportItem  `json:"findings"`
}
