package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/doctor"
	"github.com/wstein/workharbor/internal/exitcode"
	"github.com/wstein/workharbor/internal/launchd"
	"github.com/wstein/workharbor/internal/render"
	"github.com/wstein/workharbor/internal/setup"
	"github.com/wstein/workharbor/internal/setup/protocol"
)

// setupHost is a Host that runs nothing: every command is recorded.
type setupHost struct {
	outputs map[string]string
	errs    map[string]error // a command's own failure, instead of the generic exit status 1
	read    []string
	ran     []string
	opened  []string
	asked   int
}

func (h *setupHost) Output(_ context.Context, argv ...string) ([]byte, error) {
	h.read = append(h.read, strings.Join(argv, " "))
	if out, ok := h.outputs[strings.Join(argv, " ")]; ok {
		return []byte(out), nil
	}
	if err, ok := h.errs[strings.Join(argv, " ")]; ok {
		return nil, err
	}
	return nil, errors.New("exit status 1")
}

// dsclSays makes the fake dscl answer a read of the account's UniqueID with err,
// so no test depends on the host's real directory service (Linux has no dscl).
func (r *setupRig) dsclSays(account string, err error) {
	if r.host.errs == nil {
		r.host.errs = map[string]error{}
	}
	r.host.errs["dscl . -read /Users/"+account+" UniqueID"] = err
}

func (h *setupHost) Run(_ context.Context, c doctor.Cmd) error {
	h.ran = append(h.ran, strings.Join(c.Full(), " "))
	return nil
}

func (h *setupHost) Open(_ context.Context, t string) error {
	h.opened = append(h.opened, t)
	return nil
}

func (h *setupHost) Line(string) (string, error)   { h.asked++; return "", nil }
func (h *setupHost) Secret(string) (string, error) { h.asked++; return "", nil }
func (h *setupHost) Confirm(string) (bool, error)  { h.asked++; return false, nil }
func (h *setupHost) Show(string)                   {}

type aquaOnly struct{ name string }

func (a aquaOnly) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if args[0] == "managername" {
		return []byte(a.name), nil
	}
	return nil, errors.New("exit status 113")
}

type setupRig struct {
	t    *testing.T
	host *setupHost
	env  SetupEnv
	exe  string
	home string // where the setup protocol of the rig is written
}

func newSetupRig(t *testing.T) *setupRig {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "opt", "whr", "bin", "whr")
	if err := os.MkdirAll(filepath.Dir(exe), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable test file
		t.Fatal(err)
	}
	r := &setupRig{t: t, host: &setupHost{outputs: map[string]string{}}, exe: exe}
	r.home = filepath.Join(dir, "home")
	r.env = SetupEnv{
		NoRunLog: true, RuntimeSockets: []string{},
		OpenLog: func(string) (*protocol.Log, error) { return protocol.Open(r.home, nil, nil) },
		Host:    r.host, User: "werner", UID: 501, GOOS: "darwin", IsTerminal: func() bool { return true },
		// the same answer on every machine: a golden must not depend on what is installed
		LookPath:   func(string) (string, error) { return "/opt/homebrew/bin/container", nil },
		Executable: func() (string, error) { return exe, nil },
		Manager:    &launchd.Manager{R: aquaOnly{"Aqua"}, UID: 501, GOOS: "darwin"},
	}
	return r
}

// runBare runs a command line exactly as given.
func (r *setupRig) runBare(args ...string) string {
	r.t.Helper()
	var out, errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }, Setup: r.env}
	Execute(context.Background(), env, args)
	return out.String() + errOut.String()
}

func (r *setupRig) run(args ...string) (int, string, string) {
	r.t.Helper()
	var out, errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(k string) string {
		if k == "HOME" {
			return "/Users/workharbor"
		}
		return ""
	}, Setup: r.env}
	code := Execute(context.Background(), env, append(args, "--config", "/Users/workharbor/.config/whr/config.json", "--prefix", filepath.Dir(filepath.Dir(r.exe))))
	return code, out.String(), errOut.String()
}

func TestSetupNeedsATerminalUnlessItIsADryRun(t *testing.T) {
	r := newSetupRig(t)
	r.env.IsTerminal = func() bool { return false }
	code, _, errOut := r.run("setup", "host")
	if code != exitcode.Usage || !strings.Contains(errOut, "needs a terminal") || !strings.Contains(errOut, "--dry-run") {
		t.Errorf("no terminal: exit %d, stderr %q", code, errOut)
	}
	if len(r.host.ran) != 0 || r.host.asked != 0 {
		t.Error("something ran without a terminal")
	}
	if code, _, _ := r.run("setup", "host", "--dry-run"); code == exitcode.Usage {
		t.Errorf("a dry run without a terminal was refused")
	}
}

