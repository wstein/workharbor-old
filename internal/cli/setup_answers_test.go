package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/doctor"
	"github.com/wstein/workharbor/internal/exitcode"
	"github.com/wstein/workharbor/internal/render"
	"github.com/wstein/workharbor/internal/setup"
	"github.com/wstein/workharbor/internal/setup/answers"
	"github.com/wstein/workharbor/internal/setup/protocol"
)

// yesHost answers every Confirm, so a run goes through its fixes.
type yesHost struct {
	*setupHost
	yes bool
}

func (h yesHost) Confirm(string) (bool, error) { h.asked++; return h.yes, nil }

const testID = "v1.0.0@abc1234"

// answersRig is a setup rig whose build has an identity and whose host answers
// y; service-install records a fake command with no sudo in the user part.
// The setupHost never executes it or contacts launchd.
func answersRig(t *testing.T, yes bool) *setupRig {
	t.Helper()
	r := newSetupRig(t)
	r.env.Host = yesHost{setupHost: r.host, yes: yes}
	r.env.Identity = func() (string, error) { return testID, nil }
	return r
}

func (r *setupRig) setup(args ...string) (int, string) {
	r.t.Helper()
	code, _, errOut := r.run(append([]string{"setup", "--user", "werner", "--only", "service-install"}, args...)...)
	return code, errOut
}

func answersFile(t *testing.T) string { return filepath.Join(t.TempDir(), "answers.json") }

func readLog(t *testing.T, r *setupRig) []byte {
	t.Helper()
	b, err := os.ReadFile(protocol.Path(r.home))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSaveAnswersThenAnswersRunsWithoutAskingAndLogsIt(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	code, errOut := r.setup("--save-answers", f)
	if code != exitcode.Error && code != exitcode.OK { // the check stays not_verified on the fake host
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "saved 1 answers") || len(r.host.ran) != 1 || r.host.asked != 1 {
		t.Fatalf("exit %d ran %v asked %d\n%s", code, r.host.ran, r.host.asked, errOut)
	}
	fi, err := os.Stat(f)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", fi, err)
	}
	data, _ := os.ReadFile(f) //nolint:gosec // a test path
	if !strings.Contains(string(data), `"step": "service-install"`) || !strings.Contains(string(data), testID) {
		t.Fatalf("%s", data)
	}
	first := readLog(t, r)

	// second run: answered from the file, nothing is asked
	r.host.ran, r.host.asked = nil, 0
	code, errOut = r.setup("--answers", f)
	if r.host.asked != 0 || len(r.host.ran) != 1 {
		t.Fatalf("exit %d: asked %d ran %v\n%s", code, r.host.asked, r.host.ran, errOut)
	}
	both := readLog(t, r)
	if !bytes.HasPrefix(both, first) || len(both) <= len(first) {
		t.Fatal("the protocol is not append-only across runs")
	}
	entries, err := protocol.Chain(both)
	if err != nil {
		t.Fatal(err)
	}
	digest := protocol.AnswersDigest(data)
	var seen int
	for _, e := range entries {
		if e.Source == protocol.SourceAnswers {
			seen++
			if e.Answers != digest || e.Phase != "user" || e.Account != "werner" {
				t.Errorf("entry %+v", e)
			}
		}
	}
	if seen != 2 { // run.start and step.before
		t.Errorf("%d entries from the file", seen)
	}
	if strings.Contains(string(both), "/Users/workharbor") || strings.Contains(string(both), r.home) {
		t.Error("a home directory is in the protocol")
	}
}

func TestAChangedCommandOrBuildOrAccountAsksAgain(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"digest": func(s string) string {
			return regexp.MustCompile(`"fix": "[0-9a-f]{64}"`).ReplaceAllString(s, `"fix": "`+strings.Repeat("0", 64)+`"`)
		},
		"build":   func(s string) string { return strings.Replace(s, testID, "v2.0.0@def5678", 1) },
		"account": func(s string) string { return strings.Replace(s, `"account": "werner"`, `"account": "other"`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			r := answersRig(t, true)
			f := answersFile(t)
			r.setup("--save-answers", f)
			data, _ := os.ReadFile(f) //nolint:gosec // a test path
			if err := os.WriteFile(f, []byte(edit(string(data))), 0o600); err != nil {
				t.Fatal(err)
			}
			r.host.ran, r.host.asked = nil, 0
			_, errOut := r.setup("--answers", f)
			if r.host.asked != 1 {
				t.Fatalf("asked %d, want 1\n%s", r.host.asked, errOut)
			}
			if name != "digest" && !strings.Contains(errOut, "the answers file is not used") {
				t.Errorf("no note:\n%s", errOut)
			}
		})
	}
}

