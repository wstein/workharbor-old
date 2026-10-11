// Package runtime defines the runtime adapter contract. Backends report
// capabilities explicitly and do not pretend to share Docker semantics.
// See docs/content/docs/design/architecture.md §5.1.
package runtime

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/wstein/workharbor/internal/domain"
)

// Isolation describes the isolation boundary of a backend.
type Isolation string

const (
	SharedKernel Isolation = "shared-kernel"
	GuestKernel  Isolation = "guest-kernel"
	FullVM       Isolation = "full-vm"
)

// Capabilities are reported by the backend, never assumed.
type Capabilities struct {
	Isolation         Isolation
	Arch              string
	PersistentStorage []string // what survives stop, rebuild, delete
	NetworkIsolation  bool
	Suspend           bool
	SSH               bool
	Browser           bool
	// InteractiveExec says the backend's own command line can run an interactive
	// command in a running environment with the caller's terminal attached
	// directly (design §7.3, "The sign-in shell"). Whether a given backend's does
	// is measured, not assumed.
	InteractiveExec bool
}

// Errors every adapter returns for the same situations, so callers and the
// conformance suite can tell them apart.
var (
	ErrNotFound   = errors.New("environment not found")
	ErrNotRunning = errors.New("environment is not running")
	ErrRunning    = errors.New("environment is running and must be stopped first")
	ErrNotOwned   = errors.New("environment is not owned by this supervisor")

	// ErrNotPrepared is returned by Provision for a spec that did not come from Prepare.
	ErrNotPrepared = errors.New("the spec was not prepared: use runtime.Prepare")
	// ErrVolumeBusy means another running environment holds the volume read-write
	// (design §4.4: a writable volume is exclusive).
	ErrVolumeBusy = errors.New("the volume is held read-write by a running environment")
)

// Info describes one environment as the runtime reports it.
type Info struct {
	ID     string
	Owner  string // the OwnerLabel value
	Labels map[string]string
	Image  string
	// ImageDigest is the digest of the image the environment was created from, as
	// the runtime recorded it ("sha256:…"), or empty when it does not say. An
	// image reference can move (a tag); the digest says which image ran.
	ImageDigest string
	Mounts      []Mount // what the runtime mounted: the resolved paths of the prepared spec
	State       domain.EnvState
	// Addr is the address the environment has now. It is empty unless the
	// environment is running, and it changes across a restart and a recreate,
	// so it is read again every time and never stored.
	Addr string
	// Proxy is the egress proxy's URL for the agent's HTTPS_PROXY, empty unless
	// the environment runs and has an egress sidecar. Like Addr it is read each
	// time and never stored.
	Proxy string
	// EgressAllow are the hosts the environment's egress sidecar allows, sorted.
	// Nil without a sidecar. Inspect reports it; List need not.
	EgressAllow []string
}

// TerminalRequest is a command to run in a running environment with a terminal:
// a shell for the human, whose keystrokes arrive as bytes and whose screen is the
// bytes that come back.
type TerminalRequest struct {
	Cmd        []string
	Env        []string
	Dir        string
	Cols, Rows uint16 // the terminal's size; zero means 80 by 24
}

// Terminal is a running command with a terminal. Read gives its output, Write
// its input, and a Read after the command ended returns io.EOF. Close hangs up
// the terminal and ends the command.
type Terminal interface {
	io.ReadWriteCloser
	// Resize tells the command the terminal's new size.
	Resize(cols, rows uint16) error
	// Wait returns the command's exit code once it has ended.
	Wait() (exitCode int, err error)
}

// TerminalAdapter is implemented by an adapter that can run a command in an
// environment with a terminal (design D43, `whr console`). It is separate from
// Exec because a terminal is not a pair of pipes: it has a size, signals and one
// stream for both outputs. It fails with ErrNotFound, ErrNotOwned or
// ErrNotRunning like every method that takes an ID.
type TerminalAdapter interface {
	Terminal(ctx context.Context, envID string, req TerminalRequest) (Terminal, error)
}

// EgressUpdater is implemented by an adapter that can change what an
// environment's egress sidecar allows (design §4.2, D38). It is for the one moment
// it is safe: before an agent process starts in the environment, because the
// sidecar's address changes and a process that already runs keeps the old one.
type EgressUpdater interface {
	// UpdateEgress recreates the egress sidecar of an environment with the
	// allowlist of a prepared spec. The spec must name the environment's own
	// network and carry an Egress; nothing else of it is applied. It works on a
	// stopped and on a running environment (a running one gets the new sidecar
	// started), and fails with ErrNotFound or ErrNotOwned like every method.
	UpdateEgress(ctx context.Context, envID string, prep PreparedSpec) error
}

