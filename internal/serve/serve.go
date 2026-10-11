// Package serve runs the supervisor: the service, its reconciler and the JSON
// API on a loopback address (design §5.3, §9.7). It is wiring: everything it
// does is in the packages it joins, so a test can run it on the fakes.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/api"
	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/forge"
	"github.com/wstein/workharbor/internal/hostgit"
	"github.com/wstein/workharbor/internal/notify"
	"github.com/wstein/workharbor/internal/passkey"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/service"
	"github.com/wstein/workharbor/internal/skillset"
	"github.com/wstein/workharbor/internal/sshca"
	"github.com/wstein/workharbor/internal/store"
	"github.com/wstein/workharbor/internal/web"
)

// Deps is everything Run needs, already built. `whr serve` builds the real ones
// (cmd/whr); tests pass fakes.
type Deps struct {
	// BuildDir overrides the Apple build-volume location for backend-specific disposable caches.
	BuildDir            string
	ProjectInstructions func(context.Context, domain.Task, domain.Run) (service.ProjectInstructions, error)
	SkillBinding        func(context.Context, []skillset.Binding) (skillset.Binding, error)
	PrepareSkills       func(context.Context, domain.Run, service.SkillMount) error
	Config              *config.Config
	Store               *store.Store
	Runtime             runtime.Adapter
	Agent               agent.Adapter
	// Issues loads issues from the forge, and Forge is the narrow access to the
	// rest of it: guarded effects only (forge.go). The raw adapter stays in Build.
	// It is *IssueAccess, not an interface, so the compiler rejects the raw
	// client here (issue #247).
	Issues *IssueAccess
	Forge  ForgeAccess
	Git    *hostgit.Git
	// Owner is the owner label of every environment this supervisor makes.
	Owner string
	// Spec returns an environment's spec for a workspace, and Prepare is
	// runtime.Prepare with this host's options.
	// Topics and EditorDir make `whr open` work (optional).
	Topics    service.TopicsFunc
	EditorDir string
	// NewPusher returns the forge's Pusher over a repository's supervisor copy
	// (the GitHub adapter's, with its per-push scoped token, D51), and Committer
	// reads the bot identity of the App (D15). Both are needed to publish: without
	// them a stopped run is not prepared and nothing is pushed.
	NewPusher func(repo *hostgit.Repo) forge.Pusher
	Committer func(ctx context.Context) (hostgit.Identity, error)
	Spec      func(domain.Workspace) runtime.Spec
	Prepare   func(runtime.Spec) (runtime.PreparedSpec, error)
	// ConsoleSpec returns the console's spec for the workspaces mounted
	// read-write, and ConsoleImage builds the console image on first use (D43).
	// Both are optional: without them there is no console.
	// ConsoleSSH is the authority that signs the console's SSH certificates (issue
	// #32). Nil: the console has no SSH access.
	ConsoleSSH   *sshca.CA
	ConsoleSpec  func(rw []domain.Workspace) runtime.Spec
	ConsoleImage func(ctx context.Context) error
	ConsoleDir   func(w domain.Workspace) string
	// Environment reads a repository's environment from the supervisor's own
	// copy of its default branch (D38), for the egress requests a run's start
	// asks about. Optional.
	Environment func(ctx context.Context, repo, branch string) (service.RepoEnvironment, error)
	// Shell says where the agent's CLI and home are, for `whr ws shell` (design
	// §7.3, "The sign-in shell"). Optional.
	Shell *service.ShellConfig
	// AgentSpec returns how an agent is started for a run.
	AgentSpec func(domain.Task, domain.Run) agent.StartSpec
	// AgentSpecFor makes AgentSpec for a configuration. Run uses it when a
	// repository is held on its recorded workflow (a change nobody confirmed), so
	// the permission mode follows the workflow it really runs under.
	AgentSpecFor func(*config.Config) func(domain.Task, domain.Run) agent.StartSpec
	Clock        service.Clock
	// Notifier delivers a push (design §9.4); nil sends none. Run wraps it in the
	// per-task throttle and the bounded queue.
	Notifier notify.Notifier
	Logf     func(format string, args ...any)
	// ReconcileEvery is how often the reconciler compares the database with the
	// runtime. Default 30 s.
	ReconcileEvery time.Duration
	// AcceptWorkflowChange confirms that a repository's workflow preset in the
	// configuration may differ from the one recorded: a policy change, which the
	// operator confirms on the host CLI (`whr serve --accept-workflow-change`) or,
	// with a passkey enrolled, in the web UI (issue #107).
	AcceptWorkflowChange bool
	// Setup supplies optional read-only diagnostics to the web UI.
	Setup web.Setup
	// SocketPath is the unix socket the JSON API is served on, and only there
	// (D29, §7.5): the forwarded `listen` address serves the web UI alone.
	SocketPath string
	// Ready, if set, is called with the address the web UI listens on and the API's
	// socket, once both are bound.
	Ready func(web, api net.Addr)

	// reconcile replaces the service's Reconcile in tests.
	reconcile func(context.Context) (service.Report, error)
}

