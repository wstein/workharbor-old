// Package docker implements the runtime contract through an explicitly selected
// local Docker-compatible Engine socket. It does not install or discover a daemon.
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wstein/workharbor/internal/domain"
	rt "github.com/wstein/workharbor/internal/runtime"
)

const (
	roleLabel       = "workharbor.role"
	networkLabel    = "workharbor.network"
	environmentRole = "environment"
)

// ErrUnsupported identifies a capability the backend does not implement.
var ErrUnsupported = rt.ErrUnsupported

// Adapter manages only resources carrying its owner and role labels.
type Adapter struct {
	owner, endpoint, shim string
	client                *http.Client
	cleanupTimeout        time.Duration
	mu                    sync.Mutex
}

// New uses an existing local Unix socket and a guest path to the trusted shim
// supplied by the separate agent CLI tool store. No connection is made here.
func New(owner, endpoint, shim string) (*Adapter, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !path.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("docker: an explicit unix:///absolute/socket endpoint is required")
	}
	if owner == "" || !path.IsAbs(shim) {
		return nil, errors.New("docker: owner and absolute guest shim path are required")
	}
	socket := u.Path
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Adapter{owner: owner, endpoint: endpoint, shim: shim, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (*Adapter) Name() string { return "docker" }

func (*Adapter) Capabilities() rt.Capabilities {
	return rt.Capabilities{Isolation: rt.SharedKernel, Arch: "unknown", PersistentStorage: []string{"named volumes survive stop, delete and rebuild"}}
}

type apiError struct{ status int }

func (e *apiError) Error() string {
	return fmt.Sprintf("docker Engine request failed (HTTP %d)", e.status)
}

func missing(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.status == http.StatusNotFound
}

// request deliberately excludes daemon error bodies: a daemon may echo request
// content, including environment values. Response bodies are always bounded.
func (a *Adapter) request(ctx context.Context, method, route string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://engine/v1.45"+route, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker Engine transport: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotModified && method == http.MethodPost {
		return nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &apiError{res.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); err != nil {
			return fmt.Errorf("docker Engine response: %w", err)
		}
	}
	return nil
}

func labels(owner, role string) map[string]string {
	return map[string]string{rt.OwnerLabel: owner, roleLabel: role}
}

func owned(l map[string]string, owner, role string) bool {
	return l[rt.OwnerLabel] == owner && l[roleLabel] == role
}

