package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/config"
)

// scripted is a Runner that answers from a table: no test touches the machine.
type scripted map[string]string

func (s scripted) Output(_ context.Context, argv ...string) ([]byte, error) {
	if out, ok := s[strings.Join(argv, " ")]; ok {
		// "ERR:" scripts a failure, with what the command said on stderr
		if msg, failed := strings.CutPrefix(out, "ERR:"); failed {
			return nil, errors.New(msg)
		}
		return []byte(out), nil
	}
	return nil, errors.New("exit status 1")
}

// answers is a Prompter that gives the next prepared answer.
type answers struct {
	lines, secrets []string
	confirm        bool
	shown          []string
	prompts        []string
}

func (a *answers) Line(string) (string, error) {
	l := a.lines[0]
	a.lines = a.lines[1:]
	return l, nil
}

func (a *answers) Secret(prompt string) (string, error) {
	a.prompts = append(a.prompts, prompt)
	s := a.secrets[0]
	a.secrets = a.secrets[1:]
	return s, nil
}
func (a *answers) Confirm(string) (bool, error) { return a.confirm, nil }
func (a *answers) Show(t string)                { a.shown = append(a.shown, t) }

func steps(t *testing.T, d Deps) map[string]Check {
	t.Helper()
	m := map[string]Check{}
	for _, c := range Checks(d) {
		if _, dup := m[c.Name]; dup {
			t.Fatalf("two checks are named %q", c.Name)
		}
		m[c.Name] = c
	}
	return m
}

func hostDeps(r Runner) Deps {
	return Deps{ConfigPath: "/Users/workharbor/.config/whr/config.json", Home: "/Users/workharbor", GOOS: "darwin", Runner: r, User: "werner", UID: 501, Prefix: "/nonexistent/opt/whr"}
}

func status(c Check) (Status, string) { return c.Run(context.Background()) }

func TestHostAccountDefaultAndExplicitOverride(t *testing.T) {
	for _, tc := range []struct {
		name, account, want string
	}{
		{"default", "", "workharbor"},
		{"existing account", "whr", "whr"},
		{"custom account", "operator", "operator"},
		{"quoted account", "operator's", "operator's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := hostDeps(scripted{
				"dscl . -read /Users/" + tc.want + " UniqueID":        "UniqueID: 502",
				"dseditgroup -o checkmember -m " + tc.want + " admin": "no " + tc.want + " is NOT a member of admin",
			})
			d.Account = tc.account
			c := steps(t, d)["workharbor-user"]
			if c.Run == nil {
				t.Fatal("canonical workharbor-user step missing")
			}
			if c.Title != "the standard user "+tc.want+" (manual step 2)" {
				t.Errorf("title = %q", c.Title)
			}
			if got, detail := status(c); got != OK || detail != tc.want+" exists and is a standard user" {
				t.Errorf("success: %s %q", got, detail)
			}
			wantArgv := []string{"sysadminctl", "-addUser", tc.want, "-fullName", "WorkHarbor", "-password", "-"}
			if !c.Fix.Cmds[0].Sudo || !reflect.DeepEqual(c.Fix.Cmds[0].Argv, wantArgv) {
				t.Errorf("creation command = %+v", c.Fix.Cmds[0])
			}
			if !strings.Contains(c.Fix.Guide, "Then log in as "+tc.want+" on the Mac") {
				t.Errorf("guide = %q", c.Fix.Guide)
			}
			wantSetup := "whr setup"
			if tc.want != "workharbor" {
				wantSetup += " --user '" + strings.ReplaceAll(tc.want, "'", "'\"'\"'") + "'"
			}
			if !reflect.DeepEqual(c.Fix.Try, []string{wantSetup}) {
				t.Errorf("setup command missing from the commands to try: %q", c.Fix.Try)
			}
			noWrappedCommand(t, c.Name, c.Fix)
			d.Runner = scripted{"dscl . -read /Users/" + tc.want + " UniqueID": "ERR:exit status 56", "dscl . -read /Users/whr UniqueID": "ERR:exit status 56"}
			if got, detail := status(steps(t, d)["workharbor-user"]); got != Fail || detail != "there is no user "+tc.want {
				t.Errorf("missing: %s %q", got, detail)
			}
		})
	}
}

func TestTheHostChecksReadWhatMacOSPrints(t *testing.T) {
	good := scripted{
		"dscl . -read /Users/workharbor UniqueID":        "UniqueID: 502",
		"dseditgroup -o checkmember -m workharbor admin": "no workharbor is NOT a member of admin",
		"pmset -g": " sleep                0\n disksleep            0\n autorestart          1\n womp                 1\n powernap             0\n",
		"/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate": "Firewall is enabled. (State = 1)",
		"/usr/libexec/ApplicationFirewall/socketfilterfw --getstealthmode": "Firewall stealth mode is on",
		"fdesetup status":                                            "FileVault is On.",
		"/opt/homebrew/bin/brew --version":                           "Homebrew 5.0",
		"/opt/homebrew/bin/brew list --formula --versions container": "container 1.5.0",
		"/opt/homebrew/bin/brew list --formula --versions git":       "git 2.50",
		"/opt/homebrew/bin/brew list --formula --versions gh":        "gh 2.80",
		"/opt/homebrew/bin/brew list --pinned":                       "container\n",
	}
	st := steps(t, hostDeps(good))
	for _, name := range []string{"workharbor-user", "power", "firewall", "filevault", "homebrew", "brew-packages", "brew-pin"} {
		if got, detail := status(st[name]); got != OK {
			t.Errorf("%s = %s %q on a set-up Mac", name, got, detail)
		}
	}

	// each way to be wrong is a failure that says what is wrong
	bad := scripted{
		"dscl . -read /Users/workharbor UniqueID":        "UniqueID: 502",
		"dseditgroup -o checkmember -m workharbor admin": "yes workharbor is a member of admin",
		"pmset -g":        " sleep 10\n disksleep 10\n autorestart 0\n powernap 1\n",
		"fdesetup status": "FileVault is Off.",
		"/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate": "Firewall is disabled. (State = 0)",
		"/usr/libexec/ApplicationFirewall/socketfilterfw --getstealthmode": "Firewall stealth mode is off",
		"/opt/homebrew/bin/brew --version":                                 "Homebrew 5.0",
		"/opt/homebrew/bin/brew list --pinned":                             "git\n",
	}
	st = steps(t, hostDeps(bad))
	want := map[string]string{
		"power": "sleep is 10, want 0", "firewall": "stealth mode is off",
		"filevault": "off", "brew-packages": "not installed: git, gh", "brew-pin": "not pinned",
	}
	for name, sub := range want {
		got, detail := status(st[name])
		if got != Fail || !strings.Contains(detail, sub) {
			t.Errorf("%s = %s %q, want a failure that says %q", name, got, detail, sub)
		}
	}
	// an administrator whr is a warning, not a failure (D49)
	if got, detail := status(st["workharbor-user"]); got != Warn || !strings.Contains(detail, "administrator") {
		t.Errorf("an administrator whr = %s %q", got, detail)
	}
	if got, detail := status(steps(t, hostDeps(scripted{"dscl . -read /Users/workharbor UniqueID": "ERR:exit status 56", "dscl . -read /Users/whr UniqueID": "ERR:exit status 56"}))["workharbor-user"]); got != Fail || !strings.Contains(detail, "no user") {
		t.Errorf("a missing user = %s %q", got, detail)
	}
}

// Off a Mac nothing is run and nothing is claimed: the steps say not verified.
func TestOffAMacTheStepsAreNotVerified(t *testing.T) {
	d := hostDeps(scripted{})
	d.GOOS = "linux"
	for _, c := range Checks(d) {
		if c.Phase == "" || c.Name == "config-dir" || c.Name == "api-token" || c.Name == "agent-key" || c.Name == "ssh-ca" || c.Name == "config-base" || c.Name == "config-first" || c.Name == "config-github" || c.Name == "github-app" || c.Name == "public-url" || c.Name == "tool-store" || c.Name == "prefix" {
			continue
		}
		if got, _ := status(c); got != NotVerified && got != OK {
			t.Errorf("%s = %s off a Mac, want not_verified", c.Name, got)
		}
	}
}

func TestEveryFixIsArgvAndRootOwnedFilesGoThroughInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the private setup directory lies in the home
	d := hostDeps(scripted{})
	d.Whr = "/opt/whr/bin/whr"
	for _, c := range Checks(d) {
		if c.Fix == nil {
			continue
		}
		for _, cmd := range c.Fix.Cmds {
			if len(cmd.Argv) == 0 {
				t.Errorf("%s: an empty command", c.Name)
				continue
			}
			first := filepath.Base(cmd.Argv[0])
			if first == "sh" || first == "bash" || first == "zsh" || first == "tee" || cmd.Argv[0] == "-c" {
				t.Errorf("%s: %v goes through a shell or a redirect", c.Name, cmd.Argv)
			}
			for _, a := range cmd.Argv {
				if c.Fix.Build != nil && strings.HasPrefix(a, "<") && strings.HasSuffix(a, ">") {
					continue // a placeholder in the preview of a command that is built after the question
				}
				if strings.ContainsAny(a, "|;&><`") || strings.Contains(a, "$(") {
					t.Errorf("%s: %q looks like shell syntax in an argument", c.Name, a)
				}
			}
			if cmd.Argv[0] == "sudo" {
				t.Errorf("%s: sudo is the Sudo flag, not part of the argv", c.Name)
			}
		}
	}
	// the sshd file: written to a private temp file, then installed as root's, mode 0644
	ssh := steps(t, d)["ssh-keys-only"].Fix
	want := []string{"install", "-m", "0644", "-o", "root", "-g", "wheel", sshdTemp(), sshdFile}
	if len(ssh.Cmds) != 1 || !ssh.Cmds[0].Sudo || strings.Join(ssh.Cmds[0].Argv, " ") != strings.Join(want, " ") {
		t.Errorf("ssh fix %+v, want sudo %v", ssh.Cmds, want)
	}
	if err := ssh.Do(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sshdTemp())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("temp file %v %v, want 0600", fi, err)
	}
	if di, _ := os.Stat(filepath.Dir(sshdTemp())); di.Mode().Perm() != 0o700 {
		t.Errorf("temp dir %v, want 0700", di.Mode().Perm())
	}
	b, _ := os.ReadFile(sshdTemp())
	if string(b) != sshdContent {
		t.Errorf("content %q", b)
	}
	if got := strings.Join(steps(t, d)["power"].Fix.Cmds[0].Argv, " "); got != "pmset -a sleep 0 disksleep 0 autorestart 1 womp 1 powernap 0" {
		t.Errorf("power fix = %q", got)
	}
	// the privileged commands of the host part are all separate argvs with Sudo set
	for _, name := range []string{"workharbor-user", "power", "firewall", "prefix"} {
		for _, cmd := range steps(t, d)[name].Fix.Cmds {
			if !cmd.Sudo {
				t.Errorf("%s: a privileged command is not marked Sudo: %v", name, cmd.Argv)
			}
		}
	}
	// FileVault is guided: its recovery key must never pass through whr
	if fv := steps(t, d)["filevault"].Fix; len(fv.Cmds) != 0 || fv.Do != nil || !strings.Contains(fv.Guide, "recovery key") {
		t.Errorf("filevault fix %+v", fv)
	}
}

