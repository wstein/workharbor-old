package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/devcontainer"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/forge"
	"github.com/wstein/workharbor/internal/hostgit"
	"github.com/wstein/workharbor/internal/initiation"
	"github.com/wstein/workharbor/internal/policy"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/skillset"
	"github.com/wstein/workharbor/internal/store"
)

// WorkspaceMount is where a workspace folder is mounted in its environment.
const WorkspaceMount = "/ws"

// CloneDir is the agent clone's directory inside a workspace folder.
const CloneDir = "repo"

// WorkspaceConfig is what the workspace operations need besides the service.
type WorkspaceConfig struct {
	// Config says where workspaces may live (CheckWorkspacePath).
	Config *config.Config
	// Git seeds the agent clone. It is the only time the host runs git in it.
	Git *hostgit.Git
	// Spec returns the environment's spec for a workspace: image, limits, user,
	// network and egress. The service adds the workspace folder as the bind
	// mount at WorkspaceMount; nothing else comes from the workspace.
	Spec func(w domain.Workspace) runtime.Spec
	// Prepare is runtime.Prepare with this host's filesystem, roots and
	// volume ownership.
	Prepare func(runtime.Spec) (runtime.PreparedSpec, error)
	// NewID returns a fresh ID.
	NewID func() domain.ID
	// Issues loads an issue from the forge. Optional: Run needs it.
	Issues IssueSource
	// Topics and EditorDir make `whr open` work: Topics opens the supervisor's
	// own repository and the mirror of a forge repository, and EditorDir is the
	// directory the editor copies are made in, outside every workspace root.
	// Optional.
	Topics    TopicsFunc
	EditorDir string
	// BuildDir is where the environment's build volume is mounted in the guest.
	// When set, each agent's tools put their output there (buildEnv), per agent,
	// off the bind-mounted checkout (D39). Empty: nothing is relocated.
	BuildDir string
	// Shell says where the agent's CLI and home are, for the agent shell (design
	// §7.3, "The sign-in shell"). Optional: without it there is no agent shell.
	Shell *ShellConfig
	// Environment reads the repository's environment from its default branch, in
	// the supervisor's own copy, never from a checkout (D38): the hosts its
	// devcontainer.json requests and its lockfiles suggest are asked about at a
	// run's start. Optional: without it nothing is asked.
	Environment func(ctx context.Context, repo, branch string) (RepoEnvironment, error)
	// Workflow returns the preset a repository runs under (D47); a task keeps the
	// one it started under. Nil means the default preset.
	Workflow func(repo string) string
	// Branch returns the integration branch a repository runs under; a task keeps
	// the one it started with (D47). Nil or empty means none.
	Branch func(repo string) string
	// Trust is the trust tier of issue #53: it says whether an issue may start
	// a run. Nil allows every issue, today. It is the place to add the tiers,
	// not a way around them.
	Trust func(forge.Issue) error
	// QueueStatus names the board column whose cards ask for a run (D30, D40, issue
	// #71): a card there never starts one, it raises an "Accept this task?" Decision.
	// Empty turns it off.
	QueueStatus string
}

// Workspaces creates workspaces and their agents, and starts tasks on agents
// (design D42, issue #90).
type Workspaces struct {
	svc *Service
	cfg WorkspaceConfig
	// pipe continues an answer into the publish path (D51); nil without one.
	pipe *Pipeline

	// mu orders a run's save and a rebuild's start (Rebuild; the registry of
	// rebuilds is the service's): a run is not saved in a workspace being
	// rebuilt, and a rebuild is not started in a workspace that has an unfinished
	// run, under the same lock, so the two cannot pass each other's check.
	mu sync.Mutex

	// failures remembers, per card, the last error reported by the queue poll and its
	// backoff (guarded by failMu), so a failing card is reported once, not every poll,
	// and is skipped for a growing number of polls. polls counts the polls read so far
	// and lastCard is the card handled last, where the next poll starts (round robin).
	failMu   sync.Mutex
	failures map[string]*cardFailure
	polls    uint64
	lastCard string
}

// NewWorkspaces returns the workspace operations of a service.
func NewWorkspaces(s *Service, cfg WorkspaceConfig) *Workspaces { return &Workspaces{svc: s, cfg: cfg} }

// CreateRequest describes a workspace and its first agent.
type CreateRequest struct {
	Name string
	Path string // an empty folder below a workspace root
	Repo string // owner/name
	// Integration is the branch the agents rebase onto. Empty means the
	// repository's publication target; a different one is refused (issue #208).
	Integration string
	// Source is where the agent clone is seeded from: an absolute path of the
	// human's repository or an https URL. Empty means the forge's URL of Repo.
	// The source's .git is read by `git clone` and never mounted.
	Source       string
	Role         string // the first agent
	Instructions string
	Profile      string
}

