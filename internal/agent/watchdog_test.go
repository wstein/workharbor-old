package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type watchdogContractSession struct{ Session }

func (*watchdogContractSession) WatchdogGeneration() string { return "generation-1" }
func (*watchdogContractSession) AbortTool(context.Context, string) (ToolAbortResult, error) {
	return ToolAbortUnknown, nil
}

type invalidGenerationSession struct{ *watchdogContractSession }

func (*invalidGenerationSession) WatchdogGeneration() string { return "" }

func TestOptionalWatchdogRequiresCapabilityAndInterface(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps Capabilities
		sess Session
		want bool
	}{
		{"neither", Capabilities{}, nil, false},
		{"flag only", Capabilities{ToolWatchdog: true}, nil, false},
		{"interface only", Capabilities{}, &watchdogContractSession{}, false},
		{"both", Capabilities{ToolWatchdog: true}, &watchdogContractSession{}, true},
		{"typed nil", Capabilities{ToolWatchdog: true}, (*watchdogContractSession)(nil), false},
		{"invalid generation", Capabilities{ToolWatchdog: true}, &invalidGenerationSession{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WatchdogSession(tc.caps, tc.sess)
			if tc.want {
				if err != nil || got == nil {
					t.Fatalf("coverage missing: %v", err)
				}
			} else if got != nil || !errors.Is(err, ErrUnsupported) {
				t.Fatalf("unsupported coverage claimed: %v, %v", got, err)
			}
		})
	}
}

func TestWatchdogRejectsMalformedIdentityWithoutTruncation(t *testing.T) {
	for _, id := range []string{"", " ", "bad id", "bad\nidentity", "\x00", string([]byte{0xff}), strings.Repeat("x", MaxWatchdogIDBytes+1), strings.Repeat("é", 129)} {
		for _, field := range []string{"generation", "tool", "wait"} {
			t.Run(field+"/"+id, func(t *testing.T) {
				e := ToolWatchdogEvent{Generation: "g", ToolCallID: "t", WaitID: "w"}
				switch field {
				case "generation":
					e.Generation = id
				case "tool":
					e.ToolCallID = id
				case "wait":
					e.WaitID = id
				}
				original := e
				if err := e.Validate(EventToolWaitStarted); err == nil {
					t.Fatal("malformed identity accepted")
				}
				if e != original {
					t.Fatal("identity changed")
				}
			})
		}
	}
	// The byte bound accepts exact-length identities, including multibyte UTF-8.
	for _, id := range []string{strings.Repeat("x", MaxWatchdogIDBytes), strings.Repeat("é", 128)} {
		e := ToolWatchdogEvent{Generation: id, ToolCallID: id, WaitID: id}
		if err := e.Validate(EventToolWaitStarted); err != nil {
			t.Fatalf("valid boundary identity refused: %v", err)
		}
	}
}

func TestWatchdogObservationsHaveDistinctSemantics(t *testing.T) {
	for _, tc := range []struct {
		kind  EventKind
		wait  string
		bytes int64
		valid bool
	}{
		{EventToolExecutionStarted, "", 0, true},
		{EventToolExecutionEnded, "", 0, true},
		{EventToolOutput, "", 1, true},
		{EventToolOutput, "", 0, false},
		{EventToolOutput, "", -1, false},
		{EventToolWaitStarted, "w", 0, true},
		{EventToolWaitEnded, "w", 0, true},
		{EventToolWaitEnded, "", 0, false},
		{EventToolExecutionStarted, "w", 0, false},
		{EventToolExecutionEnded, "", 1, false},
		{EventToolCall, "", 0, false},
		{EventToolResult, "", 0, false},
	} {
		e := ToolWatchdogEvent{Generation: "g", ToolCallID: "t", WaitID: tc.wait, OutputBytes: tc.bytes}
		if err := e.Validate(tc.kind); (err == nil) != tc.valid {
			t.Errorf("%s wait=%q bytes=%d: %v", tc.kind, tc.wait, tc.bytes, err)
		}
	}
	if ToolAbortUnknown != 0 || ToolAbortUnknown == ToolAbortConfirmed || ToolAbortConfirmed == ToolAbortFinished {
		t.Fatal("abort outcome cannot distinguish unknown, confirmed and finished")
	}
	c := Capabilities{Headless: true, StructuredEvents: true, HostApprovals: true, MidRunInstruction: true}
	want := c.Mode()
	c.ToolWatchdog = true
	if c.Mode() != want || ContractVersion != 1 {
		t.Fatal("optional watchdog changed the existing contract version or mode")
	}
}
