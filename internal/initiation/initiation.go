// Package initiation enforces one human interaction for every agent send.
package initiation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/domain"
)

// ErrNotInitiated means no valid, unspent human initiation authorizes this send.
var ErrNotInitiated = errors.New("agent send needs a fresh human interaction")

// Marker is an in-memory capability. Its zero value authorizes nothing.
type Marker struct {
	id, kind, actor, channel string
	at                       time.Time
	decisionID               domain.ID
}

type (
	markerKey struct{}
	targetKey struct{}
	target    struct{ task, run domain.ID }
)

// UserAction mints a capability at a human API or web entry point.
func UserAction(actor, channel string) Marker {
	if actor == "" || (channel != "api" && channel != "web") {
		return Marker{}
	}
	var id [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Marker{}
	}
	return Marker{id: hex.EncodeToString(id[:]), kind: "user_action", actor: actor, channel: channel, at: time.Now().UTC()}
}

// With carries a marker across a request, including context.WithoutCancel.
func With(ctx context.Context, marker Marker) context.Context {
	return context.WithValue(ctx, markerKey{}, marker)
}

// From returns the marker on ctx, or the invalid zero marker.
func From(ctx context.Context) Marker { m, _ := ctx.Value(markerKey{}).(Marker); return m }

// Valid reports whether this context carries a human capability. Persistence
// still checks single use before a send is permitted.
func Valid(ctx context.Context) bool { return From(ctx).valid() }

func (m Marker) valid() bool {
	return m.id != "" && m.actor != "" && !m.at.IsZero() && (m.channel == "api" || m.channel == "web") && (m.kind == "user_action" || (m.kind == "decision_answer" && m.decisionID != ""))
}

// DecisionAnswer upgrades the same capability after a successful answer. It
// preserves the ID, so answering cannot turn one interaction into two sends.
func DecisionAnswer(ctx context.Context, decision domain.ID) context.Context {
	m := From(ctx)
	if !m.valid() || decision == "" {
		return With(ctx, Marker{})
	}
	m.kind, m.decisionID = "decision_answer", decision
	return With(ctx, m)
}

// ForRun binds the audit record to the run about to receive a send.
func ForRun(ctx context.Context, taskID, runID domain.ID) context.Context {
	return context.WithValue(ctx, targetKey{}, target{taskID, runID})
}

// Record is the spent capability and the send it authorizes; it contains no prompt.
type Record struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Actor      string    `json:"actor"`
	Channel    string    `json:"channel"`
	At         time.Time `json:"at"`
	DecisionID domain.ID `json:"decision_id,omitempty"`
	TaskID     domain.ID `json:"task_id"`
	RunID      domain.ID `json:"run_id"`
	Action     string    `json:"action"`
}

// Recorder atomically spends the marker and appends its audit event before
// returning. A reused marker returns ErrNotInitiated.
type Recorder interface {
	ConsumeInitiation(context.Context, Record) error
}

// Gate is the only service-owned adapter. All send methods fail closed.
type Gate struct {
	adapter  agent.Adapter
	recorder Recorder
}

// New wraps an adapter with a fail-closed initiation gate.
func New(adapter agent.Adapter, recorder Recorder) *Gate {
	return &Gate{adapter: adapter, recorder: recorder}
}

// Name implements agent.Adapter.
func (g *Gate) Name() string { return g.adapter.Name() }

// Capabilities implements agent.Adapter.
func (g *Gate) Capabilities() agent.Capabilities { return g.adapter.Capabilities() }

func (g *Gate) allow(ctx context.Context, action string) error {
	m := From(ctx)
	t, _ := ctx.Value(targetKey{}).(target)
	if !m.valid() || t.task == "" || t.run == "" || g.recorder == nil {
		return ErrNotInitiated
	}
	return g.recorder.ConsumeInitiation(ctx, Record{m.id, m.kind, m.actor, m.channel, m.at, m.decisionID, t.task, t.run, action})
}

// Start spends and audits the marker before starting the adapter.
func (g *Gate) Start(ctx context.Context, spec agent.StartSpec) (agent.Session, error) {
	if err := g.allow(ctx, "agent.start"); err != nil {
		return nil, err
	}
	s, err := g.adapter.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return g.wrap(s), nil
}

// Resume spends and audits the marker before resuming the adapter.
func (g *Gate) Resume(ctx context.Context, spec agent.StartSpec, id string) (agent.Session, error) {
	if err := g.allow(ctx, "agent.resume"); err != nil {
		return nil, err
	}
	s, err := g.adapter.Resume(ctx, spec, id)
	if err != nil {
		return nil, err
	}
	return g.wrap(s), nil
}

func (g *Gate) wrap(s agent.Session) agent.Session {
	if s == nil {
		return nil
	}
	wrapped := &session{Session: s, gate: g}
	w, watchdogErr := agent.WatchdogSession(g.Capabilities(), s)
	if p, ok := s.(agent.Pauser); ok {
		paused := &pauser{session: wrapped, pauser: p}
		if watchdogErr == nil {
			return &watchdogPauser{pauser: paused, ToolWatchdogSession: w}
		}
		return paused
	}
	if watchdogErr == nil {
		return &watchdogSession{session: wrapped, ToolWatchdogSession: w}
	}
	return wrapped
}

// The optional stop-only interface preserves the underlying implementation.
// Instruct and Continue still pass through their existing initiation gates.
type watchdogSession struct {
	*session
	agent.ToolWatchdogSession
}

type watchdogPauser struct {
	*pauser
	agent.ToolWatchdogSession
}

type session struct {
	agent.Session
	gate *Gate
}

func (s *session) Instruct(ctx context.Context, message string) (agent.Delivery, error) {
	if err := s.gate.allow(ctx, "agent.say"); err != nil {
		return "", err
	}
	return s.Session.Instruct(ctx, message)
}

type pauser struct {
	*session
	pauser agent.Pauser
}

func (p *pauser) Pause(ctx context.Context) error { return p.pauser.Pause(ctx) }

// Continue resumes a cooperative session and needs the same gate as Resume.
func (p *pauser) Continue(ctx context.Context) error {
	if err := p.gate.allow(ctx, "agent.resume"); err != nil {
		return err
	}
	return p.pauser.Continue(ctx)
}
