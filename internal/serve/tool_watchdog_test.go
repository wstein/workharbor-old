package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/config"
)

func TestServiceConfigToolWatchdogThresholds(t *testing.T) {
	for _, tc := range []struct {
		name, warn, abort   string
		wantWarn, wantAbort time.Duration
	}{
		{"defaults", "", "", 10 * time.Minute, 30 * time.Minute},
		{"configured", "2m", "7m", 2 * time.Minute, 7 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{ToolWatchdog: config.ToolWatchdog{WarnAfter: tc.warn, AbortAfter: tc.abort}}
			got := serviceConfig(Deps{Config: &cfg}, func(string, ...any) {}).ToolWatchdog
			if got.WarnAfter != tc.wantWarn || got.AbortAfter != tc.wantAbort {
				t.Fatalf("thresholds = %+v", got)
			}
		})
	}
}

func TestRunRejectsInvalidToolWatchdogBeforeStarting(t *testing.T) {
	err := Run(context.Background(), Deps{Config: &config.Config{ToolWatchdog: config.ToolWatchdog{WarnAfter: "40m", AbortAfter: "30m"}}})
	if err == nil || !strings.Contains(err.Error(), "tool_watchdog") {
		t.Fatalf("invalid watchdog: %v", err)
	}
}
