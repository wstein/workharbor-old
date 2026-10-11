package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/agent/claude"
	"github.com/wstein/workharbor/internal/api"
	"github.com/wstein/workharbor/internal/baseimage"
	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/console"
	"github.com/wstein/workharbor/internal/devcontainer"
	"github.com/wstein/workharbor/internal/devcontainer/feature"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/forge"
	"github.com/wstein/workharbor/internal/forge/github"
	"github.com/wstein/workharbor/internal/hostgit"
	"github.com/wstein/workharbor/internal/notify"
	"github.com/wstein/workharbor/internal/oci"
	"github.com/wstein/workharbor/internal/policy"
	"github.com/wstein/workharbor/internal/redact"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/runtime/apple"
	"github.com/wstein/workharbor/internal/service"
	"github.com/wstein/workharbor/internal/skillset"
	"github.com/wstein/workharbor/internal/sshca"
	"github.com/wstein/workharbor/internal/store"
	"github.com/wstein/workharbor/internal/textsafe"
	"github.com/wstein/workharbor/internal/toolstore"
)

// Owner is the owner label of this supervisor's environments.
const Owner = "whr"

// GuestHome is the agent's home in the environment: a volume of its own, where
// the human signs in (D40).
const GuestHome = "/home/workharbor"

// AgentConfigDir is the agent CLI's auth directory in the environment
// (CLAUDE_CONFIG_DIR): a run and the sign-in shell both use it.
const AgentConfigDir = GuestHome + "/.claude"

// GuestBuild is where the build volume of an environment is mounted: the output
// directories of tools that read their location from the environment (cargo's
// target, uv's environment) live there, one directory per agent, instead of in the
// bind-mounted checkout where file metadata costs 20 to 100 times as much (D39).
const GuestBuild = "/var/whr/build"

// ToolsMount is where the tool store is mounted, read-only, in every environment.
const ToolsMount = "/tools"

func callPrefixes(f func() []string) []string {
	if f == nil {
		return nil
	}
	return f()
}

// SpecOptions is what an environment's spec is made from.
type SpecOptions struct {
	Backend   string
	Toolbox   string
	Owner     string
	Env       config.Environment // resolved
	ToolStore string             // the host directory of the tool store
	Proxy     string             // the host path of whr-proxy for linux/arm64
	// HostPrefixes lists the host's own global IPv6 prefixes for the sidecar to
	// refuse (design §7.2); nil means none.
	HostPrefixes func() []string
}

// For returns the spec of a workspace's environment: hardened, on an internal
// network of its own, with the tool store read-only, the agent home on a named
// volume of its own and the egress proxy as the only way out (design §5.1,
// §7.2). The workspace folder is added by the service, not here.
func (o SpecOptions) For(w domain.Workspace) runtime.Spec {
	e := o.Env
	s := runtime.Spec{
		Image: e.Image, Owner: o.Owner, CPUs: e.CPUs, MemoryMB: e.MemoryMB, DiskMB: e.DiskMB,
		Network:      runtime.Network{Name: o.Owner + "-net-" + string(w.ID), Internal: true},
		User:         "1000:1000",
		ReadOnlyRoot: true,
		CapDrop:      []string{"ALL"},
		Init:         true,
		Tmpfs:        []string{"/tmp", "/run"},
		Mounts: []runtime.Mount{
			{Kind: runtime.MountBind, Source: o.ToolStore, Target: ToolsMount, ReadOnly: true},
			{Kind: runtime.MountVolume, Source: o.Owner + "-home-" + string(w.ID), Target: GuestHome},
			{Kind: runtime.MountVolume, Source: o.Owner + "-build-" + string(w.ID), Target: GuestBuild},
		},
		Egress: &runtime.Egress{Image: e.Image, Proxy: o.Proxy, Allow: e.EgressAllow, DenyPrefixes: callPrefixes(o.HostPrefixes)},
	}
	if o.Backend == "docker" {
		s.Egress = nil // unsupported sidecar; internal network remains enforced
		s.Mounts = s.Mounts[:2]
		s.Mounts = append(s.Mounts, runtime.Mount{Kind: runtime.MountBind, Source: o.Toolbox, Target: "/opt/bundledock/toolbox", ReadOnly: true})
		s.Tmpfs = append(s.Tmpfs, DockerBuild)
	}
	return s
}

