//go:build linux && (amd64 || arm64)

package btrfsquota

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// MountExpectation is operator configuration, never a measured Engine binding.
type MountExpectation struct{ Kind, Source, Target, VolumeDriver, VolumeMountpoint string }

// RegisteredRoot identifies an operator-preprovisioned subvolume root.
type RegisteredRoot struct {
	HostPath      string
	OwnerUID      uint32
	SubvolumeID   uint64
	SubvolumeUUID [16]byte
	Mount         MountExpectation
}

// Registration binds an opaque allocation to trusted operator expectations.
type Registration struct {
	Allocation, DaemonID, HostID, Environment string
	FSUUID                                    [16]byte
	ParentQgroup                              uint64
	Roots                                     []RegisteredRoot
}

// Selection contains opaque identities only, never request-selected paths.
type Selection struct {
	Allocation, DaemonID, HostID, Environment string
	RequestedBytes                            uint64
}

// RootObservation contains measured kernel and retained directory identities.
type RootObservation struct {
	FSUUID, SubvolumeUUID                  [16]byte
	SubvolumeID, Generation, Device, Inode uint64
}

// Observation is incomplete, non-atomic evidence, separate from expectations.
type Observation struct {
	Expected                                                             Registration
	Measured                                                             []RootObservation
	MaxReferencedBytes                                                   uint64
	StatusGeneration, StatusFlags, SavedRescanProgress, EnableGeneration uint64
}

// AdmissionSupported remains false: live health, containment and Engine/memory
// coverage are unproved. No complete QuotaProof is produced.
func (Observation) AdmissionSupported() bool { return false }

type rootIdentity struct {
	device, inode uint64
	owner         uint32
}
type pinnedRoot struct {
	fd       int
	identity rootIdentity
}
type registryIO struct {
	open    func(string, uint32, uint32) (pinnedRoot, error)
	inspect func(int, uint32) (rootIdentity, error)
	close   func(int) error
}

// Registry holds immutable operator registrations and retained directory FDs.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registeredAllocation
	owner   uint32
	io      registryIO
	reader  kernelReader
}
type registeredAllocation struct {
	expected Registration
	roots    []pinnedRoot
}

// NewRegistry reads no repository metadata and grants no privileges. The caller
// supplies trusted operator configuration; this does not load/install a helper.
// Construction checks protected paths/root identities; Observe additionally
// compares measured filesystem/subvolume metadata with the copied expectations.
func NewRegistry(entries []Registration, operatorUID uint32) (*Registry, error) {
	return newRegistry(entries, operatorUID, registryIO{open: openRegistered, inspect: inspectRegistered, close: unix.Close}, kernelReader{ioctl: readIoctl})
}

var registryToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func registryPath(path string) bool {
	if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(strings.Split(path, "/")) > 64 {
		return false
	}
	for _, c := range path {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func validateRegistrations(entries []Registration) bool {
	if len(entries) == 0 || len(entries) > 16 {
		return false
	}
	handles, environments, volumes := map[string]bool{}, map[[3]string]bool{}, map[[2]string]bool{}
	type fsID struct {
		fs [16]byte
		id uint64
	}
	groups, subvolumes, uuids := map[fsID]bool{}, map[fsID]bool{}, map[[16]byte]bool{}
	var paths []string
	for _, e := range entries {
		for _, s := range []string{e.Allocation, e.DaemonID, e.HostID, e.Environment} {
			if !registryToken.MatchString(s) {
				return false
			}
		}
		env := [3]string{e.HostID, e.DaemonID, e.Environment}
		group := fsID{e.FSUUID, e.ParentQgroup}
		if handles[e.Allocation] || environments[env] || groups[group] || e.FSUUID == [16]byte{} || e.ParentQgroup>>qgroupShift != 1 || e.ParentQgroup&((1<<qgroupShift)-1) == 0 || len(e.Roots) == 0 || len(e.Roots) > 16 {
			return false
		}
		handles[e.Allocation], environments[env], groups[group] = true, true, true
		targets := map[string]bool{}
		for _, root := range e.Roots {
			id := fsID{e.FSUUID, root.SubvolumeID}
			if !registryPath(root.HostPath) || root.SubvolumeID < 256 || root.SubvolumeID>>qgroupShift != 0 || root.SubvolumeUUID == [16]byte{} || subvolumes[id] || uuids[root.SubvolumeUUID] || !registryPath(root.Mount.Target) || targets[root.Mount.Target] {
				return false
			}
			for _, prefix := range []string{"/home/", "/root/", "/Users/"} {
				if root.HostPath == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(root.HostPath, prefix) {
					return false
				}
			}
			for _, path := range paths {
				if path == root.HostPath || strings.HasPrefix(path, root.HostPath+"/") || strings.HasPrefix(root.HostPath, path+"/") {
					return false
				}
			}
			for target := range targets {
				if strings.HasPrefix(target, root.Mount.Target+"/") || strings.HasPrefix(root.Mount.Target, target+"/") {
					return false
				}
			}
			paths = append(paths, root.HostPath)
			subvolumes[id], uuids[root.SubvolumeUUID], targets[root.Mount.Target] = true, true, true
			m := root.Mount
			switch m.Kind {
			case "bind":
				if m.Source != root.HostPath || m.VolumeDriver != "" || m.VolumeMountpoint != "" {
					return false
				}
			case "volume":
				key := [2]string{e.DaemonID, m.Source}
				if !registryToken.MatchString(m.Source) || volumes[key] || m.VolumeDriver != "local" || m.VolumeMountpoint != root.HostPath {
					return false
				}
				volumes[key] = true
			default:
				return false
			}
		}
	}
	return true
}

func newRegistry(entries []Registration, owner uint32, io registryIO, reader kernelReader) (*Registry, error) {
	if !validateRegistrations(entries) || io.open == nil || io.inspect == nil || io.close == nil || reader.ioctl == nil {
		return nil, errIncomplete
	}
	r := &Registry{entries: map[string]registeredAllocation{}, owner: owner, io: io, reader: reader}
	seen := map[[2]uint64]bool{}
	for _, e := range entries {
		e.Roots = slices.Clone(e.Roots)
		a := registeredAllocation{expected: e}
		r.entries[e.Allocation] = a
		for _, root := range e.Roots {
			pin, err := io.open(root.HostPath, owner, root.OwnerUID)
			if err != nil {
				_ = r.Close()
				return nil, errIncomplete
			}
			a.roots = append(a.roots, pin)
			r.entries[e.Allocation] = a
			key := [2]uint64{pin.identity.device, pin.identity.inode}
			if pin.fd < 0 || pin.identity.device == 0 || pin.identity.inode != 256 || pin.identity.owner != root.OwnerUID || seen[key] {
				_ = r.Close()
				return nil, errIncomplete
			}
			seen[key] = true
		}
	}
	return r, nil
}

// Close releases only this registry's descriptors, never operator backings.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result error
	for _, a := range r.entries {
		for _, pin := range a.roots {
			if r.io.close(pin.fd) != nil {
				result = errIncomplete
			}
		}
	}
	r.entries = nil
	return result
}

// Observe selects one registered allocation and reads incomplete observations.
func (r *Registry) Observe(ctx context.Context, selection Selection) (Observation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ctx.Err() != nil {
		return Observation{}, errors.Join(errIncomplete, ctx.Err())
	}
	a, ok := r.entries[selection.Allocation]
	e := a.expected
	if !ok || selection.RequestedBytes == 0 || selection.DaemonID != e.DaemonID || selection.HostID != e.HostID || selection.Environment != e.Environment {
		return Observation{}, errIncomplete
	}
	observation := Observation{Expected: e}
	observation.Expected.Roots = slices.Clone(e.Roots)
	var members []uint64
	for i, root := range e.Roots {
		if ctx.Err() != nil {
			return Observation{}, errors.Join(errIncomplete, ctx.Err())
		}
		pin := a.roots[i]
		if err := r.checkPath(root, pin); err != nil {
			return Observation{}, err
		}
		fs, err := r.reader.filesystem(ctx, pin.fd)
		if err != nil {
			return Observation{}, err
		}
		sub, err := r.reader.subvolume(ctx, pin.fd)
		if err != nil {
			return Observation{}, err
		}
		if fs.uuid != e.FSUUID || sub.id != root.SubvolumeID || sub.uuid != root.SubvolumeUUID {
			return Observation{}, errIncomplete
		}
		members = append(members, sub.id)
		observation.Measured = append(observation.Measured, RootObservation{FSUUID: fs.uuid, SubvolumeUUID: sub.uuid, SubvolumeID: sub.id, Generation: sub.generation, Device: pin.identity.device, Inode: pin.identity.inode})
	}
	metadata, err := r.reader.quota(ctx, a.roots[0].fd, e.ParentQgroup, members, selection.RequestedBytes)
	if err != nil {
		return Observation{}, err
	}
	for i, root := range e.Roots {
		if ctx.Err() != nil {
			return Observation{}, errors.Join(errIncomplete, ctx.Err())
		}
		if err := r.checkPath(root, a.roots[i]); err != nil {
			return Observation{}, err
		}
	}
	if ctx.Err() != nil {
		return Observation{}, errors.Join(errIncomplete, ctx.Err())
	}
	observation.MaxReferencedBytes = metadata.maxReferenced
	observation.StatusGeneration, observation.StatusFlags = metadata.status.generation, metadata.status.flags
	observation.SavedRescanProgress, observation.EnableGeneration = metadata.status.rescan, metadata.status.enableGeneration
	return observation, nil
}

func (r *Registry) checkPath(root RegisteredRoot, pin pinnedRoot) error {
	identity, err := r.io.inspect(pin.fd, root.OwnerUID)
	if err != nil || identity != pin.identity {
		return errIncomplete
	}
	current, err := r.io.open(root.HostPath, r.owner, root.OwnerUID)
	if err != nil {
		return errIncomplete
	}
	closeErr := r.io.close(current.fd)
	if closeErr != nil || current.identity != pin.identity {
		return errIncomplete
	}
	return nil
}

func openRegistered(path string, operatorUID, rootUID uint32) (pinnedRoot, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return pinnedRoot{}, errIncomplete
	}
	defer unix.Close(fd)
	return walkRegistered(fd, path, operatorUID, rootUID, inspectRegistered)
}

