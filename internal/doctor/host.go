package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/credcheck"
	"github.com/wstein/workharbor/internal/launchd"
	"github.com/wstein/workharbor/internal/render"
	"github.com/wstein/workharbor/internal/sshca"
	"github.com/wstein/workharbor/internal/textsafe"
	"github.com/wstein/workharbor/internal/toolstore"
)

// The setup steps beyond what `whr doctor` always checked (design D46, the manual's
// host setup). Every command and every output format below is that of the macOS version
// you are running (unverified), as its own tools document it, and none has been run against a fresh machine:
// all of it is unverified until the wizard has set up the reference Mac mini
// (issue #73). The checks only read; the fixes are shown before they run.

// MediaAnalysisMaxCPU is the percentage of one core at which the media-analysis
// check fails. Werner's headless Mac measured mediaanalysisd at 222 % while it
// ran agent environments; half a core is well above idle and well below that.
const MediaAnalysisMaxCPU = 50.0

// DefaultPrefix is where an admin-owned install of whr lives (D24).
const DefaultPrefix = "/opt/whr"

// WhrUser is the standard user workharbor runs as.
const WhrUser = "workharbor"

// DefaultBrewfile is the manual's Brewfile (step 5): the whole host software.
const DefaultBrewfile = `brew "git"
brew "gh"
cask "tailscale-app"
`

const (
	sshdFile    = "/etc/ssh/sshd_config.d/100-whr.conf"
	sshdContent = "PasswordAuthentication no\nKbdInteractiveAuthentication no\n"
)

// withFixes adds the titles and fixes of the checks doctor always had, and the
// host and user steps, in the order the wizard runs them.
func withFixes(d Deps, shared []Check) []Check {
	out := append([]Check(nil), hostSteps(d)...)
	out = append(out, userSteps(d)...)
	// Docker-selected setup must not inspect or offer Apple runtime steps.
	if m, err := readConfigMap(d.ConfigPath); err == nil {
		if r, ok := m["runtime"].(map[string]any); ok && r["backend"] == "docker" {
			filtered := out[:0]
			for _, c := range out {
				switch c.Name {
				case "brew-pin", "container-start", "container-kernel", "standard-user-check":
					continue
				}
				filtered = append(filtered, c)
			}
			out = filtered
		}
	}
	return append(out, shared...)
}

func (d Deps) output(ctx context.Context, argv ...string) (string, error) {
	if d.Runner == nil || d.GOOS != "darwin" {
		return "", errNotHere
	}
	b, err := d.Runner.Output(ctx, argv...)
	return string(b), err
}

var errNotHere = errors.New("this runs only on a Mac")

// DSCLNotFound reports whether a failed `dscl . -read` says the record is not
// there: exit status 56 or the directory service's own record error,
// eDSRecordNotFound. Loose wording such as "does not exist" is not enough, and
// any other failure (permissions, a directory-service error, an unknown format)
// says nothing about the account.
func DSCLNotFound(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return commandExitIs(err, 56) || (dsRecordNotFound.MatchString(m) && !dsRecordNegated.MatchString(m))
}

// containerLabel matches a launchd service label of Apple Container
// (com.apple.container.<name>) as a whole token: com.apple.containermanagerd
// is another service.
var containerLabel = regexp.MustCompile(`(^|\s)com\.apple\.container\.[A-Za-z0-9-]+`)

// dsRecordNotFound matches the error name as a whole token. Free text that
// merely mentions it (a sentence about what it is not) cannot be told apart
// from the real message, so the real process status, exit 56, stays primary.
var dsRecordNotFound = regexp.MustCompile(`(^|[^A-Za-z0-9_])eDSRecordNotFound($|[^A-Za-z0-9_])`)

// dsRecordNegated spots the name after "not " ("is not eDSRecordNotFound"). Other
// free text that mentions the name still matches: exit status 56 is the
// reliable signal, and offboard relies on this same function.
var dsRecordNegated = regexp.MustCompile(`(?i)\b(not|isn'?t|never)\s+eDSRecordNotFound`)

// The whole error text must be defaults's own message for this key: after an
// optional exit status prefix, an optional header line that defaults prints
// first (timestamp, defaults[pid:tid]; observed on macOS 26.6.2), then the
// message. Any other extra text says something else. VERIFIED on the desk Mac
// (macOS 26): the absent key exits 1 and prints "Error: Could not find key
// '...' in domain 'kCFPreferencesAnyApplication'." with "Error: " and a final
// full stop. NOT VERIFIED: that an absent key means log-out is off on every
// macOS version, and that a delay of 0 means off on the real host.
const defaultsHeader = `(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+ defaults\[\d+:\d+\] *\n)?`

var (
	autologoutUnset  = regexp.MustCompile(`^(exit status \d+: )?` + defaultsHeader + `(Error: )?The domain/default pair of \(/Library/Preferences/\.GlobalPreferences, com\.apple\.autologout\.AutoLogOutDelay\) does not exist\.?$`)
	autologoutAbsent = regexp.MustCompile(`^(exit status \d+: )?` + defaultsHeader + `(Error: )?Could not find key 'com\.apple\.autologout\.AutoLogOutDelay' in domain 'kCFPreferencesAnyApplication'\.?$`)
	digitsOnly       = regexp.MustCompile(`^[0-9]+$`)
)

// commandExitIs prefers the real process status. The exact textual fallback
// supports Runners that expose only an error string, including scripted checks.
func commandExitIs(err error, code int) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == code
	}
	status := "exit status " + strconv.Itoa(code)
	message := err.Error()
	return message == status || strings.HasPrefix(message, status+":")
}

// ExpectedAnswer names the answer a command's non-zero exit is, for the
// commands whose checks treat that exit as a normal answer and not as an error
// ("" otherwise): dseditgroup -o checkmember exits 67 for "not a member" and
// dscl -read exits 56 for "no such record" (both read from the checks above);
// defaults exits 1 for an absent autologout key ("Could not find key" or "domain/default pair ... does not exist"), but only when stderr says so.
func ExpectedAnswer(argv []string, exit int, stderr string) string {
	if len(argv) < 2 {
		return ""
	}
	switch {
	case argv[0] == "dseditgroup" && argv[1] == "-o" && len(argv) > 2 && argv[2] == "checkmember" && exit == 67:
		return "not a member"
	case argv[0] == "dscl" && contains(argv, "-read") && exit == 56:
		return "no such record"
	case argv[0] == "defaults" && argv[1] == "read" && exit == 1 && contains(argv, "com.apple.autologout.AutoLogOutDelay") && (autologoutAbsent.MatchString(strings.TrimSpace(stderr)) || autologoutUnset.MatchString(strings.TrimSpace(stderr))):
		return "key not set"
	}
	return ""
}

