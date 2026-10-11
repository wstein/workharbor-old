package runtime

import (
	"fmt"
	"io/fs"
	"net/netip"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// PrepareOptions are what the one checked step needs from the host.
type PrepareOptions struct {
	// PrivateExecVolume requests trusted bounded auxiliary memory storage.
	PrivateExecVolume *PrivateExecVolume
	FS                FS
	Home              string
	// Roots, when set, are the workspace roots a bind mount must lie inside
	// (CheckMountsWithin, issue #58).
	Roots []string
	// Owns says whether a named volume belongs to this environment. It is
	// required when the spec names a volume: another task's volume is refused.
	Owns func(volume string) bool
	// CacheRoots are the directories the cache object directories of
	// Spec.Alternates must lie inside (issue #45).
	CacheRoots []string
}

// PreparedSpec is a Spec that Prepare checked. Its fields are unexported, so
// the only way to get a non-zero one is Prepare, and Provision refuses any
// other (ErrNotPrepared): nothing unchecked can reach the runtime.
type PreparedSpec struct {
	privateExec *PrivateExecPlan
	spec        Spec
	ok          bool
}

// Spec returns a copy of the checked spec, with every bind mount at its
// resolved path and the cache objects as read-only mounts. An adapter mounts
// exactly these.
func (p PreparedSpec) Spec() Spec { return p.spec.clone() }

// clone returns a copy that shares no map, slice or pointer with s, so a later
// change to either cannot reach the other. Every mutable field is listed here.
// TestSpecCloneAliasesNothing walks Spec by reflection and fails when a map,
// slice or pointer field, however nested, is forgotten; the field-by-field
// TestPreparedSpecSharesNothingWithItsInputOrItsCallers checks the behaviour.
func (s Spec) clone() Spec {
	out := s
	out.Labels = cloneMap(s.Labels)
	out.Env = cloneMap(s.Env)
	out.CapDrop = cloneSlice(s.CapDrop)
	out.Tmpfs = cloneSlice(s.Tmpfs)
	out.Mounts = cloneSlice(s.Mounts)
	out.Alternates = cloneSlice(s.Alternates)
	if s.Egress != nil {
		e := *s.Egress
		e.Allow = cloneSlice(e.Allow)
		e.DenyPrefixes = cloneSlice(e.DenyPrefixes)
		out.Egress = &e
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	return append([]T{}, s...)
}

// Prepared reports whether the spec came from Prepare.
func (p PreparedSpec) Prepared() bool { return p.ok }

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Prepare is the one checked step (design §5.1). It runs Validate; requires an
// internal network and a read-only root, so hardening is required and not just
// possible; checks every bind mount with CheckMount (and within the workspace
// roots when they are set) and replaces its source with the resolved path;
// checks that every named volume belongs to this environment; checks the
// egress sidecar; and turns the cache object directories into read-only
// mounts at their host path. A spec that fails is reported whole.
func Prepare(opts PrepareOptions, spec Spec) (PreparedSpec, error) {
	if err := spec.Validate(); err != nil {
		return PreparedSpec{}, err
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !spec.Network.Internal {
		add("the environment must be on an internal network: the default network reaches the internet, the LAN and other containers")
	}
	if !spec.ReadOnlyRoot {
		add("the root filesystem must be read-only")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return PreparedSpec{}, &SpecError{Problems: problems}
	}

	out := spec.clone() // the checked spec shares nothing with the caller's
	out.Mounts = nil
	out.Alternates = nil
	for _, m := range spec.Mounts {
		switch m.Kind {
		case MountVolume:
			if opts.Owns == nil || !opts.Owns(m.Source) {
				add("volume %q does not belong to this environment", m.Source)
			}
			out.Mounts = append(out.Mounts, m)
		default:
			resolved, err := ResolveMount(opts.FS, opts.Home, m.Source)
			if err != nil {
				return PreparedSpec{}, err
			}
			if len(opts.Roots) > 0 {
				if err := withinRoots(opts.FS, opts.Roots, m.Source, resolved); err != nil {
					return PreparedSpec{}, err
				}
			}
			if !unsplittable(resolved) {
				add("mount %q resolves to %q, which holds ':'", m.Source, resolved)
				continue
			}
			m.Kind, m.Source = MountBind, resolved // the path that was checked is the path that is mounted
			out.Mounts = append(out.Mounts, m)
		}
	}
	for _, alt := range spec.Alternates {
		resolved, err := ResolveMount(opts.FS, opts.Home, alt)
		if err != nil {
			return PreparedSpec{}, err
		}
		if !insideAny(opts.FS, opts.CacheRoots, resolved) {
			add("cache objects %q are not inside a cache root", alt)
			continue
		}
		if !unsplittable(resolved) {
			add("cache objects %q resolve to %q, which holds ':'", alt, resolved)
			continue
		}
		out.Mounts = append(out.Mounts, Mount{Kind: MountBind, Source: resolved, Target: resolved, ReadOnly: true})
		out.Alternates = append(out.Alternates, resolved)
	}
	if e := spec.Egress; e != nil {
		if !ValidImage(e.Image) {
			add("the egress sidecar needs an image reference, not %q", e.Image)
		}
		if e.Proxy == "" || !filepath.IsAbs(e.Proxy) {
			add("the egress proxy binary %q must be an absolute path", e.Proxy)
		} else if resolved, err := ResolveMount(opts.FS, opts.Home, e.Proxy); err != nil {
			return PreparedSpec{}, err
		} else if !unsplittable(resolved) {
			add("the egress proxy binary %q resolves to %q, which holds ':'", e.Proxy, resolved)
		} else {
			out.Egress.Proxy = resolved // out.Egress is already a copy
		}
		for _, h := range e.Allow {
			if !validAllowEntry(h) {
				add("allowlist entry %q is not a host name or a *.name wildcard", h)
			}
		}
		for _, d := range e.DenyPrefixes {
			if msg := checkDenyPrefix(d); msg != "" {
				add("%s", msg)
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return PreparedSpec{}, &SpecError{Problems: problems}
	}
	plan, err := preparePrivateExec(opts, &out)
	if err != nil {
		return PreparedSpec{}, err
	}
	return PreparedSpec{spec: out, privateExec: plan, ok: true}, nil
}

// minDenyBits is the widest prefix the proxy is told to refuse: a /0 would
// refuse every IPv6 address.
const minDenyBits = 16

// checkDenyPrefix returns what is wrong with a denied prefix, or "".
func checkDenyPrefix(d string) string {
	p, err := netip.ParsePrefix(d)
	switch {
	case err != nil:
		return fmt.Sprintf("denied prefix %q is not a prefix", d)
	case !p.Addr().Is6() || p.Addr().Is4In6():
		return fmt.Sprintf("denied prefix %q is not an IPv6 prefix", d)
	case p.Bits() < minDenyBits:
		return fmt.Sprintf("denied prefix %q is wider than /%d, which would refuse most of the internet", d, minDenyBits)
	}
	return ""
}

// insideAny reports whether a resolved path lies inside one of the roots.
func insideAny(fsys FS, roots []string, path string) bool {
	ids := &identities{fsys: fsys, info: map[string]fs.FileInfo{}}
	for _, r := range roots {
		if resolved, err := fsys.EvalSymlinks(filepath.Clean(r)); err == nil && ids.within(path, resolved) {
			return true
		}
	}
	return false
}

// validAllowEntry accepts an allowlist entry: a host name, or `*.` and a host
// name (every subdomain, not the bare name; design §7.2). A repository's request
// never reaches here as a wildcard: the Decision names one exact host. Anything
// else with a `*` is refused.
func validAllowEntry(h string) bool {
	if rest, wild := strings.CutPrefix(h, "*."); wild {
		return validHost(rest)
	}
	return validHost(h)
}

// validHost accepts a DNS name with at least two labels whose last label is not
// numeric: no raw IP address, no wildcard, no port or path.
func validHost(h string) bool {
	h = strings.ToLower(h)
	if !hostRe.MatchString(h) {
		return false
	}
	last := h[strings.LastIndex(h, ".")+1:]
	return strings.Trim(last, "0123456789") != ""
}
