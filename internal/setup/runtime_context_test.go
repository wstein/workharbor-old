package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/doctor"
	"github.com/wstein/workharbor/internal/runlog"
)

func TestContextProbeNeverLogsMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, fail := range []bool{false, true} {
		t.Run("context", func(t *testing.T) {
			dir := t.TempDir()
			marker := "synthetic-context-private-value"
			bin := filepath.Join(dir, "docker")
			script := "#!/bin/sh\nprintf '%s\\n' '\"https://user:" + marker + "@example.invalid/socket?key=" + marker + "\"'\nprintf '%s\\n' '" + marker + "' >&2\n"
			if fail {
				script += "exit 1\n"
			}
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // executable fake command in a private temporary directory
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "run.log")
			log, err := runlog.Open(logPath)
			if err != nil {
				t.Fatal(err)
			}
			var verbose strings.Builder
			log.Stream = &verbose
			host := Terminal{Log: log, Probes: &Probes{}}
			d := doctor.Deps{ConfigPath: filepath.Join(dir, "missing.json"), Runner: host, LookPath: func(string) (string, error) { return bin, nil }, Getenv: func(k string) string {
				if k == "DOCKER_CONTEXT" {
					return "private-context-name"
				}
				return ""
			}, RuntimeSockets: []string{}}
			for _, c := range doctor.Checks(d) {
				if c.Name == "runtime" {
					got, detail := c.Run(context.Background())
					if got != doctor.NotVerified || strings.Contains(detail, marker) {
						t.Error("context response was not safely refused")
					}
				}
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(logPath) //nolint:gosec // log file under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data)+verbose.String(), marker) || strings.Contains(string(data)+verbose.String(), "private-context-name") {
				t.Fatal("raw context stdout or stderr reached run log")
			}
		})
	}
}

func TestContextCaptureBoundsAndCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ name, body string }{
		{"oversized", "printf '%5000s' x\n"},
		{"cancelled", "while :; do :; done\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tc.body), 0o700); err != nil { //nolint:gosec // executable fake command under t.TempDir
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			out, err := (Terminal{}).DockerContextEndpoint(ctx, bin, "")
			if err == nil || len(out) != 0 {
				t.Fatal("unsafe context capture was not refused")
			}
		})
	}
}
