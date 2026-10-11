package doctor

import (
	"context"
	"errors"
	"net"
	"regexp"
	"slices"
	"strconv"
	"sync"
)

// brewPath is the Homebrew the setup steps call (the brew-pin step does too).
const brewPath = "/opt/homebrew/bin/brew"

// tailscaleApp is the command line the cask app ships inside its bundle; the
// cask also links it onto the PATH.
const tailscaleApp = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// tailscaleBin finds the Tailscale command line: on the PATH, else inside the
// app. It says not found when no lookup is wired.
func (d Deps) tailscaleBin() (string, bool) {
	if d.LookPath == nil {
		return "", false
	}
	for _, name := range []string{"tailscale", tailscaleApp} {
		if p, err := d.LookPath(name); err == nil {
			return p, true
		}
	}
	return "", false
}

// listenPort is the port `whr serve` binds, from the configuration's listen
// (the default when it is unset, unreadable or not a port from 1 to 65535); the forwarder points at it.
func (d Deps) listenPort() string {
	listen := defaultListen
	if m, err := readConfigMap(d.ConfigPath); err == nil {
		if l, _ := m["listen"].(string); l != "" {
			listen = l
		}
	}
	// only a plain port number reaches an argv: never a value that could be a flag
	if _, port, err := net.SplitHostPort(listen); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n >= 1 && n <= 65535 && port == strconv.Itoa(n) {
			return port
		}
	}
	_, port, _ := net.SplitHostPort(defaultListen)
	return port
}

// tailscaleStep is the step `tailscale` (issue #408). Missing, it offers
// `brew install --cask tailscale-app`, run only after the usual confirmation,
// never as root and never with sudo. Present, signing in stays the human's.
func (d Deps) tailscaleStep() Check {
	_, found := d.tailscaleBin()
	c := Check{
		Name: "tailscale", Phase: PhaseHost, Step: 7, Title: "Tailscale installed and signed in (manual step 7)", Optional: true,
		Run: func(context.Context) (Status, string) {
			if d.Runner == nil || d.GOOS != "darwin" {
				return NotVerified, "not checked: " + errNotHere.Error()
			}
			if _, ok := d.tailscaleBin(); !ok {
				return NotVerified, "Tailscale is not installed: neither tailscale on the PATH nor " + tailscaleApp + " was found"
			}
			return NotVerified, "signing in is the human's; whr does not check a third party's state"
		},
	}
	if found {
		c.Fix = &Fix{
			Guide: "Open the Tailscale app and sign in. The next step forwards whr's name to its loopback port with HTTPS (manual step 7); or use another option of that step.",
			Open:  "https://login.tailscale.com",
		}
		return c
	}
	c.Fix = &Fix{
		Desc: "install the Tailscale app with Homebrew (a cask: it needs no sudo, and brew never runs as root)",
		Do: func(context.Context, Prompter) error {
			if d.UID == 0 {
				return errors.New("brew refuses to run as root, and whr does not try: run the host setup as your administrator account")
			}
			if _, err := d.lookPath(brewPath); err != nil {
				return errors.New("brew is not at " + brewPath + ": run the brew steps of the host setup first")
			}
			return nil
		},
		Cmds: []Cmd{{Argv: []string{brewPath, "install", "--cask", "tailscale-app"}}},
		Guide: "Open the Tailscale app and sign in with your own account; whr never sees the sign-in. " +
			"If macOS asks you to approve a system or network extension, approve it in System Settings (UNVERIFIED: whether and how it asks is not measured on macOS 26). " +
			"The next step then checks the forward and offers to create it.",
		Try:  []string{"open -a Tailscale"},
		Open: "https://login.tailscale.com",
	}
	return c
}

func (d Deps) lookPath(name string) (string, error) {
	if d.LookPath == nil {
		return "", errors.New("no lookup")
	}
	return d.LookPath(name)
}

// tailscaleServeStep is the step `tailscale-serve`: `tailscale serve status`
// (read-only) shows a forward to whr's loopback port, else it offers
// `tailscale serve --bg <port>`, the port from the configuration's listen. It
// never uses funnel and holds no key.
func (d Deps) tailscaleServeStep() Check {
	// The first call decides the command; the preview and the run share it, so
	// a configuration edit in between cannot run something other than what was shown.
	var once sync.Once
	var fixed []Cmd
	cmds := func() []Cmd {
		once.Do(func() {
			bin, ok := d.tailscaleBin()
			if !ok {
				bin = "tailscale"
			}
			fixed = []Cmd{{Argv: []string{bin, "serve", "--bg", d.listenPort()}}}
		})
		cloned := slices.Clone(fixed)
		for i := range cloned {
			cloned[i].Argv = slices.Clone(cloned[i].Argv)
		}
		return cloned
	}
	return Check{
		Name: "tailscale-serve", Phase: PhaseHost, Step: 7, Title: "Tailscale forwards whr's port with HTTPS (manual step 7)", Optional: true,
		Reach: func(ctx context.Context) *Unreachable {
			bin, ok := d.tailscaleBin()
			if !ok {
				return &Unreachable{Why: "Tailscale is not installed", Step: "tailscale"}
			}
			_, err := d.output(ctx, bin, "serve", "status")
			if err != nil {
				if st, msg, ok := notHere(err); ok && st == NotVerified {
					return &Unreachable{Why: msg, Step: "tailscale"}
				}
				return &Unreachable{Why: "tailscale did not answer (" + oneLine(err.Error()) + "): open the app and sign in first", Step: "tailscale"}
			}
			return nil
		},
		Run: func(ctx context.Context) (Status, string) {
			port := d.listenPort()
			bin, ok := d.tailscaleBin()
			if !ok {
				return NotVerified, "Tailscale is not installed"
			}
			status, err := d.output(ctx, bin, "serve", "status")
			if err != nil {
				return NotVerified, "the tailscale forward list did not answer: " + oneLine(err.Error())
			}
			if regexp.MustCompile(`(?:127\.0\.0\.1|localhost):` + port + `\b`).MatchString(status) {
				return OK, "the tailscale forward list shows a forward to port " + port + " (the status text's format is unverified)"
			}
			return NotVerified, "the tailscale forward list shows no forward to port " + port + " (the status text's format is unverified)"
		},
		Fix: &Fix{
			Desc:  "forward whr's loopback port to the tailnet with HTTPS (never funnel)",
			Show:  cmds,
			Build: func(context.Context, Prompter) ([]Cmd, error) { return cmds(), nil },
			Guide: "Tailscale may ask you to enable HTTPS certificates for the tailnet in its admin page; that is yours to decide.",
			Try:   []string{"tailscale serve status"},
		},
	}
}
