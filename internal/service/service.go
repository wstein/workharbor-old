// Package service is the one layer the JSON API and the web UI call (design
// D8), and the DB-first reconciler (D6, §5.3). It holds no state of its own
// beyond the sessions it has attached: the database is the desired state, the
// runtime and the agent are what is actually there, and every change to a task
// goes through its aggregate.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/forge"
	"github.com/wstein/workharbor/internal/initiation"
	"github.com/wstein/workharbor/internal/notify"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/skillset"
	"github.com/wstein/workharbor/internal/store"
)

// Clock is time as the service sees it, so tests run on an injected clock.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends, and returns the context's error then.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Config is what the service needs besides its adapters.
type Config struct {
	SkillSet            *skillset.Config
	SkillForbidden      []string
	ProjectInstructions func(context.Context, domain.Task, domain.Run) (ProjectInstructions, error)
	SkillBinding        func(context.Context, []skillset.Binding) (skillset.Binding, error)
	PrepareSkills       func(context.Context, domain.Run, SkillMount) error
	// Location is the supervisor's time zone: the days of the usage summary and
	// `whr usage --by day` are days there. Default time.Local.
	Location *time.Location
	// Owner is the supervisor's owner label: the reconciler lists only the
	// environments that carry it (design §5.1).
	Owner string
	// Spec returns the StartSpec to resume a run with: the working directory, a
	// prompt such as "continue", the auth mode, the permission mode and the
	// approver. The service sets nothing in it.
	Spec func(task domain.Task, run domain.Run) agent.StartSpec
	// NewID returns a fresh ID for a Decision the service raises.
	NewID func() domain.ID
	// ReadyCmd is a command that succeeds once the environment answers exec
	// (about 100 ms after a start, spike #2). Default ["true"].
	ReadyCmd []string
	// ReadyTimeout bounds the wait, and ReadyInterval is the pause between
	// attempts. Defaults 30 s and 100 ms.
	ReadyTimeout, ReadyInterval time.Duration
	// MaxAttempts is how many failed launches of the agent a run may have
	// before it ends failed (design §5.3). Default 3.
	MaxAttempts int
	// Notifier gets a push for a new blocking Decision and for a run that ended
	// or failed (design §9.4). It is best effort: a failure is reported through
	// OnError and never fails the change. Optional.
	Notifier notify.Notifier
	// Board, if set, is kept current with the state of every task that started
	// from an issue (design D30): the supervisor's own action, through the
	// forge's autonomy policy. A failed write never affects a task. Optional.
	Board forge.Board
	// BoardLink returns the link to a task in the web UI for its card. Optional.
	BoardLink func(task domain.ID) string
	// RevokeTokens revokes the forge tokens the supervisor holds and returns how
	// many it revoked: the token half of KillAll. Optional.
	RevokeTokens func(ctx context.Context) (int, error)
	// PostCreateTimeout bounds a repository's postCreateCommand, which runs before
	// the agent starts. Default 10 minutes. A command still running then fails
	// the run with that reason.
	PostCreateTimeout time.Duration
	// StartWait is how long StartTask waits for the agent to start before it
	// returns with the run still starting. Default 30 seconds.
	StartWait time.Duration
	// Budgets are the per-run and per-task limits on tokens and cost (§7.4).
	// The zero value sets none. Optional.
	Budgets Budgets
	// ToolWatchdog bounds output silence only for sessions that implement the
	// optional correlated execution and tool-only cancellation contract.
	ToolWatchdog ToolWatchdogLimits
	// LowLimits say when a provider's reported limit is low (issue #172).
	LowLimits LowLimits
	// OnError hears errors that happen in the background, such as a session's
	// event handler losing a compare-and-swap for good. Optional.
	OnError func(error)
}

