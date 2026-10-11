package domain

import "time"

// Watchdog notices describe a single tool's silence and confirmed cancellation.
// They never transition the run or task, whose duration is unlimited.
const (
	EventToolHung    EventKind = "tool.hung"
	EventToolAborted EventKind = "tool.aborted"
)

// ToolWatchdogNotice records original run/session/tool provenance. None of the
// tool identity or its output goes into the generic notification payload.
type ToolWatchdogNotice struct {
	RunID      ID     `json:"run_id"`
	Generation string `json:"generation"`
	ToolCallID string `json:"tool_call_id"`
	Episode    uint64 `json:"episode"`
}

// NewToolWatchdogNotice returns an audit event without a lifecycle transition.
func NewToolWatchdogNotice(task ID, kind EventKind, notice ToolWatchdogNotice, at time.Time) Event {
	return newEvent(task, kind, notice, at)
}
