// Package config reads the minimal configuration file `whr serve` needs
// (design D34, §13). Paths to secrets are files, never values: a token written
// in the config would end up in a backup, a log or a screenshot. The file is
// JSON, decoded strictly, so an unknown key or a misspelt one is an error and
// not silently ignored.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/wstein/workharbor/internal/baseimage"
	"github.com/wstein/workharbor/internal/credcheck"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/hostgit"
	"github.com/wstein/workharbor/internal/notify"
	"github.com/wstein/workharbor/internal/policy"
	"github.com/wstein/workharbor/internal/skillset"
)

// Repository is one repository workharbor works on.
type Repository struct {
	Name       string `json:"name"`        // "owner/name"
	CloneDepth int    `json:"clone_depth"` // 0 keeps the full history (design §4.5)
	// Workflow is the repository's preset: prototype, integration (the default) or
	// published (design D47). It is set here and nowhere else: nothing in the
	// repository chooses or reads it.
	Workflow string `json:"workflow,omitempty"`
	// IntegrationBranch is where approved commits go in the prototype and
	// integration workflows. Default: develop for integration. A prototype must
	// name one, and it must not be the repository's default branch, which the
	// supervisor never moves (D47): the configuration cannot know the default
	// branch, so whr doctor and the Guard check that. Published has none.
	IntegrationBranch string `json:"integration_branch,omitempty"`
	// Check is the command that checks this repository's work before a review
	// is asked for (D51): the supervisor's own choice, which wins over the
	// repository's `customizations.workharbor.check` and its
	// `.pre-commit-config.yaml`. It runs in the task's environment, in a
	// worktree at the prepared commit. Optional.
	Check string `json:"check,omitempty"`
	// CommitLint names the built-in linter that checks commit messages on the
	// host (D51): "conventional" (the default) or "workharbor", the rules of
	// this repository's internal/commitlint. A repository's own linter is its
	// code and runs only inside its check.
	CommitLint string `json:"commit_lint,omitempty"`
}

// Commit message linters a repository's commit_lint may name.
const (
	CommitLintConventional = "conventional"
	CommitLintWorkharbor   = "workharbor"
)

// MaxCheck bounds a configured check command.
const MaxCheck = 4096

// Linter returns the repository's commit_lint, the default when unset.
func (r Repository) Linter() string {
	if r.CommitLint == "" {
		return CommitLintConventional
	}
	return r.CommitLint
}

// Preset returns the repository's workflow. The configuration check has already
// refused an unknown one.
func (r Repository) Preset() policy.Preset {
	p, err := policy.ParsePreset(r.Workflow)
	if err != nil {
		return policy.DefaultPreset
	}
	return p
}

// Target is the branch approved commits go to, given the repository's default
// branch: the default branch for a published repository's pull request, else the
// integration branch (develop for integration when none is set). A prototype with
// none has no target, and the configuration check refuses it.
func (r Repository) Target(defaultBranch string) string {
	p := r.Preset()
	switch {
	case p.ToDefaultBranch():
		return defaultBranch
	case r.IntegrationBranch != "":
		return r.IntegrationBranch
	case p == policy.Prototype:
		return ""
	}
	return "develop"
}

// Roots are the directories workharbor owns on the host.
type Roots struct {
	// Workspaces are the folders workspaces may live under (D42): on the
	// internal disk or an external SSD. An agent writes them, so nothing
	// workharbor trusts lives in or contains one, and none is inside a git
	// repository of the human's.
	Workspaces []string `json:"workspaces"`
	ToolStore  string   `json:"tool_store"` // the shared read-only tool store (§5.6)
}

// Ntfy names the ntfy server and the files holding its topic and token.
type Ntfy struct {
	// Server is the base URL, https://ntfy.sh by default.
	Server string `json:"server,omitempty"`
	// TopicFile holds the long random topic, TokenFile an optional access token.
	TopicFile string `json:"topic_file"`
	TokenFile string `json:"token_file,omitempty"`
}

// Board names a GitHub project board (Projects v2) the supervisor writes (D30).
// Agents never write to it.
type Board struct {
	// Owner is the login of the user or organization that owns the project, and
	// Number the project's number in its URL.
	Owner  string `json:"owner"`
	Number int    `json:"number"`
	// Organization says the project belongs to an organization. A user-owned
	// project may not accept an App's token at all (unverified, issue #70).
	Organization bool `json:"organization,omitempty"`
	// StatusField, SessionField and LinkField name the project's fields; the
	// defaults are "Status", "Session" and "Task". A field the project does not
	// have is not written.
	StatusField  string `json:"status_field,omitempty"`
	SessionField string `json:"session_field,omitempty"`
	LinkField    string `json:"link_field,omitempty"`
	// QueueStatus names the Status option whose cards ask for a run (D30, D40, issue
	// #71), for example "Agent queue". A card moved there never starts one: the
	// supervisor reads the board every minute and raises an "Accept this task?"
	// Decision the human answers. Empty turns it off.
	QueueStatus string `json:"queue_status,omitempty"`
	// PublicURL is whr's HTTPS name behind the forwarder (D29): the card links
	// to the task there. Without it the card has no link.
	PublicURL string `json:"public_url,omitempty"`
}

// GitHub is the App the forge adapter acts as (D31).
type GitHub struct {
	AppID   int64  `json:"app_id"`
	KeyFile string `json:"key_file"` // the App's private key
	// APIURL is the API's base URL. Optional: https://api.github.com. It must
	// be https, or http to a loopback address for a test double.
	APIURL string `json:"api_url,omitempty"`
}