// AgentSpec returns the StartSpec of a run. In dontAsk mode the agent runs only
// the supervisor's own allowlist, which is required: with none, nothing would
// be allowed. In manual mode every prompt goes to the human (D26): the service
// supplies the approver and there is no allowlist.
func AgentSpec(mode agent.PermissionMode, allowed []string, auth agent.AuthMode) func(domain.Task, domain.Run) agent.StartSpec {
	if mode == "" {
		mode = agent.PermissionDontAsk
	}
	return func(domain.Task, domain.Run) agent.StartSpec {
		var tools []string
		if mode == agent.PermissionDontAsk {
			tools = append([]string(nil), allowed...)
		}
		return agent.StartSpec{
			Auth: auth, PermissionMode: mode, AllowedTools: tools,
			ApprovalTimeout: domain.DefaultApprovalTimeout,
			Prompt:          "continue",
			// A read-only root has no home: tools that want one (git, the
			// agent's own config lookups) use the agent home volume.
			Env: []string{"HOME=" + GuestHome},
		}
	}
}

// modeOf is the permission mode a repository's agent runs in: the human's
// override (agent_permission_mode) if there is one, else what the repository's
// workflow preset sets (D47): manual for published, dontAsk for the others.
func modeOf(c *config.Config, repo string) agent.PermissionMode {
	return modeFor(c, repo, "")
}

// modeFor is modeOf for a task that kept the preset it started under: that preset
// decides, not the repository's current one (D47).
func modeFor(c *config.Config, repo, taskWorkflow string) agent.PermissionMode {
	if c.AgentPermissionMode != "" {
		return agent.PermissionMode(c.AgentPermissionMode)
	}
	if taskWorkflow != "" {
		p, err := policy.ParsePreset(taskWorkflow)
		if err != nil {
			// The service refuses such a run before this is asked; if it is asked
			// anyway the answer is the strictest, never the repository's (§6, #256).
			return agent.PermissionMode(policy.Published.AgentMode())
		}
		return agent.PermissionMode(p.AgentMode())
	}
	for _, r := range c.Repositories {
		if strings.EqualFold(r.Name, repo) {
			return agent.PermissionMode(r.Preset().AgentMode())
		}
	}
	return agent.PermissionDontAsk
}

// needsAllowlist reports whether any repository runs in dontAsk mode, which
// cannot start without an allowlist.
func needsAllowlist(c *config.Config) bool {
	if len(c.Repositories) == 0 {
		return modeOf(c, "") == agent.PermissionDontAsk
	}
	for _, r := range c.Repositories {
		if modeOf(c, r.Name) == agent.PermissionDontAsk {
			return true
		}
	}
	return false
}

