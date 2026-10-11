// Package domain defines the core workharbor objects: Task, Workspace, Run,
// Environment, Decision, ReviewCandidate and Event. See docs/content/docs/design/domain.md §4.
package domain

import "time"

// ID is a stable, opaque identifier.
type ID string

// TaskState is the lifecycle state of a Task.
type TaskState string

const (
	TaskQueued           TaskState = "queued"
	TaskRunning          TaskState = "running"
	TaskAwaitingGuidance TaskState = "awaiting_guidance"
	TaskReadyForReview   TaskState = "ready_for_review"
	TaskCompleted        TaskState = "completed"
	TaskCancelled        TaskState = "cancelled"
	TaskFailed           TaskState = "failed"
)

// RunState is the lifecycle state of a Run. Pause is a run state, not an
// environment state.
type RunState string

const (
	RunStarting    RunState = "starting"
	RunRunning     RunState = "running"
	RunPaused      RunState = "paused"
	RunInterrupted RunState = "interrupted"
	RunStopped     RunState = "stopped"
	RunFailed      RunState = "failed"
)

// EnvState is the lifecycle state of an Environment.
type EnvState string

const (
	EnvProvisioning EnvState = "provisioning"
	EnvRunning      EnvState = "running"
	EnvStopped      EnvState = "stopped"
	EnvDeleted      EnvState = "deleted"
)

// Task is the unit of work: an issue, instructions, decisions and results.
type Task struct {
	// Version is the aggregate's version: the store saves with
	// compare-and-swap on it, so two writers cannot both win (design §5.4).
	Version int64
	ID      ID
	Repo    string
	Issue   string
	State   TaskState
	// AgentID is the agent the task is assigned to (design D42); empty for a
	// task made before agents existed.
	AgentID ID
	// Untrusted is set when text from an untrusted author is part of the task
	// (design §6): the policy decides with that context.
	Untrusted bool
	// Workflow is the repository's preset when the task started (D47). A looser
	// preset never applies to a task already started. Empty means the default.
	Workflow string
	// Branch is the integration branch the repository had when the task started
	// (empty for a published repository, or a task from before it was recorded). The
	// task publishes to it whatever the configuration says now (D47, §6).
	Branch    string
	CreatedAt time.Time
}

// Run is one execution of an agent in an environment.
type Run struct {
	Skills      SkillSelection
	ID          ID
	TaskID      ID
	WorkspaceID ID
	AgentID     ID // the agent that runs it; the task's agent
	EnvID       ID
	State       RunState
	// SessionID is the agent's session ID, recorded when the agent reports it.
	// The session, not the environment, is what survives a restart (§5.3).
	SessionID string
	// ResumeAttempts counts launches of the agent that failed since the run
	// last ran; it resets when the run is running again (design §5.3).
	ResumeAttempts int
	// Lifecycle bounds and the initiating stop reason are audit-backed.
	AdmittedAt time.Time
	EndedAt    time.Time
	// Legacy duration counters remain readable; new runs do not account or
	// enforce elapsed wall time.
	DurationMillis int64
	DurationAt     time.Time
	TerminalReason string
}

// Environment is the container or VM backing a run or workspace.
type Environment struct {
	ID      ID
	Backend string
	State   EnvState
}

// ReviewCandidate ties a prepared revision to its push, PR and CI results,
// keyed by commit SHA so a pass on an earlier revision never marks a later one
// ready. It is created when cleanup pins the SHA, before the push.
type ReviewCandidate struct {
	TaskID ID
	RunID  ID // the run that produced the revision
	Branch string
	SHA    string
	PRURL  string
	CI     CIState // the pipeline result for SHA; empty means pending
	// Source is the agent's own tip the revision was prepared from. A
	// follow-up round rebases only the agent's commits after it onto the
	// pushed revision, so pushed commits are never rewritten (design §4.5).
	Source string
	Pushed bool // the revision reached the forge
	// Files, Added and Removed are the diff stat of the revision against what it was
	// prepared onto: the files it changes and its lines added and removed (binary
	// files count as a file and no lines). Zero when it was not measured.
	Files, Added, Removed int64
}

// DiffStat is the size of a change.
type DiffStat struct{ Files, Added, Removed int64 }

// CIState is the result of a CI pipeline for one commit.
type CIState string

const (
	CIPending CIState = "pending"
	CIPassed  CIState = "passed"
	CIFailed  CIState = "failed"
)