// reconcileErrors logs the per-task errors of a reconcile pass (Report.Errors).
// A pass that reports the same errors as the one before logs nothing, so a stuck
// task does not fill the log every pass; a changed set is logged again, and a
// cleared one says so. logf is the redacting logger, so a secret in an error text
// never reaches the log. Only the reconcile loop calls it, from one goroutine at
// a time (the start pass finishes before the loop starts).
type reconcileErrors struct {
	logf func(string, ...any)
	prev string
}

func (r *reconcileErrors) note(errs []error) {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	key := strings.Join(msgs, "\n")
	if key == r.prev {
		return
	}
	r.prev = key
	if len(msgs) == 0 {
		r.logf("reconcile: the earlier task errors have cleared")
		return
	}
	for _, m := range msgs {
		r.logf("reconcile: %s", m)
	}
}

// NewID returns a random ID with a short prefix.
func NewID() domain.ID {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("serve: no randomness: " + err.Error())
	}
	return domain.ID("x-" + hex.EncodeToString(b))
}

// Run starts the supervisor and blocks until ctx ends. It reconciles once
// before it accepts a request, so a restart resumes what was running (D6), then
// again every ReconcileEvery. On the way out it stops the sessions and leaves
// their runs resumable.
func Run(ctx context.Context, d Deps) error {
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	token, err := api.TokenFromConfig(d.Config)
	if err != nil {
		return err
	}
	clients, err := api.ClientsFromConfig(d.Config)
	if err != nil {
		return err
	}
	scfg := serviceConfig(d, logf)
	svc := service.New(d.Store, d.Runtime, d.Agent, d.Clock, scfg)
	defer svc.Shutdown()
	held, err := applyWorkflows(ctx, d, logf)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		d.Config = withHeld(d.Config, held)
		// What a held repository runs under decides its agent's mode and whether an
		// allowlist is needed: both are read from the held configuration, not the
		// one in the file.
		if needsAllowlist(d.Config) && len(d.Config.AgentAllowedTools) == 0 {
			return errors.New("agent_allowed_tools is needed: a repository held on its recorded workflow runs its agent in the dontAsk mode, where only the tools you list may run")
		}
		if d.AgentSpecFor != nil {
			d.AgentSpec = d.AgentSpecFor(d.Config)
		}
	}
	ws := service.NewWorkspaces(svc, service.WorkspaceConfig{
		Workflow: func(repo string) string {
			for _, r := range d.Config.Repositories {
				if strings.EqualFold(r.Name, repo) {
					return string(r.Preset())
				}
			}
			return ""
		},
		Branch: func(repo string) string {
			for _, r := range d.Config.Repositories {
				if strings.EqualFold(r.Name, repo) {
					return workflowBranch(r)
				}
			}
			return ""
		},
		Config: d.Config, Git: d.Git, Spec: d.Spec, Prepare: d.Prepare, NewID: NewID, Issues: issueSource(d.Issues), BuildDir: resolvedBuildDir(d.BuildDir),
		Topics: d.Topics, EditorDir: d.EditorDir, Shell: d.Shell, Environment: d.Environment, QueueStatus: queueStatus(d.Config),
	})
	// The publish path (D51): prepare when a run stops, publish on approval, and
	// complete what a restart interrupted. Without a way to push, none of it runs.
	if publish := PublishFor(d, svc, ws); publish != nil {
		service.NewPipeline(svc, ws, service.PipelineConfig{Publish: publish})
	} else {
		logf("publishing is off: a stopped run is not prepared and nothing is pushed (no pusher, committer or repository copies)")
	}
	var consoles *service.Consoles
	if d.ConsoleSpec != nil {
		consoles = service.NewConsoles(svc, service.ConsoleConfig{Spec: d.ConsoleSpec, Prepare: d.Prepare, EnsureImage: d.ConsoleImage, Dir: d.ConsoleDir, SSH: d.ConsoleSSH})
		defer consoles.CloseShells() // a shell does not outlive the supervisor
		defer consoles.CloseSSH()    // nor does an SSH connection
	}
	be := api.NewBackend(svc, ws, consoles)
	auth, err := web.NewTokenAuth(token, nil)
	if err != nil {
		return err
	}
	if origin, host := d.Config.PublicOrigin(); origin != "" {
		auth.SetPublicHost(host) // a request by the forwarder's name is https
	}
	// Passkeys (D45) are bound to whr's HTTPS name, so they need public_url.
	var keys *passkey.Service
	if origin, host := d.Config.PublicOrigin(); origin != "" {
		if keys, err = passkey.New(passkey.Config{
			RPID: host, Origin: origin, DisplayName: "workharbor", Now: d.Clock.Now,
			// the first passkey ends the sessions the API token started; a revoked
			// passkey ends its own (D45)
			OnFirstEnrolled: func() { auth.EndSessions(func(id string) bool { return id == "" }) },
			OnRevoked:       func(id string) { auth.EndSessions(func(p string) bool { return p == id }) },
		}, d.Store); err != nil {
			return err
		}
	} else {
		logf("passkeys are off: set public_url to whr's https name to enrol one (D45)")
	}
	// Previews (D33): a listener per preview, on the configured ports, over a
	// runtime that can reach an environment's ports.
	var previews *service.Previews
	if d.Config.Preview.On() {
		pcfg := service.PreviewConfig{FirstPort: d.Config.Preview.FirstPort, LastPort: d.Config.Preview.LastPort}
		if origin, host := d.Config.PublicOrigin(); origin != "" {
			pcfg.Scheme, pcfg.Host = "https", host
		}
		if up, ok := d.Runtime.(runtime.Previewer); ok {
			pcfg.Upstream = up
		} else {
			logf("previews are on, but the %s runtime cannot reach an environment's ports yet: opening one will say so (issue #72, #69)", d.Runtime.Name())
		}
		if previews, err = service.NewPreviews(svc, ws, pcfg); err != nil {
			return err
		}
		auth.OnSessionEnd(previews.CloseSession) // a preview ends with the session that opened it
		sweepCtx, stopSweep := context.WithCancel(ctx)
		swept := make(chan struct{})
		go func() { defer close(swept); previews.Manager().Run(sweepCtx, 0) }()
		defer func() { stopSweep(); <-swept }()
	}
	// Sessions that idle out or expire end on a clock of their own, not only when
	// their cookie comes back, so what they opened ends with them.
	sessCtx, stopSess := context.WithCancel(ctx)
	sessDone := make(chan struct{})
	go func() {
		defer close(sessDone)
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-sessCtx.Done():
				return
			case <-t.C:
				auth.Sweep()
			}
		}
	}()
	defer func() { stopSess(); <-sessDone }()
	apiOpt := api.Options{Token: token, Clients: clients, Store: d.Store, OnError: func(err error) { logf("api error: %v", err) }}
	webOpt := web.Options{Auth: auth, Store: d.Store, Setup: d.Setup, OnError: func(err error) { logf("web error: %v", err) }}
	webOpt.SetupOrigin, _ = d.Config.PublicOrigin()
	if previews != nil {
		apiOpt.Previews, webOpt.Previews = previews, previews
	}
	if keys != nil { // a typed nil would look like a configured interface
		apiOpt.Passkeys, webOpt.Passkeys = keys, keys
		webOpt.Changes = changesOf{st: d.Store, svc: svc, revoke: scfg.RevokeTokens != nil}
	}
	srv, err := api.New(be, apiOpt)
	if err != nil {
		return err
	}
	// Bind before the first reconcile: a wrong address is the quickest failure. The
	// API gets a private unix socket, the web UI the forwarded loopback address.
	if d.SocketPath == "" {
		return errors.New("serve: no socket path for the API")
	}
	apiLn, err := api.ListenSocket(d.SocketPath)
	if err != nil {
		return err
	}
	ln, err := api.Listen(d.Config.Listen)
	if err != nil {
		_ = apiLn.Close()
		return err
	}
	reconcile := svc.Reconcile
	if d.reconcile != nil {
		reconcile = d.reconcile
	}
	errLog := &reconcileErrors{logf: logf}
	rep, err := reconcile(ctx)
	if err != nil {
		_ = ln.Close()
		_ = apiLn.Close()
		return fmt.Errorf("reconcile at start: %w", err)
	}
	logf("reconciled: %d interrupted, %d awaiting a human, %d failed", len(rep.Interrupted), len(rep.AwaitingHuman), len(rep.Failed))
	errLog.note(rep.Errors)
	if d.Ready != nil {
		d.Ready(ln.Addr(), apiLn.Addr())
	}

	every := d.ReconcileEvery
	if every <= 0 {
		every = 30 * time.Second
	}
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				rep, err := reconcile(loopCtx)
				if err != nil {
					// A pass that failed or was cut short by the shutdown has a
					// partial report: never record it as the new error set.
					if loopCtx.Err() == nil {
						logf("reconcile: %v", err)
					}
					continue
				}
				errLog.note(rep.Errors)
			}
		}
	}()

	ui, err := web.New(be, webOpt)
	if err != nil {
		_ = ln.Close()
		_ = apiLn.Close()
		return err
	}
	// The web listener has no /v1 route: a request for it is the web UI's own 404.
	// The API answers only on the socket (D29, §7.5), so a leaked API token cannot
	// be used from the forwarded network.
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	errc := make(chan error, 2)
	go func() { errc <- srv.ServeHandler(serveCtx, apiLn, srv.Handler()) }()
	go func() { errc <- srv.ServeHandler(serveCtx, ln, ui.Handler()) }()
	err = <-errc
	cancelServe()
	<-errc
	stop()
	<-loopDone
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// serviceConfig is the service's configuration for d: the budgets and limits,
// then the board, the throttled notifier and the revoker.
func serviceConfig(d Deps, logf func(string, ...any)) service.Config {
	scfg := service.Config{
		ProjectInstructions: d.ProjectInstructions,
		SkillBinding:        d.SkillBinding,
		PrepareSkills:       d.PrepareSkills,
		SkillSet:            &d.Config.SkillSet,
		SkillForbidden:      d.Config.SkillStoreForbidden(),
		Owner:               d.Owner,
		PostCreateTimeout:   d.Config.Environment.PostCreate(),
		Budgets:             Budgets(d.Config.Budgets),
		LowLimits:           service.LowLimits{WindowPercent: d.Config.Limits.WarnPercent, BalanceMicroUSD: int64(math.Round(d.Config.Limits.LowBalanceUSD * 1e6))},
		Spec:                d.AgentSpec,
		NewID:               NewID,
		OnError:             func(err error) { logf("background error: %v", err) },
	}
	addBoard(&scfg, d)
	addNotifier(&scfg, d)
	addRevoker(&scfg, d)
	return scfg
}

