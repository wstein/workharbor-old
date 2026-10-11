package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/initiation"
	"github.com/wstein/workharbor/internal/notify"
	"github.com/wstein/workharbor/internal/policy"
	"github.com/wstein/workharbor/internal/runtime"
)

// Report says what one reconciler pass did.
type Report struct {
	Interrupted []domain.ID // runs marked interrupted
	Resumed     []domain.ID // always empty from Reconcile (D57); only an explicit human resume relaunches
	Failed      []domain.ID // runs that could not be resumed and now wait on a Decision
	// AwaitingHuman lists runs left interrupted after the pass. Nothing was
	// sent for them: a human resumes them (D57). It repeats on every pass until
	// they are resumed or ended.
	AwaitingHuman []domain.ID
	Expired       []domain.ID // Decisions that passed their deadline
	Prepared      []domain.ID // tasks whose prepare was started again (D51)
	Published     []domain.ID // tasks whose approved publish was started (D51)
	// Errors are per-task problems that did not stop the pass: an environment
	// that never answered exec, a runtime call that failed. The run stays
	// interrupted and the next pass tries again.
	Errors []error
}

// Reconcile observes environments, interrupts lost runs and expires Decisions.
// It never sends to an agent: interrupted and paused runs await a human action.
// Domain state changes are made through the aggregate.
// Container addresses are read for nothing and never stored.
func (s *Service) Reconcile(ctx context.Context) (Report, error) {
	var rep Report
	infos, err := s.rt.List(ctx, s.cfg.Owner)
	if err != nil {
		return rep, fmt.Errorf("reconcile: list environments: %w", err)
	}
	seen := map[domain.ID]runtime.Info{}
	for _, in := range infos {
		seen[domain.ID(in.ID)] = in
	}
	// Stop what an earlier process left running before anything is observed, so
	// the pass sees those environments stopped (design 5.3, issue #221).
	rep.Errors = append(rep.Errors, s.stopLeftoverEnvs(ctx, seen)...)
	tasks, err := s.store.ActiveTaskIDs(ctx)
	if err != nil {
		return rep, err
	}
	for _, id := range tasks {
		if err := s.reconcileTask(ctx, id, seen, &rep); err != nil {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			rep.Errors = append(rep.Errors, fmt.Errorf("task %s: %w", id, err))
		}
	}
	// Egress expiry never authorizes a send; pending starts wait for a human.
	return rep, nil
}

func (s *Service) reconcileTask(ctx context.Context, task domain.ID, seen map[domain.ID]runtime.Info, rep *Report) error {
	// 1. Observe the environments and interrupt what they took with them.
	var interrupted, expired []domain.ID
	var lost []domain.ID // environments of the runs found without a session
	err := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		interrupted, expired, lost = nil, nil, nil // the closure may run again after a lost compare-and-swap
		for _, env := range a.Environments() {
			if env.State == domain.EnvDeleted {
				continue
			}
			observed := domain.EnvDeleted // the runtime does not know it any more
			if in, ok := seen[env.ID]; ok {
				observed = in.State
			}
			if observed == domain.EnvProvisioning || observed == env.State {
				continue
			}
			before := liveRuns(a)
			if err := a.ObserveEnv(env.ID, observed); err != nil {
				return err
			}
			for id, st := range liveRuns(a) {
				if st == domain.RunInterrupted && before[id] != domain.RunInterrupted {
					interrupted = append(interrupted, id)
				}
			}
		}
		// A live run with no agent session is lost even if its container is
		// up: after a supervisor restart the container survives and the agent
		// process the supervisor owned does not.
		for _, r := range a.Runs() {
			if (r.State == domain.RunStarting || r.State == domain.RunRunning) && !s.attached(r.ID) {
				if err := a.Interrupt(r.ID); err != nil {
					return err
				}
				interrupted = append(interrupted, r.ID)
				lost = append(lost, r.EnvID)
			}
		}
		expired = a.ExpireDecisions(s.clock.Now())
		return nil
	})
	if err != nil {
		return err
	}
	rep.Interrupted = append(rep.Interrupted, interrupted...)
	rep.Expired = append(rep.Expired, expired...)

	// The agent of a lost run may still run in its environment: one this process
	// did not start is stopped, which ends it and interrupts every live run in it
	// (design 4.1, issue #216). A failed stop is tried again by the recovery below,
	// which counts it.
	seenEnv := map[domain.ID]bool{}
	for _, env := range lost {
		if seenEnv[env] || s.envStarted(env) {
			continue
		}
		seenEnv[env] = true
		s.freshMu.Lock()
		serr := s.stopLeftover(ctx, task, env, true, rep)
		s.freshMu.Unlock()
		if serr != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("task %s: %w", task, serr))
		}
	}

	// 2. Interrupted runs stay interrupted: no marker exists here, so the pass
	// sends nothing and only reports them (D57; reset time authorizes nothing).
	agg, err := s.store.LoadTask(ctx, task)
	if err != nil {
		return err
	}
	for _, r := range agg.Runs() {
		if r.State == domain.RunInterrupted {
			rep.AwaitingHuman = append(rep.AwaitingHuman, r.ID)
			// An alert only; the notifier's throttle keeps it to one per window.
			if s.cfg.Notifier != nil {
				s.report(s.cfg.Notifier.Notify(ctx, notify.Message{TaskID: task, Kind: notify.KindRunInterrupted, RunID: r.ID}))
			}
		}
	}
	var firstErr error
	// 3. Complete what a restart interrupted on the way to a pull request: a
	// stopped run nothing was prepared for, an approved commit not yet pushed (D51).
	if s.pipe != nil {
		switch started, err := s.pipe.Kick(ctx, task); {
		case err != nil && firstErr == nil:
			firstErr = err
		case started == "prepare":
			rep.Prepared = append(rep.Prepared, task)
		case started == "publish":
			rep.Published = append(rep.Published, task)
		}
	}
	return firstErr
}

