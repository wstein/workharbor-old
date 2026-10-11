// Package apple is the runtime adapter for Apple's `container` CLI (design
// §5.1). It does not assume Docker semantics: every flag below was checked
// against `container` 1.5.0 (spike #2 and issue #26), and what the runtime does
// not do is reported, not pretended. A volume is exclusive while writable, an
// internal network is one per environment, and the egress proxy is a sidecar on
// the default and the internal network.
package apple

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/runtime"
)

// Labels the adapter puts on everything it creates, next to the owner label.
const (
	roleLabel = "workharbor.role"
	envLabel  = "workharbor.env"
	netLabel  = "workharbor.network"
	// egressLabel is on the sidecar: the hosts it allows, sorted and comma-separated.
	egressLabel = "workharbor.egress"

	roleEnv     = "environment"
	roleSidecar = "sidecar"
	roleNetwork = "network"
	roleVolume  = "volume"

	// proxyPort is where the sidecar's proxy listens.
	proxyPort = "3128"
	// idleCommand keeps an environment's container alive until it is exec'd into.
	idleSeconds = "2147483647"
	// guestProxy is where the proxy binary is mounted in the sidecar.
	guestProxy = "/whr-proxy"
)

// Adapter manages environments through the container CLI for one owner.
type Adapter struct {
	owner string
	run   cli
	// shim is the path of whr-shim in the guest (the tool store is mounted
	// there). With it, Exec runs commands under the launcher and a cancel signals
	// the process group in the guest; without it a cancel only ends the client,
	// which leaves the guest process running (spike #2).
	shim string
	// tempLane and tempPurpose, when set, mark everything the adapter creates as
	// temporary (AGENTS.md, Temporary containers): the labels make `make
	// temp-clean` find it and the names start with whtmp-. Live tests set them;
	// the supervisor never does.
	tempLane, tempPurpose string
}

// Option configures New.
type Option func(*Adapter)

// WithShim sets the guest path of whr-shim, such as /tools/whr-shim.
func WithShim(guestPath string) Option { return func(a *Adapter) { a.shim = guestPath } }

// WithTemp marks the resources the adapter creates as temporary, for the lane
// and purpose (a live test's name), and names environments whtmp-<id>.
func WithTemp(lane, purpose string) Option {
	return func(a *Adapter) { a.tempLane, a.tempPurpose = lane, purpose }
}

// New returns an adapter acting for owner. It finds the container CLI.
func New(owner string, opts ...Option) (*Adapter, error) {
	bin, err := exec.LookPath("container")
	if err != nil {
		return nil, fmt.Errorf("apple: %w", err)
	}
	a := &Adapter{owner: owner, run: execCLI(bin)}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Name implements runtime.Adapter.
func (a *Adapter) Name() string { return "apple-container" }

// Capabilities implements runtime.Adapter.
func (a *Adapter) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Isolation:         runtime.GuestKernel, // a lightweight VM per container
		Arch:              "arm64",
		PersistentStorage: []string{"volumes", "bind mounts"}, // the root filesystem does not survive a delete
		NetworkIsolation:  true,                               // --internal networks
		InteractiveExec:   true,                               // container exec -t -i; unverified on a host
	}
}

// ---- the JSON the CLI prints ----

