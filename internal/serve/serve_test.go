package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/agent/agenttest"
	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/forge"
	"github.com/wstein/workharbor/internal/forge/forgetest"
	"github.com/wstein/workharbor/internal/redact"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/runtime/runtimetest"
	"github.com/wstein/workharbor/internal/service"
	"github.com/wstein/workharbor/internal/store"
)

const token = "serve-test-token"

func newDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "api.token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := runtimetest.NewFakeHarness(t)
	// a unix socket path is short: the test's own temporary directory may not fit
	sockDir, err := os.MkdirTemp("", "whr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	return Deps{
		SocketPath: filepath.Join(sockDir, "api.sock"),
		Config:     &config.Config{Listen: "127.0.0.1:0", APITokenFile: tokenFile},
		Store:      st,
		Runtime:    h.Adapter,
		Agent:      agenttest.NewFake(agenttest.FullCaps()),
		Owner:      h.Owner,
		Spec:       func(domain.Workspace) runtime.Spec { return h.NewSpec() },
		Prepare:    h.Prepare,
		AgentSpec: func(domain.Task, domain.Run) agent.StartSpec {
			return agent.StartSpec{Auth: agent.AuthSubscription}
		},
		ReconcileEvery: 20 * time.Millisecond,
	}, st
}

func get(t *testing.T, url, tok string) (int, string) {
	t.Helper()
	return getVia(t, http.DefaultClient, url, tok)
}

// socketClient dials the API's unix socket whatever the URL says.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
}

func getVia(t *testing.T, hc *http.Client, url, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The JSON API is served only on a private unix socket; the forwarded loopback
// address serves the web UI alone, so a leaked API token cannot be used from the
// phone network (D29, §7.5).
func TestRunServesTheAPIOnASocketAndTheWebUIOnLoopback(t *testing.T) {
	d, _ := newDeps(t)
	type addrs struct{ web, api net.Addr }
	up := make(chan addrs, 1)
	d.Ready = func(web, api net.Addr) { up <- addrs{web, api} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d) }()
	var a addrs
	select {
	case a = <-up:
	case err := <-done:
		t.Fatalf("Run ended early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not come up")
	}
	if tcp, ok := a.web.(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		t.Fatalf("the web UI listens on %v, want a loopback address", a.web)
	}
	if a.api.Network() != "unix" || a.api.String() != d.SocketPath {
		t.Fatalf("the API listens on %v %v, want the socket %s", a.api.Network(), a.api, d.SocketPath)
	}
	if fi, err := os.Stat(d.SocketPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the socket: %v %v", fi, err)
	}

	sock := socketClient(d.SocketPath)
	if code, body := getVia(t, sock, "http://whr/v1/health", token); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Errorf("health = %d %s", code, body)
	}
	if code, _ := getVia(t, sock, "http://whr/v1/health", ""); code != 401 {
		t.Errorf("health without the token = %d", code)
	}
	if code, body := getVia(t, sock, "http://whr/v1/tasks", token); code != 200 || !strings.Contains(body, `"data":[]`) {
		t.Errorf("tasks = %d %s", code, body)
	}

	// the web listener has no /v1: not with the token, not without it
	web := "http://" + a.web.String()
	for _, path := range []string{"/v1/health", "/v1/tasks", "/v1/passkeys", "/v1/passkeys/enrolments", "/v1/decisions/d1/answer", "/v1/openapi.json"} {
		for _, tok := range []string{token, ""} {
			if code, body := get(t, web+path, tok); code != http.StatusNotFound || strings.Contains(body, `"schema_version"`) && !strings.Contains(body, "not found") {
				t.Errorf("GET %s on the web listener (token %v) = %d %s", path, tok != "", code, body)
			}
		}
	}
	if code, _ := get(t, web+"/login", ""); code != 200 {
		t.Errorf("the web UI's login page on the web listener = %d", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v after a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	if _, err := os.Stat(d.SocketPath); err == nil {
		t.Error("the socket file is left behind after a clean stop")
	}
}

// A state directory other users can enter is refused at start, before anything
// is served.
func TestRunRefusesASocketDirectoryOthersCanEnter(t *testing.T) {
	d, _ := newDeps(t)
	dir := filepath.Dir(d.SocketPath)
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // a deliberately open directory
		t.Fatal(err)
	}
	if err := Run(context.Background(), d); err == nil || !strings.Contains(err.Error(), "accessible to others") {
		t.Errorf("Run = %v, want a refusal of the open directory", err)
	}
}

