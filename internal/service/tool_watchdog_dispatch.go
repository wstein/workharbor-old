package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
)

type runToolWatchdog struct {
	task, run     domain.ID
	attachedAfter int64 // durable task event watermark; older publication belongs to history
	session       agent.ToolWatchdogSession
	state         *toolWatchdogState
	ctx           context.Context
	cancel        context.CancelFunc
	pending       map[string]bool // guarded by Service.mu, one dispatch worker per tool
}

func (s *Service) newRunToolWatchdog(task, run domain.ID, sess agent.Session) (*runToolWatchdog, error) {
	caps := s.ag.Capabilities()
	if !caps.ToolWatchdog {
		return nil, nil
	}
	w, err := agent.WatchdogSession(caps, sess)
	if err != nil {
		return nil, err
	}
	state, err := newToolWatchdogState(w.WatchdogGeneration(), s.cfg.ToolWatchdog)
	if err != nil {
		return nil, err
	}
	attachedAfter, err := s.store.LatestEventSeq(s.bg, task)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.bg)
	return &runToolWatchdog{task: task, run: run, attachedAfter: attachedAfter, session: w, state: state, ctx: ctx, cancel: cancel, pending: map[string]bool{}}, nil
}

// disarmToolWatchdog requires s.mu; cancellation never cancels the session.
func (s *Service) disarmToolWatchdog(sl *slot) {
	if sl != nil && sl.watchdog != nil {
		sl.watchdog.state.disarm()
		sl.watchdog.cancel()
	}
}

func (s *Service) observeToolWatchdog(sl *slot, e agent.Event) {
	s.mu.Lock()
	m := sl.watchdog
	if m == nil || s.closing || s.sessions[m.run] != sl {
		s.mu.Unlock()
		return
	}
	if e.Kind == agent.EventAuthExpired || e.Kind == agent.EventQuotaExhausted {
		s.disarmToolWatchdog(sl)
		s.mu.Unlock()
		return
	}
	switch e.Kind {
	case agent.EventToolExecutionStarted, agent.EventToolOutput, agent.EventToolExecutionEnded, agent.EventToolWaitStarted, agent.EventToolWaitEnded:
		err := m.state.observe(e.Kind, e.Watchdog, s.clock.Now())
		s.mu.Unlock()
		s.report(err)
	default:
		s.mu.Unlock()
	}
}

func (s *Service) disarmChangedToolWatchdogs(events []domain.Event) {
	for _, e := range events {
		if e.Kind != domain.EventRunState {
			continue
		}
		var change domain.StateChanged
		if json.Unmarshal(e.Payload, &change) != nil || change.To == string(domain.RunRunning) {
			continue
		}
		s.mu.Lock()
		sl := s.sessions[change.ID]
		if sl != nil && sl.watchdog != nil && sl.watchdog.task == e.TaskID && e.Seq > sl.watchdog.attachedAfter {
			s.disarmToolWatchdog(sl)
		}
		s.mu.Unlock()
	}
}

func (s *Service) checkToolWatchdogs(ctx context.Context) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	for _, sl := range s.sessions {
		m := sl.watchdog
		if m == nil {
			continue
		}
		for _, action := range m.state.due(s.clock.Now()) {
			if m.pending[action.toolID] {
				continue
			}
			m.pending[action.toolID] = true
			s.wg.Add(1)
			go func(sl *slot, m *runToolWatchdog, a toolWatchdogAction) {
				defer s.wg.Done()
				defer func() { s.mu.Lock(); delete(m.pending, a.toolID); s.mu.Unlock() }()
				s.dispatchToolWatchdog(ctx, sl, m, a)
			}(sl, m, action)
		}
	}
	s.mu.Unlock()
}

func (s *Service) dispatchToolWatchdog(ctx context.Context, sl *slot, m *runToolWatchdog, a toolWatchdogAction) {
	if ctx.Err() != nil || m.ctx.Err() != nil {
		return
	}
	// Adapter calls and database reads happen outside service and state locks.
	if m.session.WatchdogGeneration() != m.state.generation {
		m.state.disarm()
		m.cancel()
		s.report(fmt.Errorf("tool watchdog generation changed"))
		return
	}
	agg, err := s.store.LoadTask(ctx, m.task)
	if err != nil {
		s.report(err)
		return
	}
	run, ok := agg.Run(m.run)
	if !ok || run.State != domain.RunRunning {
		m.state.disarm()
		m.cancel()
		return
	}
	s.mu.Lock()
	claimed := ctx.Err() == nil && !s.closing && s.sessions[m.run] == sl && sl.watchdog == m && !sl.stopRequested && m.ctx.Err() == nil && m.state.claim(a, s.clock.Now())
	s.mu.Unlock()
	if !claimed {
		return
	}
	// The claim is the dispatch boundary. Subsequent outcomes belong to this
	// original identity even if output arrives or ownership changes during I/O.
	kind := domain.EventToolHung
	if a.abort {
		abortCtx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		result, abortErr := m.session.AbortTool(abortCtx, a.toolID)
		cancel()
		switch result {
		case agent.ToolAbortConfirmed:
			m.state.finished(a)
			kind = domain.EventToolAborted
		case agent.ToolAbortFinished:
			m.state.finished(a)
			return
		default:
			s.report(fmt.Errorf("tool watchdog cancellation unconfirmed"))
			return
		}
		if abortErr != nil {
			s.report(fmt.Errorf("tool watchdog cancellation reported an error"))
		}
	}
	notice := domain.ToolWatchdogNotice{RunID: m.run, Generation: a.generation, ToolCallID: a.toolID, Episode: a.episode}
	saved, err := s.store.Append(context.Background(), domain.NewToolWatchdogNotice(m.task, kind, notice, s.clock.Now()))
	if err != nil {
		s.report(err)
		return
	}
	s.publish(saved)
	s.notify(context.Background(), saved)
}

// RunToolWatchdogs bounds observation latency, never run lifetime.
func (s *Service) RunToolWatchdogs(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkToolWatchdogs(ctx)
		}
	}
}