func contains(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

// dsclFailure preserves stdout as well as the wrapped status and stderr.
func dsclFailure(out string, err error) error {
	if err == nil || strings.TrimSpace(out) == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
}

// LegacyUser is the account name before D49: an installation made then runs as
// it, and the host steps recognise it without ever renaming or removing it.
const LegacyUser = "whr"

// userStep is the workharbor-user check. When the account is missing and only
// the legacy account exists, it says so and points at `--user whr` instead of
// offering a second account; it never creates, renames or deletes one. Its
// Fix is the one the last Run left (the wizard shows it after the check).
func userStep(d Deps, setupCommand string) Check {
	// whr asks for the new password itself, without echo, and writes it to
	// sysadminctl's stdin (#378). UNVERIFIED: that "-password -" reads stdin, and
	// whether sysadminctl also needs an administrator password here.
	create := []Cmd{{
		Sudo: true, Argv: []string{"sysadminctl", "-addUser", d.account(), "-fullName", "WorkHarbor", "-password", "-"},
		SecretPrompt: "New password for " + d.account() + " (not shown)", SecretConfirm: true,
	}}
	createGuide := "whr asks you for the new user's password without echo and hands it to sysadminctl on its input, never in the command line. Then log in as " + d.account() + " on the Mac (or over Screen Sharing) and run, there:"
	fix := &Fix{Cmds: create, Guide: createGuide, Try: []string{setupCommand}}
	legacy := false // set by the last Run: only the Fail with a found whr
	return Check{
		Name: "workharbor-user", Phase: PhaseHost, Step: 2, Title: "the standard user " + d.account() + " (manual step 2)",
		Run: func(ctx context.Context) (Status, string) {
			fix.Cmds, fix.Guide = create, createGuide
			legacy = false
			if out, err := d.output(ctx, "dscl", ".", "-read", "/Users/"+d.account(), "UniqueID"); err != nil {
				err = dsclFailure(out, err)
				if st, msg, ok := notHere(err); ok {
					return st, msg
				}
				if DSCLNotFound(err) {
					st, msg, found := d.missingUser(ctx, fix)
					legacy = found
					return st, msg
				}
				return NotVerified, "dscl did not say whether " + d.account() + " exists: " + oneLine(err.Error())
			}
			admin, known := d.isAdmin(ctx)
			if !known {
				return NotVerified, d.account() + " exists; dseditgroup did not say whether it is an administrator"
			}
			if admin {
				return Warn, d.account() + " is an administrator: allowed, but a dedicated standard user is the recommended account (D49; see the account check and the drop-admin step)"
			}
			return OK, d.account() + " exists and is a standard user"
		},
		Fix: fix,
		UseUser: func(Status) string {
			if legacy {
				return LegacyUser
			}
			return ""
		},
	}
}

// missingUser says what a missing account means: a legacy whr account that
// exists is named, with the commands that use it (the Fix becomes guidance
// only); only a real not-found of whr leaves the creation offered, and any
// other answer is not verified.
func (d Deps) missingUser(ctx context.Context, fix *Fix) (st Status, msg string, legacyFound bool) {
	missing := "there is no user " + d.account()
	// only the default account has a legacy name (#344): an account asked for
	// by --user is created as asked
	if d.account() != WhrUser {
		return Fail, missing, false
	}
	out, err := d.output(ctx, "dscl", ".", "-read", "/Users/"+LegacyUser, "UniqueID")
	err = dsclFailure(out, err)
	switch {
	case err == nil:
		fix.Cmds = nil
		fix.Guide = "Nothing is created, renamed or deleted. To keep using the legacy account, run:"
		fix.Try = []string{"whr setup --user " + LegacyUser, "whr doctor --user " + LegacyUser}
		return Fail, missing + ", but the legacy account " + LegacyUser + " exists; the commands below use it instead of creating a second account", true
	case DSCLNotFound(err):
		return Fail, missing, false
	}
	fix.Cmds = nil
	fix.Try = []string{"whr setup --only workharbor-user"}
	fix.Guide = "Inspect the legacy account lookup failure, then retry. Account creation is unavailable until the lookup confirms that " + LegacyUser + " does not exist."
	return NotVerified, "there is no user " + d.account() + ", and dscl did not say whether the legacy account " + LegacyUser + " exists: " + oneLine(err.Error()), false
}

// serviceContainerSystem is the service the container-start step brings up and
// the steps after it need.
const serviceContainerSystem = "container-system"

// desktopSession names the session the container steps run in for the person
// reading the guide: this one when the target account is the current user (for
// example `--user <you>`), else the target account's own.
func (d Deps) desktopSession() string {
	account := d.Account
	if account == "" {
		account = WhrUser
	}
	if d.User != "" && d.User == account {
		return "this desktop session (Terminal on the Mac)"
	}
	return account + "'s own desktop session (Screen Sharing)"
}

// desktopOnly is what a check that needs Apple Container says outside the
// desktop session of the target account, where the container services do not
// answer (#156).
func (d Deps) desktopOnly() string {
	return "Apple Container answers only in " + d.desktopSession() + "; run this check there, or look at tasks with the list and show commands of whr over SSH"
}

// inDesktop asks launchd.CheckSession whether this is the Aqua session, before a
// check runs any `container` command. When it is not, the check reports
// not_verified and runs nothing.
func (d Deps) inDesktop(ctx context.Context) (Status, string, bool) {
	if d.Runner == nil || d.GOOS != "darwin" {
		return NotVerified, "not checked: " + errNotHere.Error(), false
	}
	m := launchd.Manager{R: runnerAdapter{d.Runner}, UID: d.UID, GOOS: d.GOOS}
	if err := m.CheckSession(ctx); err != nil {
		return NotVerified, d.desktopOnly(), false
	}
	return "", "", true
}

func notHere(err error) (Status, string, bool) {
	if errors.Is(err, errNotHere) {
		return NotVerified, "not checked: " + err.Error(), true
	}
	return "", "", false
}

func (d Deps) prefix() string {
	if d.Prefix != "" {
		return d.Prefix
	}
	return DefaultPrefix
}

// sshdPath is the sshd drop-in the ssh-keys-only check reads; SSHDFile
// overrides it for tests.
func (d Deps) sshdPath() string {
	if d.SSHDFile != "" {
		return d.SSHDFile
	}
	return sshdFile
}

// needsConfigFile is the Reach of a host step that reads the workspace roots
// from the configuration: without the file it cannot run, and the remedy is the
// config-first step (issue #394), not a fix of its own.
func (d Deps) needsConfigFile(context.Context) *Unreachable {
	if _, err := os.Stat(d.ConfigPath); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	u := &Unreachable{Why: "the workspace roots are read from " + d.ConfigPath + ", which is not written yet", Step: "config-first"}
	if d.adminRun() {
		u.Why = "the workspace roots are read from " + d.systemConfigFile() + ", which the config-first step writes with " + d.account() + "'s base configuration"
	}
	return u
}

// needsRoots is the Reach of the steps that only read the workspace roots
// (workspace-folders, workspace-volume, spotlight): needsConfigFile, except that
// an administrator run is reachable when the system config names the roots
// (issue #510). The system-config step keeps needsConfigFile, because it
// writes the file from the full user config.
func (d Deps) needsRoots(ctx context.Context) *Unreachable {
	if d.adminRun() {
		if _, err := os.Stat(d.ConfigPath); errors.Is(err, fs.ErrNotExist) {
			if _, st, _ := d.systemConfigRoots(err); st == "" {
				return nil
			}
		}
	}
	return d.needsConfigFile(ctx)
}

func (d Deps) configDir() string { return filepath.Dir(d.ConfigPath) }

// kv reads "name value" lines, as `pmset -g` prints them.
func kv(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

func hostSteps(d Deps) []Check {
	setupCommand := "whr setup"
	if d.account() != WhrUser {
		setupCommand += " --user '" + strings.ReplaceAll(d.account(), "'", "'\"'\"'") + "'"
	}
	brewfile := DefaultBrewfile
	configFirst := configBaseStep(d, "config-first", PhaseHost, filepath.Join(d.configDir(), "api.token"), filepath.Join(d.configDir(), "agent.env"))
	configFirst.SetupOnly = true
	if d.adminRun() {
		configFirst = d.accountConfigStep()
	}
	first, second := []Check{configFirst}, []Check{}
	if d.adminRun() {
		// the account must exist before its file can be owned by it
		first, second = second, first
	}
	rest := []Check{
		loginPictureStep(d),
		d.workspaceFoldersStep(),

		{
			Name: "autologout", Phase: PhaseHost, Step: 2, Title: "no automatic log-out after inactivity (manual step 2)",
			Run: func(ctx context.Context) (Status, string) {
				out, err := d.output(ctx, "defaults", "read", "/Library/Preferences/.GlobalPreferences", "com.apple.autologout.AutoLogOutDelay")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					// defaults exits non-zero and says "does not exist" when the key is
					// not set, which is the default: automatic log-out is off. Any
					// other failure (a timeout, a bad plist) says nothing about the
					// setting, so it is not a pass (unverified on macOS 26).
					if autologoutUnset.MatchString(strings.TrimSpace(err.Error())) {
						return OK, "automatic log-out is not set"
					}
					// the key is absent in the way macOS 26 words it (one observed
					// data point, unverified elsewhere): the system default applies,
					// which is off. Only this exact message; anything else stays
					// not verified.
					if autologoutAbsent.MatchString(strings.TrimSpace(err.Error())) {
						return OK, "key not set: the system default applies (default off)"
					}
					return NotVerified, "defaults did not answer, so the setting is not known: " + oneLine(err.Error())
				}
				delay := strings.TrimSpace(out)
				if delay == "" {
					return OK, "automatic log-out is off"
				}
				// only plain digits are a number of seconds: "+60" or "-5" is
				// not known, "00" is zero
				n, convErr := strconv.Atoi(delay)
				if !digitsOnly.MatchString(delay) || convErr != nil {
					return NotVerified, "defaults printed a value that is not a number of seconds, so the setting is not known: " + oneLine(delay)
				}
				if n == 0 {
					return OK, "automatic log-out is off"
				}
				return Fail, "the Mac logs out automatically after " + oneLine(delay) + " seconds of inactivity, which ends Apple Container's services and every agent"
			},
			Fix: &Fix{
				Guide: "Open System Settings → Privacy & Security → Advanced and turn off \"Log out automatically after inactivity\". whr does not change it for you.",
				Open:  "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension",
			},
		},

		{
			Name: "workspace-volume", Phase: PhaseHost, Step: 3, Title: "workspace volumes encrypted, with ownership honoured (manual step 3)",
			Reach: d.needsRoots,
			Run: func(ctx context.Context) (Status, string) {
				if d.GOOS != "darwin" || d.Runner == nil {
					return NotVerified, "not checked: " + errNotHere.Error()
				}
				vols, st, msg := d.workspaceVolumes(ctx)
				if st != "" {
					return st, msg
				}
				if len(vols) == 0 {
					return OK, "the workspace roots are on the internal disk, which FileVault covers"
				}
				var bad, unknown []string
				for _, v := range vols {
					out, err := d.output(ctx, "diskutil", "info", v)
					if err != nil {
						return NotVerified, "diskutil did not answer for " + v + ": " + oneLine(err.Error())
					}
					info := colonLines(out)
					fv, en := volumeAnswer(info["FileVault"], "Yes", "No"), volumeAnswer(info["Encrypted"], "Yes", "No")
					switch {
					case fv == answerYes || en == answerYes:
					case fv == answerNo || en == answerNo:
						bad = append(bad, v+" is not encrypted")
					default:
						unknown = append(unknown, v+" (FileVault: "+orNone(info["FileVault"])+")")
					}
					switch volumeAnswer(info["Owners"], "Enabled", "Disabled") {
					case answerNo:
						bad = append(bad, v+" ignores file ownership (Owners: "+orNone(info["Owners"])+")")
					case answerUnknown:
						unknown = append(unknown, v+" (Owners: "+orNone(info["Owners"])+")")
					}
				}
				if len(bad) > 0 {
					return Fail, strings.Join(bad, "; ")
				}
				if len(unknown) > 0 {
					return NotVerified, "diskutil's answer is not one this check knows for " + strings.Join(unknown, ", ") + " (its format is unverified on macOS 26)"
				}
				return OK, "every workspace volume is encrypted and honours ownership (diskutil's output format is unverified on macOS 26)"
			},
			Fix: &Fix{
				Cmds: []Cmd{{Sudo: true, Argv: []string{"diskutil", "enableOwnership", "<volume>"}}},
				Build: func(ctx context.Context, _ Prompter) ([]Cmd, error) {
					vols, _, msg := d.workspaceVolumes(ctx)
					if msg != "" && len(vols) == 0 {
						return nil, errors.New(msg)
					}
					var cmds []Cmd
					for _, v := range vols {
						out, err := d.output(ctx, "diskutil", "info", v)
						if err == nil && !strings.HasPrefix(colonLines(out)["Owners"], "Enabled") {
							cmds = append(cmds, Cmd{Sudo: true, Argv: []string{"diskutil", "enableOwnership", v}})
						}
					}
					return cmds, nil
				},
				Guide: "Ownership is turned on for you. Encryption is not: it needs a passphrase that only you may know, so erase the volume as APFS (Encrypted) in Disk Utility, or encrypt it yourself with the command below, and keep the passphrase in your password manager (manual step 3).",
				Try:   []string{"diskutil apfs encryptVolume /Volumes/<ssd> -user disk"},
				Open:  "/System/Applications/Utilities/Disk Utility.app",
			},
		},

		{
			Name: "power", Phase: PhaseHost, Step: 4, Title: "never sleep, restart after a power cut, no Power Nap (manual step 4)",
			Run: func(ctx context.Context) (Status, string) {
				out, err := d.output(ctx, "pmset", "-g")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return NotVerified, "pmset did not answer: " + oneLine(err.Error())
				}
				m := kv(out)
				var bad []string
				for _, want := range [][2]string{{"sleep", "0"}, {"disksleep", "0"}, {"autorestart", "1"}, {"womp", "1"}, {"powernap", "0"}} {
					if m[want[0]] != want[1] {
						bad = append(bad, want[0]+" is "+orNone(m[want[0]])+", want "+want[1])
					}
				}
				if len(bad) > 0 {
					return Fail, strings.Join(bad, "; ")
				}
				return OK, "the Mac does not sleep, restarts after a power cut and has Power Nap off"
			},
			Fix: &Fix{Cmds: []Cmd{{Sudo: true, Argv: []string{"pmset", "-a", "sleep", "0", "disksleep", "0", "autorestart", "1", "womp", "1", "powernap", "0"}}}},
		},

		{
			Name: "media-analysis", Phase: PhaseHost, Step: 4, Title: "Apple's media analysis is not eating the CPU (manual step 4, headless Mac)",
			Run: func(ctx context.Context) (Status, string) {
				out, err := d.output(ctx, "ps", "-axo", "user=,pcpu=,time=,comm=")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return NotVerified, "ps did not answer: " + oneLine(err.Error())
				}
				// The cache is the whr user's; only that account can read it.
				cache := "its cache is not read from here (run whr doctor as " + d.account() + ")"
				if d.User == d.account() {
					cache = "its cache is not known"
					if kb, err := d.output(ctx, "du", "-sk", filepath.Join(d.Home, "Library", "Caches", "com.apple.mediaanalysisd")); err == nil {
						if f := strings.Fields(kb); len(f) > 0 {
							if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
								cache = "its cache is " + strconv.FormatInt(n/1024, 10) + " MiB"
							}
						}
					}
				}
				// Every account's instance counts: the busiest one decides.
				found, bad := false, false
				var top float64
				var topUser, topTime string
				for _, l := range strings.Split(out, "\n") {
					f := strings.Fields(l)
					if len(f) < 4 || filepath.Base(strings.Join(f[3:], " ")) != "mediaanalysisd" {
						continue
					}
					cpu, err := strconv.ParseFloat(f[1], 64)
					if err != nil {
						bad = true
						continue
					}
					if !found || cpu > top {
						top, topUser, topTime = cpu, f[0], f[2]
					}
					found = true
				}
				if found {
					detail := fmt.Sprintf("mediaanalysisd of %s uses %.0f%% CPU, has used %s of CPU time, %s", topUser, top, topTime, cache)
					if top >= MediaAnalysisMaxCPU {
						return Fail, detail + fmt.Sprintf(" (limit %.0f%%)", MediaAnalysisMaxCPU)
					}
					if bad {
						return NotVerified, "ps's answer for another mediaanalysisd could not be read; " + detail
					}
					return OK, detail
				}
				if bad {
					return NotVerified, "ps's answer for mediaanalysisd could not be read"
				}
				return OK, "mediaanalysisd is not running; " + cache
			},
			Fix: &Fix{
				Guide: "whr never stops or deletes anything of Apple's. Turn off Apple Intelligence and Siri in System Settings, and keep Photos' analysis from running on this Mac; then keep the workspace roots out of Spotlight (step spotlight). Clearing mediaanalysisd's cache or killing it only helps for a while, the OS undoes it, so they are described in the manual and are not fixes.",
				Open:  "x-apple.systempreferences:com.apple.Siri-Settings.extension",
			},
		},

		{
			Name: "spotlight", Phase: PhaseHost, Step: 4, Title: "Spotlight does not index the workspaces (manual step 4, headless Mac)",
			Reach: d.needsRoots,
			Run: func(ctx context.Context) (Status, string) {
				if d.GOOS != "darwin" || d.Runner == nil {
					return NotVerified, "not checked: " + errNotHere.Error()
				}
				vols, st, msg := d.workspaceVolumes(ctx)
				if st != "" {
					return st, msg
				}
				if len(vols) == 0 {
					return NotVerified, "the workspace roots are on the internal disk: Spotlight's privacy list cannot be read, so add them there yourself"
				}
				var on, unknown []string
				for _, v := range vols {
					out, err := d.output(ctx, "mdutil", "-s", v)
					if err != nil {
						return NotVerified, "mdutil did not answer for " + v + ": " + oneLine(err.Error())
					}
					switch lo := strings.ToLower(out); {
					case strings.Contains(lo, "indexing enabled"):
						on = append(on, v)
					case strings.Contains(lo, "indexing disabled"):
					default:
						unknown = append(unknown, v)
					}
				}
				if len(on) > 0 {
					return Fail, "Spotlight indexes " + strings.Join(on, ", ")
				}
				if len(unknown) > 0 {
					return NotVerified, "mdutil's answer for " + strings.Join(unknown, ", ") + " is not one this check knows (its format is unverified on macOS 26)"
				}
				return OK, "Spotlight indexing is off on every workspace volume (mdutil's output format is unverified on macOS 26)"
			},
			Fix: &Fix{
				Cmds: []Cmd{{Sudo: true, Argv: []string{"mdutil", "-i", "off", "<volume>"}}},
				Build: func(ctx context.Context, _ Prompter) ([]Cmd, error) {
					vols, _, msg := d.workspaceVolumes(ctx)
					if msg != "" && len(vols) == 0 {
						return nil, errors.New(msg)
					}
					var cmds []Cmd
					for _, v := range vols {
						out, err := d.output(ctx, "mdutil", "-s", v)
						if err == nil && strings.Contains(strings.ToLower(out), "indexing enabled") {
							cmds = append(cmds, Cmd{Sudo: true, Argv: []string{"mdutil", "-i", "off", v}})
						}
					}
					return cmds, nil
				},
				Guide: "On a workspace volume of its own, the command below turns indexing off. A root on the internal disk cannot be turned off that way: add it in System Settings → Spotlight → Search Privacy.",
				Try:   []string{"sudo mdutil -i off <volume>"},
				Open:  "x-apple.systempreferences:com.apple.Siri-Settings.extension",
			},
		},

		{
			Name: "firewall", Phase: PhaseHost, Step: 8, Title: "the firewall on, in stealth mode (manual step 8)",
			Run: func(ctx context.Context) (Status, string) {
				const fw = "/usr/libexec/ApplicationFirewall/socketfilterfw"
				state, err := d.output(ctx, fw, "--getglobalstate")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return NotVerified, "socketfilterfw did not answer: " + oneLine(err.Error())
				}
				stealth, err := d.output(ctx, fw, "--getstealthmode")
				if err != nil {
					if firewallWord(state, false) == answerNo {
						return Fail, "the firewall is off"
					}
					return NotVerified, "socketfilterfw did not answer for stealth mode: " + oneLine(err.Error())
				}
				var bad, unknown []string
				switch firewallWord(state, false) {
				case answerNo:
					bad = append(bad, "the firewall is off")
				case answerUnknown:
					unknown = append(unknown, "the firewall state ("+oneLine(state)+")")
				}
				switch firewallWord(stealth, true) {
				case answerNo:
					bad = append(bad, "stealth mode is off")
				case answerUnknown:
					unknown = append(unknown, "the stealth mode state ("+oneLine(stealth)+")")
				}
				if len(bad) > 0 {
					return Fail, strings.Join(bad, "; ")
				}
				if len(unknown) > 0 {
					return NotVerified, "socketfilterfw's answer is not one this check knows for " + strings.Join(unknown, ", ") + " (its wording is unverified on macOS 26)"
				}
				return OK, "the firewall is on, in stealth mode"
			},
			Fix: &Fix{Cmds: []Cmd{
				{Sudo: true, Argv: []string{"/usr/libexec/ApplicationFirewall/socketfilterfw", "--setglobalstate", "on"}},
				{Sudo: true, Argv: []string{"/usr/libexec/ApplicationFirewall/socketfilterfw", "--setstealthmode", "on"}},
			}},
		},

		{
			Name: "ssh-keys-only", Phase: PhaseHost, Step: 8, Title: "SSH with keys only (manual step 8)",
			Run: func(context.Context) (Status, string) {
				if d.GOOS != "darwin" {
					return NotVerified, "not checked: " + errNotHere.Error()
				}
				b, err := os.ReadFile(d.sshdPath())
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return NotVerified, "could not read " + d.sshdPath() + ": " + oneLine(err.Error())
				}
				if err != nil {
					return Fail, d.sshdPath() + " is not there: password logins are not refused"
				}
				if !strings.Contains(string(b), "PasswordAuthentication no") || !strings.Contains(string(b), "KbdInteractiveAuthentication no") {
					return Fail, d.sshdPath() + " does not turn both password methods off"
				}
				return OK, "password logins are refused (" + d.sshdPath() + ")"
			},
			Fix: &Fix{
				Desc:  "write the two settings to a private temporary file, then install it as root's",
				Do:    func(context.Context, Prompter) error { return writeTemp(sshdTemp(), sshdContent) },
				Cmds:  []Cmd{{Sudo: true, Argv: []string{"install", "-m", "0644", "-o", "root", "-g", "wheel", sshdTemp(), sshdFile}}},
				Guide: "Remote Login itself is switched on in System Settings → General → Sharing → Remote Login, for your administrator account only; it cannot be checked here without a privileged command, so it is not verified.",
				Open:  "x-apple.systempreferences:com.apple.Sharing-Settings.extension",
			},
		},

		{
			Name: "filevault", Phase: PhaseHost, Step: 3, Title: "FileVault on (manual step 3)",
			Run: func(ctx context.Context) (Status, string) {
				out, err := d.output(ctx, "fdesetup", "status")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return NotVerified, "fdesetup did not answer: " + oneLine(err.Error())
				}
				switch fdesetupAnswer(out) {
				case answerYes:
					return OK, "FileVault is on"
				case answerNo:
					return Fail, "FileVault is off"
				}
				return NotVerified, "fdesetup's answer is not one this check knows: " + oneLine(out)
			},
			Fix: &Fix{
				Guide: "Enabling FileVault is interactive and prints a recovery key that must be yours alone, so whr does not run it. Run the command below yourself in this terminal (or use System Settings → Privacy & Security → FileVault) and keep the key safe.",
				Try:   []string{"sudo fdesetup enable"},
				Open:  "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension",
			},
		},

		{
			Name: "homebrew", Phase: PhaseHost, Step: 5, Title: "Homebrew (manual step 5)",
			Run: func(ctx context.Context) (Status, string) {
				if _, err := d.output(ctx, "/opt/homebrew/bin/brew", "--version"); err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return Fail, "Homebrew is not installed at /opt/homebrew"
				}
				return OK, "Homebrew is installed"
			},
			Fix: d.homebrewFix(),
		},

		{
			Name: "brew-packages", Phase: PhaseHost, Step: 5, Title: "the host packages of the Brewfile (manual step 5)",
			Run: func(ctx context.Context) (Status, string) {
				var missing []string
				for _, f := range []string{"git", "gh"} {
					out, err := d.output(ctx, "/opt/homebrew/bin/brew", "list", "--formula", "--versions", f)
					if err != nil || strings.TrimSpace(out) == "" {
						if st, msg, ok := notHere(err); ok {
							return st, msg
						}
						// brew list exits 1 for a formula that is not installed;
						// any other failure says nothing about the package
						if err != nil && !commandExitIs(err, 1) {
							return NotVerified, "brew did not say whether " + f + " is installed: " + oneLine(err.Error())
						}
						missing = append(missing, f)
					}
				}
				if len(missing) > 0 {
					return Fail, "not installed: " + strings.Join(missing, ", ")
				}
				return OK, "git and gh are installed"
			},
			Fix: &Fix{
				Desc: "write the Brewfile to a temporary file, then brew bundle it",
				Do: func(context.Context, Prompter) error {
					return writeTemp(brewfilePath(), brewfile)
				},
				Cmds: []Cmd{{Argv: []string{"/opt/homebrew/bin/brew", "bundle", "--file=" + brewfilePath()}}},
			},
		},

		{
			Name: "brew-pin", Phase: PhaseHost, Step: 6, Title: "container pinned, so brew upgrade leaves it (manual step 5)",
			Run: func(ctx context.Context) (Status, string) {
				out, err := d.output(ctx, "/opt/homebrew/bin/brew", "list", "--pinned")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return NotVerified, "brew did not answer: " + oneLine(err.Error())
				}
				for _, l := range strings.Fields(out) {
					if l == "container" {
						return OK, "container is pinned"
					}
				}
				return Fail, "container is not pinned"
			},
		},

		{
			Name: "prefix", Phase: PhaseHost, Step: 13, Title: prefixTitle(d),
			Run: func(context.Context) (Status, string) {
				fi, err := os.Stat(d.prefix())
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return NotVerified, "could not read " + d.prefix() + ": " + oneLine(err.Error())
				}
				outside := d.Whr != "" && !underDir(d.Whr, d.prefix())
				if err != nil {
					if outside { // alpha policy (#493): whr runs from elsewhere, so the prefix is not needed
						return Warn, "whr runs from " + d.Whr + ", outside " + d.prefix() + ", which does not exist: fine for the alpha, a weaker point than an administrator-owned prefix; revisit at beta (#493)"
					}
					return Fail, d.prefix() + " does not exist"
				}
				if !fi.IsDir() {
					return Fail, d.prefix() + " is not a directory"
				}
				// alpha policy (#493): who owns or writes the prefix and where the
				// binary lies are one warning, never a failure; revisit at beta
				var weak []string
				if fi.Mode().Perm()&0o022 != 0 {
					weak = append(weak, d.prefix()+" can be written by others than its owner")
				}
				for _, p := range []string{d.prefix(), filepath.Join(d.prefix(), "bin"), filepath.Join(d.prefix(), "bin", "whr")} {
					own, err := ownedBy(p, d.account())
					if errors.Is(err, fs.ErrNotExist) {
						continue // not installed yet
					}
					if err != nil {
						return NotVerified, "could not read the owner of " + p + ": " + oneLine(err.Error())
					}
					if own {
						weak = append(weak, p+" belongs to "+d.account()+", the account the supervisor runs as")
					} else if p != d.prefix() {
						if fi, err := os.Lstat(p); err == nil && fi.Mode().Perm()&0o022 != 0 {
							weak = append(weak, p+" can be written by others than its owner")
						}
					}
				}
				if outside {
					weak = append(weak, "whr runs from "+d.Whr+", outside "+d.prefix())
				}
				if len(weak) > 0 {
					return Warn, oneLine(strings.Join(weak, "; ")) + ": fine for the alpha, a weaker point than an administrator-owned prefix; revisit at beta (#493)"
				}
				return OK, d.prefix() + " exists and only the administrator writes it"
			},
			Fix: prefixFix(d),
		},
		d.systemConfigStep(), // the system config is /etc/whr/config.json, independent of the prefix

		{
			Name: "screen-sharing", Phase: PhaseHost, Step: 8, Title: "Screen Sharing for whr's desktop session (manual steps 2 and 8)", Optional: true,
			Run: func(context.Context) (Status, string) {
				return NotVerified, "macOS keeps this under privacy controls (TCC) that a command cannot read or set"
			},
			Fix: &Fix{
				Guide: "System Settings → General → Sharing → Screen Sharing: allow it only for your administrator and whr, if you want to reach whr's desktop session from afar.",
				Open:  "x-apple.systempreferences:com.apple.Sharing-Settings.extension",
			},
		},

		d.tailscaleStep(),
		d.tailscaleServeStep(),
	}
	out := append(first, userStep(d, setupCommand))
	out = append(out, second...)
	return append(out, rest...)
}