func inspectRegistered(fd int, owner uint32) (rootIdentity, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	if unix.Fstat(fd, &st) != nil || unix.Fstatfs(fd, &fs) != nil {
		return rootIdentity{}, errIncomplete
	}
	return inspectRootStat(st, fs, owner)
}

// walkRegistered pins each component relative to its predecessor. Every
// containing directory must be root/operator-owned and not group/world
// writable; even sticky temporary ancestors are deliberately unsupported.
// The final root may be guest-writable, but its entry has protected ancestry.
func walkRegistered(rootFD int, path string, operatorUID, rootUID uint32, inspect func(int, uint32) (rootIdentity, error)) (pinnedRoot, error) {
	if rootFD < 0 || !registryPath(path) || inspect == nil {
		return pinnedRoot{}, errIncomplete
	}
	fd, err := unix.FcntlInt(uintptr(rootFD), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return pinnedRoot{}, errIncomplete
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && st.Uid != operatorUID) || st.Mode&0o022 != 0 {
			_ = unix.Close(fd)
			return pinnedRoot{}, errIncomplete
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return pinnedRoot{}, errIncomplete
		}
		fd = next
	}
	identity, err := inspect(fd, rootUID)
	if err != nil {
		_ = unix.Close(fd)
		return pinnedRoot{}, errIncomplete
	}
	return pinnedRoot{fd: fd, identity: identity}, nil
}

func inspectRootStat(st unix.Stat_t, fs unix.Statfs_t, owner uint32) (rootIdentity, error) {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != owner || st.Ino != 256 || st.Dev == 0 || fs.Type != unix.BTRFS_SUPER_MAGIC {
		return rootIdentity{}, errIncomplete
	}
	return rootIdentity{st.Dev, st.Ino, st.Uid}, nil
}
