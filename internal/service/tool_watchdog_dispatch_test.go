package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/initiation"
)

type watchdogDispatchAdapter struct {
	agent.Adapter
	caps agent.Capabilities
}

func (a watchdogDispatchAdapter) Capabilities() agent.Capabilities { return a.caps }

type watchdogDispatchSession struct {
	mu         sync.Mutex
	generation string
	result     agent.ToolAbortResult
	err        error
	calls      []string
	stops      int
	abortHook  func()
	release    <-chan struct{}
	entered    chan string
	events     chan agent.Event
	done       chan struct{}
	once       sync.Once
}

func (s *watchdogDispatchSession) ID() string                 { return "session" }
func (s *watchdogDispatchSession) Events() <-chan agent.Event { return s.events }
func (s *watchdogDispatchSession) Instruct(context.Context, string) (agent.Delivery, error) {
	return "", agent.ErrUnsupported
}
func (s *watchdogDispatchSession) close() { s.once.Do(func() { close(s.events); close(s.done) }) }
func (s *watchdogDispatchSession) Stop(context.Context) error {
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
	s.close()
	return nil
}

func (s *watchdogDispatchSession) Wait() (agent.Result, error) {
	<-s.done
	return agent.Result{Status: agent.ResultStopped}, nil
}

func (s *watchdogDispatchSession) WatchdogGeneration() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

func (s *watchdogDispatchSession) AbortTool(_ context.Context, id string) (agent.ToolAbortResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, id)
	result, err, hook, release := s.result, s.err, s.abortHook, s.release
	s.mu.Unlock()
	s.entered <- id
	if hook != nil {
		hook()
	}
	if release != nil {
		<-release
	}
	return result, err
}

func (s *watchdogDispatchSession) counts() (calls, stops int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls), s.stops
}

type watchdogDispatchRig struct {
	r       *rig
	clock   *budgetTestClock
	sess    *watchdogDispatchSession
	sl      *slot
	monitor *runToolWatchdog
}

func newWatchdogDispatchRig(t *testing.T, capability, optionalInterface bool, beforeAttach ...func(*rig)) *watchdogDispatchRig {
	t.Helper()
	r := newRig(t)
	for _, prepare := range beforeAttach {
		prepare(r)
	}
	clock := &budgetTestClock{}
	clock.nanos.Store(t0.UnixNano())
	r.svc.clock = clock
	caps := r.agent.Capabilities()
	caps.ToolWatchdog = capability
	r.svc.ag = initiation.New(watchdogDispatchAdapter{r.agent, caps}, r.store)
	sess := &watchdogDispatchSession{generation: "generation", result: agent.ToolAbortConfirmed, entered: make(chan string, 8), events: make(chan agent.Event, 8), done: make(chan struct{})}
	var attached agent.Session = sess
	if !optionalInterface {
		attached = &struct{ agent.Session }{Session: sess}
	}
	sl := mustBegin(t, r.svc)
	r.svc.attach("t1", "r1", sl, attached)
	r.svc.mu.Lock()
	monitor := sl.watchdog
	r.svc.mu.Unlock()
	t.Cleanup(sess.close)
	return &watchdogDispatchRig{r, clock, sess, sl, monitor}
}