func TestADirtyBuildNeitherUsesNorSavesAnswers(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	r.setup("--save-answers", f)
	r.env.Identity = func() (string, error) { return "", answers.ErrNoBuildIdentity }
	r.host.ran, r.host.asked = nil, 0
	_, errOut := r.setup("--answers", f)
	if r.host.asked != 1 || !strings.Contains(errOut, "every step is asked") {
		t.Fatalf("asked %d\n%s", r.host.asked, errOut)
	}
	g := answersFile(t)
	code, errOut := r.setup("--save-answers", g)
	if code != exitcode.Error || !strings.Contains(errOut, "answers not saved") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if _, err := os.Stat(g); err == nil {
		t.Fatal("a file was saved by a build without identity")
	}
}

func TestTheAnswersFileModeIsJudged(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	r.setup("--save-answers", f)
	if err := os.Chmod(f, 0o644); err != nil { //nolint:gosec // the test makes the file too open on purpose
		t.Fatal(err)
	}
	r.host.ran, r.host.asked = nil, 0
	_, errOut := r.setup("--answers", f)
	if !strings.Contains(errOut, "readable by others") || r.host.asked != 0 {
		t.Fatalf("a readable file must warn and still be used (asked %d)\n%s", r.host.asked, errOut)
	}
	if err := os.Chmod(f, 0o666); err != nil { //nolint:gosec // the test makes the file too open on purpose
		t.Fatal(err)
	}
	r.host.ran = nil
	code, errOut := r.setup("--answers", f)
	if code != exitcode.Usage || len(r.host.ran) != 0 || !strings.Contains(errOut, "writable by group or others") {
		t.Fatalf("a writable file is refused: exit %d ran %v\n%s", code, r.host.ran, errOut)
	}
}

func TestTheHostPartTakesNoAnswersAndIsNeverUnattended(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	for _, flag := range [][]string{{"--answers", f}, {"--save-answers", f}, {"--unattended"}} {
		code, _, errOut := r.run(append([]string{"setup", "host", "--dry-run"}, flag...)...)
		if code != exitcode.Usage || !strings.Contains(errOut, "never answered from a file and never unattended") {
			t.Errorf("%v: exit %d\n%s", flag, code, errOut)
		}
	}
	if len(r.host.ran) != 0 {
		t.Error("something ran")
	}
	if _, err := os.Stat(protocol.Path(r.home)); err == nil {
		t.Error("a refused run wrote a protocol")
	}
}

func TestUnattendedNeedsAnAnswersFileAndLeavesWhatItDoesNotDecideWithExit6(t *testing.T) {
	r := answersRig(t, true)
	if code, errOut := r.setup("--unattended"); code != exitcode.Usage || !strings.Contains(errOut, "--unattended needs --answers") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
	// a file that decides nothing here (saved with no step in it is not possible,
	// so one for a changed command): the step is left, nothing is asked or run
	f := answersFile(t)
	r.setup("--save-answers", f)
	data, _ := os.ReadFile(f) //nolint:gosec // a test path
	bad := regexp.MustCompile(`"fix": "[0-9a-f]{64}"`).ReplaceAllString(string(data), `"fix": "`+strings.Repeat("1", 64)+`"`)
	if err := os.WriteFile(f, []byte(bad), 0o600); err != nil { //nolint:gosec // a test path
		t.Fatal(err)
	}
	r.env.IsTerminal = func() bool { return false } // an unattended run needs no terminal
	r.host.ran, r.host.asked = nil, 0
	code, errOut := r.setup("--answers", f, "--unattended")
	if code != exitcode.NeedsHuman || r.host.asked != 0 || len(r.host.ran) != 0 || !strings.Contains(errOut, "service-install need a person") || !strings.Contains(errOut, "--answers "+f+" --unattended") {
		t.Fatalf("exit %d asked %d ran %v\n%s", code, r.host.asked, r.host.ran, errOut)
	}
	// the real file decides it: it runs, and the exit is never 6
	if err := os.WriteFile(f, data, 0o600); err != nil { //nolint:gosec // a test path
		t.Fatal(err)
	}
	r.host.ran, r.host.asked = nil, 0
	code, errOut = r.setup("--answers", f, "--unattended")
	if code == exitcode.NeedsHuman || r.host.asked != 0 || len(r.host.ran) != 1 {
		t.Fatalf("exit %d asked %d ran %v\n%s", code, r.host.asked, r.host.ran, errOut)
	}
}

