package service

import (
	"sync"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/agent"
)

func watchdogStateFixture(t *testing.T, limits ToolWatchdogLimits) (*toolWatchdogState, *fakeClock) {
	t.Helper()
	w, err := newToolWatchdogState("generation", limits)
	if err != nil {
		t.Fatal(err)
	}
	return w, &fakeClock{now: t0}
}

func observeTool(t *testing.T, w *toolWatchdogState, clock *fakeClock, kind agent.EventKind, tool, wait string) {
	t.Helper()
	e := &agent.ToolWatchdogEvent{Generation: "generation", ToolCallID: tool, WaitID: wait}
	if kind == agent.EventToolOutput {
		e.OutputBytes = 1
	}
	if err := w.observe(kind, e, clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestToolWatchdogThresholds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    time.Duration
		want  int
		abort bool
	}{
		{"before warning", 10*time.Minute - time.Nanosecond, 0, false},
		{"warning boundary", 10 * time.Minute, 1, false},
		{"before abort", 30*time.Minute - time.Nanosecond, 1, false},
		{"abort boundary", 30 * time.Minute, 1, true},
		{"late first check", time.Hour, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, c := watchdogStateFixture(t, ToolWatchdogLimits{})
			observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
			c.now = c.now.Add(tc.at)
			a := w.due(c.Now())
			if len(a) != tc.want || (len(a) > 0 && a[0].abort != tc.abort) {
				t.Fatalf("actions=%+v", a)
			}
			if len(a) > 0 {
				if !w.claim(a[0], c.Now()) || w.claim(a[0], c.Now()) {
					t.Fatal("claim not single-use")
				}
				if len(w.due(c.Now())) != 0 {
					t.Fatal("repeated warning/abort in same silence episode")
				}
			}
		})
	}
}

func TestToolWatchdogConfiguredLimitsAndContinuousOutput(t *testing.T) {
	w, c := watchdogStateFixture(t, ToolWatchdogLimits{WarnAfter: time.Minute, AbortAfter: 3 * time.Minute})
	observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
	for range 120 {
		c.now = c.now.Add(59 * time.Second)
		observeTool(t, w, c, agent.EventToolOutput, "tool", "")
		if len(w.due(c.Now())) != 0 {
			t.Fatal("continuing output treated as hung")
		}
	}
	c.now = c.now.Add(time.Minute)
	a := w.due(c.Now())
	if len(a) != 1 || a[0].abort || !w.claim(a[0], c.Now()) {
		t.Fatalf("configured warning=%+v", a)
	}
	c.now = c.now.Add(2 * time.Minute)
	a = w.due(c.Now())
	if len(a) != 1 || !a[0].abort || !w.claim(a[0], c.Now()) {
		t.Fatalf("configured abort=%+v", a)
	}
}

func TestToolWatchdogOverlappingHumanWaitsFreezeSilence(t *testing.T) {
	w, c := watchdogStateFixture(t, ToolWatchdogLimits{})
	observeTool(t, w, c, agent.EventToolExecutionStarted, "waiting", "")
	observeTool(t, w, c, agent.EventToolExecutionStarted, "other", "")
	c.now = c.now.Add(9 * time.Minute)
	observeTool(t, w, c, agent.EventToolWaitStarted, "waiting", "one")
	observeTool(t, w, c, agent.EventToolWaitStarted, "waiting", "two")
	c.now = c.now.Add(time.Hour)
	a := w.due(c.Now())
	if len(a) != 1 || a[0].toolID != "other" || !a[0].abort {
		t.Fatalf("per-tool waits=%+v", a)
	}
	observeTool(t, w, c, agent.EventToolExecutionEnded, "other", "")
	observeTool(t, w, c, agent.EventToolWaitEnded, "waiting", "one")
	observeTool(t, w, c, agent.EventToolWaitEnded, "waiting", "unknown")
	c.now = c.now.Add(time.Hour)
	if len(w.due(c.Now())) != 0 {
		t.Fatal("overlapping wait ended too early")
	}
	observeTool(t, w, c, agent.EventToolWaitEnded, "waiting", "two")
	c.now = c.now.Add(time.Minute)
	a = w.due(c.Now())
	if len(a) != 1 || a[0].abort {
		t.Fatalf("wait reset rather than froze silence: %+v", a)
	}
}

