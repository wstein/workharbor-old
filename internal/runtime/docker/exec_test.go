package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/runtime"
)

func TestExecStreamsOutputAndUsesPrivateFileForSecrets(t *testing.T) {
	const secret = "test-value-with-spaces=$literal"
	archiveSeen := false
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
			_ = json.NewEncoder(w).Encode(stagingVolume())
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/"):
			_ = json.NewEncoder(w).Encode(stagingContainer())
		case strings.HasSuffix(r.URL.Path, "/archive"):
			tr := tar.NewReader(r.Body)
			header, err := tr.Next()
			if err != nil {
				t.Error(err)
				return
			}
			if r.URL.Query().Get("path") == runtime.PrivateExecTarget {
				if header.Typeflag != tar.TypeDir || header.Mode != 0o700 {
					t.Error("ownership handoff parent is not private")
				}
			} else {
				if header.Typeflag != tar.TypeReg || header.Mode != 0o600 || header.Name != "env" {
					t.Error("environment file is not private")
				}
				data, _ := io.ReadAll(tr)
				if string(data) != "TOKEN="+secret+"\n" {
					t.Error("archive changed environment value")
				}
				archiveSeen = true
			}
		case strings.HasSuffix(r.URL.Path, "/exec"):
			data, _ := io.ReadAll(r.Body)
			if bytes.Contains(data, []byte(secret)) {
				t.Error("secret reached exec request")
			}
			_, _ = io.WriteString(w, `{"Id":"exec1"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			var start map[string]any
			_ = json.NewDecoder(r.Body).Decode(&start)
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			for _, frame := range []struct {
				stream byte
				data   string
			}{{1, "out\n"}, {2, "err\n"}} {
				header := make([]byte, 8)
				header[0] = frame.stream
				binary.BigEndian.PutUint32(header[4:], uint32(len(frame.data))) //nolint:gosec // tiny constant fixture frames
				_, _ = buf.Write(header)
				_, _ = buf.WriteString(frame.data)
			}
			_ = buf.Flush()
		case strings.Contains(r.URL.Path, "/exec/"):
			_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.Exec(context.Background(), "abcdef", runtime.ExecRequest{Cmd: []string{"echo", "hi"}, Env: []string{"TOKEN=" + secret}})
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, code, err := runtime.Collect(st)
	if err != nil || string(out) != "out\n" || string(stderr) != "err\n" || code != 0 {
		t.Fatalf("stream/code mismatch: code=%d err=%v", code, err)
	}
	if !archiveSeen {
		t.Fatal("environment was not transported through a private file")
	}
}

func TestCancelledExecSignalsGuestBeforeRemovingPrivateState(t *testing.T) {
	mainStarted := make(chan struct{})
	guestKilled := make(chan struct{})
	var mu sync.Mutex
	commands := map[string]string{}
	next := 0
	cleaned := false
	stopped := false
	statusReads := 0
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
			_ = json.NewEncoder(w).Encode(stagingVolume())
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(stagingContainer())
		case strings.HasSuffix(r.URL.Path, "/archive"):
			_, _ = io.Copy(io.Discard, r.Body)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			kind := "main"
			if len(body.Cmd) > 1 && body.Cmd[1] == "reserve-env" {
				kind = "reserve"
			}
			if len(body.Cmd) > 1 && body.Cmd[1] == "kill" {
				kind = "kill"
			}
			if len(body.Cmd) > 1 && (body.Cmd[1] == "clear-env" || body.Cmd[1] == "release-env") {
				kind = "cleanup"
			}
			mu.Lock()
			next++
			id := fmt.Sprintf("e%d", next)
			commands[id] = kind
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.Contains(r.URL.Path, "/exec/"):
			bits := strings.Split(r.URL.Path, "/")
			id := bits[len(bits)-2]
			mu.Lock()
			kind := commands[id]
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "/start") {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				_ = buf.Flush()
				switch kind {
				case "main":
					close(mainStarted)
					<-guestKilled
				case "kill":
					close(guestKilled)
				case "cleanup":
					select {
					case <-guestKilled:
					default:
						t.Error("private state removed before guest cancellation")
					}
					mu.Lock()
					if !stopped {
						t.Error("private marker removed before guest launcher reached terminal state")
					}
					cleaned = true
					mu.Unlock()
				}
			} else {
				if kind == "main" {
					mu.Lock()
					statusReads++
					stopped = statusReads >= 2
					running := !stopped
					mu.Unlock()
					_, _ = fmt.Fprintf(w, `{"Running":%t,"ExitCode":0}`, running)
				} else {
					_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				}
			}
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	st, err := a.Exec(ctx, "abcdef", runtime.ExecRequest{Cmd: []string{"sleep", "30"}, Stdin: reader})
	if err != nil {
		t.Fatal(err)
	}
	<-mainStarted
	cancel()
	_, _, _, err = runtime.Collect(st)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !cleaned {
		t.Fatal("private state cleanup was not attempted")
	}
}

func TestExecFailureRetainsStateUntilGuestTerminationIsCertain(t *testing.T) {
	for _, mode := range []string{"before start", "start response lost", "inspection interrupted", "cancelled inspection", "guest never terminates", "status never terminal"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			kinds := map[string]string{}
			next := 0
			cleaned, killed := false, false
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
					_ = json.NewEncoder(w).Encode(stagingVolume())
				case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
					_ = json.NewEncoder(w).Encode(stagingContainer())
				case strings.HasSuffix(r.URL.Path, "/archive"):
					_, _ = io.Copy(io.Discard, r.Body)
				case strings.HasSuffix(r.URL.Path, "/exec"):
					var body struct{ Cmd []string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					kind := "main"
					if len(body.Cmd) > 1 && body.Cmd[1] == "reserve-env" {
						kind = "reserve"
					}
					if len(body.Cmd) > 1 && body.Cmd[1] == "kill" {
						kind = "kill"
					}
					if len(body.Cmd) > 1 && (body.Cmd[1] == "clear-env" || body.Cmd[1] == "release-env") {
						kind = "cleanup"
					}
					if kind == "main" && mode == "before start" {
						w.WriteHeader(500)
						return
					}
					mu.Lock()
					next++
					id := fmt.Sprintf("e%d", next)
					kinds[id] = kind
					mu.Unlock()
					_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
				case strings.Contains(r.URL.Path, "/exec/"):
					bits := strings.Split(r.URL.Path, "/")
					id := bits[len(bits)-2]
					mu.Lock()
					kind := kinds[id]
					mu.Unlock()
					if strings.HasSuffix(r.URL.Path, "/start") {
						conn, buf, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						if kind == "main" && mode == "start response lost" {
							return
						}
						_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
						_ = buf.Flush()
						mu.Lock()
						if kind == "cleanup" {
							cleaned = true
						}
						if kind == "kill" {
							killed = true
						}
						mu.Unlock()
						if kind == "main" && mode == "guest never terminates" {
							close(started)
							<-release
						}
					} else {
						if kind == "main" && mode == "status never terminal" {
							_, _ = io.WriteString(w, `{"Running":true,"ExitCode":0}`)
							return
						}
						if kind == "main" && (mode == "inspection interrupted" || mode == "cancelled inspection") {
							if mode == "cancelled inspection" {
								cancel()
							}
							w.WriteHeader(500)
							return
						}
						_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			a, err := New("mine", endpoint, "/tools/whr-shim")
			if err != nil {
				t.Fatal(err)
			}
			a.cleanupTimeout = 50 * time.Millisecond
			st, err := a.Exec(ctx, "abcdef", runtime.ExecRequest{Cmd: []string{"sleep", "30"}})
			if mode == "guest never terminates" {
				if err != nil {
					t.Fatal(err)
				}
				<-started
				cancel()
			}
			if st != nil {
				_, _, _, err = runtime.Collect(st)
			}
			if err == nil {
				t.Fatal("expected explicit execution failure")
			}
			mu.Lock()
			defer mu.Unlock()
			if cleaned != (mode == "before start") {
				t.Fatal("private exec state removed despite uncertain guest termination")
			}
			if (mode == "start response lost" || mode == "guest never terminates" || mode == "cancelled inspection") && !killed {
				t.Fatal("protective cancellation marker was not requested")
			}
		})
	}
}

func TestExecForwardsStdinWhileCommandRunsAndClosesAtEOF(t *testing.T) {
	var mu sync.Mutex
	commands := map[string]bool{}
	next := 0
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
			_ = json.NewEncoder(w).Encode(stagingVolume())
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(stagingContainer())
		case strings.HasSuffix(r.URL.Path, "/archive"):
			_, _ = io.Copy(io.Discard, r.Body)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				Cmd         []string
				AttachStdin bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			next++
			id := fmt.Sprintf("e%d", next)
			commands[id] = body.AttachStdin
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.Contains(r.URL.Path, "/exec/"):
			if !strings.HasSuffix(r.URL.Path, "/start") {
				_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				return
			}
			bits := strings.Split(r.URL.Path, "/")
			id := bits[len(bits)-2]
			mu.Lock()
			hasInput := commands[id]
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buf.Flush()
			if hasInput {
				writeTestFrame(t, buf, 1, []byte("ready\n"))
				_ = buf.Flush()
				data, err := io.ReadAll(buf)
				if err != nil {
					t.Error(err)
					return
				}
				writeTestFrame(t, buf, 1, data)
				_ = buf.Flush()
			}
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	a, err := New("mine", endpoint, "/tools/whr-shim")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := a.Exec(ctx, "abcdef", runtime.ExecRequest{Cmd: []string{"cat"}, Stdin: reader})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-st.Chunks():
		if string(chunk.Data) != "ready\n" {
			t.Fatal("command did not stream before stdin EOF")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := io.WriteString(writer, "later input\n"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	out, _, code, err := runtime.Collect(st)
	if err != nil || code != 0 || string(out) != "later input\n" {
		t.Fatalf("stdin forwarding/code failed: code=%d err=%v", code, err)
	}
}

func writeTestFrame(t *testing.T, w io.Writer, stream byte, data []byte) {
	t.Helper()
	var header [8]byte
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(data))) //nolint:gosec // bounded tiny test data
	if _, err := w.Write(header[:]); err != nil {
		t.Error(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Error(err)
	}
}

func TestInvalidExecEnvironmentNeverEntersControlRequests(t *testing.T) {
	const marker = "private-regression-marker"
	for _, env := range [][]string{{"BAD-KEY=" + marker}, {"TOKEN=" + marker + "\nOTHER=value"}, {"TOKEN=" + marker + "\x00"}, {"TOKEN=" + marker, "TOKEN=duplicate"}, {"TOKEN=" + strings.Repeat("x", 1<<20)}} {
		mutations := 0
		endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				mutations++
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode(stagingContainer())
		}))
		a, _ := New("mine", endpoint, "/tools/whr-shim")
		if _, err := a.Exec(context.Background(), "abcdef", runtime.ExecRequest{Cmd: []string{"true"}, Env: env}); err == nil || strings.Contains(err.Error(), marker) {
			t.Fatalf("invalid environment reporting=%v", err)
		}
		if mutations != 0 {
			t.Fatal("invalid environment reached daemon mutation")
		}
	}
}

func TestCancelAfterOutputEOF(t *testing.T) {
	var mu sync.Mutex
	kinds := map[string]string{}
	polled := make(chan struct{})
	var once sync.Once
	killed, cleaned := false, false
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
			_ = json.NewEncoder(w).Encode(stagingVolume())
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/"):
			_ = json.NewEncoder(w).Encode(stagingContainer())
		case strings.HasSuffix(r.URL.Path, "/archive"):
			_, _ = io.Copy(io.Discard, r.Body)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			kind := body.Cmd[1]
			mu.Lock()
			kinds[kind] = kind
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": kind})
		case strings.HasPrefix(r.URL.Path, "/v1.45/exec/"):
			parts := strings.Split(r.URL.Path, "/")
			mu.Lock()
			kind := kinds[parts[len(parts)-2]]
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "/json") {
				running := false
				if kind == "run" {
					mu.Lock()
					running = !killed
					mu.Unlock()
					once.Do(func() { close(polled) })
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Running": running, "ExitCode": 0})
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			mu.Lock()
			if kind == "kill" {
				killed = true
			}
			if kind == "clear-env" || kind == "release-env" {
				if !killed {
					t.Error("cleanup preceded cancellation termination")
				}
				cleaned = true
			}
			mu.Unlock()
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buf.Flush()
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	a, _ := New("mine", endpoint, "/tools/whr-shim")
	a.cleanupTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := a.Exec(ctx, "abcdef", runtime.ExecRequest{Cmd: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-polled:
	case <-time.After(time.Second):
		t.Fatal("guest terminal polling never began")
	}
	cancel()
	_, _, _, err = runtime.Collect(stream)
	mu.Lock()
	defer mu.Unlock()
	if !killed || !cleaned || !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation killed=%v cleaned=%v err=%v", killed, cleaned, err)
	}
}

func TestStoppedCancellationWatcherHonorsPendingProtection(t *testing.T) {
	for _, cause := range []string{"caller cancellation", "stream failure", "normal completion"} {
		t.Run(cause, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			trigger := make(chan struct{})
			if cause == "caller cancellation" {
				cancel()
			}
			if cause == "stream failure" {
				close(trigger)
			}
			// Exercise the stop-selected branch directly: a failed terminal
			// inspection can stop the watcher before it is first scheduled.
			called := false
			killErr := errors.New("protective kill failed")
			err := pendingCancellation(ctx, trigger, func() error { called = true; return killErr })
			want := cause != "normal completion"
			if called != want || errors.Is(err, killErr) != want {
				t.Fatalf("pending protection called=%v err=%v", called, err)
			}
		})
	}
}
