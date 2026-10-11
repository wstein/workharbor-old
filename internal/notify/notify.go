// Package notify sends a push when a blocking Decision stops a task (design
// §9.4). The message is generic by design: a task ID, an event kind and a link.
// It leaves the host and may pass a public relay, and issue text is untrusted
// input, so nothing else is ever put in it. The inbox stays the source of
// truth; a push is best effort.
//
// Throttle sits in `whr serve` (serve.NewNotifier): Ntfy is wrapped in Throttled
// with one shared Throttle, and service.New puts its Async queue in front, so
// every kind of push goes through the per-task limit.
package notify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/wstein/workharbor/internal/domain"
)

// Kind says why the human is being notified.
type Kind string

const (
	KindAgentMayRun    Kind = "agent_may_run"
	KindQuestion       Kind = "question"
	KindApproval       Kind = "approval"
	KindReview         Kind = "review"
	KindAuthExpired    Kind = "auth_expired"
	KindQuotaExhausted Kind = "quota_exhausted"
	KindRunFailed      Kind = "run_failed"
	KindRunEnded       Kind = "run_ended"
	KindToolHung       Kind = "tool_hung"
	KindToolAborted    Kind = "tool_aborted"
	// KindBudgetWarning is a soft budget threshold reached; KindBudgetExceeded a
	// hard limit that ended the task as failed.
	KindBudgetWarning  Kind = "budget_warning"
	KindBudgetExceeded Kind = "budget_exceeded"
	// KindLimitLow is a provider limit the agent reported as low (issue #172).
	KindLimitLow Kind = "limit_low"
	// KindRunInterrupted is an interrupted run waiting for the human to resume
	// it; the supervisor never resumes it itself (D57, issue #356).
	KindRunInterrupted Kind = "run_interrupted"
)

// Message is everything a notification carries: IDs and a kind, nothing the
// agent or the repository wrote.
type Message struct {
	TaskID     domain.ID
	Kind       Kind
	DecisionID domain.ID // set for a Decision; part of the dedup key and the link
	RunID      domain.ID // set for a run that ended; part of the dedup key
}

// Notifier delivers a message. An implementation must not add anything to it.
type Notifier interface {
	Notify(ctx context.Context, m Message) error
}

// Link is the URL a notification opens: the task in the web UI behind the
// supervisor login (design §9.4). base is the configured address of the API
// (D29); the link holds IDs only.
func Link(base string, m Message) string {
	link := strings.TrimRight(base, "/") + "/tasks/" + string(m.TaskID)
	if m.DecisionID != "" {
		link += "?decision=" + string(m.DecisionID)
	}
	return link
}

// FromEvents picks the notifications out of the events a change recorded: a
// new blocking Decision (a question, an approval or a review; a login or quota
// question by its cause), the non-blocking agent-may-run notice, and a run
// that ended or failed. Other non-blocking Decisions do not notify.
func FromEvents(events []domain.Event) []Message {
	var out []Message
	for _, e := range events {
		switch e.Kind {
		case domain.EventToolHung, domain.EventToolAborted:
			var p domain.ToolWatchdogNotice
			if json.Unmarshal(e.Payload, &p) != nil || p.RunID == "" {
				continue
			}
			kind := KindToolHung
			if e.Kind == domain.EventToolAborted {
				kind = KindToolAborted
			}
			out = append(out, Message{TaskID: e.TaskID, Kind: kind, RunID: p.RunID})
		case domain.EventDecisionRaised:
			var p domain.DecisionRaised
			if json.Unmarshal(e.Payload, &p) != nil || (!p.Blocking && p.Cause != domain.CauseAgentMayRun) {
				continue
			}
			kind := Kind(p.Kind)
			switch p.Cause {
			case domain.CauseAgentMayRun:
				kind = KindAgentMayRun
			case domain.CauseAuthExpired:
				kind = KindAuthExpired
			case domain.CauseQuotaExhausted:
				kind = KindQuotaExhausted
			case domain.CauseRunFailed:
				kind = KindRunFailed // the retry-or-cancel question of a failed run
			}
			out = append(out, Message{TaskID: e.TaskID, Kind: kind, DecisionID: p.ID})
		case domain.EventBudgetWarned:
			out = append(out, Message{TaskID: e.TaskID, Kind: KindBudgetWarning})
		case domain.EventBudgetExceeded:
			out = append(out, Message{TaskID: e.TaskID, Kind: KindBudgetExceeded})
		case domain.EventRunState:
			var p domain.StateChanged
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if p.To == string(domain.RunStopped) {
				out = append(out, Message{TaskID: e.TaskID, Kind: KindRunEnded, RunID: p.ID})
			}
		}
	}
	return out
}

// Throttle deduplicates and rate-limits per task, so a stalled run does not
// notify repeatedly. A message equal to one sent within Window (the whole
// Message: task, kind, Decision and run) is dropped, and a task gets at most
// MaxPerWindow messages in a Window. What is dropped is still in the inbox.
type Throttle struct {
	Window       time.Duration // default 1 hour
	MaxPerWindow int           // default 5
	Now          func() time.Time

	mu   sync.Mutex
	seen map[Message]time.Time
	sent map[domain.ID][]time.Time
}

// Allow reports whether the message may be sent now, and records it if so.
func (t *Throttle) Allow(m Message) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	window, limit, now := t.Window, t.MaxPerWindow, time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	if window <= 0 {
		window = time.Hour
	}
	if limit <= 0 {
		limit = 5
	}
	if t.seen == nil {
		t.seen, t.sent = map[Message]time.Time{}, map[domain.ID][]time.Time{}
	}
	// Forget what is older than the window, so the maps stay small.
	for k, at := range t.seen {
		if now.Sub(at) >= window {
			delete(t.seen, k)
		}
	}
	for task, times := range t.sent {
		if len(times) == 0 || now.Sub(times[len(times)-1]) >= window {
			delete(t.sent, task)
		}
	}
	if at, ok := t.seen[m]; ok && now.Sub(at) < window {
		return false
	}
	recent := t.sent[m.TaskID][:0]
	for _, at := range t.sent[m.TaskID] {
		if now.Sub(at) < window {
			recent = append(recent, at)
		}
	}
	if len(recent) >= limit {
		t.sent[m.TaskID] = recent
		return false
	}
	t.seen[m] = now
	t.sent[m.TaskID] = append(recent, now)
	return true
}

// Throttled wraps a Notifier with a Throttle.
type Throttled struct {
	Next     Notifier
	Throttle *Throttle
}

// Notify sends the message unless the throttle drops it. A dropped message is
// not an error.
func (t Throttled) Notify(ctx context.Context, m Message) error {
	if !t.Throttle.Allow(m) {
		return nil
	}
	return t.Next.Notify(ctx, m)
}