func orNone(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// setupDir is where files wait before a command reads them: a 0700 directory
// of the administrator in their own home, never the shared temporary directory,
// where another account could create it first and swap a file that root then
// installs.
func setupDir() string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		home = "/nonexistent" // writeTemp then fails; nothing is written elsewhere
	}
	return filepath.Join(home, "Library", "Caches", "whr-setup")
}

// sshdTemp is where the sshd settings wait before sudo install copies them.
func sshdTemp() string { return filepath.Join(setupDir(), "100-whr.conf") }

func brewfilePath() string { return filepath.Join(setupDir(), "Brewfile") }

// writeTemp writes a file the next command reads. The directory must be a real
// directory of this user with mode 0700, so no other account can enter it, and
// the file is created anew, never through a symbolic link. It holds no secret,
// so an earlier copy is replaced.
func writeTemp(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the path is the private setup directory checked above
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// privateDir refuses a directory that is a link, belongs to another user or
// can be entered by anyone else.
func privateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("%s does not belong to you", dir)
	case fi.Mode().Perm() != 0o700:
		return fmt.Errorf("%s has mode %o, want 700", dir, fi.Mode().Perm())
	}
	return nil
}

// prefixInstallArgv creates the prefix. When the account running setup is the
// configured account (an administrator whr, D49) it must not own the prefix, so
// root does; otherwise the running administrator does.
func (d Deps) prefixInstallArgv() []string {
	if d.User == d.account() {
		return []string{"install", "-d", "-o", "root", "-g", "wheel", "-m", "755", d.prefix()}
	}
	return []string{"install", "-d", "-o", d.User, "-g", "admin", "-m", "755", d.prefix()}
}

