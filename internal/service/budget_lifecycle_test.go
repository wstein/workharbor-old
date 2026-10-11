package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/store"
)

func TestLateUsageCannotStopSuccessorRun(t *testing.T) {
	r := newRig(t)
	must(t, r.svc.update(bg, "t1", func(a *domain.TaskAggregate) error {
		if err := a.StopRun("r1"); err != nil {
			return err
		}
		return a.StartRun(domain.Run{ID: "r2", EnvID: r.env})
	}))
	r.svc.cfg.Budgets = Budgets{PerTask: Limit{MaxTokens: 1}}
	r.turn(1, 2, 0)
	v, err := r.svc.Show(bg, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Task.State == domain.TaskFailed || v.Runs[1].State.Terminal() {
		t.Fatal("late r1 usage stopped its successor")
	}
}

func TestNoUsageIsUnknown(t *testing.T) {
	if got := FormatUsageLine(UsageReport{}); !strings.Contains(got, "unknown") {
		t.Fatalf("no reports = %q, want unknown", got)
	}
}

func TestTokenSoftWarningIsAtomicAndHardTakesPrecedence(t *testing.T) {
	r := newRig(t)
	r.svc.cfg.Budgets = Budgets{PerRun: Limit{MaxTokens: 10}}
	r.turn(1, 8, 0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); r.svc.checkBudgets(bg, "t1", "r1") }()
	}
	wg.Wait()
	if got := len(r.auditKinds(domain.EventBudgetWarned)); got != 1 {
		t.Fatalf("soft warnings = %d", got)
	}
	r.turn(2, 2, 0)
	r.svc.checkBudgets(bg, "t1", "r1")
	r.svc.checkBudgets(bg, "t1", "r1")
	a := r.load()
	run, _ := a.Run("r1")
	if run.TerminalReason != "budget_breach" || len(r.auditKinds(domain.EventBudgetExceeded)) != 1 {
		t.Fatalf("terminal run=%+v", run)
	}
}

func TestTerminalReasonSurvivesLateCompletionAndUsage(t *testing.T) {
	r := newRig(t)
	must(t, r.svc.Cancel(bg, "t1"))
	must(t, r.svc.finish(bg, "t1", "r1", agent.Result{Status: agent.ResultCompleted}))
	r.turn(1, 99, 99)
	run, _ := r.load().Run("r1")
	if run.TerminalReason != "human_cancellation" {
		t.Fatalf("reason=%q", run.TerminalReason)
	}
}

type budgetTestClock struct {
	SystemClock
	nanos atomic.Int64
}

func (c *budgetTestClock) Now() time.Time { return time.Unix(0, c.nanos.Load()) }

func TestUnknownZeroAndPartialUsageAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  store.UsageRow
		want string
	}{
		{"unknown", store.UsageRow{Turns: 2, TurnsWithoutToken: 2, TurnsWithoutCost: 2}, "tokens unknown"},
		{"zero", store.UsageRow{Turns: 1}, "0 in, 0 out"},
		{"partial", store.UsageRow{Turns: 2, TurnsWithoutToken: 1, TurnsWithoutCost: 1}, "partial; 1 turns without cost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatUsageLine(UsageReport{Rows: []UsageRow{{UsageRow: tc.row}}})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("%s: %q", tc.name, got)
			}
		})
	}
}

func TestTokenCheckKeepsOwnershipWithoutBlockingOtherRuns(t *testing.T) {
	r := newRig(t)
	r.live()
	r.svc.mu.Lock()
	sl := r.svc.sessions["r1"]
	gate := &gateSession{Session: sl.sess, entered: make(chan struct{}), gate: make(chan struct{})}
	sl.sess = gate
	r.svc.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate.gate) }) }
	t.Cleanup(release)
	second := domain.NewTaskAggregate(domain.Task{ID: "t2", Repo: "wstein/workharbor", State: domain.TaskRunning, CreatedAt: t0})
	second.AddEnvironment(domain.Environment{ID: "e2", Backend: "fake", State: domain.EnvRunning})
	must(t, second.StartRun(domain.Run{ID: "r2", EnvID: "e2"}))
	_, err := r.store.SaveTask(bg, second)
	must(t, err)
	r.svc.cfg.Budgets = Budgets{PerRun: Limit{MaxTokens: 1}}
	r.clock.now = t0.Add(time.Second)
	checked := make(chan struct{})
	go func() { defer close(checked); r.turn(1, 1, 0) }()
	r.svc.recordUsage(bg, "t2", "r2", agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "m", Tokens: &agent.TokenCounts{Input: 1}}})
	select {
	case <-gate.entered:
	case <-time.After(4 * time.Second):
		t.Fatal("budget cleanup never started")
	}
	pollUntil(t, func() bool {
		a, err := r.store.LoadTask(bg, "t2")
		return err == nil && a.Task().State == domain.TaskFailed
	})
	if err := r.svc.checkEnvFree(bg, r.env, ""); err == nil {
		t.Fatal("terminal run released environment before cleanup")
	}
	release()
	<-checked
	r.svc.Wait()
}