// liveRuns maps each run that is not over to its state.
func liveRuns(a *domain.TaskAggregate) map[domain.ID]domain.RunState {
	out := map[domain.ID]domain.RunState{}
	for _, r := range a.Runs() {
		if !r.State.Terminal() {
			out[r.ID] = r.State
		}
	}
	return out
}

// recover brings one interrupted or due run back: start its environment, wait
// until exec answers, record the environment as running, move the run to
// starting and relaunch the agent from its session. A run that cannot be
// resumed ends failed and waits on a Decision.
func (s *Service) recover(ctx context.Context, task, run domain.ID, rep *Report) error {
	if !initiation.Valid(ctx) {
		return nil
	}
	agg, err := s.store.LoadTask(ctx, task)
	if err != nil {
		return err
	}
	r, ok := agg.Run(run)
	if !ok || (r.State != domain.RunInterrupted && r.State != domain.RunPaused) {
		return nil
	}
	if err := s.executionAdmission(ctx, task, run); err != nil {
		return err
	}
	// A workspace being rebuilt has its environment replaced: the old one is not
	// started for this run. The next pass takes it up, in the new one or not.
	if s.rebuilding(r.WorkspaceID) {
		return nil
	}
	// A run that cannot resume (an open login or quota question) costs no
	// container: the human has to answer first.
	if agg.ResumeBlocked(run) != nil {
		return nil
	}
	env, ok := agg.Environment(r.EnvID)
	if !ok || env.State == domain.EnvDeleted {
		return s.failRun(ctx, task, run, rep) // the environment is gone: nothing to resume in
	}
	if r.SessionID == "" {
		return s.failRun(ctx, task, run, rep) // the agent never reported a session
	}

	// An interrupted run counts for its environment: another run that owns it
	// (a new task started there, say) keeps this one from resuming (issue #216).
	if err := s.checkEnvFree(ctx, env.ID, run); err != nil {
		return err
	}
	// Only an environment this process started is relaunched in (issue #216): any
	// other is stopped and started first.
	if s.envStarted(env.ID) {
		if env.State != domain.EnvRunning {
			if err := s.startEnv(ctx, string(env.ID)); err != nil {
				return fmt.Errorf("start environment %s: %w", env.ID, err)
			}
		}
	} else if err := s.freshenRun(ctx, task, run, env.ID, rep); err != nil {
		return err
	}
	if err := s.waitReady(ctx, env.ID); err != nil {
		return err
	}
	// Starting an agent is serialised per run with Resume and an answer that
	// resumes (design 4.1, one live agent per run). The wait above can be long, so
	// the lock is taken only now, and the run is read again: a human may have
	// resumed it meanwhile, and the path that loses the race changes nothing.
	unlock, err := s.lockRun(ctx, run)
	if err != nil {
		return err
	}
	defer unlock() // released again once the starting write is saved: the launch needs no lock (#225)
	agg, err = s.store.LoadTask(ctx, task)
	if err != nil {
		return err
	}
	r, ok = agg.Run(run)
	if !ok || (r.State != domain.RunInterrupted && r.State != domain.RunPaused) || s.attached(run) ||
		s.rebuilding(r.WorkspaceID) || agg.ResumeBlocked(run) != nil {
		return nil
	}
	if err := s.checkEnvFree(ctx, env.ID, run); err != nil {
		return err // a run started in the environment during the wait above
	}
	// The database says starting before the agent is launched (DB-first). The
	// slot is taken first, so this run is not taken for a lost one meanwhile.
	sl, err := s.begin(run)
	if err != nil {
		return nil // lost the race for the slot: the other path owns the run
	}
	err = s.update(ctx, task, func(a *domain.TaskAggregate) error {
		if err := a.ObserveEnv(env.ID, domain.EnvRunning); err != nil {
			return err
		}
		return a.Resume(run)
	})
	unlock()
	if err != nil {
		s.end(run, sl)
		return err
	}
	if err := s.launch(ctx, task, run, sl); err != nil {
		if errors.Is(err, agent.ErrNoSession) || errors.Is(err, errAttemptsUsedUp) {
			return s.failRun(ctx, task, run, rep)
		}
		return err
	}
	rep.Resumed = append(rep.Resumed, run)
	return nil
}