func TestADryRunWithAnswersShowsWhatWouldRunAndWhichQuestionsStayOpen(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	r.setup("--save-answers", f)
	r.host.ran, r.host.asked = nil, 0
	_, errOut := r.setup("--answers", f, "--dry-run")
	if !strings.Contains(strings.Join(strings.Fields(errOut), " "), "the answers file says run: it would run without asking") || len(r.host.ran) != 0 || r.host.asked != 0 {
		t.Fatalf("ran %v asked %d\n%s", r.host.ran, r.host.asked, errOut)
	}
	lines := bytes.Count(readLog(t, r), []byte("\n"))
	r.setup("--answers", f, "--dry-run")
	if bytes.Count(readLog(t, r), []byte("\n")) != lines {
		t.Error("a dry run wrote to the protocol")
	}
	if code, errOut := r.setup("--save-answers", answersFile(t), "--dry-run"); code != exitcode.Usage || !strings.Contains(errOut, "a dry run asks nothing") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
}

func TestAnAnswersFileTheHumanDidNotNameIsNeverRead(t *testing.T) {
	r := answersRig(t, false)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "whr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "whr", "setup-answers.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, errOut := r.setup() // the default is to ask
	if code == exitcode.Usage || strings.Contains(errOut, "answers file") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if r.host.asked != 1 {
		t.Errorf("asked %d", r.host.asked)
	}
}

func TestTheProtocolFailingStopsBeforeAnythingRuns(t *testing.T) {
	r := answersRig(t, true)
	if err := os.MkdirAll(r.home, 0o755); err != nil { //nolint:gosec // a test directory the state dir check refuses
		t.Fatal(err)
	}
	r.env.OpenLog = func(string) (*protocol.Log, error) { return nil, protocol.ErrConflict }
	code, errOut := r.setup()
	if code == exitcode.OK || len(r.host.ran) != 0 || !strings.Contains(errOut, "setup protocol") {
		t.Fatalf("exit %d ran %v\n%s", code, r.host.ran, errOut)
	}
}

func TestNothingLikeACredentialIsStored(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	r.setup("--save-answers", f)
	data, _ := os.ReadFile(f) //nolint:gosec // a test path
	log := readLog(t, r)
	for _, bad := range []string{"password", "token", "secret", "-password"} {
		if strings.Contains(strings.ToLower(string(data)), bad) || strings.Contains(strings.ToLower(string(log)), bad) {
			t.Errorf("%q in a saved file", bad)
		}
	}
}

func TestUnattendedAccountRiskNeverAsksOrRuns(t *testing.T) {
	r := answersRig(t, true)
	r.env.IsTerminal = func() bool { return false }
	r.host.outputs["dseditgroup -o checkmember -m werner admin"] = "yes werner is a member of admin"
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"account":"shared","public_url":"https://whr.example.ts.net"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return r.home }, Setup: r.env}
	code := Execute(context.Background(), env, []string{"setup", "--user", "werner", "--only", "service-install", "--answers", answersFile(t), "--unattended", "--config", configPath, "--prefix", filepath.Dir(filepath.Dir(r.exe))})
	if code != exitcode.NeedsHuman || r.host.asked != 0 || len(r.host.ran) != 0 || !strings.Contains(errOut.String(), "account risk needs your confirmation") {
		t.Fatalf("exit %d asked %d ran %v: %s", code, r.host.asked, r.host.ran, errOut.String())
	}
}

