package doctor

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeRuntimeSocket(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "d.sock")
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("mutation %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/_ping":
			_, _ = w.Write([]byte("OK"))
		case "/version":
			_, _ = w.Write([]byte(`{"ApiVersion":"1.45"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "unix://" + socket
}

func TestRuntimeSocket(t *testing.T) {
	endpoint := fakeRuntimeSocket(t)
	t.Setenv("DOCKER_HOST", endpoint)
	d := hostDeps(scripted{})
	d.Getenv = os.Getenv
	d.ConfigPath = filepath.Join(t.TempDir(), "missing.json")
	d.LookPath = func(string) (string, error) { return "container", nil }
	got, detail := status(steps(t, d)["runtime"])
	if got != OK || !strings.Contains(detail, endpoint) || !strings.Contains(detail, "ping and API") {
		t.Fatalf("%s %q: expected a verified fake Docker-compatible socket", got, detail)
	}
}

func TestMissingRuntimeSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/workharbor-test.sock")
	d := hostDeps(scripted{})
	d.Getenv = os.Getenv
	d.ConfigPath = filepath.Join(t.TempDir(), "missing.json")
	d.LookPath = func(string) (string, error) { return "container", nil }
	got, detail := steps(t, d)["runtime"].Run(context.Background())
	if got != NotVerified || !strings.Contains(detail, "docs/manual/container-runtime") {
		t.Fatalf("%s %q: missing socket must be not_verified with manual link", got, detail)
	}
	if _, err := os.Stat(d.ConfigPath); !os.IsNotExist(err) {
		t.Fatal("discovery wrote configuration")
	}
}

func TestSocketSources(t *testing.T) {
	for _, source := range []string{"configured", "context", "known"} {
		t.Run(source, func(t *testing.T) {
			endpoint := fakeRuntimeSocket(t)
			d := hostDeps(scripted{})
			d.ConfigPath = filepath.Join(t.TempDir(), "config.json")
			switch source {
			case "configured":
				if err := os.WriteFile(d.ConfigPath, []byte(`{"runtime":{"backend":"docker","endpoint":"`+endpoint+`"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				d.Getenv = func(k string) string {
					if k == "DOCKER_HOST" {
						return "unix:///wrong.sock"
					}
					return ""
				}
			case "context":
				r := &recording{scripted: scripted{`docker context inspect --format {{json .Endpoints.docker.Host}} chosen`: `"` + endpoint + `"`}}
				d.Runner = r
				d.LookPath = func(string) (string, error) { return "docker", nil }
				d.Getenv = func(k string) string {
					if k == "DOCKER_CONTEXT" {
						return "chosen"
					}
					return ""
				}
				t.Cleanup(func() {
					if len(r.calls) != 1 {
						t.Errorf("context calls %v", r.calls)
					}
				})
			case "known":
				d.RuntimeSockets = []string{endpoint}
			}
			c := steps(t, d)["runtime"]
			if c.Fix != nil {
				t.Fatal("discovery offers mutation")
			}
			got, detail := status(c)
			if got != OK || !strings.Contains(detail, endpoint) {
				t.Fatalf("%s %q", got, detail)
			}
			if source == "configured" {
				for _, name := range []string{"brew-pin", "container-start", "container-kernel", "standard-user-check"} {
					if _, present := steps(t, d)[name]; present {
						t.Errorf("Docker setup still includes %s", name)
					}
				}
			}
		})
	}
}

func TestSocketRefusals(t *testing.T) {
	for _, endpoint := range []string{"https://user:password@example.invalid", "unix:///missing.sock", "unix://user:password@host/path", "unix:///tmp/socket\npassword", "unix:///tmp/socket?password"} {
		t.Run("unsafe or unavailable", func(t *testing.T) {
			d := hostDeps(scripted{})
			d.ConfigPath = filepath.Join(t.TempDir(), "absent.json")
			d.Getenv = func(k string) string {
				if k == "DOCKER_HOST" {
					return endpoint
				}
				return ""
			}
			d.RuntimeSockets = []string{fakeRuntimeSocket(t)}
			got, detail := status(steps(t, d)["runtime"])
			if got != NotVerified || strings.Contains(detail, "password") || strings.Contains(detail, "\n") {
				t.Fatalf("%s %q", got, detail)
			}
		})
	}
}

func TestSocketProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, ping, version string
		status              int
	}{
		{"bad ping", "not Docker", `{"ApiVersion":"1.45"}`, 200},
		{"old API", "OK", `{"ApiVersion":"1.44"}`, 200},
		{"malformed API", "OK", `{"ApiVersion":"unknown"}`, 200},
		{"oversized response", "OK", strings.Repeat("x", 4097), 200},
		{"redirect", "OK", `{"ApiVersion":"1.45"}`, 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "d.sock")
			listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.invalid")
				w.WriteHeader(tc.status)
				if r.URL.Path == "/_ping" {
					_, _ = w.Write([]byte(tc.ping))
				} else {
					_, _ = w.Write([]byte(tc.version))
				}
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			if socketAnswers(context.Background(), "unix://"+socket) {
				t.Fatal("incompatible socket verified")
			}
		})
	}
}

func TestContextSelectionIsAuthoritative(t *testing.T) {
	for _, hasCLI := range []bool{false, true} {
		d := hostDeps(scripted{})
		d.ConfigPath = filepath.Join(t.TempDir(), "absent.json")
		d.RuntimeSockets = []string{fakeRuntimeSocket(t)}
		if hasCLI {
			d.LookPath = func(string) (string, error) { return "docker", nil }
		}
		d.Getenv = func(k string) string {
			if k == "DOCKER_CONTEXT" {
				return "chosen"
			}
			return ""
		}
		got, detail := status(steps(t, d)["runtime"])
		if got != NotVerified || !strings.Contains(detail, "selected Docker context") {
			t.Fatalf("%s %q", got, detail)
		}
	}
}

func TestDefaultContext(t *testing.T) {
	endpoint := fakeRuntimeSocket(t)
	r := &recording{scripted: scripted{`docker context inspect --format {{json .Endpoints.docker.Host}}`: `"` + endpoint + `"`}}
	d := hostDeps(r)
	d.ConfigPath = filepath.Join(t.TempDir(), "absent.json")
	d.LookPath = func(string) (string, error) { return "docker", nil }
	got, detail := status(steps(t, d)["runtime"])
	if got != OK || len(r.calls) != 1 {
		t.Fatalf("%s %q calls=%v", got, detail, r.calls)
	}
}

func TestSocketCancellation(t *testing.T) {
	endpoint := fakeRuntimeSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if socketAnswers(ctx, endpoint) {
		t.Fatal("cancelled probe passed")
	}
}

type outputOnlyContext struct{ calls int }

func (r *outputOnlyContext) Output(context.Context, ...string) ([]byte, error) {
	r.calls++
	return nil, nil
}

func TestContextNeverUsesLoggedOutput(t *testing.T) {
	r := &outputOnlyContext{}
	d := hostDeps(r)
	d.ConfigPath = filepath.Join(t.TempDir(), "missing.json")
	d.LookPath = func(string) (string, error) { return "docker", nil }
	d.Getenv = func(k string) string {
		if k == "DOCKER_CONTEXT" {
			return "chosen"
		}
		return ""
	}
	d.RuntimeSockets = []string{fakeRuntimeSocket(t)}
	got, _ := status(steps(t, d)["runtime"])
	if got != NotVerified || r.calls != 0 {
		t.Fatal("context discovery used generic logged Output or ignored explicit selection")
	}
}

func TestContextRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"-option", "url/user:password", "line\npassword", "bidi\u202epassword", strings.Repeat("a", 257)} {
		r := &recording{scripted: scripted{}}
		d := hostDeps(r)
		d.ConfigPath = filepath.Join(t.TempDir(), "missing.json")
		d.LookPath = func(string) (string, error) { return "docker", nil }
		d.Getenv = func(k string) string {
			if k == "DOCKER_CONTEXT" {
				return name
			}
			return ""
		}
		got, detail := status(steps(t, d)["runtime"])
		if got != NotVerified || len(r.calls) != 0 || strings.Contains(detail, "password") {
			t.Fatal("unsafe context name reached a command or report")
		}
	}
}
