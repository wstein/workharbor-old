package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wstein/workharbor/internal/runtime"
)

var execPrivatePath = regexp.MustCompile(`^/whr-exec/exec-[0-9a-f]{32}$`)

// uploadEnvironment sends secrets only as a mode-0600 file in a random private
// parent. No Env field in an Engine exec request contains a secret.
func (a *Adapter) uploadEnvironment(ctx context.Context, c container, dir string, data []byte) error {
	if !execPrivatePath.MatchString(dir) {
		return errors.New("docker: invalid private execution path")
	}
	if err := checkStagingParent(c); err != nil {
		return err
	}
	if _, present, err := a.inspectPrivateVolume(ctx, c); err != nil {
		return err
	} else if !present {
		return fmt.Errorf("%w: checked private execution storage is required", ErrUnsupported)
	}
	id, user := c.ID, c.Config.User
	uid, gid, ok := strings.Cut(user, ":")
	if !ok {
		gid = uid
	}
	u, err := strconv.Atoi(uid)
	if err != nil || u <= 0 {
		return errors.New("docker: exec requires a numeric non-root container user")
	}
	g, err := strconv.Atoi(gid)
	if err != nil || g < 0 {
		return errors.New("docker: exec requires a numeric container group")
	}
	stageCtx, stopStage := context.WithTimeout(ctx, a.cleanupLimit())
	defer stopStage()
	ctx = stageCtx
	if err := a.reserveEnvironment(ctx, id, dir); err != nil {
		return errors.Join(err, errors.New("docker: metadata reservation uncertain; private state retained; operator inspection required"))
	}
	// Guest processes cannot change the root-owned 0700 parent during upload.
	// Engine preserves the guest UID on the protected regular file, then a
	// separate metadata-only archive hands the parent to that UID.
	file := &tar.Header{Name: "env", Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(data)), Uid: u, Gid: g}
	if err := a.uploadArchive(ctx, id, dir, file, data); err != nil {
		return errors.Join(err, errors.New("docker: protected file transfer uncertain; private state retained; operator inspection required"))
	}
	parent := &tar.Header{Name: path.Base(dir) + "/", Mode: 0o700, Typeflag: tar.TypeDir, Uid: u, Gid: g}
	if err := a.uploadArchive(ctx, id, runtime.PrivateExecTarget, parent, nil); err != nil {
		return errors.Join(err, errors.New("docker: ownership handoff uncertain; private state retained; operator inspection required"))
	}
	return nil
}

