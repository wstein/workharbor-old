package serve

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// This Engine models an existing owned environment independently of Provision,
// which intentionally remains quota-blocked. No guest artifact or daemon runs.
func factoryExecutionEngine(t *testing.T, listed *atomic.Bool, receivedPath *atomic.Value) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.45/containers/json":
			listed.Store(true)
			_, _ = io.WriteString(w, "[]")
		case r.URL.Path == "/v1.45/containers/abcdef/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "abcdef", "Config": map[string]any{"User": "1000:1000", "Env": []string{"PATH=/usr/local/go/bin:/usr/bin:/bin"}, "Labels": map[string]string{
					"workharbor.owner": Owner, "workharbor.role": "environment", "workharbor.network": "whr-net-w", "workharbor.private-volume": "whr-exec-w", "workharbor.private-memory-mb": "2", "workharbor.private-identity": "fixture",
				}}, "State": map[string]bool{"Running": true}, "HostConfig": map[string]bool{"ReadonlyRootfs": true},
				"Mounts": []map[string]any{{"Type": "volume", "Name": "whr-exec-w", "Destination": "/whr-exec", "RW": true}},
			})
		case r.URL.Path == "/v1.45/volumes/whr-exec-w":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Name": "whr-exec-w", "Driver": "local", "Options": map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=2097152,uid=0,gid=0,mode=0711,nosuid,nodev,noexec"},
				"Labels": map[string]string{"workharbor.owner": Owner, "workharbor.role": "private-exec-volume", "workharbor.private-association": "whr-net-w", "workharbor.private-memory-mb": "2", "workharbor.private-identity": "fixture"},
			})
		case strings.HasSuffix(r.URL.Path, "/archive"):
			tr := tar.NewReader(r.Body)
			header, err := tr.Next()
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			if header.Name == "env" {
				data, err := io.ReadAll(tr)
				if err != nil {
					t.Error(err)
				}
				receivedPath.Store(string(data))
			}
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				Cmd, Env   []string
				User       string
				Privileged bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			if len(body.Cmd) < 2 || len(body.Env) != 0 || body.Privileged {
				t.Error("invalid supervisor exec")
				w.WriteHeader(500)
				return
			}
			kind := body.Cmd[1]
			if kind == "reserve-env" || kind == "release-env" {
				if body.User != "0" {
					t.Error("metadata not root")
				}
			} else if body.User != "" {
				t.Error("guest command changed user")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": kind})
		case strings.HasPrefix(r.URL.Path, "/v1.45/exec/"):
			if strings.HasSuffix(r.URL.Path, "/json") {
				_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if strings.Contains(r.URL.Path, "/run/") {
				data := []byte("service-output\n")
				var header [8]byte
				header[0] = 1
				binary.BigEndian.PutUint32(header[4:], uint32(len(data))) //nolint:gosec // tiny fixed fixture
				_, _ = buf.Write(header[:])
				_, _ = buf.Write(data)
			}
			_ = buf.Flush()
		default:
			t.Errorf("unexpected supervisor request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}
}
