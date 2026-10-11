package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/store"
)

// Limit is a budget of one scope. A zero field is no limit.
type Limit struct {
	MaxTokens       int64 // every token the agent reported: input, output, cache read and cache write
	MaxCostMicroUSD int64 // the cost the agent reported, in millionths of a US dollar
}

// Budgets are the per-run and per-task limits of design §7.4. A soft
// threshold warns once; a hard limit ends the task as failed (D13, D21). They
// compare against the usage counters of §5.7, the same totals `whr usage`
// shows, so they only count what the agent reported: a turn that reported
// neither tokens nor a cost adds nothing, and a limit is never read as reached
// because something is unknown.
type Budgets struct {
	PerRun, PerTask Limit
	// SoftPercent is the share of a limit at which the human is warned, 1 to
	// 99. Zero means 80.
	SoftPercent int
}

// defaultSoftPercent is the warning threshold when none is configured.
const defaultSoftPercent = 80

func (b Budgets) soft() int64 {
	if b.SoftPercent < 1 || b.SoftPercent > 99 {
		return defaultSoftPercent
	}
	return int64(b.SoftPercent)
}

// counters are the totals a budget is compared with.
type counters struct{ tokens, cost int64 }

func total(rows []store.UsageRow) counters {
	var c counters
	for _, r := range rows {
		c.tokens = domain.SatAdd(c.tokens, r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheRead, r.Tokens.CacheWrite)
		c.cost = domain.SatAdd(c.cost, r.ReportedMicroUSD)
	}
	return c
}

// checkBudgets compares the totals of a run and of its task with the budgets
// after a turn was recorded. The first hard limit reached ends the task as
// failed, stops the session and is recorded; a soft threshold reached is
// recorded and notified once. Errors are reported, never returned: the turn is
// already recorded and the run goes on.
func (s *Service) checkBudgets(ctx context.Context, task, run domain.ID) {
	b := s.cfg.Budgets
	agg, err := s.store.LoadTask(ctx, task)
	if err != nil {
		s.report(err)
		return
	}
	r, ok := agg.Run(run)
	if !ok || r.State.Terminal() || agg.Task().State.Terminal() {
		return
	}
	if b == (Budgets{}) {
		return
	}
	runRows, err := s.store.UsageTotals(ctx, store.UsageFilter{TaskID: task, RunID: run}, store.GroupAll)
	if err != nil {
		s.report(fmt.Errorf("budget: %w", err))
		return
	}
	taskRows, err := s.store.UsageTotals(ctx, store.UsageFilter{TaskID: task}, store.GroupAll)
	if err != nil {
		s.report(fmt.Errorf("budget: %w", err))
		return
	}
	runCounts, taskCounts := total(runRows), total(taskRows)
	checks := []struct {
		scope  domain.BudgetScope
		run    domain.ID
		limit  Limit
		counts counters
	}{
		{domain.BudgetRun, run, b.PerRun, runCounts},
		{domain.BudgetTask, "", b.PerTask, taskCounts},
	}
	var warnings []domain.BudgetBreach
	for _, c := range checks {
		for _, m := range []struct {
			metric      domain.BudgetMetric
			limit, used int64
		}{
			{domain.BudgetTokens, c.limit.MaxTokens, c.counts.tokens},
			{domain.BudgetCost, c.limit.MaxCostMicroUSD, c.counts.cost},
		} {
			if m.limit <= 0 {
				continue
			}
			breach := domain.BudgetBreach{Scope: c.scope, Metric: m.metric, RunID: c.run, Limit: m.limit, Used: m.used}
			if m.used >= m.limit {
				s.exceed(ctx, task, run, breach)
				return // the task is over; nothing else to warn about
			}
			if m.used >= (m.limit/100)*b.soft()+(m.limit%100*b.soft()+99)/100 {
				warnings = append(warnings, breach)
			}
		}
	}
	for _, w := range warnings {
		s.warnRun(ctx, task, run, w)
	}
}

// exceed ends the task for a hard limit and stops the live session.
func (s *Service) exceed(ctx context.Context, task, run domain.ID, b domain.BudgetBreach) {
	var env domain.ID
	hold := &stopHold{s: s}
	err := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		hold.set("")
		env = ""
		if r, ok := a.Run(run); !ok || r.State.Terminal() {
			return domain.NewConflict(domain.RuleRunLive, "budget event belongs to an ended run")
		}
		if r, ok := a.Run(run); ok && ownsAgent(r.State) {
			env = r.EnvID
			hold.set(env)
		}
		return a.ExceedBudget(b)
	})
	var conflict *domain.ConflictError
	switch {
	case err == nil:
		s.cancelStart(run)    // preparation belongs to the ended run, not a successor
		s.dropEgressWait(run) // no answer may launch an ended run
		// The environment is busy until the stop and its fallback have ended (#238).
		s.report(s.stopAgent(ctx, task, run, env, "failed (a budget was reached)", "budget", false, hold.take()))
	case errors.As(err, &conflict):
		hold.drop()
		// The task is already over, or in review: nothing to end.
	default:
		hold.drop()
		s.report(fmt.Errorf("budget: %w", err))
	}
}

// warn records a soft threshold once and notifies it.
func (s *Service) warnRun(ctx context.Context, task, run domain.ID, b domain.BudgetBreach) {
	saved, err := s.store.AppendBudgetWarning(ctx, task, run, b, s.clock.Now())
	if err != nil {
		s.report(fmt.Errorf("budget: %w", err))
		return
	}
	s.publish(saved)
	s.notify(ctx, saved)
}
