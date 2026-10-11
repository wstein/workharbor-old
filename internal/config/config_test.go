package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/policy"
)

type rig struct {
	dir string
	cfg Config
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{dir: dir}
	for _, d := range []string{"workspaces", "store", "secrets"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	secret := func(name string) string {
		p := filepath.Join(dir, "secrets", name)
		if err := os.WriteFile(p, []byte("value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r.cfg = Config{
		Listen:             "127.0.0.1:8787",
		Repositories:       []Repository{{Name: "wstein/workharbor", CloneDepth: 0}},
		Roots:              Roots{Workspaces: []string{filepath.Join(dir, "workspaces")}, ToolStore: filepath.Join(dir, "store")},
		GitHub:             GitHub{AppID: 12345, KeyFile: secret("app.pem")},
		AgentAPIKeyEnvFile: secret("agent.env"), APITokenFile: secret("api.token"),
	}
	if err := os.WriteFile(r.cfg.AgentAPIKeyEnvFile, []byte("ANTHROPIC_API_KEY=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) parse(t *testing.T) (*Config, error) {
	t.Helper()
	raw, err := json.Marshal(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(raw)
}

func problems(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return strings.Join(ce.Problems, "\n")
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestAGoodConfigLoads(t *testing.T) {
	r := newRig(t)
	raw, _ := json.MarshalIndent(r.cfg, "", "  ")
	path := filepath.Join(r.dir, "whr.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8787" || len(c.Repositories) != 1 || c.GitHub.AppID != 12345 {
		t.Errorf("config = %+v", c)
	}
	if _, err := Load(filepath.Join(r.dir, "missing.json")); err == nil || errors.As(err, new(*Error)) {
		t.Errorf("a missing file = %v, want a plain read error", err)
	}
}

// development_prefix was retired (issue #493): an older config.json that still
// has it loads, and the value is ignored, whatever it is.
func TestARetiredDevelopmentPrefixKeyIsIgnored(t *testing.T) {
	r := newRig(t)
	raw, _ := json.Marshal(r.cfg)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["development_prefix"] = "/not/absolute/../and/unchecked"
	raw, _ = json.Marshal(m)
	path := filepath.Join(r.dir, "whr.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("an old development_prefix key broke loading: %v", err)
	}
}

// Every problem is reported at once, with the key it is about.
func TestEveryProblemIsReportedWithItsKey(t *testing.T) {
	r := newRig(t)
	r.cfg.Listen = "0.0.0.0:8787"
	r.cfg.Repositories = []Repository{{Name: "no-slash"}, {Name: "Wstein/Workharbor"}, {Name: "wstein/workharbor"}, {Name: "a/b", CloneDepth: -1}}
	r.cfg.Roots.Workspaces = []string{"relative/ws"}
	r.cfg.Roots.ToolStore = filepath.Join(r.dir, "nope")
	r.cfg.GitHub.AppID = 0
	r.cfg.APITokenFile = ""
	_, err := r.parse(t)
	got := problems(err)
	for _, want := range []string{
		"listen: \"0.0.0.0\" is not an explicit unicast IP address",
		"repositories[0].name: \"no-slash\"",
		"repositories[2].name: \"wstein/workharbor\" is the same repository as repositories[1]",
		"repositories[3].clone_depth: -1",
		"roots.workspaces[0]: \"relative/ws\" must be an absolute",
		"roots.tool_store:",
		"github.app_id",
		"api_token_file: a file path is needed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q:\n%s", want, got)
		}
	}
}

func TestWebListenAddresses(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8787": true, "[::1]:8787": true, "127.0.0.2:80": true,
		"192.168.1.5:8787": true, "10.44.0.1:8787": true, "[fd42::1]:8787": true,
		"0.0.0.0:8787": false, "[::]:8787": false, "[::ffff:0.0.0.0]:8787": false,
		"localhost:8787": false, "224.0.0.1:8787": false, "255.255.255.255:8787": false,
		"169.254.1.1:8787": false, "[fe80::1]:8787": false, "[fe80::1%en0]:8787": false,
		"8787": false, "": false, "127.0.0.1:0": false, "127.0.0.1:70000": false, "127.0.0.1:x": false,
	} {
		r := newRig(t)
		r.cfg.Listen = addr
		_, err := r.parse(t)
		if ok != (err == nil) {
			t.Errorf("listen %q: accepted=%v, want %v (%s)", addr, err == nil, ok, problems(err))
		}
	}
}

func TestSecretsAreFilesWithModeZeroSixHundred(t *testing.T) {
	r := newRig(t)
	for name, mod := range map[string]func(){
		"group readable": func() { _ = os.Chmod(r.cfg.APITokenFile, 0o640) }, //nolint:gosec // a test making a secret too open,
		"world readable": func() { _ = os.Chmod(r.cfg.APITokenFile, 0o644) }, //nolint:gosec // a test making a secret too open,
		"too open":       func() { _ = os.Chmod(r.cfg.APITokenFile, 0o700) }, //nolint:gosec // a test making a secret too open,
		"empty": func() {
			_ = os.Chmod(r.cfg.APITokenFile, 0o600)
			_ = os.WriteFile(r.cfg.APITokenFile, nil, 0o600)
		},
		"a symlink": func() {
			link := filepath.Join(r.dir, "secrets", "link")
			_ = os.Symlink(r.cfg.APITokenFile, link)
			r.cfg.APITokenFile = link
		},
		"a directory": func() { r.cfg.APITokenFile = filepath.Join(r.dir, "secrets") },
		"missing":     func() { r.cfg.APITokenFile = filepath.Join(r.dir, "secrets", "nope") },
	} {
		r2 := newRig(t)
		r = r2
		mod()
		if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "api_token_file") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	// A value written in place of a path is refused too: it is not an absolute path.
	r = newRig(t)
	r.cfg.GitHub.KeyFile = "-----BEGIN PRIVATE KEY-----"
	if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "github.key_file") {
		t.Errorf("a key pasted into the config = %v", err)
	}
	// The error never prints a file's content.
	if strings.Contains(problems(err2(t)), "value") {
		t.Error("an error leaked a secret's content")
	}
}

func err2(t *testing.T) error {
	r := newRig(t)
	_ = os.Chmod(r.cfg.APITokenFile, 0o644) //nolint:gosec // a test making a secret too open
	_, err := r.parse(t)
	return err
}

func TestTheRootsAreSeparateAndSecretsAreOutOfTheWorkspace(t *testing.T) {
	r := newRig(t)
	r.cfg.Roots.ToolStore = filepath.Join(r.dir, "workspaces") // the tool store is where an agent writes
	if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "overlap") {
		t.Errorf("the tool store in the workspace root = %v", err)
	}
	r = newRig(t)
	nested := filepath.Join(r.dir, "workspaces", "store")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	r.cfg.Roots.ToolStore = nested
	if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "overlap") {
		t.Errorf("the tool store inside the workspace root = %v", err)
	}
	r = newRig(t)
	inside := filepath.Join(r.dir, "workspaces", "api.token")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg.APITokenFile = inside
	if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "inside a workspace root") {
		t.Errorf("a secret in the workspace root = %v", err)
	}
	// A link that leads into the workspace root is judged by where it leads.
	r = newRig(t)
	link := filepath.Join(r.dir, "secrets", "ws")
	if err := os.Symlink(filepath.Join(r.dir, "workspaces"), link); err != nil {
		t.Skip(err)
	}
	r.cfg.Roots.ToolStore = link
	if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "overlap") {
		t.Errorf("a link to the workspace root = %v", err)
	}
}