// The values of Config.Account (D49).
const (
	AccountDedicated = "dedicated"
	AccountShared    = "shared"
)

// PublicOrigin returns whr's HTTPS origin (https://host[:port]) and its host name,
// or empty strings when no public URL is configured. It uses public_url and then
// board.public_url.
func (c *Config) PublicOrigin() (origin, host string) {
	raw := c.PublicURL
	if raw == "" && c.Board != nil {
		raw = c.Board.PublicURL
	}
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", ""
	}
	return "https://" + u.Host, u.Hostname()
}

// APISocketName is the JSON API's unix socket in the state directory (D29, §7.5).
const APISocketName = "api.sock"

// StateDirOf returns the state directory: stateDir when set, else
// ~/.local/state/whr.
func StateDirOf(stateDir, home string) string {
	if stateDir != "" {
		return stateDir
	}
	return filepath.Join(home, ".local", "state", "whr")
}

// APISocketPath is where the JSON API listens and where the CLI connects.
func APISocketPath(stateDir, home string) string {
	return filepath.Join(StateDirOf(stateDir, home), APISocketName)
}

// Config is the whole file.
type Config struct {
	// Schema is the optional editor hint `$schema`, the URL of the published JSON
	// Schema (issue #268). It is data only: nothing resolves, fetches or checks
	// it, and its value never affects loading or validation.
	Schema   string          `json:"$schema,omitempty"`
	SkillSet skillset.Config `json:"skill_set,omitzero"`
	// Listen is the web UI address: loopback by default, or one explicit VPN
	// interface IP. The JSON API stays on its private unix socket (D29).
	Listen string `json:"listen"`
	// PublicURL is whr's HTTPS name behind the forwarder (D29), for example
	// https://whr.tailnet.example. Passkeys (D45) are bound to its host name, so
	// they are off without it; board.public_url is used when this is empty.
	PublicURL    string       `json:"public_url,omitempty"`
	Repositories []Repository `json:"repositories"`
	Roots        Roots        `json:"roots"`
	GitHub       GitHub       `json:"github"`
	// AgentAPIKeyEnvFile holds an agent API key (for example
	// ANTHROPIC_API_KEY) as KEY=VALUE lines. It is optional: with a
	// subscription login there is none, because whr never handles a
	// subscription credential; the human signs in inside the environment
	// (D40), and a subscription token in this file is refused.
	AgentAPIKeyEnvFile string `json:"agent_api_key_env_file,omitempty"`
	// BotSigningKeyFile is the bot's SSH ed25519 private key (D51): a secret file
	// like the others (0600, owned by the whr user, outside every root) that the
	// prepared commits are signed with. Optional in the file, but without it no
	// topic is prepared: nothing is ever committed unsigned.
	BotSigningKeyFile string `json:"bot_signing_key_file,omitempty"`
	// APITokenFile holds the API token that guards the API (D29).
	APITokenFile string `json:"api_token_file"`
	// APIClients are further named API clients, each with a token file of its
	// own, so the audit tells callers apart. Optional and additive: the
	// api_token_file stays the client "default".
	APIClients []APIClient `json:"api_clients,omitempty"`
	// StateDir holds the supervisor's database. Optional: an absolute path, or
	// ~/.local/state/whr. It must not lie in a workspace root, where an agent
	// writes.
	StateDir string `json:"state_dir,omitempty"`
	// Environment shapes the environments `whr serve` provisions. Optional.
	Environment Environment `json:"environment,omitzero"`
	// Console shapes the console environment (D43). Optional.
	Console Console `json:"console,omitzero"`
	// Budgets limit the tokens and the cost a run and a task may use (design
	// §7.4). Optional: none means no limit.
	Budgets Budgets `json:"budgets,omitzero"`
	// Limits say when a provider limit the agent reported counts as low.
	Limits Limits `json:"limits,omitzero"`
	// Preview turns on the preview proxy (D33, issue #72): the loopback ports a
	// preview of an agent's dev server may listen on, which the forwarder maps.
	// Optional: without it there are no previews.
	Preview Preview `json:"preview,omitzero"`
	// ToolProfile names the profile of the tool store the environments use
	// (`profiles/<name>`). Optional when the store has exactly one.
	ToolProfile string `json:"tool_profile,omitempty"`
	// Board is the project board the supervisor keeps current with the state of
	// its tasks (D30). Optional; it adds the board's permission to the App.
	Board *Board `json:"board,omitempty"`
	// Ntfy turns on push notifications (design §9.4). Optional: without it no
	// push is sent and the inbox stays the source of truth. The topic and the
	// token are secret files like the others, never values in this file.
	Ntfy *Ntfy `json:"ntfy,omitempty"`
	// Account says whether the account whr runs as is "dedicated" to it (the
	// default) or "shared", the developer's own on a dual-use Mac (D49). It is
	// the human's statement: nothing guesses it. `whr doctor` warns about a
	// shared account and about an administrator.
	Account string `json:"account,omitempty"`
	// DevelopmentPrefix is a retired key (the --dev installation was removed in
	// the alpha, issue #493). It is still accepted, so an older config.json
	// loads, and nothing reads it.
	DevelopmentPrefix string `json:"development_prefix,omitempty"`
	// AgentPermissionMode is how the agent's permission prompts are handled:
	// "dontAsk" (the default) never asks and runs only AgentAllowedTools, and
	// "manual" routes every prompt to the human as an approval Decision over
	// the stdio control channel (D26). Optional.
	AgentPermissionMode string `json:"agent_permission_mode,omitempty"`
	// AgentAllowedTools are the tools the agent may use without asking in the
	// dontAsk mode (design §5.2): the supervisor's own choice, never the
	// repository's. Required in dontAsk, and refused in manual.
	AgentAllowedTools []string `json:"agent_allowed_tools,omitempty"`
}