// NewNotifier puts next behind one shared per-task Throttle, so every kind of
// push is deduplicated and rate-limited (design §9.4). The service puts the
// result behind its own bounded queue (service.New), which also reports
// delivery errors, so no second queue is added here. It returns nil for a nil
// next: nothing is sent.
func NewNotifier(next notify.Notifier) notify.Notifier {
	if next == nil {
		return nil
	}
	return notify.Throttled{Next: next, Throttle: &notify.Throttle{}}
}

// addNotifier gives the service the push notifier of design §9.4, behind the
// shared per-task throttle; without a notifier in Deps it stays nil.
func addNotifier(scfg *service.Config, d Deps) {
	if n := NewNotifier(d.Notifier); n != nil {
		scfg.Notifier = n
	}
}

// addBoard gives the service the project board of D30: the supervisor's own
// writes, through the autonomy table, with a link to the task on each card. The
// guard's pusher and verifier are for the push flow, not for this.
func addBoard(scfg *service.Config, d Deps) {
	b := d.Config.Board
	if b == nil || d.Forge.Guard == nil {
		return
	}
	scfg.Board = d.Forge.Guard(nil, nil)
	if b.PublicURL != "" {
		scfg.BoardLink = func(task domain.ID) string { return notify.Link(b.PublicURL, notify.Message{TaskID: task}) }
	}
}