// errAttemptsUsedUp tells recover that a run's launch attempts are used up.
var errAttemptsUsedUp = errors.New("the run's launch attempts are used up")

// launch sends one explicitly initiated resume and attaches the gated session.
func (s *Service) launch(ctx context.Context, task, run domain.ID, sl *slot) error {
	if err := s.executionAdmission(ctx, task, run); err != nil {
		s.end(run, sl)
		return err
	}
	agg, err := s.store.LoadTask(ctx, task)
	if err != nil {
		s.end(run, sl)
		return err
	}
	r, ok := agg.Run(run)
	if !ok {
		s.end(run, sl)
		return &domain.NotFoundError{Kind: "run", ID: string(run)}
	}
	if err := taskPresetReadable(agg.Task()); err != nil {
		// refused, not started: it counts as an attempt, so it does not loop (§6, #256)
		s.end(run, sl)
		return s.recordLaunchFailure(ctx, task, run, err)
	}
	spec := s.cfg.Spec(agg.Task(), r)
	s.fillApprover(&spec, task, run)
	spec.Prompt = Briefing(agg.Task(), r, agg.SupersededOf(run), spec.Prompt)
	_, mount, err := s.composeSkills(ctx, agg.Task(), r, &spec, false)
	if err == nil {
		err = s.prepareSkills(ctx, r, mount)
	}
	if err != nil {
		s.end(run, sl)
		return s.recordLaunchFailure(ctx, task, run, err)
	}
	spec.EnvID, spec.Env = string(r.EnvID), append(spec.Env, s.agentEnv(ctx, r.EnvID)...)
	if r.AgentID != "" { // a resumed agent works in its own worktree, as a started one does
		a, err := s.store.Agent(ctx, r.AgentID)
		if err != nil {
			// never fall back to the default workdir: the agent would work outside its worktree (§6, #250)
			s.end(run, sl)
			return s.recordLaunchFailure(ctx, task, run, fmt.Errorf("the run's agent cannot be read, so it is not started: %w", err))
		}
		spec.Workdir = a.Worktree
	}
	// The session outlives the call that starts it: an answer to a Decision comes
	// in on a request that ends long before the agent does.
	if err := s.executionAdmission(ctx, task, run); err != nil {
		s.end(run, sl)
		return err
	}
	sess, err := s.ag.Resume(initiation.ForRun(context.WithoutCancel(ctx), task, run), spec, r.SessionID)
	if err != nil {
		s.end(run, sl)
		if errors.Is(err, initiation.ErrNotInitiated) {
			return errors.Join(err, s.update(ctx, task, func(a *domain.TaskAggregate) error { return a.Interrupt(run) }))
		}
		if errors.Is(err, agent.ErrNoSession) {
			return err
		}
		return s.recordLaunchFailure(ctx, task, run, err)
	}
	if err := s.update(ctx, task, func(a *domain.TaskAggregate) error { return a.MarkRunning(run) }); err != nil {
		_ = sess.Stop(ctx)
		s.end(run, sl)
		return err
	}
	s.attach(task, run, sl, sess)
	return nil
}