func TestSecretsAreGeneratedOrTypedAndWrittenPrivatelyWithoutOverwriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "whr")
	d := Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: t.TempDir(), GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502}
	st := steps(t, d)
	ctx := context.Background()

	if err := st["config-dir"].Fix.Do(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := status(steps(t, d)["config-dir"]); got != OK {
		t.Error("the directory is not private after its fix")
	}
	if err := st["api-token"].Fix.Do(ctx, nil); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(dir, "api.token")
	fi, err := os.Stat(tok)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token %v %v", fi, err)
	}
	first, _ := os.ReadFile(filepath.Clean(tok))
	if len(strings.TrimSpace(string(first))) < 40 {
		t.Errorf("a token of %d characters", len(first))
	}
	if err := st["api-token"].Fix.Do(ctx, nil); err == nil || !strings.Contains(err.Error(), "not overwritten") {
		t.Errorf("a second token = %v, want a refusal", err)
	}
	if again, _ := os.ReadFile(filepath.Clean(tok)); string(again) != string(first) {
		t.Error("the token was overwritten")
	}
	if got, _ := status(steps(t, d)["api-token"]); got != OK {
		t.Error("the token check fails after its fix")
	}

	// the SSH authority is optional: absent is fine, made it is a private key, never overwritten
	if got, detail := status(steps(t, d)["ssh-ca"]); got != OK || !strings.Contains(detail, "off") {
		t.Errorf("no authority = %s %q", got, detail)
	}
	if err := st["ssh-ca"].Fix.Do(ctx, nil); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ssh-ca")
	if fi, err := os.Stat(ca); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("authority key %v %v", fi, err)
	}
	if got, detail := status(steps(t, d)["ssh-ca"]); got != OK || !strings.Contains(detail, "console.ssh_ca_key_file") {
		t.Errorf("authority = %s %q", got, detail)
	}
	if err := st["ssh-ca"].Fix.Do(ctx, nil); err == nil {
		t.Error("a second authority key overwrote the first")
	}

	// the API key is typed, never echoed, never shown
	key := "sk-ant-api03-" + strings.Repeat("x", 30)
	p := &answers{secrets: []string{key}}
	if err := st["agent-key"].Fix.Do(ctx, p); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "agent.env")
	if fi, _ := os.Stat(env); fi.Mode().Perm() != 0o600 {
		t.Errorf("agent.env mode %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Clean(env)); string(b) != "ANTHROPIC_API_KEY="+key+"\n" {
		t.Errorf("agent.env %q", b)
	}
	if strings.Contains(strings.Join(p.shown, ""), key) {
		t.Error("the key was shown")
	}
	if err := st["agent-key"].Fix.Do(ctx, &answers{secrets: []string{key}}); err == nil {
		t.Error("an existing key file was overwritten")
	}
	for _, bad := range []string{`{"accessToken":"synthetic-value-0123456789"}`, `accessToken:synthetic-value-0123456789`} {
		_ = os.Remove(env)
		err := st["agent-key"].Fix.Do(ctx, &answers{secrets: []string{bad}})
		if err == nil || !strings.Contains(err.Error(), "API key, not a setup-token") || strings.Contains(err.Error(), "synthetic") {
			t.Errorf("%q: %v", bad, err)
		}
		if _, err := os.Stat(env); err == nil {
			t.Errorf("%q left a file behind", bad)
		}
	}
	for _, empty := range []string{"", "   "} {
		_ = os.Remove(env)
		err := st["agent-key"].Fix.Do(ctx, &answers{secrets: []string{empty}})
		if err == nil || !strings.Contains(err.Error(), "no key entered") || strings.Contains(err.Error(), "setup-token") {
			t.Errorf("%q: %v", empty, err)
		}
		if _, err := os.Stat(env); err == nil {
			t.Errorf("%q left a file behind", empty)
		}
	}
	for _, bad := range []string{"short", "has space in it 1234567890", "a=b1234567890123456789"} {
		_ = os.Remove(env)
		if err := st["agent-key"].Fix.Do(ctx, &answers{secrets: []string{bad}}); err == nil {
			t.Errorf("%q was accepted as a key", bad)
		}
		if _, err := os.Stat(env); err == nil {
			t.Errorf("%q left a file behind", bad)
		}
	}
}

// The configuration is written in two steps, because `whr github app create`
// needs the first part and the App needs that command: no cycle, and the wizard
// edits an existing file as a diff after a y, atomically, keeping unknown keys.
func TestTheConfigurationIsWrittenInTwoStepsWithoutADropOfAnythingUnknown(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "whr")
	d := Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: home, GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502}
	ctx := context.Background()
	st := steps(t, d)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st["config-dir"].Fix.Do(ctx, nil))
	must(st["api-token"].Fix.Do(ctx, nil))

	if got, _ := status(st["config-base"]); got != Fail {
		t.Fatal("no config file yet")
	}
	if err := st["config-base"].Fix.Do(ctx, &answers{lines: []string{"not a repo", "", ""}}); err == nil {
		t.Error("a bad repository name was written")
	}
	if _, err := os.Stat(d.ConfigPath); err == nil {
		t.Fatal("a refused answer left a file")
	}
	must(st["config-base"].Fix.Do(ctx, &answers{confirm: true, lines: []string{"wstein/workharbor", "", ""}}))
	fi, _ := os.Stat(d.ConfigPath)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", fi.Mode().Perm())
	}
	if got, detail := status(steps(t, d)["config-base"]); got != OK {
		t.Errorf("config-base = %s %q after its fix", got, detail)
	}
	if got, _ := status(steps(t, d)["config-github"]); got != Fail {
		t.Error("the base file is not a whole configuration: config-github must still fail")
	}
	if err := st["config-base"].Fix.Do(ctx, &answers{lines: []string{"a/b", "", ""}}); err == nil {
		t.Error("an existing configuration was overwritten")
	}

	// no App key yet
	if err := st["config-github"].Fix.Do(ctx, &answers{confirm: true}); err == nil || !strings.Contains(err.Error(), "github-app") {
		t.Errorf("config-github without an App = %v", err)
	}
	// a key the wizard does not know in the file, and the App's key beside it
	var m map[string]any
	raw, _ := os.ReadFile(d.ConfigPath)
	must(json.Unmarshal(raw, &m))
	m["a_key_from_the_future"] = map[string]any{"x": 1}
	raw, _ = json.MarshalIndent(m, "", "  ")
	must(os.WriteFile(d.ConfigPath, raw, 0o600))
	must(os.WriteFile(filepath.Join(dir, "github-app-77.pem"), []byte("-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"), 0o600))

	// declined: nothing changes
	decline := &answers{confirm: false}
	if err := st["config-github"].Fix.Do(ctx, decline); err == nil {
		t.Error("a declined change succeeded")
	}
	if after, _ := os.ReadFile(d.ConfigPath); string(after) != string(raw) {
		t.Error("a declined change was written")
	}
	if len(decline.shown) != 1 || !strings.Contains(decline.shown[0], `+     "app_id": 77`) {
		t.Errorf("the diff shown: %v", decline.shown)
	}
	// confirmed: written atomically, keeping the unknown key
	must(st["config-github"].Fix.Do(ctx, &answers{confirm: true}))
	after, _ := os.ReadFile(d.ConfigPath)
	if !strings.Contains(string(after), "a_key_from_the_future") || !strings.Contains(string(after), `"app_id": 77`) {
		t.Errorf("the edited configuration lost a key or lacks the App:\n%s", after)
	}
	if fi, _ := os.Stat(d.ConfigPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v after the edit", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(dir); func() bool {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".whr-config-") {
				return true
			}
		}
		return false
	}() {
		t.Error("a temporary file was left beside the configuration")
	}
}

func TestStepNamesAreKebabCaseInTheWizardsOrderWithoutACycle(t *testing.T) {
	kebab := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	all := Checks(hostDeps(scripted{}))
	for _, c := range all {
		if !kebab.MatchString(c.Name) {
			t.Errorf("%q is not kebab-case", c.Name)
		}
		if c.Title == "" && c.Phase != "" {
			t.Errorf("step %s has no title", c.Name)
		}
	}
	var user []string
	for _, c := range Steps(all, PhaseUser) {
		user = append(user, c.Name)
	}
	want := "config-dir api-token agent-key ssh-ca container-start container-kernel standard-user-check config-base public-url github-app config-github tool-store service-install drop-admin"
	if strings.Join(user, " ") != want {
		t.Errorf("user steps %v\nwant %s", user, want)
	}
	if len(Steps(all, PhaseHost)) < 8 {
		t.Errorf("host steps %d", len(Steps(all, PhaseHost)))
	}
	if len(Shared(all)) == 0 || len(Shared(all))+len(Steps(all, PhaseHost))+len(Steps(all, PhaseUser)) != len(all) {
		t.Error("every check is shared or a step of one phase")
	}
}