// Budgets turns the configured budgets into the service's: dollars become
// millionths of a dollar, so no float reaches a comparison with a total.
func Budgets(b config.Budgets) service.Budgets {
	limit := func(l config.BudgetLimit) service.Limit {
		return service.Limit{MaxTokens: l.MaxTokens, MaxCostMicroUSD: int64(math.Round(l.MaxCostUSD * 1e6))}
	}
	return service.Budgets{PerRun: limit(b.PerRun), PerTask: limit(b.PerTask), SoftPercent: b.SoftPercent}
}

// addRevoker gives kill-all the forge's way to revoke the tokens it holds, when
// the forge adapter has one (the GitHub App client does).
func addRevoker(scfg *service.Config, d Deps) {
	if d.Forge.RevokeTokens != nil {
		scfg.RevokeTokens = d.Forge.RevokeTokens
	}
}

// applyWorkflows records the preset of each configured repository. A change of
// one is a policy change: unless the operator confirmed it on the host
// (--accept-workflow-change) it is not applied. With a passkey enrolled the
// supervisor then starts and returns the repositories it holds on their recorded
// workflow, each with a pending change the web UI can confirm with a step-up
// (D45, issue #107); without one it refuses to start, as it always did. A
// confirmed change is appended to the audit log of changes. A task already
// started keeps the preset it started under either way.
func applyWorkflows(ctx context.Context, d Deps, logf func(string, ...any)) (held map[string]store.WorkflowRecord, err error) {
	canConfirm, err := webCanConfirm(ctx, d)
	if err != nil {
		return nil, err
	}
	for _, r := range d.Config.Repositories {
		want := store.WorkflowRecord{Workflow: string(r.Preset()), Branch: workflowBranch(r)}
		prev, ok, err := d.Store.RecordedWorkflow(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		if ok && prev != want && !d.AcceptWorkflowChange {
			if !canConfirm {
				return nil, fmt.Errorf("the workflow of %s is %s on %q in the configuration and was %s on %q: that is a policy change; check it and start with --accept-workflow-change to confirm it, or enrol a passkey (whr passkey add) to confirm it in the web UI (tasks already started keep what they started under)", r.Name, want.Workflow, want.Branch, prev.Workflow, prev.Branch)
			}
			if _, err := d.Store.RaiseChange(ctx, string(NewID()), r.Name, prev, want, time.Now()); err != nil {
				return nil, err
			}
			if held == nil {
				held = map[string]store.WorkflowRecord{}
			}
			held[strings.ToLower(r.Name)] = prev
			logf("the workflow of %s is %s on %q in the configuration and stays %s on %q until you confirm it (web UI, with a passkey, or --accept-workflow-change)", r.Name, want.Workflow, want.Branch, prev.Workflow, prev.Branch)
			continue
		}
		by := "serve"
		if d.AcceptWorkflowChange {
			by = "host-cli"
		}
		if _, changed, err := d.Store.ApplyWorkflow(ctx, r.Name, want, by, time.Now()); err != nil {
			return nil, err
		} else if changed {
			logf("workflow of %s changed from %s on %q to %s on %q (confirmed on the host CLI)", r.Name, prev.Workflow, prev.Branch, want.Workflow, want.Branch)
		}
	}
	// What the configuration no longer asks for is withdrawn.
	keep := make([]string, 0, len(held))
	for r := range held {
		keep = append(keep, r)
	}
	if _, err := d.Store.WithdrawChangesExcept(ctx, keep, time.Now()); err != nil {
		return nil, err
	}
	return held, nil
}

// queueStatus is the board column whose cards ask for a run, or empty.
func queueStatus(c *config.Config) string {
	if c.Board == nil {
		return ""
	}
	return c.Board.QueueStatus
}

// withHeld returns the configuration with each held repository back on the
// workflow it was recorded under: a change nobody confirmed does not apply. The
// caller's configuration is left as it is.
func withHeld(c *config.Config, held map[string]store.WorkflowRecord) *config.Config {
	if len(held) == 0 {
		return c
	}
	cp := *c
	cp.Repositories = slices.Clone(c.Repositories)
	for i, r := range cp.Repositories {
		if rec, ok := held[strings.ToLower(r.Name)]; ok {
			cp.Repositories[i].Workflow, cp.Repositories[i].IntegrationBranch = rec.Workflow, rec.Branch
		}
	}
	return &cp
}

// webCanConfirm says whether the web UI could confirm a change: passkeys are on
// (public_url is set) and one is enrolled.
func webCanConfirm(ctx context.Context, d Deps) (bool, error) {
	if origin, _ := d.Config.PublicOrigin(); origin == "" {
		return false, nil
	}
	keys, err := d.Store.Passkeys(ctx)
	return len(keys) > 0, err
}

// workflowBranch is the integration branch a repository runs under: the one its
// preset writes to, or none for a published repository, whose pull requests go to
// the default branch.
func workflowBranch(r config.Repository) string {
	if r.Preset().ToDefaultBranch() {
		return ""
	}
	return r.Target("")
}

// issueSource keeps a nil *IssueAccess a nil interface: the service tests
// Issues for nil, and a typed nil would pass that test.
func issueSource(a *IssueAccess) service.IssueSource {
	if a == nil {
		return nil
	}
	return a
}

func resolvedBuildDir(dir string) string {
	if dir == "" {
		return GuestBuild
	}
	return dir
}