// Console is the supervisor's choice of what the console environment looks like
// (design D43). Zero values take the defaults of the field.
type Console struct {
	// EgressAllow are the host names the console may reach through its egress
	// proxy: package registries and the forge for read-only fetches by default.
	// An entry is one exact host name, or `*.name` for every subdomain and not the
	// bare name (§7.2); no IP, port or path. The console holds no credentials.
	EgressAllow []string `json:"egress_allow,omitempty"`
	CPUs        int      `json:"cpus,omitempty"`      // default 2
	MemoryMB    int      `json:"memory_mb,omitempty"` // default 2048
	DiskMB      int      `json:"disk_mb,omitempty"`   // default 10240
	// SSHCAKeyFile is the private key of the authority that signs the console's
	// SSH certificates (issue #32): a secret file like the others (0600, outside
	// every root), made by `whr setup`. Without it there is no SSH access.
	SSHCAKeyFile string `json:"ssh_ca_key_file,omitempty"`
	// Base is the console's own base: fedora, ubuntu or alpine (issue #93). Empty
	// means environment.base. Alpine is for the console only: agent environments
	// stay on a glibc base.
	Base string `json:"base,omitempty"`
}

// DefaultConsoleEgress are the hosts a console may reach by default, each exactly
// (§7.2: an entry matches one host, so none of these admits a subdomain): the
// package registries of the toolchains workharbor knows, and GitHub for read-only
// fetches. No wildcard is deliberate here.
var DefaultConsoleEgress = []string{
	"github.com",             // git clone and fetch over https: the smart protocol is served from this one host
	"proxy.golang.org",       // Go module downloads
	"sum.golang.org",         // Go checksum database
	"registry.npmjs.org",     // npm packages and metadata
	"pypi.org",               // Python package index
	"files.pythonhosted.org", // Python package files, which pypi.org redirects to
}

// Resolved returns the console with the defaults filled in.
func (c Console) Resolved() Console {
	if len(c.EgressAllow) == 0 {
		c.EgressAllow = append([]string(nil), DefaultConsoleEgress...)
	}
	if c.CPUs == 0 {
		c.CPUs = DefaultCPUs
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 2048
	}
	if c.DiskMB == 0 {
		c.DiskMB = DefaultDiskMB
	}
	return c
}

// Preview is the range of loopback ports previews listen on, each with an origin
// of its own (D33). Both ends are set, or neither: no range, no previews.
type Preview struct {
	FirstPort int `json:"first_port,omitempty"`
	LastPort  int `json:"last_port,omitempty"`
}

// On reports whether previews are configured.
func (p Preview) On() bool { return p.FirstPort != 0 }

// MaxPreviewPorts is the most ports the range may hold: a preview is a listener
// and a forwarder rule, and each one shows agent-written code.
const MaxPreviewPorts = 50

// Budgets are the per-run and per-task limits. A soft threshold warns once; a
// hard limit ends the task as failed. They count what the agent reported, the
// same totals `whr usage` shows (design §5.7).
type Budgets struct {
	PerRun  BudgetLimit `json:"per_run,omitzero"`
	PerTask BudgetLimit `json:"per_task,omitzero"`
	// SoftPercent is the share of a limit at which the human is warned, 1 to
	// 99. Default 80.
	SoftPercent int `json:"soft_percent,omitempty"`
}

// Limits say when a limit an agent reported is low: the dashboard warns and a
// push is sent. Zero values take the defaults.
type Limits struct {
	// WarnPercent is the share of a usage window used at which it is low, 1 to
	// 99. Default 90.
	WarnPercent int `json:"warn_percent,omitempty"`
	// LowBalanceUSD is a reported balance at or under which it is low. Zero sets none.
	LowBalanceUSD float64 `json:"low_balance_usd,omitempty"`
}

// BudgetLimit is one scope's limits. Zero is no limit.
type BudgetLimit struct {
	MaxDuration string  `json:"max_duration,omitempty"`
	MaxTokens   int64   `json:"max_tokens,omitempty"`   // all reported tokens: input, output, cache read and write
	MaxCostUSD  float64 `json:"max_cost_usd,omitempty"` // the cost the agent reports
}

// Environment is the supervisor's choice of what an agent environment looks
// like (design §5.1, D38: a repository may request, never grant). Zero values
// take the defaults of the field.
type Environment struct {
	// Image is an image to run instead of the workharbor base image. Empty, the
	// default, means the base image of Base, which whr builds once (D44). An
	// image set here must have git: a workspace on one without is refused.
	Image string `json:"image,omitempty"`
	// Base is the first-class base of the workharbor base image: "fedora", the
	// default, or "ubuntu" (D43, D44).
	Base string `json:"base,omitempty"`
	// EgressAllow are the host names the agent may reach through the egress
	// proxy, `api.anthropic.com` by default. An entry is one exact host name, or
	// `*.name` for every subdomain and not the bare name (§7.2); no IP, port or path.
	EgressAllow []string `json:"egress_allow,omitempty"`
	CPUs        int      `json:"cpus,omitempty"`      // default 2
	MemoryMB    int      `json:"memory_mb,omitempty"` // default 4096
	DiskMB      int      `json:"disk_mb,omitempty"`   // default 10240
	// PostCreateTimeout bounds the repository's postCreateCommand, a Go duration
	// such as "10m" (the default). A command still running then fails the run.
	PostCreateTimeout string `json:"post_create_timeout,omitempty"`
}

