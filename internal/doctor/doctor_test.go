package doctor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/forge/github"
	"github.com/wstein/workharbor/internal/runtime"
)

type rig struct {
	dir, cfgPath string
	cfg          config.Config
	gh           *httptest.Server
	appPerms     map[string]string
	instStatus   int
	instPerms    map[string]string
	appStatus    int
	graphql      func() string // the body answered for POST /graphql
	mintStatus   int           // when set, the status of an installation token request that asks for the board
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"workspaces", "store", "secrets", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	secret := func(name, body string) string {
		p := filepath.Join(dir, "secrets", name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := &rig{dir: dir, cfgPath: filepath.Join(dir, "config.json")}
	r.cfg = config.Config{
		Listen:             "127.0.0.1:8787",
		Repositories:       []config.Repository{{Name: "wstein/workharbor"}},
		Roots:              config.Roots{Workspaces: []string{filepath.Join(dir, "workspaces")}, ToolStore: filepath.Join(dir, "store")},
		GitHub:             config.GitHub{AppID: 1, KeyFile: secret("app.pem", "-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n")},
		AgentAPIKeyEnvFile: secret("agent.env", "ANTHROPIC_API_KEY=x\n"),
		APITokenFile:       secret("api.token", "x\n"),
	}
	r.appPerms, r.instPerms, r.instStatus, r.appStatus = github.AppPermissions(), github.AppPermissions(), 200, 200
	r.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/app":
			w.WriteHeader(r.appStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "slug": "workharbor-x", "permissions": r.appPerms, "message": "Bad credentials"})
		case strings.HasSuffix(req.URL.Path, "/installation"):
			w.WriteHeader(r.instStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "permissions": r.instPerms, "message": "Not Found"})
		case strings.HasSuffix(req.URL.Path, "/access_tokens"):
			raw, _ := io.ReadAll(req.Body)
			if r.mintStatus != 0 && strings.Contains(string(raw), "organization_projects") {
				w.WriteHeader(r.mintStatus)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "The permissions requested are not granted to this installation."})
				return
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installationtoken0123456789", "expires_at": "2099-01-01T00:00:00Z"})
		case req.URL.Path == "/graphql" && r.graphql != nil:
			_, _ = io.WriteString(w, r.graphql())
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(r.gh.Close)
	return r
}

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func (r *rig) write(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) deps() Deps {
	return Deps{
		ConfigPath: r.cfgPath, Home: filepath.Join(r.dir, "home"), FS: runtime.OSFS{},
		LookPath: func(string) (string, error) { return "/usr/local/bin/container", nil },
		Probe:    func(context.Context) error { return nil },
		GitHub: func(c *config.Config) (*github.Client, error) {
			gc := github.Config{AppID: c.GitHub.AppID, Key: testKey, Repos: []string{"wstein/workharbor"}, BaseURL: r.gh.URL}
			if c.Board != nil {
				gc.Board = &github.BoardConfig{Owner: c.Board.Owner, Organization: c.Board.Organization, Number: c.Board.Number}
			}
			return github.New(gc)
		},
	}
}

func statuses(rs []Result) map[string]Status {
	m := map[string]Status{}
	for _, r := range rs {
		m[r.Check] = r.Status
	}
	return m
}

func run(d Deps, skip ...string) []Result {
	sk := map[string]bool{}
	for _, s := range skip {
		sk[s] = true
	}
	return Run(context.Background(), Shared(Checks(d)), sk)
}

func TestAHealthyHostPassesAndStillSaysWhatIsNotVerified(t *testing.T) {
	r := newRig(t)
	r.write(t)
	rs := run(r.deps())
	if Failed(rs) {
		t.Fatalf("failed: %+v", rs)
	}
	got := statuses(rs)
	for _, name := range []string{"config", "server", "forge-key", "forge-app", "forge-board", "agent-login", "mounts"} {
		if got[name] != OK {
			t.Errorf("%s = %s, want ok", name, got[name])
		}
	}
	// what nothing measured is never reported as passed; notifications is
	// not_verified here only because the rig configures no ntfy block
	for _, name := range []string{"forge-limits", "egress", "reboot", "capacity", "notifications"} {
		if got[name] != NotVerified {
			t.Errorf("%s = %s, want not_verified", name, got[name])
		}
	}
}

func TestASubscriptionLoginIsNotVerifiedAndPointsAtTheVendorTerms(t *testing.T) {
	r := newRig(t)
	r.cfg.AgentAPIKeyEnvFile = ""
	r.write(t)
	for _, res := range run(r.deps()) {
		if res.Check == "agent-login" {
			if res.Status != NotVerified || !strings.Contains(res.Detail, VendorTerms) || !strings.Contains(res.Detail, "D40") {
				t.Errorf("agent-login: %+v", res)
			}
			return
		}
	}
	t.Fatal("no agent-login check")
}

