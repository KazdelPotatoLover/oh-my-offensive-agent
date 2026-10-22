package controlplane

import (
	"regexp"
	"strings"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// ClassifiedCommand is an unambiguous human imperative. A question that merely
// contains a control word is not a command. Budget changes are recognized
// separately and are not executed here.
type ClassifiedCommand struct {
	Type        contract.CommandType
	WorkItemID  *uuid.UUID
	Instruction string
	Reason      string
	Text        string
}

var (
	uuidPattern = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	killRe      = regexp.MustCompile(`(?i)(?:终止工作项|杀掉工作项|kill work(?: item)?)\s*[:：]?\s*(` + uuidPattern + `)`)
	steerRe     = regexp.MustCompile(`(?i)(?:纠偏工作项|指导工作项|steer work(?: item)?)\s*[:：]?\s*(` + uuidPattern + `)\s*[:：]\s*(.+)$`)
	retryRe     = regexp.MustCompile(`(?i)(?:重试工作项|retry work(?: item)?)\s*[:：]?\s*(` + uuidPattern + `)`)
	continueRe  = regexp.MustCompile(`(?i)(?:继续执行工作项|continue work(?: item)?)\s*[:：]?\s*(` + uuidPattern + `)`)
	hintRe      = regexp.MustCompile(`(?i)(?:添加提示|记录提示|add a hint)\s*[:：]\s*(.+)$`)
	intentRe    = regexp.MustCompile(`(?i)(?:添加方向|add intent)\s*[:：]\s*(.+)$`)
	budgetRe    = regexp.MustCompile(`(?i)把总预算|增加预算|grant budget|additional_model_calls`)
)

// ExplicitControlRequested reports whether the human asked for a control action
// this agent is allowed to perform. A budget phrase is not included: Main Agent
// cannot grant budget.
func ExplicitControlRequested(message string) bool {
	_, ok := ClassifyCommand(message)
	return ok
}

// BudgetChangeRequested reports a human ask to change the run budget. The
// caller must use the structured grant path and must not Apply grant_budget
// from the main conversation.
func BudgetChangeRequested(message string) bool {
	if isQuestion(message) {
		return false
	}
	return budgetRe.MatchString(message)
}

// ClassifyCommand returns one unambiguous command. It does not treat a model
// sentence as an instruction.
func ClassifyCommand(message string) (ClassifiedCommand, bool) {
	text := strings.TrimSpace(message)
	if text == "" || isQuestion(text) {
		return ClassifiedCommand{}, false
	}
	if m := steerRe.FindStringSubmatch(text); len(m) == 3 {
		id, err := uuid.Parse(m[1])
		if err == nil && strings.TrimSpace(m[2]) != "" {
			return ClassifiedCommand{Type: contract.CmdSteerWork, WorkItemID: &id, Instruction: strings.TrimSpace(m[2]), Reason: text}, true
		}
	}
	if m := killRe.FindStringSubmatch(text); len(m) == 2 {
		id, err := uuid.Parse(m[1])
		if err == nil {
			return ClassifiedCommand{Type: contract.CmdKillWork, WorkItemID: &id, Reason: "explicit user command", Text: text}, true
		}
	}
	if m := retryRe.FindStringSubmatch(text); len(m) == 2 {
		id, err := uuid.Parse(m[1])
		if err == nil {
			return ClassifiedCommand{Type: contract.CmdRetryWork, WorkItemID: &id, Instruction: text}, true
		}
	}
	if m := continueRe.FindStringSubmatch(text); len(m) == 2 {
		id, err := uuid.Parse(m[1])
		if err == nil {
			return ClassifiedCommand{Type: contract.CmdContinueWork, WorkItemID: &id, Instruction: text}, true
		}
	}
	if m := hintRe.FindStringSubmatch(text); len(m) == 2 && strings.TrimSpace(m[1]) != "" {
		return ClassifiedCommand{Type: contract.CmdHint, Text: strings.TrimSpace(m[1])}, true
	}
	if m := intentRe.FindStringSubmatch(text); len(m) == 2 && strings.TrimSpace(m[1]) != "" {
		return ClassifiedCommand{Type: contract.CmdAddIntent, Text: strings.TrimSpace(m[1])}, true
	}
	lower := strings.ToLower(text)
	switch {
	case text == "暂停" || lower == "pause" || strings.Contains(text, "暂停这个任务") || strings.Contains(text, "暂停运行") || strings.Contains(lower, "pause the run") || strings.Contains(lower, "pause this run"):
		return ClassifiedCommand{Type: contract.CmdPauseRun, Reason: text}, true
	case strings.Contains(text, "继续这个任务") || strings.Contains(lower, "resume the run") || strings.Contains(lower, "resume this run"):
		return ClassifiedCommand{Type: contract.CmdResumeRun, Reason: text}, true
	case strings.Contains(text, "停止这个任务") || strings.Contains(text, "停止运行") || strings.Contains(lower, "stop the run") || strings.Contains(lower, "stop this run") || text == "停止" || lower == "stop":
		return ClassifiedCommand{Type: contract.CmdStopRun, Reason: text}, true
	case strings.Contains(text, "开启新纪元") || strings.Contains(lower, "start a new epoch") || strings.Contains(lower, "start epoch") || strings.Contains(text, "恢复这个已停止"):
		return ClassifiedCommand{Type: contract.CmdStartEpoch, Reason: text}, true
	default:
		return ClassifiedCommand{}, false
	}
}

func isQuestion(message string) bool {
	text := strings.TrimSpace(message)
	if text == "" {
		return false
	}
	if strings.HasSuffix(text, "?") || strings.HasSuffix(text, "？") {
		return true
	}
	lower := strings.ToLower(text)
	markers := []string{"为什么", "为何", "是否", "怎么", "如何", "what ", "why ", "how ", "did the", "is the"}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
