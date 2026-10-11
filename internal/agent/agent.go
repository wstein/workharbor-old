// Package agent defines the agent adapter contract for supported coding
// agents (design §5.2). The contract is versioned, its capabilities are
// reported and never assumed, and a conformance suite (agenttest) turns the
// claims into verified facts.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ContractVersion is the version of this contract. An adapter reports the
// version it implements in Capabilities.
const ContractVersion = 1

// AuthMode is how an agent authenticates.
type AuthMode string

const (
	// AuthAPIKey keeps the key in the host-side proxy and issues it per run (§7.3).
	AuthAPIKey AuthMode = "api-key"
	// AuthSubscription is a consumer-plan login kept in a per-environment auth
	// directory; the CLI refreshes the token itself.
	AuthSubscription AuthMode = "subscription"
)

// Mode says how much of the contract an adapter meets.
type Mode string

const (
	// ModeFull has headless operation, structured events, mid-run injection
	// and host-routed approvals. Claude Code is the first agent that must be.
	ModeFull Mode = "full"
	// ModeDegraded runs headless with structured events but without injection
	// or host approvals: a message becomes a resumed turn and only a fixed
	// allowlist is permitted. The UI labels it.
	ModeDegraded Mode = "degraded"
	// ModeUnsupported cannot be supervised.
	ModeUnsupported Mode = "unsupported"
)

// Capabilities describe what an agent supports. Every flag is a claim the
// conformance suite checks.
type Capabilities struct {
	ContractVersion int `json:"contract_version"`

	Headless          bool `json:"headless"`            // runs unattended
	StructuredEvents  bool `json:"structured_events"`   // typed events; required of every adapter
	MidRunInstruction bool `json:"mid_run_instruction"` // a message can be sent into a running session
	HostApprovals     bool `json:"host_approvals"`      // permission prompts are routed to the host
	CooperativePause  bool `json:"cooperative_pause"`   // pause without ending the process (D11: none of the measured agents)
	SessionResume     bool `json:"session_resume"`      // a stopped session can be resumed by ID
	IssuePRTooling    bool `json:"issue_pr_tooling"`
	AwaitingGuidance  bool `json:"awaiting_guidance"` // can raise a blocking question
	ReportsQuota      bool `json:"reports_quota"`     // reports quota_exhausted with the reset time
	ReportsUsage      bool `json:"reports_usage"`     // emits usage events with a typed payload (§5.7)
	// ToolWatchdog requires correlated execution/output/wait events and confirmed
	// tool-only cancellation through ToolWatchdogSession. False means uncovered.
	ToolWatchdog bool `json:"tool_watchdog"`

	AuthModes []AuthMode `json:"auth_modes"`
}

// Mode computes the mode from the capabilities.
func (c Capabilities) Mode() Mode {
	switch {
	case !c.Headless || !c.StructuredEvents:
		return ModeUnsupported
	case c.MidRunInstruction && c.HostApprovals:
		return ModeFull
	default:
		return ModeDegraded
	}
}

// Supports reports whether the agent offers an auth mode.
func (c Capabilities) Supports(m AuthMode) bool {
	for _, have := range c.AuthModes {
		if have == m {
			return true
		}
	}
	return false
}

// EventKind classifies an event.
type EventKind string

const (
	EventSession        EventKind = "session" // the session ID is known
	EventMessage        EventKind = "message"
	EventToolCall       EventKind = "tool_call"
	EventToolResult     EventKind = "tool_result"
	EventDiff           EventKind = "diff"
	EventTestResult     EventKind = "test_result"
	EventUsage          EventKind = "usage"
	EventApproval       EventKind = "approval" // a permission prompt was put to the host
	EventAuthExpired    EventKind = "auth_expired"
	EventQuotaExhausted EventKind = "quota_exhausted"
	EventError          EventKind = "error"
)