// The file root installs is never written where another account could have
// prepared the directory or a link: a directory that is loose or a link is
// refused, and a link in place of the file is replaced, not followed.
func TestWriteTempRefusesADirectoryOthersControl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := setupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test making the directory too open
		t.Fatal(err)
	}
	if err := writeTemp(sshdTemp(), "x"); err == nil {
		t.Error("a 0755 directory was accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory, 0700 is private
		t.Fatal(err)
	}
	victim := filepath.Join(home, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, sshdTemp()); err != nil {
		t.Fatal(err)
	}
	if err := writeTemp(sshdTemp(), "new"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" { //nolint:gosec // a file the test made
		t.Errorf("the link was followed: victim now %q", b)
	}
	if fi, err := os.Lstat(sshdTemp()); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("the temp file is not a fresh 0600 file: %v %v", fi, err)
	}

	other := filepath.Join(home, "elsewhere")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	if err := writeTemp(sshdTemp(), "x"); err == nil {
		t.Error("a linked directory was accepted")
	}
}

const autologoutKey = "defaults read /Library/Preferences/.GlobalPreferences com.apple.autologout.AutoLogOutDelay"

func TestAutomaticLogOutIsOffWhenTheKeyIsAbsentOrZero(t *testing.T) {
	for name, tc := range map[string]struct {
		out    scripted
		want   Status
		detail string
	}{
		"not set (defaults says so)":        {scripted{autologoutKey: "ERR:exit status 1: The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, OK, "not set"},
		"absent key, as macOS words it":     {scripted{autologoutKey: "ERR:exit status 1: Could not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication'"}, OK, "key not set: the system default applies (default off)"},
		"another key is not this one":       {scripted{autologoutKey: "ERR:exit status 1: Could not find key 'com.apple.other' in domain 'kCFPreferencesAnyApplication'"}, NotVerified, "not known"},
		"another domain is not this one":    {scripted{autologoutKey: "ERR:exit status 1: Could not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'x'"}, NotVerified, "not known"},
		"does not exist for another key":    {scripted{autologoutKey: "ERR:exit status 1: The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.other) does not exist"}, NotVerified, "not known"},
		"pair message plus other text":      {scripted{autologoutKey: "ERR:exit status 1: permission denied; The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, NotVerified, "not known"},
		"absent message plus text":          {scripted{autologoutKey: "ERR:exit status 1: Could not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication' (denied)"}, NotVerified, "not known"},
		"absent key, Error prefix and stop": {scripted{autologoutKey: "ERR:exit status 1: Error: Could not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication'."}, OK, "key not set"},
		"unset pair, Error prefix":          {scripted{autologoutKey: "ERR:exit status 1: Error: The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, OK, "not set"},
		"unset pair, final stop":            {scripted{autologoutKey: "ERR:exit status 1: The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist."}, OK, "not set"},
		"double zero":                       {scripted{autologoutKey: "00\n"}, OK, "off"},
		"plus sign":                         {scripted{autologoutKey: "+60\n"}, NotVerified, "+60"},
		"negative":                          {scripted{autologoutKey: "-5\n"}, NotVerified, "-5"},
		"real header with exit prefix":      {scripted{autologoutKey: "ERR:exit status 1: 2026-10-07 06:42:22.545 defaults[44241:79688178] \nThe domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, OK, "not set"},
		"real header without prefix":        {scripted{autologoutKey: "ERR:2026-10-07 06:42:22.545 defaults[44241:79688178] \nThe domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, OK, "not set"},
		"absent key with header":            {scripted{autologoutKey: "ERR:exit status 1: 2026-10-07 06:42:22.545 defaults[44241:79688178] \nCould not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication'"}, OK, "key not set"},
		"absent key header, no prefix":      {scripted{autologoutKey: "ERR:2026-10-07 06:42:22.545 defaults[44241:79688178] \nCould not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication'"}, OK, "key not set"},
		"odd header is not a header":        {scripted{autologoutKey: "ERR:exit status 1: warning: something\nThe domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, NotVerified, "not known"},
		"prefix text before the message":    {scripted{autologoutKey: "ERR:x exit status 1: The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist"}, NotVerified, "not known"},
		"suffix text after the message":     {scripted{autologoutKey: "ERR:exit status 1: The domain/default pair of (/Library/Preferences/.GlobalPreferences, com.apple.autologout.AutoLogOutDelay) does not exist!"}, NotVerified, "not known"},
		"text before absent message":        {scripted{autologoutKey: "ERR:denied: Could not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication'"}, NotVerified, "not known"},
		"suffix after absent message":       {scripted{autologoutKey: "ERR:Could not find key 'com.apple.autologout.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication' x"}, NotVerified, "not known"},
		"loose does not exist":              {scripted{autologoutKey: "ERR:exit status 1: the file does not exist"}, NotVerified, "not known"},
		"unreadable value":                  {scripted{autologoutKey: "garbled\n"}, NotVerified, "garbled"},
		"defaults fails otherwise":          {scripted{autologoutKey: "ERR:signal: killed"}, NotVerified, "not known"},
		"permission error":                  {scripted{autologoutKey: "ERR:exit status 1: Could not read the plist: permission denied"}, NotVerified, "not known"},
		"no answer at all":                  {scripted{}, NotVerified, "not known"},
		"zero":                              {scripted{autologoutKey: "0\n"}, OK, "off"},
		"ten minutes":                       {scripted{autologoutKey: "600\n"}, Fail, "after 600 seconds"},
	} {
		got, detail := status(steps(t, hostDeps(tc.out))["autologout"])
		if got != tc.want || !strings.Contains(detail, tc.detail) {
			t.Errorf("%s: %s %q, want %s with %q", name, got, detail, tc.want, tc.detail)
		}
	}
	d := hostDeps(scripted{})
	d.GOOS = "linux"
	if got, _ := status(steps(t, d)["autologout"]); got != NotVerified {
		t.Errorf("off a Mac: %s", got)
	}
	// the fix is a guide, never a command: whr changes no login setting
	fix := steps(t, hostDeps(scripted{}))["autologout"].Fix
	if fix == nil || len(fix.Cmds) != 0 || fix.Guide == "" || fix.Open == "" {
		t.Errorf("fix = %+v", fix)
	}
}

const (
	goodVolume = "   Volume Name:               ssd\n   FileVault:                 Yes (Unlocked)\n   Owners:                    Enabled\n"
	noOwners   = "   FileVault:                 Yes\n   Owners:                    Disabled\n"
	plain      = "   FileVault:                 No\n   Owners:                    Enabled\n"
)

// writeRoots writes a configuration whose workspace roots are the given
// directories, which must exist: a root is resolved before its disk is asked.
func writeRoots(t *testing.T, roots ...string) Deps {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"roots": map[string]any{"workspaces": roots}})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d := hostDeps(nil)
	d.ConfigPath = path
	return d
}

// root makes a directory and returns its resolved path.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// onDisk scripts `df -P` saying a directory is on a mount point.
func onDisk(dir, mount string) (string, string) {
	return "df -P " + dir, "Filesystem 512-blocks Used Available Capacity Mounted on\n/dev/disk5s1 100 1 99 1% " + mount + "\n"
}

func TestMediaAnalysisFailsOnlyOnMeasuredCPU(t *testing.T) {
	d := hostDeps(nil)
	d.User = WhrUser
	cache := "du -sk /Users/workharbor/Library/Caches/com.apple.mediaanalysisd"
	check := func(r scripted) (Status, string) {
		d.Runner = r
		return status(steps(t, d)["media-analysis"])
	}
	const ps = "ps -axo user=,pcpu=,time=,comm="
	const bin = "/System/Library/PrivateFrameworks/MediaAnalysis.framework/Versions/A/mediaanalysisd"
	if st, detail := check(scripted{ps: " root 12.0 1:02.03 /usr/sbin/cfprefsd\n", cache: "2048\tx"}); st != OK || !strings.Contains(detail, "not running") || !strings.Contains(detail, "2 MiB") {
		t.Errorf("no process: %s %q", st, detail)
	}
	if st, detail := check(scripted{ps: " whr 3.0 0:10.00 " + bin + "\n"}); st != OK || !strings.Contains(detail, "3%") {
		t.Errorf("idle: %s %q", st, detail)
	}
	if st, detail := check(scripted{ps: " whr 222.0 21:35:00 " + bin + "\n", cache: "9437184\tx"}); st != Fail || !strings.Contains(detail, "222%") || !strings.Contains(detail, "21:35:00") || !strings.Contains(detail, "9216 MiB") {
		t.Errorf("busy: %s %q", st, detail)
	}
	// an idle instance of another account listed first does not hide a busy one
	if st, detail := check(scripted{ps: " werner 1.0 0:01.00 " + bin + "\n whr 222.0 21:35:00 " + bin + "\n"}); st != Fail || !strings.Contains(detail, "of whr") {
		t.Errorf("two instances: %s %q", st, detail)
	}
	// run as another account, the whr user's cache is not read
	d.User = "werner"
	if st, detail := check(scripted{ps: " whr 3.0 0:10.00 " + bin + "\n", cache: "2048\tx"}); st != OK || strings.Contains(detail, "2 MiB") || !strings.Contains(detail, "run whr doctor as workharbor") {
		t.Errorf("another account: %s %q", st, detail)
	}
	d.User = WhrUser
	if st, _ := check(scripted{}); st != NotVerified {
		t.Errorf("ps that does not answer: %s", st)
	}
	if st, _ := check(scripted{ps: " whr x 1:00 mediaanalysisd\n"}); st != NotVerified {
		t.Errorf("unreadable ps: %s", st)
	}
	fix := steps(t, d)["media-analysis"].Fix
	if len(fix.Cmds) != 0 || fix.Build != nil || fix.Do != nil || fix.Open == "" || strings.Contains(fix.Guide, "killall ") {
		t.Errorf("the fix must be guided only: %+v", fix)
	}
}

func TestSpotlightIsOffOnWorkspaceVolumes(t *testing.T) {
	a := root(t)
	ka, va := onDisk(a, "/Volumes/ssd")
	d := writeRoots(t, a)
	check := func(r scripted) (Status, string) {
		d.Runner = r
		return status(steps(t, d)["spotlight"])
	}
	if st, _ := check(scripted{ka: va, "mdutil -s /Volumes/ssd": "/Volumes/ssd:\n\tIndexing disabled.\n"}); st != OK {
		t.Errorf("indexing off: %s", st)
	}
	on := scripted{ka: va, "mdutil -s /Volumes/ssd": "/Volumes/ssd:\n\tIndexing enabled.\n"}
	if st, detail := check(on); st != Fail || !strings.Contains(detail, "/Volumes/ssd") {
		t.Errorf("indexing on: %s %q", st, detail)
	}
	if st, _ := check(scripted{ka: va}); st != NotVerified {
		t.Errorf("mdutil that does not answer: %s", st)
	}
	for _, out := range []string{"", "Error: unknown indexing state.\n", "/Volumes/ssd:\n\tNo index.\n"} {
		if st, _ := check(scripted{ka: va, "mdutil -s /Volumes/ssd": out}); st != NotVerified {
			t.Errorf("unrecognised mdutil output %q: %s", out, st)
		}
	}
	ki, vi := onDisk(a, "/System/Volumes/Data")
	if st, detail := check(scripted{ki: vi}); st != NotVerified || !strings.Contains(detail, "internal disk") {
		t.Errorf("internal root: %s %q", st, detail)
	}
	d.Runner = on
	cmds, err := steps(t, d)["spotlight"].Fix.Build(context.Background(), nil)
	if err != nil || len(cmds) != 1 || !cmds[0].Sudo || strings.Join(cmds[0].Argv, " ") != "mdutil -i off /Volumes/ssd" {
		t.Errorf("fix commands = %+v, %v", cmds, err)
	}
}

func TestAWorkspaceVolumeMustBeEncryptedAndHonourOwnership(t *testing.T) {
	check := func(out scripted, roots ...string) (Status, string) {
		d := writeRoots(t, roots...)
		d.Runner = out
		return status(steps(t, d)["workspace-volume"])
	}
	a, b := root(t), root(t)
	ka, va := onDisk(a, "/Volumes/ssd")
	kb, vb := onDisk(b, "/Volumes/ssd")
	if st, detail := check(scripted{ka: va, kb: vb, "diskutil info /Volumes/ssd": goodVolume}, a, b); st != OK {
		t.Errorf("a good volume: %s %q", st, detail)
	}
	ki, vi := onDisk(a, "/System/Volumes/Data")
	if st, detail := check(scripted{ki: vi}, a); st != OK || !strings.Contains(detail, "internal disk") {
		t.Errorf("roots on the internal disk: %s %q", st, detail)
	}
	for name, tc := range map[string]struct{ out, want string }{
		"no ownership":  {noOwners, "ignores file ownership"},
		"not encrypted": {plain, "is not encrypted"},
	} {
		if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": tc.out}, a); st != Fail || !strings.Contains(detail, tc.want) {
			t.Errorf("%s: %s %q", name, st, detail)
		}
	}
	if st, _ := check(scripted{ka: va}, a); st != NotVerified {
		t.Errorf("diskutil that does not answer: %s", st)
	}
	// output this check does not know is not a failure: only a stated No or
	// Disabled is (the format is unverified on macOS 26)
	for name, out := range map[string]string{
		"empty output":      "",
		"no keys":           "   Volume Name:               ssd\n",
		"unknown encrypted": "   FileVault:                 Maybe\n   Owners:                    Enabled\n",
		"unknown owners":    "   FileVault:                 Yes\n   Owners:                    Perhaps\n",
		"owners missing":    "   FileVault:                 Yes\n",
		"None owners":       "   FileVault:                 Yes\n   Owners:                    None\n",
		"Nope filevault":    "   FileVault:                 Nope\n   Owners:                    Enabled\n",
		"Yesterday":         "   FileVault:                 Yesterday\n   Owners:                    Enabled\n",
		"Enabledish owners": "   FileVault:                 Yes\n   Owners:                    Enabledish\n",
	} {
		if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": out}, a); st != NotVerified || !strings.Contains(detail, "/Volumes/ssd") {
			t.Errorf("%s: %s %q, want not_verified", name, st, detail)
		}
	}
	// the Encrypted key stands in when FileVault is not stated
	if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": "   Encrypted:                 Yes\n   Owners:                    Enabled\n"}, a); st != OK {
		t.Errorf("Encrypted: Yes alone: %s %q", st, detail)
	}
	if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": "   Encrypted:                 No\n   Owners:                    Enabled\n"}, a); st != Fail || !strings.Contains(detail, "is not encrypted") {
		t.Errorf("Encrypted: No alone: %s %q", st, detail)
	}
	if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": "   FileVault:                 Maybe\n   Encrypted:                 Yes\n   Owners:                    Enabled\n"}, a); st != OK {
		t.Errorf("unknown FileVault with Encrypted: Yes: %s %q", st, detail)
	}
	// a stated failure still wins over an unknown field
	if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": "   FileVault:                 No\n"}, a); st != Fail || !strings.Contains(detail, "is not encrypted") {
		t.Errorf("a stated No with owners unknown: %s %q", st, detail)
	}
	// a mount point that holds spaces is read whole
	ks, vs := onDisk(a, "/Volumes/My SSD")
	if st, detail := check(scripted{ks: vs, "diskutil info /Volumes/My SSD": plain}, a); st != Fail || !strings.Contains(detail, "/Volumes/My SSD is not encrypted") {
		t.Errorf("a mount point with a space: %s %q", st, detail)
	}
	// df that does not answer, or a root that is not there: not a pass
	if st, _ := check(scripted{}, a); st != NotVerified {
		t.Errorf("df that does not answer: %s", st)
	}
	if st, _ := check(scripted{}, filepath.Join(a, "missing")); st != NotVerified {
		t.Errorf("a root that does not exist: %s", st)
	}
	// a link to an external disk is that disk, not the internal one
	link := filepath.Join(root(t), "ws")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	if st, detail := check(scripted{ka: va, "diskutil info /Volumes/ssd": plain}, link); st != Fail || !strings.Contains(detail, "is not encrypted") {
		t.Errorf("a root linked to an unencrypted external disk: %s %q", st, detail)
	}
	// without a configuration the roots are not known: not verified, never a pass
	d := hostDeps(scripted{})
	d.ConfigPath = filepath.Join(t.TempDir(), "none.json")
	if st, _ := status(steps(t, d)["workspace-volume"]); st != NotVerified {
		t.Errorf("no configuration: %s", st)
	}

	// the fix turns ownership on for the volumes that need it, as sudo argv, and
	// leaves encryption to the human
	c := root(t)
	kc, vc := onDisk(c, "/Volumes/good")
	d = writeRoots(t, a, c)
	d.Runner = scripted{ka: va, kc: vc, "diskutil info /Volumes/ssd": noOwners, "diskutil info /Volumes/good": goodVolume}
	fix := steps(t, d)["workspace-volume"].Fix
	cmds, err := fix.Build(context.Background(), nil)
	if err != nil || len(cmds) != 1 || !cmds[0].Sudo || strings.Join(cmds[0].Argv, " ") != "diskutil enableOwnership /Volumes/ssd" {
		t.Errorf("fix commands = %+v, %v", cmds, err)
	}
	if !strings.Contains(fix.Guide, "Encryption is not") {
		t.Errorf("the guide does not leave encryption to the human: %q", fix.Guide)
	}
}

// recording is a scripted runner that remembers every command it was asked.
type recording struct {
	scripted
	calls []string
}

func (r *recording) Output(ctx context.Context, argv ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(argv, " "))
	return r.scripted.Output(ctx, argv...)
}

// Outside the Aqua session (SSH) the container checks are not_verified and run
// no container command; in Aqua they behave as before (#156).
func TestContainerChecksNeedTheDesktopSession(t *testing.T) {
	names := []string{"container-kernel", "container-start", "standard-user-check"}
	for _, session := range []string{"Background", "System"} {
		r := &recording{scripted: scripted{"launchctl managername": session}}
		st := steps(t, hostDeps(r))
		for _, name := range names {
			got, detail := status(st[name])
			if got != NotVerified || !strings.Contains(detail, "workharbor's own desktop session (Screen Sharing)") || !strings.Contains(detail, "list and show commands") || strings.Contains(detail, "whr service status") {
				t.Errorf("%s over %s = %s %q", name, session, got, detail)
			}
		}
		for _, c := range r.calls {
			if strings.HasPrefix(c, "container") {
				t.Errorf("ran %q outside Aqua", c)
			}
		}
	}
	r := &recording{scripted: scripted{
		"launchctl managername":   "Aqua",
		"container system status": "apiserver is running",
		"container list --all":    "",
		"launchctl print gui/501": launchdPrint("com.apple.container.apiserver"),
	}}
	d := hostDeps(r)
	d.Home = t.TempDir()
	if err := os.MkdirAll(filepath.Join(d.Home, "Library", "Application Support", "com.apple.container", "kernels"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Home, "Library", "Application Support", "com.apple.container", "kernels", "vmlinux"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := steps(t, d)
	for _, name := range names {
		if got, detail := status(st[name]); got != OK {
			t.Errorf("%s in Aqua = %s %q", name, got, detail)
		}
	}
}

// A running container system with no kernel is a failure the kernel step can fix;
// a stopped one is not verifiable (#265).
func TestKernelCheckSeesAMissingKernelOnceTheSystemRuns(t *testing.T) {
	d := hostDeps(scripted{"launchctl managername": "Aqua", "container system status": "apiserver is running"})
	d.Home = t.TempDir()
	if got, detail := status(steps(t, d)["container-kernel"]); got != Fail || !strings.Contains(detail, "no Linux kernel") ||
		!strings.Contains(detail, filepath.Join(d.Home, "Library", "Application Support", "com.apple.container", "kernels")) || !strings.Contains(detail, "unverified until issue #73") {
		t.Errorf("running without a kernel must name the directory it read and that it is unverified = %s %q", got, detail)
	}
	d = hostDeps(scripted{"launchctl managername": "Aqua", "container system status": "ERR:XPC connection error"})
	d.Home = t.TempDir()
	if got, _ := status(steps(t, d)["container-kernel"]); got != NotVerified {
		t.Errorf("stopped = %s", got)
	}
}

// Outside the Aqua session the detail names the session by who the target
// account is, as the guide does (#265).
func TestDesktopOnlyDetailDependsOnTheTargetUser(t *testing.T) {
	for _, c := range []struct{ user, account, want, not string }{
		{"werner", "werner", "this desktop session", "Screen Sharing"},
		{"werner", "", "workharbor's own desktop session (Screen Sharing)", "this desktop session"},
		{"werner", "other", "other's own desktop session", "this desktop session"},
	} {
		d := hostDeps(scripted{"launchctl managername": "Background"})
		d.User, d.Account = c.user, c.account
		_, detail := status(steps(t, d)["container-start"])
		if !strings.Contains(detail, c.want) || strings.Contains(detail, c.not) {
			t.Errorf("%s for %q: %q", c.user, c.account, detail)
		}
	}
}

// The guide names the session by who the target account is (#265).
func TestDesktopGuideDependsOnTheTargetUser(t *testing.T) {
	for _, c := range []struct{ user, account, want, not string }{
		{"werner", "werner", "this desktop session", "Screen Sharing"},
		{"werner", "", "workharbor's own desktop session (Screen Sharing)", "this desktop session"},
		{"werner", "other", "other's own desktop session", "this desktop session"},
		{"workharbor", "", "this desktop session", "Screen Sharing"},
	} {
		d := hostDeps(scripted{})
		d.User, d.Account = c.user, c.account
		st := steps(t, d)
		for _, name := range []string{"standard-user-check"} {
			g := st[name].Fix.Guide
			if !strings.Contains(g, c.want) || strings.Contains(g, c.not) {
				t.Errorf("%s as %s for %q: %q", name, c.user, c.account, g)
			}
		}
	}
}

// A step whose fix needs a service must come after the step that starts it, so
// the order cannot put container-kernel before container-start again (#265).
func TestNoStepNeedsAServiceALaterStepStarts(t *testing.T) {
	provided := map[string]bool{}
	needed := 0
	for _, c := range Steps(Checks(hostDeps(scripted{})), PhaseUser) {
		if c.Needs != "" {
			needed++
			if !provided[c.Needs] {
				t.Errorf("step %s needs %q, which no earlier step provides", c.Name, c.Needs)
			}
		}
		if c.Provides != "" {
			provided[c.Provides] = true
		}
	}
	if needed == 0 {
		t.Error("no step declares a need: the test checks nothing")
	}
}

func TestWhrUserFailsOnlyOnARecognizableNotFound(t *testing.T) {
	const key = "dscl . -read /Users/workharbor UniqueID"
	for _, msg := range []string{
		"exit status 56",
		"exit status 56: <dscl_cmd> DS Error: -14136 (eDSRecordNotFound)",
	} {
		got, detail := status(steps(t, hostDeps(scripted{key: "ERR:" + msg, dsclLegacy: "ERR:exit status 56"}))["workharbor-user"])
		if got != Fail || detail != "there is no user workharbor" {
			t.Errorf("%q = %s %q, want fail", msg, got, detail)
		}
	}
	for _, msg := range []string{
		"exit status 1: Operation not permitted",
		"exit status 2: eDSServiceNotAvailable",
		"exit status 70: odd output",
		"exit status 1",
		"exit status 1: <dscl_cmd> DS Error: Record does not exist",
		"exit status 1: the record does not exist",
		"exit status 1: xeDSRecordNotFoundx",
		"exit status 1: eDSRecordNotFoundish",
		"exit status 1: permission denied (not eDSRecordNotFound)",
	} {
		got, detail := status(steps(t, hostDeps(scripted{key: "ERR:" + msg}))["workharbor-user"])
		if got != NotVerified || !strings.Contains(detail, msg) || strings.Contains(detail, "there is no user") {
			t.Errorf("%q = %s %q, want not_verified carrying the error", msg, got, detail)
		}
	}
}

func TestFileChecksFailOnlyOnAbsentNotOnUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) }) //nolint:gosec // a directory must be enterable again for cleanup
	d := hostDeps(scripted{})
	d.Prefix = locked
	if got, detail := status(steps(t, d)["prefix"]); got != NotVerified || !strings.Contains(detail, "permission denied") {
		t.Errorf("unreadable prefix = %s %q", got, detail)
	}
	d.Prefix = filepath.Join(t.TempDir(), "absent")
	if got, _ := status(steps(t, d)["prefix"]); got != Fail {
		t.Errorf("absent prefix = %s", got)
	}
}

// failing is a Runner whose every command fails with err.
type failing struct{ err error }

func (f failing) Output(context.Context, ...string) ([]byte, error) { return nil, f.err }

func TestWhrUserFailsOnARealExitStatus56ButNotOnOtherRealExits(t *testing.T) {
	realExit := func(code string) error {
		err := exec.CommandContext(t.Context(), "sh", "-c", "echo boom >&2; exit "+code).Run() //nolint:gosec // a fixed test script, the code is a literal
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("not an exit error: %v", err)
		}
		return fmt.Errorf("%w: %s", ee, "boom") // as setup.Terminal wraps stderr
	}
	// the message carries no "56" text of its own: only the exit code can match
	err := hidden{realExit("56")}
	if got, detail := status(steps(t, hostDeps(failing{err}))["workharbor-user"]); got != Fail {
		t.Errorf("exit 56 = %s %q", got, detail)
	}
	if got, detail := status(steps(t, hostDeps(failing{realExit("185")}))["workharbor-user"]); got != NotVerified || !strings.Contains(detail, "exit status 185") || !strings.Contains(detail, "boom") {
		t.Errorf("exit 185 = %s %q", got, detail)
	}
}