func TestRunRefusesAWildcardAddress(t *testing.T) {
	d, _ := newDeps(t)
	d.Config.Listen = "0.0.0.0:0"
	if err := Run(context.Background(), d); err == nil || !strings.Contains(err.Error(), "unspecified") {
		t.Errorf("Run = %v, want a wildcard refusal", err)
	}
}

func TestRunRefusesALooseTokenFile(t *testing.T) {
	d, _ := newDeps(t)
	if err := os.Chmod(d.Config.APITokenFile, 0o644); err != nil { //nolint:gosec // a deliberately loose file
		t.Fatal(err)
	}
	if err := Run(context.Background(), d); err == nil {
		t.Error("a world-readable token file was accepted")
	}
}

// What was running when the supervisor died is reconciled before the first
// request is served: a run with no session is not left running (D6).
func TestRunReconcilesBeforeItAcceptsRequests(t *testing.T) {
	d, st := newDeps(t)
	prep, err := d.Prepare(d.Spec(domain.Workspace{ID: "w1"}))
	if err != nil {
		t.Fatal(err)
	}
	envID, err := d.Runtime.Provision(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Runtime.Start(context.Background(), envID); err != nil {
		t.Fatal(err)
	}
	agg := domain.NewTaskAggregate(domain.Task{ID: "t1", Repo: "a/b", Issue: "#1", State: domain.TaskQueued, CreatedAt: time.Now()})
	agg.AddEnvironment(domain.Environment{ID: domain.ID(envID), Backend: "fake", State: domain.EnvRunning})
	if err := agg.StartRun(domain.Run{ID: "r1", WorkspaceID: "w1", EnvID: domain.ID(envID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveTask(context.Background(), agg); err != nil {
		t.Fatal(err)
	}

	ready := make(chan net.Addr, 1)
	d.Ready = func(web, _ net.Addr) { ready <- web }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Run(ctx, d) }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("no server")
	}
	got, err := st.LoadTask(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := got.Run("r1"); r.State != domain.RunInterrupted {
		t.Errorf("the run is %s: it must await human initiation", r.State)
	}
	if started := d.Agent.(*agenttest.Fake).Started(); started != 0 {
		t.Errorf("startup sent %d agent requests", started)
	}
}

func TestNewIDIsRandomAndPrefixed(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b || !strings.HasPrefix(string(a), "x-") || len(a) != 18 {
		t.Errorf("ids %q %q", a, b)
	}
}

// With a board in the configuration the service writes it through the guard, and
// the card links the task on whr's HTTPS name.
func TestTheBoardIsWiredThroughTheGuard(t *testing.T) {
	var scfg service.Config
	addBoard(&scfg, Deps{Config: &config.Config{}, Forge: NewForgeAccess(forgetest.NewFake())})
	if scfg.Board != nil || scfg.BoardLink != nil {
		t.Error("a board without configuration")
	}
	cfg := &config.Config{Board: &config.Board{Owner: "acme", Number: 3, PublicURL: "https://whr.example.test"}}
	f := forgetest.NewFake()
	addBoard(&scfg, Deps{Config: cfg, Forge: NewForgeAccess(f)})
	if _, ok := scfg.Board.(*forge.Guard); !ok {
		t.Fatalf("the board is %T, want the guard", scfg.Board)
	}
	if got := scfg.BoardLink("t1"); got != "https://whr.example.test/tasks/t1" {
		t.Errorf("link %q", got)
	}
	if err := scfg.Board.UpdateCard(context.Background(), "wstein/workharbor", 7, forge.CardUpdate{Status: forge.StatusDone}); err != nil || len(f.CardsSeen()) != 1 {
		t.Errorf("update: %v, cards %v", err, f.CardsSeen())
	}
}

func TestBudgetsAreConvertedToMicroDollars(t *testing.T) {
	got := Budgets(config.Budgets{PerRun: config.BudgetLimit{MaxTokens: 7, MaxCostUSD: 2.5}, PerTask: config.BudgetLimit{MaxCostUSD: 0.1 + 0.2}, SoftPercent: 70})
	want := service.Budgets{PerRun: service.Limit{MaxTokens: 7, MaxCostMicroUSD: 2_500_000}, PerTask: service.Limit{MaxCostMicroUSD: 300_000}, SoftPercent: 70}
	if got != want {
		t.Errorf("Budgets = %+v, want %+v", got, want)
	}
}

type revokingForge struct {
	*forgetest.Fake
	calls int
}

func (f *revokingForge) RevokeTokens(context.Context) (int, error) { f.calls++; return 3, nil }

func TestKillAllGetsTheForgesRevoker(t *testing.T) {
	var scfg service.Config
	addRevoker(&scfg, Deps{Forge: NewForgeAccess(forgetest.NewFake())})
	if scfg.RevokeTokens != nil {
		t.Error("a forge that cannot revoke must not get a revoker")
	}
	f := &revokingForge{Fake: forgetest.NewFake()}
	addRevoker(&scfg, Deps{Forge: NewForgeAccess(f)})
	if scfg.RevokeTokens == nil {
		t.Fatal("no revoker")
	}
	if n, err := scfg.RevokeTokens(context.Background()); n != 3 || err != nil || f.calls != 1 {
		t.Errorf("revoked %d, %v, calls %d", n, err, f.calls)
	}
}

// A change of a repository's preset is a policy change: refused unless the
// operator confirmed it on the host CLI, and then audited (D47).
func TestAChangeOfPresetNeedsTheHostsConfirmationAndIsAudited(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workharbor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := func(workflow string, branch ...string) Deps {
		r := config.Repository{Name: "wstein/workharbor", Workflow: workflow}
		if len(branch) > 0 {
			r.IntegrationBranch = branch[0]
		}
		return Deps{Config: &config.Config{Repositories: []config.Repository{r}}, Store: st}
	}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	ctx := context.Background()

	if _, err := applyWorkflows(ctx, cfg(""), logf); err != nil { // the first start records the default
		t.Fatal(err)
	}
	if _, err := applyWorkflows(ctx, cfg("integration"), logf); err != nil {
		t.Errorf("the same preset: %v", err)
	}
	_, err = applyWorkflows(ctx, cfg("prototype", "dev"), logf)
	if err == nil || !strings.Contains(err.Error(), "--accept-workflow-change") || !strings.Contains(err.Error(), "policy change") {
		t.Fatalf("an unconfirmed change = %v", err)
	}
	if w, _, _ := st.RecordedWorkflow(ctx, "wstein/workharbor"); w.Workflow != "integration" {
		t.Errorf("a refused change was recorded: %q", w)
	}
	d := cfg("prototype", "dev")
	d.AcceptWorkflowChange = true
	if _, err := applyWorkflows(ctx, d, logf); err != nil {
		t.Fatal(err)
	}
	ch, _ := st.WorkflowChanges(ctx, "wstein/workharbor")
	if len(ch) != 1 || ch[0].From != "integration" || ch[0].To != "prototype" || ch[0].ConfirmedBy != "host-cli" {
		t.Errorf("audit %+v", ch)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "changed from integration on \"develop\" to prototype") {
		t.Errorf("logs %v", logs)
	}
}

// A new integration branch is a policy change like a new preset, and the
// repository's name in another case is the same repository (D47, §6).
func TestAChangeOfIntegrationBranchNeedsTheHostsConfirmationToo(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workharbor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := func(name, branch string) Deps {
		return Deps{Config: &config.Config{Repositories: []config.Repository{{Name: name, Workflow: "integration", IntegrationBranch: branch}}}, Store: st}
	}
	logf := func(string, ...any) {}
	ctx := context.Background()
	if _, err := applyWorkflows(ctx, cfg("wstein/workharbor", "develop"), logf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wstein/workharbor", "Wstein/WorkHarbor"} {
		_, err := applyWorkflows(ctx, cfg(name, "next"), logf)
		if err == nil || !strings.Contains(err.Error(), "--accept-workflow-change") {
			t.Fatalf("%s: an unconfirmed branch change = %v", name, err)
		}
	}
	if rec, _, _ := st.RecordedWorkflow(ctx, "wstein/workharbor"); rec.Branch != "develop" {
		t.Errorf("a refused change was recorded: %+v", rec)
	}
	d := cfg("WSTEIN/workharbor", "next")
	d.AcceptWorkflowChange = true
	if _, err := applyWorkflows(ctx, d, logf); err != nil {
		t.Fatal(err)
	}
	ch, _ := st.WorkflowChanges(ctx, "wstein/workharbor")
	if len(ch) != 1 || ch[0].FromBranch != "develop" || ch[0].ToBranch != "next" {
		t.Errorf("audit %+v", ch)
	}
}

// With a passkey enrolled, a workflow change nobody confirmed does not stop the
// supervisor from starting: the repository is held on what it was recorded under
// and the change waits for the web UI's step-up (D45, issue #107). Without a
// passkey it still refuses, and the host's flag still confirms.
func TestAnUnconfirmedWorkflowChangeIsHeldWhenAPasskeyCanConfirmIt(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workharbor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	cfg := func(workflow, branch string) Deps {
		c := &config.Config{PublicURL: "https://whr.example.test", Repositories: []config.Repository{{Name: "wstein/workharbor", Workflow: workflow, IntegrationBranch: branch}}}
		return Deps{Config: c, Store: st}
	}
	logf := func(string, ...any) {}
	if _, err := applyWorkflows(ctx, cfg("integration", "develop"), logf); err != nil {
		t.Fatal(err)
	}

	// no passkey: refused, and nothing is raised
	if _, err := applyWorkflows(ctx, cfg("prototype", "scratch"), logf); err == nil {
		t.Fatal("a change was accepted with no passkey to confirm it")
	}
	if open, _ := st.OpenChanges(ctx); len(open) != 0 {
		t.Fatalf("a refused change was raised: %+v", open)
	}

	// a passkey is enrolled: held, raised, not recorded
	if err := st.AddPasskey(ctx, store.Passkey{ID: "k1", Name: "phone", Credential: []byte("x"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := cfg("prototype", "scratch")
	held, err := applyWorkflows(ctx, d, logf)
	if err != nil {
		t.Fatal(err)
	}
	want := store.WorkflowRecord{Workflow: "integration", Branch: "develop"}
	if held["wstein/workharbor"] != want {
		t.Fatalf("held = %+v, want the recorded workflow %+v", held, want)
	}
	if rec, _, _ := st.RecordedWorkflow(ctx, "wstein/workharbor"); rec != want {
		t.Errorf("a held change was recorded: %+v", rec)
	}
	open, _ := st.OpenChanges(ctx)
	if len(open) != 1 || open[0].To.Workflow != "prototype" {
		t.Fatalf("open = %+v", open)
	}
	// what the supervisor then runs under is the held workflow, and the caller's
	// configuration is untouched
	eff := withHeld(d.Config, held)
	if r := eff.Repositories[0]; r.Workflow != "integration" || r.IntegrationBranch != "develop" {
		t.Errorf("effective repository = %+v", r)
	}
	if r := d.Config.Repositories[0]; r.Workflow != "prototype" {
		t.Errorf("the configuration was changed: %+v", r)
	}
	// a restart raises the same change, not a second one
	if _, err := applyWorkflows(ctx, cfg("prototype", "scratch"), logf); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.OpenChanges(ctx); len(again) != 1 || again[0].ID != open[0].ID {
		t.Errorf("a restart raised another change: %+v", again)
	}

	// the configuration goes back: the change is withdrawn
	held, err = applyWorkflows(ctx, cfg("integration", "develop"), logf)
	if err != nil || len(held) != 0 {
		t.Fatalf("held = %v, %v", held, err)
	}
	if again, _ := st.OpenChanges(ctx); len(again) != 0 {
		t.Errorf("a change the configuration no longer asks for is still open: %+v", again)
	}

	// the host's flag confirms and leaves nothing open
	if _, err := applyWorkflows(ctx, cfg("prototype", "scratch"), logf); err != nil {
		t.Fatal(err)
	}
	flag := cfg("prototype", "scratch")
	flag.AcceptWorkflowChange = true
	if held, err := applyWorkflows(ctx, flag, logf); err != nil || len(held) != 0 {
		t.Fatalf("with the flag: held %v, %v", held, err)
	}
	if again, _ := st.OpenChanges(ctx); len(again) != 0 {
		t.Errorf("the host's confirmation left a change open: %+v", again)
	}
	if rec, _, _ := st.RecordedWorkflow(ctx, "wstein/workharbor"); rec.Workflow != "prototype" {
		t.Errorf("recorded = %+v", rec)
	}
}

// A repository held on its recorded workflow runs its agent in the mode of that
// workflow, and needs the allowlist it needs, whatever the file now says.
func TestAHeldRepositoryRunsInTheModeOfItsRecordedWorkflow(t *testing.T) {
	file := &config.Config{AgentAllowedTools: []string{"Read"}, Repositories: []config.Repository{{Name: "wstein/workharbor", Workflow: "published"}}}
	held := withHeld(file, map[string]store.WorkflowRecord{"wstein/workharbor": {Workflow: "integration", Branch: "develop"}})
	task := domain.Task{Repo: "wstein/workharbor"}
	if got := AgentSpecFor(file, agent.AuthSubscription)(task, domain.Run{}).PermissionMode; got != agent.PermissionManual {
		t.Fatalf("the file's own mode = %q, want manual", got)
	}
	spec := AgentSpecFor(held, agent.AuthSubscription)(task, domain.Run{})
	if spec.PermissionMode != agent.PermissionDontAsk || len(spec.AllowedTools) != 1 {
		t.Errorf("held spec = %q with tools %v, want dontAsk with the allowlist", spec.PermissionMode, spec.AllowedTools)
	}
	if !needsAllowlist(held) || needsAllowlist(file) {
		t.Errorf("needsAllowlist: held %v, file %v", needsAllowlist(held), needsAllowlist(file))
	}
}

func TestAddNotifierThrottlesOnlyWhenOneIsGiven(t *testing.T) {
	var scfg service.Config
	addNotifier(&scfg, Deps{})
	if scfg.Notifier != nil {
		t.Fatal("a notifier without one in Deps")
	}
	addNotifier(&scfg, Deps{Notifier: &recordingNotifier{}})
	if scfg.Notifier == nil {
		t.Fatal("no notifier assigned")
	}
	if _, ok := scfg.Notifier.(*recordingNotifier); ok {
		t.Fatal("the notifier is not wrapped in the throttle")
	}
}

// Run's service configuration carries the board, the revoker and the throttled
// notifier (#183): each of the three calls could be deleted unseen otherwise.
func TestServiceConfigCarriesTheBoardTheRevokerAndTheThrottledNotifier(t *testing.T) {
	rec := &recordingNotifier{}
	f := &revokingForge{Fake: forgetest.NewFake()}
	cfg := &config.Config{Board: &config.Board{Owner: "acme", Number: 3}}
	scfg := serviceConfig(Deps{Config: cfg, Forge: NewForgeAccess(f), Notifier: rec}, nil)
	if scfg.Board == nil {
		t.Error("no board")
	}
	if scfg.RevokeTokens == nil {
		t.Error("no revoker")
	}
	if scfg.Notifier == nil {
		t.Fatal("no notifier")
	}
	if _, ok := scfg.Notifier.(*recordingNotifier); ok {
		t.Error("the notifier is not behind the throttle")
	}
}

// A task error the reconciler reports is logged at start and on each pass, once
// while it stays the same, and again when it changes or clears.
func TestRunLogsTheReconcilersTaskErrors(t *testing.T) {
	d, _ := newDeps(t)
	var mu sync.Mutex
	var lines []string
	d.Logf = func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, a...))
	}
	var calls atomic.Int32
	d.reconcile = func(context.Context) (service.Report, error) {
		switch n := calls.Add(1); {
		case n <= 3:
			return service.Report{Errors: []error{errors.New("task t1: environment did not start")}}, nil
		case n <= 5:
			return service.Report{Errors: []error{errors.New("task t1: readiness timed out")}}, nil
		}
		return service.Report{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d) }()
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < 8 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	count := func(sub string) (n int) {
		for _, l := range lines {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	if n := count("environment did not start"); n != 1 {
		t.Errorf("the repeated error was logged %d times, want 1", n)
	}
	if n := count("readiness timed out"); n != 1 {
		t.Errorf("the changed error was logged %d times, want 1", n)
	}
	if n := count("have cleared"); n != 1 {
		t.Errorf("the clearing was logged %d times, want 1", n)
	}
}

func TestRedactedLogfMasksSecrets(t *testing.T) {
	rd := redact.New()
	if !rd.Add("s3cr3t-token-value-xyz") {
		t.Fatal("secret not registered")
	}
	var got string
	redactedLogf(rd, func(f string, a ...any) { got = fmt.Sprintf(f, a...) })("task %s: %v", "t1", errors.New("auth s3cr3t-token-value-xyz failed"))
	if strings.Contains(got, "s3cr3t") || !strings.Contains(got, "task t1") {
		t.Errorf("log line = %q", got)
	}
}

// A pass cut short by the shutdown returns a partial report: it is not recorded
// as the new error set, so the log does not claim the errors cleared.
func TestRunDoesNotRecordAPassCutShortByShutdown(t *testing.T) {
	d, _ := newDeps(t)
	var mu sync.Mutex
	var lines []string
	d.Logf = func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, a...))
	}
	var calls atomic.Int32
	d.reconcile = func(ctx context.Context) (service.Report, error) {
		if calls.Add(1) == 1 {
			return service.Report{Errors: []error{errors.New("task t1: environment did not start")}}, nil
		}
		<-ctx.Done()
		return service.Report{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d) }()
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "have cleared") || strings.Contains(l, "reconcile: context") {
			t.Errorf("unexpected log line after a cancelled pass: %q", l)
		}
	}
}