// Create makes a workspace: it checks the folder, seeds the agent clone, records
// the workspace, provisions and starts its environment and adds the first
// agent with its worktree, which git makes inside the environment, never on the
// host. If a step fails what was made is taken back.
func (w *Workspaces) Create(ctx context.Context, req CreateRequest) (domain.Workspace, domain.Agent, error) {
	now := w.svc.clock.Now()
	path, err := w.cfg.Config.CheckWorkspacePath(req.Path)
	if err != nil {
		return domain.Workspace{}, domain.Agent{}, &domain.InvalidError{Msg: err.Error()}
	}
	integration, branchFrom, err := w.integrationFor(ctx, req.Repo, req.Integration)
	if err != nil {
		return domain.Workspace{}, domain.Agent{}, err
	}
	ws, wev, err := domain.NewWorkspace(w.cfg.NewID(), req.Name, path, req.Repo, integration, now)
	if err != nil {
		// The branch is the last check of NewWorkspace, so this refusal is about
		// the name: say where the name came from, which the human can change.
		var inv *domain.InvalidError
		if errors.As(err, &inv) && strings.HasPrefix(inv.Msg, "integration branch") {
			err = &domain.InvalidError{Msg: inv.Msg + " (taken from " + branchFrom + "; relaxing the names is #229)"}
		}
		return domain.Workspace{}, domain.Agent{}, err
	}
	ag, aev, err := domain.NewAgent(w.cfg.NewID(), ws.ID, req.Role, req.Instructions, req.Profile, now)
	if err != nil {
		return domain.Workspace{}, domain.Agent{}, err
	}
	origin := "https://github.com/" + ws.Repo + ".git"
	source := req.Source
	if source == "" {
		source = origin
	}

	clone := filepath.Join(path, CloneDir)
	if err := w.cfg.Git.SeedAgentClone(ctx, clone, source, ws.Integration, origin); err != nil {
		return domain.Workspace{}, domain.Agent{}, err
	}
	var undo []func()
	fail := func(err error) (domain.Workspace, domain.Agent, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return domain.Workspace{}, domain.Agent{}, err
	}
	undo = append(undo, func() { _ = os.RemoveAll(clone) })

	if err := w.svc.store.AddWorkspace(ctx, ws, wev); err != nil {
		return fail(err)
	}
	undo = append(undo, func() {
		_ = w.svc.store.RemoveWorkspace(context.WithoutCancel(ctx), ws, domain.RemovedWorkspaceEvent(ws, w.svc.clock.Now()))
	})

	env, err := w.provision(ctx, ws)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() {
		bg := context.WithoutCancel(ctx)
		// A volume outlives its environment (design §4.4), so the home volume
		// this call made is removed here, or a failed Create would leak it (found
		// by the serve integration run).
		res, _ := w.svc.rt.Resources(bg, env)
		_ = w.svc.rt.Stop(bg, env)
		_ = w.svc.rt.Delete(bg, env)
		for _, v := range res.Volumes {
			_ = w.svc.rt.RemoveVolume(bg, v)
		}
	})
	if err := w.svc.store.SetWorkspaceEnv(ctx, ws.ID, domain.ID(env)); err != nil {
		return fail(err)
	}
	ws.EnvID = domain.ID(env)

	if err := w.svc.store.AddAgent(ctx, ag, aev); err != nil {
		return fail(err)
	}
	if err := w.addWorktree(ctx, ws, ag); err != nil {
		_ = w.svc.store.RemoveAgent(context.WithoutCancel(ctx), ag, domain.RemovedAgentEvent(ag, w.svc.clock.Now()))
		return fail(err)
	}
	return ws, ag, nil
}

// provision makes the workspace's environment, with the folder mounted
// read-write, and starts it.
func (w *Workspaces) provision(ctx context.Context, ws domain.Workspace) (string, error) {
	prep, err := w.prepareSpec(ctx, ws, "")
	if err != nil {
		return "", err
	}
	return w.bringUp(ctx, prep, false)
}

// prepareSpec builds and checks the spec of the workspace's environment: the
// image the repository's default branch resolves to now (built if it needs
// building, which can take minutes), the folder mounted read-write, and the egress
// hosts the repository has been allowed. netSuffix is added to the network's name,
// for a second environment that exists beside the first for a moment (a rebuild).
func (w *Workspaces) prepareSpec(ctx context.Context, ws domain.Workspace, netSuffix string) (runtime.PreparedSpec, error) {
	spec := w.cfg.Spec(ws)
	spec.Network.Name += netSuffix
	spec.Mounts = append(spec.Mounts, runtime.Mount{Kind: runtime.MountBind, Source: ws.Path, Target: WorkspaceMount})
	// A repository with its own environment (a devcontainer.json or a Dockerfile)
	// runs its own image, built from the default branch, with its containerEnv;
	// everything else of the spec stays the supervisor's (D38). A repository that
	// cannot be read gets the supervisor's default (reported), but one whose image
	// cannot be built is refused: it asked for an environment it cannot have.
	if re, ok := w.repoEnvironment(ctx, ws); ok && re.Origin != devcontainer.OriginDefault {
		image := re.Environment.Image
		if re.Image != nil {
			var err error
			if image, err = re.Image(ctx); err != nil {
				return runtime.PreparedSpec{}, fmt.Errorf("build the environment of %s: %w", ws.Repo, err)
			}
		}
		var err error
		if spec, err = re.Spec(spec, image); err != nil {
			return runtime.PreparedSpec{}, fmt.Errorf("the environment of %s: %w", ws.Repo, err)
		}
	}
	if spec.Egress != nil {
		// A repository's allowed hosts are in the sidecar from the start, so no
		// other workspace of it is asked again.
		allowed, err := w.svc.EgressAllow(ctx, ws.Repo)
		if err != nil {
			return runtime.PreparedSpec{}, err
		}
		egress := *spec.Egress
		egress.Allow = unionHosts(egress.Allow, allowed)
		spec.Egress = &egress
	}
	return w.cfg.Prepare(spec)
}

// bringUp provisions, starts and checks the environment of a prepared spec. A
// failure takes back what it made; the volumes it made go too, unless
// keepVolumes says they were already there (a rebuild reuses the home and build
// volumes, which hold the agents' work).
func (w *Workspaces) bringUp(ctx context.Context, prep runtime.PreparedSpec, keepVolumes bool) (string, error) {
	env, err := w.svc.rt.Provision(ctx, prep)
	if err != nil {
		return "", err
	}
	if err := w.svc.startEnv(ctx, env); err != nil {
		bg := context.WithoutCancel(ctx)
		_ = w.svc.rt.Delete(bg, env)
		return "", fmt.Errorf("start environment %s: %w", env, err)
	}
	if err := w.svc.waitReady(ctx, domain.ID(env)); err != nil {
		bg := context.WithoutCancel(ctx)
		_ = w.svc.rt.Stop(bg, env)
		_ = w.svc.rt.Delete(bg, env)
		return "", err
	}
	if err := w.checkGit(ctx, env, prep.Spec().Image); err != nil {
		// Like Create's own undo: a volume outlives its environment (§4.4).
		bg := context.WithoutCancel(ctx)
		res, _ := w.svc.rt.Resources(bg, env)
		_ = w.svc.rt.Stop(bg, env)
		_ = w.svc.rt.Delete(bg, env)
		if !keepVolumes {
			for _, v := range res.Volumes {
				_ = w.svc.rt.RemoveVolume(bg, v)
			}
		}
		return "", err
	}
	return env, nil
}