func TestWhrUserNotFoundByTextAlone(t *testing.T) {
	d := hostDeps(scripted{"dscl . -read /Users/workharbor UniqueID": "ERR:<dscl_cmd> DS Error: -14136 (eDSRecordNotFound)", dsclLegacy: "ERR:exit status 56"})
	if got, detail := status(steps(t, d)["workharbor-user"]); got != Fail {
		t.Errorf("%s %q", got, detail)
	}
}

func lockedDir(t *testing.T) (locked, absent string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	base := t.TempDir()
	locked = filepath.Join(base, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) }) //nolint:gosec // a directory must be enterable again for cleanup
	return locked, filepath.Join(t.TempDir(), "absent")
}

func TestUnreadablePathsAreNotVerifiedAndAbsentOnesFail(t *testing.T) {
	locked, absent := lockedDir(t)
	d := hostDeps(scripted{})
	d.SSHDFile = locked
	if got, detail := status(steps(t, d)["ssh-keys-only"]); got != NotVerified || !strings.Contains(detail, "permission denied") {
		t.Errorf("ssh unreadable = %s %q", got, detail)
	}
	d.SSHDFile = absent
	if got, _ := status(steps(t, d)["ssh-keys-only"]); got != Fail {
		t.Errorf("ssh absent = %s", got)
	}
	d = hostDeps(scripted{})
	d.ConfigPath = filepath.Join(locked, "config.json")
	st := steps(t, d)
	for _, name := range []string{"config-dir", "agent-key", "ssh-ca"} {
		if got, detail := status(st[name]); got != NotVerified || !strings.Contains(detail, "permission denied") {
			t.Errorf("%s unreadable = %s %q", name, got, detail)
		}
	}
	d.ConfigPath = filepath.Join(absent, "config.json")
	st = steps(t, d)
	if got, _ := status(st["config-dir"]); got != Fail {
		t.Errorf("config-dir absent = %s", got)
	}
	for _, name := range []string{"agent-key", "ssh-ca"} {
		if got, _ := status(st[name]); got != OK {
			t.Errorf("%s absent = %s", name, got)
		}
	}
}

