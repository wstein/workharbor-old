package service

import (
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/domain"
)

func TestRunHasNoDurationDeadline(t *testing.T) {
	for _, state := range []domain.RunState{domain.RunRunning, domain.RunPaused, domain.RunInterrupted} {
		t.Run(string(state), func(t *testing.T) {
			r := newRig(t)
			r.svc.cfg.Budgets = Budgets{PerRun: Limit{MaxTokens: 100}, PerTask: Limit{MaxCostMicroUSD: 100}}
			if state != domain.RunRunning {
				must(t, r.svc.update(bg, "t1", func(a *domain.TaskAggregate) error {
					if state == domain.RunPaused {
						return a.Pause("r1")
					}
					return a.Interrupt("r1")
				}))
			}
			r.clock.now = t0.Add(48 * time.Hour)
			if err := r.svc.executionAdmission(bg, "t1", "r1"); err != nil {
				t.Fatalf("elapsed wall time refused admission: %v", err)
			}
			run, _ := r.load().Run("r1")
			if run.State != state || run.TerminalReason != "" || len(r.auditKinds(domain.EventBudgetExceeded)) != 0 {
				t.Fatalf("elapsed wall time changed run: %+v", run)
			}
			if run.DurationMillis != 0 || !run.DurationAt.IsZero() {
				t.Fatalf("elapsed wall time was accounted: %+v", run)
			}
			// Restart and a backwards wall clock must not create a run deadline.
			r.clock.now = t0.Add(-time.Hour)
			fresh := New(r.store, r.rt.Adapter, r.agent, r.clock, r.svc.cfg)
			if err := fresh.executionAdmission(bg, "t1", "r1"); err != nil {
				t.Fatalf("backwards wall clock refused admission after restart: %v", err)
			}
		})
	}
}