// checkGit refuses an environment whose image has no git (design D44). The
// agents' worktrees and the bundle export run git inside the environment, so an
// image without it would fail later in `git worktree add` with an exec error;
// this says what is missing, when the workspace is created. The image may be
// the workharbor base image, which has git, or a repository's own.
func (w *Workspaces) checkGit(ctx context.Context, env, image string) error {
	out, code, err := w.svc.exec(ctx, env, runtime.ExecRequest{Cmd: []string{"git", "--version"}, Env: gitEnv()})
	if err != nil {
		return fmt.Errorf("check git in environment %s: %w", env, err)
	}
	if code == 0 {
		return nil
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return &domain.InvalidError{Msg: fmt.Sprintf(
		"the environment image %q has no git (`git --version` exited %d: %q): agents need git for their worktrees and for the bundle export, so use an image that has it, or leave environment.image unset for the workharbor base image (D44)",
		image, code, strings.TrimSpace(out))}
}

// buildEnv points the output directories of the tools that read them from the
// environment to the agent's own directory on the build volume (D39): cargo's
// target and uv's project environment. The paths lie outside the checkout, so no
// mount point is made in it, and they are per agent, so two agents of a workspace
// do not share a virtual environment of different branches. Tools that cannot be
// moved this way (npm's node_modules) are not touched here.
func (w *Workspaces) buildEnv(a domain.Agent) []string {
	if w.cfg.BuildDir == "" {
		return nil
	}
	dir := path.Join(w.cfg.BuildDir, a.Role)
	return []string{
		"CARGO_TARGET_DIR=" + path.Join(dir, "cargo-target"),
		"UV_PROJECT_ENVIRONMENT=" + path.Join(dir, "venv"),
	}
}

// noteRepoSize counts the files the agent's checkout tracks and records it in
// the task's events, with a warning above RepoSizeWarnFiles (D39). It runs in the
// environment, because the host never runs git in a workspace, and it never
// fails a start: a count that cannot be made is reported and the run goes on.
func (w *Workspaces) noteRepoSize(ctx context.Context, task, run domain.ID, ws domain.Workspace, a domain.Agent) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, code, err := w.svc.exec(cctx, string(ws.EnvID), runtime.ExecRequest{
		Cmd: []string{"sh", "-c", `git -C "$0" ls-files -z | tr -cd '\000' | wc -c`, a.Worktree},
		Env: gitEnv(),
	})
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out))
	}
	var files int
	if err == nil {
		if files, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
			err = fmt.Errorf("the count %q is not a number", strings.TrimSpace(out))
		}
	}
	if err != nil {
		w.svc.report(fmt.Errorf("count the tracked files of %s: %w", a.Worktree, err))
		return
	}
	ev, err := domain.NewRepoSizeEvent(task, run, files, w.svc.clock.Now())
	if err != nil {
		w.svc.report(err)
		return
	}
	saved, err := w.svc.store.Append(ctx, ev)
	if err != nil {
		w.svc.report(fmt.Errorf("record the size of %s: %w", a.Worktree, err))
		return
	}
	w.svc.publish(saved)
}

// addWorktree makes the agent's worktree and branch inside the environment. The
// host never runs git in a workspace's clone after it was seeded (§4.5).
func (w *Workspaces) addWorktree(ctx context.Context, ws domain.Workspace, a domain.Agent) error {
	clone := WorkspaceMount + "/" + CloneDir
	out, code, err := w.svc.exec(ctx, string(ws.EnvID), runtime.ExecRequest{
		Cmd: []string{"git", "-C", clone, "worktree", "add", "-b", a.Branch, a.Worktree, ws.Integration},
		Env: gitEnv(),
	})
	if err != nil {
		return fmt.Errorf("add worktree for agent %s: %w", a.Role, err)
	}
	if code != 0 {
		return fmt.Errorf("add worktree for agent %s: git exited %d: %s", a.Role, code, strings.TrimSpace(out))
	}
	if w.cfg.BuildDir != "" {
		// The agent's directory on the build volume, so a tool that does not make
		// parent directories finds it. The volume is the agent's user's own.
		dir := path.Join(w.cfg.BuildDir, a.Role)
		if out, code, err := w.svc.exec(ctx, string(ws.EnvID), runtime.ExecRequest{Cmd: []string{"mkdir", "-p", dir}}); err != nil || code != 0 {
			if err == nil {
				err = fmt.Errorf("mkdir exited %d: %s", code, strings.TrimSpace(out))
			}
			return fmt.Errorf("make the build directory of agent %s: %w", a.Role, err)
		}
	}
	return nil
}

// AddAgent adds a named agent to a workspace: a record, and its worktree and
// branch inside the environment, which must be running.
func (w *Workspaces) AddAgent(ctx context.Context, workspace, role, instructions, profile string) (domain.Agent, error) {
	ws, err := w.svc.store.Workspace(ctx, workspace)
	if err != nil {
		return domain.Agent{}, err
	}
	if ws.EnvID == "" {
		return domain.Agent{}, domain.NewConflict(domain.RuleEnvRunning, "workspace %s has no environment", ws.Name)
	}
	release, err := w.svc.leaseEnvironment(ws)
	if err != nil {
		return domain.Agent{}, err
	}
	defer release()
	a, ev, err := domain.NewAgent(w.cfg.NewID(), ws.ID, role, instructions, profile, w.svc.clock.Now())
	if err != nil {
		return domain.Agent{}, err
	}
	if err := w.svc.store.AddAgent(ctx, a, ev); err != nil {
		return domain.Agent{}, err
	}
	if err := w.addWorktree(ctx, ws, a); err != nil {
		_ = w.svc.store.RemoveAgent(context.WithoutCancel(ctx), a, domain.RemovedAgentEvent(a, w.svc.clock.Now()))
		return domain.Agent{}, err
	}
	return a, nil
}

// StartRequest starts a task on an agent.
type StartRequest struct {
	AgentID domain.ID
	Issue   string // the issue the task works on
	Prompt  string // the first message to the agent
}

// StartTask creates a task assigned to an agent and starts a run of it in the
// workspace's environment. The environment holds one live run at most
// (design §4.3): the check and the save are one transaction, so two starts
// cannot both pass it. The agent starts in its worktree, under the launcher
// that can cancel its process group (D25), with the session recorded by the
// run. A start that stops half-way leaves a starting run with no session, which
// the reconciler fails into a retry-or-cancel Decision.
func (w *Workspaces) StartTask(ctx context.Context, req StartRequest) (domain.ID, domain.ID, error) {
	if !initiation.Valid(ctx) {
		return "", "", initiation.ErrNotInitiated
	}
	a, ws, err := w.agentAndWorkspace(ctx, req.AgentID)
	if err != nil {
		return "", "", err
	}
	if err := w.ensureEnvironment(ctx, ws); err != nil {
		return "", "", err
	}
	task, run := w.cfg.NewID(), w.cfg.NewID()
	agg := domain.NewTaskAggregate(domain.Task{
		ID: task, Repo: ws.Repo, Issue: req.Issue, State: domain.TaskQueued, AgentID: a.ID, Workflow: w.workflowOf(ws.Repo), Branch: w.branchOf(ws.Repo), CreatedAt: w.svc.clock.Now(),
	})
	agg.AddEnvironment(domain.Environment{ID: ws.EnvID, Backend: w.svc.rt.Name(), State: domain.EnvRunning})
	if err := w.launch(ctx, agg, ws, a, run, req.Prompt); err != nil {
		return "", "", err
	}
	return task, run, nil
}