// DefaultPostCreateTimeout is how long a postCreateCommand may run.
const DefaultPostCreateTimeout = 10 * time.Minute

// PostCreate returns the post-create timeout, the default when none is set.
// Validate has checked that a set value is a duration.
func (e Environment) PostCreate() time.Duration {
	if d, err := time.ParseDuration(e.PostCreateTimeout); err == nil && d > 0 {
		return d
	}
	return DefaultPostCreateTimeout
}

// Defaults of Environment.
const (
	DefaultBase     = "fedora" // the workharbor base image's base (D43, D44)
	DefaultCPUs     = 2
	DefaultMemoryMB = 4096
	DefaultDiskMB   = 10240
)

// Resolved returns the environment with the defaults filled in.
func (e Environment) Resolved() Environment {
	if e.Base == "" {
		e.Base = DefaultBase
	}
	if len(e.EgressAllow) == 0 {
		e.EgressAllow = []string{"api.anthropic.com"}
	}
	if e.CPUs == 0 {
		e.CPUs = DefaultCPUs
	}
	if e.MemoryMB == 0 {
		e.MemoryMB = DefaultMemoryMB
	}
	if e.DiskMB == 0 {
		e.DiskMB = DefaultDiskMB
	}
	return e
}

// Error lists everything wrong with a configuration, each with the key it is
// about, so one start reports every problem and not the first.
type Error struct{ Problems []string }

func (e *Error) Error() string {
	return "invalid configuration:\n  " + strings.Join(e.Problems, "\n  ")
}