func TestTheHostPartRefusesRootAndTheWhrUser(t *testing.T) {
	r := newSetupRig(t)
	r.env.User, r.env.UID = "root", 0
	if code, _, errOut := r.run("setup", "host"); code != exitcode.Usage || !strings.Contains(errOut, "never runs as root") {
		t.Errorf("root: exit %d, stderr %q", code, errOut)
	}
	r.env.User, r.env.UID = "workharbor", 502
	if code, _, errOut := r.run("setup", "host"); code != exitcode.Usage || !strings.Contains(errOut, "workharbor's own standard account") {
		t.Errorf("the whr user: exit %d, stderr %q", code, errOut)
	}
	if len(r.host.ran) != 0 {
		t.Errorf("a refused run ran %v", r.host.ran)
	}
	// an administrator whr account may run the host part (D49)
	r.host.outputs["dseditgroup -o checkmember -m workharbor admin"] = "yes workharbor is a member of admin"
	if code, _, errOut := r.run("setup", "host", "--dry-run"); code == exitcode.Usage && strings.Contains(errOut, "own standard account") {
		t.Errorf("an administrator whr was refused: %q", errOut)
	}
}

func TestTheUserPartNeedsWhrInItsDesktopSession(t *testing.T) {
	r := newSetupRig(t)
	if code, _, errOut := r.run("setup", "--only", "api-token"); code != exitcode.Usage || !strings.Contains(errOut, "it is for workharbor, and this is werner") {
		t.Errorf("another user: exit %d, stderr %q", code, errOut)
	}
	r.env.User, r.env.UID = "workharbor", 502
	r.env.Manager = &launchd.Manager{R: aquaOnly{"Background"}, UID: 502, GOOS: "darwin"}
	if code, _, errOut := r.run("setup"); code != exitcode.Usage || !strings.Contains(errOut, "desktop session") {
		t.Errorf("over SSH: exit %d, stderr %q", code, errOut)
	}
	r.env.User, r.env.UID = "root", 0
	if code, _, errOut := r.run("setup"); code != exitcode.Usage || !strings.Contains(errOut, "never runs as root") {
		t.Errorf("root: exit %d, stderr %q", code, errOut)
	}
}

// A whr may lie anywhere (alpha, #493); only a file that is not executable is refused.
func TestTheWizardRunsFromAnyExecutableBinary(t *testing.T) {
	r := newSetupRig(t)
	other := filepath.Join(t.TempDir(), "whr")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable test file
		t.Fatal(err)
	}
	r.env.Executable = func() (string, error) { return other, nil }
	for _, args := range [][]string{{"setup", "host", "--dry-run"}, {"setup", "host", "--only", "power"}} {
		if code, _, errOut := r.run(args...); code == exitcode.Usage && strings.Contains(errOut, other) || strings.Contains(errOut, "not an installed binary") || strings.Contains(errOut, "note (dry run): "+other) {
			t.Errorf("%v: a binary outside the prefix was refused: exit %d, %q", args, code, errOut)
		}
	}
	plain := filepath.Join(t.TempDir(), "whr")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.env.Executable = func() (string, error) { return plain, nil }
	if code, _, errOut := r.run("setup", "host"); code != exitcode.Usage || !strings.Contains(errOut, "not an executable file") {
		t.Errorf("a file that is not executable: exit %d, stderr %q", code, errOut)
	}
}

