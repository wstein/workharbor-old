//go:build linux && (amd64 || arm64)

package btrfsquota

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func registrationFixture() Registration {
	return Registration{Allocation: "allocation", DaemonID: "daemon", HostID: "host", Environment: "env", FSUUID: [16]byte{1}, ParentQgroup: 1<<48 | 10, Roots: []RegisteredRoot{
		{HostPath: "/srv/alloc/home", OwnerUID: 1000, SubvolumeID: 256, SubvolumeUUID: [16]byte{2}, Mount: MountExpectation{Kind: "volume", Source: "owned-home", Target: "/home/agent", VolumeDriver: "local", VolumeMountpoint: "/srv/alloc/home"}},
		{HostPath: "/srv/alloc/workspace", OwnerUID: 1000, SubvolumeID: 257, SubvolumeUUID: [16]byte{3}, Mount: MountExpectation{Kind: "bind", Source: "/srv/alloc/workspace", Target: "/workspace"}},
	}}
}

type registryFixtureState struct {
	paths                      map[string]rootIdentity
	fds                        map[int]rootIdentity
	next, opens, closes, reads int
	wrong                      string
	afterRead                  func(uint)
}

func registryObservationFixture(t *testing.T) (*Registry, *registryFixtureState, Registration) {
	t.Helper()
	e := registrationFixture()
	s := &registryFixtureState{paths: map[string]rootIdentity{}, fds: map[int]rootIdentity{}}
	for i, root := range e.Roots {
		s.paths[root.HostPath] = rootIdentity{uint64(100 + i), 256, root.OwnerUID}
	}
	io := registryIO{
		open: func(path string, _ uint32, owner uint32) (pinnedRoot, error) {
			id, ok := s.paths[path]
			if !ok || id.owner != owner {
				return pinnedRoot{}, errors.New("private path")
			}
			s.next++
			s.opens++
			s.fds[s.next] = id
			return pinnedRoot{s.next, id}, nil
		},
		inspect: func(fd int, owner uint32) (rootIdentity, error) {
			id, ok := s.fds[fd]
			if !ok || id.owner != owner {
				return rootIdentity{}, errIncomplete
			}
			return id, nil
		},
		close: func(fd int) error {
			if _, ok := s.fds[fd]; !ok {
				t.Fatal("closed unowned or already closed descriptor", fd)
			}
			delete(s.fds, fd)
			s.closes++
			return nil
		},
	}
	records := []treeItem{
		{key: treeKey{0, statusKey, 0}, data: quotaWords(1, 77, 1, 999, 456)},
		{key: treeKey{0, limitKey, e.ParentQgroup}, data: quotaWords(1, 1000, 0, 0, 0)},
		{key: treeKey{e.ParentQgroup, relationKey, 256}},
		{key: treeKey{e.ParentQgroup, relationKey, 257}},
		{key: treeKey{256, relationKey, e.ParentQgroup}},
		{key: treeKey{257, relationKey, e.ParentQgroup}},
	}
	search := quotaReaderFixture(t, records)
	reader := kernelReader{ioctl: func(fd int, cmd uint, b []byte) error {
		s.reads++
		id, ok := s.fds[fd]
		if !ok {
			t.Fatal("ioctl descriptor not retained")
		}
		switch cmd {
		case fsInfoCommand:
			copy(b[16:32], e.FSUUID[:])
			if s.wrong == "FSUUID" {
				b[16]++
			}
		case subvolInfoCommand:
			root := e.Roots[0]
			if id.device == 101 {
				root = e.Roots[1]
			}
			binary.NativeEndian.PutUint64(b, root.SubvolumeID)
			binary.NativeEndian.PutUint64(b[280:], 99)
			copy(b[296:312], root.SubvolumeUUID[:])
			if s.wrong == "subvolume ID" {
				binary.NativeEndian.PutUint64(b, 300)
			}
			if s.wrong == "subvolume UUID" {
				b[296]++
			}
		case searchCommand:
			if err := search.ioctl(fd, cmd, b); err != nil {
				return err
			}
		default:
			t.Fatal("unapproved command")
		}
		if s.afterRead != nil {
			s.afterRead(cmd)
		}
		return nil
	}}
	r, err := newRegistry([]Registration{e}, 0, io, reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
		if len(s.fds) != 0 {
			t.Error("retained descriptors leaked")
		}
	})
	return r, s, e
}

