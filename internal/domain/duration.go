package domain

import (
	"encoding/json"
	"time"
)

// StampRunBounds stamps only bounds represented by this change's durable events.
// Legacy rows without sufficient audit evidence remain unknown.
func (a *TaskAggregate) StampRunBounds(now time.Time) {
	for i := range a.events {
		if a.events[i].At.IsZero() {
			a.events[i].At = now.UTC()
		}
		e := a.events[i]
		at := e.At
		if at.IsZero() {
			at = now
		}
		if e.Kind == EventRunStarted {
			var p RunStarted
			if json.Unmarshal(e.Payload, &p) == nil {
				if r, err := a.run(p.RunID); err == nil && r.AdmittedAt.IsZero() {
					r.AdmittedAt = at.UTC()
				}
			}
		}
		if e.Kind == EventRunState {
			var p StateChanged
			if json.Unmarshal(e.Payload, &p) == nil && RunState(p.To).Terminal() {
				if r, err := a.run(p.ID); err == nil && r.EndedAt.IsZero() {
					r.EndedAt = at.UTC()
				}
			}
		}
	}
}