func TestADryRunPrintsEveryFixAndChangesNothing(t *testing.T) {
	r := newSetupRig(t)
	r.host.outputs["pmset -g"] = " sleep 10\n autorestart 0\n"
	r.host.outputs["fdesetup status"] = "FileVault is Off."
	r.host.outputs["/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate"] = "Firewall is disabled. (State = 0)"
	r.host.outputs["/usr/libexec/ApplicationFirewall/socketfilterfw --getstealthmode"] = "Firewall stealth mode is off"
	code, out, errOut := r.run("setup", "host", "--dry-run")
	if len(r.host.ran) != 0 || len(r.host.opened) != 0 || r.host.asked != 0 {
		t.Fatalf("a dry run ran %v opened %v asked %d", r.host.ran, r.host.opened, r.host.asked)
	}
	for _, want := range []string{
		"    sudo pmset -a sleep 0 disksleep 0 autorestart 1 womp 1 powernap 0",
		"    sudo /usr/libexec/ApplicationFirewall/socketfilterfw --setglobalstate on",
		"    sudo sysadminctl -addUser workharbor -fullName WorkHarbor -password -",
		"    sudo install -m 0644 -o root -g wheel",
		"sudo fdesetup enable",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the dry run does not show %q\n%s", want, errOut)
		}
	}
	for _, want := range []string{"fail\tpower\t", "fail\tfirewall\t", "fail\tfilevault\t"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if code != exitcode.Error {
		t.Errorf("exit %d, want 1 while steps are not done", code)
	}
	// optional steps (Screen Sharing, Tailscale) wait to be named
	if strings.Contains(out, "screen-sharing") || strings.Contains(out, "tailscale") {
		t.Error("optional steps ran unasked")
	}
	if code, out, _ := r.run("setup", "host", "--dry-run", "--only", "tailscale"); !strings.Contains(out, "not_verified\ttailscale") {
		t.Errorf("--only tailscale: exit %d, stdout %q", code, out)
	}
}

func TestAnUnknownStepIsAUsageErrorThatListsTheKnownOnes(t *testing.T) {
	r := newSetupRig(t)
	code, _, errOut := r.run("setup", "host", "--dry-run", "--only", "nope")
	if code != exitcode.Usage || !strings.Contains(errOut, `no step "nope"`) || !strings.Contains(errOut, "power") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := r.run("setup", "host", "--dry-run", "--from", "config-base"); code != exitcode.Usage {
		t.Errorf("a step of the other part: exit %d, stderr %q", code, errOut)
	}
}

func TestTheStepsCompleteInTheShell(t *testing.T) {
	r := newSetupRig(t)
	out := r.runBare("__complete", "setup", "--only", "")
	for _, want := range []string{"power", "firewall", "filevault", "brew-packages"} {
		if !strings.Contains(out, want) {
			t.Errorf("--only does not complete %q:\n%s", want, out)
		}
	}
	out = r.runBare("__complete", "setup", "--from", "")
	if !strings.Contains(out, "config-base") || !strings.Contains(out, "firewall") {
		t.Errorf("--from of setup (both parts):\n%s", out)
	}
}

func TestDoctorRunsEveryCheckReadOnlyAndNamesTheFix(t *testing.T) {
	r := newSetupRig(t)
	code, out, errOut := r.run("doctor")
	if code != exitcode.Error {
		t.Errorf("exit %d, want %d (the configuration is missing)", code, exitcode.Error)
	}
	lines := map[string][]string{}
	var order []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 4 {
			t.Fatalf("want four tab-separated columns: %q", l)
		}
		lines[f[1]] = f
		order = append(order, f[1])
	}
	for _, name := range []string{"config", "power", "firewall", "container-start", "config-base", "service-install", "egress"} {
		if lines[name] == nil {
			t.Errorf("no line for %s: %q", name, out)
		}
	}
	idx := func(n string) int {
		for i, o := range order {
			if o == n {
				return i
			}
		}
		return -1
	}
	if idx("power") >= idx("config-dir") || idx("config-dir") >= idx("service-install") {
		t.Errorf("host steps, then user steps, in the wizard's order: %v", order)
	}
	pfx := " --prefix " + shellArgument(filepath.Dir(filepath.Dir(r.exe)))
	if got := lines["power"][3]; got != "whr setup --only power"+pfx {
		t.Errorf("power fix %q", got)
	}
	// run as werner, not whr: the user phase says so, and says to run as workharbor
	if f := lines["config-base"]; f[0] != "not_verified" || !strings.Contains(f[2], "check it as workharbor") || f[3] != "whr setup --only config-first"+pfx {
		t.Errorf("config-base: %q", f)
	}
	if f := lines["config"]; f[0] != "fail" || !strings.Contains(f[3], "whr setup") {
		t.Errorf("config: %q", f)
	}
	if f := lines["egress"]; f[3] != "" {
		t.Errorf("a check no step fixes names no command: %q", f)
	}
	if !strings.Contains(errOut, "    whr setup --only power") {
		t.Errorf("stderr lacks the fix: %q", errOut)
	}
	// read-only: nothing ran, nothing opened, nobody asked, no sudo among the reads
	if len(r.host.ran) != 0 || len(r.host.opened) != 0 || r.host.asked != 0 {
		t.Errorf("doctor ran %v, opened %v, asked %d", r.host.ran, r.host.opened, r.host.asked)
	}
	for _, c := range r.host.read {
		if strings.HasPrefix(c, "sudo") {
			t.Errorf("doctor read through sudo: %q", c)
		}
	}
	// --skip works on a step's name too
	if _, out, _ := r.run("doctor", "--skip", "power"); !strings.Contains(out, "skipped\tpower\t") {
		t.Errorf("--skip power: %q", out)
	}
	// as workharbor, the user phase is checked for real
	r.env.User, r.env.UID = "workharbor", 502
	if _, out, _ := r.run("doctor"); strings.Contains(out, "this check describes the account") {
		t.Errorf("as workharbor nothing is deferred: %q", out)
	}
}