// AgentSpecFor returns the StartSpec factory for the configuration: the mode of a
// run is that of its repository's workflow, unless the human overrode it.
func AgentSpecFor(c *config.Config, auth agent.AuthMode) func(domain.Task, domain.Run) agent.StartSpec {
	return func(t domain.Task, r domain.Run) agent.StartSpec {
		spec := AgentSpec(modeFor(c, t.Repo, t.Workflow), c.AgentAllowedTools, auth)(t, r)
		if c.Runtime.IsDocker() {
			spec.Env = append(spec.Env, "PATH=/opt/bundledock/toolbox/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
		}
		return spec
	}
}

// checkToolStore verifies the tool store against the hashes recorded when its
// entries were made (issue #125) and refuses to start on a tool whose content, mode
// or shape is not what was installed: every environment mounts the store and runs
// what is in it. A check that could not be made, an entry from before the full hash
// was recorded, is logged and does not stop the start.
func checkToolStore(root string, logf func(string, ...any)) error {
	var severe []string
	for _, p := range (&toolstore.Store{Root: root}).Verify() {
		if p.Severe {
			severe = append(severe, p.String())
			continue
		}
		logf("tool store: %s", p)
	}
	if len(severe) > 0 {
		return fmt.Errorf("the tool store %s does not verify, so no environment would run it: %s (run `whr tools build` to make it again)", root, strings.Join(severe, "; "))
	}
	return nil
}

// toolProfile finds the tool store profile to use: the configured one, or the
// only one there is.
func toolProfile(c *config.Config) (string, error) {
	if c.ToolProfile != "" {
		if _, err := os.Stat(filepath.Join(c.Roots.ToolStore, "profiles", c.ToolProfile)); err != nil {
			return "", fmt.Errorf("tool_profile %q is not in the tool store: run `whr tools build`: %w", c.ToolProfile, err)
		}
		return c.ToolProfile, nil
	}
	entries, err := os.ReadDir(filepath.Join(c.Roots.ToolStore, "profiles"))
	if err != nil {
		return "", fmt.Errorf("the tool store has no profiles (run `whr tools build`): %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 1 {
		// Fedora and Ubuntu are glibc bases (D43): take the glibc build.
		if n, ok := toolstore.ProfileFor(names, toolstore.Glibc); ok {
			return n, nil
		}
	}
	if len(names) != 1 {
		return "", fmt.Errorf("the tool store has %d profiles %v: set tool_profile", len(names), names)
	}
	return names[0], nil
}

// Redactor returns the redactor the store applies before it writes (§5.4,
// threat model T9): the well-known token formats, plus the exact secrets this
// supervisor holds, so they are masked whatever their format: the API tokens and
// the values of the agent's API-key file (D40). A value too short to register
// is refused rather than silently left unredacted.
func Redactor(c *config.Config, agentEnv []string) (*redact.Redactor, error) {
	rd := redact.New()
	tok, err := api.TokenFromConfig(c)
	if err != nil {
		return nil, err
	}
	if !rd.Add(string(tok)) {
		return nil, fmt.Errorf("api_token_file: the token is shorter than %d characters", redact.MinSecretLength)
	}
	clients, err := api.ClientsFromConfig(c)
	if err != nil {
		return nil, err
	}
	for _, cl := range clients {
		if !rd.Add(string(cl.Token)) {
			return nil, fmt.Errorf("api_clients %q: the token is shorter than %d characters", cl.Name, redact.MinSecretLength)
		}
	}
	if c.GitHub.KeyFile != "" {
		key, err := config.ReadSecret(c.GitHub.KeyFile)
		if err != nil {
			return nil, err
		}
		if !rd.Add(string(key)) {
			return nil, fmt.Errorf("github.key_file: the key is shorter than %d characters", redact.MinSecretLength)
		}
	}
	if c.Console.SSHCAKeyFile != "" {
		ca, err := sshca.Load(c.Console.SSHCAKeyFile)
		if err != nil {
			return nil, fmt.Errorf("console.ssh_ca_key_file: %w", err)
		}
		ca.Register(rd)
	}
	for i, e := range agentEnv {
		_, v, _ := strings.Cut(e, "=")
		if v == "" { // AgentAPIKey refuses an empty value; other callers may still pass one
			continue
		}
		if !rd.Add(v) {
			return nil, fmt.Errorf("agent_api_key_env_file: the value of entry %d is shorter than %d characters", i+1, redact.MinSecretLength)
		}
	}
	return rd, nil
}

// StateDir returns the directory of the database.
func StateDir(c *config.Config, home string) string { return config.StateDirOf(c.StateDir, home) }

func unavailableProjectInstructions(context.Context, domain.Task, domain.Run) (service.ProjectInstructions, error) {
	return service.ProjectInstructions{}, fmt.Errorf("%w: reviewed immutable project policy input and operator review proof are not configured", agent.ErrUnsupported)
}

func unavailableSkillBinding(context.Context, []skillset.Binding) (skillset.Binding, error) {
	return skillset.Binding{}, fmt.Errorf("%w: exact native CLI artifact/schema, model/effort tuple and isolated external loading are not measured", agent.ErrUnsupported)
}

func unavailableSkillPreparation(context.Context, domain.Run, service.SkillMount) error {
	return fmt.Errorf("%w: selected external read-only mount capability and idle environment lifecycle integration are not established", agent.ErrUnsupported)
}

// Build makes the real dependencies of `whr serve` from the configuration:
// the database, hostgit, the Apple Container runtime, the Claude Code adapter
// and the spec factory. exe is the path of the running whr, which says where
// the installed egress proxy is. The returned function releases what was opened.
func Build(c *config.Config, exe, home string, logf func(string, ...any)) (Deps, func(), error) {
	if needsAllowlist(c) && len(c.AgentAllowedTools) == 0 {
		return Deps{}, nil, errors.New("agent_allowed_tools is needed: a repository runs its agent in the dontAsk mode, where only the tools you list may run (a published repository asks for each tool instead, and agent_permission_mode: manual does so for all, D26, D47)")
	}
	profile, err := toolProfile(c)
	if err != nil {
		return Deps{}, nil, err
	}
	if err := checkToolStore(c.Roots.ToolStore, logf); err != nil {
		return Deps{}, nil, err
	}
	bin := ToolsMount + "/profiles/" + profile + "/bin"
	rt, err := configuredRuntime(c, bin)
	if err != nil {
		return Deps{}, nil, err
	}
	imageBuilder, _ := rt.(baseimage.Builder)
	proxy := ""
	if !c.Runtime.IsDocker() {
		proxy, err = service.InstalledProxy(exe)
		if err != nil {
			return Deps{}, nil, err
		}
	}
	env, mode, err := service.AgentCredentials(c)
	if err != nil {
		return Deps{}, nil, err
	}

	dir := StateDir(c, home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Deps{}, nil, err
	}
	rd, err := Redactor(c, env)
	if err != nil {
		return Deps{}, nil, err
	}
	gh, err := newGitHub(c, rd)
	if err != nil {
		return Deps{}, nil, err
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "workharbor.db"), store.WithRedactor(rd))
	if err != nil {
		return Deps{}, nil, err
	}
	// The mirrors of the forge repositories and the supervisor's own repositories
	// live in the state directory, outside every workspace root; an editor copy
	// is never made in one (D42, §4.5).
	for _, d := range []string{"mirrors", "topics", EditorCopyDir} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			_ = st.Close()
			return Deps{}, nil, err
		}
	}
	gitOpts := []hostgit.Option{hostgit.WithCacheRoot(filepath.Join(dir, "mirrors"))}
	for _, root := range c.Roots.Workspaces {
		gitOpts = append(gitOpts, hostgit.WithWorkspaceRoot(root))
	}
	git, err := hostgit.New(gitOpts...)
	if err != nil {
		_ = st.Close()
		return Deps{}, nil, err
	}
	// Env files an earlier run left in the temp directory (an agent's API key could
	// be in one, from before the environment went through a pipe) go now.
	if gone := apple.SweepEnvFiles("", time.Now()); len(gone) > 0 {
		logf("removed %d env file(s) an earlier run left behind", len(gone))
	}
	ag := claude.New(rt, claude.Config{Bin: bin + "/claude", ConfigDir: AgentConfigDir, Env: env})

	spec := c.Environment.Resolved()
	if spec.Image == "" {
		// The workharbor base image (D44): built once, again only when the
		// pinned base changes. The first start takes a while.
		tag, built, err := baseimage.Ensure(context.Background(), imageBuilder, baseimage.Distro(spec.Base), filepath.Join(dir, "build"))
		if err != nil {
			_ = git.Close()
			_ = st.Close()
			return Deps{}, nil, err
		}
		if built {
			logf("built the base image %s", tag)
		}
		spec.Image = tag
	}
	hostPrefixes := func() []string { return HostIPv6Prefixes(logf) }
	opts := SpecOptions{Owner: Owner, Backend: c.Runtime.Backend, Toolbox: c.Runtime.Toolbox, Env: spec, ToolStore: c.Roots.ToolStore, Proxy: proxy, HostPrefixes: hostPrefixes}
	roots := append(append([]string(nil), c.Roots.Workspaces...), c.Roots.ToolStore)
	if proxy != "" {
		roots = append(roots, filepath.Dir(proxy))
	}
	if c.Runtime.IsDocker() {
		roots = append(roots, c.Runtime.Toolbox)
	}
	prepare := func(s runtime.Spec) (runtime.PreparedSpec, error) {
		return prepareRuntimeSpec(c, runtime.PrepareOptions{
			FS: runtime.OSFS{}, Home: home, Roots: roots,
			Owns: ownsVolumes(s),
		}, s)
	}
	consoleDistro := baseimage.Distro(spec.Base)
	if c.Console.Base != "" {
		consoleDistro = baseimage.Distro(c.Console.Base) // the console's own base, Alpine allowed (#93)
	}
	consoleTag, err := console.Tag(consoleDistro)
	if err != nil {
		_ = git.Close()
		_ = st.Close()
		return Deps{}, nil, err
	}
	consoleOpts := ConsoleOptions{Owner: Owner, Image: consoleTag, Console: c.Console.Resolved(), Roots: c.Roots.Workspaces, Proxy: proxy, HostPrefixes: hostPrefixes}
	ensureConsole := func(ctx context.Context) error {
		if imageBuilder == nil {
			return errors.New("runtime docker: console image builds are unsupported")
		}
		_, built, err := console.Ensure(ctx, imageBuilder, consoleDistro, filepath.Join(dir, "build"))
		if built {
			logf("built the console image %s", consoleTag)
		}
		return err
	}
	var consoleSSH *sshca.CA
	if c.Console.SSHCAKeyFile != "" {
		if consoleSSH, err = sshca.Load(c.Console.SSHCAKeyFile); err != nil {
			_ = git.Close()
			_ = st.Close()
			return Deps{}, nil, fmt.Errorf("console.ssh_ca_key_file: %w", err)
		}
	}
	return Deps{
		ProjectInstructions: unavailableProjectInstructions,
		SkillBinding:        unavailableSkillBinding,
		PrepareSkills:       unavailableSkillPreparation,
		Notifier:            ntfyNotifier(c),
		ConsoleSSH:          consoleSSH,
		SocketPath:          config.APISocketPath(c.StateDir, home), Config: c, Store: st, Runtime: rt, Agent: ag, Issues: NewIssueAccess(gh), Forge: NewForgeAccess(gh), Git: git, Owner: Owner,
		ConsoleSpec: consoleOpts.For, ConsoleImage: ensureConsole, ConsoleDir: consoleOpts.Dir,
		BuildDir: backendBuildDir(c),
		Environment: Environment(git, Topics(git, c, dir), devcontainer.Options{BaseImage: spec.Image, ToolchainImages: devcontainer.DefaultToolchainImages, Features: &feature.Resolver{Client: oci.New(oci.Config{})}}, imageBuilder, Owner, filepath.Join(dir, "build"), func(ctx context.Context, repo string) (map[string]string, error) {
			a, err := st.FeatureSources(ctx, repo)
			out := map[string]string{}
			for ref, ans := range a {
				if ans.Allowed {
					out[ref] = ans.Digest
				}
			}
			return out, err
		}),
		Topics: Topics(git, c, dir), EditorDir: filepath.Join(dir, EditorCopyDir),
		Shell:     &service.ShellConfig{Bin: bin, Dir: GuestHome, Env: []string{"HOME=" + GuestHome, "CLAUDE_CONFIG_DIR=" + AgentConfigDir}},
		NewPusher: func(r *hostgit.Repo) forge.Pusher { return gh.NewPusher(r) }, Committer: gh.BotIdentity,
		Spec: opts.For, Prepare: prepare, AgentSpec: AgentSpecFor(c, mode), Logf: redactedLogf(rd, logf),
		AgentSpecFor: func(held *config.Config) func(domain.Task, domain.Run) agent.StartSpec {
			return AgentSpecFor(held, mode)
		},
	}, func() {
		_ = git.Close()
		_ = st.Close()
	}, nil
}

