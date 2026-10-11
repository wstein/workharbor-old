package codex

import (
	"unicode/utf8"

	"github.com/wstein/workharbor/internal/agent"
)

// commandOutputObservation is decoded semantic input, not a native JSON wire
// schema. Raw outputDelta notification intake remains unsupported until the
// exact authorized target schema is retained and checked.
type commandOutputObservation struct {
	threadID, turnID, itemID, delta string
}

// commandOutputTranslator binds output to one immutable session generation and
// the existing native ownership state. Callers serialize translate with native
// observations under session.order. It retains no output or independent ledger.
// Output alone never establishes an execution start or cancellation capability.
type commandOutputTranslator struct {
	owner                    *state
	generation, thread, turn string
}

func newCommandOutputTranslator(owner *state, generation string) (*commandOutputTranslator, error) {
	if owner == nil || owner.result != nil {
		return nil, errProtocol
	}
	for _, id := range []string{owner.thread, owner.turn} {
		if (agent.ToolWatchdogEvent{Generation: generation, ToolCallID: id}).Validate(agent.EventToolExecutionStarted) != nil {
			return nil, errProtocol
		}
	}
	return &commandOutputTranslator{owner: owner, generation: generation, thread: owner.thread, turn: owner.turn}, nil
}

func (translator *commandOutputTranslator) translate(observation commandOutputObservation) ([]agent.Event, error) {
	owner := translator.owner
	if owner.result != nil || owner.thread != translator.thread || owner.turn != translator.turn || observation.threadID != translator.thread || observation.turnID != translator.turn {
		return nil, errProtocol
	}
	item, exists := owner.items[observation.itemID]
	if !exists || item.ID != observation.itemID || item.Type != "commandExecution" || item.Status != "inProgress" || owner.completed[observation.itemID] || len(observation.delta) > maxMessage || !utf8.ValidString(observation.delta) {
		return nil, errProtocol
	}
	identity := agent.ToolWatchdogEvent{Generation: translator.generation, ToolCallID: observation.itemID}
	// Validate identity even for empty output; empty output is never progress.
	if identity.Validate(agent.EventToolExecutionStarted) != nil {
		return nil, errProtocol
	}
	if len(observation.delta) == 0 {
		return nil, nil
	}
	identity.OutputBytes = int64(len(observation.delta))
	event := owner.event(agent.EventToolOutput)
	event.Watchdog = &identity
	return []agent.Event{event}, nil
}