func TestSetupAsksOnceMoreWhenAnAdministratorIsReachableFromAfar(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"public_url":"https://whr.example.ts.net"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newSetupRig(t)
	r.env.User, r.env.UID = "workharbor", 502
	r.host.outputs["dseditgroup -o checkmember -m workharbor admin"] = "yes workharbor is a member of admin"
	var out, errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }, Setup: r.env}
	code := Execute(context.Background(), env, []string{"setup", "--config", cfg, "--prefix", filepath.Dir(filepath.Dir(r.exe))})
	// the fake host answers no, so setup stops before any step
	if code != exitcode.Usage || !strings.Contains(errOut.String(), "other devices") || !strings.Contains(errOut.String(), "stopped") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
	if len(r.host.ran) != 0 {
		t.Errorf("a declined confirmation ran %v", r.host.ran)
	}
}

func TestAccountStepCanonicalAndLegacySelection(t *testing.T) {
	for _, name := range []string{"workharbor-user", "whr-user", "whr-user,workharbor-user"} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t)
			r.dsclSays("operator", errors.New("exit status 56"))
			r.dsclSays("whr", errors.New("exit status 56"))
			r.dsclSays("whr", errors.New("exit status 56"))
			code, out, errOut := r.run("setup", "host", "--dry-run", "--only", name, "--user", "operator")
			text := out + errOut
			if code != 1 || !strings.Contains(text, "workharbor-user") || !strings.Contains(text, "sysadminctl -addUser operator") {
				t.Fatalf("code %d: %s", code, text)
			}
			// once in the step, and once more in the closing "What you need to do now"
			body, todo, found := strings.Cut(text, "What you need to do now")
			if !found || strings.Count(body, "    sudo sysadminctl -addUser operator") != 1 || strings.Count(todo, "    sudo sysadminctl -addUser operator") != 1 {
				t.Fatalf("account fix repeated or missing: %s", text)
			}
			if len(r.host.ran) != 0 {
				t.Fatalf("dry run executed %v", r.host.ran)
			}
		})
	}
}

func TestDoctorAccountStepCanonicalAndLegacySkip(t *testing.T) {
	for _, name := range []string{"workharbor-user", "whr-user", "whr-user,workharbor-user"} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t)
			_, out, errOut := r.run("doctor", "--skip", name)
			if !strings.Contains(out, "skipped\tworkharbor-user\tskipped on request") {
				t.Fatalf("out %q stderr %q", out, errOut)
			}
			for _, read := range r.host.read {
				if strings.HasPrefix(read, "dscl . -read /Users/workharbor UniqueID") {
					t.Fatalf("skipped account read: %s", read)
				}
			}
			if !strings.Contains(out, "\tautologout\t") || strings.Contains(out, "skipped\tautologout\t") {
				t.Fatalf("unrelated check skipped: %s", out)
			}
			if len(r.host.ran) != 0 {
				t.Fatal("doctor executed a fix")
			}
		})
	}
}

func TestAccountStepUnknownSelectionRejected(t *testing.T) {
	for _, args := range [][]string{{"setup", "host", "--dry-run", "--only", "workharbor-user-unknown"}, {"doctor", "--skip", "workharbor-user-unknown"}} {
		r := newSetupRig(t)
		code, _, text := r.run(args...)
		if code != exitcode.Usage || !strings.Contains(text, "workharbor-user-unknown") {
			t.Fatalf("code %d: %s", code, text)
		}
		if len(r.host.ran) != 0 {
			t.Fatalf("unknown selection ran commands: %v %v", r.host.read, r.host.ran)
		}
	}
}