// ownedBy reports whether a path belongs to the named account, which must
// never own what runs the supervisor (D24, D49).
func ownedBy(path, name string) (bool, error) {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return false, nil // no such user yet: nothing it could own
	}
	if err != nil {
		return false, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && strconv.FormatUint(uint64(st.Uid), 10) == u.Uid, nil
}

// WriteSecret writes a secret file: mode 0600, exclusive create, so it never
// overwrites one, and the directory it lies in must be private. It never prints
// the value.
func WriteSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the secret's own path, in the user's configuration directory
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: not overwritten", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

var appKeyRE = regexp.MustCompile(`^github-app-([0-9]+)\.pem$`)

// appKey finds the private key `whr github app create` wrote and the App ID in
// its name. More than one is ambiguous.
func appKey(dir string) (id int64, path string, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, "", err
	}
	for _, e := range ents {
		if m := appKeyRE.FindStringSubmatch(e.Name()); m != nil {
			if path != "" {
				return 0, "", errors.New("more than one github-app-<id>.pem is in " + dir + ": keep the one in use")
			}
			id, _ = strconv.ParseInt(m[1], 10, 64)
			path = filepath.Join(dir, e.Name())
		}
	}
	if path == "" {
		return 0, "", fs.ErrNotExist
	}
	return id, path, nil
}