func TestSimultaneousHardBudgetsUseDeterministicScopeAndMetric(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  Limit
		metric domain.BudgetMetric
		scope  domain.BudgetScope
	}{
		{"run tokens", Limit{MaxTokens: 2, MaxCostMicroUSD: 2}, domain.BudgetTokens, domain.BudgetRun},
		{"run cost", Limit{MaxCostMicroUSD: 2}, domain.BudgetCost, domain.BudgetRun},
		{"task tokens", Limit{}, domain.BudgetTokens, domain.BudgetTask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.svc.cfg.Budgets = Budgets{PerRun: tc.limit, PerTask: Limit{MaxTokens: 2, MaxCostMicroUSD: 2}}
			r.turn(1, 2, 2)
			b := r.auditKinds(domain.EventBudgetExceeded)
			if len(b) != 1 || b[0].Scope != tc.scope || b[0].Metric != tc.metric {
				t.Fatalf("hard precedence=%+v", b)
			}
			if len(r.auditKinds(domain.EventBudgetWarned)) != 0 {
				t.Fatal("warning recorded beside simultaneous hard breach")
			}
		})
	}
}

// The command observes cancellation but deliberately delays returning, like a
// runtime finishing cleanup after its process was asked to stop.
type budgetPreparationGate struct {
	runtime.Adapter
	entered, cancelled, release chan struct{}
}

func (g *budgetPreparationGate) Exec(ctx context.Context, env string, req runtime.ExecRequest) (runtime.ExecStream, error) {
	if len(req.Cmd) == 1 && req.Cmd[0] == "sleep" {
		close(g.entered)
		<-ctx.Done()
		close(g.cancelled)
		<-g.release
		return nil, ctx.Err()
	}
	return g.Adapter.Exec(ctx, env, req)
}

func TestTokenBudgetCancelsPreparationAndHoldsUntilCleanup(t *testing.T) {
	r := newWsRig(t)
	r.withBlockingPostCreate()
	gate := &budgetPreparationGate{Adapter: r.svc.rt, entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	r.svc.rt = gate
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(func() {
		r.svc.mu.Lock()
		for _, job := range r.svc.starts {
			job.cancel()
		}
		r.svc.mu.Unlock()
		release()
	})
	clock := &budgetTestClock{}
	clock.nanos.Store(t0.UnixNano())
	r.svc.clock = clock
	r.svc.cfg.Budgets = Budgets{PerRun: Limit{MaxTokens: 1}}
	w, a := r.create("budget-start")
	_, _, err := r.ws.StartTask(userContext(), StartRequest{AgentID: a.ID, Issue: "#1"})
	must(t, err)
	task, run := r.onlyTask()
	<-gate.entered
	clock.nanos.Store(t0.Add(time.Second).UnixNano())
	r.svc.recordUsage(bg, task, run, agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "m", Tokens: &agent.TokenCounts{Input: 1}}})
	select {
	case <-gate.cancelled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("hard token budget left detached preparation executing")
	}
	if err := r.svc.checkEnvFree(bg, w.EnvID, ""); err == nil {
		t.Fatal("budget released environment while preparation cleanup was pending")
	}
	agg, err := r.store.LoadTask(bg, task)
	must(t, err)
	ended, _ := agg.Run(run)
	if ended.TerminalReason != "budget_breach" || agg.Task().State != domain.TaskFailed {
		t.Fatalf("terminal preparation=%+v", ended)
	}
	release()
	pollUntil(t, func() bool { return r.svc.startDoneChan(run) == nil && !r.svc.attached(run) })
	if err := r.svc.checkEnvFree(bg, w.EnvID, ""); err != nil {
		t.Fatalf("cleanup did not release ownership: %v", err)
	}
	if r.agent.Started() != 0 {
		t.Fatal("budget-stopped preparation launched an agent")
	}
	agg, err = r.store.LoadTask(bg, task)
	must(t, err)
	ended, _ = agg.Run(run)
	if ended.TerminalReason != "budget_breach" {
		t.Fatal("preparation cleanup replaced initiating budget reason")
	}
}

func TestTokenBudgetDropsOnlyOriginatingEgressWait(t *testing.T) {
	r := newWsRig(t)
	r.withEgressRequests()
	r.svc.cfg.Budgets = Budgets{PerRun: Limit{MaxTokens: 1}}
	w, a := r.create("budget-egress")
	task, run, err := r.ws.StartTask(userContext(), StartRequest{AgentID: a.ID, Issue: "#7"})
	must(t, err)
	open := r.openEgress(task)
	r.clock.now = t0.Add(time.Second)
	r.svc.recordUsage(bg, task, run, agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "m", Tokens: &agent.TokenCounts{Input: 1}}})
	if len(r.svc.egressWaits) != 0 || r.svc.attached(run) {
		t.Fatal("budget-stopped run retained its egress wait or ownership")
	}
	must(t, r.svc.checkEnvFree(bg, w.EnvID, ""))
	nextTask, nextRun, err := r.ws.StartTask(userContext(), StartRequest{AgentID: a.ID, Issue: "#8"})
	must(t, err)
	if err := r.svc.AnswerDecision(userContext(), open[0].ID, domain.Response{Option: domain.AnswerAllow, By: "werner", At: t0}); err == nil {
		t.Fatal("late answer for budget-stopped run was accepted")
	}
	r.svc.checkBudgets(bg, task, run)
	if !r.svc.attached(nextRun) || len(r.svc.egressWaits) != 1 {
		t.Fatal("stale budget check disturbed successor egress wait")
	}
	next, err := r.store.LoadTask(bg, nextTask)
	must(t, err)
	live, _ := next.Run(nextRun)
	if live.State.Terminal() || r.agent.Started() != 0 {
		t.Fatal("late answer or stale budget changed successor execution")
	}
}