// agentAndWorkspace loads an agent and its workspace, which must have an
// environment.
func (w *Workspaces) agentAndWorkspace(ctx context.Context, agentID domain.ID) (domain.Agent, domain.Workspace, error) {
	a, err := w.svc.store.Agent(ctx, agentID)
	if err != nil {
		return domain.Agent{}, domain.Workspace{}, err
	}
	ws, err := w.svc.store.Workspace(ctx, string(a.WorkspaceID))
	if err != nil {
		return domain.Agent{}, domain.Workspace{}, err
	}
	if ws.EnvID == "" {
		return domain.Agent{}, domain.Workspace{}, domain.NewConflict(domain.RuleEnvRunning, "workspace %s has no environment", ws.Name)
	}
	return a, ws, nil
}

// ensureEnvironment starts the workspace's environment if it is not running and
// waits until exec answers. It is the first step of every new run's start, so an
// environment this process did not start is stopped and started again first (an
// agent an earlier process left behind may still run in it; design §4.1, "No
// surviving agent before a relaunch", issue #216), after the check that no run
// owns it. A failed stop fails the start and nothing is launched.
func (w *Workspaces) ensureEnvironment(ctx context.Context, ws domain.Workspace) error {
	release, err := w.svc.leaseEnvironment(ws)
	if err != nil {
		return err
	}
	defer release()
	if !w.svc.envStarted(ws.EnvID) {
		if err := w.svc.checkEnvFree(ctx, ws.EnvID, ""); err != nil {
			return err
		}
		if err := w.svc.restartLeftover(ctx, ws.EnvID); err != nil {
			return err
		}
		return w.svc.waitReady(ctx, ws.EnvID)
	}
	info, err := w.svc.rt.Inspect(ctx, string(ws.EnvID))
	if err != nil {
		return err
	}
	if info.State != domain.EnvRunning {
		if err := w.svc.startEnv(ctx, string(ws.EnvID)); err != nil {
			return fmt.Errorf("start environment %s: %w", ws.EnvID, err)
		}
	}
	return w.svc.waitReady(ctx, ws.EnvID)
}

// saveStartingRun saves the task with its new run, if the environment is free and
// is not being rebuilt: the check and the save are one change under the lock a
// rebuild takes, so a rebuild never starts while a run is being saved, and a run is
// never saved while one is under way.
func (w *Workspaces) saveStartingRun(ctx context.Context, ws domain.Workspace, agg *domain.TaskAggregate) ([]domain.Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.svc.rebuilding(ws.ID) {
		return nil, domain.NewConflict(domain.RuleEnvRunning, "workspace %s is being rebuilt: start the task when it is done", ws.Name)
	}
	// The record is read again under the lock: a start that loaded it before a
	// rebuild switched the environment must not save its run against the old one.
	cur, err := w.svc.store.Workspace(ctx, string(ws.ID))
	if err != nil {
		return nil, err
	}
	if cur.EnvID != ws.EnvID {
		return nil, domain.NewConflict(domain.RuleEnvRunning, "the environment of workspace %s changed (rebuilt) while the task was starting: start it again", ws.Name)
	}
	var saved []domain.Event
	err = w.svc.store.Update(ctx, func(tx *store.Tx) error {
		owning, err := tx.UnfinishedRuns(ctx, ws.EnvID)
		if err != nil {
			return err
		}
		if err := domain.CheckEnvironmentFree(ws.EnvID, owning); err != nil {
			return err
		}
		if err := w.svc.checkNotHeld(ws.EnvID); err != nil { // read after the run check: see HoldEnvironment
			return err
		}
		agg.StampRunBounds(w.svc.clock.Now())
		saved, err = tx.SaveTask(ctx, agg)
		return err
	})
	return saved, err
}

// launch starts a run on a task aggregate that has none live: the run is added,
// saved with the one-run check in one transaction, and the agent is started in
// its worktree and attached. If the agent cannot be started the run fails into
// a retry-or-cancel Decision, so the environment is free again.
func (w *Workspaces) launch(ctx context.Context, agg *domain.TaskAggregate, ws domain.Workspace, a domain.Agent, run domain.ID, prompt string) error {
	r := domain.Run{ID: run, WorkspaceID: ws.ID, AgentID: a.ID, EnvID: ws.EnvID}
	initialRun := r
	initialRun.TaskID, initialRun.State = agg.Task().ID, domain.RunStarting
	initial := w.svc.cfg.Spec(agg.Task(), initialRun)
	if prompt != "" {
		initial.Prompt = prompt
	}
	selection, _, err := w.svc.composeSkills(ctx, agg.Task(), r, &initial, true)
	if err != nil {
		return err
	}
	r.Skills = selection
	if err := agg.StartRun(r); err != nil {
		return err
	}
	task := agg.Task().ID
	sl, err := w.svc.begin(run) // taken before the run is visible, so the reconciler does not take it for lost
	if err != nil {
		return err
	}
	saved, err := w.saveStartingRun(ctx, ws, agg)
	if err != nil {
		w.svc.end(run, sl)
		return err
	}
	w.svc.publish(saved)

	env, _ := w.repoEnvironment(ctx, ws)
	start := func(ctx context.Context) error { return w.startAgent(ctx, task, run, ws, a, prompt, sl, env) }
	waiting, err := w.gateEgress(ctx, task, run, ws, env, start, sl)
	if err != nil {
		w.svc.end(run, sl)
		var rep Report
		return errors.Join(err, w.svc.failRun(context.WithoutCancel(ctx), task, run, &rep))
	}
	if waiting {
		return nil // the run stays starting; the answers start the agent
	}
	// The start runs on its own context and is tracked (startDetached): the
	// request may end, and Cancel may stop it. The request waits for it a while,
	// so a start that is quick returns with the agent running, and a slow one
	// (a long postCreate) returns with the run starting.
	job := w.svc.startDetached(ctx, run, start)
	if job == nil {
		w.svc.end(run, sl)
		return errors.New("the supervisor is shutting down")
	}
	select {
	case <-job.done:
		if job.err != nil && !errors.Is(job.err, context.Canceled) {
			return job.err // the run has failed into a Decision, as before
		}
	case <-ctx.Done():
		w.svc.reportWhenDone(run, job)
	case <-time.After(w.svc.startWait()):
		w.svc.reportWhenDone(run, job)
	}
	return nil
}