func TestBrewPackagesNotVerifiedOnAnUnexpectedBrewError(t *testing.T) {
	const k = "/opt/homebrew/bin/brew list --formula --versions "
	if got, detail := status(steps(t, hostDeps(scripted{k + "git": "ERR:exit status 1"}))["brew-packages"]); got != Fail {
		t.Errorf("not installed = %s %q", got, detail)
	}
	d := hostDeps(scripted{k + "git": "ERR:exit status 2: Error: cannot lock"})
	if got, detail := status(steps(t, d)["brew-packages"]); got != NotVerified || !strings.Contains(detail, "cannot lock") {
		t.Errorf("odd brew error = %s %q", got, detail)
	}
}

// hidden keeps an error's chain but not its text.
type hidden struct{ err error }

func (h hidden) Error() string { return "dscl failed" }
func (h hidden) Unwrap() error { return h.err }

const (
	dsclWorkharbor = "dscl . -read /Users/workharbor UniqueID"
	dsclLegacy     = "dscl . -read /Users/whr UniqueID"
)

func TestMissingWorkharborWithLegacyWhrPointsAtUserWhr(t *testing.T) {
	c := steps(t, hostDeps(scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: "UniqueID: 502"}))["workharbor-user"]
	got, detail := status(c)
	if got != Fail {
		t.Errorf("status = %s, want fail", got)
	}
	if try := strings.Join(c.Fix.Try, "|"); try != "whr setup --user whr|whr doctor --user whr" {
		t.Errorf("Try = %q", try)
	}
	for _, want := range []string{"there is no user workharbor", "legacy"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	if len(c.Fix.Cmds) != 0 {
		t.Errorf("a second account is offered: %+v", c.Fix.Cmds)
	}
	for _, cmd := range c.Fix.Cmds {
		if strings.Contains(strings.Join(cmd.Argv, " "), "addUser") {
			t.Errorf("addUser offered: %+v", cmd)
		}
	}
	if !strings.Contains(strings.Join(c.Fix.Try, "\n"), "--user whr") || strings.Contains(c.Fix.Guide, "sysadminctl") {
		t.Errorf("guide = %q, try = %q", c.Fix.Guide, c.Fix.Try)
	}
	noWrappedCommand(t, c.Name, c.Fix)
}

