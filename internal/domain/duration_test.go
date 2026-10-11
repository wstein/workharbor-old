package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestInitiatingTerminalReasonsAreAuditedAndImmutable(t *testing.T) {
	for _, reason := range []string{"agent_completion", "human_cancellation", "budget_breach", "kill_all", "lost_environment", "failed_start", "failed_resume", "agent_failure"} {
		t.Run(reason, func(t *testing.T) {
			a, r, _ := newRunningAggregate(t)
			a.TakeEvents()
			var err error
			switch reason {
			case "agent_completion":
				err = a.StopRun(r.ID)
			case "human_cancellation":
				err = a.Cancel()
			case "budget_breach":
				err = a.ExceedBudget(BudgetBreach{Scope: BudgetRun, Metric: BudgetTokens, RunID: r.ID, Limit: 10, Used: 10})
			case "kill_all":
				err = a.CancelWithReason(reason)
			case "lost_environment":
				_, err = a.FailRunLostEnvironment(r.ID, "q1", time.Now())
			case "failed_start":
				r.State = RunStarting
				_, err = a.FailRun(r.ID, "q1", time.Now())
			case "failed_resume":
				r.State = RunInterrupted
				r.ResumeAttempts = 1
				_, err = a.FailRun(r.ID, "q1", time.Now())
			case "agent_failure":
				_, err = a.FailRun(r.ID, "q1", time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.TerminalReason != reason {
				t.Fatalf("reason=%q", r.TerminalReason)
			}
			found := false
			for _, e := range a.PendingEvents() {
				if e.Kind == EventRunState {
					var p StateChanged
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if p.Reason == reason {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("terminal reason absent from transition audit")
			}
			if err := a.StopRun(r.ID); err == nil || r.TerminalReason != reason {
				t.Fatal("late completion changed terminal reason")
			}
		})
	}
}