// Service is the supervisor's use-case layer.
type Service struct {
	store *store.Store
	rt    runtime.Adapter
	ag    *initiation.Gate
	clock Clock
	cfg   Config

	wg       sync.WaitGroup
	mu       sync.Mutex
	sessions map[domain.ID]*slot // by run: the sessions the service owns, and launches in progress
	// starts are the agent starts in progress, by run: at most one each, detached
	// from the request that began it and cancelled by Cancel and Shutdown.
	starts map[domain.ID]*startJob
	// testBeforeAttach, when set by a test, runs in startAgent between the run
	// being marked running and the session being attached.
	testBeforeAttach func(run domain.ID)
	// egressWaits are the runs that stay starting until their egress requests are
	// answered (design §4.2), by run.
	egressWaits map[domain.ID]*egressWait
	runLocks    map[domain.ID]*runLock // one resume of a run at a time
	// startedEnvs are the environments this process started (issue #216): an agent
	// is relaunched only in one of them. freshMu serialises the stop and start of
	// one that is not, so each is stopped once per process.
	startedEnvs map[domain.ID]bool
	freshMu     sync.Mutex
	// loadTask reads a task for the resume, egress, pause and stale paths; a test replaces it to fail a read.
	loadTask    func(ctx context.Context, id domain.ID) (*domain.TaskAggregate, error)
	limitWarned map[string]limitWarn // per key: last low-limit push and state
	// egressSources is what each open egress request was asked about (its
	// devcontainer.json digest), kept to store with the answer.
	egressSources map[domain.ID]string
	closing       bool                              // set by Shutdown: no session joins the wait group any more
	async         *notify.Async                     // the queue that delivers cfg.Notifier's messages, when set
	bus           bus                               // live events for subscribers (design §5.3)
	board         boardQueue                        // card updates waiting for the worker (D30)
	approvals     map[domain.ID]chan agent.Approval // approval Decisions an agent is waiting for (D26)
	// rebuilds are the workspaces whose environment is being replaced (issue
	// #128), guarded by rebuildMu alone, which is never held across a call out.
	// Every path that starts or uses a workspace's environment asks it.
	rebuildMu sync.Mutex
	rebuilds  map[domain.ID]bool
	// leases counts the operations that are starting or using a workspace's
	// environment (see leaseEnvironment), also under rebuildMu.
	leases map[domain.ID]int
	// holds counts the holders of an environment's busy mark (HoldEnvironment),
	// under rebuildMu.
	holds map[domain.ID]int
	// exclusiveHolds marks a sign-in shell for its whole lifetime. Counted
	// operation holds cannot join it; admission and release use rebuildMu.
	exclusiveHolds map[domain.ID]bool
	// stopHolds counts the holders among holds that are an agent stop's
	// (holdEnvBusy): a check's HoldEnvironment is refused while one is on.
	stopHolds map[domain.ID]int

	// pipe prepares a task when its run stops and publishes what is approved (D51);
	// nil when this supervisor does not publish. bg is its context, cancelled by
	// Shutdown so a prepare or a publish in progress ends.
	pipe   *Pipeline
	bg     context.Context
	bgStop context.CancelFunc
}

// rebuilding reports whether the workspace's environment is being replaced.
func (s *Service) rebuilding(ws domain.ID) bool {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	return s.rebuilds[ws]
}

// markRebuilding marks the workspace as being rebuilt; false if it already was
// or an operation holds a lease on its environment (leased says which).
func (s *Service) markRebuilding(ws domain.ID) (marked, leased bool) {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	if s.rebuilds[ws] {
		return false, false
	}
	if s.leases[ws] > 0 {
		return false, true
	}
	if s.rebuilds == nil {
		s.rebuilds = map[domain.ID]bool{}
	}
	s.rebuilds[ws] = true
	return true, false
}

func (s *Service) unmarkRebuilding(ws domain.ID) {
	s.rebuildMu.Lock()
	delete(s.rebuilds, ws)
	s.rebuildMu.Unlock()
}

// leaseEnvironment is what a path that would start or use the workspace's
// environment takes first: a conflict while it is being replaced (the old one is
// not started, and the caller tries again when the rebuild is done), otherwise a
// lease that keeps a rebuild from beginning until release is called. Check and
// lease are one step under rebuildMu, which is never held across a call out.
func (s *Service) leaseEnvironment(ws domain.Workspace) (release func(), err error) {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	if s.rebuilds[ws.ID] {
		return nil, domain.NewConflict(domain.RuleEnvRunning, "workspace %s is being rebuilt: try again when it is done", ws.Name)
	}
	if s.leases == nil {
		s.leases = map[domain.ID]int{}
	}
	s.leases[ws.ID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.rebuildMu.Lock()
			defer s.rebuildMu.Unlock()
			if s.leases[ws.ID]--; s.leases[ws.ID] <= 0 {
				delete(s.leases, ws.ID)
			}
		})
	}, nil
}

