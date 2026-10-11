package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRetiredDurationBudgetCannotEndTask(t *testing.T) {
	a, r, _ := newRunningAggregate(t)
	a.TakeEvents()
	if err := a.ExceedBudget(BudgetBreach{Scope: BudgetRun, Metric: BudgetDuration, RunID: r.ID, Limit: 1, Used: 48 * 60 * 60 * 1000}); err == nil {
		t.Fatal("retired duration budget accepted")
	}
	if a.Task().State != TaskRunning || r.State != RunRunning || len(a.PendingEvents()) != 0 {
		t.Fatal("retired duration budget changed lifecycle or audit")
	}
}

func TestExceedBudgetFailsTheTaskAndStopsItsRun(t *testing.T) {
	a, _, _ := newRunningAggregate(t)
	if _, err := a.RaiseDecision(NewDecision{
		ID: "d1", RunID: "r1", Kind: DecisionQuestion, Blocking: true, Subject: "which one?", Now: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	a.TakeEvents()
	b := BudgetBreach{Scope: BudgetTask, Metric: BudgetTokens, Limit: 1000, Used: 1200}
	if err := a.ExceedBudget(b); err != nil {
		t.Fatal(err)
	}
	if a.Task().State != TaskFailed {
		t.Errorf("task = %s, want failed", a.Task().State)
	}
	if r, _ := a.Run("r1"); r.State != RunStopped {
		t.Errorf("run = %s, want stopped", r.State)
	}
	if d, _ := a.Decision("d1"); d.Status != DecisionSuperseded {
		t.Errorf("decision = %s, want superseded", d.Status)
	}
	var seen bool
	for _, e := range a.TakeEvents() {
		if e.Kind == EventBudgetExceeded {
			var got BudgetBreach
			if err := json.Unmarshal(e.Payload, &got); err != nil || got != b || e.Tier != TierAudit {
				t.Errorf("event = %+v payload %+v %v", e, got, err)
			}
			seen = true
		}
	}
	if !seen {
		t.Error("no budget.exceeded audit entry")
	}
	// A failed task cannot fail again.
	if err := a.ExceedBudget(b); err == nil {
		t.Error("a task that is over cannot exceed a budget")
	}
}

func TestExceedBudgetIsRefusedWhereTheTaskCannotFail(t *testing.T) {
	a := NewTaskAggregate(Task{ID: "t1", State: TaskReadyForReview})
	if err := a.ExceedBudget(BudgetBreach{Scope: BudgetRun, Metric: BudgetCost, Limit: 1, Used: 2}); err == nil {
		t.Error("a task in review does not fail on a budget")
	}
	if a.Task().State != TaskReadyForReview || len(a.PendingEvents()) != 0 {
		t.Errorf("a refused call changed the task: %s, %d events", a.Task().State, len(a.PendingEvents()))
	}
}

func TestBudgetWarnedIsAnAuditEntry(t *testing.T) {
	e := NewBudgetWarned("t1", BudgetBreach{Scope: BudgetRun, Metric: BudgetCost, RunID: "r1", Limit: 10, Used: 8}, time.Unix(5, 0))
	if e.Kind != EventBudgetWarned || e.Tier != TierAudit || e.TaskID != "t1" {
		t.Errorf("event = %+v", e)
	}
}