// startWait is how long StartTask waits for the agent to start before it returns
// with the run still starting.
func (s *Service) startWait() time.Duration {
	if s.cfg.StartWait > 0 {
		return s.cfg.StartWait
	}
	return 30 * time.Second
}

// repoEnvironment reads the repository's environment from its default branch,
// in the supervisor's own copy (D38): the devcontainer file, the Dockerfile,
// postCreateCommand and the egress requests, never from the integration branch,
// where an approved agent commit could configure the agent's next environment
// (design §6). A repository that cannot be read, or whose default branch the forge
// cannot name, yields the zero environment and the failure is reported: nothing it
// would have requested is allowed, and nothing it would have run is run.
func (w *Workspaces) repoEnvironment(ctx context.Context, ws domain.Workspace) (RepoEnvironment, bool) {
	if w.cfg.Environment == nil {
		return RepoEnvironment{}, false
	}
	db, ok := w.cfg.Issues.(forge.DefaultBrancher)
	if !ok {
		w.svc.report(fmt.Errorf("the environment of %s: the forge adapter cannot name the default branch, so it is not read", ws.Repo))
		return RepoEnvironment{}, false
	}
	def, err := db.DefaultBranchName(ctx, ws.Repo)
	if err != nil {
		w.svc.report(fmt.Errorf("the environment of %s: the default branch could not be read: %w", ws.Repo, err))
		return RepoEnvironment{}, false
	}
	env, err := w.cfg.Environment(ctx, ws.Repo, def)
	if err != nil {
		w.svc.report(fmt.Errorf("the environment of %s: %w", ws.Repo, err))
		return RepoEnvironment{}, false
	}
	return env, true
}

// gateEgress asks the human about the hosts the repository requests or suggests
// and has not answered yet, and about the feature sources it uses outside the allowed
// one (design §4.2): one blocking approval per host or reference, raised
// for the run, which stays starting. It returns true when it did, and the agent
// starts when the last request is answered (continueEgress). A repository that
// cannot be read right now does not stop the run: it starts with the hosts
// already allowed, and the failure is reported, since no host is ever allowed
// by what could not be read. A task that cannot be read does stop it: its
// preset decides which old answers still count, so none is trusted (#226).
func (w *Workspaces) gateEgress(ctx context.Context, task, run domain.ID, ws domain.Workspace, env RepoEnvironment, start func(context.Context) error, sl *slot) (bool, error) {
	if env.Commit == "" {
		return false, nil // nothing was read
	}
	agg, err := w.svc.loadTask(ctx, task) // the preset the task started under
	if err != nil {
		return false, err // fail closed: old answers are not trusted on a preset not known
	}
	preset, err := taskPreset(agg.Task())
	if err != nil {
		return false, err // fail closed: an unreadable preset is not "not published" (§6, #256)
	}
	published := preset == policy.Published
	pending, err := w.svc.PendingEgress(ctx, ws.Repo, env.Environment, published)
	if err != nil {
		return false, err
	}
	feats, err := w.svc.PendingFeatureSources(ctx, ws.Repo, env.Environment)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 && len(feats) == 0 {
		return false, nil
	}
	w.svc.holdForEgress(run, &egressWait{task: task, finish: start, sl: sl}) // before the requests are visible, so an answer finds it
	if _, err := w.svc.RequestEgress(ctx, task, run, pending); err != nil {
		w.svc.takeEgressWait(run)
		return false, err
	}
	if err := w.svc.RequestFeatureSources(ctx, task, run, feats); err != nil {
		// the host requests are raised already: the run fails and supersedes them
		w.svc.takeEgressWait(run)
		return false, err
	}
	return true, nil
}

// applyEgress makes the environment's proxy sidecar allow what the supervisor's
// configuration allows and the human allowed for the repository, if it does not
// already (design §4.2). It runs only before an agent process starts, because the
// sidecar's address changes. A runtime that cannot change the allowlist leaves it.
func (w *Workspaces) applyEgress(ctx context.Context, ws domain.Workspace) error {
	up, ok := w.svc.rt.(runtime.EgressUpdater)
	if !ok {
		return nil
	}
	spec := w.cfg.Spec(ws)
	if spec.Egress == nil {
		return nil
	}
	allowed, err := w.svc.EgressAllow(ctx, ws.Repo)
	if err != nil {
		return err
	}
	want := unionHosts(spec.Egress.Allow, allowed)
	info, err := w.svc.rt.Inspect(ctx, string(ws.EnvID))
	if err != nil {
		return err
	}
	if slices.Equal(info.EgressAllow, want) {
		return nil
	}
	egress := *spec.Egress
	egress.Allow = want
	spec.Egress = &egress
	// The spec names the environment's own network, as the runtime says it is.
	res, err := w.svc.rt.Resources(ctx, string(ws.EnvID))
	if err != nil {
		return err
	}
	spec.Network.Name = res.Network
	spec.Mounts = append(spec.Mounts, runtime.Mount{Kind: runtime.MountBind, Source: ws.Path, Target: WorkspaceMount})
	prep, err := w.cfg.Prepare(spec)
	if err != nil {
		return err
	}
	return up.UpdateEgress(ctx, string(ws.EnvID), prep)
}

