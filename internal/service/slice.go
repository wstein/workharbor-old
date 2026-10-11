package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/initiation"
	"github.com/wstein/workharbor/internal/store"
)

// ---- the live event bus ----

// subBuffer is how many events a subscriber may be behind before it is dropped.
const subBuffer = 256

type subscriber struct {
	ch   chan domain.Event
	task domain.ID
}

// bus delivers events to subscribers from memory. A subscriber that falls
// behind is closed rather than waited for: it reconnects with the last
// sequence number it saw and replays the durable events (design §5.3).
type bus struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func (b *bus) add(task domain.ID) *subscriber {
	s := &subscriber{ch: make(chan domain.Event, subBuffer), task: task}
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[*subscriber]struct{}{}
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *bus) remove(s *subscriber) {
	b.mu.Lock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
	}
	b.mu.Unlock()
}

func (b *bus) publish(events ...domain.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range events {
		for s := range b.subs {
			if s.task != "" && s.task != e.TaskID {
				continue
			}
			select {
			case s.ch <- e:
			default: // too slow: drop it
				delete(b.subs, s)
				close(s.ch)
			}
		}
	}
}

// publish hands saved events to subscribers and keeps the board in step. The
// caller has committed them.
func (s *Service) publish(events []domain.Event) {
	s.bus.publish(events...)
	s.mirror(events)
}

// PublishEphemeral sends an event to subscribers of a task from memory, with no
// sequence number, and never stores it: token deltas and heartbeats (§5.4). Its
// payload is redacted like a stored one first: a token the agent prints must not
// reach a client just because the event is not kept (threat model T9).
func (s *Service) PublishEphemeral(task domain.ID, kind domain.EventKind, payload []byte) {
	s.bus.publish(domain.Event{TaskID: task, Kind: kind, Tier: domain.TierEphemeral, Payload: s.store.Redact(payload), At: s.clock.Now()})
}

// record stores one observation of the agent as a transcript-tier event and
// publishes it. A message or a tool call that cannot be stored is reported, not
// fatal: the run goes on.
func (s *Service) record(ctx context.Context, task domain.ID, e agent.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		s.report(fmt.Errorf("transcript event: %w", err))
		return
	}
	saved, err := s.store.Append(ctx, domain.NewTranscriptEvent(task, payload, e.At))
	if err != nil {
		s.report(fmt.Errorf("transcript event: %w", err))
		return
	}
	s.publish(saved)
}

// Subscribe returns the events of a task after sequence number since: first the
// durable ones from the store, then the live ones as they happen, among them
// ephemeral events that have no sequence number. The store is the buffer, so a
// long history is replayed at the client's pace, and a client that falls too
// far behind the live feed is caught up from the store again, without losing a
// durable event (ephemeral ones it missed are gone, by design). The channel is
// closed when ctx ends or when the store fails.
func (s *Service) Subscribe(ctx context.Context, task domain.ID, since int64) (<-chan domain.Event, error) {
	if _, err := s.store.EventsSince(ctx, task, since, 1); err != nil {
		return nil, err
	}
	out := make(chan domain.Event, subBuffer)
	sub := s.bus.add(task) // registered before Subscribe returns, so an event published after it is not lost
	go func() {
		defer close(out)
		last := since
		send := func(e domain.Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			if sub == nil {
				sub = s.bus.add(task) // again before the replay, so nothing between the two is lost
			}
			for {
				page, err := s.store.EventsSince(ctx, task, last, 500)
				if err != nil {
					s.bus.remove(sub)
					if ctx.Err() == nil {
						s.report(fmt.Errorf("subscribe %s: %w", task, err))
					}
					return
				}
				for _, e := range page {
					if !send(e) {
						s.bus.remove(sub)
						return
					}
					last = e.Seq
				}
				if len(page) < 500 {
					break
				}
			}
			dropped := false
			for !dropped {
				select {
				case <-ctx.Done():
					s.bus.remove(sub)
					return
				case e, ok := <-sub.ch:
					if !ok {
						dropped, sub = true, nil // the bus let go of a slow subscriber: catch up from the store
						break
					}
					if e.Seq != 0 && e.Seq <= last {
						continue // already replayed
					}
					if e.Seq != 0 {
						last = e.Seq
					}
					if !send(e) {
						s.bus.remove(sub)
						return
					}
				}
			}
		}
	}()
	return out, nil
}

// ---- say, list, show ----