func (a *Adapter) uploadArchive(ctx context.Context, id, destination string, header *tar.Header, data []byte) error {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(header); err != nil {
		return errors.New("docker: cannot prepare protected archive")
	}
	if _, err := tw.Write(data); err != nil {
		return errors.New("docker: cannot prepare protected archive")
	}
	if err := tw.Close(); err != nil {
		return errors.New("docker: cannot finish protected archive")
	}
	route := "/containers/" + url.PathEscape(id) + "/archive?path=" + url.QueryEscape(destination) + "&noOverwriteDirNonDir=true&copyUIDGID=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://engine/v1.45"+route, &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := a.client.Do(req)
	if err != nil {
		return errors.New("docker: protected file transfer failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &apiError{res.StatusCode}
	}
	return nil
}

type attachedExec struct {
	id     string
	conn   net.Conn
	reader *bufio.Reader
	sent   bool
}

// attach uses Engine's HTTP upgrade to carry both stdin and multiplexed output.
// Using a plain response body would lose the write half needed by stream-json.
func (a *Adapter) attach(ctx context.Context, id string, cmd []string, dir string, stdin bool) (attachedExec, error) {
	return a.attachCommand(ctx, id, cmd, dir, stdin, false)
}

func (a *Adapter) attachCommand(ctx context.Context, id string, cmd []string, dir string, stdin, metadata bool) (attachedExec, error) {
	var made struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"AttachStdout": true, "AttachStderr": true, "AttachStdin": stdin, "Tty": false, "Cmd": cmd, "WorkingDir": dir}
	if metadata {
		// Root execution is internal and restricted to the token-free reservation
		// grammar. No public ExecRequest can override its user or privileges.
		if stdin || dir != "" || len(cmd) != 4 || cmd[0] != a.shim || (cmd[1] != "reserve-env" && cmd[1] != "release-env") || cmd[2] != "-dir" || !execPrivatePath.MatchString(cmd[3]) {
			return attachedExec{}, errors.New("docker: invalid metadata reservation")
		}
		body["User"] = "0"
		body["Privileged"] = false
	}

	if err := a.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", body, &made); err != nil {
		return attachedExec{}, err
	}
	u, _ := url.Parse(a.endpoint)
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
	if err != nil {
		return attachedExec{}, err
	}
	stopHandshake := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopHandshake()
	failed := true
	defer func() {
		if failed {
			_ = conn.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	data := bytes.NewBufferString(`{"Detach":false,"Tty":false}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://engine/v1.45/exec/"+url.PathEscape(made.ID)+"/start", data)
	if err != nil {
		return attachedExec{}, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(conn); err != nil {
		return attachedExec{id: made.ID, sent: true}, err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		return attachedExec{id: made.ID, sent: true}, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		_ = res.Body.Close()
		return attachedExec{id: made.ID, sent: true}, fmt.Errorf("%w: Engine exec HTTP upgrade (HTTP %d)", ErrUnsupported, res.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	failed = false
	return attachedExec{id: made.ID, conn: conn, reader: br, sent: true}, nil
}

func readFrames(ctx context.Context, r io.Reader, chunks chan<- runtime.Chunk) error {
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if h[0] != 1 && h[0] != 2 || h[1] != 0 || h[2] != 0 || h[3] != 0 {
			return errors.New("docker: invalid exec stream header")
		}
		n := binary.BigEndian.Uint32(h[4:])
		if n > 8<<20 {
			return errors.New("docker: exec stream frame exceeds limit")
		}
		data := make([]byte, int(n))
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		which := runtime.Stdout
		if h[0] == 2 {
			which = runtime.Stderr
		}
		select {
		case chunks <- runtime.Chunk{Stream: which, Data: data}:
		case <-ctx.Done():
			// Keep draining until the guest closes its stream. Caller cancellation
			// must not masquerade as guest termination or remove its cancel marker.
			continue
		}
	}
}

func (a *Adapter) execStatus(ctx context.Context, id string) (bool, int, error) {
	var status struct {
		Running  bool
		ExitCode int
	}
	err := a.request(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil, &status)
	return status.Running, status.ExitCode, err
}

func (a *Adapter) command(ctx context.Context, id string, cmd []string) error {
	ex, err := a.attach(ctx, id, cmd, "", false)
	if err != nil {
		return err
	}
	return a.finishControl(ctx, ex)
}

func (a *Adapter) reserveEnvironment(ctx context.Context, id, dir string) error {
	ex, err := a.attachCommand(ctx, id, []string{a.shim, "reserve-env", "-dir", dir}, "", false, true)
	if err != nil {
		return err
	}
	return a.finishControl(ctx, ex)
}

func (a *Adapter) finishControl(ctx context.Context, ex attachedExec) error {
	defer func() { _ = ex.conn.Close() }()
	// Internal control commands have bounded contexts and no caller channels.
	if deadline, ok := ctx.Deadline(); ok {
		_ = ex.conn.SetDeadline(deadline)
	}
	discarded := make(chan runtime.Chunk, 16)
	done := make(chan struct{})
	go func() {
		for range discarded {
		}
		close(done)
	}()
	err := readFrames(ctx, ex.reader, discarded)
	close(discarded)
	<-done
	if err != nil {
		return err
	}
	running, code, err := a.execStatus(ctx, ex.id)
	if err != nil {
		return err
	}
	if running || code != 0 {
		return errors.New("docker: guest control command failed")
	}
	return nil
}

func (a *Adapter) cleanupLimit() time.Duration {
	if a.cleanupTimeout > 0 {
		return a.cleanupTimeout
	}
	return 10 * time.Second
}

var _ runtime.Adapter = (*Adapter)(nil)

// checkStagingParent binds the transfer boundary to the inspected configuration.
// The trusted root reservation helper independently fstats its opened parent.
func checkStagingParent(c container) error {
	if !c.HostConfig.ReadonlyRootfs {
		return fmt.Errorf("%w: private execution requires the read-only root", ErrUnsupported)
	}
	_, present, err := privateContainerPlan(c)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: checked private execution storage is required", ErrUnsupported)
	}
	for _, mount := range c.Mounts {
		target := path.Clean(mount.Destination)
		if target == runtime.PrivateExecTarget && mount.Type == "volume" && mount.Name == c.Config.Labels[privateVolumeLabel] && mount.RW {
			continue
		}
		if target == "/" || target == runtime.PrivateExecTarget || strings.HasPrefix(target, runtime.PrivateExecTarget+"/") || strings.HasPrefix(runtime.PrivateExecTarget, target+"/") {
			return fmt.Errorf("%w: conflicting private execution mount", ErrUnsupported)
		}
	}
	for target := range c.HostConfig.Tmpfs {
		target = path.Clean(target)
		if target == "/" || target == runtime.PrivateExecTarget || strings.HasPrefix(target, runtime.PrivateExecTarget+"/") || strings.HasPrefix(runtime.PrivateExecTarget, target+"/") {
			return fmt.Errorf("%w: conflicting private execution tmpfs", ErrUnsupported)
		}
	}
	return nil
}

// cleanupEnvironment is called only after the actual guest launcher is known
// terminal. Unknown contents or control outcomes retain state and return error.
func (a *Adapter) cleanupEnvironment(ctx context.Context, id, dir string) error {
	if !execPrivatePath.MatchString(dir) {
		return errors.New("docker: invalid private cleanup path")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, a.cleanupLimit())
	defer cancel()
	if err := a.command(cleanupCtx, id, []string{a.shim, "clear-env", "-dir", dir}); err != nil {
		return err
	}
	ex, err := a.attachCommand(cleanupCtx, id, []string{a.shim, "release-env", "-dir", dir}, "", false, true)
	if err != nil {
		return err
	}
	return a.finishControl(cleanupCtx, ex)
}