// configBaseStep is the base configuration as a step of the given phase. The
// user phase runs it as config-base; the host phase runs the same code first as
// config-first (issue #394), because workspace-volume and spotlight read the
// workspace roots from the file it writes.
func configBaseStep(d Deps, name string, phase Phase, tokenPath, envPath string) Check {
	return Check{
		Name: name, Phase: phase, Step: 1, Title: "the base configuration: listen, token, roots, repositories (manual step 13)",
		Run: func(context.Context) (Status, string) {
			m, err := readConfigMap(d.ConfigPath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return Fail, d.ConfigPath + " does not exist"
				}
				return Fail, oneLine(err.Error())
			}
			for _, k := range []string{"listen", "api_token_file", "roots", "repositories"} {
				if _, ok := m[k]; !ok {
					return Fail, d.ConfigPath + " lacks " + k
				}
			}
			return OK, d.ConfigPath + " has the base settings"
		},
		Fix: &Fix{Desc: "ask for the repository and the folders, choose the volume, then write " + d.ConfigPath + " (0600, atomic, an existing file is saved to .bak first)", Do: func(ctx context.Context, p Prompter) error {
			return writeConfigBase(ctx, d, p, tokenPath, envPath)
		}},
	}
}

// toolStoreCheck is the tool-store step's check: a profile, and every tool
// matching the hash recorded when it was installed.
func (d Deps) toolStoreCheck(context.Context) (Status, string) {
	c, err := config.Load(d.ConfigPath)
	if err != nil {
		return Fail, "needs a valid configuration (the config-file step)"
	}
	ents, err := os.ReadDir(filepath.Join(c.Roots.ToolStore, "profiles"))
	if err != nil || len(ents) == 0 {
		return Fail, "the tool store " + c.Roots.ToolStore + " has no profile"
	}
	// The tools are what every environment runs: check them against the
	// hashes recorded when they were installed.
	var severe []string
	unchecked := 0
	for _, p := range (&toolstore.Store{Root: c.Roots.ToolStore}).Verify() {
		if p.Severe {
			severe = append(severe, p.String())
		} else {
			unchecked++
		}
	}
	if len(severe) > 0 {
		return Fail, "the tool store does not verify: " + oneLine(strings.Join(severe, "; "))
	}
	if unchecked > 0 {
		return OK, fmt.Sprintf("the tool store has a profile; %d tool(s) have no full hash recorded and were checked only by the hash in their name", unchecked)
	}
	return OK, "the tool store has a profile and every tool matches its recorded hash"
}

// toolsBuild is the command that builds the tool store, with the shim of this
// installation: the same path the guest-helper check looks at.
func (d Deps) toolsBuild(store string) Cmd {
	return Cmd{Argv: []string{d.Whr, "tools", "build", "-store", store, "-shim", filepath.Join(d.libexec(), shimName)}}
}

const (
	shimName  = "whr-shim-linux-arm64"
	proxyName = "whr-proxy-linux-arm64"
)

// libexec is where `whr serve` looks for the guest helpers: relative to the
// running binary, <dir(dir(binary))>/libexec/whr (service.InstalledProxy), which
// is under the prefix for an installed whr and elsewhere for a copied one.
func (d Deps) libexec() string {
	if d.Whr == "" {
		return filepath.Join(d.prefix(), "libexec", "whr")
	}
	bin := d.Whr
	if r, err := filepath.EvalSymlinks(bin); err == nil {
		bin = r
	}
	return filepath.Join(filepath.Dir(filepath.Dir(bin)), "libexec", "whr")
}