// Load reads and validates the file at path. The error is an *Error for a
// configuration problem and an ordinary error for an unreadable file.
func Load(path string) (*Config, error) {
	// O_NONBLOCK: opening a FIFO must not wait for a writer
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the operator names the config file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes and validates a configuration.
func Parse(raw []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, &Error{Problems: []string{"the file is not a valid configuration: " + err.Error()}}
	}
	if dec.More() {
		return nil, &Error{Problems: []string{"the file has more than one JSON value"}}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the configuration against the host.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if err := checkListen(c.Listen); err != "" {
		add("listen: %s", err)
	}

	if p := c.Preview; p != (Preview{}) {
		switch {
		case p.FirstPort < 1024 || p.LastPort > 65535 || p.LastPort < p.FirstPort:
			add("preview: first_port and last_port must be ports from 1024 to 65535, first not above last")
		case p.LastPort-p.FirstPort+1 > MaxPreviewPorts:
			add("preview: at most %d ports (first_port to last_port)", MaxPreviewPorts)
		default:
			if _, port, err := net.SplitHostPort(c.Listen); err == nil {
				if n, _ := strconv.Atoi(port); n >= p.FirstPort && n <= p.LastPort {
					add("preview: the range holds the port whr listens on (%d): a preview needs an origin of its own", n)
				}
			}
		}
	}
	for name, l := range map[string]BudgetLimit{"budgets.per_run": c.Budgets.PerRun, "budgets.per_task": c.Budgets.PerTask} {
		if l.MaxDuration != "" {
			d, err := time.ParseDuration(l.MaxDuration)
			if err != nil || d < 0 || d%time.Millisecond != 0 {
				add("%s.max_duration: want a nonnegative Go duration in whole milliseconds", name)
			}
		}
		if l.MaxTokens < 0 || l.MaxCostUSD < 0 || math.IsNaN(l.MaxCostUSD) || math.IsInf(l.MaxCostUSD, 0) {
			add("%s: a limit cannot be negative", name)
		}
	}
	for i, h := range c.Console.EgressAllow {
		if !validEgressEntry(h) {
			add("console.egress_allow[%d]: %q is not a host name or a *.name wildcard (no address, port or path)", i, h)
		}
	}
	if b := c.Console.Base; b != "" && b != "alpine" && !baseimage.Distro(b).Valid() {
		add("console.base: %q is not a base with a console image (want fedora, ubuntu or alpine)", b)
	}
	if c.Console.CPUs < 0 || c.Console.MemoryMB < 0 || c.Console.DiskMB < 0 {
		add("console: cpus, memory_mb and disk_mb cannot be negative")
	}
	if p := c.Budgets.SoftPercent; p != 0 && (p < 1 || p > 99) {
		add("budgets.soft_percent: %d is not from 1 to 99", p)
	}
	if p := c.Limits.WarnPercent; p != 0 && (p < 1 || p > 99) {
		add("limits.warn_percent: %d is not from 1 to 99", p)
	}
	if v := c.Limits.LowBalanceUSD; v < 0 || v > MaxLowBalanceUSD || math.IsNaN(v) || math.IsInf(v, 0) {
		add("limits.low_balance_usd: %v is not from 0 to %d", v, int64(MaxLowBalanceUSD))
	}
	if b := c.Environment.Base; b != "" && !baseimage.Distro(b).Valid() {
		add("environment.base: %q is not a first-class base (want fedora or ubuntu)", b)
	}

	seen := map[string]string{}
	if len(c.Repositories) == 0 {
		add("repositories: at least one repository is needed")
	}
	for i, r := range c.Repositories {
		key := fmt.Sprintf("repositories[%d]", i)
		if !hostgit.ValidRepoName(r.Name) {
			add("%s.name: %q is not owner/name of ASCII letters, digits, '.', '_' and '-'", key, r.Name)
		}
		if lower := strings.ToLower(r.Name); seen[lower] != "" {
			add("%s.name: %q is the same repository as %s on a case-insensitive disk", key, r.Name, seen[lower])
		} else {
			seen[lower] = key
		}
		if r.CloneDepth < 0 {
			add("%s.clone_depth: %d is negative", key, r.CloneDepth)
		}
		if _, err := policy.ParsePreset(r.Workflow); err != nil {
			add("%s.workflow: %v", key, err)
		}
		if b := r.IntegrationBranch; b != "" && !domain.ValidIntegrationBranch(b) {
			add("%s.integration_branch: %q is not a branch name an agent cannot write", key, b)
		}
		if r.Workflow == string(policy.Prototype) && r.IntegrationBranch == "" {
			add("%s.integration_branch: a prototype repository needs an integration branch that is not the default branch: the supervisor never moves the default branch", key)
		}
		switch r.CommitLint {
		case "", CommitLintConventional, CommitLintWorkharbor:
		default:
			add("%s.commit_lint: %q is not %s or %s", key, r.CommitLint, CommitLintConventional, CommitLintWorkharbor)
		}
		if msg := checkCheckCommand(r.Check); msg != "" {
			add("%s.check: %s", key, msg)
		}
		if r.Workflow == string(policy.Published) && r.IntegrationBranch != "" {
			add("%s.integration_branch: a published repository has no integration branch: its pull requests go to the default branch", key)
		}
	}

	roots := map[string]string{"roots.tool_store": c.Roots.ToolStore}
	if c.SkillSet.Store != "" {
		roots["skill_set.store"] = c.SkillSet.Store
		if err := (skillset.Store{Root: c.SkillSet.Store, Forbidden: c.SkillStoreForbidden()}).CheckRoot(); err != nil {
			add("skill_set.store: %v", err)
		}
	}
	if c.SkillSet.Selection != "" || c.SkillSet.Default != nil || c.SkillSet.Package != nil {
		pin, err := c.SkillSet.Resolve()
		if err != nil {
			add("skill_set: %v", err)
		} else if pin != nil && c.SkillSet.Store == "" {
			add("skill_set.store: a package needs a dedicated store")
		}
	}
	if len(c.Roots.Workspaces) == 0 {
		add("roots.workspaces: at least one workspace root is needed")
	}
	for i, w := range c.Roots.Workspaces {
		roots[fmt.Sprintf("roots.workspaces[%d]", i)] = w
	}
	resolved := map[string]string{}
	for _, key := range sortedKeys(roots) {
		dir, msg := checkDir(roots[key])
		if msg != "" {
			add("%s: %s", key, msg)
			continue
		}
		resolved[key] = dir
		if repo := enclosingRepo(dir); repo != "" {
			add("%s: %s is inside the git repository %s, which is not workharbor's to mount", key, dir, repo)
		}
	}
	// An agent writes its workspace, so nothing workharbor trusts may live in or
	// contain it, and the roots are separate places.
	keys := sortedKeys(resolved)
	for i, a := range keys {
		for _, b := range keys[i+1:] {
			if within(resolved[a], resolved[b]) || within(resolved[b], resolved[a]) {
				add("%s and %s overlap (%s, %s): they must be separate directories", a, b, resolved[a], resolved[b])
			}
		}
	}

	if c.StateDir != "" {
		if dir, msg := checkDir(c.StateDir); msg != "" {
			add("state_dir: %s", msg)
		} else if err := CheckStateDir(c.StateDir); err != nil {
			add("state_dir: %v", err)
		} else {
			for _, key := range sortedKeys(resolved) {
				if strings.HasPrefix(key, "roots.workspaces") && (within(dir, resolved[key]) || within(resolved[key], dir)) {
					add("state_dir: %s overlaps %s: the database must be out of reach of an agent", dir, key)
				}
			}
		}
	}
	if e := c.Environment; e.CPUs < 0 || e.MemoryMB < 0 || e.DiskMB < 0 {
		add("environment: cpus, memory_mb and disk_mb must not be negative")
	}
	if v := c.Environment.PostCreateTimeout; v != "" {
		if d, err := time.ParseDuration(v); err != nil || d < time.Second || d > 6*time.Hour {
			add("environment.post_create_timeout: %q is not a duration from 1s to 6h, such as \"10m\"", v)
		}
	}
	for _, h := range c.Environment.EgressAllow {
		if !validEgressEntry(h) {
			add("environment.egress_allow: %q is not a host name or a *.name wildcard (no IP address, port or path)", h)
		}
	}

	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			add("public_url: %q must be an https address without a path", c.PublicURL)
		}
	}
	if b := c.Board; b != nil {
		if !boardOwnerRE.MatchString(b.Owner) {
			add("board.owner: %q is not a GitHub login", b.Owner)
		}
		if b.Number <= 0 {
			add("board.number: the project number must be positive")
		}
		for name, f := range map[string]string{"status_field": b.StatusField, "session_field": b.SessionField, "link_field": b.LinkField} {
			if len(f) > 100 || strings.ContainsFunc(f, unicode.IsControl) {
				add("board.%s: a field name is at most 100 characters, without control characters", name)
			}
		}
		if b.PublicURL != "" {
			if u, err := url.Parse(b.PublicURL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
				add("board.public_url: %q must be an https address without a path", b.PublicURL)
			}
		}
	}
	switch c.Account {
	case "", AccountDedicated, AccountShared:
	default:
		add("account: %q is not dedicated or shared", c.Account)
	}
	switch c.AgentPermissionMode {
	case "", "dontAsk":
	case "manual":
		if len(c.AgentAllowedTools) > 0 {
			add("agent_allowed_tools: an allowlist is for the dontAsk mode; in manual mode every prompt goes to you")
		}
	default:
		add("agent_permission_mode: %q is not dontAsk or manual", c.AgentPermissionMode)
	}

	if c.GitHub.AppID <= 0 {
		add("github.app_id: a positive App ID is needed")
	}
	if msg := checkAPIURL(c.GitHub.APIURL); msg != "" {
		add("github.api_url: %s", msg)
	}
	secrets := map[string]string{"github.key_file": c.GitHub.KeyFile, "api_token_file": c.APITokenFile}
	if c.AgentAPIKeyEnvFile != "" {
		secrets["agent_api_key_env_file"] = c.AgentAPIKeyEnvFile
	}
	for i, cl := range c.APIClients {
		secrets[fmt.Sprintf("api_clients[%d].token_file (client %q)", i, cl.Name)] = cl.TokenFile
	}
	if c.BotSigningKeyFile != "" {
		secrets["bot_signing_key_file"] = c.BotSigningKeyFile
	}
	if c.Console.SSHCAKeyFile != "" {
		secrets["console.ssh_ca_key_file"] = c.Console.SSHCAKeyFile
	}
	if n := c.Ntfy; n != nil {
		if n.Server != "" && !notify.ValidServer(n.Server) {
			add("ntfy.server: must be an https URL (or a loopback one) without a username, password, query or fragment")
		}
		if origin, _ := c.PublicOrigin(); origin == "" {
			add("ntfy: a push links to the task, so public_url (or board.public_url) must be an https URL")
		}
		secrets["ntfy.topic_file"] = n.TopicFile
		if n.TokenFile != "" {
			secrets["ntfy.token_file"] = n.TokenFile
		}
	}
	for _, key := range sortedKeys(secrets) {
		if msg := checkSecretFile(secrets[key]); msg != "" {
			add("%s: %s", key, msg)
		} else if key == "ntfy.topic_file" {
			if b, err := ReadSecret(secrets[key]); err == nil && !notify.ValidTopic(strings.TrimSpace(string(b))) {
				add("%s: the topic must be at least %d characters, without / ? # or whitespace", key, notify.MinTopicLength)
			}
		} else if key == "agent_api_key_env_file" {
			if _, err := c.AgentAPIKey(); err != nil {
				add("%s: %s", key, strings.TrimPrefix(err.Error(), "config: "+key+": "))
			}
		}
	}
	for _, p := range c.CheckAPIClients() {
		add("%s", p)
	}
	// A secret file inside a root would reach an agent: it writes the
	// workspaces, every environment mounts the tool store, and checkouts share
	// the cache's objects. Directories are compared by identity, so a case
	// variant of a path on APFS is caught too.
	why := func(root string) string {
		if root == "roots.tool_store" {
			return "the tool store, which every environment mounts"
		}
		return "a workspace root, where an agent can read it"
	}
	for key, path := range secrets {
		for _, root := range sortedKeys(resolved) {
			if within(path, resolved[root]) {
				add("%s: %s is inside %s", key, path, why(root))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Slice(problems, func(i, j int) bool { return naturalLess(problems[i], problems[j]) })
	return &Error{Problems: problems}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return naturalLess(keys[i], keys[j]) })
	return keys
}

