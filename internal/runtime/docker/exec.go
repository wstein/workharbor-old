package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/wstein/workharbor/internal/runtime"
)

var environmentKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func environmentData(env []string) ([]byte, error) {
	var b strings.Builder
	seen := map[string]bool{}
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || !environmentKey.MatchString(k) || seen[k] || strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("docker: invalid exec environment entry")
		}
		seen[k] = true
		b.WriteString(entry)
		b.WriteByte('\n')
	}
	if b.Len() > 1<<20 {
		return nil, errors.New("docker: exec environment exceeds limit")
	}
	return []byte(b.String()), nil
}

type execStream struct {
	chunks chan runtime.Chunk
	done   chan struct{}
	code   int
	err    error
}

func (s *execStream) Chunks() <-chan runtime.Chunk { return s.chunks }
func (s *execStream) Wait() (int, error)           { <-s.done; return s.code, s.err }

func (a *Adapter) awaitTerminal(ctx context.Context, id string) (int, error) {
	for {
		running, code, err := a.execStatus(ctx, id)
		if err != nil {
			return 0, err
		}
		if !running {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Exec wraps every command in the trusted shim. Cancellation ends the guest
// process group before closing the client socket; closing an Engine exec socket
// alone does not stop the guest command.
func (a *Adapter) Exec(ctx context.Context, id string, req runtime.ExecRequest) (runtime.ExecStream, error) {
	c, err := a.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if !c.State.Running {
		return nil, runtime.ErrNotRunning
	}
	if err := checkStagingParent(c); err != nil {
		return nil, err
	}
	if len(req.Cmd) == 0 {
		return nil, errors.New("docker: exec command is required")
	}
	data, err := environmentData(req.Env)
	if err != nil {
		return nil, err
	}
	nonce, err := token()
	if err != nil {
		return nil, err
	}
	dir := runtime.PrivateExecTarget + "/exec-" + nonce
	if err := a.uploadEnvironment(ctx, c, dir, data); err != nil {
		return nil, err
	}
	cleanup := func(ctx context.Context) error { return a.cleanupEnvironment(ctx, id, dir) }
	cmd := []string{a.shim, "run", "-pidfile", dir + "/pid", "-env-file", dir + "/env", "-cancel-file", dir + "/cancel", "--"}
	cmd = append(cmd, req.Cmd...)
	ex, err := a.attach(ctx, id, cmd, req.Dir, req.Stdin != nil)
	if err != nil {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cleanupLimit())
		defer cancel()
		if ex.sent {
			killErr := a.command(cleanCtx, id, []string{a.shim, "kill", "-pidfile", dir + "/pid", "-cancel-file", dir + "/cancel", "-grace", "2s"})
			return nil, errors.Join(err, killErr, errors.New("docker: launch outcome uncertain; private exec state retained; operator inspection required"))
		}
		return nil, errors.Join(err, cleanup(cleanCtx))
	}
	if req.Stdin != nil {
		go func() {
			_, _ = io.Copy(ex.conn, req.Stdin)
			if uc, ok := ex.conn.(*net.UnixConn); ok {
				_ = uc.CloseWrite()
			}
		}()
	}
	st := &execStream{chunks: make(chan runtime.Chunk, 16), done: make(chan struct{})}
	pumpCtx, pumpCancel := context.WithCancel(ctx)
	readDone := make(chan error, 1)
	go func() { readDone <- readFrames(pumpCtx, ex.reader, st.chunks) }()
	go func() {
		defer close(st.done)
		defer close(st.chunks)
		defer pumpCancel()
		defer func() { _ = ex.conn.Close() }()
		readFinished := false
		var streamErr error
		select {
		case streamErr = <-readDone:
			readFinished = true
		case <-ctx.Done():
		}
		controlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cleanupLimit())
		defer cancel()
		// Output EOF does not prove that the guest is terminal. Keep observing
		// cancellation throughout status polling, with one bounded kill request.
		killTrigger := make(chan struct{})
		if streamErr != nil {
			close(killTrigger)
		}
		stopCancellation := make(chan struct{})
		cancellationDone := make(chan error, 1)
		go func() {
			select {
			case <-ctx.Done():
			case <-killTrigger:
			case <-stopCancellation:
				cancellationDone <- pendingCancellation(ctx, killTrigger, func() error {
					return a.command(controlCtx, id, []string{a.shim, "kill", "-pidfile", dir + "/pid", "-cancel-file", dir + "/cancel", "-grace", "2s"})
				})
				return
			}
			cancellationDone <- a.command(controlCtx, id, []string{a.shim, "kill", "-pidfile", dir + "/pid", "-cancel-file", dir + "/cancel", "-grace", "2s"})
		}()
		finishCancellation := func() error { close(stopCancellation); return <-cancellationDone }
		if !readFinished {
			select {
			case streamErr = <-readDone:
			case <-controlCtx.Done():
				_ = ex.conn.Close()
				<-readDone
				st.err = errors.Join(ctx.Err(), finishCancellation(), errors.New("docker: guest launcher did not terminate; private cancellation state retained; operator inspection required"))
				st.code = 130
				return
			}
		}
		code, statusErr := a.awaitTerminal(controlCtx, ex.id)
		st.code = code
		cancelErr := finishCancellation()
		st.err = errors.Join(ctx.Err(), cancelErr, streamErr, statusErr)
		if ctx.Err() != nil {
			st.code = 130
		}
		if streamErr != nil || statusErr != nil {
			st.err = errors.Join(st.err, errors.New("docker: private exec state retained; operator inspection required"))
			return
		}
		st.err = errors.Join(st.err, cleanup(controlCtx))
	}()
	return st, nil
}

// pendingCancellation checks protection requested before the watcher stopped.
func pendingCancellation(ctx context.Context, trigger <-chan struct{}, kill func() error) error {
	select {
	case <-ctx.Done():
		return kill()
	case <-trigger:
		return kill()
	default:
		return nil
	}
}