// recordLaunchFailure counts a failed launch attempt of a run and returns cause,
// joined with errAttemptsUsedUp when the attempts are used up.
func (s *Service) recordLaunchFailure(ctx context.Context, task, run domain.ID, cause error) error {
	if errors.Is(cause, initiation.ErrNotInitiated) {
		return cause
	}
	var exhausted bool
	if uerr := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		var rerr error
		exhausted, rerr = a.RecordLaunchFailure(run, s.cfg.MaxAttempts)
		return rerr
	}); uerr != nil {
		return errors.Join(cause, uerr)
	}
	if exhausted {
		return errors.Join(cause, errAttemptsUsedUp)
	}
	return cause
}

// taskPresetReadable is nil when the task's stored preset can be read: empty
// (a task from before presets were recorded) or a known one. An unreadable one
// is an error, so the run is refused and nothing falls back to the repository's
// preset or the default (§6, issues #236, #256).
func taskPresetReadable(t domain.Task) error {
	_, err := taskPreset(t)
	return err
}

// taskPreset is the preset a task started under, "" for none.
func taskPreset(t domain.Task) (policy.Preset, error) {
	if t.Workflow == "" {
		return "", nil
	}
	p, err := policy.ParsePreset(t.Workflow)
	if err != nil {
		return "", fmt.Errorf("the task's stored preset cannot be read, so the run is not started: %w", err)
	}
	return p, nil
}

// failRun ends a run that cannot be resumed and opens the retry-or-cancel
// Decision.
func (s *Service) failRun(ctx context.Context, task, run domain.ID, rep *Report) error {
	err := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		r, ok := a.Run(run)
		if !ok || r.State.Terminal() {
			return nil
		}
		if r.State == domain.RunPaused || r.State == domain.RunStarting { // neither can fail directly; it is lost first
			if err := a.Interrupt(run); err != nil {
				return err
			}
		}
		var err error
		if env, ok := a.Environment(r.EnvID); !ok || env.State == domain.EnvDeleted {
			_, err = a.FailRunLostEnvironment(run, s.cfg.NewID(), s.clock.Now())
		} else {
			_, err = a.FailRun(run, s.cfg.NewID(), s.clock.Now())
		}
		return err
	})
	if err == nil {
		rep.Failed = append(rep.Failed, run)
	}
	return err
}

// readyOnce runs the readiness command once and collects its output, both
// under a context that ends after left (what remains of ReadyTimeout), so a
// command that never ends cannot outlive the deadline or the caller's wait.
func (s *Service) readyOnce(ctx context.Context, env domain.ID, left time.Duration) bool {
	if left <= 0 {
		left = time.Nanosecond
	}
	ctx, cancel := context.WithTimeout(ctx, left)
	defer cancel()
	st, err := s.rt.Exec(ctx, string(env), runtime.ExecRequest{Cmd: s.cfg.ReadyCmd})
	if err != nil {
		return false
	}
	_, _, code, werr := runtime.Collect(st)
	return werr == nil && code == 0
}

// waitReady polls exec until the environment answers, as the recovery loop
// needs (spike #2: about 100 ms after a start), bounded by ReadyTimeout on the
// injected clock.
func (s *Service) waitReady(ctx context.Context, env domain.ID) error {
	deadline := s.clock.Now().Add(s.cfg.ReadyTimeout)
	for {
		if s.readyOnce(ctx, env, deadline.Sub(s.clock.Now())) {
			return nil
		}
		if !s.clock.Now().Before(deadline) {
			return fmt.Errorf("environment %s did not answer exec within %s", env, s.cfg.ReadyTimeout)
		}
		if err := s.clock.Sleep(ctx, s.cfg.ReadyInterval); err != nil {
			return err
		}
	}
}