func TestASubscriptionTokenInTheKeyFileFails(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(r.cfg.AgentAPIKeyEnvFile, []byte("CLAUDE_CODE_OAUTH_TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.write(t)
	rs := run(r.deps())
	if !Failed(rs) {
		t.Fatalf("a subscription token in agent_api_key_env_file passed: %+v", rs)
	}
}

func TestAnInvalidConfigFailsAndTheChecksThatNeedItSayWhy(t *testing.T) {
	r := newRig(t)
	r.cfg.Listen = "0.0.0.0:8787"
	r.write(t)
	got := run(r.deps())
	st := statuses(got)
	if st["config"] != Fail || st["forge-key"] != Fail || st["agent-login"] != Fail {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got[0].Detail, "listen") {
		t.Errorf("detail %q does not name the problem", got[0].Detail)
	}
	// a missing file is a failure too, with the path in the message
	r.cfgPath = filepath.Join(r.dir, "missing.json")
	if rs := run(r.deps()); statuses(rs)["config"] != Fail {
		t.Fatalf("%+v", rs)
	}
}

func TestFailuresOfTheHost(t *testing.T) {
	r := newRig(t)
	r.write(t)
	d := r.deps()
	d.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	d.Probe = func(context.Context) error { return errors.New("401 token\nrejected") }
	st := statuses(run(d))
	if st["server"] != Fail {
		t.Fatalf("%+v", st)
	}
}

func TestSkippedChecksAreReportedAndNotRun(t *testing.T) {
	r := newRig(t)
	r.write(t)
	d := r.deps()
	d.Probe = func(context.Context) error { t.Error("a skipped check ran"); return nil }
	rs := run(d, "server")
	if statuses(rs)["server"] != Skipped || Failed(rs) {
		t.Fatalf("%+v", rs)
	}
}

func forgeAppDetail(rs []Result) string {
	for _, r := range rs {
		if r.Check == "forge-app" {
			return r.Detail
		}
	}
	return ""
}

func TestForgeAppChecksTheInstallationAndItsPermissions(t *testing.T) {
	r := newRig(t)
	r.write(t)

	r.instStatus = 404 // the App exists but is not installed on the repository
	rs := run(r.deps())
	if st := statuses(rs)["forge-app"]; st != Fail || !strings.Contains(forgeAppDetail(rs), "not installed on wstein/workharbor") || !strings.Contains(forgeAppDetail(rs), "https://github.com/apps/workharbor-x/installations/new") {
		t.Errorf("not installed: %s %q", st, forgeAppDetail(rs))
	}

	r.instStatus = 200
	r.instPerms = github.AppPermissions()
	r.instPerms["administration"] = "write"
	rs = run(r.deps())
	if st := statuses(rs)["forge-app"]; st != Fail || !strings.Contains(forgeAppDetail(rs), "administration, which is not wanted") {
		t.Errorf("too wide: %s %q", st, forgeAppDetail(rs))
	}

	r.instPerms = github.AppPermissions()
	r.appStatus = 401 // a revoked or foreign key
	if st := statuses(run(r.deps()))["forge-app"]; st != Fail {
		t.Errorf("a refused key: %s", st)
	}

	r.appStatus = 200
	r.gh.Close() // GitHub unreachable: not verified, never passed or failed
	if st := statuses(run(r.deps()))["forge-app"]; st != NotVerified {
		t.Errorf("unreachable: %s, want not_verified", st)
	}
}

const boardProject = `{"data":{"organization":{"projectV2":{"id":"P1","fields":{"nodes":[
{"id":"F1","name":"Status","dataType":"SINGLE_SELECT","options":[{"id":"a","name":"Needs you"},{"id":"b","name":"In progress"},{"id":"c","name":"Ready to push"},{"id":"d","name":"Done"}]}]}}}}}`

func (r *rig) withBoard(t *testing.T) {
	t.Helper()
	r.cfg.Board = &config.Board{Owner: "acme", Organization: true, Number: 3}
	r.write(t)
}

func TestForgeBoardNamesARefusalAndNeverCallsAReadAWrite(t *testing.T) {
	r := newRig(t)
	r.write(t)
	if rs := run(r.deps()); statuses(rs)["forge-board"] != OK {
		t.Errorf("no board: %+v", rs)
	}

	r.withBoard(t)
	r.graphql = func() string { return boardProject }
	rs := run(r.deps())
	// the project is found, but a write was never tried: not verified, not passed
	if st := statuses(rs)["forge-board"]; st != NotVerified || !strings.Contains(forgeBoardDetail(rs), "not tested") || !strings.Contains(forgeBoardDetail(rs), "three Status options") {
		t.Errorf("a readable board: %s %q", st, forgeBoardDetail(rs))
	}

	r.graphql = func() string {
		return `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`
	}
	rs = run(r.deps())
	if st := statuses(rs)["forge-board"]; st != Fail || !strings.Contains(forgeBoardDetail(rs), "board not writable") || !strings.Contains(forgeBoardDetail(rs), "organization") {
		t.Errorf("a refused board: %s %q", st, forgeBoardDetail(rs))
	}

	r.graphql = func() string {
		return strings.Replace(boardProject, `{"id":"d","name":"Done"}`, `{"id":"e","name":"Shipped"}`, 1)
	}
	rs = run(r.deps())
	if st := statuses(rs)["forge-board"]; st != Fail || !strings.Contains(forgeBoardDetail(rs), "Done") {
		t.Errorf("a missing option: %s %q", st, forgeBoardDetail(rs))
	}
}

func forgeBoardDetail(rs []Result) string {
	for _, r := range rs {
		if r.Check == "forge-board" {
			return r.Detail
		}
	}
	return ""
}

func TestNotificationsAreConfiguredOnlyWhenAnNtfyBlockValidates(t *testing.T) {
	r := newRig(t)
	r.write(t)
	if got := statuses(run(r.deps()))["notifications"]; got != NotVerified {
		t.Errorf("without a block: %s, want not_verified", got)
	}
	topic := filepath.Join(r.dir, "secrets", "ntfy.topic")
	if err := os.WriteFile(topic, []byte("a-long-random-topic-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg.PublicURL = "https://whr.example.com"
	r.cfg.Ntfy = &config.Ntfy{TopicFile: topic}
	r.write(t)
	if got := statuses(run(r.deps()))["notifications"]; got != OK {
		t.Errorf("with a valid block: %s, want ok", got)
	}
	r.cfg.Ntfy = &config.Ntfy{Server: "https://ntfy.example.com/x/y", TopicFile: topic}
	r.write(t)
	for _, res := range run(r.deps()) {
		if res.Check == "notifications" && !strings.Contains(res.Detail, "ntfy: https://ntfy.example.com,") {
			t.Errorf("detail %q, want only scheme and host", res.Detail)
		}
	}
}

func TestTheBotKeyCheck(t *testing.T) {
	r := newRig(t)
	statusOf := func() Result {
		t.Helper()
		r.write(t)
		for _, res := range run(r.deps()) {
			if res.Check == "bot-key" {
				return res
			}
		}
		t.Fatal("no bot-key check")
		return Result{}
	}
	if res := statusOf(); res.Status != Warn || !strings.Contains(res.Detail, "bot_signing_key_file") {
		t.Errorf("no key: %+v", res)
	}
	write := func(name string, pem []byte) string {
		p := filepath.Join(r.dir, "secrets", name)
		if err := os.WriteFile(p, pem, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.BotSigningKeyFile = write("bot", pem.EncodeToMemory(blk))
	if res := statusOf(); res.Status != OK {
		t.Errorf("an ed25519 key: %+v", res)
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rblk, err := ssh.MarshalPrivateKey(rsaKey, "")
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.BotSigningKeyFile = write("bot-rsa", pem.EncodeToMemory(rblk))
	if res := statusOf(); res.Status != Fail || !strings.Contains(res.Detail, "not ed25519") {
		t.Errorf("an RSA key: %+v", res)
	}
	r.cfg.BotSigningKeyFile = write("bot-junk", []byte("not a key\n"))
	if res := statusOf(); res.Status != Fail {
		t.Errorf("junk: %+v", res)
	}
	if err := os.Chmod(r.cfg.BotSigningKeyFile, 0o640); err != nil { //nolint:gosec // a test making a secret too open
		t.Fatal(err)
	}
	// the configuration check refuses it first, and says why
	r.write(t)
	for _, res := range run(r.deps()) {
		if res.Check == "config" && (res.Status != Fail || !strings.Contains(res.Detail, "bot_signing_key_file") || !strings.Contains(res.Detail, "0600")) {
			t.Errorf("a group-readable key: %+v", res)
		}
	}
}

// A subscription value in the key file fails the configuration check with the
// advice (never the value), and agent-login, which needs a valid configuration,
// fails with it.
func TestASubscriptionValueInTheKeyFileFailsConfigAndAgentLoginWithTheAdvice(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(r.cfg.AgentAPIKeyEnvFile, []byte("ANTHROPIC_API_KEY={\"access_token\":\"SYNTHETIC-SECRET\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.write(t)
	seen := map[string]Result{}
	for _, res := range run(r.deps()) {
		seen[res.Check] = res
	}
	if res := seen["config"]; res.Status != Fail || !strings.Contains(res.Detail, "Use an API key") || strings.Contains(res.Detail, "SECRET") {
		t.Errorf("config: %+v", res)
	}
	if res := seen["agent-login"]; res.Status != Fail || strings.Contains(res.Detail, "SECRET") {
		t.Errorf("agent-login: %+v", res)
	}
}