func (r *watchdogDispatchRig) advance(d time.Duration) { r.clock.nanos.Add(int64(d)) }
func (r *watchdogDispatchRig) observe(t *testing.T, kind agent.EventKind) {
	t.Helper()
	if r.monitor == nil {
		t.Fatal("missing supported watchdog")
	}
	e := &agent.ToolWatchdogEvent{Generation: "generation", ToolCallID: "tool"}
	if kind == agent.EventToolOutput {
		e.OutputBytes = 1
	}
	if kind == agent.EventToolWaitStarted || kind == agent.EventToolWaitEnded {
		e.WaitID = "human"
	}
	if err := r.monitor.state.observe(kind, e, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func watchdogNoticeRows(t *testing.T, r *rig, kind domain.EventKind) []domain.Event {
	t.Helper()
	events, err := r.store.EventsOfKind(bg, "t1", kind)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestWatchdogAttachedLiveEventsUseReceiptClock(t *testing.T) {
	r := newWatchdogDispatchRig(t, true, true)
	r.sess.events <- agent.Event{Kind: agent.EventToolExecutionStarted, At: t0.Add(-48 * time.Hour), Watchdog: &agent.ToolWatchdogEvent{Generation: "generation", ToolCallID: "tool"}}
	pollUntil(t, func() bool {
		r.monitor.state.mu.Lock()
		defer r.monitor.state.mu.Unlock()
		return len(r.monitor.state.tools) == 1
	})
	r.advance(10*time.Minute - time.Nanosecond)
	r.r.svc.checkToolWatchdogs(bg)
	if len(watchdogNoticeRows(t, r.r, domain.EventToolHung)) != 0 {
		t.Fatal("vendor timestamp caused early warning")
	}
	r.advance(time.Nanosecond)
	r.r.svc.checkToolWatchdogs(bg)
	pollUntil(t, func() bool { return len(watchdogNoticeRows(t, r.r, domain.EventToolHung)) == 1 })
	r.advance(20 * time.Minute)
	r.r.svc.checkToolWatchdogs(bg)
	pollUntil(t, func() bool { return len(watchdogNoticeRows(t, r.r, domain.EventToolAborted)) == 1 })
	if calls, stops := r.sess.counts(); calls != 1 || stops != 0 {
		t.Fatalf("calls=%d session stops=%d", calls, stops)
	}
	run, _ := r.r.load().Run("r1")
	if run.State != domain.RunRunning || run.TerminalReason != "" {
		t.Fatal("tool abort terminated run")
	}
}

func TestWatchdogAbortOutcomesAreTruthfulAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  agent.ToolAbortResult
		err     error
		notices int
	}{
		{"confirmed", agent.ToolAbortConfirmed, nil, 1},
		{"unknown", agent.ToolAbortUnknown, nil, 0},
		{"already finished", agent.ToolAbortFinished, nil, 0},
		{"error", agent.ToolAbortUnknown, errors.New("fixture failure"), 0},
		{"invalid", agent.ToolAbortResult(99), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWatchdogDispatchRig(t, true, true)
			r.sess.result, r.sess.err = tc.result, tc.err
			r.observe(t, agent.EventToolExecutionStarted)
			r.advance(30 * time.Minute)
			a := r.monitor.state.due(r.clock.Now())
			if len(a) != 1 {
				t.Fatal("missing abort")
			}
			r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, a[0])
			r.advance(time.Hour)
			r.r.svc.checkToolWatchdogs(bg)
			if calls, stops := r.sess.counts(); calls != 1 || stops != 0 {
				t.Fatalf("calls=%d session stops=%d", calls, stops)
			}
			if n := len(watchdogNoticeRows(t, r.r, domain.EventToolAborted)); n != tc.notices {
				t.Fatalf("abort notices=%d", n)
			}
			if len(watchdogNoticeRows(t, r.r, domain.EventToolHung)) != 0 {
				t.Fatal("late abort emitted stale warning")
			}
		})
	}
}

func TestWatchdogDispatchRevalidatesOwnershipGenerationAndProgress(t *testing.T) {
	for _, change := range []string{"slot", "generation", "output", "wait", "end", "terminal"} {
		t.Run(change, func(t *testing.T) {
			r := newWatchdogDispatchRig(t, true, true)
			r.observe(t, agent.EventToolExecutionStarted)
			r.advance(30 * time.Minute)
			a := r.monitor.state.due(r.clock.Now())
			if len(a) != 1 {
				t.Fatal("missing abort")
			}
			switch change {
			case "slot":
				r.r.svc.mu.Lock()
				r.r.svc.sessions["r1"] = &slot{}
				r.r.svc.mu.Unlock()
			case "generation":
				r.sess.mu.Lock()
				r.sess.generation = "successor"
				r.sess.mu.Unlock()
			case "output":
				r.observe(t, agent.EventToolOutput)
			case "wait":
				r.observe(t, agent.EventToolWaitStarted)
			case "end":
				r.observe(t, agent.EventToolExecutionEnded)
			case "terminal":
				must(t, r.r.svc.update(bg, "t1", func(a *domain.TaskAggregate) error { return a.StopRun("r1") }))
			}
			r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, a[0])
			if calls, stops := r.sess.counts(); calls != 0 || stops != 0 {
				t.Fatal("stale dispatch called tool or session stop")
			}
			if len(watchdogNoticeRows(t, r.r, domain.EventToolAborted)) != 0 {
				t.Fatal("stale dispatch claimed abort")
			}
		})
	}
}