// naturalLess orders strings with a run of digits by its number, so
// api_clients[2] comes before api_clients[10].
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		if isDigit(a[0]) && isDigit(b[0]) {
			na, nb := digitRun(a), digitRun(b)
			ta, tb := strings.TrimLeft(a[:na], "0"), strings.TrimLeft(b[:nb], "0")
			if len(ta) != len(tb) {
				return len(ta) < len(tb)
			}
			if ta != tb {
				return ta < tb
			}
			a, b = a[na:], b[nb:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitRun(s string) int {
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	return n
}

// within reports whether path is dir or lies below it. It compares directories
// by identity (device and inode), walking up from path, so a link or a case
// variant of a name on a case-insensitive disk does not hide the relation.
func within(path, dir string) bool {
	d, err := os.Stat(dir)
	if err != nil {
		return false
	}
	p := filepath.Clean(path)
	if target, err := filepath.EvalSymlinks(p); err == nil {
		p = target
	}
	for {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, d) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// checkListen checks the web UI address and a nonzero TCP port.
func checkListen(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Sprintf("%q is not host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Sprintf("port %q is not a port", port)
	}
	if _, err := WebListenIP(addr); err != nil {
		return err.Error()
	}
	return ""
}

// WebListenIP returns the explicit unicast IP of the web listener. Names and
// unspecified addresses are refused so binding never expands to all interfaces.
// The operator chooses the VPN interface; an IP alone cannot prove VPN isolation.
func WebListenIP(addr string) (net.IP, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%q is not host:port", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() && !ip.IsGlobalUnicast() {
		return nil, fmt.Errorf("%q is not an explicit unicast IP address (names, unspecified and multicast addresses are refused)", host)
	}
	return ip, nil
}

// CheckStateDir refuses a state directory that another user could enter or
// replace things in: it must be a real directory (not a link) owned by this user,
// with no permission for group or others. The configuration check and the start of
// `whr serve` apply the same rule, so a configuration that passes does not fail at
// start (D29, §7.5).
func CheckStateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (a link to one is not accepted)", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the state directory %s is accessible to others (mode %04o): make it 0700, or the API socket could be reached by other users (D29)", dir, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("the state directory %s is owned by another user", dir)
	}
	return nil
}

