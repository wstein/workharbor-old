package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/runtime"
)

func TestEnvironmentTransferReservesRootBeforeProtectedUploadAndMetadataHandoff(t *testing.T) {
	const dir = "/whr-exec/exec-0123456789abcdef0123456789abcdef"
	const secret = "TOKEN=private-value\n"
	reserved, uploaded, handedOff := false, false, false
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
			_ = json.NewEncoder(w).Encode(stagingVolume())
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				Cmd                     []string
				User                    string
				Privileged, AttachStdin bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			expected := []string{"/tools/whr-shim", "reserve-env", "-dir", dir}
			if strings.Join(body.Cmd, "|") != strings.Join(expected, "|") || body.User != "0" || body.Privileged || body.AttachStdin {
				t.Errorf("root control exceeded fixed metadata grammar: %+v", body)
			}
			_, _ = io.WriteString(w, `{"Id":"reserve"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			reserved = true
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buf.Flush()
		case strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		case strings.HasSuffix(r.URL.Path, "/archive"):
			tr := tar.NewReader(r.Body)
			header, err := tr.Next()
			if err != nil {
				t.Error(err)
				return
			}
			if !reserved {
				t.Error("secret transfer preceded exclusive root reservation")
			}
			if r.URL.Query().Get("copyUIDGID") != "true" {
				t.Error("ownership transfer not explicit")
			}
			if r.URL.Query().Get("path") == dir {
				if header.Name != "env" || header.Typeflag != tar.TypeReg || header.Mode != 0o600 || header.Uid != 1000 || header.Gid != 1000 || handedOff {
					t.Errorf("protected payload header = %+v", header)
				}
				data, err := io.ReadAll(tr)
				if err != nil || string(data) != secret {
					t.Error("protected payload changed")
				}
				uploaded = true
			} else {
				if r.URL.Query().Get("path") != runtime.PrivateExecTarget || !uploaded || header.Name != strings.TrimPrefix(dir, runtime.PrivateExecTarget+"/")+"/" || header.Typeflag != tar.TypeDir || header.Mode != 0o700 || header.Uid != 1000 || header.Gid != 1000 || header.Size != 0 {
					t.Errorf("metadata handoff before upload or incorrect: %+v", header)
				}
				handedOff = true
			}
			if _, err := tr.Next(); err != io.EOF {
				t.Error("archive contained extra paths or payload")
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
	if err := a.uploadEnvironment(context.Background(), stagingContainer(), dir, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	if !reserved || !uploaded || !handedOff {
		t.Fatal("protected staging sequence incomplete")
	}
}

func stagingContainer() container {
	c := privateContainer()
	c.Config.User = "1000:1000"
	c.State.Running = true
	c.HostConfig.ReadonlyRootfs = true
	return c
}

func stagingVolume() named {
	v := named{Name: "task-exec", Driver: "local", Options: privateVolumeOptions(privatePlan()), Labels: labels("mine", privateVolumeRole)}
	v.Labels[privateAssociationLabel], v.Labels[privateSizeLabel], v.Labels[privateNonceLabel] = "task-net", "2", "identity"
	return v
}

func TestTransferRefusesUnsafeStagingBoundaryBeforeMutation(t *testing.T) {
	for _, kind := range []string{"missing plan", "nested mount", "writable root", "disk fallback", "foreign volume", "symlink target alias"} {
		t.Run(kind, func(t *testing.T) {
			c := stagingContainer()
			v := stagingVolume()
			switch kind {
			case "missing plan":
				delete(c.Config.Labels, privateVolumeLabel)
			case "nested mount":
				m := c.Mounts[0]
				m.Destination = runtime.PrivateExecTarget + "/shadow"
				m.Name = "other"
				c.Mounts = append(c.Mounts, m)
			case "writable root":
				c.HostConfig.ReadonlyRootfs = false
			case "disk fallback":
				v.Options["type"] = "none"
			case "foreign volume":
				v.Labels[runtime.OwnerLabel] = "other"
			case "symlink target alias":
				c.Mounts[0].Destination = "/elsewhere"
			}
			mutations := 0
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(v)
			}))
			a, _ := New("mine", endpoint, "/tools/whr-shim")
			if err := a.uploadEnvironment(context.Background(), c, "/whr-exec/exec-0123456789abcdef0123456789abcdef", []byte("TOKEN=secret\n")); err == nil {
				t.Fatal("unsafe staging boundary accepted")
			}
			if mutations != 0 {
				t.Fatal("unsafe boundary received secret/control mutation")
			}
		})
	}
}

func TestProtectedTransferFailuresRetainStateWithoutLeakingSecrets(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			const secret = "do-not-print-protected-value" //nolint:gosec // sentinel for leak regression
			controls, archives := 0, 0
			endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1.45/volumes/"):
					_ = json.NewEncoder(w).Encode(stagingVolume())
				case strings.HasSuffix(r.URL.Path, "/exec"):
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if strings.Contains(string(raw), secret) {
						t.Error("secret entered control argv or Engine Env")
					}
					controls++
					_, _ = io.WriteString(w, `{"Id":"reserve"}`)
				case strings.HasSuffix(r.URL.Path, "/start"):
					_, _ = io.Copy(io.Discard, r.Body)
					conn, buf, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					_ = buf.Flush()
				case strings.HasSuffix(r.URL.Path, "/json"):
					_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				case strings.HasSuffix(r.URL.Path, "/archive"):
					_, _ = io.Copy(io.Discard, r.Body)
					archives++
					if archives == failAt {
						w.WriteHeader(500)
						_, _ = io.WriteString(w, secret)
					} else {
						w.WriteHeader(200)
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			a, _ := New("mine", endpoint, "/tools/whr-shim")
			err := a.uploadEnvironment(context.Background(), stagingContainer(), "/whr-exec/exec-0123456789abcdef0123456789abcdef", []byte("TOKEN="+secret+"\n"))
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("unsafe failure reporting: %v", err)
			}
			if controls != 1 || archives != failAt {
				t.Fatalf("continued after uncertain transfer: controls=%d archives=%d", controls, archives)
			}
		})
	}
}

func TestRootMetadataTransportRejectsGenericCommandsAndUntrustedPaths(t *testing.T) {
	calls := 0
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	a, _ := New("mine", endpoint, "/tools/whr-shim")
	for _, cmd := range [][]string{
		{"/bin/sh", "-c", "anything"},
		{a.shim, "run", "-dir", "/whr-exec/exec-0123456789abcdef0123456789abcdef"},
		{a.shim, "reserve-env", "-dir", "/tmp/whr-exec-0123456789abcdef0123456789abcdef"},
		{a.shim, "release-env", "-dir", "/whr-exec/exec-0123456789abcdef0123456789abcdef/child"},
	} {
		if _, err := a.attachCommand(context.Background(), "abcdef", cmd, "", false, true); err == nil {
			t.Fatal("root override accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid metadata request reached Engine")
	}
}

func TestPrivateCleanupUsesGuestKnownFilesThenRootEmptyRelease(t *testing.T) {
	order := []string{}
	endpoint := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				Cmd                     []string
				User                    string
				Privileged, AttachStdin bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			order = append(order, body.Cmd[1])
			wantUser := ""
			if body.Cmd[1] == "release-env" {
				wantUser = "0"
			}
			if body.User != wantUser || body.Privileged || body.AttachStdin {
				t.Errorf("cleanup privilege boundary: %+v", body)
			}
			_, _ = io.WriteString(w, `{"Id":"cleanup"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buf.Flush()
		case strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	a, _ := New("mine", endpoint, "/tools/whr-shim")
	if err := a.cleanupEnvironment(context.Background(), "abcdef", "/whr-exec/exec-0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "clear-env,release-env" {
		t.Fatalf("cleanup order=%v", order)
	}
}
