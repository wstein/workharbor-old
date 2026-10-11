package initiation

import (
	"context"
	"errors"
	"testing"

	"github.com/wstein/workharbor/internal/agent"
)

type watchdogAdapter struct {
	agent.Adapter
	caps agent.Capabilities
}

func (a watchdogAdapter) Capabilities() agent.Capabilities { return a.caps }

type watchdogSessionFixture struct {
	agent.Session
	aborted string
	stopped bool
}

func (*watchdogSessionFixture) WatchdogGeneration() string { return "generation" }
func (s *watchdogSessionFixture) AbortTool(_ context.Context, id string) (agent.ToolAbortResult, error) {
	s.aborted = id
	return agent.ToolAbortConfirmed, nil
}
func (s *watchdogSessionFixture) Stop(context.Context) error { s.stopped = true; return nil }

type watchdogPauserFixture struct{ *watchdogSessionFixture }

func (*watchdogPauserFixture) Pause(context.Context) error    { return nil }
func (*watchdogPauserFixture) Continue(context.Context) error { return nil }

type badGenerationFixture struct{ *watchdogSessionFixture }

func (*badGenerationFixture) WatchdogGeneration() string { return "bad generation" }

func TestGateDoesNotInventWatchdogForMismatchedOrInvalidSessions(t *testing.T) {
	caps := agent.Capabilities{ToolWatchdog: true}
	gate := New(watchdogAdapter{caps: caps}, nil)
	for _, sess := range []agent.Session{&watchdogSessionFixture{Session: nil}, &badGenerationFixture{}, &session{}} {
		// The first fixture is valid; the others lack a valid optional contract.
		wrapped := gate.wrap(sess)
		_, err := agent.WatchdogSession(caps, wrapped)
		_, valid := sess.(*watchdogSessionFixture)
		if valid && err != nil {
			t.Fatal(err)
		}
		if !valid && !errors.Is(err, agent.ErrUnsupported) {
			t.Fatal("wrapper invented supported optional coverage")
		}
	}
}

func TestGatePreservesOnlySupportedOptionalWatchdog(t *testing.T) {
	for _, cooperative := range []bool{false, true} {
		for _, supported := range []bool{false, true} {
			raw := &watchdogSessionFixture{}
			var sess agent.Session = raw
			if cooperative {
				sess = &watchdogPauserFixture{raw}
			}
			caps := agent.Capabilities{ToolWatchdog: supported}
			gate := New(watchdogAdapter{caps: caps}, nil)
			wrapped := gate.wrap(sess)
			w, err := agent.WatchdogSession(caps, wrapped)
			if supported {
				if err != nil {
					t.Fatalf("optional interface stripped, cooperative=%v: %v", cooperative, err)
				}
				result, err := w.AbortTool(context.Background(), "tool")
				if err != nil || result != agent.ToolAbortConfirmed || raw.aborted != "tool" || raw.stopped {
					t.Fatal("tool-only abort was changed or turned into session stop")
				}
			} else if !errors.Is(err, agent.ErrUnsupported) {
				t.Fatal("unsupported interface became available")
			}
			if _, err := wrapped.Instruct(context.Background(), "message"); !errors.Is(err, ErrNotInitiated) {
				t.Fatal("optional interface bypassed instruction initiation")
			}
			p, ok := wrapped.(agent.Pauser)
			if ok != cooperative {
				t.Fatal("optional watchdog changed pause capability")
			}
			if ok && !errors.Is(p.Continue(context.Background()), ErrNotInitiated) {
				t.Fatal("optional watchdog bypassed resume initiation")
			}
		}
	}
}