func TestRegistryCompositionMeasuresDataAndCopiesExpectationsWithoutAdmission(t *testing.T) {
	r, s, e := registryObservationFixture(t)
	e.Roots[0].HostPath = "/changed-after-construction"
	selection := Selection{"allocation", "daemon", "host", "env", 1024}
	got, err := r.Observe(context.Background(), selection)
	if err != nil || got.AdmissionSupported() || len(got.Measured) != 2 || got.MaxReferencedBytes != 1000 || got.StatusGeneration != 77 || got.SavedRescanProgress != 999 {
		t.Fatal("incomplete observation incorrect", got, err)
	}
	if got.Measured[0].FSUUID != [16]byte{1} || got.Measured[1].SubvolumeID != 257 || got.Measured[1].SubvolumeUUID != [16]byte{3} || got.Measured[1].Device != 101 || got.Measured[1].Inode != 256 {
		t.Fatal("kernel identities not measured", got.Measured)
	}
	if got.Expected.Roots[0].HostPath != "/srv/alloc/home" || got.Expected.Roots[0].Mount.Source != "owned-home" {
		t.Fatal("expectations changed")
	}
	got.Expected.Roots[0].Mount.Source = "mutated-result"
	got.Measured[0].SubvolumeID = 999
	if second, err := r.Observe(context.Background(), selection); err != nil || second.Expected.Roots[0].Mount.Source != "owned-home" || second.Measured[0].SubvolumeID != 256 {
		t.Fatal("returned values alias registry", err)
	}
	if len(s.fds) != 2 {
		t.Fatal("temporary recheck descriptors leaked", s.fds)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Observe(context.Background(), selection); !errors.Is(err, errIncomplete) {
		t.Fatal("closed registry admitted observation", err)
	}
}

func TestRegistryRefusesKernelAndPinnedPathIdentityChanges(t *testing.T) {
	for _, name := range []string{"FSUUID", "subvolume ID", "subvolume UUID", "replacement before", "replacement during", "FD change during", "canceled"} {
		t.Run(name, func(t *testing.T) {
			r, s, e := registryObservationFixture(t)
			s.wrong = name
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			change := func() { id := s.paths[e.Roots[0].HostPath]; id.device = 999; s.paths[e.Roots[0].HostPath] = id }
			switch name {
			case "replacement before":
				change()
			case "replacement during":
				s.afterRead = func(cmd uint) {
					if cmd == searchCommand {
						change()
					}
				}
			case "FD change during":
				s.afterRead = func(cmd uint) {
					if cmd == searchCommand {
						id := s.fds[1]
						id.device = 999
						s.fds[1] = id
					}
				}
			case "canceled":
				cancel()
			}
			got, err := r.Observe(ctx, Selection{"allocation", "daemon", "host", "env", 1024})
			if !errors.Is(err, errIncomplete) || len(got.Measured) != 0 {
				t.Fatal("identity/unknown outcome accepted", got, err)
			}
			if len(s.fds) != 2 {
				t.Fatal("recheck FD leaked", s.fds)
			}
			if name == "replacement before" && s.reads != 0 {
				t.Fatal("replaced path reached ioctl")
			}
		})
	}
}

func TestRegistryOpenFailureAndIdentityAliasesReleaseOnlyOwnedFDs(t *testing.T) {
	for _, name := range []string{"second open fails", "identity alias"} {
		t.Run(name, func(t *testing.T) {
			opened, closed := 0, 0
			io := registryIO{open: func(_ string, _ uint32, owner uint32) (pinnedRoot, error) {
				opened++
				if name == "second open fails" && opened == 2 {
					return pinnedRoot{}, errors.New("private path")
				}
				return pinnedRoot{opened, rootIdentity{100, 256, owner}}, nil
			}, inspect: func(int, uint32) (rootIdentity, error) { return rootIdentity{}, nil }, close: func(int) error { closed++; return nil }}
			_, err := newRegistry([]Registration{registrationFixture()}, 0, io, kernelReader{ioctl: func(int, uint, []byte) error { t.Fatal("constructor called ioctl"); return nil }})
			want := 1
			if name == "identity alias" {
				want = 2
			}
			if !errors.Is(err, errIncomplete) || closed != want {
				t.Fatal("failure/alias cleanup incorrect", closed, err)
			}
		})
	}
}

func TestRegisteredRootRequiresBtrfsExactRootAndExpectedOwner(t *testing.T) {
	for _, name := range []string{"wrong FS", "not root", "wrong owner", "not directory", "zero device"} {
		st := unix.Stat_t{Dev: 20, Ino: 256, Uid: 1000, Mode: unix.S_IFDIR | 0o700}
		fs := unix.Statfs_t{Type: unix.BTRFS_SUPER_MAGIC}
		switch name {
		case "wrong FS":
			fs.Type = unix.TMPFS_MAGIC
		case "not root":
			st.Ino = 300
		case "wrong owner":
			st.Uid = 2000
		case "not directory":
			st.Mode = unix.S_IFREG | 0o600
		case "zero device":
			st.Dev = 0
		}
		if _, err := inspectRootStat(st, fs, 1000); !errors.Is(err, errIncomplete) {
			t.Fatal("invalid root accepted", name, err)
		}
	}
}

func TestRegisteredWalkPinsPrivateAncestryWithoutFollowingLinks(t *testing.T) {
	for _, name := range []string{"valid", "symlink ancestor", "symlink root", "writable ancestor", "sticky ancestor", "regular file", "inspection error"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "alloc")
			target := filepath.Join(parent, "data")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			root, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(root)
			var st unix.Stat_t
			if unix.Fstat(root, &st) != nil {
				t.Fatal("fixture stat")
			}
			owner := st.Uid
			switch name {
			case "symlink ancestor":
				if err := os.Rename(parent, parent+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("alloc-real", parent); err != nil {
					t.Fatal(err)
				}
			case "symlink root":
				if err := os.Rename(target, target+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("data-real", target); err != nil {
					t.Fatal(err)
				}
			case "writable ancestor", "sticky ancestor":
				mode := os.FileMode(0o777)
				if name == "sticky ancestor" {
					mode |= os.ModeSticky
				}
				if err := os.Chmod(parent, mode); err != nil {
					t.Fatal(err)
				} //nolint:gosec // intentionally unsafe disposable ancestor fixture
			case "regular file":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			inspected := -1
			pin, err := walkRegistered(root, "/alloc/data", owner, owner, func(fd int, uid uint32) (rootIdentity, error) {
				inspected = fd
				if uid != owner {
					t.Fatal("owner changed")
				}
				var actual unix.Stat_t
				if unix.Fstat(fd, &actual) != nil {
					t.Fatal("descriptor not retained")
				}
				if name == "inspection error" {
					return rootIdentity{}, errors.New("private host path")
				}
				return rootIdentity{actual.Dev, actual.Ino, actual.Uid}, nil
			})
			if name == "valid" {
				if err != nil || pin.fd < 0 || inspected != pin.fd {
					t.Fatal("root not pinned", pin, err)
				}
				defer unix.Close(pin.fd)
				if err := os.Rename(target, target+"-moved"); err != nil {
					t.Fatal(err)
				}
				var actual unix.Stat_t
				if unix.Fstat(pin.fd, &actual) != nil || actual.Ino != pin.identity.inode {
					t.Fatal("rename redirected retained FD")
				}
			} else {
				if !errors.Is(err, errIncomplete) {
					t.Fatal("unsafe ancestry accepted", err)
				}
				if inspected >= 0 {
					var actual unix.Stat_t
					if !errors.Is(unix.Fstat(inspected, &actual), unix.EBADF) {
						t.Fatal("failed inspection leaked FD")
					}
				}
			}
		})
	}
}

func TestRegistryRejectsDuplicateSharedAndRemappedRegistrationsBeforeIO(t *testing.T) {
	for _, name := range []string{"duplicate handle", "shared subvolume", "shared UUID", "shared aggregate", "shared environment", "remap", "wrong volume", "overlap", "bad token", "zero FSUUID", "wrong parent", "duplicate target", "duplicate path", "host home root", "nested target"} {
		t.Run(name, func(t *testing.T) {
			a := registrationFixture()
			entries := []Registration{a}
			switch name {
			case "duplicate handle":
				entries = append(entries, a)
			case "shared subvolume":
				a.Roots[1].SubvolumeID = a.Roots[0].SubvolumeID
			case "shared UUID":
				a.Roots[1].SubvolumeUUID = a.Roots[0].SubvolumeUUID
			case "shared aggregate", "shared environment":
				b := registrationFixture()
				b.Allocation = "other"
				if name == "shared aggregate" {
					b.Environment = "other"
				}
				entries = append(entries, b)
			case "remap":
				a.Roots[1].Mount.Source = "/different"
			case "wrong volume":
				a.Roots[0].Mount.VolumeDriver = "other"
			case "overlap":
				a.Roots[1].HostPath = a.Roots[0].HostPath + "/child"
				a.Roots[1].Mount.Source = a.Roots[1].HostPath
			case "bad token":
				a.Environment = "private\nvalue"
			case "zero FSUUID":
				a.FSUUID = [16]byte{}
			case "wrong parent":
				a.ParentQgroup = 2<<48 | 10
			case "duplicate target":
				a.Roots[1].Mount.Target = a.Roots[0].Mount.Target
			case "duplicate path":
				a.Roots[1].HostPath = a.Roots[0].HostPath
				a.Roots[1].Mount.Source = a.Roots[1].HostPath
			case "host home root":
				a.Roots[1].HostPath = "/home"
				a.Roots[1].Mount.Source = "/home"
			case "nested target":
				a.Roots[1].Mount.Target = a.Roots[0].Mount.Target + "/workspace"
			}
			entries[0] = a
			io := registryIO{close: func(int) error { return nil }, inspect: func(int, uint32) (rootIdentity, error) { return rootIdentity{}, nil }, open: func(string, uint32, uint32) (pinnedRoot, error) {
				t.Fatal("invalid registration reached filesystem")
				return pinnedRoot{}, nil
			}}
			if _, err := newRegistry(entries, 0, io, kernelReader{ioctl: func(int, uint, []byte) error { t.Fatal("invalid registration reached ioctl"); return nil }}); !errors.Is(err, errIncomplete) {
				t.Fatal("invalid registration accepted", err)
			}
		})
	}
}

func TestRegistrySelectionMustMatchRegisteredOpaqueIdentities(t *testing.T) {
	opened, closed := 0, 0
	io := registryIO{inspect: func(int, uint32) (rootIdentity, error) { return rootIdentity{}, nil }, open: func(_ string, _ uint32, owner uint32) (pinnedRoot, error) {
		opened++
		device := uint64(1)
		if opened == 2 {
			device = 2
		}
		return pinnedRoot{fd: opened, identity: rootIdentity{device, 256, owner}}, nil
	}, close: func(int) error { closed++; return nil }}
	r, err := newRegistry([]Registration{registrationFixture()}, 0, io, kernelReader{ioctl: func(int, uint, []byte) error { t.Fatal("invalid selection reached ioctl"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, name := range []string{"unknown handle", "wrong daemon", "wrong host", "wrong env", "zero bytes", "path handle"} {
		s := Selection{"allocation", "daemon", "host", "env", 1024}
		switch name {
		case "unknown handle":
			s.Allocation = "other"
		case "wrong daemon":
			s.DaemonID = "other"
		case "wrong host":
			s.HostID = "other"
		case "wrong env":
			s.Environment = "other"
		case "zero bytes":
			s.RequestedBytes = 0
		case "path handle":
			s.Allocation = "/srv/alloc/home"
		}
		if _, err := r.Observe(context.Background(), s); !errors.Is(err, errIncomplete) {
			t.Fatal("invalid selector accepted", name, err)
		}
	}
	if opened != 2 || closed != 0 {
		t.Fatal("invalid selection changed descriptor inventory", opened, closed)
	}
}