func token() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type container struct {
	ID     string `json:"Id"`
	Image  string
	Config struct {
		Image  string
		User   string
		Labels map[string]string
	}
	State      struct{ Running bool }
	HostConfig struct {
		ReadonlyRootfs bool
		Tmpfs          map[string]string
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

func (a *Adapter) find(ctx context.Context, id string) (container, error) {
	var c container
	if id == "" || strings.ContainsAny(id, "/*?\\") {
		return c, rt.ErrNotFound
	}
	err := a.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &c)
	if missing(err) {
		return c, rt.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	// Engine accepts names and ID prefixes. The contract accepts exact IDs only.
	if c.ID != id {
		return c, rt.ErrNotFound
	}
	if !owned(c.Config.Labels, a.owner, environmentRole) {
		return c, rt.ErrNotOwned
	}
	return c, nil
}

func info(c container) rt.Info {
	state := domain.EnvStopped
	addr := ""
	if c.State.Running {
		state = domain.EnvRunning
		for _, n := range c.NetworkSettings.Networks {
			if n.IPAddress != "" {
				addr = n.IPAddress
				break
			}
		}
	}
	out := rt.Info{ID: c.ID, Owner: c.Config.Labels[rt.OwnerLabel], Labels: c.Config.Labels, Image: c.Config.Image, ImageDigest: c.Image, State: state, Addr: addr}
	for _, m := range c.Mounts {
		source := m.Source
		kind := rt.MountBind
		if m.Type == "volume" {
			source = m.Name
			kind = rt.MountVolume
		}
		out.Mounts = append(out.Mounts, rt.Mount{Kind: kind, Source: source, Target: m.Destination, ReadOnly: !m.RW})
	}
	return out
}

func (a *Adapter) Inspect(ctx context.Context, id string) (rt.Info, error) {
	c, err := a.find(ctx, id)
	return info(c), err
}

func (a *Adapter) containers(ctx context.Context) ([]container, error) {
	var list []struct {
		ID string `json:"Id"`
	}
	if err := a.request(ctx, http.MethodGet, "/containers/json?all=true", nil, &list); err != nil {
		return nil, err
	}
	out := make([]container, 0, len(list))
	for _, v := range list {
		var c container
		err := a.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(v.ID)+"/json", nil, &c)
		if missing(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (a *Adapter) List(ctx context.Context, owner string) ([]rt.Info, error) {
	if owner != a.owner {
		return nil, nil
	}
	all, err := a.containers(ctx)
	if err != nil {
		return nil, err
	}
	var out []rt.Info
	for _, c := range all {
		if owned(c.Config.Labels, a.owner, environmentRole) {
			out = append(out, info(c))
		}
	}
	return out, nil
}

func (a *Adapter) Provision(ctx context.Context, p rt.PreparedSpec) (string, error) {
	if !p.Prepared() {
		return "", rt.ErrNotPrepared
	}
	s := p.Spec()
	if !s.Network.Internal {
		n, ok := p.DockerOutboundNetwork()
		if !ok || n != s.Network {
			return "", rt.ErrNotPrepared
		}
	}
	if err := a.checkSocketMounts(s.Mounts); err != nil {
		return "", err
	}
	if s.DiskMB != 0 {
		return "", fmt.Errorf("%w: disk quotas are not implemented by this adapter", ErrUnsupported)
	}
	if s.Owner != a.owner {
		return "", &rt.SpecError{Problems: []string{"owner does not match this supervisor"}}
	}
	if s.Egress != nil {
		return "", fmt.Errorf("%w: egress sidecar", ErrUnsupported)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	name, err := token()
	if err != nil {
		return "", err
	}
	name = "whr-" + name
	// No pull endpoint is invoked. The operator supplies an existing stock image.
	var image struct {
		ID string `json:"Id"`
	}
	if err := a.request(ctx, http.MethodGet, "/images/"+url.PathEscape(s.Image)+"/json", nil, &image); err != nil {
		return "", fmt.Errorf("docker: stock image must already exist locally: %w", err)
	}
	if err := a.ensureNetwork(ctx, s.Network); err != nil {
		return "", err
	}
	private, hasPrivate := p.PrivateExecVolume()
	for _, m := range s.Mounts {
		if m.Kind == rt.MountVolume {
			if hasPrivate && m.Source == private.Name {
				if err := a.ensurePrivateVolume(ctx, private); err != nil {
					return "", err
				}
				continue
			}
			if err := a.ensureVolume(ctx, m.Source); err != nil {
				return "", err
			}
		}
	}
	ls := labels(a.owner, environmentRole)
	for k, v := range s.Labels {
		ls[k] = v
	}
	ls[networkLabel] = s.Network.Name
	if hasPrivate {
		var v named
		if err := a.request(ctx, http.MethodGet, "/volumes/"+url.PathEscape(private.Name), nil, &v); err != nil {
			return "", err
		}
		if err := a.checkPrivateVolume(v, private); err != nil {
			return "", err
		}
		ls[privateVolumeLabel], ls[privateSizeLabel], ls[privateNonceLabel] = private.Name, v.Labels[privateSizeLabel], v.Labels[privateNonceLabel]
	}
	mounts := make([]map[string]any, 0, len(s.Mounts))
	for _, m := range s.Mounts {
		kind := string(m.Kind)
		if kind == "" {
			kind = "bind"
		}
		mounts = append(mounts, map[string]any{"Type": kind, "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly})
	}
	tmpfs := map[string]string{}
	for _, d := range s.Tmpfs {
		tmpfs[d] = "rw,nosuid,nodev,uid=1000,gid=1000"
		if d == "/tmp" {
			tmpfs[d] = "rw,nosuid,nodev,uid=0,gid=0,mode=1777"
		}
	}
	env := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	config := map[string]any{"Image": image.ID, "User": s.User, "Labels": ls, "Env": env, "Entrypoint": []string{"sleep"}, "Cmd": []string{"2147483647"}, "HostConfig": map[string]any{"Init": true, "ReadonlyRootfs": s.ReadOnlyRoot, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "Memory": int64(s.MemoryMB) << 20, "NanoCpus": int64(s.CPUs) * 1_000_000_000, "NetworkMode": s.Network.Name, "Mounts": mounts, "Tmpfs": tmpfs}}
	var created struct {
		ID string `json:"Id"`
	}
	if err := a.request(ctx, http.MethodPost, "/containers/create?name="+name, config, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

type named struct {
	ID         string `json:"Id"`
	Name       string
	Labels     map[string]string
	Driver     string
	Scope      string
	Internal   bool
	Options    map[string]string
	Containers map[string]json.RawMessage
}

func (a *Adapter) ensureNetwork(ctx context.Context, n rt.Network) error {
	var existing named
	err := a.request(ctx, http.MethodGet, "/networks/"+url.PathEscape(n.Name), nil, &existing)
	if err == nil {
		if !owned(existing.Labels, a.owner, "network") {
			return rt.ErrNotOwned
		}
		return &rt.SpecError{Problems: []string{"the environment network already exists"}}
	}
	if !missing(err) {
		return err
	}
	if err := a.request(ctx, http.MethodPost, "/networks/create", map[string]any{"Name": n.Name, "Driver": "bridge", "Internal": n.Internal, "CheckDuplicate": true, "Labels": labels(a.owner, "network")}, &existing); err != nil {
		return err
	}
	id := existing.ID
	if id == "" {
		return errors.New("docker: network creation has uncertain identity; operator inspection required")
	}
	if err := a.request(ctx, http.MethodGet, "/networks/"+url.PathEscape(id), nil, &existing); err != nil {
		return err
	}
	if existing.ID != id || existing.Name != n.Name || !owned(existing.Labels, a.owner, "network") || existing.Driver != "bridge" || existing.Scope != "local" || existing.Internal != n.Internal || len(existing.Containers) != 0 {
		return rt.ErrNotOwned
	}
	return nil
}

func (a *Adapter) ensureVolume(ctx context.Context, name string) error {
	var existing named
	err := a.request(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &existing)
	if err == nil {
		if !owned(existing.Labels, a.owner, "volume") {
			return rt.ErrNotOwned
		}
		return nil
	}
	if !missing(err) {
		return err
	}
	if err := a.request(ctx, http.MethodPost, "/volumes/create", map[string]any{"Name": name, "Labels": labels(a.owner, "volume")}, &existing); err != nil {
		return err
	}
	if !owned(existing.Labels, a.owner, "volume") {
		return rt.ErrNotOwned
	}
	return nil
}

func (a *Adapter) Start(ctx context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.transition(ctx, id, false, func(c container) error {
		if _, _, err := a.inspectPrivateVolume(ctx, c); err != nil {
			return err
		}
		if c.State.Running {
			return nil
		}
		all, err := a.containers(ctx)
		if err != nil {
			return err
		}
		for _, m := range c.Mounts {
			if m.Type != "volume" || !m.RW {
				continue
			}
			for _, other := range all {
				if other.ID == id || !other.State.Running {
					continue
				}
				for _, om := range other.Mounts {
					if om.Type == "volume" && om.Name == m.Name && om.RW {
						return rt.ErrVolumeBusy
					}
				}
			}
		}
		if err := a.acquireLeases(ctx, c); err != nil {
			return err
		}
		return a.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil)
	})
}

func (a *Adapter) Stop(ctx context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.transition(ctx, id, false, func(c container) error {
		if c.State.Running {
			if err := a.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t=10", nil, nil); err != nil {
				return err
			}
		}
		return a.releaseLeases(ctx, id)
	})
}

func (a *Adapter) Delete(ctx context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.transition(ctx, id, true, func(c container) error {
		if c.State.Running {
			return rt.ErrRunning
		}
		if err := a.releaseLeases(ctx, id); err != nil {
			return err
		}
		network := c.Config.Labels[networkLabel]
		if network != "" {
			var n named
			err := a.request(ctx, http.MethodGet, "/networks/"+url.PathEscape(network), nil, &n)
			if err != nil && !missing(err) {
				return err
			}
			if err == nil {
				if n.Name != network || !owned(n.Labels, a.owner, "network") {
					return rt.ErrNotOwned
				}
				for attached := range n.Containers {
					if attached != id {
						return rt.ErrNotOwned
					}
				}
				if _, connected := n.Containers[id]; connected {
					if err := a.request(ctx, http.MethodPost, "/networks/"+url.PathEscape(n.ID)+"/disconnect", map[string]any{"Container": id, "Force": true}, nil); err != nil && !missing(err) {
						return err
					}
				}
				// Keep the stopped container until its network is gone: a retry can
				// recover the exact network name even after a partial removal failure.
				if err := a.request(ctx, http.MethodDelete, "/networks/"+url.PathEscape(n.ID), nil, nil); err != nil && !missing(err) {
					return err
				}
			}
		}
		if err := a.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), nil, nil); err != nil && !missing(err) {
			return err
		}
		return a.deletePrivateVolume(ctx, c)
	})
}

func (a *Adapter) RemoveVolume(ctx context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var v named
	err := a.request(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &v)
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if owned(v.Labels, a.owner, privateVolumeRole) {
		return rt.ErrVolumeBusy
	}
	if !owned(v.Labels, a.owner, "volume") {
		return rt.ErrNotOwned
	}
	err = a.request(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil)
	var e *apiError
	if errors.As(err, &e) && e.status == http.StatusConflict {
		return rt.ErrVolumeBusy
	}
	if missing(err) {
		return nil
	}
	return err
}

func (a *Adapter) Resources(ctx context.Context, id string) (rt.Resources, error) {
	c, err := a.find(ctx, id)
	if err != nil {
		return rt.Resources{}, err
	}
	r := rt.Resources{Network: c.Config.Labels[networkLabel]}
	for _, m := range c.Mounts {
		if m.Type == "volume" {
			r.Volumes = append(r.Volumes, m.Name)
		}
	}
	return r, nil
}

func (a *Adapter) Inventory(ctx context.Context) (rt.Inventory, error) {
	var out rt.Inventory
	all, err := a.containers(ctx)
	if err != nil {
		return out, err
	}
	for _, c := range all {
		if owned(c.Config.Labels, a.owner, leaseRole) || owned(c.Config.Labels, a.owner, transitionRole) {
			out.Helpers = append(out.Helpers, c.ID)
		}
	}
	var networks []named
	if err := a.request(ctx, http.MethodGet, "/networks", nil, &networks); err != nil {
		return out, err
	}
	for _, n := range networks {
		if owned(n.Labels, a.owner, "network") {
			out.Networks = append(out.Networks, n.Name)
		}
	}
	var volumes struct{ Volumes []named }
	if err := a.request(ctx, http.MethodGet, "/volumes", nil, &volumes); err != nil {
		return out, err
	}
	for _, v := range volumes.Volumes {
		if owned(v.Labels, a.owner, "volume") || owned(v.Labels, a.owner, privateVolumeRole) {
			out.Volumes = append(out.Volumes, v.Name)
		}
	}
	sort.Strings(out.Helpers)
	sort.Strings(out.Networks)
	sort.Strings(out.Volumes)
	return out, nil
}

func (a *Adapter) Endpoints(ctx context.Context, id string) ([]rt.Endpoint, error) {
	_, err := a.find(ctx, id)
	return nil, err
}

func (a *Adapter) Logs(ctx context.Context, id string) ([]byte, error) {
	if _, err := a.find(ctx, id); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: container logs", ErrUnsupported)
}

// Close releases idle socket connections; running streams belong to their caller.
func (a *Adapter) Close() { a.client.CloseIdleConnections() }