// newGitHub builds the GitHub App client: the App's key is read with
// config.ReadSecret, and every installation token it mints is registered with
// the redactor (D31). It is scoped to the configured repositories.
func newGitHub(c *config.Config, rd *redact.Redactor) (*github.Client, error) {
	pemBytes, err := config.ReadSecret(c.GitHub.KeyFile)
	if err != nil {
		return nil, err
	}
	key, err := github.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("github.key_file: %w", err)
	}
	repos := make([]string, len(c.Repositories))
	for i, r := range c.Repositories {
		repos[i] = r.Name
	}
	gc := github.Config{AppID: c.GitHub.AppID, Key: key, Repos: repos, Redactor: rd, BaseURL: c.GitHub.APIURL}
	if b := c.Board; b != nil {
		gc.Board = &github.BoardConfig{Owner: b.Owner, Organization: b.Organization, Number: b.Number, StatusField: b.StatusField, SessionField: b.SessionField, LinkField: b.LinkField}
	}
	return github.New(gc)
}

// EditorCopyDir is the directory of the state directory that `whr open` makes
// its copies in.
const EditorCopyDir = "open"

// Topics opens, for a forge repository, the mirror (fed only from the forge)
// and the supervisor's own bare repository that an agent's branch is imported
// into (D42, §4.5). The mirror is fetched from the repository's public https
// address: an authenticated fetch of a private repository comes with the push
// flow (issue #27), so until then `whr open` on one fails at the fetch.
func Topics(git *hostgit.Git, c *config.Config, dir string) service.TopicsFunc {
	return func(ctx context.Context, repo string) (*hostgit.Repo, *hostgit.Cache, error) {
		mirror, err := git.CachePath(repo)
		if err != nil {
			return nil, nil, err
		}
		depth := 0
		for _, r := range c.Repositories {
			if strings.EqualFold(r.Name, repo) {
				depth = r.CloneDepth
			}
		}
		cache, err := git.OpenCache(ctx, mirror, hostgit.CacheConfig{Source: "https://github.com/" + repo + ".git", CloneDepth: depth})
		if err != nil {
			return nil, nil, err
		}
		own := filepath.Join(dir, "topics", filepath.Base(mirror))
		var topics *hostgit.Repo
		if _, serr := os.Stat(own); serr == nil {
			topics, err = git.OpenBare(ctx, own)
		} else {
			topics, err = git.InitBare(ctx, own)
		}
		if err != nil {
			return nil, nil, err
		}
		return topics, cache, nil
	}
}