// checkDir returns the resolved path of an existing, absolute, clean directory.
func checkDir(path string) (string, string) {
	switch {
	case path == "":
		return "", "a directory is needed"
	case !filepath.IsAbs(path) || filepath.Clean(path) != path:
		return "", fmt.Sprintf("%q must be an absolute, clean path", path)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Sprintf("%q does not exist", path)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return "", fmt.Sprintf("%q is not a directory", path)
	}
	return target, ""
}

// checkCheckCommand refuses a configured check command that is empty after
// trimming (when set), too long, or holds a control character other than a tab:
// it is one line handed to `sh -c`.
func checkCheckCommand(cmd string) string {
	if cmd == "" {
		return ""
	}
	switch {
	case strings.TrimSpace(cmd) == "":
		return "is blank"
	case len(cmd) > MaxCheck:
		return fmt.Sprintf("is longer than %d bytes", MaxCheck)
	}
	for _, r := range cmd {
		if r != '\t' && unicode.IsControl(r) {
			return "must be one line without control characters"
		}
	}
	return ""
}

// checkSecretFile requires a regular file, not a link, owned by the current
// user, with mode 0600, a single link and some content: the value is in the
// file, never in the configuration. A second hard link could sit where an
// agent reads it.
func checkSecretFile(path string) string { return CheckSecretFile(path) }

// CheckSecretFile is the check every secret file passes in the configuration
// check, for the doctor: it returns what is wrong, or "".
func CheckSecretFile(path string) string {
	switch {
	case path == "":
		return "a file path is needed"
	case !filepath.IsAbs(path) || filepath.Clean(path) != path:
		return fmt.Sprintf("%q must be an absolute, clean path", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("%q does not exist", path)
	}
	return checkSecretInfo(path, info)
}

func checkSecretInfo(path string, info os.FileInfo) string {
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("%q is not a regular file (a link or a directory is refused)", path)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fmt.Sprintf("%q has mode %04o, it must be 0600 so that nobody else can read it", path, perm)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Getuid() { //nolint:gosec // a uid fits an int
			return fmt.Sprintf("%q is owned by another user", path)
		}
		if st.Nlink != 1 {
			return fmt.Sprintf("%q has %d hard links, it must have one", path, st.Nlink)
		}
	}
	if info.Size() == 0 {
		return fmt.Sprintf("%q is empty", path)
	}
	if info.Size() > maxSecret {
		return fmt.Sprintf("%q is larger than %d bytes", path, maxSecret)
	}
	return ""
}

// maxSecret bounds a secret file; a key or a token is far smaller.
const maxSecret = 64 << 10