func TestWatchdogAbortIOIsOutsideLocksAndOutcomeKeepsOriginalIdentity(t *testing.T) {
	r := newWatchdogDispatchRig(t, true, true)
	r.observe(t, agent.EventToolExecutionStarted)
	r.advance(30 * time.Minute)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	r.sess.release = gate
	r.sess.abortHook = func() {
		// Reentrant observations and ownership access would deadlock under either lock.
		r.r.svc.mu.Lock()
		owned := r.r.svc.sessions["r1"] == r.sl
		r.r.svc.mu.Unlock()
		if !owned {
			t.Error("ownership changed before callback")
		}
		r.observe(t, agent.EventToolOutput)
	}
	a := r.monitor.state.due(r.clock.Now())
	if len(a) != 1 {
		t.Fatal("missing abort")
	}
	done := make(chan struct{})
	go func() { defer close(done); r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, a[0]) }()
	select {
	case <-r.sess.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not begin")
	}
	must(t, r.r.svc.update(bg, "t1", func(a *domain.TaskAggregate) error {
		if err := a.StopRun("r1"); err != nil {
			return err
		}
		return a.StartRun(domain.Run{ID: "r2", EnvID: r.r.env})
	}))
	successor := &slot{}
	r.r.svc.mu.Lock()
	r.r.svc.sessions["r2"] = successor
	r.r.svc.mu.Unlock()
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abort I/O held a state/ownership lock")
	}
	rows := watchdogNoticeRows(t, r.r, domain.EventToolAborted)
	if len(rows) != 1 {
		t.Fatalf("confirmed notices=%d", len(rows))
	}
	var notice domain.ToolWatchdogNotice
	if err := json.Unmarshal(rows[0].Payload, &notice); err != nil {
		t.Fatal(err)
	}
	if notice.RunID != "r1" || notice.Generation != "generation" || notice.ToolCallID != "tool" || notice.Episode != 1 {
		t.Fatalf("callback provenance=%+v", notice)
	}
	r.r.svc.mu.Lock()
	kept := r.r.svc.sessions["r2"] == successor
	delete(r.r.svc.sessions, "r2")
	r.r.svc.mu.Unlock()
	if !kept {
		t.Fatal("old abort changed successor slot")
	}
	run, _ := r.r.load().Run("r2")
	if run.State.Terminal() {
		t.Fatal("old abort ended successor run")
	}
	if _, stops := r.sess.counts(); stops != 0 {
		t.Fatal("tool watchdog stopped entire session")
	}
}

func TestWatchdogUnsupportedAndMismatchedAdaptersNeverFallBack(t *testing.T) {
	for _, tc := range []struct{ caps, iface bool }{{false, false}, {false, true}, {true, false}} {
		r := newWatchdogDispatchRig(t, tc.caps, tc.iface)
		if r.monitor != nil {
			t.Fatal("unsupported coverage claimed")
		}
		r.r.svc.observeToolWatchdog(r.sl, agent.Event{Kind: agent.EventToolExecutionStarted, Watchdog: &agent.ToolWatchdogEvent{Generation: "generation", ToolCallID: "tool"}})
		r.advance(48 * time.Hour)
		r.r.svc.checkToolWatchdogs(bg)
		if calls, stops := r.sess.counts(); calls != 0 || stops != 0 {
			t.Fatal("unsupported adapter used session-silence fallback")
		}
	}
}

func TestWatchdogLifecycleDisarmsBeforeDispatch(t *testing.T) {
	for _, kind := range []agent.EventKind{agent.EventAuthExpired, agent.EventQuotaExhausted} {
		t.Run(string(kind), func(t *testing.T) {
			r := newWatchdogDispatchRig(t, true, true)
			r.observe(t, agent.EventToolExecutionStarted)
			r.advance(30 * time.Minute)
			action := r.monitor.state.due(r.clock.Now())[0]
			r.r.svc.observeToolWatchdog(r.sl, agent.Event{Kind: kind})
			r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, action)
			if calls, stops := r.sess.counts(); calls != 0 || stops != 0 {
				t.Fatalf("disarmed dispatch: %d/%d", calls, stops)
			}
			if r.monitor.ctx.Err() == nil {
				t.Fatal("monitor context still active")
			}
		})
	}
}