// guestHelpersReach is the Reach of the tool-store step: the shim and the proxy
// are installed next to the binary by `make install` and the release installer,
// never by a copy of the whr binary alone. When one is missing and the step has
// work to do, the command that builds the store could only fail, so the step is
// not reachable (never FAIL, #394) and the way to install them is named. A store
// that already verifies, or a configuration that does not load, is left to the
// check itself.
func (d Deps) guestHelpersReach(check func(context.Context) (Status, string)) func(context.Context) *Unreachable {
	return func(ctx context.Context) *Unreachable {
		var missing []string
		for _, n := range []string{shimName, proxyName} {
			if _, err := os.Stat(filepath.Join(d.libexec(), n)); err != nil {
				missing = append(missing, n)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if _, err := config.Load(d.ConfigPath); err != nil {
			return nil
		}
		if st, _ := check(ctx); st == OK {
			return nil
		}
		u := &Unreachable{Why: "only the whr binary is installed; the guest helpers " + strings.Join(missing, " and ") + " are missing under " + d.libexec()}
		// a managed prefix (/opt/whr, /usr/local, Homebrew) is never a make install
		// target: the source preflight only warns about it, but a managed prefix
		// belongs to the administrator, so a release is installed
		u.Where = "as the administrator, in the unpacked release archive (the second command: in a clone)"
		u.Tools = []string{
			"sudo ./install.sh <tag> " + shellWord(d.prefix()),
			"make install-release VERSION=<tag> PREFIX=" + shellWord(d.prefix()),
		}
		u.Note = "Both install a release into <prefix>, with the guest helpers next to the binary; whr serve looks for them at " + d.libexec() + " (relative to the running whr), so a whr copied alone does not start: install the release there, or place the two guest binaries in that directory by hand. The first needs only the unpacked release archive (manual, Install from the release archive); the second runs from a clone and needs gh signed in as a writer of the repository for a draft. <prefix> must be the prefix the binary sits in, <prefix>/bin/whr. A whr copied there by hand has no libexec/whr/VERSION, so the installer refuses until --allow-downgrade (make: ALLOW_DOWNGRADE=1) is given, and then replaces it. A binary downloaded with a browser may carry the quarantine attribute: check it with `xattr -l <whr>` (unverified)."
		return u
	}
}

// shellWord quotes a word for a copied command line only when it needs it.
func shellWord(w string) string {
	if w != "" && !strings.ContainsAny(w, " \t\"'$`\\<>|&;*?()#~") {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

func userSteps(d Deps) []Check {
	dir := d.configDir()
	tokenPath := filepath.Join(dir, "api.token")
	envPath := filepath.Join(dir, "agent.env")
	caPath := filepath.Join(dir, "ssh-ca")
	return ordered([]Check{
		{Name: "runtime", Phase: PhaseUser, Step: 4, Title: "the existing Docker-compatible socket (manual step 6)", Run: d.runtimeCheck},
		{
			Name: "container-start", Phase: PhaseUser, Step: 4, Provides: serviceContainerSystem, Title: "the container system running (manual step 6)",
			Run: func(ctx context.Context) (Status, string) {
				if st, msg, ok := d.inDesktop(ctx); !ok {
					return st, msg
				}
				out, err := d.output(ctx, "container", "system", "status")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return Fail, "the container system is not running"
				}
				if strings.Contains(out, "running") {
					return OK, "the container system is running"
				}
				return Fail, "the container system is not running"
			},
		},
		{
			Name: "container-kernel", Phase: PhaseUser, Step: 4, Title: "the Linux kernel containers boot (manual step 6)",
			Needs: serviceContainerSystem,
			Run: func(ctx context.Context) (Status, string) {
				if st, msg, ok := d.inDesktop(ctx); !ok {
					return st, msg
				}
				out, err := d.output(ctx, "container", "system", "status")
				if err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return NotVerified, "whether the kernel is installed is not visible until the container system runs (step container-start)"
				}
				if !strings.Contains(out, "running") {
					return NotVerified, "whether the kernel is installed is not visible until the container system runs (step container-start)"
				}
				// Where `container system kernel set` puts a kernel is unverified
				// (issue #73): the start fix skips the kernel install, so a running
				// system says nothing about it.
				kernels := filepath.Join(d.Home, "Library", "Application Support", "com.apple.container", "kernels")
				ents, _ := os.ReadDir(kernels)
				if len(ents) == 0 {
					return Fail, "no Linux kernel is installed: containers cannot boot without one (read " + kernels + ", a location that is unverified until issue #73; if a kernel is installed elsewhere, tell issue #73)"
				}
				return OK, "a Linux kernel is installed (found in " + kernels + ", a location that is unverified until issue #73)"
			},
		},
		{
			Name: "standard-user-check", Phase: PhaseUser, Step: 4, Title: "containers answer for this standard user (manual steps 2 and 6)",
			Run: func(ctx context.Context) (Status, string) {
				if st, msg, ok := d.inDesktop(ctx); !ok {
					return st, msg
				}
				if _, err := d.output(ctx, "container", "list", "--all"); err != nil {
					if st, msg, ok := notHere(err); ok {
						return st, msg
					}
					return Fail, "container list failed: " + oneLine(err.Error()) + " (tell issue #38 before making this user an administrator)"
				}
				out, err := d.output(ctx, "launchctl", "print", "gui/"+strconv.Itoa(d.UID))
				if err != nil {
					return NotVerified, "launchctl print did not answer for gui/" + strconv.Itoa(d.UID) + ": " + oneLine(err.Error())
				}
				listed, found := containerServiceListed(out)
				if !found {
					return NotVerified, "launchctl print's output for gui/" + strconv.Itoa(d.UID) + " has no services list this check knows (its format is unverified on macOS 26)"
				}
				if !listed {
					return Fail, "the container services are not in this user's GUI launchd domain (gui/" + strconv.Itoa(d.UID) + ")"
				}
				return OK, "the container services answer in gui/" + strconv.Itoa(d.UID)
			},
			Fix: &Fix{
				Guide: "Run the command below in " + d.desktopSession() + ", not over SSH or sudo. If it fails with a permission or bootstrap error for this standard user, note the message in issue #38.",
				Try:   []string{"container system start --disable-kernel-install"},
			},
		},

		{
			Name: "config-dir", Phase: PhaseUser, Step: 4, Title: "the private configuration directory (manual step 12)",
			Run: func(context.Context) (Status, string) {
				fi, err := os.Stat(dir)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return NotVerified, "could not read " + dir + ": " + oneLine(err.Error())
				}
				if err != nil {
					return Fail, dir + " does not exist"
				}
				if fi.Mode().Perm()&0o077 != 0 {
					return Fail, dir + " can be entered by others"
				}
				return OK, dir + " is private"
			},
			Fix: &Fix{Desc: "make " + dir + " with mode 0700", Do: func(context.Context, Prompter) error {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				return os.Chmod(dir, 0o700) //nolint:gosec // a directory needs the execute bit
			}},
		},

		{
			Name: "api-token", Phase: PhaseUser, Step: 1, Title: "the API token (manual step 12)",
			Run: func(context.Context) (Status, string) {
				if _, err := config.ReadSecret(tokenPath); err != nil {
					return Fail, oneLine(err.Error())
				}
				return OK, tokenPath + " is a private file"
			},
			Fix: &Fix{Desc: "generate a random token into " + tokenPath + " (0600, never overwritten, never shown)", Do: func(context.Context, Prompter) error {
				b := make([]byte, 32)
				if _, err := rand.Read(b); err != nil {
					return err
				}
				return WriteSecret(tokenPath, []byte(base64.StdEncoding.EncodeToString(b)+"\n"))
			}},
		},

		{
			Name: "agent-key", Phase: PhaseUser, Step: 3, Title: "an agent API key (optional; a subscription needs none, D40)", Optional: true,
			Run: func(context.Context) (Status, string) {
				if _, err := os.Stat(envPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return NotVerified, "could not read " + envPath + ": " + oneLine(err.Error())
				} else if err != nil {
					return OK, "no API key: the agent signs in inside the environment (subscription)"
				}
				if _, err := (&config.Config{AgentAPIKeyEnvFile: envPath}).AgentAPIKey(); err != nil {
					return Fail, oneLine(err.Error())
				}
				return OK, envPath + " is a private file"
			},
			Fix: &Fix{Desc: agentKeyPrompt + "; written to " + envPath + " (0600, never overwritten, never shown)", Do: func(_ context.Context, p Prompter) error {
				if _, err := os.Lstat(envPath); err == nil {
					return errors.New(envPath + " already exists and is never overwritten; fix its mode or content, or remove it, then run again")
				}
				key, err := p.Secret(agentKeyPrompt)
				if err != nil {
					return err
				}
				key = strings.TrimSpace(key)
				if err := credcheck.Check("ANTHROPIC_API_KEY", key); errors.Is(err, credcheck.ErrEmptyValue) {
					return errors.New("no key entered; nothing was written")
				} else if err != nil {
					return errors.New("refused: " + err.Error() + "; nothing was written. " + credcheck.Advice)
				}
				if len(key) < 20 || strings.ContainsAny(key, " \t\r\n=") {
					return errors.New("that does not look like an API key; nothing was written")
				}
				return WriteSecret(envPath, []byte("ANTHROPIC_API_KEY="+key+"\n"))
			}},
		},

		{
			Name: "ssh-ca", Phase: PhaseUser, Step: 3, Title: "the console's SSH certificate authority (optional; for whr ssh, issue #32)", Optional: true,
			Run: func(context.Context) (Status, string) {
				if _, err := os.Stat(caPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return NotVerified, "could not read " + caPath + ": " + oneLine(err.Error())
				} else if err != nil {
					return OK, "no SSH authority: whr ssh is off"
				}
				if _, err := sshca.Load(caPath); err != nil {
					return Fail, oneLine(err.Error())
				}
				return OK, caPath + " is a private authority key; set console.ssh_ca_key_file to it in the configuration to turn whr ssh on"
			},
			Fix: &Fix{Desc: "generate an Ed25519 authority key into " + caPath + " (0600, never overwritten, never shown); add \"console\": {\"ssh_ca_key_file\": \"" + caPath + "\"} to the configuration to use it", Do: func(context.Context, Prompter) error {
				return sshca.Generate(caPath)
			}},
		},

		{
			Name: "github-app", Phase: PhaseUser, Step: 2, Title: "your own GitHub App (manual step 11)",
			Run: func(context.Context) (Status, string) {
				if id, path, err := appKey(dir); err == nil {
					return OK, fmt.Sprintf("App %d: its key is %s; installing it on your repositories and the ruleset check stay yours (whr doctor checks the installation)", id, path)
				}
				return Fail, "no GitHub App key in " + dir
			},
			Fix: &Fix{
				Cmds: []Cmd{{Argv: []string{d.Whr, "github", "app", "create", "--config", d.ConfigPath, "--public-url", "<name>"}}},
				Build: func(_ context.Context, p Prompter) ([]Cmd, error) {
					// the configured name is used by the command itself; only an
					// absent one is asked for, and shown normalised before it runs
					if m, err := readConfigMap(d.ConfigPath); err == nil {
						if s, _ := m["public_url"].(string); s != "" {
							return []Cmd{{Argv: []string{d.Whr, "github", "app", "create", "--config", d.ConfigPath}}}, nil
						}
					}
					u, err := p.Line("whr's public name behind the forwarder (manual step 7), for example whr.example.ts.net (https:// is added)")
					if err != nil {
						return nil, err
					}
					u, err = config.NormalizePublicURL(u)
					if err != nil {
						return nil, errors.New("that is not usable: " + oneLine(err.Error()) + "; nothing was run")
					}
					p.Show("the link will use " + textsafe.Escape(u))
					return []Cmd{{Argv: []string{d.Whr, "github", "app", "create", "--config", d.ConfigPath, "--public-url", u}}}, nil
				},
				Guide: "whr github app create prints a link: open it, press Continue to GitHub and confirm. Then install the App on your selected repositories and check that main's ruleset does not list it as a bypass actor (D15). The link uses public_url from the configuration (the public-url step), or the name you give here. If it times out, check the name and the forwarder; the command below gives a link for a browser on this Mac.",
				Try:   []string{"whr github app create --local"},
				Open:  "https://github.com/settings/apps",
			},
		},

		d.publicURLStep(),
		configBaseStep(d, "config-base", PhaseUser, tokenPath, envPath),
		{
			Name: "config-github", Phase: PhaseUser, Step: 2, Title: "the GitHub App in the configuration (manual step 13)",
			Run: func(context.Context) (Status, string) {
				if _, err := config.Load(d.ConfigPath); err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return Fail, d.ConfigPath + " does not exist"
					}
					return Fail, problems(err)
				}
				return OK, d.ConfigPath + " is valid"
			},
			Fix: &Fix{Desc: "add github.app_id and github.key_file to " + d.ConfigPath + " as a diff, after a y, written atomically, keeping every other key", Do: func(_ context.Context, p Prompter) error {
				return addGitHub(d, p)
			}},
		},
		{
			Name: "tool-store", Phase: PhaseUser, Step: 4, Title: "the tool store with Claude Code (manual step 13)",
			Reach: d.guestHelpersReach(d.toolStoreCheck),
			Run:   d.toolStoreCheck,
			Fix: &Fix{
				Show: func() []Cmd {
					c, err := config.Load(d.ConfigPath)
					if err != nil {
						return nil
					}
					return []Cmd{d.toolsBuild(c.Roots.ToolStore)}
				},
				Build: func(context.Context, Prompter) ([]Cmd, error) {
					c, err := config.Load(d.ConfigPath)
					if err != nil {
						return nil, errors.New("needs a valid configuration first (config-base, github-app, config-github)")
					}
					return []Cmd{d.toolsBuild(c.Roots.ToolStore)}, nil
				},
			},
		},

		{
			Name: "service-install", Phase: PhaseUser, Step: 4, Title: "whr serve as a LaunchAgent (manual step 13)",
			Run: func(ctx context.Context) (Status, string) {
				if d.Runner == nil || d.GOOS != "darwin" {
					return NotVerified, "not checked: " + errNotHere.Error()
				}
				m := launchd.Manager{R: runnerAdapter{d.Runner}, UID: d.UID, GOOS: d.GOOS}
				st, err := m.Status(ctx, launchd.Spec{Label: launchd.Label, Home: d.Home})
				if err != nil {
					return NotVerified, oneLine(err.Error())
				}
				if !st.Loaded {
					return Fail, "the job is not loaded"
				}
				return OK, "the job is loaded (" + st.State + ")"
			},
			Fix: &Fix{Cmds: []Cmd{{Argv: serviceInstallArgv(d)}}},
		},
		dropAdmin(d),
	}, userOrder)
}

