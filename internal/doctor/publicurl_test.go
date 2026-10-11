package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fwGlobal = "/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate"

func publicDeps(t *testing.T, cfg string, r scripted) Deps {
	t.Helper()
	d := hostDeps(r)
	d.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	if cfg != "" {
		if err := os.WriteFile(d.ConfigPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestThePublicURLCheckSaysWhatItVerifiesAndWhatItDoesNot(t *testing.T) {
	on := scripted{fwGlobal: "Firewall is enabled. (State = 1)"}
	cases := []struct {
		name, cfg string
		r         scripted
		want      Status
		sub       string
	}{
		{"no file", "", on, NotVerified, "needs the configuration"},
		{"no name", `{"listen":"127.0.0.1:8787"}`, on, NotVerified, "--local"},
		{"open listener", `{"listen":"0.0.0.0:8787","public_url":"https://w.example"}`, on, Fail, "not a loopback"},
		{"bare host", `{"public_url":"w.example"}`, on, Fail, "public_url"},
		{"http", `{"public_url":"http://w.example"}`, on, Fail, "http:// is refused"},
		{"good, firewall on", `{"public_url":"https://w.example"}`, on, OK, "does not filter loopback"},
		{"good, firewall unreadable", `{"public_url":"https://w.example"}`, scripted{}, OK, "could not be read"},
		{"good, firewall off", `{"public_url":"https://w.example"}`, scripted{fwGlobal: "Firewall is disabled. (State = 0)"}, OK, "firewall is off"},
	}
	for _, tc := range cases {
		got, detail := status(steps(t, publicDeps(t, tc.cfg, tc.r))["public-url"])
		if got != tc.want || !strings.Contains(detail, tc.sub) {
			t.Errorf("%s: %s %q, want %s containing %q", tc.name, got, detail, tc.want, tc.sub)
		}
		if tc.want == OK && !strings.Contains(detail, "tailscale-serve step") {
			t.Errorf("%s: no remedy named: %q", tc.name, detail)
		}
		if tc.want == OK && !strings.Contains(detail, "not checked") {
			t.Errorf("%s: an ok that does not say the forwarder is unchecked: %q", tc.name, detail)
		}
	}
}

func TestThePublicURLFixNormalisesShowsAndWritesAfterAY(t *testing.T) {
	d := publicDeps(t, `{"listen":"127.0.0.1:8787","other":1}`, scripted{})
	fix := steps(t, d)["public-url"].Fix
	ctx := context.Background()

	for name, a := range map[string]*answers{
		"unusable": {confirm: true, lines: []string{"https://w.example/path"}},
		"declined": {lines: []string{"W.Example"}},
	} {
		if err := fix.Do(ctx, a); err == nil {
			t.Errorf("%s: no error", name)
		}
		if b, _ := os.ReadFile(d.ConfigPath); strings.Contains(string(b), "public_url") {
			t.Errorf("%s: wrote %s", name, b)
		}
	}
	a := &answers{confirm: true, lines: []string{"W.Example/"}}
	if err := fix.Do(ctx, a); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(d.ConfigPath)
	if !strings.Contains(string(b), `"public_url": "https://w.example"`) || !strings.Contains(string(b), `"other": 1`) {
		t.Errorf("config = %s", b)
	}
	if !strings.Contains(strings.Join(a.shown, "\n"), "public_url will be https://w.example") {
		t.Errorf("the normalised value was not shown: %q", a.shown)
	}
	if got, _ := status(steps(t, d)["public-url"]); got != OK {
		t.Errorf("after the fix: %s", got)
	}

	missing := publicDeps(t, "", scripted{})
	if err := steps(t, missing)["public-url"].Fix.Do(ctx, &answers{confirm: true, lines: []string{"w.example"}}); err == nil || !strings.Contains(err.Error(), "config-base") {
		t.Errorf("without a file: %v", err)
	}
}

func TestTheGitHubAppStepUsesTheConfiguredNameOrAsksAndNormalises(t *testing.T) {
	ctx := context.Background()
	d := publicDeps(t, `{"public_url":"https://w.example"}`, scripted{})
	d.Whr = "/x/whr"
	cmds, err := steps(t, d)["github-app"].Fix.Build(ctx, &answers{})
	if err != nil || strings.Contains(strings.Join(cmds[0].Argv, " "), "--public-url") {
		t.Errorf("configured: %v %v", cmds, err)
	}

	d = publicDeps(t, `{}`, scripted{})
	d.Whr = "/x/whr"
	a := &answers{lines: []string{" W.Example "}}
	cmds, err = steps(t, d)["github-app"].Fix.Build(ctx, a)
	if err != nil || cmds[0].Argv[len(cmds[0].Argv)-1] != "https://w.example" {
		t.Errorf("asked: %v %v", cmds, err)
	}
	if !strings.Contains(strings.Join(a.shown, "\n"), "https://w.example") {
		t.Errorf("the normalised name was not shown: %q", a.shown)
	}
	if _, err := steps(t, d)["github-app"].Fix.Build(ctx, &answers{lines: []string{"http://w.example"}}); err == nil {
		t.Error("http accepted")
	}
}

func tailscaleDeps(t *testing.T, cfg string, r scripted, tools ...string) Deps {
	t.Helper()
	d := publicDeps(t, cfg, r)
	d.LookPath = func(name string) (string, error) {
		for _, tool := range tools {
			if name == tool {
				return name, nil
			}
		}
		return "", errors.New("not found")
	}
	return d
}

func TestTheTailscaleStepInstallsTheCaskOnlyByArgvAndNeverAsRoot(t *testing.T) {
	ctx := context.Background()
	d := tailscaleDeps(t, "", scripted{}, brewPath)
	c := steps(t, d)["tailscale"]
	if st, detail := status(c); st != NotVerified || !strings.Contains(detail, "not installed") {
		t.Fatalf("missing: %s %q", st, detail)
	}
	want := []string{brewPath, "install", "--cask", "tailscale-app"}
	if len(c.Fix.Cmds) != 1 || c.Fix.Cmds[0].Sudo || strings.Join(c.Fix.Cmds[0].Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("cmds %+v", c.Fix.Cmds)
	}
	if err := c.Fix.Do(ctx, &answers{}); err != nil {
		t.Errorf("an administrator with brew: %v", err)
	}
	if !strings.Contains(c.Fix.Guide, "UNVERIFIED") || len(c.Fix.Try) != 1 || strings.Contains(c.Fix.Guide, "open -a") {
		t.Errorf("guide %q try %q", c.Fix.Guide, c.Fix.Try)
	}

	root := d
	root.UID = 0
	if err := steps(t, root)["tailscale"].Fix.Do(ctx, &answers{}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("root: %v", err)
	}
	nobrew := tailscaleDeps(t, "", scripted{})
	if err := steps(t, nobrew)["tailscale"].Fix.Do(ctx, &answers{}); err == nil || !strings.Contains(err.Error(), "brew is not at") {
		t.Errorf("no brew: %v", err)
	}

	for _, tool := range []string{"tailscale", tailscaleApp} {
		have := steps(t, tailscaleDeps(t, "", scripted{}, tool))["tailscale"]
		if st, _ := status(have); st != NotVerified || have.Fix.Cmds != nil || have.Fix.Do != nil {
			t.Errorf("%s present: %s, fix %+v", tool, st, have.Fix)
		}
	}
}

func TestTheTailscaleServeStepUsesTheConfiguredPortAndStaysReadOnly(t *testing.T) {
	ctx := context.Background()
	const bin = "tailscale"
	cfg := `{"listen":"127.0.0.1:9191"}`
	cases := []struct {
		name  string
		r     scripted
		tools []string
		want  Status
		reach bool
	}{
		{"no tailscale", scripted{}, nil, "", true},
		{"not signed in", scripted{}, []string{bin}, "", true},
		{"not forwarded", scripted{"tailscale serve status": "No serve config"}, []string{bin}, NotVerified, false},
		{"other port", scripted{"tailscale serve status": "https://w.ts.net (tailnet only)\n|-- / proxy http://127.0.0.1:8787"}, []string{bin}, NotVerified, false},
		{"longer port", scripted{"tailscale serve status": "|-- / proxy http://127.0.0.1:91910"}, []string{bin}, NotVerified, false},
		{"forwarded", scripted{"tailscale serve status": "|-- / proxy http://127.0.0.1:9191"}, []string{bin}, OK, false},
	}
	for _, tc := range cases {
		c := steps(t, tailscaleDeps(t, cfg, tc.r, tc.tools...))["tailscale-serve"]
		if u := c.Reach(ctx); (u != nil) != tc.reach {
			t.Errorf("%s: reach %+v", tc.name, u)
			continue
		}
		if !tc.reach {
			if st, detail := status(c); st != tc.want {
				t.Errorf("%s: %s %q", tc.name, st, detail)
			}
		}
	}
	c := steps(t, tailscaleDeps(t, cfg, scripted{}, bin))["tailscale-serve"]
	cmds, err := c.Fix.Build(ctx, &answers{})
	if err != nil || len(cmds) != 1 || cmds[0].Sudo || strings.Join(cmds[0].Argv, " ") != "tailscale serve --bg 9191" {
		t.Errorf("build %+v %v", cmds, err)
	}
	if shown := c.Fix.Preview(); strings.Join(shown[0].Argv, " ") != "tailscale serve --bg 9191" {
		t.Errorf("preview %+v", shown)
	}
	def := steps(t, tailscaleDeps(t, "", scripted{}, bin))["tailscale-serve"]
	if got := def.Fix.Preview()[0].Argv[3]; got != "8787" {
		t.Errorf("default port %s", got)
	}
}

func TestTheTailscalePortIsOnlyEverAPlainPortNumber(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:--https=443", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:+80", "127.0.0.1:080", "nonsense"} {
		d := tailscaleDeps(t, `{"listen":"`+listen+`"}`, scripted{}, "tailscale")
		if got := d.listenPort(); got != "8787" {
			t.Errorf("%s gave %q, want the default", listen, got)
		}
	}
	if got := tailscaleDeps(t, `{"listen":"127.0.0.1:65535"}`, scripted{}).listenPort(); got != "65535" {
		t.Errorf("65535 gave %q", got)
	}
}

func TestTheTailscaleCommandRunIsTheOneThatWasShown(t *testing.T) {
	d := tailscaleDeps(t, `{"listen":"127.0.0.1:9191"}`, scripted{}, "tailscale")
	c := steps(t, d)["tailscale-serve"]
	shown := c.Fix.Preview()
	if err := os.WriteFile(d.ConfigPath, []byte(`{"listen":"127.0.0.1:9292"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmds, err := c.Fix.Build(context.Background(), &answers{})
	if err != nil || strings.Join(cmds[0].Argv, " ") != strings.Join(shown[0].Argv, " ") {
		t.Errorf("shown %+v, built %+v, %v", shown, cmds, err)
	}
}

func TestAnUnnormalisedPublicURLNamesTheFixAsTheWriter(t *testing.T) {
	c := steps(t, publicDeps(t, `{"public_url":"https://W.ts.net/"}`, scripted{}))["public-url"]
	if st, detail := status(c); st != Fail || !strings.Contains(detail, "the fix writes") {
		t.Errorf("%s %q", st, detail)
	}
}

func TestTheTailscaleCommandResultsDoNotShareCachedStorage(t *testing.T) {
	for _, source := range []string{"preview", "build"} {
		for _, mutation := range []string{"command", "argv"} {
			t.Run(source+"/"+mutation, func(t *testing.T) {
				c := steps(t, tailscaleDeps(t, `{"listen":"127.0.0.1:9191"}`, scripted{}, "tailscale"))["tailscale-serve"]
				var cmds []Cmd
				if source == "preview" {
					cmds = c.Fix.Preview()
				} else {
					var err error
					cmds, err = c.Fix.Build(context.Background(), &answers{})
					if err != nil {
						t.Fatal(err)
					}
				}
				if mutation == "command" {
					cmds[0] = Cmd{Argv: []string{"changed"}, Sudo: true}
				} else {
					cmds[0].Argv[3] = "9292"
				}
				built, err := c.Fix.Build(context.Background(), &answers{})
				if err != nil {
					t.Fatal(err)
				}
				for _, got := range [][]Cmd{c.Fix.Preview(), built} {
					if len(got) != 1 || got[0].Sudo || strings.Join(got[0].Argv, " ") != "tailscale serve --bg 9191" {
						t.Errorf("cached command changed: %+v", got)
					}
				}
			})
		}
	}
}