func TestSaveAnswersKeepsEligibleDecisionsInAMixedRun(t *testing.T) {
	env := SetupEnv{User: "workharbor", Identity: func() (string, error) { return testID, nil }}
	checks := []doctor.Check{
		{Name: "normal", Phase: doctor.PhaseUser, Fix: &doctor.Fix{Cmds: []doctor.Cmd{{Argv: []string{"normal"}}}}},
		{Name: "sudo", Phase: doctor.PhaseUser, Fix: &doctor.Fix{Cmds: []doctor.Cmd{{Argv: []string{"sudo-step"}, Sudo: true}}}},
		{Name: "drop-admin", Phase: doctor.PhaseUser, Fix: &doctor.Fix{Irreversible: true, Cmds: []doctor.Cmd{{Argv: []string{"drop-admin"}}}}},
	}
	var outs []setup.Outcome
	for _, c := range checks {
		outs = append(outs, setup.Outcome{Step: c.Name, Decision: answers.Run, Fix: answers.FixDigest(c)})
	}
	path := answersFile(t)
	if err := saveAnswers(env, path, checks, outs, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	f, _, err := answers.Load(path, os.Getuid())
	if err != nil || len(f.Answers) != 1 || f.Answers[0].Step != "normal" {
		t.Fatalf("saved %+v: %v", f, err)
	}
}

func TestUnattendedKernelNeverInstallsAnything(t *testing.T) {
	r := answersRig(t, true)
	path := answersFile(t)
	if _, errOut := r.setup("--save-answers", path); !strings.Contains(errOut, "saved 1 answers") {
		t.Fatal(errOut)
	}
	r.host.ran, r.host.asked = nil, 0
	r.env.IsTerminal = func() bool { return false }
	code, _, errOut := r.run("setup", "--user", "werner", "--only", "container-kernel", "--answers", path, "--unattended")
	if code == exitcode.NeedsHuman || len(r.host.ran) != 0 || r.host.asked != 0 || strings.Contains(errOut, "kernel set") || !strings.Contains(errOut, "not verified") {
		t.Fatalf("exit %d ran %v asked %d: %s", code, r.host.ran, r.host.asked, errOut)
	}
}

type quitRiskHost struct{ *setupHost }

func (h quitRiskHost) Ask(string, render.Default) (render.Answer, error) {
	h.asked++
	return render.Quit, nil
}

func TestAccountRiskQuitIsRecordedBeforeEngineStarts(t *testing.T) {
	r := answersRig(t, false)
	r.env.Host = quitRiskHost{r.host}
	r.host.outputs["dseditgroup -o checkmember -m werner admin"] = "yes werner is a member of admin"
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"account":"shared","public_url":"https://whr.example.ts.net"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return r.home }, Setup: r.env}
	code := Execute(context.Background(), env, []string{"setup", "--user", "werner", "--only", "service-install", "--config", configPath, "--prefix", filepath.Dir(filepath.Dir(r.exe))})
	if code != exitcode.Quit || len(r.host.ran) != 0 || r.host.asked != 1 {
		t.Fatalf("exit %d asked %d ran %v: %s", code, r.host.asked, r.host.ran, errOut.String())
	}
	entries, err := protocol.Chain(readLog(t, r))
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries %+v: %v", entries, err)
	}
	if entries[1].Source != protocol.SourceInteractive || entries[1].Answer != protocol.AnswerQuit || entries[2].Outcome != protocol.OutQuit || entries[3].Outcome != protocol.RunQuit {
		t.Fatalf("entries %+v", entries)
	}
}

func TestAccountRiskQuitHintQuotesItsArguments(t *testing.T) {
	r := answersRig(t, false)
	r.env.Host = quitRiskHost{r.host}
	r.host.outputs["dseditgroup -o checkmember -m werner admin"] = "yes werner is a member of admin"
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"account":"shared","public_url":"https://whr.example.ts.net"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(t.TempDir(), "my prefix")
	if err := os.Symlink(filepath.Dir(filepath.Dir(r.exe)), prefix); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return r.home }, Setup: r.env}
	code := Execute(context.Background(), env, []string{"setup", "--user", "werner", "--only", "service-install", "--config", configPath, "--prefix", prefix})
	if code != exitcode.Quit {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "'"+prefix+"'") {
		t.Errorf("the restart hint does not quote the argument with a space:\n%s", errOut.String())
	}
}

func TestSaveAnswersPathIsCheckedBeforeTheRun(t *testing.T) {
	r := answersRig(t, true)
	missing := filepath.Join(t.TempDir(), "nope", "a.json")
	code, errOut := r.setup("--save-answers", missing)
	if code != exitcode.Usage || r.host.asked != 0 || len(r.host.ran) != 0 || !strings.Contains(errOut, "--save-answers") {
		t.Fatalf("exit %d asked %d ran %v\n%s", code, r.host.asked, r.host.ran, errOut)
	}
}

func TestSaveAnswersRefusesToOverwriteBeforeTheRun(t *testing.T) {
	r := answersRig(t, true)
	f := answersFile(t)
	if err := os.WriteFile(f, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, errOut := r.setup("--save-answers", f)
	if code != exitcode.Usage || r.host.asked != 0 || len(r.host.ran) != 0 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("exit %d asked %d ran %v\n%s", code, r.host.asked, r.host.ran, errOut)
	}
	if b, _ := os.ReadFile(f); string(b) != "keep me" { //nolint:gosec // a test path
		t.Errorf("the file changed: %q", b)
	}
}
