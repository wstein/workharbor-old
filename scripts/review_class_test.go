package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/gittest"
)

// reviewClassRepo makes a repo with base commit "main" and a branch "topic"
// that adds the given files.
func reviewClassRepo(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // fixed git
		cmd.Dir = dir
		cmd.Env = gittest.Env(t.TempDir(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("switch", "-q", "-c", "topic")
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change")
	return dir
}

func TestReviewClass(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("review-class.sh")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{"ordinary", []string{"docs/guide.md", "README.md"}, []string{"class: ordinary", "required: review/sonnet or review/opus or review/gpt-6.1-sol-medium or review/gpt-6.1-sol-high"}},
		{"carve-out", []string{"scripts/x.sh"}, []string{"class: carve-out", "required: review/opus or review/gpt-6.1-sol-high"}},
		{"mixed", []string{"README.md", ".agents/dispatch.md"}, []string{"class: carve-out", "required: review/opus or review/gpt-6.1-sol-high"}},
		{"design mock readme", []string{"design/mock/README.md"}, []string{"class: carve-out", "required: review/opus or review/gpt-6.1-sol-high"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := reviewClassRepo(t, tc.files...)
			cmd := exec.CommandContext(t.Context(), "sh", script, "topic", "main") //nolint:gosec // fixed script
			cmd.Dir = dir
			cmd.Env = gittest.Env(t.TempDir())
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("review-class.sh: %v\n%s", err, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(out), w+"\n") {
					t.Errorf("output %q lacks %q", out, w)
				}
			}
		})
	}
}

// A base that looks like a git option must be a revision, never an option: with
// --output it would write the diff to a file and read as an empty, ordinary change.
func TestReviewClassTreatsAnOptionLikeBaseAsARevision(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("review-class.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := reviewClassRepo(t, "scripts/x.sh")
	cmd := exec.CommandContext(t.Context(), "sh", script, "topic", "--output=leak") //nolint:gosec // fixed script
	cmd.Dir = dir
	cmd.Env = gittest.Env(t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an option-like base was accepted: %s", out)
	}
	if strings.Contains("\n"+string(out), "\nclass: ") {
		t.Errorf("a class was printed for a bad base: %s", out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "leak")); statErr == nil {
		t.Error("git treated the base as --output and wrote a file")
	}
}

// The fail-closed paths: an unknown revision must exit 2 and print no class.
func TestReviewClassFailsClosedOnAnUnknownRevision(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("review-class.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := reviewClassRepo(t, "docs/guide.md")
	cmd := exec.CommandContext(t.Context(), "sh", script, "no-such-branch", "main") //nolint:gosec // fixed script
	cmd.Dir = dir
	cmd.Env = gittest.Env(t.TempDir())
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("want exit 2, got %v\n%s", err, out)
	}
	if strings.Contains("\n"+string(out), "\nclass: ") || !strings.Contains(string(out), "git diff failed") {
		t.Errorf("want the refusal and no class, got: %s", out)
	}
}