func TestAccountStepCompletionIsCanonical(t *testing.T) {
	r := newSetupRig(t)
	out := r.runBare("__complete", "setup", "--only", "")
	if !strings.Contains(out, "workharbor-user\t") || strings.Contains(out, "\nwhr-user\t") {
		t.Fatalf("completion %q", out)
	}
}

func TestAccountStepLegacyFrom(t *testing.T) {
	r := newSetupRig(t)
	r.dsclSays("operator", errors.New("exit status 56"))
	r.dsclSays("whr", errors.New("exit status 56"))
	code, out, errOut := r.run("setup", "host", "--dry-run", "--from", "whr-user", "--only", "workharbor-user", "--user", "operator")
	if code != 1 || !strings.Contains(out+errOut, "    sudo sysadminctl -addUser operator") {
		t.Fatalf("code %d: %s%s", code, out, errOut)
	}
	if len(r.host.ran) != 0 {
		t.Fatal("dry run executed commands")
	}
}

func TestAccountStepDsclErrorThatIsNotNotFoundIsNotVerified(t *testing.T) {
	r := newSetupRig(t)
	r.dsclSays("operator", errors.New("dscl: command not found"))
	_, out, errOut := r.run("setup", "host", "--dry-run", "--only", "workharbor-user", "--user", "operator")
	text := out + errOut
	if !strings.Contains(text, "not_verified\tworkharbor-user\tdscl did not say whether operator exists: dscl: command not found") || strings.Contains(text, "there is no user") {
		t.Fatalf("a dscl failure that is not a not-found must be not_verified: %s", text)
	}
}

func TestDoctorRepairsKeepSelectedAccount(t *testing.T) {
	for _, account := range []string{"workharbor", "whr", "operator", "operator's"} {
		r := newSetupRig(t)
		_, out, _ := r.run("doctor", "--user", account)
		// the doctor keeps the prefix it was given (issue #399)
		suffix := " --prefix " + shellArgument(filepath.Dir(filepath.Dir(r.exe)))
		if account != "workharbor" {
			suffix += " --user " + shellArgument(account)
		}
		want := "whr setup --only workharbor-user" + suffix
		if !strings.Contains(out, want) {
			t.Fatalf("repair lacks selected account %q: %s", account, out)
		}
		want = "whr setup --only config-dir" + suffix
		if !strings.Contains(out, want) {
			t.Fatalf("user repair lacks selected account %q: %s", account, out)
		}
	}
}

func TestSetupHostNextPointsAtLegacyUserWhr(t *testing.T) {
	r := newSetupRig(t)
	r.dsclSays("workharbor", errors.New("exit status 56"))
	r.host.outputs["dscl . -read /Users/whr UniqueID"] = "UniqueID: 502"
	_, out, errOut := r.run("setup", "host", "--dry-run", "--only", "workharbor-user")
	text := out + errOut
	if strings.Contains(text, "-addUser") || strings.Contains(text, "--only workharbor-user") || !strings.Contains(text, "next: whr setup ") || !strings.Contains(text, " --user whr\n") {
		t.Errorf("legacy next line wrong:\n%s", text)
	}
	if strings.Contains(text, "--user whr --user") {
		t.Errorf("double --user:\n%s", text)
	}
}

