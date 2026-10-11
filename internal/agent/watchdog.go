package agent

import (
	"context"
	"fmt"
	"reflect"
	"unicode"
	"unicode/utf8"
)

// Watchdog event kinds describe actual tool execution, independently of the
// existing tool_call proposal and tool_result transcript observations.
const (
	EventToolExecutionStarted EventKind = "tool_execution_started"
	EventToolOutput           EventKind = "tool_output"
	EventToolExecutionEnded   EventKind = "tool_execution_ended"
	EventToolWaitStarted      EventKind = "tool_wait_started"
	EventToolWaitEnded        EventKind = "tool_wait_ended"
)

// MaxWatchdogIDBytes bounds opaque correlation IDs without truncating identity.
const MaxWatchdogIDBytes = 256

// ToolWatchdogEvent correlates observations within one live session generation.
// OutputBytes counts actual nonempty stdout/stderr bytes, never model messages,
// heartbeats or replay. WaitID pairs overlapping waits for human input per tool.
// IDs are nonempty valid UTF-8, without whitespace or control characters.
type ToolWatchdogEvent struct {
	Generation  string `json:"generation"`
	ToolCallID  string `json:"tool_call_id"`
	WaitID      string `json:"wait_id,omitempty"`
	OutputBytes int64  `json:"output_bytes,omitempty"`
}

// ToolAbortResult describes observed tool-only cancellation. The zero value
// proves nothing; Finished means execution already ended without this abort.
type ToolAbortResult uint8

const (
	ToolAbortUnknown ToolAbortResult = iota
	ToolAbortConfirmed
	ToolAbortFinished
)

// ToolWatchdogSession is optional. Both this interface and Capabilities.ToolWatchdog
// are required. Its generation is immutable for the attached session; tool IDs
// are unique in that generation and are never reused or reconstructed from replay.
// AbortTool must cancel only the named tool, returning Confirmed only after
// observing that cancellation. Stop, turn interruption and client shutdown do
// not implement this contract. A cancelled context or unconfirmed outcome is
// Unknown (with an error when appropriate), never an optimistic confirmation.
type ToolWatchdogSession interface {
	WatchdogGeneration() string
	AbortTool(ctx context.Context, toolCallID string) (ToolAbortResult, error)
}

// Validate checks the entire observation without changing correlation identity.
func (e ToolWatchdogEvent) Validate(kind EventKind) error {
	if !validWatchdogID(e.Generation) || !validWatchdogID(e.ToolCallID) {
		return fmt.Errorf("invalid watchdog generation or tool identity")
	}
	switch kind {
	case EventToolExecutionStarted, EventToolExecutionEnded:
		if e.WaitID != "" || e.OutputBytes != 0 {
			return fmt.Errorf("execution lifecycle event contains wait or output data")
		}
	case EventToolOutput:
		if e.WaitID != "" || e.OutputBytes <= 0 {
			return fmt.Errorf("tool output needs a positive byte count and no wait identity")
		}
	case EventToolWaitStarted, EventToolWaitEnded:
		if !validWatchdogID(e.WaitID) || e.OutputBytes != 0 {
			return fmt.Errorf("human wait needs a valid identity and no output data")
		}
	default:
		return fmt.Errorf("event is not a tool execution observation")
	}
	return nil
}

func validWatchdogID(id string) bool {
	if len(id) == 0 || len(id) > MaxWatchdogIDBytes || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// WatchdogSession selects verified optional coverage, never a session fallback.
func WatchdogSession(c Capabilities, s Session) (ToolWatchdogSession, error) {
	w, ok := s.(ToolWatchdogSession)
	if !c.ToolWatchdog || !ok {
		return nil, fmt.Errorf("%w: tool watchdog needs both capability and session interface", ErrUnsupported)
	}
	switch value := reflect.ValueOf(w); value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Chan, reflect.Func, reflect.Map, reflect.Slice:
		if value.IsNil() {
			return nil, fmt.Errorf("%w: nil tool watchdog session", ErrUnsupported)
		}
	}
	if !validWatchdogID(w.WatchdogGeneration()) {
		return nil, fmt.Errorf("%w: invalid tool watchdog generation", ErrUnsupported)
	}
	return w, nil
}