type listing struct {
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
		Image  struct {
			Reference  string `json:"reference"`
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"image"`
		Mounts []mountInfo `json:"mounts"`
	} `json:"configuration"`
	Status struct {
		State    string `json:"state"`
		Networks []struct {
			Network string `json:"network"`
			IPv4    string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

// mountInfo is a mount as `container list` prints it. A volume's source is the
// path of its image file; its name is in the type.
type mountInfo struct {
	Destination string                     `json:"destination"`
	Source      string                     `json:"source"`
	Options     []string                   `json:"options"`
	Type        map[string]json.RawMessage `json:"type"`
}

// volume returns the volume's name, or "" for a mount that is not a volume.
func (m mountInfo) volume() string {
	raw, ok := m.Type["volume"]
	if !ok {
		return ""
	}
	var v struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.Name
}

func (a *Adapter) containers(ctx context.Context) ([]listing, error) {
	out, _, err := a.run(ctx, nil, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var l []listing
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("apple: cannot read the container list: %w", err)
	}
	return l, nil
}

type named struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Config struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
	Labels map[string]string `json:"labels"`
}

func (n named) name() string {
	switch {
	case n.Name != "":
		return n.Name
	case n.Config.Name != "":
		return n.Config.Name
	}
	return n.ID
}

func (n named) labels() map[string]string {
	if len(n.Config.Labels) > 0 {
		return n.Config.Labels
	}
	return n.Labels
}

func (a *Adapter) listNamed(ctx context.Context, kind string) ([]named, error) {
	out, _, err := a.run(ctx, nil, kind, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var l []named
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("apple: cannot read the %s list: %w", kind, err)
	}
	return l, nil
}

// find returns the container with an exact name. ErrNotFound if there is none,
// ErrNotOwned if another owner has it (design §5.1).
func (a *Adapter) find(ctx context.Context, id string) (listing, error) {
	all, err := a.containers(ctx)
	if err != nil {
		return listing{}, err
	}
	for _, c := range all {
		if c.Configuration.ID == id {
			if c.Configuration.Labels[runtime.OwnerLabel] != a.owner || c.Configuration.Labels[roleLabel] != roleEnv {
				return listing{}, runtime.ErrNotOwned
			}
			return c, nil
		}
	}
	return listing{}, runtime.ErrNotFound
}

func (a *Adapter) info(c listing) runtime.Info {
	info := runtime.Info{
		ID: c.Configuration.ID, Owner: c.Configuration.Labels[runtime.OwnerLabel], Labels: map[string]string{},
		Image: c.Configuration.Image.Reference, ImageDigest: c.Configuration.Image.Descriptor.Digest, State: domain.EnvStopped,
	}
	for k, v := range c.Configuration.Labels {
		info.Labels[k] = v
	}
	for _, m := range c.Configuration.Mounts {
		mount := runtime.Mount{Source: m.Source, Target: m.Destination, ReadOnly: isReadOnly(m.Options)}
		if name := m.volume(); name != "" {
			mount.Kind, mount.Source = runtime.MountVolume, name
		} else if _, ok := m.Type["tmpfs"]; ok {
			continue // tmpfs mounts are not part of the prepared mounts
		} else {
			mount.Kind = runtime.MountBind
		}
		info.Mounts = append(info.Mounts, mount)
	}
	if c.Status.State == "running" {
		info.State = domain.EnvRunning
		for _, n := range c.Status.Networks {
			info.Addr = strings.Split(n.IPv4, "/")[0] // read now, never stored: it changes on every start
			break
		}
	}
	return info
}

// ---- Provision ----

func (a *Adapter) randomID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	prefix := "whr-"
	if a.tempLane != "" {
		prefix = "whtmp-"
	}
	return prefix + hex.EncodeToString(b), nil
}

// labelArgs returns the --label flags. The adapter's own labels are set last,
// so an extra label can never replace the owner, the role or the environment.
func (a *Adapter) labelArgs(role, env string, extra map[string]string) []string {
	labels := map[string]string{}
	for k, v := range extra {
		labels[k] = v
	}
	labels[runtime.OwnerLabel], labels[roleLabel] = a.owner, role
	if a.tempLane != "" {
		labels["workharbor.temp"], labels["workharbor.lane"], labels["workharbor.purpose"] = "true", a.tempLane, a.tempPurpose
	}
	if env != "" {
		labels[envLabel] = env
	}
	var args []string
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}

// Provision implements runtime.Adapter: it creates the internal network, the
// volumes the spec names, the environment's container and, when the spec has
// one, the egress sidecar. If any step fails what was created is removed.
func (a *Adapter) Provision(ctx context.Context, prep runtime.PreparedSpec) (string, error) {
	if !prep.Prepared() {
		return "", runtime.ErrNotPrepared
	}
	if _, has := prep.PrivateExecVolume(); has {
		return "", fmt.Errorf("%w: bounded private execution storage", runtime.ErrUnsupported)
	}
	spec := prep.Spec()
	if spec.Owner != a.owner {
		return "", &runtime.SpecError{Problems: []string{fmt.Sprintf("owner %q is not this adapter's owner %q", spec.Owner, a.owner)}}
	}
	if err := a.requireLocalBuilt(ctx, spec.Image); err != nil {
		return "", err
	}
	if spec.Egress != nil { // the sidecar's image too: in serve it is a built image
		if err := a.requireLocalBuilt(ctx, spec.Egress.Image); err != nil {
			return "", err
		}
	}
	nets, err := a.listNamed(ctx, "network")
	if err != nil {
		return "", err
	}
	for _, n := range nets {
		if n.name() == spec.Network.Name {
			return "", &runtime.SpecError{Problems: []string{fmt.Sprintf("network %q is already in use: a network is never shared between environments", spec.Network.Name)}}
		}
	}
	id, err := a.randomID()
	if err != nil {
		return "", err
	}

	var undo []func()
	fail := func(err error) (string, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return "", err
	}
	bg := context.WithoutCancel(ctx) // cleanup runs even if the caller gave up

	netArgs := append([]string{"network", "create", "--internal"}, a.labelArgs(roleNetwork, id, nil)...)
	if _, _, err := a.run(ctx, nil, append(netArgs, spec.Network.Name)...); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { _, _, _ = a.run(bg, nil, "network", "delete", spec.Network.Name) })

	vols, err := a.listNamed(ctx, "volume")
	if err != nil {
		return fail(err)
	}
	for _, m := range spec.Mounts {
		if m.Kind != runtime.MountVolume {
			continue
		}
		exists := false
		for _, v := range vols {
			if v.name() == m.Source {
				exists = true
				if v.labels()[runtime.OwnerLabel] != a.owner {
					return fail(runtime.ErrNotOwned)
				}
			}
		}
		if exists {
			continue
		}
		volArgs := append([]string{"volume", "create", "-s", strconv.Itoa(spec.DiskMB) + "M"}, a.labelArgs(roleVolume, "", nil)...)
		if _, _, err := a.run(ctx, nil, append(volArgs, m.Source)...); err != nil {
			return fail(err)
		}
		// A volume outlives the environment (design §4.4): it is not undone here
		// unless this call created it, which is the case in this branch.
		name := m.Source
		undo = append(undo, func() { _, _, _ = a.run(bg, nil, "volume", "delete", name) })
		// A new volume is an empty ext4 filesystem owned by root. The agent runs
		// as an unprivileged user, so its home would be unwritable (found by the
		// serve integration run): hand the volume to that user once, at creation.
		if err := a.ownVolume(ctx, id, spec, name); err != nil {
			return fail(err)
		}
	}

	create := a.createArgs(id, spec)
	if _, _, err := a.run(ctx, nil, create...); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { _, _, _ = a.run(bg, nil, "delete", id) })

	if spec.Egress != nil {
		if _, _, err := a.run(ctx, nil, a.sidecarArgs(id, spec)...); err != nil {
			return fail(err)
		}
	}
	return id, nil
}

// requireLocalBuilt makes an image named under runtime.BuiltImageHost local
// only (design §7, rule 4a). `container create` has no option to never pull
// (container 1.5.0: only `build` has --pull), so on a local miss it asks the
// registry, and for a bare name that is docker.io: the adapter asks the local
// store first and fails with a clear error instead of letting the CLI go to the
// network. Before anything is created, so a miss leaves nothing behind. It is
// asked of the environment's image and of the egress sidecar's (the environment's
// own image in serve, the console's for the console): both are built images.
// Only an image outside that host (an operator's configured one) keeps the CLI's
// behaviour.
func (a *Adapter) requireLocalBuilt(ctx context.Context, image string) error {
	if !runtime.IsBuiltImage(image) {
		return nil
	}
	ok, err := a.HasImage(ctx, image)
	if err != nil {
		return err
	}
	if !ok {
		return &runtime.SpecError{Problems: []string{fmt.Sprintf("the image %s is built by whr and is not here: build it (it is never pulled)", image)}}
	}
	return nil
}

// createArgs builds `container create` for the environment: hardened, on its
// internal network only, with exactly the prepared mounts. It never passes
// --ssh (it forwards the host's ssh-agent) and never --rm.
func (a *Adapter) createArgs(id string, spec runtime.Spec) []string {
	args := []string{
		"create", "--name", id, "--init", "--read-only", "--cap-drop", "ALL", "--user", spec.User,
		"--cpus", strconv.Itoa(spec.CPUs), "--memory", strconv.Itoa(spec.MemoryMB) + "M", "--network", spec.Network.Name,
	}
	extra := map[string]string{}
	for k, v := range spec.Labels {
		if !strings.HasPrefix(strings.ToLower(k), runtime.ReservedLabelPrefix) { // Validate refuses them too
			extra[k] = v
		}
	}
	extra[netLabel] = spec.Network.Name
	args = append(args, a.labelArgs(roleEnv, id, extra)...)
	for _, t := range spec.Tmpfs {
		args = append(args, "--tmpfs", t)
	}
	for _, m := range spec.Mounts {
		args = append(args, mountArgs(m)...)
	}
	// The variables come from the repository and are not secrets; Validate
	// refused every name the supervisor sets. Sorted, so the command is stable.
	names := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	return append(args, spec.Image, "sleep", idleSeconds)
}

// mountArgs returns the flag and value for one mount. It uses -v, whose value
// is not split at commas, so a path with a comma cannot add an option.
func mountArgs(m runtime.Mount) []string {
	v := m.Source + ":" + m.Target
	if m.ReadOnly {
		v += ":ro"
	}
	return []string{"-v", v}
}

func (a *Adapter) sidecarArgs(id string, spec runtime.Spec) []string {
	e := spec.Egress
	args := []string{
		"create", "--name", sidecarName(id), "--init", "--read-only", "--cap-drop", "ALL", "--user", "1000:1000",
		"--cpus", sidecarCPUs, "--memory", sidecarMemory,
		"--network", "default", "--network", spec.Network.Name,
		"-v", e.Proxy + ":" + guestProxy + ":ro",
	}
	allow := append([]string(nil), e.Allow...)
	sort.Strings(allow)
	args = append(args, a.labelArgs(roleSidecar, id, map[string]string{egressLabel: strings.Join(allow, ",")})...)
	args = append(args, e.Image, guestProxy, "-listen", "0.0.0.0:"+proxyPort, "-allow", strings.Join(e.Allow, ","))
	if len(e.DenyPrefixes) > 0 {
		args = append(args, "-deny-prefixes", strings.Join(e.DenyPrefixes, ","))
	}
	return args
}

func sidecarName(id string) string { return id + "-proxy" }

// The sidecar's limits: the proxy needs little, and an agent that floods it
// must not take the host's memory or every core. Container 1.5.0 refuses less
// than 200 MiB; 256M with one CPU boots (checked by hand).
const (
	sidecarCPUs   = "1"
	sidecarMemory = "256M"
)

// ---- lifecycle ----

// Start implements runtime.Adapter. The sidecar starts first, since the agent's
// only way out is through it. A volume another running environment holds
// read-write is refused (ErrVolumeBusy) before anything starts.
func (a *Adapter) Start(ctx context.Context, id string) error {
	c, err := a.find(ctx, id)
	if err != nil {
		return err
	}
	if c.Status.State == "running" {
		return nil
	}
	if err := a.checkVolumes(ctx, c); err != nil {
		return err
	}
	if sc, err := a.sidecarOf(ctx, id); err != nil {
		return err
	} else if sc != "" {
		if _, _, err := a.run(ctx, nil, "start", sc); err != nil {
			return err
		}
	}
	if _, _, err := a.run(ctx, nil, "start", id); err != nil {
		if strings.Contains(stderrOf(err), "attachment") { // "The storage device attachment is invalid" (spike #2)
			return runtime.ErrVolumeBusy
		}
		return err
	}
	return nil
}

// checkVolumes refuses to start an environment whose volume another running
// environment holds read-write.
func (a *Adapter) checkVolumes(ctx context.Context, c listing) error {
	all, err := a.containers(ctx)
	if err != nil {
		return err
	}
	for _, m := range c.Configuration.Mounts {
		if m.volume() == "" || isReadOnly(m.Options) {
			continue
		}
		for _, o := range all {
			if o.Configuration.ID == c.Configuration.ID || o.Status.State != "running" {
				continue
			}
			for _, om := range o.Configuration.Mounts {
				if om.volume() == m.volume() && !isReadOnly(om.Options) {
					return runtime.ErrVolumeBusy
				}
			}
		}
	}
	return nil
}

func isReadOnly(opts []string) bool {
	for _, o := range opts {
		if o == "ro" || o == "readonly" {
			return true
		}
	}
	return false
}

// sidecarListing returns the environment's sidecar as `container list` shows it.
func (a *Adapter) sidecarListing(ctx context.Context, id string) (listing, bool) {
	all, err := a.containers(ctx)
	if err != nil {
		return listing{}, false
	}
	for _, o := range all {
		l := o.Configuration.Labels
		if l[roleLabel] == roleSidecar && l[envLabel] == id && l[runtime.OwnerLabel] == a.owner {
			return o, true
		}
	}
	return listing{}, false
}

// sidecarOf returns the environment's sidecar container name, or "".
func (a *Adapter) sidecarOf(ctx context.Context, id string) (string, error) {
	all, err := a.containers(ctx)
	if err != nil {
		return "", err
	}
	for _, o := range all {
		l := o.Configuration.Labels
		if l[roleLabel] == roleSidecar && l[envLabel] == id && l[runtime.OwnerLabel] == a.owner {
			return o.Configuration.ID, nil
		}
	}
	return "", nil
}

// Stop implements runtime.Adapter.
func (a *Adapter) Stop(ctx context.Context, id string) error {
	c, err := a.find(ctx, id)
	if err != nil {
		return err
	}
	if c.Status.State == "running" {
		if _, _, err := a.run(ctx, nil, "stop", id); err != nil {
			return err
		}
	}
	if sc, err := a.sidecarOf(ctx, id); err == nil && sc != "" {
		if _, _, err := a.run(ctx, nil, "stop", sc); err != nil && !strings.Contains(stderrOf(err), "not running") {
			return err
		}
	}
	return nil
}

// Delete implements runtime.Adapter: the container, its sidecar and its
// network, each by exact name. Its volumes stay (design §4.4).
func (a *Adapter) Delete(ctx context.Context, id string) error {
	c, err := a.find(ctx, id)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil // a retry of a delete that already happened
	}
	if err != nil {
		return err
	}
	if c.Status.State == "running" {
		return runtime.ErrRunning
	}
	if sc, err := a.sidecarOf(ctx, id); err != nil {
		return err
	} else if sc != "" {
		_, _, _ = a.run(ctx, nil, "stop", sc)
		if _, _, err := a.run(ctx, nil, "delete", sc); err != nil {
			return err
		}
	}
	if _, _, err := a.run(ctx, nil, "delete", id); err != nil {
		return err
	}
	// A helper of ownVolume that was left behind refers to the network: remove it.
	if all, err := a.containers(ctx); err == nil {
		for _, h := range all {
			l := h.Configuration.Labels
			if l[roleLabel] == roleVolume && l[envLabel] == id && l[runtime.OwnerLabel] == a.owner {
				a.removeHelper(ctx, h.Configuration.ID)
			}
		}
	}
	if net := c.Configuration.Labels[netLabel]; net != "" {
		if _, _, err := a.run(ctx, nil, "network", "delete", net); err != nil {
			return err
		}
	}
	return nil
}

// RemoveVolume implements runtime.Adapter.
func (a *Adapter) RemoveVolume(ctx context.Context, name string) error {
	vols, err := a.listNamed(ctx, "volume")
	if err != nil {
		return err
	}
	found := false
	for _, v := range vols {
		if v.name() == name {
			if v.labels()[runtime.OwnerLabel] != a.owner {
				return runtime.ErrNotOwned
			}
			found = true
		}
	}
	if !found {
		return nil
	}
	all, err := a.containers(ctx)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.Status.State != "running" {
			continue
		}
		for _, m := range c.Configuration.Mounts {
			if m.volume() == name {
				return runtime.ErrVolumeBusy
			}
		}
	}
	_, _, err = a.run(ctx, nil, "volume", "delete", name)
	return err
}

// ---- reading ----

// Inspect implements runtime.Adapter.
func (a *Adapter) Inspect(ctx context.Context, id string) (runtime.Info, error) {
	c, err := a.find(ctx, id)
	if err != nil {
		return runtime.Info{}, err
	}
	info := a.info(c)
	if sc, ok := a.sidecarListing(ctx, id); ok {
		info.EgressAllow = []string{}
		if v := sc.Configuration.Labels[egressLabel]; v != "" {
			info.EgressAllow = strings.Split(v, ",")
		}
		if info.State == domain.EnvRunning {
			info.Proxy = a.proxyURL(ctx, sc.Configuration.ID, c.Configuration.Labels[netLabel])
		}
	}
	return info, nil
}

// proxyURL reads the sidecar's address on the environment's internal network.
func (a *Adapter) proxyURL(ctx context.Context, sidecar, network string) string {
	all, err := a.containers(ctx)
	if err != nil {
		return ""
	}
	for _, o := range all {
		if o.Configuration.ID != sidecar {
			continue
		}
		for _, n := range o.Status.Networks {
			if n.Network == network {
				return "http://" + strings.Split(n.IPv4, "/")[0] + ":" + proxyPort
			}
		}
	}
	return ""
}

// List implements runtime.Adapter: only environments with this owner label.
func (a *Adapter) List(ctx context.Context, owner string) ([]runtime.Info, error) {
	all, err := a.containers(ctx)
	if err != nil {
		return nil, err
	}
	var out []runtime.Info
	for _, c := range all {
		l := c.Configuration.Labels
		if l[runtime.OwnerLabel] == owner && l[roleLabel] == roleEnv {
			out = append(out, a.info(c))
		}
	}
	return out, nil
}

// Resources implements runtime.Adapter.
func (a *Adapter) Resources(ctx context.Context, id string) (runtime.Resources, error) {
	c, err := a.find(ctx, id)
	if err != nil {
		return runtime.Resources{}, err
	}
	res := runtime.Resources{Network: c.Configuration.Labels[netLabel]}
	for _, m := range c.Configuration.Mounts {
		if name := m.volume(); name != "" {
			res.Volumes = append(res.Volumes, name)
		}
	}
	if sc, err := a.sidecarOf(ctx, id); err == nil {
		res.Sidecar = sc
	}
	return res, nil
}

// Inventory implements runtime.Adapter: everything this owner has.
func (a *Adapter) Inventory(ctx context.Context) (runtime.Inventory, error) {
	var inv runtime.Inventory
	all, err := a.containers(ctx)
	if err != nil {
		return inv, err
	}
	for _, c := range all {
		if c.Configuration.Labels[runtime.OwnerLabel] == a.owner && c.Configuration.Labels[roleLabel] == roleSidecar {
			inv.Sidecars = append(inv.Sidecars, c.Configuration.ID)
		}
	}
	for kind, dst := range map[string]*[]string{"network": &inv.Networks, "volume": &inv.Volumes} {
		items, err := a.listNamed(ctx, kind)
		if err != nil {
			return inv, err
		}
		for _, n := range items {
			if n.labels()[runtime.OwnerLabel] == a.owner {
				*dst = append(*dst, n.name())
			}
		}
	}
	sortStrings(inv.Networks)
	sortStrings(inv.Volumes)
	sortStrings(inv.Sidecars)
	return inv, nil
}

// Logs implements runtime.Adapter.
func (a *Adapter) Logs(ctx context.Context, id string) ([]byte, error) {
	if _, err := a.find(ctx, id); err != nil {
		return nil, err
	}
	out, _, err := a.run(ctx, nil, "logs", id)
	return out, err
}

// Endpoints implements runtime.Adapter: no ssh or browser endpoints are offered.
func (a *Adapter) Endpoints(ctx context.Context, id string) ([]runtime.Endpoint, error) {
	if _, err := a.find(ctx, id); err != nil {
		return nil, err
	}
	return nil, nil
}

// buildArgs is `container build` for a checked BuildSpec. The flags are those
// of container 1.5.0 (`container build --help`). It never passes --ssh (it
// forwards the host's ssh-agent), --secret or --output, and sorts the build
// arguments so the command is stable.
func buildArgs(b runtime.BuildSpec) []string {
	args := []string{"build", "--progress", "plain", "--tag", b.Tag, "--file", b.Dockerfile}
	names := make([]string, 0, len(b.Args))
	for k := range b.Args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		args = append(args, "--build-arg", k+"="+b.Args[k])
	}
	return append(args, "--", b.ContextDir)
}

// Build builds an image from a context directory the supervisor wrote. It
// returns the builder's output, and on failure the error with the last of it.
// The build's own network access is the builder VM's, not an environment's:
// the egress allowlist does not apply to it (design §7.2).
func (a *Adapter) Build(ctx context.Context, b runtime.BuildSpec) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out, errOut, err := a.run(ctx, nil, buildArgs(b)...)
	return append(out, errOut...), err
}

var _ runtime.Builder = (*Adapter)(nil)

// ownVolume gives a new volume to the environment's user. A short container as
// root runs `whr-shim chown` from the read-only tool store as its entrypoint,
// so no program of the environment's image runs as root (since D38 the image
// may be built from the repository), on the environment's internal network, so
// it has no way out, and is deleted. It is the only place the adapter runs
// anything as root.
func (a *Adapter) ownVolume(ctx context.Context, env string, spec runtime.Spec, volume string) error {
	args, err := a.ownVolumeArgs(env, spec, volume)
	if err != nil {
		return err
	}
	_, _, err = a.run(ctx, nil, args...)
	a.removeHelper(context.WithoutCancel(ctx), ownHelperName(env, volume))
	if err != nil {
		return fmt.Errorf("give volume %s to user %s: %w", volume, spec.User, err)
	}
	return nil
}

// removeHelper deletes the short container ownVolume ran, and tries again when
// the runtime says it is not ready: a helper left behind keeps the environment's
// network from being deleted later (found when a rebuild removed an environment
// whose network a helper still referred to). A container that is already gone is
// done.
func (a *Adapter) removeHelper(ctx context.Context, name string) {
	for range 10 {
		if _, errOut, err := a.run(ctx, nil, "delete", "--force", name); err == nil || strings.Contains(strings.ToLower(string(errOut)), "not found") {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func ownHelperName(env, volume string) string {
	helper := env + "-own-" + volume
	if len(helper) > 100 {
		helper = helper[:100]
	}
	return helper
}

// ErrNoLauncher means the adapter has no whr-shim in the tool store to give a
// volume to the environment's user without running the image as root.
var ErrNoLauncher = errors.New("apple: a volume needs whr-shim from the tool store (WithShim) to be given to the environment's user")

func (a *Adapter) ownVolumeArgs(env string, spec runtime.Spec, volume string) ([]string, error) {
	if a.shim == "" {
		return nil, ErrNoLauncher
	}
	var tools *runtime.Mount
	for i, m := range spec.Mounts {
		if m.Kind != runtime.MountVolume && m.ReadOnly && strings.HasPrefix(a.shim, strings.TrimSuffix(m.Target, "/")+"/") {
			tools = &spec.Mounts[i]
			break
		}
	}
	if tools == nil {
		return nil, fmt.Errorf("%w: no read-only mount of the spec holds %s", ErrNoLauncher, a.shim)
	}
	args := []string{
		"run", "--name", ownHelperName(env, volume), "--user", "0:0", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--read-only",
		"--network", spec.Network.Name, "--entrypoint", a.shim,
		"-v", volume + ":/v",
	}
	args = append(args, mountArgs(*tools)...)
	args = append(args, a.labelArgs(roleVolume, env, nil)...)
	return append(args, spec.Image, "chown", "-owner", spec.User, "/v"), nil
}

// HasImage reports whether the runtime has an image with this tag, with
// `container image inspect`, which exits 1 for an image that is not there
// ("image not found", container 1.5.0). Any other failure is an error: a runtime
// that cannot be asked must not read as "no image" and trigger a build.
func (a *Adapter) HasImage(ctx context.Context, tag string) (bool, error) {
	if !runtime.ValidImage(tag) {
		return false, &runtime.BuildError{Problems: []string{"tag " + `"` + tag + `"` + " is not an image reference"}}
	}
	_, errOut, err := a.run(ctx, nil, "image", "inspect", "--", tag)
	if err == nil {
		return true, nil
	}
	if strings.Contains(string(errOut)+stderrOf(err), "image not found") {
		return false, nil
	}
	return false, err
}

// UpdateEgress implements runtime.EgressUpdater: it replaces the environment's
// egress sidecar with one that has the allowlist of the prepared spec. The old
// sidecar is stopped and deleted first, the new one created on the same networks
// and, if the environment runs, started. The sidecar's address changes, so the
// caller does it only before an agent process starts (design §4.2).
func (a *Adapter) UpdateEgress(ctx context.Context, id string, prep runtime.PreparedSpec) error {
	if !prep.Prepared() {
		return runtime.ErrNotPrepared
	}
	spec := prep.Spec()
	c, err := a.find(ctx, id)
	if err != nil {
		return err
	}
	if spec.Egress == nil || spec.Network.Name == "" || spec.Network.Name != c.Configuration.Labels[netLabel] {
		return &runtime.SpecError{Problems: []string{"the spec must carry an Egress and the environment's own network"}}
	}
	if err := a.requireLocalBuilt(ctx, spec.Egress.Image); err != nil { // before the old sidecar is touched
		return err
	}
	if old, err := a.sidecarOf(ctx, id); err != nil {
		return err
	} else if old != "" {
		if _, _, err := a.run(ctx, nil, "stop", old); err != nil && !strings.Contains(stderrOf(err), "not running") {
			return fmt.Errorf("stop the old sidecar: %w", err)
		}
		if _, _, err := a.run(ctx, nil, "delete", old); err != nil {
			return fmt.Errorf("delete the old sidecar: %w", err)
		}
	}
	if _, _, err := a.run(ctx, nil, a.sidecarArgs(id, spec)...); err != nil {
		return err
	}
	if c.Status.State == "running" {
		if _, _, err := a.run(ctx, nil, "start", sidecarName(id)); err != nil {
			return err
		}
	}
	return nil
}

var _ runtime.EgressUpdater = (*Adapter)(nil)
