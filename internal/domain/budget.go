package domain

import (
	"fmt"
	"time"
)

// Audit entries of the resource budgets (design §5.7, §7.4). A soft threshold
// warns and a hard limit ends the task as failed (D13, D21); both are kept
// forever, so the record says what ran out and when.
const (
	EventBudgetWarned   EventKind = "budget.warned"
	EventBudgetExceeded EventKind = "budget.exceeded"
)

// BudgetScope says what a budget is counted over.
type BudgetScope string

// The scopes of a budget.
const (
	BudgetRun  BudgetScope = "run"
	BudgetTask BudgetScope = "task"
)

// BudgetMetric says what a budget counts.
type BudgetMetric string

// The metrics of a budget: every token the agent reported for the turns (input,
// output, cache read and cache write), and the cost the agent reported.
const (
	BudgetTokens   BudgetMetric = "tokens"
	BudgetDuration BudgetMetric = "duration" // legacy audit metric; never enforced
	BudgetCost     BudgetMetric = "cost"     // millionths of a US dollar
)

// BudgetBreach is a budget reached: the payload of both budget events. A turn
// whose tokens or cost the agent did not report adds nothing to Used, which is
// therefore at least what was spent, never more.
type BudgetBreach struct {
	Scope  BudgetScope  `json:"scope"`
	Metric BudgetMetric `json:"metric"`
	RunID  ID           `json:"run_id,omitempty"` // the run of a run budget
	Limit  int64        `json:"limit"`
	Used   int64        `json:"used"`
}

// NewBudgetWarned returns the audit entry of a soft threshold reached.
func NewBudgetWarned(task ID, b BudgetBreach, at time.Time) Event {
	return newEvent(task, EventBudgetWarned, b, at)
}

// ExceedBudget ends the task as failed because a hard limit was reached, which
// is one of the two ways a task fails (design §4.1, D21). Every unfinished run is
// stopped, the open Decisions are superseded, and the audit entry says which
// limit was reached. It is refused in a state the task cannot fail from.
func (a *TaskAggregate) ExceedBudget(b BudgetBreach) error {
	if b.Metric == BudgetDuration {
		return fmt.Errorf("duration budgets are retired: a run has no time limit")
	}
	if !a.task.State.CanTransition(TaskFailed) {
		return a.task.transition(TaskFailed) // reports the illegal transition
	}
	for _, run := range a.runs {
		if !run.State.Terminal() {
			run.TerminalReason = "budget_breach"
			if err := a.moveRun(run, RunStopped); err != nil {
				return err
			}
		}
	}
	for _, d := range a.decisions {
		if d.Status == DecisionOpen && d.Cause != CauseAgentMayRun && d.move(DecisionSuperseded, time.Time{}) == nil {
			a.absorb(d)
		}
	}
	a.record(EventBudgetExceeded, b)
	return a.moveTask(TaskFailed)
}

// EventKillAll is the audit entry of `whr kill-all` (design §7.7). It belongs to
// no task, so it is kept in the supervisor's own stream.
const EventKillAll EventKind = "supervisor.kill_all"

// SupervisorStream is the key of the supervisor's own audit entries in the
// event log, next to the task IDs and the workspace streams.
const SupervisorStream ID = "supervisor"

// KillAll is the payload of EventKillAll: who pulled the switch, which tasks
// were cancelled, how many tokens were revoked and what could not be done.
type KillAll struct {
	Actor         string   `json:"actor"`
	Cancelled     []ID     `json:"cancelled"`
	TokensRevoked int      `json:"tokens_revoked"`
	Problems      []string `json:"problems,omitempty"`
}

// NewKillAllEvent returns the audit entry of a kill-all.
func NewKillAllEvent(k KillAll, at time.Time) Event {
	if k.Cancelled == nil {
		k.Cancelled = []ID{}
	}
	return newEvent(SupervisorStream, EventKillAll, k, at)
}

// EventTokensRevoked is the audit entry of revoking the forge tokens without
// stopping anything else: a secret operation of the web UI behind a passkey
// step-up (D45, issue #107). Like EventKillAll it is kept in the supervisor's own
// stream.
const EventTokensRevoked EventKind = "supervisor.tokens_revoked"

// TokensRevoked is the payload of EventTokensRevoked: who did it, how many tokens
// were revoked and what could not be done. It holds no token.
type TokensRevoked struct {
	Actor   string `json:"actor"`
	Revoked int    `json:"revoked"`
	Problem string `json:"problem,omitempty"`
}

// NewTokensRevokedEvent returns the audit entry of a token revocation.
func NewTokensRevokedEvent(r TokensRevoked, at time.Time) Event {
	return newEvent(SupervisorStream, EventTokensRevoked, r, at)
}

// Events of the preview proxy (D33, issue #72). A preview shows agent-written code
// in the human's browser, so opening and closing one are audit entries in the
// supervisor's own stream. They hold no token.
const (
	EventPreviewOpened EventKind = "supervisor.preview_opened"
	EventPreviewClosed EventKind = "supervisor.preview_closed"
)

// PreviewChange is the payload of both: who, which task and port, and for a close
// why.
type PreviewChange struct {
	Preview string `json:"preview"`
	Task    string `json:"task"`
	Port    int    `json:"port"`
	Actor   string `json:"actor,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// NewPreviewEvent returns the audit entry of a preview opening or closing.
func NewPreviewEvent(kind EventKind, c PreviewChange, at time.Time) Event {
	return newEvent(SupervisorStream, kind, c, at)
}