func TestWatchdogUnrelatedBlockingDecisionDoesNotFreezeTool(t *testing.T) {
	r := newWatchdogDispatchRig(t, true, true)
	r.observe(t, agent.EventToolExecutionStarted)
	must(t, r.r.svc.update(bg, "t1", func(a *domain.TaskAggregate) error {
		_, err := a.RaiseDecision(domain.NewDecision{ID: "unrelated", RunID: "r1", Kind: domain.DecisionQuestion, Blocking: true, Subject: "question", Now: r.clock.Now()})
		return err
	}))
	r.advance(30 * time.Minute)
	actions := r.monitor.state.due(r.clock.Now())
	if len(actions) != 1 {
		t.Fatal("unrelated decision froze tool")
	}
	r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, actions[0])
	if calls, _ := r.sess.counts(); calls != 1 {
		t.Fatal("tool abort did not dispatch")
	}
}

func TestWatchdogConcurrentSchedulerChecksDispatchOnce(t *testing.T) {
	r := newWatchdogDispatchRig(t, true, true)
	r.observe(t, agent.EventToolExecutionStarted)
	r.advance(30 * time.Minute)
	var checks sync.WaitGroup
	for range 16 {
		checks.Add(1)
		go func() { defer checks.Done(); r.r.svc.checkToolWatchdogs(bg) }()
	}
	checks.Wait()
	pollUntil(t, func() bool { return len(watchdogNoticeRows(t, r.r, domain.EventToolAborted)) == 1 })
	if calls, stops := r.sess.counts(); calls != 1 || stops != 0 {
		t.Fatalf("concurrent dispatch: %d/%d", calls, stops)
	}
}

func TestWatchdogStaleGenerationCannotDisarmReplacementMonitor(t *testing.T) {
	r := newWatchdogDispatchRig(t, true, true)
	r.observe(t, agent.EventToolExecutionStarted)
	r.advance(30 * time.Minute)
	action := r.monitor.state.due(r.clock.Now())[0]
	successorState, err := newToolWatchdogState("successor", ToolWatchdogLimits{})
	must(t, err)
	successorCtx, cancel := context.WithCancel(bg)
	defer cancel()
	successor := &runToolWatchdog{state: successorState, ctx: successorCtx, cancel: cancel}
	r.sess.mu.Lock()
	r.sess.generation = "successor"
	r.sess.mu.Unlock()
	r.r.svc.mu.Lock()
	r.sl.watchdog = successor
	r.r.svc.mu.Unlock()
	defer func() { r.r.svc.mu.Lock(); r.sl.watchdog = r.monitor; r.r.svc.mu.Unlock() }()
	r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, action)
	if successorCtx.Err() != nil {
		t.Fatal("old generation cancelled successor monitor")
	}
	successorState.mu.Lock()
	active := successorState.active
	successorState.mu.Unlock()
	if !active {
		t.Fatal("old generation disarmed successor state")
	}
}

func TestWatchdogDelayedInterruptedPublicationCannotDisarmResumedRun(t *testing.T) {
	var delayed []domain.Event
	r := newWatchdogDispatchRig(t, true, true, func(r *rig) {
		agg, err := r.store.LoadTask(bg, "t1")
		must(t, err)
		must(t, agg.Interrupt("r1"))
		delayed, err = r.store.SaveTask(bg, agg)
		must(t, err)
		// Human resume sees the durable interruption before its publisher runs.
		agg, err = r.store.LoadTask(bg, "t1")
		must(t, err)
		must(t, agg.Resume("r1"))
		must(t, agg.MarkRunning("r1"))
		_, err = r.store.SaveTask(bg, agg)
		must(t, err)
	})
	r.observe(t, agent.EventToolExecutionStarted)
	r.r.svc.publish(delayed)
	if r.monitor.ctx.Err() != nil {
		t.Fatal("delayed old interruption cancelled resumed monitor")
	}
	r.advance(30 * time.Minute)
	actions := r.monitor.state.due(r.clock.Now())
	if len(actions) != 1 {
		t.Fatal("resumed tool lost silence observation")
	}
	r.r.svc.dispatchToolWatchdog(bg, r.sl, r.monitor, actions[0])
	if calls, _ := r.sess.counts(); calls != 1 {
		t.Fatal("resumed tool watchdog did not dispatch")
	}
	// A later interruption genuinely belongs to this attachment and must disarm.
	must(t, r.r.svc.update(bg, "t1", func(a *domain.TaskAggregate) error { return a.Interrupt("r1") }))
	if r.monitor.ctx.Err() == nil {
		t.Fatal("current interruption left monitor active")
	}
}
