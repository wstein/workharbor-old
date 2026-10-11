package codex

import (
	"errors"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/agent"
)

// These synthetic translator-contract tests are not native wire-schema or
// pinned target protocol evidence. Raw notification intake stays unsupported.
func outputTranslatorFixture(t *testing.T) (*commandOutputTranslator, *state) {
	t.Helper()
	owner := newState("thread", "turn", "synthetic-model")
	owner.items["command"] = nativeItem{ID: "command", Type: "commandExecution", Status: "inProgress"}
	translator, err := newCommandOutputTranslator(owner, "generation")
	if err != nil {
		t.Fatal(err)
	}
	return translator, owner
}

func TestCommandOutputTranslatorPreservesIdentityAndActualByteCount(t *testing.T) {
	for _, delta := range []string{"hello", " \n\t", "é🙂", strings.Repeat("x", maxMessage)} {
		translator, owner := outputTranslatorFixture(t)
		beforeBytes := owner.itemBytes
		events, err := translator.translate(commandOutputObservation{"thread", "turn", "command", delta})
		if err != nil || len(events) != 1 {
			t.Fatalf("output events=%d, err=%v", len(events), err)
		}
		event := events[0]
		if event.Kind != agent.EventToolOutput || event.SessionID != "thread" || event.Watchdog == nil {
			t.Fatalf("output event=%+v", event)
		}
		if event.Watchdog.Generation != "generation" || event.Watchdog.ToolCallID != "command" || event.Watchdog.OutputBytes != int64(len(delta)) || event.Watchdog.WaitID != "" {
			t.Fatalf("output metadata=%+v", event.Watchdog)
		}
		if event.Text != "" || event.Input != "" || event.Tool != "" {
			t.Fatal("translator retained output or invented transcript metadata")
		}
		if owner.itemBytes != beforeBytes || owner.items["command"].AggregatedOutput != nil || len(owner.items) != 1 || len(owner.completed) != 0 {
			t.Fatal("output mutated or hydrated native ownership")
		}
	}
	translator, _ := outputTranslatorFixture(t)
	events, err := translator.translate(commandOutputObservation{"thread", "turn", "command", ""})
	if err != nil || len(events) != 0 {
		t.Fatalf("empty output=%+v, %v", events, err)
	}
}

func TestCommandOutputTranslatorRefusesUnownedOrStaleOutput(t *testing.T) {
	for _, change := range []string{"thread", "turn", "missing", "wrong kind", "completed", "terminal", "changed owner", "mismatched item", "not running", "invalid utf8", "oversize", "malformed id"} {
		t.Run(change, func(t *testing.T) {
			translator, owner := outputTranslatorFixture(t)
			observation := commandOutputObservation{"thread", "turn", "command", "output"}
			switch change {
			case "thread":
				observation.threadID = "other"
			case "turn":
				observation.turnID = "other"
			case "missing":
				observation.itemID = "missing"
			case "wrong kind":
				owner.items["command"] = nativeItem{ID: "command", Type: "agentMessage", Status: "inProgress"}
			case "completed":
				owner.completed["command"] = true
			case "terminal":
				owner.result = &agent.Result{Status: agent.ResultCompleted}
			case "changed owner":
				owner.turn = "successor"
			case "mismatched item":
				owner.items["command"] = nativeItem{ID: "other", Type: "commandExecution", Status: "inProgress"}
			case "not running":
				owner.items["command"] = nativeItem{ID: "command", Type: "commandExecution", Status: "completed"}
			case "invalid utf8":
				observation.delta = string([]byte{0xff})
			case "oversize":
				observation.delta = strings.Repeat("x", maxMessage+1)
			case "malformed id":
				observation.itemID = "bad\nidentity"
			}
			events, err := translator.translate(observation)
			if !errors.Is(err, errProtocol) || len(events) != 0 {
				t.Fatalf("accepted invalid output: %d events, %v", len(events), err)
			}
		})
	}
}

func TestCommandOutputTranslatorValidatesGenerationAndOwner(t *testing.T) {
	for _, generation := range []string{"", "white space", string([]byte{0xff}), strings.Repeat("x", agent.MaxWatchdogIDBytes+1)} {
		if _, err := newCommandOutputTranslator(newState("thread", "turn", "model"), generation); !errors.Is(err, errProtocol) {
			t.Fatal("accepted invalid generation")
		}
	}
	if _, err := newCommandOutputTranslator(nil, "generation"); !errors.Is(err, errProtocol) {
		t.Fatal("accepted nil owner")
	}
	for _, id := range []string{"", "bad\nidentity", strings.Repeat("x", agent.MaxWatchdogIDBytes+1)} {
		if _, err := newCommandOutputTranslator(newState(id, "turn", "model"), "generation"); !errors.Is(err, errProtocol) {
			t.Fatal("accepted invalid thread")
		}
		if _, err := newCommandOutputTranslator(newState("thread", id, "model"), "generation"); !errors.Is(err, errProtocol) {
			t.Fatal("accepted invalid turn")
		}
	}
}

func TestCommandOutputTranslatorDoesNotEnableNativeWatchdogOrRawIntake(t *testing.T) {
	translator, owner := outputTranslatorFixture(t)
	if _, err := translator.translate(commandOutputObservation{"thread", "turn", "command", "output"}); err != nil {
		t.Fatal(err)
	}
	if fixtureAdapter(nil).Capabilities().ToolWatchdog {
		t.Fatal("output translator enabled unverified watchdog")
	}
	if _, err := owner.observe(nativeMessage(t, "item/commandExecution/outputDelta", `{"threadId":"thread","turnId":"turn","itemId":"command","delta":"output"}`)); !errors.Is(err, errProtocol) {
		t.Fatal("unmeasured raw notification intake enabled")
	}
	if _, ok := any(translator).(agent.ToolWatchdogSession); ok {
		t.Fatal("output translator claims cancellation session contract")
	}
}

func TestNativeSessionCommandOutputTranslatorHasImmutableGeneration(t *testing.T) {
	runner := &fixtureRunner{startTurn: make(chan struct{})}
	attached, err := fixtureAdapter(runner).launch(t.Context(), fixtureSpec(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = attached.Stop(t.Context()); _, _ = attached.Wait() })
	session := attached.(*session)
	if session.commandOutput == nil || session.commandOutput.generation != session.generation || session.commandOutput.generation == "" || session.commandOutput.owner != session.state {
		t.Fatal("output translator missing session-local generation before run")
	}
}
