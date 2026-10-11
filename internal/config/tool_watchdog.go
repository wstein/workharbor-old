package config

import (
	"fmt"
	"time"
)

// ToolWatchdog configures per-tool output silence. Omitted values select the
// defaults; explicitly configured durations must be positive with warning < abort.
type ToolWatchdog struct {
	WarnAfter  string `json:"warn_after,omitempty"`
	AbortAfter string `json:"abort_after,omitempty"`
}

// Thresholds parses and validates the warning and abort limits.
func (c ToolWatchdog) Thresholds() (warn, abort time.Duration, err error) {
	parse := func(name, value string, fallback time.Duration) (time.Duration, error) {
		if value == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("%s must be a positive Go duration", name)
		}
		return d, nil
	}
	warn, err = parse("warn_after", c.WarnAfter, 10*time.Minute)
	if err != nil {
		return 0, 0, err
	}
	abort, err = parse("abort_after", c.AbortAfter, 30*time.Minute)
	if err != nil {
		return 0, 0, err
	}
	if warn >= abort {
		return 0, 0, fmt.Errorf("warn_after must be less than abort_after")
	}
	return warn, abort, nil
}