func TestTheFileIsDecodedStrictly(t *testing.T) {
	r := newRig(t)
	raw, _ := json.Marshal(r.cfg)
	for name, body := range map[string]string{
		"an unknown key":        strings.Replace(string(raw), `"listen"`, `"listen_addr":"x","listen"`, 1),
		"a token value":         strings.Replace(string(raw), `"api_token_file"`, `"api_token":"ghp_abc","api_token_file"`, 1),
		"two values":            string(raw) + string(raw),
		"not json":              "listen = 127.0.0.1",
		"a string for a number": strings.Replace(string(raw), `"app_id":12345`, `"app_id":"12345"`, 1),
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAgentPermissionMode(t *testing.T) {
	r := newRig(t)
	for _, ok := range []string{"", "dontAsk", "manual"} {
		r.cfg.AgentPermissionMode, r.cfg.AgentAllowedTools = ok, nil
		if _, err := r.parse(t); err != nil {
			t.Errorf("mode %q: %s", ok, problems(err))
		}
	}
	r.cfg.AgentPermissionMode = "bypassPermissions"
	if _, err := r.parse(t); !strings.Contains(problems(err), "agent_permission_mode") {
		t.Errorf("an unknown mode: %s", problems(err))
	}
	r.cfg.AgentPermissionMode, r.cfg.AgentAllowedTools = "manual", []string{"Read"}
	if _, err := r.parse(t); !strings.Contains(problems(err), "agent_allowed_tools") {
		t.Errorf("an allowlist in manual mode: %s", problems(err))
	}
}

func TestBoardIsOptionalAndChecked(t *testing.T) {
	r := newRig(t)
	if _, err := r.parse(t); err != nil {
		t.Fatalf("no board: %s", problems(err))
	}
	r.cfg.Board = &Board{Owner: "acme", Number: 3, Organization: true, StatusField: "Stage", PublicURL: "https://whr.example.test"}
	if c, err := r.parse(t); err != nil || c.Board == nil || c.Board.Number != 3 {
		t.Fatalf("a good board: %s", problems(err))
	}
	for name, mut := range map[string]func(*Board){
		"owner a path":     func(b *Board) { b.Owner = "../x" },
		"no number":        func(b *Board) { b.Number = 0 },
		"control in field": func(b *Board) { b.LinkField = "a\nb" },
		"http link":        func(b *Board) { b.PublicURL = "http://whr.example.test" },
		"link with path":   func(b *Board) { b.PublicURL = "https://whr.example.test/x" },
		"link credentials": func(b *Board) { b.PublicURL = "https://u:p@whr.example.test" },
	} {
		r.cfg.Board = &Board{Owner: "acme", Number: 3}
		mut(r.cfg.Board)
		if _, err := r.parse(t); !strings.Contains(problems(err), "board.") {
			t.Errorf("%s: %s", name, problems(err))
		}
	}
}

func TestBudgets(t *testing.T) {
	r := newRig(t)
	r.cfg.Budgets = Budgets{PerRun: BudgetLimit{MaxTokens: 500_000, MaxCostUSD: 2.5}, PerTask: BudgetLimit{MaxCostUSD: 10}, SoftPercent: 90}
	if _, err := r.parse(t); err != nil {
		t.Errorf("valid budgets: %s", problems(err))
	}
	for name, b := range map[string]Budgets{
		"budgets.per_run":      {PerRun: BudgetLimit{MaxTokens: -1}},
		"budgets.per_task":     {PerTask: BudgetLimit{MaxCostUSD: -0.5}},
		"budgets.soft_percent": {SoftPercent: 100},
	} {
		r.cfg.Budgets = b
		if _, err := r.parse(t); !strings.Contains(problems(err), name) {
			t.Errorf("%s: %s", name, problems(err))
		}
	}
}

func TestRepositoriesRunUnderAWorkflow(t *testing.T) {
	r := newRig(t)
	if c, err := r.parse(t); err != nil || c.Repositories[0].Preset() != policy.Integration || c.Repositories[0].Target("main") != "develop" {
		t.Fatalf("the default: %v", err)
	}
	for _, tc := range []struct {
		workflow, branch, defaultBranch, want string
		preset                                policy.Preset
	}{
		{"prototype", "dev", "main", "dev", policy.Prototype},
		{"integration", "", "main", "develop", policy.Integration},
		{"integration", "next", "main", "next", policy.Integration},
		{"published", "", "trunk", "trunk", policy.Published},
	} {
		r.cfg.Repositories = []Repository{{Name: "wstein/workharbor", Workflow: tc.workflow, IntegrationBranch: tc.branch}}
		c, err := r.parse(t)
		if err != nil {
			t.Fatalf("%+v: %s", tc, problems(err))
		}
		if got := c.Repositories[0]; got.Preset() != tc.preset || got.Target(tc.defaultBranch) != tc.want {
			t.Errorf("%+v: preset %s target %q", tc, got.Preset(), got.Target(tc.defaultBranch))
		}
	}
	for name, repo := range map[string]Repository{
		"unknown":               {Name: "a/b", Workflow: "yolo"},
		"wrong case":            {Name: "a/b", Workflow: "Published"},
		"an agent branch":       {Name: "a/b", IntegrationBranch: "agent/x"},
		"a case variant":        {Name: "a/b", IntegrationBranch: "Agent/x"},
		"a bare agent":          {Name: "a/b", IntegrationBranch: "AGENT"},
		"a bad branch":          {Name: "a/b", IntegrationBranch: "a..b"},
		"published with branch": {Name: "a/b", Workflow: "published", IntegrationBranch: "develop"},
		"a prototype without an integration branch": {Name: "a/b", Workflow: "prototype"},
	} {
		r.cfg.Repositories = []Repository{repo}
		if _, err := r.parse(t); !strings.Contains(problems(err), "repositories[0]") {
			t.Errorf("%s: %s", name, problems(err))
		}
	}
	// the repository cannot choose: nothing reads a file of it, and the key is strict
	if _, err := Parse([]byte(`{"repositories":[{"name":"a/b","preset":"prototype"}]}`)); err == nil {
		t.Error("an unknown key was accepted")
	}
}

func TestConsoleDefaultsAndValidation(t *testing.T) {
	d := Console{}.Resolved()
	if d.CPUs != 2 || d.MemoryMB != 2048 || d.DiskMB != 10240 || len(d.EgressAllow) < 4 {
		t.Errorf("defaults = %+v", d)
	}
	for _, h := range d.EgressAllow {
		if !validEgressHost(h) || h == "api.anthropic.com" {
			t.Errorf("default console host %q", h)
		}
	}
	d.EgressAllow[0] = "changed.example"
	if (Console{}).Resolved().EgressAllow[0] == "changed.example" {
		t.Error("the default list is shared between callers")
	}
	r := newRig(t)
	r.cfg.Console = Console{EgressAllow: []string{"proxy.golang.org"}, CPUs: 4}
	if _, err := r.parse(t); err != nil {
		t.Errorf("a good console: %s", problems(err))
	}
	// The supervisor's own configuration may hold a wildcard: every subdomain, not
	// the bare name (§7.2).
	r.cfg.Console = Console{EgressAllow: []string{"*.github.io", "github.com"}}
	if _, err := r.parse(t); err != nil {
		t.Errorf("a wildcard from the configuration: %s", problems(err))
	}
	for _, bad := range []string{"*", "*.com", "**.example.com", "a*.example.com", "*.*.example.com", "example.*", "*example.com", "*.", "*.example.com:443", "*.10.0.0.1"} {
		r.cfg.Console = Console{EgressAllow: []string{bad}}
		if _, err := r.parse(t); !strings.Contains(problems(err), "console.egress_allow") {
			t.Errorf("%q was accepted as a console entry: %s", bad, problems(err))
		}
		r.cfg.Console = Console{}
		r.cfg.Environment = Environment{EgressAllow: []string{bad}}
		if _, err := r.parse(t); !strings.Contains(problems(err), "environment.egress_allow") {
			t.Errorf("%q was accepted as an environment entry: %s", bad, problems(err))
		}
		r.cfg.Environment = Environment{}
	}
	r.cfg.Environment = Environment{EgressAllow: []string{"api.anthropic.com", "*.example.com"}}
	if _, err := r.parse(t); err != nil {
		t.Errorf("a wildcard in the environment's list: %s", problems(err))
	}
	r.cfg.Environment = Environment{}
	for name, c := range map[string]Console{
		"console": {MemoryMB: -1},
	} {
		r.cfg.Console = c
		if _, err := r.parse(t); !strings.Contains(problems(err), name) {
			t.Errorf("%s: %s", name, problems(err))
		}
	}
	r.cfg.Console = Console{EgressAllow: []string{"10.0.0.1"}}
	if _, err := r.parse(t); !strings.Contains(problems(err), "console.egress_allow") {
		t.Errorf("an IP address: %s", problems(err))
	}
}

func TestPreviewPorts(t *testing.T) {
	r := newRig(t)
	if c, err := r.parse(t); err != nil || c.Preview.On() {
		t.Fatalf("no preview section: %v", err)
	}
	r.cfg.Preview = Preview{FirstPort: 9400, LastPort: 9409}
	if c, err := r.parse(t); err != nil || !c.Preview.On() {
		t.Errorf("a valid range: %v", problems(err))
	}
	for name, p := range map[string]Preview{
		"below 1024":     {FirstPort: 80, LastPort: 90},
		"above 65535":    {FirstPort: 9400, LastPort: 70000},
		"backwards":      {FirstPort: 9410, LastPort: 9400},
		"only the end":   {LastPort: 9410},
		"too many":       {FirstPort: 9400, LastPort: 9400 + MaxPreviewPorts},
		"holds the UI's": {FirstPort: 8700, LastPort: 8800},
	} {
		r.cfg.Preview = p
		if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "preview") {
			t.Errorf("%s: %q", name, problems(err))
		}
	}
}

func TestAccountIsDedicatedOrShared(t *testing.T) {
	for acct, ok := range map[string]bool{"": true, "dedicated": true, "shared": true, "own": false, "Shared": false} {
		r := newRig(t)
		r.cfg.Account = acct
		if _, err := r.parse(t); ok != (err == nil) {
			t.Errorf("account %q: accepted=%v, want %v (%s)", acct, err == nil, ok, problems(err))
		}
	}
}

func TestLowBalanceIsBounded(t *testing.T) {
	for _, v := range []float64{-1, MaxLowBalanceUSD * 2, 1e13} {
		r := newRig(t)
		r.cfg.Limits.LowBalanceUSD = v
		if _, err := r.parse(t); !strings.Contains(problems(err), "limits.low_balance_usd") {
			t.Errorf("%v accepted: %v", v, err)
		}
	}
	r := newRig(t)
	r.cfg.Limits.LowBalanceUSD = MaxLowBalanceUSD
	if _, err := r.parse(t); err != nil {
		t.Errorf("the cap itself: %v", err)
	}
}

// D51: the check command, the commit linter and the bot's signing key.
func TestPublishSettings(t *testing.T) {
	r := newRig(t)
	c, err := r.parse(t)
	if err != nil || c.Repositories[0].Linter() != CommitLintConventional || c.Repositories[0].Check != "" || c.BotSigningKeyFile != "" {
		t.Fatalf("the defaults: %v", err)
	}
	r.cfg.Repositories = []Repository{{Name: "a/b", Check: "make check", CommitLint: "workharbor"}}
	if c, err = r.parse(t); err != nil || c.Repositories[0].Linter() != CommitLintWorkharbor || c.Repositories[0].Check != "make check" {
		t.Fatalf("set: %v", err)
	}
	for name, repo := range map[string]Repository{
		"an unknown linter": {Name: "a/b", CommitLint: "strict"},
		"a blank check":     {Name: "a/b", Check: "  "},
		"a newline":         {Name: "a/b", Check: "make\ncheck"},
		"a long check":      {Name: "a/b", Check: strings.Repeat("x", MaxCheck+1)},
	} {
		r.cfg.Repositories = []Repository{repo}
		if _, err := r.parse(t); !strings.Contains(problems(err), "repositories[0].") {
			t.Errorf("%s: %s", name, problems(err))
		}
	}
}

func TestBotSigningKeyIsASecretOutsideTheRoots(t *testing.T) {
	r := newRig(t)
	key := filepath.Join(r.dir, "secrets", "bot_ed25519")
	if err := os.WriteFile(key, []byte("key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg.BotSigningKeyFile = key
	if _, err := r.parse(t); err != nil {
		t.Fatalf("a good key file: %s", problems(err))
	}
	if err := os.Chmod(key, 0o640); err != nil { //nolint:gosec // a test making a secret too open
		t.Fatal(err)
	}
	if got := problems(mustFail(t, r)); !strings.Contains(got, "bot_signing_key_file: ") || !strings.Contains(got, "0600") {
		t.Errorf("a group-readable key: %s", got)
	}
	_ = os.Chmod(key, 0o600)
	inRoot := filepath.Join(r.dir, "workspaces", "bot_ed25519")
	if err := os.WriteFile(inRoot, []byte("key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg.BotSigningKeyFile = inRoot
	if got := problems(mustFail(t, r)); !strings.Contains(got, "bot_signing_key_file: ") || !strings.Contains(got, "workspace root") {
		t.Errorf("a key in a workspace root: %s", got)
	}
}

func mustFail(t *testing.T, r *rig) error {
	t.Helper()
	_, err := r.parse(t)
	if err == nil {
		t.Fatal("the configuration was accepted")
	}
	return err
}

func TestDurationBudgetValidation(t *testing.T) {
	for _, v := range []string{"", "0", "0s", "1ms", "1.5s", "24h"} {
		t.Run("valid="+v, func(t *testing.T) {
			r := newRig(t)
			r.cfg.Budgets.PerRun.MaxDuration = v
			if _, err := r.parse(t); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, v := range []string{"-1s", "oops", "1ns", "1.0001ms", "999999999999999999999h"} {
		t.Run("invalid="+v, func(t *testing.T) {
			r := newRig(t)
			r.cfg.Budgets.PerTask.MaxDuration = v
			if _, err := r.parse(t); err == nil || !strings.Contains(problems(err), "max_duration") {
				t.Fatalf("invalid duration accepted: %v", err)
			}
		})
	}
}

func TestNaturalLessOrdersIndexesNumerically(t *testing.T) {
	got := []string{"api_clients[10]: x", "api_clients[2]: x", "api_clients[1]: x", "api_token_file: x"}
	sort.Slice(got, func(i, j int) bool { return naturalLess(got[i], got[j]) })
	want := []string{"api_clients[1]: x", "api_clients[2]: x", "api_clients[10]: x", "api_token_file: x"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A FIFO named as the configuration must not hang the process (#493).
func TestLoadDoesNotHangOnAFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "config.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Load(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO loaded as a configuration")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Load hangs on a FIFO")
	}
}
