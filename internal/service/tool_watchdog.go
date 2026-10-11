package service

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wstein/workharbor/internal/agent"
)

// ToolWatchdogLimits bound one tool's output silence, never a run's duration.
// Zero selects the warning/abort defaults of 10 and 30 minutes.
type ToolWatchdogLimits struct {
	WarnAfter, AbortAfter time.Duration
}

func (l ToolWatchdogLimits) effective() (ToolWatchdogLimits, error) {
	if l.WarnAfter == 0 {
		l.WarnAfter = 10 * time.Minute
	}
	if l.AbortAfter == 0 {
		l.AbortAfter = 30 * time.Minute
	}
	if l.WarnAfter < 0 || l.AbortAfter < 0 || l.WarnAfter >= l.AbortAfter {
		return l, fmt.Errorf("tool watchdog needs positive warning < abort limits")
	}
	return l, nil
}

type toolWatchdogAction struct {
	source     *toolWatchdogState
	generation string
	toolID     string
	episode    uint64
	revision   uint64
	abort      bool
}

type watchedTool struct {
	anchor     time.Time
	silence    time.Duration
	episode    uint64
	revision   uint64
	warned     bool
	abortTried bool
	waits      map[string]bool
	seenWaits  map[string]bool
}

// State is process-local and is armed only from fresh execution observations.
// Historical transcripts, model messages and pending Decisions never feed it.
// Callers perform adapter I/O only after claim returns and outside every lock.
type toolWatchdogState struct {
	mu         sync.Mutex
	generation string
	limits     ToolWatchdogLimits
	active     bool
	tools      map[string]*watchedTool
	seen       map[string]bool
}

func newToolWatchdogState(generation string, limits ToolWatchdogLimits) (*toolWatchdogState, error) {
	if err := (agent.ToolWatchdogEvent{Generation: generation, ToolCallID: "validation"}).Validate(agent.EventToolExecutionStarted); err != nil {
		return nil, err
	}
	l, err := limits.effective()
	if err != nil {
		return nil, err
	}
	return &toolWatchdogState{generation: generation, limits: l, active: true, tools: map[string]*watchedTool{}, seen: map[string]bool{}}, nil
}

func (w *toolWatchdogState) observe(kind agent.EventKind, e *agent.ToolWatchdogEvent, now time.Time) error {
	if e == nil {
		return fmt.Errorf("tool execution observation has no watchdog payload")
	}
	if err := e.Validate(kind); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e.Generation != w.generation {
		return fmt.Errorf("tool execution observation belongs to a different generation")
	}
	if !w.active {
		return nil
	}
	if kind == agent.EventToolExecutionStarted {
		if !w.seen[e.ToolCallID] {
			w.seen[e.ToolCallID] = true
			w.tools[e.ToolCallID] = &watchedTool{anchor: now, episode: 1, waits: map[string]bool{}, seenWaits: map[string]bool{}}
		}
		return nil
	}
	if kind == agent.EventToolExecutionEnded {
		w.seen[e.ToolCallID] = true
		delete(w.tools, e.ToolCallID)
		return nil
	}
	t := w.tools[e.ToolCallID]
	if t == nil {
		return nil // Output and waits cannot invent an execution start.
	}
	switch kind {
	case agent.EventToolOutput:
		t.anchor, t.silence = now, 0
		t.episode++
		t.revision++
		t.warned, t.abortTried = false, false
	case agent.EventToolWaitStarted:
		if t.seenWaits[e.WaitID] {
			return nil
		}
		if len(t.waits) == 0 {
			t.silence = t.elapsed(now)
		}
		t.waits[e.WaitID], t.seenWaits[e.WaitID] = true, true
		t.revision++
	case agent.EventToolWaitEnded:
		t.seenWaits[e.WaitID] = true
		if !t.waits[e.WaitID] {
			return nil
		}
		delete(t.waits, e.WaitID)
		t.revision++
		if len(t.waits) == 0 {
			t.anchor = now
		}
	}
	return nil
}

func (t *watchedTool) elapsed(now time.Time) time.Duration {
	if len(t.waits) != 0 || now.Before(t.anchor) {
		return t.silence
	}
	d := now.Sub(t.anchor)
	const largest = time.Duration(1<<63 - 1)
	if d > largest-t.silence {
		return largest
	}
	return t.silence + d
}

func (w *toolWatchdogState) due(now time.Time) []toolWatchdogAction {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return nil
	}
	ids := make([]string, 0, len(w.tools))
	for id := range w.tools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []toolWatchdogAction
	for _, id := range ids {
		t := w.tools[id]
		if len(t.waits) != 0 || t.abortTried {
			continue
		}
		elapsed := t.elapsed(now)
		if elapsed >= w.limits.AbortAfter || (elapsed >= w.limits.WarnAfter && !t.warned) {
			out = append(out, toolWatchdogAction{w, w.generation, id, t.episode, t.revision, elapsed >= w.limits.AbortAfter})
		}
	}
	return out
}

// claim is the last state operation before dispatch. Output, completion, a
// human wait, disarm or another claimant invalidates the pending action.
func (w *toolWatchdogState) claim(a toolWatchdogAction, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.tools[a.toolID]
	if !w.active || a.source != w || a.generation != w.generation || t == nil || t.episode != a.episode || t.revision != a.revision || len(t.waits) != 0 || t.abortTried {
		return false
	}
	elapsed := t.elapsed(now)
	if a.abort {
		if elapsed < w.limits.AbortAfter {
			return false
		}
		t.abortTried = true
		return true
	}
	if t.warned || elapsed < w.limits.WarnAfter || elapsed >= w.limits.AbortAfter {
		return false
	}
	t.warned = true
	return true
}

func (w *toolWatchdogState) disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active = false
	w.tools = nil
}

// finished retires only the original execution; its ID remains a replay tombstone.
func (w *toolWatchdogState) finished(a toolWatchdogAction) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if a.source == w && a.generation == w.generation {
		delete(w.tools, a.toolID)
	}
}