func TestMissingWorkharborOffersAddUserOnlyWhenLegacyIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name, legacy string
		want         Status
		addUser      bool
	}{
		{"neither exists", "ERR:exit status 56", Fail, true},
		{"legacy not found by record error", "ERR:exit status 1: DS Error: -14136 (eDSRecordNotFound)", Fail, true},
		{"legacy loose wording is not a not-found", "ERR:exit status 1: Record does not exist", NotVerified, false},
		{"legacy unreadable", "ERR:exit status 1: Operation not permitted", NotVerified, false},
		{"legacy unknown failure", "ERR:exit status 70: odd output", NotVerified, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := steps(t, hostDeps(scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: tc.legacy}))["workharbor-user"]
			got, detail := status(c)
			if got != tc.want {
				t.Errorf("status = %s %q, want %s", got, detail, tc.want)
			}
			if strings.Contains(detail, "legacy") && !strings.Contains(detail, "not") {
				t.Errorf("detail claims a legacy account: %q", detail)
			}
			if tc.addUser && (len(c.Fix.Cmds) != 1 || c.Fix.Cmds[0].Argv[1] != "-addUser" || detail != "there is no user workharbor") {
				t.Errorf("addUser fix lost: %q %+v", detail, c.Fix.Cmds)
			}
			if !tc.addUser && len(c.Fix.Cmds) != 0 {
				t.Errorf("creation offered when legacy existence is unknown: %+v", c.Fix.Cmds)
			}
			if !tc.addUser && strings.Contains(detail, "whr setup --user whr") {
				t.Errorf("points at whr without finding it: %q", detail)
			}
		})
	}
}

type legacyCalls struct {
	scripted
	calls *[]string
}

func (r legacyCalls) Output(ctx context.Context, argv ...string) ([]byte, error) {
	*r.calls = append(*r.calls, strings.Join(argv, " "))
	return r.scripted.Output(ctx, argv...)
}

func TestLegacyIsNotLookedUpWhenTheRequestedAccountIsWhrOrExists(t *testing.T) {
	var calls []string
	d := hostDeps(legacyCalls{scripted{dsclLegacy: "ERR:exit status 56"}, &calls})
	d.Account = "whr"
	if got, detail := status(steps(t, d)["workharbor-user"]); got != Fail || detail != "there is no user whr" {
		t.Errorf("whr requested and missing = %s %q", got, detail)
	}
	if len(calls) != 1 {
		t.Errorf("dscl calls = %v, want only the requested account", calls)
	}
	calls = nil
	d = hostDeps(legacyCalls{scripted{dsclWorkharbor: "UniqueID: 503", "dseditgroup -o checkmember -m workharbor admin": "no workharbor is NOT a member of admin"}, &calls})
	if got, _ := status(steps(t, d)["workharbor-user"]); got != OK {
		t.Errorf("existing workharbor = %s", got)
	}
	for _, c := range calls {
		if strings.Contains(c, "/Users/whr ") {
			t.Errorf("legacy looked up although workharbor exists: %v", calls)
		}
	}
}

func userResult(t *testing.T, d Deps) Result {
	t.Helper()
	for _, r := range Run(context.Background(), Steps(Checks(d), PhaseHost), nil) {
		if r.Check == "workharbor-user" {
			return r
		}
	}
	t.Fatal("no workharbor-user result")
	return Result{}
}

func TestLegacyFixCommandOnlyOnTheFoundCase(t *testing.T) {
	found := userResult(t, hostDeps(scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: "UniqueID: 502"}))
	if found.Status != Fail || found.Fix != "whr setup --user whr" {
		t.Errorf("legacy found = %s fix %q", found.Status, found.Fix)
	}
	unread := userResult(t, hostDeps(scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: "ERR:exit status 1: Operation not permitted"}))
	if unread.Status != NotVerified || strings.Contains(unread.Fix, "--user whr") {
		t.Errorf("legacy unreadable = %s fix %q", unread.Status, unread.Fix)
	}
	none := userResult(t, hostDeps(scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: "ERR:exit status 56"}))
	if none.Fix != "whr setup --only workharbor-user" {
		t.Errorf("neither = %q", none.Fix)
	}
}

func TestLegacyLookupIsOnlyForTheDefaultAccount(t *testing.T) {
	var calls []string
	d := hostDeps(legacyCalls{scripted{"dscl . -read /Users/operator UniqueID": "ERR:exit status 56", dsclLegacy: "UniqueID: 502"}, &calls})
	d.Account = "operator"
	c := steps(t, d)["workharbor-user"]
	if got, detail := status(c); got != Fail || detail != "there is no user operator" {
		t.Errorf("%s %q", got, detail)
	}
	if len(c.Fix.Cmds) != 1 || c.Fix.Cmds[0].Argv[1] != "-addUser" || len(calls) != 1 {
		t.Errorf("addUser lost or whr read: %+v %v", c.Fix.Cmds, calls)
	}
}

func TestTheLegacyFixResetsOnEveryRun(t *testing.T) {
	legacy := &mutableRunner{answers: scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: "UniqueID: 502"}}
	c := steps(t, hostDeps(legacy))["workharbor-user"]
	if _, _ = status(c); len(c.Fix.Cmds) != 0 || c.UseUser(Fail) != "whr" {
		t.Fatalf("first run: %+v %q", c.Fix.Cmds, c.UseUser(Fail))
	}
	legacy.answers = scripted{dsclWorkharbor: "ERR:exit status 56", dsclLegacy: "ERR:exit status 56"}
	if got, detail := status(c); got != Fail || detail != "there is no user workharbor" {
		t.Fatalf("second run: %s %q", got, detail)
	}
	if len(c.Fix.Cmds) != 1 || c.Fix.Cmds[0].Argv[1] != "-addUser" || !strings.Contains(c.Fix.Guide, "sysadminctl") || c.UseUser(Fail) != "" {
		t.Errorf("fix not reset: %+v %q", c.Fix, c.UseUser(Fail))
	}
	legacy.answers = scripted{dsclLegacy: "UniqueID: 502", dsclWorkharbor: "ERR:exit status 56"}
	status(c)
	legacy.answers = scripted{dsclWorkharbor: "UniqueID: 503", "dseditgroup -o checkmember -m workharbor admin": "no workharbor is NOT a member of admin"}
	if got, _ := status(c); got != OK || c.UseUser(OK) != "" {
		t.Errorf("an existing account still redirects: %s %q", got, c.UseUser(OK))
	}
}

type mutableRunner struct{ answers scripted }

func (m *mutableRunner) Output(ctx context.Context, argv ...string) ([]byte, error) {
	return m.answers.Output(ctx, argv...)
}

func TestTheAgentKeyStepSaysItAsksForAnAPIKeyNotALogin(t *testing.T) {
	for _, s := range []string{"from the vendor's console", "ANTHROPIC_API_KEY", "not a setup-token or login token", "not echoed"} {
		if !strings.Contains(agentKeyPrompt, s) {
			t.Errorf("prompt lacks %q: %s", s, agentKeyPrompt)
		}
	}
	dir := filepath.Join(t.TempDir(), "whr")
	st := steps(t, Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: t.TempDir(), GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502})
	if !strings.Contains(st["agent-key"].Fix.Desc, agentKeyPrompt) {
		t.Errorf("fix desc %q", st["agent-key"].Fix.Desc)
	}
	// short subscription-shaped input gets the credcheck message and the advice
	for _, bad := range []string{"{", "{}", "\uFEFF{}"} {
		pr := &answers{secrets: []string{bad}}
		err := st["agent-key"].Fix.Do(context.Background(), pr)
		if len(pr.prompts) != 1 || pr.prompts[0] != agentKeyPrompt {
			t.Errorf("prompt passed to Secret = %q", pr.prompts)
		}
		if err == nil || !strings.Contains(err.Error(), "refused: "+"a value shaped like a subscription login") || !strings.Contains(err.Error(), "Use an API key") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestTheAgentKeyStepValidatesExistingFileContents(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		mode           os.FileMode
		fail           bool
	}{
		{"API key", "ANTHROPIC_API_KEY=test-synthetic-api-key-value\n", 0o600, false},
		{"login JSON", "ANTHROPIC_API_KEY={\"accessToken\":\"synthetic-login-value\"}\n", 0o600, true},
		{"login assignment", "ANTHROPIC_API_KEY=accessToken:synthetic-login-value\n", 0o600, true},
		{"login name", "ANTHROPIC_AUTH_TOKEN=synthetic-login-value\n", 0o600, true},
		{"short login JSON", "ANTHROPIC_API_KEY={}\n", 0o600, true},
		{"invalid assignment", "synthetic-invalid-line\n", 0o600, true},
		{"empty", "", 0o600, true},
		{"public file", "ANTHROPIC_API_KEY=test-synthetic-api-key-value\n", 0o644, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.env")
			if err := os.WriteFile(path, []byte(tc.contents), tc.mode); err != nil {
				t.Fatal(err)
			}
			check := steps(t, Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: t.TempDir(), GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502})["agent-key"]
			got, detail := status(check)
			_, err := (&config.Config{AgentAPIKeyEnvFile: path}).AgentAPIKey()
			if tc.fail {
				if err == nil {
					t.Fatal("config accepted the invalid fixture")
				}
				if got != Fail || detail != oneLine(err.Error()) {
					t.Errorf("setup validation differs from config: status %s", got)
				}
			} else if got != OK || err != nil {
				t.Errorf("valid private file failed: status %s", got)
			}
			if strings.Contains(detail, "synthetic-") {
				t.Error("validation exposed a synthetic credential value")
			}
		})
	}
}