// Previewer is implemented by an adapter that can open a connection to a TCP
// port of an environment (design D33, issue #72): the way the preview proxy
// reaches a dev server the agent runs. The connection goes over the
// environment's internal network, through the sidecar, which relays it without
// widening what the environment may reach (§7.2); it is for that one environment
// and port, never another host. It fails with ErrNotFound, ErrNotOwned or
// ErrNotRunning like every method that takes an ID. Whether an adapter's sidecar
// can relay inbound traffic is measured, not assumed (issue #69).
type Previewer interface {
	DialPreview(ctx context.Context, envID string, port int) (net.Conn, error)
}

// InteractiveRequest is an interactive command in a running environment for the
// human's own terminal: the caller replaces its own process with the runtime's
// command line for it, so nothing the human types or the command writes passes
// through workharbor (design §7.3, "The sign-in shell"). Env holds non-secret
// variables only: they appear on a command line.
type InteractiveRequest struct {
	Cmd  []string
	Env  []string
	Dir  string
	User string // "uid:gid"; empty for the environment's own user
}

// Resources are what an environment depends on besides its own container.
type Resources struct {
	Network string   // the internal network
	Volumes []string // named volumes
	Sidecar string   // the egress proxy container, empty when the spec has none
}

// Inventory lists the networks, volumes and sidecars an owner has.
type Inventory struct {
	Networks, Volumes, Sidecars []string
	// Helpers are owned auxiliary resources, such as storage writer claims.
	Helpers []string
}

// ExecRequest is a command to run in a running environment.
type ExecRequest struct {
	Cmd []string
	Env []string
	Dir string
	// Stdin is copied into the command's standard input, which is closed when
	// the reader ends or the context is cancelled. A caller that keeps writing
	// to a pipe while the command runs can send it input as it goes (a
	// stream-json agent, design §5.2). Nil means the command's stdin is closed.
	Stdin io.Reader
}

// Stream names a chunk's source.
type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// Chunk is output produced by a command.
type Chunk struct {
	Stream Stream
	Data   []byte
}

// ExecStream is a running command. Chunks delivers output as it is produced
// and is closed when the command has ended and everything was delivered; then
// Wait returns the exit code. A caller that stops reading cancels the context
// it passed to Exec.
type ExecStream interface {
	Chunks() <-chan Chunk
	Wait() (exitCode int, err error)
}

// Collect reads a stream to the end and returns its output and exit code.
func Collect(st ExecStream) (stdout, stderr []byte, exitCode int, err error) {
	for c := range st.Chunks() {
		switch c.Stream {
		case Stdout:
			stdout = append(stdout, c.Data...)
		case Stderr:
			stderr = append(stderr, c.Data...)
		}
	}
	exitCode, err = st.Wait()
	return stdout, stderr, exitCode, err
}

// Endpoint is a temporary access point into an environment.
type Endpoint struct {
	Kind string // "ssh" | "browser"
	Addr string
}

// Adapter manages execution environments on one backend. Start, Stop and
// Delete are idempotent, so a reconciler can retry them (design §5.3), and
// every method that takes an ID refuses an environment the adapter does not
// own with ErrNotOwned.
type Adapter interface {
	Name() string
	Capabilities() Capabilities

	// Provision creates an environment, stopped, from a spec that Prepare
	// checked (ErrNotPrepared otherwise). It also creates the environment's
	// internal network, the volumes it names and the egress sidecar.
	Provision(ctx context.Context, spec PreparedSpec) (envID string, err error)
	Start(ctx context.Context, envID string) error
	Stop(ctx context.Context, envID string) error
	// Delete removes one stopped environment by its exact ID, never by pattern
	// (ErrRunning if it is running, as the environment machine of design §4.1
	// deletes only from stopped), together with its internal network and its
	// egress sidecar. Its volumes stay: the agent home survives a delete and a
	// rebuild (design §4.4), and RemoveVolume discards one.
	Delete(ctx context.Context, envID string) error
	// RemoveVolume discards one volume by its exact name (ErrVolumeBusy if a
	// running environment holds it). Removing one that is gone succeeds.
	RemoveVolume(ctx context.Context, name string) error
	// Resources reports the network, volumes and sidecar of an environment.
	Resources(ctx context.Context, envID string) (Resources, error)
	// Inventory lists every network, volume and sidecar this adapter's owner
	// has, so a conformance run can show that a delete left nothing behind.
	Inventory(ctx context.Context) (Inventory, error)

	Inspect(ctx context.Context, envID string) (Info, error)
	// List returns the environments with this owner label and no others.
	List(ctx context.Context, owner string) ([]Info, error)

	// Exec runs a command in a running environment (ErrNotRunning otherwise).
	Exec(ctx context.Context, envID string, req ExecRequest) (ExecStream, error)
	Logs(ctx context.Context, envID string) ([]byte, error)
	Endpoints(ctx context.Context, envID string) ([]Endpoint, error)
}