// slot is a run's entry in the sessions map. It is put there before the agent
// is called, so a run whose launch is in progress is not mistaken for a lost
// one, and the session is filled in once the agent is up.
//
// A stop that arrives before the session is up (a pause or a cancel between the
// run being marked running and the session being attached) is remembered in
// stopRequested, and attach honours it, so the agent never runs on after it was
// told to stop. Both fields are guarded by Service.mu.
type slot struct {
	sess          agent.Session
	watchdog      *runToolWatchdog
	stopRequested bool
	// after and release belong to a stop requested before the session was up
	// (stopAgent): attach runs after when its stop fails, then releases the
	// environment's busy mark; end releases it when the session never comes up.
	after   func(error) error
	release func()
}

// New returns a service.
func New(st *store.Store, rt runtime.Adapter, ag agent.Adapter, clock Clock, cfg Config) *Service {
	if clock == nil {
		clock = SystemClock{}
	}
	if len(cfg.ReadyCmd) == 0 {
		cfg.ReadyCmd = []string{"true"}
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.ReadyInterval <= 0 {
		cfg.ReadyInterval = 100 * time.Millisecond
	}
	s := &Service{store: st, loadTask: st.LoadTask, rt: rt, ag: initiation.New(ag, st), clock: clock, cfg: cfg, sessions: map[domain.ID]*slot{}, approvals: map[domain.ID]chan agent.Approval{}}
	s.bg, s.bgStop = context.WithCancel(context.Background())
	if cfg.Notifier != nil {
		// A slow relay must never hold up a reconcile pass or a session
		// handler: messages go through a bounded queue (design §9.4).
		s.async = notify.NewAsync(cfg.Notifier, 64, 10*time.Second)
		s.async.OnError(s.report)
		s.cfg.Notifier = s.async
	}
	return s
}

// Wait blocks until every attached session's handler has finished.
func (s *Service) Wait() { s.wg.Wait() }

// Shutdown stops every attached session and waits for their handlers. A stop
// leaves a session resumable, so the next start of the supervisor reconciles it
// from the database (design §5.3).
func (s *Service) Shutdown() {
	s.bgStop() // a prepare or a publish in progress ends; the reconciler repeats it
	s.mu.Lock()
	s.closing = true // from now on attach stops a session instead of adding it
	for _, j := range s.starts {
		j.cancel() // a start in progress ends: its run stays starting for the next start's reconcile
	}
	live := make([]agent.Session, 0, len(s.sessions))
	for _, sl := range s.sessions {
		s.disarmToolWatchdog(sl)
		if sl.sess != nil {
			live = append(live, sl.sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range live {
		_ = sess.Stop(context.Background())
	}
	s.wg.Wait()
	s.waitBoard(10 * time.Second)  // the last card updates, bounded: the board is never worth a hang
	s.closeBoard(10 * time.Second) // and the worker goes with the service
	if s.async != nil {
		s.async.Close() // deliver what is queued, then stop
	}
}

func (s *Service) waitBoard(limit time.Duration) {
	done := make(chan struct{})
	go func() { s.board.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

func (s *Service) report(err error) {
	if err != nil && s.cfg.OnError != nil {
		s.cfg.OnError(err)
	}
}

// maxRetries is how often a change is retried after losing a compare-and-swap.
const maxRetries = 5

// update loads a task, applies fn and saves what changed, retrying from a fresh
// load when another writer got in first. fn may return an error after it
// changed the aggregate (a refused answer can still expire a Decision): the
// change is saved and the error returned.
func (s *Service) update(ctx context.Context, task domain.ID, fn func(*domain.TaskAggregate) error) error {
	var err error
	for range maxRetries {
		var agg *domain.TaskAggregate
		if agg, err = s.store.LoadTask(ctx, task); err != nil {
			return err
		}
		fnErr := fn(agg)
		if len(agg.PendingEvents()) > 0 {
			agg.StampRunBounds(s.clock.Now())
			var saved []domain.Event
			if saved, err = s.store.SaveTask(ctx, agg); err != nil {
				if errors.Is(err, store.ErrStale) {
					continue
				}
				return err
			}
			s.publish(saved)
			s.notify(ctx, saved)
		}
		return fnErr
	}
	return fmt.Errorf("task %s: %w", task, err)
}

// notify pushes what the saved events call for. It runs after the change is
// committed and never fails it; the inbox is the source of truth.
func (s *Service) notify(ctx context.Context, events []domain.Event) {
	if s.cfg.Notifier == nil {
		return
	}
	cancelled := false
	for _, e := range events {
		if e.Kind == domain.EventBudgetExceeded {
			cancelled = true // the budget message says why the run ended
		}
		if e.Kind == domain.EventTaskState {
			var p domain.StateChanged
			if json.Unmarshal(e.Payload, &p) == nil && p.To == string(domain.TaskCancelled) {
				cancelled = true
			}
		}
	}
	for _, m := range notify.FromEvents(events) {
		if cancelled && m.Kind == notify.KindRunEnded {
			continue // the human cancelled it; they know
		}
		s.report(s.cfg.Notifier.Notify(ctx, m)) // queued: returns at once
	}
}

// errRunAttached is the refusal of a path that would start an agent for a run
// that already has a session, or a launch in progress (design 4.1, one live
// agent per run).
var errRunAttached = errors.New("the run already has a live agent")

// begin marks a run's launch as in progress. It never replaces an occupied
// slot: a run has at most one live agent, so a second launch is refused with
// errRunAttached and changes nothing.
func (s *Service) begin(run domain.ID) (*slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[run]; ok {
		return nil, fmt.Errorf("run %s: %w", run, errRunAttached)
	}
	sl := &slot{}
	s.sessions[run] = sl
	return sl, nil
}

// end removes a run's entry, but only if it is still this one: a relaunch
// during a pass may have put a new session there.
func (s *Service) end(run domain.ID, sl *slot) {
	s.mu.Lock()
	s.disarmToolWatchdog(sl)
	if s.sessions[run] == sl {
		delete(s.sessions, run)
	}
	release := sl.release
	s.mu.Unlock()
	if release != nil {
		release()
	}
}

// attached reports whether the service owns a session, or a launch in
// progress, for a run.
func (s *Service) attached(run domain.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[run]
	return ok
}

// attach pumps a session's events into the task until the session ends: the
// agent's session ID is recorded, a login or quota end suspends the run, and the
// result ends, fails or interrupts it. Everything goes through the aggregate.
func (s *Service) attach(task, run domain.ID, sl *slot, sess agent.Session) {
	monitor, coverageErr := s.newRunToolWatchdog(task, run, sess)
	s.mu.Lock()
	if s.closing {
		if monitor != nil {
			monitor.state.disarm()
			monitor.cancel()
		}
		// Shutdown has begun: the session never joins the wait group, so
		// wg.Add cannot race wg.Wait. It is stopped and stays resumable; the
		// next start reconciles the run (design §5.3).
		s.mu.Unlock()
		_ = sess.Stop(context.Background())
		s.end(run, sl)
		return
	}
	sl.sess = sess
	sl.watchdog = monitor
	stopNow, after, release := sl.stopRequested, sl.after, sl.release
	if stopNow {
		s.disarmToolWatchdog(sl)
	}
	s.wg.Add(1) // under s.mu, so it happens before Shutdown sets closing or not at all
	s.mu.Unlock()
	s.report(coverageErr)
	go func() {
		defer s.wg.Done()
		defer s.end(run, sl)
		ctx := context.Background()
		if stopNow {
			// Asked to stop while it was starting.
			if serr := sess.Stop(ctx); serr != nil && after != nil {
				s.report(after(serr))
			}
			if release != nil {
				release()
			}
		}
		for e := range sess.Events() {
			s.observeToolWatchdog(sl, e)
			switch e.Kind {
			case agent.EventSession:
				s.report(s.update(ctx, task, func(a *domain.TaskAggregate) error { return a.RecordSession(run, e.SessionID) }))
			case agent.EventAuthExpired:
				s.report(s.suspend(ctx, task, run, domain.CauseAuthExpired, time.Time{}))
			case agent.EventQuotaExhausted:
				s.report(s.suspend(ctx, task, run, domain.CauseQuotaExhausted, e.ResetAt))
			case agent.EventUsage:
				s.recordUsage(ctx, task, run, e)
			default:
				s.record(ctx, task, e)
			}
		}
		s.mu.Lock()
		s.disarmToolWatchdog(sl)
		s.mu.Unlock()
		res, _ := sess.Wait()
		s.report(s.finish(ctx, task, run, res))
	}()
}

// suspend saves the run paused with its question first, then stops the agent as
// Pause does (design 4.2, "Suspending is a pause"): a paused run has no agent
// process (D11). A failed stop of the agent stops its environment instead (design
// 4.1, issue #221). A repeated event changes nothing.
func (s *Service) suspend(ctx context.Context, task, run domain.ID, cause domain.DecisionCause, reset time.Time) error {
	var env domain.ID
	changed := false
	err := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		changed = false
		r, ok := a.Run(run)
		if !ok || r.State != domain.RunRunning {
			return nil // already suspended or over: the event is a repeat
		}
		env = r.EnvID
		if _, err := a.SuspendRun(run, cause, reset, s.cfg.NewID(), s.clock.Now()); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil || !changed {
		return err
	}
	// The run is paused, so the session's end is not taken for a loss.
	if serr := s.stopSessionErr(run); serr != nil {
		err := s.stopEnvForPause(ctx, task, env, serr)
		if agentMayRun(err) {
			// No human call waits for this error: the task keeps it too.
			err = errors.Join(err, s.recordAgentMayRun(ctx, task, run, env, "suspension", err))
		}
		return err
	}
	return nil
}

// finish applies a session's result to its run, unless the run is no longer
// the live one (a stop or a suspension got there first).
func (s *Service) finish(ctx context.Context, task, run domain.ID, res agent.Result) error {
	// A run that stops because its agent finished is prepared next (D51): from the
	// moment it is saved stopped until the prepare has pinned a revision or been
	// refused, its environment counts as busy, so no run starts there and no other
	// task's commits reach the export. The hold is taken before the save.
	hold := &stopHold{s: s, plain: true}
	defer hold.drop() // a no-op once the prepare has taken it
	stopped := false
	err := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		stopped = false
		r, ok := a.Run(run)
		if !ok || r.State.Terminal() || r.State == domain.RunPaused || r.State == domain.RunInterrupted {
			return nil
		}
		switch res.Status {
		case agent.ResultCompleted:
			if s.pipe != nil {
				hold.set(r.EnvID)
			}
			stopped = s.pipe != nil
			return a.StopRun(run)
		case agent.ResultStopped:
			// A stop the human asked for (cancel) has ended the run already, so
			// a live run here lost its agent to the supervisor: a shutdown. The
			// run is interrupted and resumes on the next start.
			return a.Interrupt(run)
		case agent.ResultFailed:
			_, err := a.FailRun(run, s.cfg.NewID(), s.clock.Now())
			return err
		case agent.ResultAuthExpired:
			_, err := a.SuspendRun(run, domain.CauseAuthExpired, time.Time{}, s.cfg.NewID(), s.clock.Now())
			return err
		case agent.ResultQuotaExhausted:
			_, err := a.SuspendRun(run, domain.CauseQuotaExhausted, res.ResetAt, s.cfg.NewID(), s.clock.Now())
			return err
		}
		return nil
	})
	if err == nil && stopped {
		s.pipe.afterStop(task, hold.take())
	}
	return err
}

// AnswerDecision records a human's answer. An answer that resumes a run
// ("resume") relaunches the agent from its session; "cancel" stops it. The
// error of a refused answer is returned after what it changed was saved.
func (s *Service) AnswerDecision(ctx context.Context, id domain.ID, r domain.Response) error {
	row, err := s.store.LoadDecision(ctx, id)
	if err != nil {
		return err
	}
	if row.Cause == domain.CauseAgentMayRun && r.Option != domain.AnswerSeen {
		return domain.ErrDecisionOption
	}
	// An egress request is asked before the agent starts, so no agent waits for it.
	if row.Kind == domain.DecisionApproval && !row.Cause.AsksBeforeStart() && row.Status == domain.DecisionOpen && !s.approvalWaiting(id) {
		// The agent that asked is gone (stopped, paused, or the supervisor
		// restarted), so there is nothing to answer: refused, not stored (D23).
		return domain.NewConflict(domain.RuleDecisionClosed, "no agent is waiting for approval %s: its run was stopped or the supervisor restarted; the agent asks again after it resumes", id)
	}
	resumes := r.Option == domain.AnswerResume && row.Cause != domain.CauseRunFailed && row.RunID != ""
	var sl *slot
	unlock := func() {}
	if resumes {
		if !initiation.Valid(ctx) {
			return initiation.ErrNotInitiated
		}
		if err := s.executionAdmission(ctx, row.TaskID, row.RunID); err != nil {
			return err
		}
		// An environment this process did not start is stopped and started first;
		// a failed stop launches nothing and leaves the answer open (#216).
		if err := s.freshenForResume(ctx, row.TaskID, row.RunID, true); err != nil {
			return err
		}
		// One live agent per run (design 4.1): the answer holds the run's lock
		// like Resume and recovery, and is refused while an agent is attached.
		var lerr error
		if unlock, lerr = s.lockRun(ctx, row.RunID); lerr != nil {
			return lerr
		}
		defer unlock() // released after the answer is saved, before the launch (#225)
		// A failed read stops the answer: the gate must not fail open.
		agg, lerr := s.loadTask(ctx, row.TaskID)
		if lerr != nil {
			return lerr
		}
		if r, ok := agg.Run(row.RunID); ok {
			if err := s.checkEnvFree(ctx, r.EnvID, r.ID); err != nil {
				return err
			}
		}
		var berr error
		if sl, berr = s.begin(row.RunID); berr != nil { // the run is about to start: it is not lost
			return domain.NewConflict(domain.RuleTransition, "run %s is already running", row.RunID)
		}
	}
	// An answer that cancels may save its run terminal: its environment counts as
	// busy from before that until the agent stop and its fallback have ended (#238).
	hold := &stopHold{s: s}
	defer hold.drop() // a no-op once stopAgent has taken it
	var cancelEnv domain.ID
	// Only an answer the domain turns into a cancel of the task (a cause) ends the run.
	if r.Option == domain.AnswerCancel && row.RunID != "" && row.Cause != "" {
		if agg, lerr := s.loadTask(ctx, row.TaskID); lerr == nil {
			if run, ok := agg.Run(row.RunID); ok && ownsAgent(run.State) {
				cancelEnv = run.EnvID
				hold.set(cancelEnv)
			}
		}
	}
	d, answered, err := s.store.RespondDecision(ctx, id, r)
	unlock()
	s.publish(answered)
	if err == nil {
		s.deliverApproval(d)
	}
	if err != nil {
		hold.drop()
		if sl != nil {
			s.end(row.RunID, sl)
		}
		return err
	}
	ctx = initiation.DecisionAnswer(ctx, id)
	if row.Cause == domain.CauseFeatureSource && row.Feature != "" {
		// The run continues even if keeping the answer failed: the Decision is closed.
		keepErr := s.keepFeatureAnswer(ctx, *row, r.Option)
		if keepErr != nil {
			keepErr = fmt.Errorf("keep the answer for %s: %w", row.Feature, keepErr)
		}
		contErr := s.continueEgress(ctx, row.TaskID, row.RunID)
		if contErr != nil {
			contErr = fmt.Errorf("start the run after the feature answers: %w", contErr)
		}
		if err := errors.Join(keepErr, contErr); err != nil {
			return err
		}
	}
	if row.Cause == domain.CauseEgressRequest && row.Host != "" {
		if err := s.keepEgressAnswer(ctx, *row, r.Option); err != nil {
			return fmt.Errorf("keep the answer for %s: %w", row.Host, err)
		}
		// The agent starts once the last request of its run is answered.
		if err := s.continueEgress(ctx, row.TaskID, row.RunID); err != nil {
			return fmt.Errorf("start the run after the egress answers: %w", err)
		}
	}
	switch {
	case r.Option == domain.AnswerCancel && cancelEnv != "":
		return s.stopAgent(ctx, row.TaskID, row.RunID, cancelEnv, "cancelled", "answer", true, hold.take())
	case r.Option == domain.AnswerCancel:
		s.stopSession(row.RunID)
	case resumes:
		err := s.launch(ctx, row.TaskID, row.RunID, sl)
		if errors.Is(err, agent.ErrNoSession) || errors.Is(err, errAttemptsUsedUp) {
			// The session is gone, or the attempts are used up: the run ends
			// failed and waits on a retry-or-cancel Decision.
			var rep Report
			return errors.Join(err, s.failRun(ctx, row.TaskID, row.RunID, &rep))
		}
		return err
	}
	return nil
}

// Cancel cancels a task: its live run is stopped, its Decisions are
// superseded and its agent session ends. A failed stop of the agent stops its
// environment instead (design 4.1, fifth path, issue #238); the environment counts
// as busy from before the run is saved until that has ended.
func (s *Service) Cancel(ctx context.Context, task domain.ID) error {
	return s.cancel(ctx, task, false)
}

// cancel is Cancel; agentStopped says kill-all has stopped the agent already.
func (s *Service) cancel(ctx context.Context, task domain.ID, agentStopped bool) error {
	return s.cancelReason(ctx, task, agentStopped, "human_cancellation")
}

func (s *Service) cancelReason(ctx context.Context, task domain.ID, agentStopped bool, reason string) error {
	var live, env domain.ID
	var leftover domain.ID // the environment of a paused or interrupted run, which may hold its agent
	hold := &stopHold{s: s}
	err := s.update(ctx, task, func(a *domain.TaskAggregate) error {
		live, env, leftover = "", "", ""
		hold.set("")
		if r, ok := a.LiveRun(); ok {
			live = r.ID
			if r.State == domain.RunPaused || r.State == domain.RunInterrupted {
				leftover = r.EnvID
			}
			if ownsAgent(r.State) && !agentStopped {
				env = r.EnvID
				hold.set(env)
			}
		}
		return a.CancelWithReason(reason)
	})
	if err != nil {
		hold.drop()
		return err
	}
	var serr error
	switch {
	case agentStopped:
		hold.drop()
	case env == "":
		s.stopSession(live)
	default:
		serr = s.stopAgent(ctx, task, live, env, "cancelled", "cancel", true, hold.take())
	}
	s.cancelStart(live)    // a postCreate or an agent start that is already running stops
	s.dropEgressWait(live) // a run still waiting for its egress answers never starts
	if leftover != "" {
		// An agent an earlier process left behind in a run that is not live
		// has no session to stop: stopping its environment reaches it (#216).
		s.freshMu.Lock()
		lerr := s.stopLeftover(ctx, task, leftover, false, nil)
		s.freshMu.Unlock()
		if lerr != nil {
			// The cancel is saved; the environment is still running and not
			// one this process started, so the reconciler's next pass stops it.
			serr = errors.Join(serr, fmt.Errorf("task %s is cancelled, but its environment was not stopped (the next reconciler pass tries again): %w", task, lerr))
		}
	}
	return serr
}

// stopSession stops a run's agent. When the session is not up yet it records the
// request on the run's slot, and attach stops the session as soon as it exists.
func (s *Service) stopSession(run domain.ID) { _ = s.stopSessionErr(run) }

// stopSessionErr is stopSession that reports a failed stop.
func (s *Service) stopSessionErr(run domain.ID) error {
	s.mu.Lock()
	sl := s.sessions[run]
	if sl == nil {
		s.mu.Unlock()
		return nil
	}
	s.disarmToolWatchdog(sl)
	sess := sl.sess
	if sess == nil {
		sl.stopRequested = true
	}
	s.mu.Unlock()
	if sess != nil {
		return sess.Stop(context.Background())
	}
	return nil
}

// agentEnv is the environment the supervisor adds for an agent process: the
// egress proxy's address, read from the runtime now because it changes with
// every start of the environment and is never stored. Without a sidecar there
// is no proxy and nothing is added. It holds no secret.
func (s *Service) agentEnv(ctx context.Context, env domain.ID) []string {
	info, err := s.rt.Inspect(ctx, string(env))
	if err != nil || info.Proxy == "" {
		if err != nil {
			s.report(fmt.Errorf("find the egress proxy of %s: %w", env, err))
		}
		return nil
	}
	return []string{"HTTPS_PROXY=" + info.Proxy, "HTTP_PROXY=" + info.Proxy, "NO_PROXY=localhost,127.0.0.1,::1"}
}