// ReadSecret reads a secret file without following a link, and checks the file
// it opened (not the path) like the configuration check does, so a file
// swapped after the check is refused. An error names the path, never the content.
func ReadSecret(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("config: %q must be an absolute, clean path", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // the operator names the secret; checked on the open file
	if err != nil {
		return nil, fmt.Errorf("config: cannot open the secret %q (a link is refused): %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if msg := checkSecretInfo(path, info); msg != "" {
		return nil, errors.New("config: " + msg)
	}
	return io.ReadAll(io.LimitReader(f, maxSecret))
}

// AgentAPIKey reads AgentAPIKeyEnvFile: KEY=VALUE lines, with blank lines and
// lines starting with '#' ignored. The entries go to the agent's process
// environment through the runtime's env file, never a command line. With no
// file it returns nothing. A subscription credential is refused: whr never
// reads, stores or relays one (D40); an empty or blank value is refused;
// errors name the line, never its value.
func (c *Config) AgentAPIKey() ([]string, error) {
	if c.AgentAPIKeyEnvFile == "" {
		return nil, nil
	}
	raw, err := ReadSecret(c.AgentAPIKeyEnvFile)
	if err != nil {
		return nil, err
	}
	var env []string
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		switch {
		case !ok || !envKey.MatchString(k):
			return nil, fmt.Errorf("config: agent_api_key_env_file: line %d is not KEY=VALUE", i+1)
		}
		if err := credcheck.Check(k, v); errors.Is(err, credcheck.ErrEmptyValue) {
			return nil, fmt.Errorf("config: agent_api_key_env_file: line %d sets %s to an empty value; write the value after the = in %s, or delete the file and remove agent_api_key_env_file from the configuration to use a subscription login", i+1, k, c.AgentAPIKeyEnvFile)
		} else if err != nil {
			return nil, fmt.Errorf("config: agent_api_key_env_file: line %d sets %s: %w; whr never handles a subscription credential (D40). %s", i+1, k, err, credcheck.Advice)
		}
		env = append(env, line)
	}
	if len(env) == 0 {
		return nil, errors.New("config: agent_api_key_env_file: no KEY=VALUE line")
	}
	return env, nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// enclosingRepo returns the nearest directory at or above dir that holds a
// .git, or "". A workspace root there would put a human's repository around
// the agents' clones, and a human's own repository is never mounted (D42).
func enclosingRepo(dir string) string {
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		if filepath.Dir(p) == p {
			return ""
		}
	}
}

// CheckWorkspacePath says whether a folder may become a workspace (design
// §4.4, D42) and returns its resolved path. It must be absolute and clean, an
// existing empty directory, strictly below one of the workspace roots, and not
// inside a git repository: the human's own repositories are never mounted, and
// a clone of one is not a workspace. Everything is compared by identity, so a
// link or a case variant cannot hide where the folder really is.
func (c *Config) CheckWorkspacePath(path string) (string, error) {
	resolved, msg := checkDir(path)
	if msg != "" {
		return "", fmt.Errorf("workspace path: %s", msg)
	}
	under := false
	for _, root := range c.Roots.Workspaces {
		if rd, err := os.Stat(root); err == nil {
			if pd, err := os.Stat(resolved); err == nil && os.SameFile(rd, pd) {
				return "", fmt.Errorf("workspace path: %s is a workspace root itself: use a folder below it", resolved)
			}
		}
		if within(resolved, root) {
			under = true
		}
	}
	if !under {
		return "", fmt.Errorf("workspace path: %s is not below a workspace root (roots.workspaces)", resolved)
	}
	if repo := enclosingRepo(resolved); repo != "" {
		return "", fmt.Errorf("workspace path: %s is inside the git repository %s: a human's own repository is never mounted", resolved, repo)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "", fmt.Errorf("workspace path: %w", err)
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("workspace path: %s is not empty: the agent clone is created in it", resolved)
	}
	return resolved, nil
}

// validEgressEntry accepts an entry of the supervisor's own allowlists: a host
// name, or `*.` and a host name (every subdomain, not the bare name; design §7.2).
// A repository cannot write one: its egress request is one exact host.
func validEgressEntry(h string) bool {
	if rest, wild := strings.CutPrefix(h, "*."); wild {
		return validEgressHost(rest)
	}
	return validEgressHost(h)
}

// validEgressHost accepts a plain DNS name: letters, digits, '-' and '.',
// with at least one dot and no IP address, so the allowlist cannot be a way
// around the proxy's rule that a raw IP is refused.
func validEgressHost(h string) bool {
	if h == "" || len(h) > 253 || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// checkAPIURL accepts an https URL, or an http URL to a loopback address (a test
// double of the API), with no credentials. Empty means GitHub's own.
func checkAPIURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Sprintf("%q is not a plain URL", raw)
	}
	switch u.Scheme {
	case "https":
		return ""
	case "http":
		host, _, herr := net.SplitHostPort(u.Host)
		if herr != nil {
			host = u.Host
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return ""
		}
	}
	return fmt.Sprintf("%q must be https, or http to a loopback address", raw)
}

var boardOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// MaxLowBalanceUSD caps limits.low_balance_usd, far under what converts to
// micro-USD in an int64.
const MaxLowBalanceUSD = 1e9

// APIClient is one named API client: a label for the audit and the file that
// holds its token. The token never lives in the configuration.
type APIClient struct {
	Name      string `json:"name"`
	TokenFile string `json:"token_file"`
}

// DefaultAPIClient names the client that api_token_file defines. Its audit
// actor is the derived id, so the name is reserved.
const DefaultAPIClient = "default"

var (
	apiClientNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	derivedIDRE     = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// CheckAPIClientName says why a client name is refused, or returns "".
func CheckAPIClientName(name string) string {
	switch {
	case name == DefaultAPIClient:
		return fmt.Sprintf("the name %q is reserved for api_token_file", name)
	case !apiClientNameRE.MatchString(name):
		return fmt.Sprintf("the name %q must match [a-z][a-z0-9-]{0,31}", name)
	case derivedIDRE.MatchString(name):
		return fmt.Sprintf("the name %q looks like a derived client id (12 hex digits)", name)
	}
	return ""
}

// CheckAPIClients checks the names, and that no two clients (the default
// included) share a token. Tokens are compared by digest; a problem names the
// clients and paths only, never a token or a digest. The files themselves are
// checked with the other secrets.
func (c *Config) CheckAPIClients() []string {
	var out []string
	names := map[string]bool{}
	type entry struct{ name, path string }
	all := []entry{{DefaultAPIClient, c.APITokenFile}}
	for i, cl := range c.APIClients {
		if msg := CheckAPIClientName(cl.Name); msg != "" {
			out = append(out, fmt.Sprintf("api_clients[%d]: %s", i, msg))
		} else if names[cl.Name] {
			out = append(out, fmt.Sprintf("api_clients[%d]: the name %q is used twice", i, cl.Name))
		}
		names[cl.Name] = true
		all = append(all, entry{cl.Name, cl.TokenFile})
	}
	seen := map[[sha256.Size]byte]entry{}
	for _, e := range all {
		if checkSecretFile(e.path) != "" {
			continue // reported with the secret files
		}
		raw, err := ReadSecret(e.path)
		if err != nil {
			continue
		}
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(raw))))
		if first, ok := seen[sum]; ok {
			out = append(out, fmt.Sprintf("api_clients: %q (%s) and %q (%s) share one token", first.name, first.path, e.name, e.path))
			continue
		}
		seen[sum] = e
	}
	return out
}