// Say sends a message to the task's running agent and returns how it was
// delivered (design §5.3): an agent without mid-run instruction answers
// `resumed_turn`, and that is what is shown, never an injection it cannot do.
// The message and its delivery are recorded as an audit event. A task with no
// attached session is a conflict.
func (s *Service) Say(ctx context.Context, task domain.ID, message string) (agent.Delivery, error) {
	agg, err := s.store.LoadTask(ctx, task)
	if err != nil {
		return "", err
	}
	run, ok := agg.LiveRun()
	if !ok || run.State != domain.RunRunning {
		return "", domain.NewConflict(domain.RuleRunLive, "task %s has no running run to send a message to", task)
	}
	s.mu.Lock()
	sl := s.sessions[run.ID]
	s.mu.Unlock()
	if sl == nil || sl.sess == nil {
		return "", domain.NewConflict(domain.RuleRunLive, "run %s has no attached session", run.ID)
	}
	delivery, err := sl.sess.Instruct(initiation.ForRun(ctx, task, run.ID), message)
	if err != nil {
		return "", err
	}
	saved, aerr := s.store.Append(ctx, domain.NewInstructionEvent(task, run.ID, string(delivery), message, s.clock.Now()))
	if aerr != nil {
		return delivery, fmt.Errorf("the message was sent (%s) but its record failed: %w", delivery, aerr)
	}
	s.publish(saved)
	return delivery, nil
}

// List returns a summary of every task, newest first.
func (s *Service) List(ctx context.Context, onlyActive bool) ([]store.TaskSummary, error) {
	return s.store.Tasks(ctx, onlyActive)
}

// TaskView is one task as `whr show` and the API present it.
type TaskView struct {
	Task      domain.Task
	Runs      []domain.Run
	Open      []domain.Decision // the Decisions waiting for the human
	Candidate *domain.ReviewCandidate
	Agent     string // "<workspace>/<role>", empty for a task made before agents
	// AgentMayRun lists the runs whose agent stop and environment stop failed
	// where no human call waited (a suspension, a budget stop, kill-all): the
	// agent may still run (design 4.1, issue #238).
	AgentMayRun []domain.AgentMayRun
	// Check is the receipt of the check that ran on the current revision (D51,
	// §4.5), nil when none ran for it: which commit was checked, by what and with
	// what result. Its output is the agent's data and untrusted.
	Check *domain.CheckReceipt
	// PublishAttempts are the failed publish steps of the current revision, oldest
	// first: a transport fault that left the publish outstanding, or the refusal
	// that ended it (D51, §4.5).
	PublishAttempts []domain.PublishAttempt
}

// Show returns a task with its runs, its open Decisions and its current
// revision. An unknown task is a *domain.NotFoundError.
func (s *Service) Show(ctx context.Context, task domain.ID) (TaskView, error) {
	agg, err := s.store.LoadTask(ctx, task)
	if err != nil {
		return TaskView{}, err
	}
	v := TaskView{Task: agg.Task(), Runs: agg.Runs()}
	if id := agg.Task().AgentID; id != "" {
		if a, err := s.store.Agent(ctx, id); err == nil {
			if w, err := s.store.Workspace(ctx, string(a.WorkspaceID)); err == nil {
				v.Agent = w.Name + "/" + a.Role
			}
		}
	}
	for _, d := range agg.Decisions() {
		if d.Status == domain.DecisionOpen {
			v.Open = append(v.Open, d)
		}
	}
	if c, ok := agg.CurrentCandidate(); ok {
		v.Candidate = &c
	}
	if v.AgentMayRun, err = s.agentNotices(ctx, task); err != nil {
		return TaskView{}, err
	}
	if v.Candidate != nil {
		if v.Check, v.PublishAttempts, err = s.publishRecords(ctx, task, v.Candidate.SHA); err != nil {
			return TaskView{}, err
		}
	}
	// A check that failed belongs to a revision that was never pinned, so the
	// current candidate has no receipt for it: with prepare_failed open, show the
	// failed check's own receipt instead of the previous revision's or none.
	if q, ok := agg.OpenCause(domain.CausePrepareFailed); ok {
		rc, err := s.failedCheckReceipt(ctx, task, q)
		if err != nil {
			return TaskView{}, err
		}
		if rc != nil {
			v.Check = rc
		}
	}
	return v, nil
}

// Inbox returns the Decisions waiting for the human, oldest first.
func (s *Service) Inbox(ctx context.Context) ([]domain.Decision, error) {
	rows, err := s.store.InboxDecisions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Decision, len(rows))
	for i, d := range rows {
		out[i] = *d
	}
	return out, nil
}

// WorkspaceView is a workspace with its agents, for lists and completion.
type WorkspaceView struct {
	Workspace domain.Workspace
	Agents    []domain.Agent
}

// WorkspaceList returns every workspace with its agents.
func (s *Service) WorkspaceList(ctx context.Context) ([]WorkspaceView, error) {
	list, err := s.store.Workspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceView, 0, len(list))
	for _, w := range list {
		agents, err := s.store.Agents(ctx, w.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, WorkspaceView{Workspace: w, Agents: agents})
	}
	return out, nil
}

// Log returns the durable events of a task after sequence number since, at
// most limit of them (1 to 1000), oldest first. It is the non-streaming form of
// Subscribe: ephemeral events are not in it. An unknown task is a
// *domain.NotFoundError.
func (s *Service) Log(ctx context.Context, task domain.ID, since int64, limit int) ([]domain.Event, error) {
	if _, err := s.store.LoadTask(ctx, task); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return s.store.EventsSince(ctx, task, since, limit)
}