// unionHosts returns the sorted union of two host lists without duplicates.
func unionHosts(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range append(append([]string(nil), a...), b...) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// startAgent starts the agent of a run that is starting, after the allowlist was
// brought up to date. It is the second half of launch: the run is saved, and the
// egress requests, if any, are answered.
func (w *Workspaces) startAgent(ctx context.Context, task, run domain.ID, ws domain.Workspace, a domain.Agent, prompt string, sl *slot, env RepoEnvironment) error {
	if err := w.svc.executionAdmission(ctx, task, run); err != nil {
		w.svc.end(run, sl)
		return err
	}
	agg, err := w.svc.store.LoadTask(ctx, task)
	if err != nil {
		w.svc.end(run, sl)
		return err
	}
	r, ok := agg.Run(run)
	if !ok || r.State != domain.RunStarting {
		w.svc.end(run, sl) // cancelled while it waited
		return nil
	}
	if err := w.applyEgress(ctx, ws); err != nil {
		return w.abortStart(ctx, task, run, sl, fmt.Errorf("apply the egress allowlist: %w", err))
	}
	if err := taskPresetReadable(agg.Task()); err != nil {
		return w.abortStart(ctx, task, run, sl, err)
	}
	spec := w.svc.cfg.Spec(agg.Task(), r)
	w.svc.fillApprover(&spec, task, run)
	spec.EnvID, spec.Workdir = string(ws.EnvID), a.Worktree
	if prompt != "" {
		spec.Prompt = prompt
	}
	_, mount, err := w.svc.composeSkills(ctx, agg.Task(), r, &spec, false)
	if err != nil {
		return w.abortStart(ctx, task, run, sl, err)
	}
	if r.Skills.Mode != "legacy" && r.Skills.InstructionSHA256 != skillset.Digest([]byte(spec.Prompt)) {
		return w.abortStart(ctx, task, run, sl, errors.New("the initial composed instruction changed after its provenance was recorded"))
	}
	if err := w.svc.prepareSkills(ctx, r, mount); err != nil {
		return w.abortStart(ctx, task, run, sl, err)
	}
	spec.Env = append(spec.Env, w.svc.agentEnv(ctx, ws.EnvID)...)
	spec.Env = append(spec.Env, w.buildEnv(a)...)
	w.noteRepoSize(ctx, task, run, ws, a)
	if err := w.postCreateWithin(ctx, ws, a, spec.Env, env); err != nil {
		return w.abortStart(ctx, task, run, sl, err)
	}
	// The session outlives this start (the agent runs for hours): it gets a
	// context that is never cancelled by Cancel of the start; Shutdown and a stop
	// end it. A cancel that came while the agent was starting stops it at once.
	if err := w.svc.executionAdmission(ctx, task, run); err != nil {
		w.svc.end(run, sl)
		return err
	}
	sess, err := w.svc.ag.Start(initiation.ForRun(context.WithoutCancel(ctx), task, run), spec)
	if err != nil {
		return w.abortStart(ctx, task, run, sl, err)
	}
	if ctx.Err() != nil {
		_ = sess.Stop(context.WithoutCancel(ctx))
		w.svc.end(run, sl)
		return ctx.Err()
	}
	if err := w.svc.update(ctx, task, func(t *domain.TaskAggregate) error { return t.MarkRunning(run) }); err != nil {
		_ = sess.Stop(context.WithoutCancel(ctx))
		w.svc.end(run, sl)
		return err
	}
	if h := w.svc.testBeforeAttach; h != nil {
		h(run) // a test seam: the interleaving of a Cancel with the end of the start
	}
	// A Cancel that came after the check above and after the run was marked
	// running stops the session here, or, if it comes later still, through the
	// slot's stopRequested, which attach honours.
	if ctx.Err() != nil {
		_ = sess.Stop(context.WithoutCancel(ctx))
		w.svc.end(run, sl)
		return ctx.Err()
	}
	w.svc.attach(task, run, sl, sess)
	return nil
}

// NewRun starts a new run on an existing task (design §5.3): rework after
// changes were requested, and after the answers of the failed-run and
// rebase-conflict questions. It checks the one-run rule like StartTask. The
// agent starts with the briefing of D27 for a new run; notes are untrusted data
// the caller wants in it, such as the paths of a rebase conflict.
func (w *Workspaces) NewRun(ctx context.Context, task domain.ID, prompt, notes string) (domain.ID, error) {
	if !initiation.Valid(ctx) {
		return "", initiation.ErrNotInitiated
	}
	agg, err := w.svc.store.LoadTask(ctx, task)
	if err != nil {
		return "", err
	}
	if agg.Task().AgentID == "" {
		return "", domain.NewConflict(domain.RuleRunAgent, "task %s has no agent: it was made before agents existed", task)
	}
	a, ws, err := w.agentAndWorkspace(ctx, agg.Task().AgentID)
	if err != nil {
		return "", err
	}
	if err := w.ensureEnvironment(ctx, ws); err != nil {
		return "", err
	}
	if env, ok := agg.Environment(ws.EnvID); !ok {
		agg.AddEnvironment(domain.Environment{ID: ws.EnvID, Backend: w.svc.rt.Name(), State: domain.EnvRunning})
	} else if env.State != domain.EnvRunning {
		if err := agg.ObserveEnv(ws.EnvID, domain.EnvRunning); err != nil {
			return "", err
		}
	}
	run := w.cfg.NewID()
	next := domain.Run{ID: run, AgentID: a.ID, WorkspaceID: ws.ID}
	if err := w.launch(ctx, agg, ws, a, run, NewRunBriefing(agg.Task(), next, prompt, notes)); err != nil {
		return "", err
	}
	return run, nil
}

// exec runs a command in an environment and returns its combined output and
// exit code.
func (s *Service) exec(ctx context.Context, env string, req runtime.ExecRequest) (string, int, error) {
	st, err := s.rt.Exec(ctx, env, req)
	if err != nil {
		return "", 0, err
	}
	stdout, stderr, code, err := runtime.Collect(st)
	return string(stdout) + string(stderr), code, err
}

// RebaseError is a rebase that conflicts: the conflict rule, git's report and
// the conflicting paths, which are the agent's data and untrusted.
type RebaseError struct {
	conflict *domain.ConflictError
	Paths    []string
}

func (e *RebaseError) Error() string { return e.conflict.Error() }

// Unwrap lets errors.As find the domain conflict (rule rebase-conflict).
func (e *RebaseError) Unwrap() error { return e.conflict }

// Rebase rebases an agent's branch onto the workspace's integration branch,
// inside the environment (design §4.5). It is refused while the agent has a
// starting or running run, which is editing the worktree. On a conflict the
// conflicting paths are read, the rebase is aborted, so the worktree is as it
// was, and a *RebaseError is returned: the export turns it into a Decision on
// the task's stopped run (design §4.2); a rebase asked for directly raises
// none.
func (w *Workspaces) Rebase(ctx context.Context, agentID domain.ID) error {
	a, err := w.svc.store.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	ws, err := w.svc.store.Workspace(ctx, string(a.WorkspaceID))
	if err != nil {
		return err
	}
	if ws.EnvID == "" {
		return domain.NewConflict(domain.RuleEnvRunning, "workspace %s has no environment", ws.Name)
	}
	release, err := w.svc.leaseEnvironment(ws)
	if err != nil {
		return err
	}
	defer release()
	live, err := w.svc.store.LiveRuns(ctx, ws.EnvID)
	if err != nil {
		return err
	}
	for _, r := range live {
		if r.AgentID == a.ID && (r.State == domain.RunStarting || r.State == domain.RunRunning) {
			return domain.NewConflict(domain.RuleAgentActive, "agent %s has run %s (%s): pause or finish it before a rebase", a.Role, r.ID, r.State)
		}
	}
	env := gitEnv("GIT_COMMITTER_NAME=workharbor agent "+a.Role, "GIT_COMMITTER_EMAIL=agent@workharbor.invalid") // a rebase writes commits; the authors are kept
	out, code, err := w.svc.exec(ctx, string(ws.EnvID), runtime.ExecRequest{
		Cmd: []string{"git", "-C", a.Worktree, "rebase", ws.Integration}, Env: env,
	})
	if err != nil {
		return fmt.Errorf("rebase agent %s: %w", a.Role, err)
	}
	if code == 0 {
		return nil
	}
	bg := context.WithoutCancel(ctx)
	var paths []string
	if list, lcode, lerr := w.svc.exec(bg, string(ws.EnvID), runtime.ExecRequest{
		Cmd: []string{"git", "-C", a.Worktree, "diff", "--name-only", "--diff-filter=U"}, Env: env,
	}); lerr == nil && lcode == 0 {
		for _, p := range strings.Split(strings.TrimSpace(list), "\n") {
			if p != "" {
				paths = append(paths, p)
			}
		}
	}
	// Leave the worktree as it was; a failed abort is reported with the conflict.
	_, acode, aerr := w.svc.exec(bg, string(ws.EnvID), runtime.ExecRequest{
		Cmd: []string{"git", "-C", a.Worktree, "rebase", "--abort"}, Env: env,
	})
	msg := fmt.Sprintf("agent/%s does not rebase onto %s: %s", a.Role, ws.Integration, strings.TrimSpace(out))
	if aerr != nil || acode != 0 {
		msg += fmt.Sprintf(" (and `rebase --abort` failed: exit %d, %v)", acode, aerr)
	}
	return &RebaseError{conflict: domain.NewConflict(domain.RuleRebase, "%s", msg), Paths: paths}
}

// gitEnv is the environment of a git command in the guest: the clone belongs to
// the host user, the guest runs as another, so safe.directory is set for it. A
// checkout's .git is written by agents, so what it configures is hostile input to
// the supervisor's own git commands as it is to the host's (hostgit): hooks and
// fsmonitor are off and the ext transport, which runs a command, is refused. The
// settings go through GIT_CONFIG_COUNT, which wins over every file.
func gitEnv(extra ...string) []string {
	return append([]string{
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null",
		"GIT_CONFIG_KEY_2=core.fsmonitor", "GIT_CONFIG_VALUE_2=false",
		"GIT_CONFIG_KEY_3=protocol.ext.allow", "GIT_CONFIG_VALUE_3=never",
	}, extra...)
}

// RemoveAgent removes an agent record. It is refused (store.RuleInUse) while a
// task that is not finished is assigned to it. The worktree and the branch stay
// in the clone, so no unpushed work is lost; the human removes the workspace
// folder when they are done with it.
func (w *Workspaces) RemoveAgent(ctx context.Context, workspace, role string) error {
	ws, err := w.svc.store.Workspace(ctx, workspace)
	if err != nil {
		return err
	}
	a, err := w.svc.store.AgentByRole(ctx, ws.ID, role)
	if err != nil {
		return err
	}
	if err := w.svc.store.RemoveAgent(ctx, a, domain.RemovedAgentEvent(a, w.svc.clock.Now())); err != nil {
		return err
	}
	w.clearBuildDir(ctx, ws, a)
	return nil
}

// clearBuildDir empties the agent's directory on the build volume, so a role that
// is added again later does not inherit the old one's output (a virtual
// environment of another branch's dependencies). The record is already gone, so a
// failure, such as a stopped environment, is reported and not returned: the
// directory is then cleared by the next RemoveAgent of that role, or stays until
// the workspace goes.
func (w *Workspaces) clearBuildDir(ctx context.Context, ws domain.Workspace, a domain.Agent) {
	if w.cfg.BuildDir == "" || ws.EnvID == "" {
		return
	}
	dir := path.Join(w.cfg.BuildDir, a.Role)
	// The role is a checked name, but this runs rm: refuse anything that is not a
	// direct child of the build directory.
	if path.Dir(dir) != path.Clean(w.cfg.BuildDir) || a.Role == "" || a.Role == "." || a.Role == ".." {
		w.svc.report(fmt.Errorf("not clearing %q: it is not a directory of an agent below %s", dir, w.cfg.BuildDir))
		return
	}
	out, code, err := w.svc.exec(ctx, string(ws.EnvID), runtime.ExecRequest{Cmd: []string{"rm", "-rf", "--", dir}})
	if err == nil && code != 0 {
		err = fmt.Errorf("rm exited %d: %s", code, strings.TrimSpace(out))
	}
	if err != nil {
		w.svc.report(fmt.Errorf("clear the build directory of agent %s: %w", a.Role, err))
	}
}

// Remove removes a workspace: its environment, with its network, sidecar and
// home volume, and its record. It is refused while the workspace has agents
// (remove them first). The folder and the clone in it are the human's and stay.
func (w *Workspaces) Remove(ctx context.Context, workspace string) error {
	ws, err := w.svc.store.Workspace(ctx, workspace)
	if err != nil {
		return err
	}
	agents, err := w.svc.store.Agents(ctx, ws.ID)
	if err != nil {
		return err
	}
	if len(agents) > 0 {
		return domain.NewConflict(store.RuleInUse, "workspace %s still has %d agent(s): remove them first", ws.Name, len(agents))
	}
	if ws.EnvID != "" {
		env := string(ws.EnvID)
		res, _ := w.svc.rt.Resources(ctx, env)
		if err := w.svc.rt.Stop(ctx, env); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return fmt.Errorf("stop environment %s: %w", env, err)
		}
		if err := w.svc.rt.Delete(ctx, env); err != nil {
			return fmt.Errorf("delete environment %s: %w", env, err)
		}
		for _, v := range res.Volumes {
			if err := w.svc.rt.RemoveVolume(ctx, v); err != nil {
				return fmt.Errorf("remove volume %s: %w", v, err)
			}
		}
	}
	return w.svc.store.RemoveWorkspace(ctx, ws, domain.RemovedWorkspaceEvent(ws, w.svc.clock.Now()))
}

