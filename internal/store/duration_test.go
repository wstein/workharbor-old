package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/domain"
)

func TestDurationBoundsAndTerminalReasonSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duration.db")
	st, err := Open(bg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	a := newAggregate(t, "t1")
	if _, err := st.SaveTask(bg, a); err != nil {
		t.Fatal(err)
	}
	a, err = st.LoadTask(bg, "t1")
	if err != nil {
		t.Fatal(err)
	}
	run := a.Runs()[0]
	if run.AdmittedAt.IsZero() {
		t.Fatal("admission not persisted")
	}
	end := run.AdmittedAt.Add(9 * time.Second)
	st.now = func() time.Time { return end }
	if err := a.Cancel(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveTask(bg, a); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(bg, path, WithClock(func() time.Time { return end.Add(time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	a, err = st.LoadTask(bg, "t1")
	if err != nil {
		t.Fatal(err)
	}
	run = a.Runs()[0]
	if !run.EndedAt.Equal(end) || run.DurationMillis != 0 || run.TerminalReason != "human_cancellation" {
		t.Fatalf("stored terminal=%+v", run)
	}
	// Saving again after downtime cannot extend a terminal run's lifetime.
	if _, err := st.SaveTask(bg, a); err != nil {
		t.Fatal(err)
	}
	a, err = st.LoadTask(bg, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Runs()[0].DurationMillis != 0 {
		t.Fatal("terminal duration kept growing")
	}
}

func TestLegacyBoundsRequireSufficientAuditEvidence(t *testing.T) {
	for _, evidence := range []bool{true, false} {
		t.Run(map[bool]string{true: "lifecycle", false: "missing"}[evidence], func(t *testing.T) {
			st := openTemp(t)
			a := newAggregate(t, "t1")
			if _, err := st.SaveTask(bg, a); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(bg, `UPDATE runs SET admitted_at=0,ended_at=0,duration_ms=0,duration_at=0`); err != nil {
				t.Fatal(err)
			}
			if !evidence {
				if _, err := st.db.ExecContext(bg, `UPDATE runs SET id='legacy-without-events'`); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := st.LoadTask(bg, "t1")
			if err != nil {
				t.Fatal(err)
			}
			r := loaded.Runs()[0]
			if evidence {
				if r.AdmittedAt.IsZero() {
					t.Fatal("durable admission not reconstructed")
				}
			} else if !r.AdmittedAt.IsZero() || r.DurationMillis != 0 {
				t.Fatal("unknown legacy bounds fabricated")
			}
		})
	}
}

func TestBudgetWarningDeduplicatesDurably(t *testing.T) {
	st := openTemp(t)
	a := newAggregate(t, "t1")
	if _, err := st.SaveTask(bg, a); err != nil {
		t.Fatal(err)
	}
	r := a.Runs()[0]
	b := domain.BudgetBreach{Scope: domain.BudgetRun, Metric: domain.BudgetDuration, RunID: r.ID, Limit: 10000, Used: 8000}
	first, err := st.AppendBudgetWarning(bg, "t1", r.ID, b, time.Now())
	if err != nil || len(first) != 1 {
		t.Fatalf("warning=%v %v", first, err)
	}
	second, err := st.AppendBudgetWarning(bg, "t1", r.ID, b, time.Now())
	if err != nil || len(second) != 0 {
		t.Fatalf("duplicate=%v %v", second, err)
	}
}