func TestRedactedLogfEscapesControlCharacters(t *testing.T) {
	var got string
	redactedLogf(redact.New(), func(f string, a ...any) { got = fmt.Sprintf(f, a...) })(
		"task %s: %v", "t1", errors.New("boom\n2026-01-01 forged line\r\x1b[2J\u009b31m\tend\x7f\u202e\u2066\u200f\u061c\u2028\u2029"))
	if strings.ContainsAny(got, "\n\r\x1b\x7f\u009b\u202e\u2066\u200f\u061c\u2028\u2029") {
		t.Errorf("log line has a raw control character: %q", got)
	}
	for _, want := range []string{`boom\n2026`, `\r\x1b[2J`, `\u009b31m`, "\tend", `\x7f`, `\u202e`, `\u2066`, `\u200f`, `\u061c`, `\u2028`, `\u2029`} {
		if !strings.Contains(got, want) {
			t.Errorf("log line %q lacks %q", got, want)
		}
	}
}

func TestDirectWebListenerRequiresRemotePasskeyReadiness(t *testing.T) {
	for _, publicURL := range []string{"", "http://w.example", "https://w.example"} {
		t.Run(publicURL, func(t *testing.T) {
			d, _ := newDeps(t)
			d.Config.Listen = "10.44.0.1:8787"
			d.Clock = service.SystemClock{}
			d.Config.PublicURL = publicURL
			d.Ready = func(net.Addr, net.Addr) { t.Error("remote listener became ready without a usable enrolled passkey") }
			err := Run(context.Background(), d)
			if err == nil || !strings.Contains(err.Error(), "passkey") {
				t.Fatalf("Run = %v, want remote passkey readiness refusal before binding", err)
			}
		})
	}
}

func TestRemoteWebReadinessUsesEffectiveOriginAndFailsClosedOnStoreError(t *testing.T) {
	d, st := newDeps(t)
	d.Config.Listen = "10.44.0.1:8787"
	d.Config.Board = &config.Board{PublicURL: "https://w.example"}
	if err := st.AddPasskey(context.Background(), store.Passkey{ID: "k1", Name: "phone", Credential: []byte("credential"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if remote, err := remoteWebReady(context.Background(), d); err != nil || !remote {
		t.Fatalf("enrolled effective HTTPS origin: remote=%v err=%v", remote, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteWebReady(context.Background(), d); err == nil || !strings.Contains(err.Error(), "passkey readiness") {
		t.Fatalf("closed passkey store: %v", err)
	}
	d.Config.Listen = "127.0.0.1:8787"
	d.Config.Board = nil
	if remote, err := remoteWebReady(context.Background(), d); err != nil || remote {
		t.Fatalf("loopback recovery: remote=%v err=%v", remote, err)
	}
}