// integrationFor returns the branch a new workspace rebases onto: the
// repository's publication target (the integration branch of the configuration,
// develop for integration when none is set, the forge's default branch read now
// for published), so the workspace and its tasks' commits meet on one branch. A
// requested branch that differs is refused, naming both; a target that cannot
// be determined refuses too, and no name is guessed (issue #208). The second
// result names where the target came from, for a refusal of the name.
func (w *Workspaces) integrationFor(ctx context.Context, repo, requested string) (string, string, error) {
	preset, err := policy.ParsePreset(w.workflowOf(repo))
	if err != nil {
		return "", "", &domain.InvalidError{Msg: fmt.Sprintf("the workflow of %s is not known: %v", repo, err)}
	}
	var target, source string
	switch {
	case preset.ToDefaultBranch():
		db, ok := w.cfg.Issues.(forge.DefaultBrancher)
		if !ok {
			return "", "", &domain.InvalidError{Msg: fmt.Sprintf("%s is published, and the forge adapter cannot name its default branch, so the workspace's branch is not known", repo)}
		}
		target, err = db.DefaultBranchName(ctx, repo)
		switch {
		case err != nil:
			return "", "", &domain.InvalidError{Msg: fmt.Sprintf("the default branch of %s could not be read, so the workspace's branch is not known: %v", repo, err)}
		case target == "":
			return "", "", &domain.InvalidError{Msg: fmt.Sprintf("the forge reported no default branch for %s, so the workspace's branch is not known", repo)}
		}
		source = "the forge's default branch of " + repo
	case w.branchOf(repo) != "":
		target, source = w.branchOf(repo), "integration_branch in the configuration"
	case preset == policy.Integration:
		target, source = "develop", "the integration preset's default"
	default:
		return "", "", &domain.InvalidError{Msg: fmt.Sprintf("%s has no integration branch in the configuration, so the workspace's branch is not known", repo)}
	}
	if requested != "" && requested != target {
		return "", "", &domain.InvalidError{Msg: fmt.Sprintf("the branch %q is not the publication target of %s, which is %q: leave --branch out or set integration_branch in the configuration", requested, repo, target)}
	}
	return target, source, nil
}