// GuestConsoleHome is the console user's home: a volume of its own, kept across
// consoles, with the human's dotfiles and shell history. It holds no credential
// of workharbor's and none of the agents' (D43).
const GuestConsoleHome = "/home/workharbor"

// WorkspacesMount is where the workspace roots are mounted in the console.
const WorkspacesMount = "/workspaces"

// ConsoleOptions is what the console's spec is made from.
type ConsoleOptions struct {
	Owner   string
	Image   string         // the console image's tag
	Console config.Console // resolved
	Roots   []string       // the workspace roots, absolute and resolved
	Proxy   string         // the host path of whr-proxy for linux/arm64
	// HostPrefixes is as in SpecOptions.
	HostPrefixes func() []string
}

// rootName is the directory name a workspace root gets under WorkspacesMount.
// A second root with the same base name gets a numeric suffix, so two roots
// never share a target.
func rootNames(roots []string) []string {
	names := make([]string, len(roots))
	seen := map[string]int{}
	for i, r := range roots {
		base := filepath.Base(r)
		seen[base]++
		if n := seen[base]; n > 1 {
			base = fmt.Sprintf("%s-%d", base, n)
		}
		names[i] = base
	}
	return names
}

// dir returns where a workspace is in the console: its place in its root below
// WorkspacesMount. A workspace outside every root has none.
func (o ConsoleOptions) dir(w domain.Workspace) (string, bool) {
	names := rootNames(o.Roots)
	for i, root := range o.Roots {
		if rel, err := filepath.Rel(root, w.Path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return WorkspacesMount + "/" + names[i] + "/" + filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// Dir returns the directory a shell for a workspace starts in: the workspace's
// place in the console, or the workspaces' root for one outside every root.
func (o ConsoleOptions) Dir(w domain.Workspace) string {
	if dir, ok := o.dir(w); ok {
		return dir
	}
	return WorkspacesMount
}

// ownsVolumes says which named volumes a spec may mount: exactly the ones of its own
// environment, found from the network it names (owner-net-<workspace>): the home and
// build volumes of that workspace, or the console's home for the console. Another
// workspace's volume, the console's from a workspace spec and any other volume that
// merely starts with the owner's prefix are refused (a volume holds an agent's login).
func ownsVolumes(s runtime.Spec) func(string) bool {
	owner := Owner
	id, ok := strings.CutPrefix(s.Network.Name, owner+"-net-")
	if !ok || id == "" {
		return func(string) bool { return false }
	}
	allowed := map[string]bool{}
	if id == "console" {
		allowed[owner+"-console-home"] = true
	} else {
		allowed[owner+"-home-"+id] = true
		allowed[owner+"-build-"+id] = true
	}
	return func(volume string) bool { return allowed[volume] }
}

// For returns the console's spec: hardened like an agent's environment, on an
// internal network of its own behind the egress proxy, with every workspace root
// mounted read-only and each workspace in rw mounted read-write over its place in
// its root, and a home volume. It has no tool store, no agent and no
// credential (design D43, §7.4); the workspace's .git, which agents write, is
// reached through the git wrapper of the image.
func (o ConsoleOptions) For(rw []domain.Workspace) runtime.Spec {
	c := o.Console
	names := rootNames(o.Roots)
	mounts := make([]runtime.Mount, 0, len(o.Roots)+len(rw)+1)
	for i, root := range o.Roots {
		mounts = append(mounts, runtime.Mount{Kind: runtime.MountBind, Source: root, Target: WorkspacesMount + "/" + names[i], ReadOnly: true})
	}
	for _, w := range rw {
		if dir, ok := o.dir(w); ok {
			mounts = append(mounts, runtime.Mount{Kind: runtime.MountBind, Source: w.Path, Target: dir})
		}
	}
	mounts = append(mounts, runtime.Mount{Kind: runtime.MountVolume, Source: o.Owner + "-console-home", Target: GuestConsoleHome})
	return runtime.Spec{
		Image: o.Image, Owner: o.Owner, CPUs: c.CPUs, MemoryMB: c.MemoryMB, DiskMB: c.DiskMB,
		Network:      runtime.Network{Name: o.Owner + "-net-console", Internal: true},
		User:         "1000:1000",
		ReadOnlyRoot: true,
		CapDrop:      []string{"ALL"},
		Init:         true,
		Tmpfs:        []string{"/tmp", "/run"},
		Mounts:       mounts,
		Egress:       &runtime.Egress{Image: o.Image, Proxy: o.Proxy, Allow: c.EgressAllow, DenyPrefixes: callPrefixes(o.HostPrefixes)},
	}
}

// Environment reads a repository's environment from its default branch (design
// §4.2, D38): it refreshes the supervisor's own mirror of the integration branch
// and resolves the devcontainer.json, the toolchain files and the lockfiles of
// that commit there, never in a workspace. Its Image builds the repository's own
// image, once per commit and tag, from a context exported from that commit into a
// directory of the supervisor's own (design §5.1). A mirror that cannot be
// fetched is an error, which the caller reports; no host is allowed and nothing
// is run for what could not be read.
func Environment(git *hostgit.Git, topics service.TopicsFunc, opt devcontainer.Options, rt baseimage.Builder, owner, workDir string, approved func(ctx context.Context, repo string) (map[string]string, error)) func(ctx context.Context, repo, branch string) (service.RepoEnvironment, error) {
	return func(ctx context.Context, repo, branch string) (service.RepoEnvironment, error) {
		_, cache, err := topics(ctx, repo)
		if err != nil {
			return service.RepoEnvironment{}, err
		}
		if err := cache.Refresh(ctx, branch); err != nil {
			return service.RepoEnvironment{}, fmt.Errorf("fetch %s: %w", repo, err)
		}
		mirror, err := git.OpenBare(ctx, cache.Path())
		if err != nil {
			return service.RepoEnvironment{}, err
		}
		// the feature sources the human allowed for this repository, each at the digest
		// the human saw: a tag that was moved is asked about again
		if approved != nil && opt.Features != nil {
			ok, err := approved(ctx, repo)
			if err != nil {
				return service.RepoEnvironment{}, err
			}
			r := *opt.Features
			r.Approved = func(ref, digest string) bool { d, found := ok[ref]; return found && digest != "" && d == digest }
			opt.Features = &r
		}
		env, err := devcontainer.Resolve(ctx, mirror, "refs/heads/"+branch, opt)
		if err != nil {
			return service.RepoEnvironment{}, err
		}
		re := service.RepoEnvironment{Environment: env}
		if env.Origin != devcontainer.OriginDefault {
			re.Image = func(ctx context.Context) (string, error) {
				if !env.Built() {
					return env.Image, nil
				}
				if rt == nil {
					return "", errors.New("selected runtime does not support repository image builds")
				}
				tag := env.Tag(owner)
				if have, err := rt.HasImage(ctx, tag); err != nil {
					return "", err
				} else if have {
					return tag, nil
				}
				if err := os.MkdirAll(workDir, 0o700); err != nil {
					return "", err
				}
				dir, err := os.MkdirTemp(workDir, "devcontainer-")
				if err != nil {
					return "", err
				}
				defer func() { _ = os.RemoveAll(dir) }()
				build := func(b runtime.BuildSpec) error {
					if out, err := rt.Build(ctx, b); err != nil {
						if len(out) > 2000 {
							out = out[len(out)-2000:]
						}
						return fmt.Errorf("%w\n%s", err, out)
					}
					return nil
				}
				b, _, err := env.Stage(ctx, mirror, owner, dir)
				if err != nil {
					return "", err
				}
				if !env.FeaturesOnDockerfile() {
					return tag, build(b)
				}
				// features on a Dockerfile: the repository's image first, then the
				// features on top of it, the second build from the first's tag
				if have, err := rt.HasImage(ctx, b.Tag); err != nil {
					return "", err
				} else if !have {
					if err := build(b); err != nil {
						return "", err
					}
				}
				top, _, err := env.StageFeatures(owner, filepath.Join(dir, "features"), b.Tag)
				if err != nil {
					return "", err
				}
				return tag, build(top)
			}
		}
		return re, nil
	}
}

// fileSecrets is the credential service of the ntfy channel: each secret is a
// file the operator named (0600, outside every root, checked by the
// configuration). The value never reaches a log.
type fileSecrets map[string]string

// Get reads the named secret, or "" when no file is configured for it.
func (f fileSecrets) Get(name string) (string, error) {
	path := f[name]
	if path == "" {
		return "", nil
	}
	b, err := config.ReadSecret(path)
	if err != nil {
		return "", errors.New("the secret file cannot be read")
	}
	return strings.TrimSpace(string(b)), nil
}

// publicBase is the address a push's link opens.
func publicBase(c *config.Config) string {
	origin, _ := c.PublicOrigin()
	return origin
}

// ntfyNotifier is the ntfy notifier the configuration asks for, or nil without
// an ntfy block.
func ntfyNotifier(c *config.Config) notify.Notifier {
	n := c.Ntfy
	if n == nil {
		return nil
	}
	return notify.Ntfy{Server: n.Server, Secrets: fileSecrets{notify.SecretTopic: n.TopicFile, notify.SecretToken: n.TokenFile}, BaseURL: publicBase(c)}
}

// redactedLogf masks the supervisor's known secrets in every line it logs and
// escapes control characters, so text an agent produced (in an error) can
// neither forge a log line nor drive the terminal.
func redactedLogf(rd *redact.Redactor, logf func(string, ...any)) func(string, ...any) {
	return func(format string, args ...any) {
		logf("%s", textsafe.Escape(rd.String(fmt.Sprintf(format, args...))))
	}
}