// userOrder is the order of `whr setup`: nothing needs a later step, so there is
// no cycle (the base configuration comes before the App, which comes before the
// configuration that names it; the container system starts before the kernel is
// set, because `container system kernel set` needs the running system). A step
// that needs a service says so with Needs, and a test holds the order to it.
var userOrder = []string{
	"config-dir", "api-token", "agent-key", "ssh-ca", "runtime", "container-start", "container-kernel", "standard-user-check",
	"config-base", "public-url", "github-app", "config-github", "tool-store", "service-install", "drop-admin",
}

// ordered returns the checks in the given order. A name with no check is a bug
// in this package, caught by its tests.
func ordered(checks []Check, order []string) []Check {
	by := map[string]Check{}
	for _, c := range checks {
		by[c.Name] = c
	}
	out := make([]Check, 0, len(order))
	for _, n := range order {
		if c, ok := by[n]; ok {
			out = append(out, c)
		}
	}
	return out
}

// runnerAdapter lets the launchd manager ask launchctl through the same Runner.
type runnerAdapter struct{ r Runner }

func (a runnerAdapter) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return a.r.Output(ctx, append([]string{name}, args...)...)
}

func readConfigMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator's own configuration file
	if err != nil {
		return nil, err
	}
	// UseNumber keeps a number as its text, so a large integer or 1e2 is
	// written back exactly as it was read.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s is not JSON: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF { // Unmarshal refused trailing data too
		return nil, fmt.Errorf("%s is not JSON: unexpected data after the top-level value", path)
	}
	if m == nil { // the file holds only null, which Unmarshal accepts into a nil map
		return nil, fmt.Errorf("%s is not a JSON object", path)
	}
	return m, nil
}

// writeConfigBase asks for what `whr github app create` and the rest need before
// the App exists, and writes the file (0600, atomically) after the person has seen
// the result and said y; a file that exists is saved to .bak and keeps its keys. It is not the
// whole configuration: github comes later (config-github), so it is not validated
// as one.
func writeConfigBase(ctx context.Context, d Deps, p Prompter, tokenPath, envPath string) error {
	raw, mkdirs, err := composeConfigBase(ctx, d, p, tokenPath, envPath, false)
	if err != nil {
		return err
	}
	for _, dir := range append(mkdirs, filepath.Dir(d.ConfigPath)) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return errors.New("could not create " + textsafe.Escape(dir) + ": " + oneLine(err.Error()) + "; nothing was written")
		}
	}
	return replaceWithBackup(p, d.ConfigPath, raw)
}

// composeConfigBase asks the questions and returns the file's text after the
// person confirmed it, and the folders inside d.ConfigPath's account to make.
// fresh is the administrator's run for a separate account: the account's file
// is not read (it is not the administrator's to read), so every key is asked
// for and nothing is merged.
func composeConfigBase(ctx context.Context, d Deps, p Prompter, tokenPath, envPath string, fresh bool) ([]byte, []string, error) {
	// An existing file keeps every key it has: only what is missing is asked
	// for and added, and the summary is read back from the merged result.
	m := map[string]any{}
	if !fresh {
		var err error
		m, err = readConfigMap(d.ConfigPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			m = map[string]any{}
		case err != nil:
			return nil, nil, err
		}
	}
	roots, _ := m["roots"].(map[string]any)
	if m["roots"] != nil && roots == nil {
		return nil, nil, errors.New(textsafe.Escape(d.ConfigPath) + " has a roots entry that is not an object; nothing was written")
	}
	var mkdirs []string
	added := 0
	set := func(k string, v any) {
		if _, ok := m[k]; !ok {
			m[k], added = v, added+1
		}
	}
	if _, ok := m["repositories"]; !ok {
		repo, err := p.Line("Repository to work on (owner/name)")
		if err != nil {
			return nil, nil, err
		}
		repo = strings.TrimSpace(repo)
		if !ownerNameRE.MatchString(repo) {
			return nil, nil, errors.New("that is not owner/name; nothing was written")
		}
		set("repositories", []map[string]any{{"name": repo}})
	}
	if _, ok := roots["workspaces"]; !ok {
		ws, err := d.chooseWorkspaces(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		if roots == nil {
			roots = map[string]any{}
		}
		roots["workspaces"], added = []string{ws}, added+1
		// outside the home folder the host step workspace-folders makes it,
		// with sudo, owned by the whr account (issue #395)
		if rel, err := filepath.Rel(d.Home, ws); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			mkdirs = append(mkdirs, ws)
		}
	}
	if _, ok := roots["tool_store"]; !ok {
		store := filepath.Join(d.Home, "tools")
		if roots == nil {
			roots = map[string]any{}
		}
		roots["tool_store"], added = store, added+1
		mkdirs = append(mkdirs, store)
	}
	m["roots"] = roots
	if _, ok := m["account"]; !ok {
		acct, err := p.Line("Is " + d.account() + " dedicated to workharbor, or your own account that you also work in (D49)? [dedicated/shared, default dedicated]")
		if err != nil {
			return nil, nil, err
		}
		acct = strings.ToLower(strings.TrimSpace(acct))
		if acct == "" {
			acct = config.AccountDedicated
		}
		if acct != config.AccountDedicated && acct != config.AccountShared {
			return nil, nil, errors.New("that is not dedicated or shared; nothing was written")
		}
		set("account", acct)
	}
	set("listen", "127.0.0.1:8787")
	set("api_token_file", tokenPath)
	set("agent_allowed_tools", []string{"Read", "Edit", "Write", "Bash(git status:*)", "Bash(make check:*)"})
	if _, err := os.Stat(envPath); !fresh && err == nil {
		set("agent_api_key_env_file", envPath)
	}
	if added == 0 {
		return nil, nil, fmt.Errorf("%s already has these settings: not overwritten", d.ConfigPath)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	p.Show(configSummary(m))
	if d.Yes {
		p.Show("yes: write " + textsafe.Escape(d.ConfigPath))
	} else if ok, err := p.Confirm("Write " + textsafe.Escape(d.ConfigPath)); err != nil || !ok {
		if err == nil {
			err = errors.New("not written")
		}
		return nil, nil, err
	}
	return append(raw, '\n'), mkdirs, nil
}

// configSummary shows what the file will say, read from the merged map, so the
// confirmed text is what is written.
func configSummary(m map[string]any) string {
	repos := []string{}
	switch r := m["repositories"].(type) {
	case []map[string]any:
		for _, e := range r {
			repos = append(repos, fmt.Sprint(e["name"]))
		}
	case []any:
		for _, e := range r {
			if em, ok := e.(map[string]any); ok {
				repos = append(repos, fmt.Sprint(em["name"]))
			}
		}
	}
	roots, _ := m["roots"].(map[string]any)
	// a list shows its items joined, never through Trim, which would also eat
	// a bracket that belongs to the last path
	show := func(v any) string {
		switch l := v.(type) {
		case nil:
			return ""
		case []string:
			return textsafe.Escape(strings.Join(l, ", "))
		case []any:
			items := make([]string, len(l))
			for i, e := range l {
				items[i] = fmt.Sprint(e)
			}
			return textsafe.Escape(strings.Join(items, ", "))
		}
		return textsafe.Escape(fmt.Sprint(v))
	}
	return strings.Join([]string{
		"The configuration will have:",
		"  repository:  " + textsafe.Escape(strings.Join(repos, ", ")),
		"  workspaces:  " + show(roots["workspaces"]),
		"  tool store:  " + show(roots["tool_store"]),
		"  account:     " + show(m["account"]),
	}, "\n")
}

var ownerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// addGitHub adds the App to the configuration: the file is read as generic JSON,
// so a key this version does not know is kept, the change is shown as a diff,
// and the file is replaced atomically only after a y.
func addGitHub(d Deps, p Prompter) error {
	id, key, err := appKey(d.configDir())
	if err != nil {
		return errors.New("run the github-app step first: no github-app-<id>.pem in " + d.configDir())
	}
	m, err := readConfigMap(d.ConfigPath)
	if err != nil {
		return err
	}
	old, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	gh, _ := m["github"].(map[string]any)
	if gh == nil {
		gh = map[string]any{}
	}
	gh["app_id"], gh["key_file"] = id, key
	m["github"] = gh
	updated, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := validateKeepingUnknown(m); err != nil {
		return errors.New("the configuration would still not start, so it was not changed: " + problems(err))
	}
	p.Show(lineDiff(string(old), string(updated)))
	ok, err := p.Confirm("Write this change to " + d.ConfigPath)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("not written")
		}
		return err
	}
	return replaceWithBackup(p, d.ConfigPath, append(updated, '\n'))
}