// The --json document of `whr doctor` is a contract: the human rendering (#320)
// must not change one byte of it. The golden file was recorded before the
// rendering existed; the temporary directory of the rig is replaced by a name.
func TestDoctorJSONIsByteIdentical(t *testing.T) {
	r := newSetupRig(t)
	r.env.IsTerminal = func() bool { return false }
	_, out, _ := r.run("doctor", "--json", "--user", "operator")
	out = strings.ReplaceAll(out, filepath.Dir(filepath.Dir(filepath.Dir(r.exe))), "<tmp>")
	p := filepath.Join("testdata", "doctor_json.golden")
	if os.Getenv("WHR_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p) //nolint:gosec // a golden file of this package
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != out {
		t.Errorf("--json output changed:\n%s", out)
	}
}

func TestResolveWiresTheInterruptSignalHolder(t *testing.T) {
	st := &state{env: &Env{Stdin: strings.NewReader(""), Stderr: &bytes.Buffer{}}}
	e, err := SetupEnv{User: "werner", UID: 501, GOOS: "darwin"}.resolve(st, render.Style{})
	if err != nil {
		t.Fatal(err)
	}
	term, ok := e.Host.(setup.Terminal)
	if !ok || term.Sig == nil {
		t.Errorf("Host %#v: want a setup.Terminal with Sig set", e.Host)
	}
}

func TestTailscaleIsInstalledWithBrewOnlyAfterTheConfirmation(t *testing.T) {
	missing := func(name string) (string, error) {
		if name == "/opt/homebrew/bin/brew" {
			return name, nil
		}
		return "", errors.New("not found")
	}
	const install = "/opt/homebrew/bin/brew install --cask tailscale-app"

	r := newSetupRig(t)
	r.env.LookPath = missing
	code, out, errOut := r.run("setup", "host", "--dry-run", "--only", "tailscale")
	if len(r.host.ran) != 0 || r.host.asked != 0 || !strings.Contains(out, "not_verified\ttailscale\t") || !strings.Contains(errOut, "    "+install+"\n") {
		t.Errorf("dry run: exit %d, ran %v, asked %d\nout %q\nerr %q", code, r.host.ran, r.host.asked, out, errOut)
	}
	if strings.Contains(errOut, "    sudo") || !strings.Contains(errOut, "UNVERIFIED") || !strings.Contains(errOut, "    open -a Tailscale\n") {
		t.Errorf("dry run text:\n%s", errOut)
	}

	r = newSetupRig(t) // --yes skips the prompt
	r.env.LookPath = missing
	r.run("setup", "host", "--yes", "--only", "tailscale")
	if len(r.host.ran) != 1 || r.host.ran[0] != install {
		t.Errorf("--yes ran %v", r.host.ran)
	}

	r = newSetupRig(t) // the prompt says no: nothing runs
	r.env.LookPath = missing
	r.run("setup", "host", "--only", "tailscale")
	if len(r.host.ran) != 0 {
		t.Errorf("a declined install ran %v", r.host.ran)
	}
}

// Issue #507: an account that cannot sudo gets one warning, the read-only
// checks run, and each step that needs sudo is listed, not run; the exit code
// is non-zero. root and the standard whr account stay refused.
func TestTheHostPartWarnsWhenTheAccountCannotSudo(t *testing.T) {
	const member = "dseditgroup -o checkmember -m werner admin"
	setup := func(answer string) *setupRig {
		r := newSetupRig(t)
		r.host.outputs[member] = answer
		r.host.outputs["pmset -g"] = " sleep 10\n autorestart 0\n"
		return r
	}
	r := setup("no werner is NOT a member of admin")
	code, _, errOut := r.run("setup", "host", "--only", "power")
	if code == exitcode.OK || code == exitcode.Usage {
		t.Errorf("non-admin: exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"cannot sudo", "sudo pmset", "an administrator", "need an administrator (sudo)", "this run is not complete"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("non-admin: stderr lacks %q: %q", want, errOut)
		}
	}
	if len(r.host.ran) != 0 || r.host.asked != 0 {
		t.Errorf("non-admin ran %v and asked %d", r.host.ran, r.host.asked)
	}
	r = setup("yes werner is a member of admin")
	if _, _, errOut := r.run("setup", "host", "--only", "power"); strings.Contains(errOut, "cannot sudo") {
		t.Errorf("an administrator was warned: %q", errOut)
	}
	r = setup("unreadable")
	if _, _, errOut := r.run("setup", "host", "--only", "power"); strings.Contains(errOut, "cannot sudo") {
		t.Errorf("an unreadable answer warned: %q", errOut)
	}
	r = setup("no werner is NOT a member of admin")
	r.env.User, r.env.UID = "root", 0
	if code, _, errOut := r.run("setup", "host", "--only", "power"); code != exitcode.Usage || strings.Contains(errOut, "cannot sudo") {
		t.Errorf("root: exit %d, stderr %q", code, errOut)
	}
	r = setup("")
	r.env.User, r.env.UID = "workharbor", 502
	r.host.outputs["dseditgroup -o checkmember -m workharbor admin"] = "no workharbor is NOT a member of admin"
	if code, _, errOut := r.run("setup", "host", "--only", "power"); code != exitcode.Usage || !strings.Contains(errOut, "own standard account") {
		t.Errorf("the whr user: exit %d, stderr %q", code, errOut)
	}
}