func TestTheAgentKeyStepFailsAnEmptyValueWithAnActionableStep(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.env"), []byte("ANTHROPIC_API_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := steps(t, Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: t.TempDir(), GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502})["agent-key"]
	got, detail := status(check)
	if got != Fail || !strings.Contains(detail, "write the value after the =") || !strings.Contains(detail, "remove agent_api_key_env_file from the configuration") || strings.Contains(detail, "D40") || strings.Contains(detail, "setup-token") {
		t.Errorf("status %s, detail %q", got, detail)
	}
}

func TestTheAgentKeyStepRefusesTypedLoginWithoutWriting(t *testing.T) {
	for _, value := range []string{"{}", "{", "\uFEFF{}", "accessToken:synthetic-login-value", "{\"refreshToken\":\"synthetic-login-value\"}"} {
		dir := t.TempDir()
		check := steps(t, Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: t.TempDir(), GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502})["agent-key"]
		p := &answers{secrets: []string{value}}
		err := check.Fix.Do(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "a value shaped like a subscription login") || !strings.Contains(err.Error(), "Use an API key") {
			t.Error("typed login did not receive the credential refusal and advice")
		}
		if err != nil && strings.Contains(err.Error(), value) {
			t.Error("refusal exposed the typed value")
		}
		if len(p.shown) != 0 {
			t.Error("setup showed secret input")
		}
		if _, err := os.Stat(filepath.Join(dir, "agent.env")); !errors.Is(err, os.ErrNotExist) {
			t.Error("refused login left a file")
		}
	}
}

func TestFirewallFilevaultAndLaunchdFailOnlyOnStatedAnswers(t *testing.T) {
	const fw = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	const on, stealthOn = "Firewall is enabled. (State = 1)", "Firewall stealth mode is on"
	fire := func(state, stealth string) (Status, string) {
		return status(steps(t, hostDeps(scripted{fw + " --getglobalstate": state, fw + " --getstealthmode": stealth}))["firewall"])
	}
	// the observed and the inferred wording
	for name, tc := range map[string]struct {
		state, stealth string
		want           Status
	}{
		"all on":               {on, stealthOn, OK},
		"state 2 enabled":      {"Firewall is enabled. (State = 2)", stealthOn, OK},
		"is on, state 1":       {"Firewall is on. (State = 1)", stealthOn, OK},
		"observed off":         {"Firewall is disabled. (State = 0)", "Firewall stealth mode is off", Fail},
		"stealth off":          {on, "Firewall stealth mode is off", Fail},
		"global off only":      {"Firewall is disabled. (State = 0)", stealthOn, Fail},
		"no state suffix":      {"Firewall is enabled", stealthOn, OK},
		"trailing newline":     {on + "\n", stealthOn + "\n", OK},
		"case does not matter": {"firewall is ENABLED. (State = 1)", stealthOn, OK},
	} {
		if got, detail := fire(tc.state, tc.stealth); got != tc.want {
			t.Errorf("firewall %s: %s %q, want %s", name, got, detail, tc.want)
		}
	}
	// everything else, hostile or merely new, is never a pass or a failure
	for _, hostile := range []string{
		"Firewall enabled: false", "Firewall will be enabled after restart", "Firewall was enabled",
		"Failed to turn stealth mode on", "Stealth mode: on=false", "Stealth mode is on? unknown",
		"Firewall isn\u2019t enabled", "Firewall isn't enabled", "Firewall wasn't enabled", "Firewall is un-enabled",
		"Firewall is not enabled", "Firewall is never on", "Firewall is on (State = 0)", "Firewall is disabled. (State = 1)",
		"Firewall is enabled. (State = 0)", "Firewall is on. (State = 2)", "Firewall is enabled. (State = x)",
		"Stealth mode enabled", "Firewall stealth mode is on (State = 1)", "Firewall is enabled.\nFirewall is disabled.",
		"Firewall is enabled. (State = 1) extra", "xFirewall is enabled", "", "Firewall is",
	} {
		if got, detail := fire(hostile, stealthOn); got != NotVerified {
			t.Errorf("firewall state %q = %s %q, want not_verified", hostile, got, detail)
		}
		if got, detail := fire(on, hostile); got != NotVerified {
			t.Errorf("firewall stealth %q = %s %q, want not_verified", hostile, got, detail)
		}
	}
	if got, detail := status(steps(t, hostDeps(scripted{fw + " --getglobalstate": "Firewall is disabled. (State = 0)", fw + " --getstealthmode": "ERR:exit status 1: denied"}))["firewall"]); got != Fail {
		t.Errorf("firewall off with stealth error = %s %q, want fail", got, detail)
	}
	for name, tc := range map[string]struct {
		out    scripted
		detail string
	}{
		"stealth lookup fails":   {scripted{fw + " --getglobalstate": on, fw + " --getstealthmode": "ERR:exit status 1: denied"}, "denied"},
		"stealth lookup missing": {scripted{fw + " --getglobalstate": on}, "stealth"},
	} {
		got, detail := status(steps(t, hostDeps(tc.out))["firewall"])
		if got != NotVerified || !strings.Contains(detail, tc.detail) {
			t.Errorf("firewall %s: %s %q", name, got, detail)
		}
	}
	for name, tc := range map[string]struct {
		out  string
		want Status
	}{
		"on":                  {"FileVault is On.", OK},
		"off":                 {"FileVault is Off.", Fail},
		"unknown":             {"Encryption in progress: 40%", NotVerified},
		"empty":               {"", NotVerified},
		"contradictory lines": {"FileVault is Off.\nFileVault is On.", NotVerified},
		"trailing text":       {"FileVault is On? unknown", NotVerified},
		"two lines both on":   {"FileVault is On.\nFileVault is On.", NotVerified},
	} {
		if got, detail := status(steps(t, hostDeps(scripted{"fdesetup status": tc.out}))["filevault"]); got != tc.want {
			t.Errorf("filevault %s: %s %q, want %s", name, got, detail, tc.want)
		}
	}
	// the standard-user check: a failed launchctl print is not a missing service
	base := scripted{"launchctl managername": "Aqua", "container list --all": ""}
	for name, tc := range map[string]struct {
		print string
		want  Status
	}{
		"service listed":      {launchdPrint("com.apple.container.apiserver"), OK},
		"service not listed":  {launchdPrint("com.example.other"), Fail},
		"manager only":        {launchdPrint("com.apple.containermanagerd"), Fail},
		"prefixed label":      {launchdPrint("xcom.apple.container.apiserver"), Fail},
		"label only disabled": {"\tservices = {\n\t}\n\tdisabled services = {\n\t\t\"com.apple.container.apiserver\" => disabled\n\t}\n", Fail},
		"no services section": {"com.apple.container.apiserver", NotVerified},
		"launchctl fails":     {"ERR:exit status 113: Could not find domain", NotVerified},
	} {
		r := scripted{}
		for k, v := range base {
			r[k] = v
		}
		r["launchctl print gui/501"] = tc.print
		got, detail := status(steps(t, hostDeps(r))["standard-user-check"])
		if got != tc.want {
			t.Errorf("launchctl %s: %s %q, want %s", name, got, detail, tc.want)
		}
	}
}

// launchdPrint is a `launchctl print gui/<uid>` excerpt that lists the labels.
func launchdPrint(labels ...string) string {
	out := "gui/501 = {\n\tservices = {\n"
	for _, l := range labels {
		out += "\t\t     0      - \t" + l + "\n"
	}
	return out + "\t}\n}\n"
}

func TestContainerServiceListedAnchorsTheLabelAndTheSection(t *testing.T) {
	for _, tc := range []struct {
		name, out     string
		listed, found bool
	}{
		{"label as token", "services = {\n  0 0 com.apple.container.apiserver\n}\n", true, true},
		{"label after a slash", "services = {\n  x /tmp/com.apple.container.apiserver\n}\n", false, true},
		{"label after a colon", "services = {\n  x:com.apple.container.apiserver\n}\n", false, true},
		{"unclosed section does not absorb the disabled block", "services = {\n  0 0 other\ndisabled services = {\n  \"com.apple.container.apiserver\" => disabled\n}\n", false, true},
		{"never closed", "services = {\n  0 0 com.apple.container.apiserver\n", false, false},
	} {
		if l, f := containerServiceListed(tc.out); l != tc.listed || f != tc.found {
			t.Errorf("%s: listed %v found %v, want %v %v", tc.name, l, f, tc.listed, tc.found)
		}
	}
}

