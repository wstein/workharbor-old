package doctor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RuntimeManual explains runtime discovery, configuration and support limits.
const RuntimeManual = "docs/manual/container-runtime"

// contextMetadata captures context output without logging raw stdout or stderr.
// Generic Runner.Output is unsuitable because endpoint metadata can contain secrets.
type contextMetadata interface {
	DockerContextEndpoint(context.Context, string, string) ([]byte, error)
}

var contextName = regexp.MustCompile(`^[a-zA-Z0-9_.][a-zA-Z0-9_.-]{0,255}$`)

func validContextName(name string) bool {
	return name == "" || contextName.MatchString(name)
}

// runtimeCheck only reads metadata and asks an existing local socket for its
// ping and API version. It never starts a daemon or changes configuration.
func (d Deps) runtimeCheck(ctx context.Context) (Status, string) {
	missing := func(source string) (Status, string) {
		return NotVerified, source + "; setup installs no runtime; see " + RuntimeManual
	}
	getenv := d.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	endpoint, source := "", ""
	if m, err := readConfigMap(d.ConfigPath); err == nil {
		if r, ok := m["runtime"].(map[string]any); ok {
			endpoint, _ = r["endpoint"].(string)
			if endpoint != "" {
				source = "configured runtime.endpoint"
			}
		}
	}
	if endpoint == "" {
		endpoint = getenv("DOCKER_HOST")
		if endpoint != "" {
			source = "DOCKER_HOST"
		}
	}
	// An explicit endpoint is authoritative: never silently fall back to another
	// daemon if the selected one is unavailable or unsafe.
	if endpoint == "" && d.Runner != nil {
		name := getenv("DOCKER_CONTEXT")
		if !validContextName(name) {
			return missing("Docker context name is invalid")
		}
		if capture, ok := d.Runner.(contextMetadata); ok {
			if bin, err := d.lookPath("docker"); err == nil {
				probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				out, err := capture.DockerContextEndpoint(probeCtx, bin, name)
				cancel()
				if err == nil && len(out) <= 4096 && json.Unmarshal(out, &endpoint) == nil && endpoint != "" {
					source = "Docker context"
				}
			}
		}
	}
	if endpoint == "" && getenv("DOCKER_CONTEXT") != "" {
		return missing("selected Docker context did not provide a local endpoint")
	}
	candidates := d.RuntimeSockets
	if endpoint != "" {
		candidates = []string{endpoint}
	}
	if candidates == nil && d.Runner != nil {
		candidates = []string{"unix://" + filepath.Join(d.Home, ".docker/run/docker.sock"), "unix://" + filepath.Join(d.Home, ".orbstack/run/docker.sock"), "unix:///var/run/docker.sock"}
	}
	for _, candidate := range candidates {
		if socketAnswers(ctx, candidate) {
			return OK, candidate + ": ping and API >= 1.45 verified; environment provisioning remains blocked by unsupported disk quotas; configuration unchanged; see " + RuntimeManual
		}
	}
	if source != "" {
		return missing(source + " did not answer as a local Docker-compatible socket")
	}
	return missing("no existing Docker-compatible socket answered")
}

func socketAnswers(ctx context.Context, endpoint string) bool {
	if len(endpoint) > 4096 || strings.IndexFunc(endpoint, func(r rune) bool { return r < 32 || r >= 127 }) >= 0 {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	get := func(route string) ([]byte, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine"+route, nil)
		if err != nil {
			return nil, false
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, false
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return nil, false
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, 4097))
		return data, err == nil && len(data) <= 4096
	}
	ping, ok := get("/_ping")
	if !ok || strings.TrimSpace(string(ping)) != "OK" {
		return false
	}
	data, ok := get("/version")
	if !ok {
		return false
	}
	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	if json.Unmarshal(data, &version) != nil {
		return false
	}
	major, minor, ok := strings.Cut(version.APIVersion, ".")
	a, e1 := strconv.Atoi(major)
	b, e2 := strconv.Atoi(minor)
	return ok && e1 == nil && e2 == nil && (a > 1 || a == 1 && b >= 45)
}
