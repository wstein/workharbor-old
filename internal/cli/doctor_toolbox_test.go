package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/config"
)

func TestDoctorExposesDockerToolboxReportInTextAndJSON(t *testing.T) {
	rig := newSetupRig(t)
	dir := t.TempDir()
	cfg := config.Config{
		Listen: "127.0.0.1:1", StateDir: filepath.Join(dir, "state"), APITokenFile: filepath.Join(dir, "token"),
		Roots:        config.Roots{Workspaces: []string{filepath.Join(dir, "workspace")}, ToolStore: filepath.Join(dir, "store")},
		GitHub:       config.GitHub{AppID: 1, KeyFile: filepath.Join(dir, "key")},
		Repositories: []config.Repository{{Name: "wstein/workharbor"}},
		Runtime:      config.Runtime{Backend: "docker", Endpoint: "unix:///tmp/unused.sock", Toolbox: dir},
	}
	for _, name := range []string{"workspace", "store", "state"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"token": "test-token", "key": "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(file); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		args := []string{"doctor", "--config", file, "--prefix", filepath.Dir(filepath.Dir(rig.exe))}
		if asJSON {
			args = append(args, "--json")
		}
		env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Getenv: func(key string) string {
			if key == "HOME" {
				return dir
			}
			return ""
		}, Setup: rig.env}
		_ = Execute(context.Background(), env, args)
		if !strings.Contains(stdout.String(), "bundledock-toolbox") || !strings.Contains(stdout.String(), "versions and provenance unverified") {
			t.Fatalf("report absent json=%v: %s", asJSON, stdout.String()[max(0, len(stdout.String())-1800):])
		}
		if asJSON {
			var report struct {
				Checks []struct{ Check, Status, Detail string }
			}
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, check := range report.Checks {
				if check.Check == "bundledock-toolbox" {
					found = true
					if check.Status != "not_verified" {
						t.Fatal(check)
					}
				}
			}
			if !found {
				t.Fatal("JSON checks omitted toolbox result")
			}
		} else {
			for _, line := range strings.Split(stdout.String(), "\n") {
				if strings.Contains(line, "bundledock-toolbox") && len(strings.Split(line, "\t")) != 4 {
					t.Fatal("text contract changed")
				}
			}
		}
	}
	if len(rig.host.ran) != 0 || rig.host.asked != 0 {
		t.Fatal("report executed a command or asked to install")
	}
}