func TestToolWatchdogOutputDuringWaitResetsButDoesNotResume(t *testing.T) {
	w, c := watchdogStateFixture(t, ToolWatchdogLimits{})
	observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
	c.now = c.now.Add(9 * time.Minute)
	observeTool(t, w, c, agent.EventToolWaitStarted, "tool", "human")
	observeTool(t, w, c, agent.EventToolOutput, "tool", "")
	c.now = c.now.Add(48 * time.Hour)
	if len(w.due(c.Now())) != 0 {
		t.Fatal("output resumed frozen human wait")
	}
	observeTool(t, w, c, agent.EventToolWaitEnded, "tool", "human")
	c.now = c.now.Add(9 * time.Minute)
	if len(w.due(c.Now())) != 0 {
		t.Fatal("output during wait failed to reset silence")
	}
	c.now = c.now.Add(time.Minute)
	if len(w.due(c.Now())) != 1 {
		t.Fatal("warning missing after resumed silence")
	}
}

func TestToolWatchdogRevalidatesBeforeClaim(t *testing.T) {
	for _, change := range []string{"output", "end", "wait", "disarm"} {
		t.Run(change, func(t *testing.T) {
			w, c := watchdogStateFixture(t, ToolWatchdogLimits{})
			observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
			c.now = c.now.Add(30 * time.Minute)
			a := w.due(c.Now())
			if len(a) != 1 {
				t.Fatal("missing abort candidate")
			}
			switch change {
			case "output":
				observeTool(t, w, c, agent.EventToolOutput, "tool", "")
			case "end":
				observeTool(t, w, c, agent.EventToolExecutionEnded, "tool", "")
			case "wait":
				observeTool(t, w, c, agent.EventToolWaitStarted, "tool", "human")
			case "disarm":
				w.disarm()
			}
			if w.claim(a[0], c.Now()) {
				t.Fatal("stale candidate dispatched")
			}
		})
	}
}

func TestToolWatchdogGenerationReplayAndProposalNeverArm(t *testing.T) {
	w, c := watchdogStateFixture(t, ToolWatchdogLimits{})
	if err := w.observe(agent.EventToolCall, &agent.ToolWatchdogEvent{Generation: "generation", ToolCallID: "proposal"}, c.Now()); err == nil {
		t.Fatal("proposal accepted as execution")
	}
	if err := w.observe(agent.EventToolExecutionStarted, &agent.ToolWatchdogEvent{Generation: "stale", ToolCallID: "old"}, c.Now()); err == nil {
		t.Fatal("stale generation accepted")
	}
	observeTool(t, w, c, agent.EventToolOutput, "unknown", "")
	observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
	observeTool(t, w, c, agent.EventToolExecutionEnded, "tool", "")
	observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
	c.now = c.now.Add(48 * time.Hour)
	if len(w.due(c.Now())) != 0 {
		t.Fatal("replay/proposal/unknown output armed watchdog")
	}
}

func TestToolWatchdogConcurrentClaimIsSingleUse(t *testing.T) {
	w, c := watchdogStateFixture(t, ToolWatchdogLimits{})
	observeTool(t, w, c, agent.EventToolExecutionStarted, "tool", "")
	c.now = c.now.Add(time.Hour)
	a := w.due(c.Now())
	if len(a) != 1 {
		t.Fatal("missing candidate")
	}
	var wg sync.WaitGroup
	results := make(chan bool, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- w.claim(a[0], c.Now()) }()
	}
	wg.Wait()
	close(results)
	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("claims=%d", count)
	}
}

func TestToolWatchdogRejectsInvalidLimits(t *testing.T) {
	for _, l := range []ToolWatchdogLimits{{WarnAfter: -1}, {AbortAfter: -1}, {WarnAfter: time.Hour, AbortAfter: time.Minute}, {WarnAfter: time.Minute, AbortAfter: time.Minute}} {
		if _, err := l.effective(); err == nil {
			t.Fatalf("invalid limits accepted: %+v", l)
		}
	}
}