// branchOf is the integration branch a task started now would keep.
func (w *Workspaces) branchOf(repo string) string {
	if w.cfg.Branch == nil {
		return ""
	}
	return w.cfg.Branch(repo)
}

// workflowOf is the preset a task started now would keep.
func (w *Workspaces) workflowOf(repo string) string {
	if w.cfg.Workflow == nil {
		return string(policy.DefaultPreset)
	}
	if p := w.cfg.Workflow(repo); p != "" {
		return p
	}
	return string(policy.DefaultPreset)
}

// abortStart ends a run whose start failed: its slot is freed and, unless the
// start was cancelled (the task was cancelled and the run stopped), the run
// fails into a retry-or-cancel Decision. The error is returned for the report.
func (w *Workspaces) abortStart(ctx context.Context, task, run domain.ID, sl *slot, cause error) error {
	w.svc.end(run, sl)
	if errors.Is(cause, initiation.ErrNotInitiated) {
		return errors.Join(cause, w.svc.update(context.WithoutCancel(ctx), task, func(a *domain.TaskAggregate) error { return a.Interrupt(run) }))
	}
	if ctx.Err() != nil {
		return ctx.Err() // cancelled: nothing to fail
	}
	var rep Report
	if err := w.svc.failRun(context.WithoutCancel(ctx), task, run, &rep); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// postCreateWithin runs postCreate for at most the configured time (default ten
// minutes). A command still running then fails the run with that reason, and a
// cancel of the start stops it.
func (w *Workspaces) postCreateWithin(ctx context.Context, ws domain.Workspace, a domain.Agent, env []string, re RepoEnvironment) error {
	limit := w.svc.cfg.PostCreateTimeout
	if limit <= 0 {
		limit = 10 * time.Minute
	}
	pctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := w.postCreate(pctx, ws, a, env, re)
	if err != nil && ctx.Err() == nil && errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("the post-create command did not finish within %s", limit)
	}
	return err
}

// postCreate runs the repository's postCreateCommand inside the environment, as
// the agent's user, in the agent's worktree and with the agent's environment
// (the egress proxy among it), once per environment and command set: a marker in
// the agent home records it, so a later run does not run it again, and a
// changed command runs again. It runs after the allowlist is up to date and
// before the agent starts, so what it fetches goes through the hosts the human
// allowed. A command that fails ends the run as failed, with the tail of its
// output (the repository's own text, untrusted).
func (w *Workspaces) postCreate(ctx context.Context, ws domain.Workspace, a domain.Agent, env []string, re RepoEnvironment) error {
	cmds := re.PostCreate()
	if len(cmds) == 0 {
		return nil
	}
	raw, err := json.Marshal(cmds)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	marker := `"$HOME/.whr-post-create-` + hex.EncodeToString(sum[:8]) + `"`
	if _, code, err := w.svc.exec(ctx, string(ws.EnvID), runtime.ExecRequest{Cmd: []string{"sh", "-c", "test -f " + marker}, Env: env}); err != nil {
		return fmt.Errorf("check the post-create marker: %w", err)
	} else if code == 0 {
		return nil
	}
	for _, c := range cmds {
		c.Env, c.Dir = env, a.Worktree
		out, code, err := w.svc.exec(ctx, string(ws.EnvID), c)
		if err != nil {
			return fmt.Errorf("post-create command %q: %w", strings.Join(c.Cmd, " "), err)
		}
		if code != 0 {
			if len(out) > 400 {
				out = out[len(out)-400:]
			}
			return fmt.Errorf("post-create command %q exited %d: %s", strings.Join(c.Cmd, " "), code, strings.TrimSpace(out))
		}
	}
	out, code, err := w.svc.exec(ctx, string(ws.EnvID), runtime.ExecRequest{Cmd: []string{"sh", "-c", "touch " + marker}, Env: env})
	if err != nil {
		return fmt.Errorf("record that post-create ran: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("record that post-create ran: exited %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}