// Event is one typed observation from a running agent. Everything in it comes
// from the agent or the repository and is untrusted data.
type Event struct {
	Kind      EventKind `json:"kind"`
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
	Text      string    `json:"text,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Input     string    `json:"input,omitempty"`   // capped
	ResetAt   time.Time `json:"reset_at,omitzero"` // for quota_exhausted, when known
	// Usage is set on a usage event (design §5.7).
	Usage *Usage `json:"usage,omitempty"`
	// Approval is set on an approval event: the record the audit entry is
	// written from (design §5.4).
	Approval *ApprovalRecord `json:"approval,omitempty"`
	// Watchdog accompanies only the optional execution watchdog event kinds.
	Watchdog *ToolWatchdogEvent `json:"watchdog,omitempty"`
}

// ApprovalRecord is what happened to one permission prompt: the request that
// was put to the host and the answer the agent was given. A prompt that no
// human answered is a denial with the reason why.
type ApprovalRecord struct {
	ID     string `json:"id"` // the ApprovalRequest ID
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// ResultStatus is how a session ended.
type ResultStatus string

const (
	ResultCompleted ResultStatus = "completed"
	ResultFailed    ResultStatus = "failed"
	// ResultStopped is a hard interrupt; the session stays resumable.
	ResultStopped ResultStatus = "stopped"
	// ResultAuthExpired and ResultQuotaExhausted end a run without failing it:
	// the supervisor opens a blocking Decision (§4.2) and the session stays
	// resumable.
	ResultAuthExpired    ResultStatus = "auth_expired"
	ResultQuotaExhausted ResultStatus = "quota_exhausted"
)

// Result is the outcome of a session.
type Result struct {
	Status    ResultStatus `json:"status"`
	SessionID string       `json:"session_id,omitempty"`
	Text      string       `json:"text,omitempty"`
	ResetAt   time.Time    `json:"reset_at,omitzero"` // for ResultQuotaExhausted, when known
}

// Delivery says how an instruction reached the agent.
type Delivery string

const (
	// DeliveryInjected reached the agent immediately.
	DeliveryInjected Delivery = "injected"
	// DeliveryNextTurn is picked up at the next model step, after the running
	// tool finishes (Claude Code, spike #1).
	DeliveryNextTurn Delivery = "next_turn"
	// DeliveryResumedTurn is the degraded mode: the message becomes a resumed turn.
	DeliveryResumedTurn Delivery = "resumed_turn"
)

// PermissionMode says what happens when the agent wants to use a tool. The
// agent CLIs have coarser modes of their own (design §6); these are the two the
// supervisor needs, and an adapter maps them. `bypassPermissions` and `auto`
// are not offered.
type PermissionMode string

const (
	// PermissionManual routes every permission prompt to the host. It needs an
	// adapter with HostApprovals.
	PermissionManual PermissionMode = "manual"
	// PermissionDontAsk never asks: a tool on AllowedTools runs and every other
	// is denied, and the approver is not consulted. A degraded agent runs in it.
	PermissionDontAsk PermissionMode = "dontAsk"
)

// StartSpec describes a session to start or resume.
type StartSpec struct {
	EnvID   string   `json:"env_id"`
	Workdir string   `json:"workdir"`
	Prompt  string   `json:"prompt"`
	Auth    AuthMode `json:"auth"`
	// PermissionMode is fixed when the agent process starts. Empty means
	// PermissionManual.
	PermissionMode PermissionMode `json:"permission_mode,omitempty"`
	// AllowedTools are the tools that run without asking in PermissionDontAsk,
	// and must be empty in any other mode.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// Approver answers the agent's permission prompts. An adapter that reports
	// HostApprovals needs one. It does not cross a process boundary: the plugin
	// calls back to the supervisor (design §5.5).
	Approver Approver `json:"-"`
	// ApprovalTimeout is how long the Approver has; zero means
	// DefaultApprovalTimeout. It is in nanoseconds on the wire.
	ApprovalTimeout time.Duration `json:"approval_timeout_ns,omitempty"`
	// Env is KEY=VALUE for the agent process, chosen by the supervisor for this
	// start: the egress proxy's address (it changes with every start of the
	// environment) and the agent's HOME. It holds no secret; a credential
	// reaches the agent only through the adapter's own configuration (D40).
	Env []string `json:"env,omitempty"`
}

// Errors an adapter returns for the same situations.
var (
	ErrUnsupported     = errors.New("the agent does not support this")
	ErrUnsupportedAuth = errors.New("the agent does not offer this auth mode")
	ErrNoApprover      = errors.New("this agent routes approvals to the host and needs an Approver")
	ErrNoSession       = errors.New("no such session")
	ErrNotRunning      = errors.New("the session is not running")
	ErrBadSpec         = errors.New("the start spec is not valid")
)

// Mode returns the permission mode, with the empty value read as manual.
func (s StartSpec) Mode() PermissionMode {
	if s.PermissionMode == "" {
		return PermissionManual
	}
	return s.PermissionMode
}

// CheckSpec checks a StartSpec against what an agent supports, in the order
// every adapter must report: the auth mode (ErrUnsupportedAuth), the
// permission mode (ErrUnsupported when the agent cannot honour it, ErrBadSpec
// when the spec contradicts itself) and the approver (ErrNoApprover).
func (c Capabilities) CheckSpec(s StartSpec) error {
	if !c.Supports(s.Auth) {
		return ErrUnsupportedAuth
	}
	switch s.Mode() {
	case PermissionManual:
		if len(s.AllowedTools) > 0 {
			return fmt.Errorf("%w: an allowlist needs the %s mode", ErrBadSpec, PermissionDontAsk)
		}
		if !c.HostApprovals {
			return fmt.Errorf("%w: this agent cannot route approvals to the host, so %s is not available", ErrUnsupported, PermissionManual)
		}
		if s.Approver == nil {
			return ErrNoApprover
		}
	case PermissionDontAsk:
	default:
		return fmt.Errorf("%w: permission mode %q", ErrUnsupported, s.PermissionMode)
	}
	return nil
}

// Adapter starts and resumes sessions of one coding agent. The context passed to
// Start and Resume bounds the session: when it ends, the session is stopped. A
// caller that wants a session to outlive a request passes a context that does
// not end with it (the serve integration run found a session dying when the API
// request that started it returned).
type Adapter interface {
	Name() string
	Capabilities() Capabilities
	Start(ctx context.Context, spec StartSpec) (Session, error)
	// Resume continues a stopped session by ID (ErrUnsupported without
	// SessionResume, ErrNoSession for an unknown ID).
	Resume(ctx context.Context, spec StartSpec, sessionID string) (Session, error)
}

// Session is one running agent. Events is closed when the session has ended.
type Session interface {
	// ID is the agent's session ID. It may be empty until the first user
	// message has been sent (spike #1), and is set in the Result at the latest.
	ID() string
	Events() <-chan Event
	// Instruct sends a message and says how it was delivered. An agent without
	// MidRunInstruction answers DeliveryResumedTurn and never claims the other two.
	Instruct(ctx context.Context, message string) (Delivery, error)
	// Stop is a hard interrupt that leaves the session resumable.
	Stop(ctx context.Context) error
	// Wait blocks until the session ends and returns its result. An end by
	// auth, quota or Stop is a Result and not an error.
	Wait() (Result, error)
}

// Pauser is implemented by a session only if the agent reports
// CooperativePause (D11). Without it, pausing is Stop followed by Resume.
type Pauser interface {
	Pause(ctx context.Context) error
	Continue(ctx context.Context) error
}
