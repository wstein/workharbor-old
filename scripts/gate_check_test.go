package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// st builds one status object as the Statuses API returns it.
func st(id int, context, state, login string) string {
	return fmt.Sprintf(`{"id":%d,"context":%q,"state":%q,"creator":{"login":%q}}`, id, context, state, login)
}

func gateCheck(t *testing.T, class, json string, logins ...string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	file := filepath.Join(t.TempDir(), "statuses.json")
	if err := os.WriteFile(file, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"gate-check.sh", class, file}, logins...)
	cmd := exec.CommandContext(t.Context(), "sh", args...) //nolint:gosec // fixed script
	cmd.Dir = "."
	cmd.Env = []string{"PATH=/usr/bin:/bin:/opt/homebrew/bin:/usr/local/bin", "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // direct exec error
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("gate-check.sh: %v", err)
	return -1, ""
}

func TestGateCheck(t *testing.T) {
	t.Parallel()
	arr := func(s ...string) string { return "[" + strings.Join(s, ",") + "]" }
	tests := []struct {
		name, class, json string
		logins            []string
		want              int
	}{
		{"no statuses ordinary", "ordinary", "[]", []string{"wstein"}, 1},
		{"no statuses carve-out", "carve-out", "[]", []string{"wstein"}, 1},
		{"both missing, other context", "ordinary", arr(st(1, "ci/other", "success", "wstein")), []string{"wstein"}, 1},
		{"wrong creator", "ordinary", arr(st(1, "review/opus", "success", "mallory")), []string{"wstein", "desk"}, 1},
		{"pending", "ordinary", arr(st(1, "review/sonnet", "pending", "wstein")), []string{"wstein"}, 1},
		{"failure", "ordinary", arr(st(1, "review/opus", "failure", "wstein")), []string{"wstein"}, 1},
		{"stale success superseded by failure", "carve-out", arr(st(2, "review/opus", "failure", "wstein"), st(1, "review/opus", "success", "wstein")), []string{"wstein"}, 1},
		{"stale success, newest listed last", "carve-out", arr(st(1, "review/opus", "success", "wstein"), st(2, "review/opus", "failure", "wstein")), []string{"wstein"}, 1},
		{"older success, newer pending", "carve-out", arr(st(1, "review/opus", "success", "wstein"), st(2, "review/opus", "pending", "wstein")), []string{"wstein"}, 1},
		{"older success, newer pending, newest first", "carve-out", arr(st(2, "review/opus", "pending", "wstein"), st(1, "review/opus", "success", "wstein")), []string{"wstein"}, 1},
		{"older success, newer pending (sonnet)", "ordinary", arr(st(1, "review/sonnet", "success", "wstein"), st(2, "review/sonnet", "pending", "wstein")), []string{"wstein"}, 1},
		{"opus pending, sonnet success on carve-out", "carve-out", arr(st(1, "review/sonnet", "success", "wstein"), st(2, "review/opus", "pending", "wstein")), []string{"wstein"}, 1},
		{"pending then newer success", "carve-out", arr(st(1, "review/opus", "pending", "wstein"), st(2, "review/opus", "success", "wstein")), []string{"wstein"}, 0},
		{"error state", "carve-out", arr(st(1, "review/opus", "error", "wstein")), []string{"wstein"}, 1},
		{"failure then newer success", "carve-out", arr(st(1, "review/opus", "failure", "wstein"), st(2, "review/opus", "success", "wstein")), []string{"wstein"}, 0},
		{"sonnet on carve-out", "carve-out", arr(st(1, "review/sonnet", "success", "wstein")), []string{"wstein"}, 1},
		{"opus on carve-out", "carve-out", arr(st(1, "review/opus", "success", "wstein")), []string{"wstein"}, 0},
		{"sonnet on ordinary", "ordinary", arr(st(1, "review/sonnet", "success", "wstein")), []string{"wstein"}, 0},
		{"opus on ordinary", "ordinary", arr(st(1, "review/opus", "success", "desk")), []string{"wstein", "desk"}, 0},
		{"sol high on ordinary", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "success", "wstein")), []string{"wstein"}, 0},
		{"sol high on carve-out", "carve-out", arr(st(1, "review/gpt-6.1-sol-high", "success", "wstein")), []string{"wstein"}, 0},
		{"sol medium on ordinary", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "success", "wstein")), []string{"wstein"}, 0},
		{"sol medium on carve-out", "carve-out", arr(st(1, "review/gpt-6.1-sol-medium", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/sol", "ordinary", arr(st(1, "review/sol", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/gpt-6.1-sol", "ordinary", arr(st(1, "review/gpt-6.1-sol", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/gpt-6.1-sol-low", "ordinary", arr(st(1, "review/gpt-6.1-sol-low", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/gpt-6.1-sol-xhigh", "ordinary", arr(st(1, "review/gpt-6.1-sol-xhigh", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/gpt-6.1-sol-unknown", "ordinary", arr(st(1, "review/gpt-6.1-sol-unknown", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/gpt-6-sol-high", "ordinary", arr(st(1, "review/gpt-6-sol-high", "success", "wstein")), []string{"wstein"}, 1},
		{"unknown context review/gpt-6.1-sol-high-extra", "ordinary", arr(st(1, "review/gpt-6.1-sol-high-extra", "success", "wstein")), []string{"wstein"}, 1},
		{"sol high wrong creator", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "success", "mallory")), []string{"wstein"}, 1},
		{"sol high pending", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "pending", "wstein")), []string{"wstein"}, 1},
		{"sol high newer pending", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "success", "wstein"), st(2, "review/gpt-6.1-sol-high", "pending", "wstein")), []string{"wstein"}, 1},
		{"sol high failure", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "failure", "wstein")), []string{"wstein"}, 1},
		{"sol high newer failure", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "success", "wstein"), st(2, "review/gpt-6.1-sol-high", "failure", "wstein")), []string{"wstein"}, 1},
		{"sol high error", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "error", "wstein")), []string{"wstein"}, 1},
		{"sol high newer error", "ordinary", arr(st(1, "review/gpt-6.1-sol-high", "success", "wstein"), st(2, "review/gpt-6.1-sol-high", "error", "wstein")), []string{"wstein"}, 1},
		{"sol high paginated newer pending", "ordinary", "[" + arr(st(2, "review/gpt-6.1-sol-high", "pending", "wstein")) + "," + arr(st(1, "review/gpt-6.1-sol-high", "success", "wstein")) + "]", []string{"wstein"}, 1},
		{"sol high paginated success", "ordinary", "[" + arr(st(1, "x", "success", "a")) + "," + arr(st(2, "review/gpt-6.1-sol-high", "success", "wstein")) + "]", []string{"wstein"}, 0},
		{"sol medium wrong creator", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "success", "mallory")), []string{"wstein"}, 1},
		{"sol medium pending", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "pending", "wstein")), []string{"wstein"}, 1},
		{"sol medium newer pending", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "success", "wstein"), st(2, "review/gpt-6.1-sol-medium", "pending", "wstein")), []string{"wstein"}, 1},
		{"sol medium failure", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "failure", "wstein")), []string{"wstein"}, 1},
		{"sol medium newer failure", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "success", "wstein"), st(2, "review/gpt-6.1-sol-medium", "failure", "wstein")), []string{"wstein"}, 1},
		{"sol medium error", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "error", "wstein")), []string{"wstein"}, 1},
		{"sol medium newer error", "ordinary", arr(st(1, "review/gpt-6.1-sol-medium", "success", "wstein"), st(2, "review/gpt-6.1-sol-medium", "error", "wstein")), []string{"wstein"}, 1},
		{"sol medium paginated newer pending", "ordinary", "[" + arr(st(2, "review/gpt-6.1-sol-medium", "pending", "wstein")) + "," + arr(st(1, "review/gpt-6.1-sol-medium", "success", "wstein")) + "]", []string{"wstein"}, 1},
		{"sol medium paginated success", "ordinary", "[" + arr(st(1, "x", "success", "a")) + "," + arr(st(2, "review/gpt-6.1-sol-medium", "success", "wstein")) + "]", []string{"wstein"}, 0},
		{"empty desk identity means only wstein", "ordinary", arr(st(1, "review/opus", "success", "desk")), []string{"wstein", ""}, 1},
		{"no allowed login at all", "ordinary", arr(st(1, "review/opus", "success", "")), []string{"", ""}, 1},
		{"paginated pages", "ordinary", "[" + arr(st(1, "x", "success", "a")) + "," + arr(st(2, "review/sonnet", "success", "wstein")) + "]", []string{"wstein"}, 0},
		{"login case differs", "ordinary", arr(st(1, "review/opus", "success", "WStein")), []string{"wstein"}, 1},
		{"not an array", "ordinary", `{"message":"Not Found"}`, []string{"wstein"}, 2},
		{"unknown class", "weird", "[]", []string{"wstein"}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, out := gateCheck(t, tc.class, tc.json, tc.logins...)
			if got != tc.want {
				t.Fatalf("exit %d, want %d: %s", got, tc.want, out)
			}
		})
	}
}

func TestGateClass(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, input, want string }{
		{"empty", "", "ordinary"},
		{"ordinary", "LICENSE\x00internal/version/x.go\x00", "ordinary"},
		{"carve-out", "LICENSE\x00.github/workflows/x.yml\x00", "carve-out"},
		{"newline in path", "LICENSE\ninternal/version/x.go\x00", "carve-out"},
		{"control byte in path", "LICENSE\x01\x00", "carve-out"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "sh", "gate-class.sh") //nolint:gosec // fixed script
			cmd.Dir = "."
			cmd.Stdin = strings.NewReader(tc.input)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGateClassTrFailsClosed(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tr"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "gate-class.sh") //nolint:gosec // fixed script
	cmd.Stdin = strings.NewReader("LICENSE\x00")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "carve-out" {
		t.Fatalf("got %q, want carve-out", got)
	}
}