// lineDiff shows the lines that were added and removed, one per line.
func lineDiff(before, after string) string {
	seen := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	b, a := seen(before), seen(after)
	var out []string
	for _, l := range strings.Split(before, "\n") {
		if a[l] == 0 {
			out = append(out, "- "+l)
		} else {
			a[l]--
		}
	}
	for _, l := range strings.Split(after, "\n") {
		if b[l] == 0 {
			out = append(out, "+ "+l)
		} else {
			b[l]--
		}
	}
	return strings.Join(out, "\n")
}

// replaceFile replaces a file atomically with mode 0600: the new content is
// written beside it and renamed over it.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".whr-config-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := os.Chmod(tmp.Name(), 0o600); err != nil { //nolint:gosec // the configuration is private
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// replaceWithBackup saves the file it replaces to <path>.bak (0600, so the
// copy is as private as the original; its content is never shown), prints that
// path, and then replaces the file. A file that does not exist yet has no backup.
func replaceWithBackup(p Prompter, path string, data []byte) error {
	old, err := os.ReadFile(filepath.Clean(path))
	switch {
	case err == nil:
		bak := path + ".bak"
		if err := replaceFile(bak, old); err != nil {
			return errors.New("could not save the backup, so nothing was changed: " + err.Error())
		}
		p.Show(strings.TrimRight(render.Backup(render.Style{}, filepath.Base(path), bak), "\n"))
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return replaceFile(path, data)
}

var unknownFieldRE = regexp.MustCompile(`unknown field "([^"]+)"`)

// validateKeepingUnknown checks a configuration the way `whr serve` would, but a
// top-level key this version does not know (a newer whr wrote it) does not stop
// the wizard: the key is kept in the file, and only the rest is validated.
func validateKeepingUnknown(m map[string]any) error {
	probe := map[string]any{}
	for k, v := range m {
		probe[k] = v
	}
	for range 20 {
		raw, err := json.Marshal(probe)
		if err != nil {
			return err
		}
		_, err = config.Parse(raw)
		if err == nil {
			return nil
		}
		match := unknownFieldRE.FindStringSubmatch(err.Error())
		if match == nil {
			return err
		}
		if _, top := probe[match[1]]; !top {
			return err
		}
		delete(probe, match[1])
	}
	return errors.New("too many unknown keys")
}

// internalMounts are the mount points of the Mac's own disk, which FileVault covers.
var internalMounts = map[string]bool{"/": true, "/System/Volumes/Data": true}

// dfMount reads the mount point out of `df -P`: the last line's sixth column on,
// since a mount point may hold spaces.
var dfMount = regexp.MustCompile(`(?m)^\S+\s+\d+\s+\d+\s+\d+\s+\d+%\s+(.+)$`)

// workspaceVolumes returns the mount points, other than the Mac's own disk, that
// hold a configured workspace root, without duplicates. Each root is resolved
// first, as the configuration does, so a link to an external disk counts as that
// disk. A status other than "" means the roots or their disks are not known (no
// configuration yet, a root that does not exist, a `df` that does not answer),
// which is not verified rather than a pass.
func (d Deps) workspaceVolumes(ctx context.Context) ([]string, Status, string) {
	roots, st, msg := d.workspaceRoots()
	if st != "" {
		return nil, st, msg
	}
	var vols []string
	seen := map[string]bool{}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, NotVerified, "the workspace root " + textsafe.Escape(root) + " cannot be resolved, so its disk is not known: " + textsafe.Escape(oneLine(err.Error()))
		}
		out, err := d.output(ctx, "df", "-P", resolved)
		if err != nil {
			return nil, NotVerified, "df did not say which disk " + textsafe.Escape(resolved) + " is on: " + textsafe.Escape(oneLine(err.Error()))
		}
		m := dfMount.FindStringSubmatch(out)
		if m == nil {
			return nil, NotVerified, "df's answer for " + textsafe.Escape(resolved) + " could not be read"
		}
		mount := strings.TrimSpace(m[1])
		if internalMounts[mount] || seen[mount] {
			continue
		}
		seen[mount] = true
		vols = append(vols, mount)
	}
	return vols, "", ""
}

type volumeState int

const (
	answerUnknown volumeState = iota
	answerYes
	answerNo
)

// volumeAnswer reads one diskutil value by its first whole word, ignoring case:
// the yes word is yes, the no word is no, and anything else ("None", "Nope",
// "Yesterday", empty, a new wording) is unknown. Text after the word, such as
// "Yes (Unlocked)", is allowed.
func volumeAnswer(v, yes, no string) volumeState {
	f := strings.Fields(v)
	if len(f) == 0 {
		return answerUnknown
	}
	switch {
	case strings.EqualFold(f[0], yes):
		return answerYes
	case strings.EqualFold(f[0], no):
		return answerNo
	}
	return answerUnknown
}

// firewallLine is the one whole line socketfilterfw prints, nothing around it:
// "Firewall is disabled. (State = 0)" (global state; observed on macOS 26.6.2)
// or "Firewall stealth mode is off" (observed). "enabled", "on" and State 1 or 2
// are inferred, not measured.
var firewallLine = regexp.MustCompile(`(?i)^Firewall( stealth mode)? is (on|off|enabled|disabled)\.?( \(State = ([0-9]+)\))?$`)

// firewallWord is a strict allow-list: the output must be exactly one such line.
// A "(State = N)" is cross-checked with the word and exists only for the global
// state: 0 is off, 1 is on, and 2 (block all incoming connections, justified only
// by being a non-zero state) is on only when the wording says enabled. Anything
// else, in particular any other sentence, a negation, a second line or a word and
// state that disagree, is unknown, which is never a pass.
func firewallWord(out string, stealth bool) volumeState {
	m := firewallLine.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil || (m[1] != "") != stealth {
		return answerUnknown
	}
	on := strings.EqualFold(m[2], "on") || strings.EqualFold(m[2], "enabled")
	if stealth {
		if m[3] != "" {
			return answerUnknown
		}
	} else if m[4] != "" {
		switch {
		case m[4] == "0" && !on:
		case m[4] == "1" && on:
		case m[4] == "2" && strings.EqualFold(m[2], "enabled"):
		default:
			return answerUnknown
		}
	}
	if on {
		return answerYes
	}
	return answerNo
}

// containerServiceListed reads `launchctl print gui/<uid>`: the label must be a
// token in the "services = {" section, not in "disabled services" or elsewhere.
// found is false when the output has no such section at all.
func containerServiceListed(out string) (listed, found bool) {
	in := false
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case !in && t == "services = {":
			in, found = true, true
		case in && (t == "}" || strings.HasSuffix(t, "services = {")):
			in = false // a "disabled services = {" block is never the loaded list
		case in && containerLabel.MatchString(l):
			listed = true
		}
	}
	if in { // the section never closed: what it holds is not trustworthy
		return false, false
	}
	return listed, found
}

// fdesetupAnswer accepts only one whole line: "FileVault is On." is yes,
// "FileVault is Off." is no; several lines or anything else is unknown.
func fdesetupAnswer(out string) volumeState {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		return answerUnknown
	}
	switch strings.TrimSuffix(strings.TrimSpace(lines[0]), ".") {
	case "FileVault is On":
		return answerYes
	case "FileVault is Off":
		return answerNo
	}
	return answerUnknown
}

// colonLines reads "Key: value" lines, as `diskutil info` prints them.
func colonLines(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func prefixInstallCommands(d Deps) []Cmd {
	return []Cmd{{Sudo: true, Argv: d.prefixInstallArgv()}}
}

func serviceInstallArgv(d Deps) []string {
	args := []string{d.Whr, "service", "install", "--config", d.ConfigPath}
	return args
}

func prefixFix(d Deps) *Fix {
	guide, try := prefixInstallGuide(d.prefix())
	return &Fix{Cmds: prefixInstallCommands(d), Guide: guide, Try: try}
}

func prefixTitle(d Deps) string {
	return "the admin-owned prefix " + d.prefix() + " (manual step 13, D24)"
}

func prefixInstallGuide(prefix string) (string, []string) {
	install := "sudo ./install.sh <tag>"
	if prefix != "/opt/whr" {
		install += " " + shellWord(prefix)
	}
	return "Then install whr there from the release archive (manual, Install from the release archive): download whr_<version>_darwin_arm64.tar.gz and checksums.txt, check the archive against the checksums, unpack it, and in the unpacked folder run, as the administrator:", []string{install}
}

// agentKeyPrompt says what the agent-key step asks for: an API key, never a
// subscription login (D40, issue #348).
const agentKeyPrompt = "Agent API key from the vendor's console (ANTHROPIC_API_KEY; not a setup-token or login token; not echoed)"

// underDir reports whether path lies in dir, after symbolic links.
func underDir(path, dir string) bool {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
