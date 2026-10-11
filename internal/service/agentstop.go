package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/wstein/workharbor/internal/domain"
)

// holdEnvBusy marks an environment busy until the returned release is called, as
// a check does (HoldEnvironment), without a lease or a check of the runs: a cancel,
// a budget stop or kill-all takes it before it saves its run terminal, so the
// environment does not count as free while the agent stop and its fallback are
// under way (design 4.1, fifth path; issue #238).
func (s *Service) holdEnvBusy(env domain.ID) (release func()) { return s.holdEnv(env, true) }

// holdEnv is holdEnvBusy; stop says the hold is an agent stop's, which a check's
// HoldEnvironment is refused beside. The hold of a run that stopped for a prepare
// (D51) is not: the check inside that prepare takes its own, counted beside it.
func (s *Service) holdEnv(env domain.ID, stop bool) (release func()) {
	s.rebuildMu.Lock()
	if s.holds == nil {
		s.holds = map[domain.ID]int{}
	}
	s.holds[env]++
	if stop {
		if s.stopHolds == nil {
			s.stopHolds = map[domain.ID]int{}
		}
		s.stopHolds[env]++
	}
	s.rebuildMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.rebuildMu.Lock()
			if s.holds[env]--; s.holds[env] <= 0 {
				delete(s.holds, env)
			}
			if stop {
				if s.stopHolds[env]--; s.stopHolds[env] <= 0 {
					delete(s.stopHolds, env)
				}
			}
			s.rebuildMu.Unlock()
		})
	}
}

// stopHold is a hold that a change function takes while it runs inside update,
// which may call it more than once: set follows the env of the run it sees now.
type stopHold struct {
	s     *Service
	env   domain.ID
	rel   func()
	plain bool // not an agent stop's hold: see holdEnv
}

// set holds env, or nothing when env is empty, and drops an earlier hold on another.
func (h *stopHold) set(env domain.ID) {
	if h.env == env {
		return
	}
	h.drop()
	if env != "" {
		h.env, h.rel = env, h.s.holdEnv(env, !h.plain)
	}
}

func (h *stopHold) drop() {
	if h.rel != nil {
		h.rel()
	}
	h.env, h.rel = "", nil
}

// take hands the hold on; the result is never nil.
func (h *stopHold) take() func() {
	rel := h.rel
	h.env, h.rel = "", nil
	if rel == nil {
		return func() {}
	}
	return rel
}

// ownsAgent reports whether a run in this state may have an agent this process
// launched: the states a cancel, a budget stop or kill-all saves terminal.
func ownsAgent(st domain.RunState) bool {
	return st == domain.RunStarting || st == domain.RunRunning
}

// stopAgent ends a run's agent on a path that saves the run terminal (a cancel, a
// budget stop, kill-all). The caller took the hold of the run's environment before
// it saved; stopAgent releases it when the agent stop and, if the stop failed, the
// fallback have ended. A failed agent stop stops the environment instead (design
// 4.1, fifth path): when that fails too, the run keeps its state, the process
// forgets its start mark and the error says the agent may still run. When no human
// call waits for the error (human false), or the stop is left to attach because
// the session is not up yet (the call has returned by then), it is also recorded as
// an event on the task, with path as its path. A session that is not up yet is
// stopped by attach, which then does the same; the hold goes with it.
func (s *Service) stopAgent(ctx context.Context, task, run, env domain.ID, state, path string, human bool, release func()) error {
	_, err := s.stopAgentWhenSaved(ctx, task, run, env, state, path, human, release, nil)
	return err
}

// stopAgentWhenSaved delays only a deferred notice until kill-all has saved its
// cancellation. An attached agent still stops before any task is cancelled.
func (s *Service) stopAgentWhenSaved(ctx context.Context, task, run, env domain.ID, state, path string, human bool, release func(), saved <-chan struct{}) (pending bool, err error) {
	ctx = context.WithoutCancel(ctx) // attach may run it after the call that asked has ended
	notice := func(serr error, deferred bool) error {
		err := s.stopEnvAfterAgent(ctx, task, env, state, serr)
		if agentMayRun(err) && (!human || deferred) {
			if saved != nil {
				<-saved
			}
			return errors.Join(err, s.recordAgentMayRun(ctx, task, run, env, path, err))
		}
		return err
	}
	s.mu.Lock()
	sl := s.sessions[run]
	if sl == nil {
		s.mu.Unlock()
		release()
		return false, nil
	}
	s.disarmToolWatchdog(sl)
	if sl.sess == nil {
		sl.stopRequested = true
		// No call waits for what attach finds, so it goes to the task as an event.
		sl.after, sl.release = func(serr error) error { return notice(serr, true) }, release
		s.mu.Unlock()
		return true, nil
	}
	sess := sl.sess
	s.mu.Unlock()
	defer release()
	if serr := sess.Stop(context.Background()); serr != nil {
		return false, notice(serr, false)
	}
	return false, nil
}

// recordAgentMayRun saves the audit entry of an agent that may still run.
func (s *Service) recordAgentMayRun(ctx context.Context, task, run, env domain.ID, path string, cause error) error {
	id := s.cfg.NewID()
	return s.update(ctx, task, func(a *domain.TaskAggregate) error {
		_, err := a.RaiseAgentMayRun(id, domain.AgentMayRun{RunID: run, EnvID: env, Path: path, Error: cause.Error()}, s.clock.Now())
		return err
	})
}

// agentNotices returns the runs of a task whose agent may still run.
func (s *Service) agentNotices(ctx context.Context, task domain.ID) ([]domain.AgentMayRun, error) {
	evs, err := s.store.EventsOfKind(ctx, task, domain.EventAgentMayRun)
	if err != nil {
		return nil, err
	}
	var out []domain.AgentMayRun
	for _, e := range evs {
		var a domain.AgentMayRun
		if json.Unmarshal(e.Payload, &a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