func TestAgentKeyFixRefusesBeforeAskingWhenTheFileExists(t *testing.T) {
	dir := t.TempDir()
	d := Deps{ConfigPath: filepath.Join(dir, "config.json"), Home: t.TempDir(), GOOS: "darwin", Runner: scripted{}, User: "workharbor", UID: 502}
	env := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(env, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &answers{secrets: []string{"sk-ant-api03-" + strings.Repeat("x", 30)}}
	if err := steps(t, d)["agent-key"].Fix.Do(context.Background(), p); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	if len(p.secrets) != 1 {
		t.Error("the key was asked for although the file exists")
	}
}

// Issue #378: the account is created with a password whr asks for itself,
// twice and without echo; dropping either flag brings the tool's own echoing
// prompt or the policy retry back out.
func TestUserStepAsksForTheNewPasswordItself(t *testing.T) {
	c := userStep(hostDeps(scripted{}), "whr setup")
	if len(c.Fix.Cmds) != 1 || c.Fix.Cmds[0].SecretPrompt == "" || !c.Fix.Cmds[0].SecretConfirm {
		t.Errorf("the -addUser command needs SecretPrompt and SecretConfirm: %+v", c.Fix.Cmds)
	}
}

func TestConfigDependentHostChecksWaitForTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"workspace-volume", "spotlight"} {
		check := func(content string, write bool) (Check, Status, string) {
			d := hostDeps(scripted{})
			d.ConfigPath = filepath.Join(dir, name+".json")
			if write {
				if err := os.WriteFile(d.ConfigPath, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := steps(t, d)[name]
			st, detail := status(c)
			return c, st, detail
		}
		c, st, detail := check("", false)
		if st != NotVerified || !strings.HasPrefix(detail, needsConfig) || strings.Contains(detail, "no such file") || strings.Contains(detail, "open ") {
			t.Errorf("%s, no configuration: %s %q", name, st, detail)
		}
		if got := c.FixCommand(detail); got != "whr setup --only config-first" {
			t.Errorf("%s: next action %q", name, got)
		}
		if _, st, detail = check("{", true); st != NotVerified || !strings.HasPrefix(detail, needsConfig) || !strings.Contains(detail, "cannot be read") {
			t.Errorf("%s, broken configuration: %s %q", name, st, detail)
		}
	}
	// a valid configuration runs the check: no roots means the internal disk
	d := writeRoots(t)
	d.Runner = scripted{}
	if st, detail := status(steps(t, d)["workspace-volume"]); st != OK {
		t.Errorf("valid configuration: %s %q", st, detail)
	}
}

func TestAConfigThatCannotBeReadIsNotReportedAsMissing(t *testing.T) {
	d := hostDeps(scripted{})
	d.ConfigPath = t.TempDir() // a directory: reading it fails, but not with ENOENT
	_, detail := status(steps(t, d)["workspace-volume"])
	if !strings.HasPrefix(detail, needsConfig) || !strings.Contains(detail, "cannot be read") || strings.Contains(detail, "not written yet") {
		t.Errorf("detail %q", detail)
	}
}

// The host phase writes the configuration itself, first (issue #394): the steps
// that read the workspace roots are not reachable without it, and the remedy is
// config-first. An account other than workharbor's never writes it.
func TestConfigFirstIsTheFirstHostStepAndTheReadersNeedIt(t *testing.T) {
	d := hostDeps(scripted{})
	d.ConfigPath = filepath.Join(t.TempDir(), "none.json")
	d.User, d.Account = "werner", "werner"
	hs := Steps(Checks(d), PhaseHost)
	if hs[0].Name != "config-first" || !hs[0].SetupOnly || hs[0].Reach != nil {
		t.Fatalf("first host step %+v", hs[0])
	}
	for _, name := range []string{"workspace-volume", "spotlight"} {
		u := steps(t, d)[name].Reach(context.Background())
		if u == nil || u.Step != "config-first" || !strings.Contains(u.Why, d.ConfigPath) {
			t.Errorf("%s: %+v", name, u)
		}
	}
	for _, r := range Run(context.Background(), hs[:1], nil) {
		if r.Check != "" {
			t.Error("doctor ran the setup-only config-first")
		}
	}
	if err := os.WriteFile(d.ConfigPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if u := steps(t, d)["workspace-volume"].Reach(context.Background()); u != nil {
		t.Errorf("with a file: %+v", u)
	}
	// an administrator who is not the whr account initializes its configuration
	// with sudo install (the file is never read or overwritten), after the account exists
	d.User, d.Account = "admin", "workharbor"
	d.ConfigPath = filepath.Join(t.TempDir(), "none.json")
	d.AccountHome = t.TempDir()
	d.SystemConfigFile = filepath.Join(t.TempDir(), "etc", "config.json")
	hs = Steps(Checks(d), PhaseHost)
	var cf Check
	for i, c := range hs {
		if c.Name == "config-first" {
			cf = c
			if i == 0 || hs[i-1].Name != "workharbor-user" {
				t.Errorf("config-first must follow the account step, got %v", hs[i-1].Name)
			}
		}
	}
	if u := cf.Reach(context.Background()); u != nil {
		t.Errorf("admin: %+v", u)
	}
	if st, _ := cf.Run(context.Background()); st != Fail {
		t.Errorf("no file: %s", st)
	}
	cmds := cf.Fix.Cmds
	if len(cmds) != 6 {
		t.Fatalf("commands %+v", cmds)
	}
	for _, c := range cmds {
		if !c.Sudo {
			t.Errorf("not a sudo command: %v", c.Argv)
		}
		// root never writes inside the account's home: those commands run as the account
		inHome := false
		for _, a := range c.Argv {
			inHome = inHome || strings.HasPrefix(a, d.AccountHome)
		}
		if inHome && (c.Argv[0] != "-u" || c.Argv[1] != "workharbor") {
			t.Errorf("a command touches the account's home as root: %v", c.Argv)
		}
	}
	if got := strings.Join(cmds[1].Argv, " "); got != "-u workharbor /bin/mkdir -p -m 0700 "+d.AccountHome+"/.config/whr" {
		t.Errorf("folders: %s", got)
	}
	if got := strings.Join(cmds[3].Argv, " "); !strings.HasPrefix(got, "-u workharbor /bin/cp -n ") || !strings.HasSuffix(got, d.AccountHome+"/.config/whr/config.json") {
		t.Errorf("file copy: %s", got)
	}
	if u := steps(t, d)["spotlight"].Reach(context.Background()); u == nil || u.Step != "config-first" {
		t.Errorf("spotlight as admin: %+v", u)
	}
	// a linked .config is refused before anything is installed
	{
		h := t.TempDir()
		dd := d
		dd.AccountHome = h
		if err := os.Symlink(t.TempDir(), filepath.Join(h, ".config")); err != nil {
			t.Fatal(err)
		}
		if err := dd.checkAccountPath(); err == nil || !strings.Contains(err.Error(), "link") {
			t.Errorf("symlinked .config: %v", err)
		}
	}
	// an existing file is present, never read or replaced
	if err := os.MkdirAll(filepath.Join(d.AccountHome, ".config", "whr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.AccountHome, ".config", "whr", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := cf.Run(context.Background()); st != OK {
		t.Errorf("present file: %s", st)
	}
	if _, err := cf.Fix.Build(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not overwritten") {
		t.Errorf("build over an existing file: %v", err)
	}
}

// Issue #397: the tool-store step looks for the guest helpers next to the
// binary before it offers `whr tools build`, and names the real remedy.
// toolStoreDeps is a user-phase Deps with a valid configuration that names
// a tool store.
func toolStoreDeps(t *testing.T) Deps {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"ws", "tools"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"account":"shared","repositories":[{"name":"own/repo"}],"listen":"127.0.0.1:8484","api_token_file":"` + key + `","github":{"app_id":1,"key_file":"` + key + `"},"roots":{"workspaces":["` + dir + `/ws"],"tool_store":"` + dir + `/tools"}}`
	d := hostDeps(nil)
	d.ConfigPath = filepath.Join(dir, "config.json")
	if err := os.WriteFile(d.ConfigPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestToolStoreIsNotReachableWithoutTheGuestHelpers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		have         []string
		wantMissing  string
		wantNotNamed string
	}{
		{"shim missing", []string{proxyName}, shimName, proxyName},
		{"proxy missing", []string{shimName}, proxyName, shimName},
		{"both missing", nil, shimName + " and " + proxyName, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := toolStoreDeps(t)
			d.Prefix = t.TempDir()
			d.Whr = filepath.Join(d.Prefix, "bin", "whr")
			lib := filepath.Join(d.Prefix, "libexec", "whr")
			if err := os.MkdirAll(lib, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, n := range tc.have {
				if err := os.WriteFile(filepath.Join(lib, n), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := steps(t, d)["tool-store"]
			u := c.Reach(context.Background())
			if u == nil {
				t.Fatal("reachable although a helper is missing")
			}
			if !strings.HasPrefix(u.Why, "only the whr binary is installed; the guest helpers "+tc.wantMissing+" are missing under "+lib) {
				t.Errorf("why %q", u.Why)
			}
			if tc.wantNotNamed != "" && strings.Contains(u.Why, tc.wantNotNamed) {
				t.Errorf("names a helper that is there: %q", u.Why)
			}
			// a managed prefix is never a make install target
			if len(u.Tools) != 2 || u.Tools[0] != "sudo ./install.sh <tag> "+d.Prefix || u.Tools[1] != "make install-release VERSION=<tag> PREFIX="+d.Prefix ||
				!strings.Contains(u.Note, "--allow-downgrade") || !strings.Contains(u.Note, "xattr -l") || strings.Contains(strings.Join(u.Tools, ";"), "make install PREFIX") {
				t.Errorf("managed: %q %q", u.Tools, u.Note)
			}
			// with both helpers there, the step is reachable and offers the command
			for _, n := range []string{shimName, proxyName} {
				if err := os.WriteFile(filepath.Join(lib, n), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if u := c.Reach(context.Background()); u != nil {
				t.Errorf("unreachable with both helpers: %+v", u)
			}
		})
	}
}

func TestToolStoreShowsTheRealCommandNotThePlaceholder(t *testing.T) {
	d := toolStoreDeps(t)
	d.Whr = "/p/bin/whr"
	d.Prefix = "/p"
	cmds := steps(t, d)["tool-store"].Fix.Preview()
	if len(cmds) != 1 || strings.Contains(strings.Join(cmds[0].Argv, " "), "<") || cmds[0].Argv[len(cmds[0].Argv)-1] != "/p/libexec/whr/"+shimName {
		t.Errorf("preview %v", cmds)
	}
}
