package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExecReservationIsExclusiveAndPrivate(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "exec-test")
	fd, err := unix.Open(parent, metadataParentAccess|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := reserveExecAt(fd, filepath.Base(dir)); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "env"): 0o600} {
		info, err := os.Lstat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("reservation modes = %v %v", info, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reserveExecAt(fd, filepath.Base(dir)); err == nil {
		t.Fatal("existing reservation was reused")
	}
	data, err := os.ReadFile(filepath.Join(dir, "env")) //nolint:gosec // private disposable test fixture
	if err != nil || string(data) != "keep" {
		t.Fatal("duplicate reservation altered existing file")
	}
}

func TestExecReservationRefusesSymlinkAndUntrustedGrammar(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := reserveExecEnvironment(link); err == nil {
		t.Fatal("symlink reservation accepted")
	}
	if _, err := os.Lstat(filepath.Join(target, "env")); !os.IsNotExist(err) {
		t.Fatal("reservation changed symlink target")
	}
	for _, dir := range []string{"/tmp/other", "/tmp/whr-exec-../x", "relative", "/tmp/whr-exec-00000000000000000000000000000000/subdir"} {
		if err := reserveCmd([]string{"-dir", dir}); err == nil {
			t.Fatalf("untrusted reserve grammar accepted %s", dir)
		}
	}
}

func TestExecReservationRejectsGuestOwnedParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an ordinary user-owned test parent")
	}
	dir := filepath.Join(t.TempDir(), "reserved")
	if err := reserveExecEnvironment(dir); err == nil {
		t.Fatal("guest-owned staging parent accepted")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("unsafe parent changed before rejection")
	}
}

func TestPrivateVolumeReservationUsesFixedStableParentGrammar(t *testing.T) {
	good := "/whr-exec/exec-0123456789abcdef0123456789abcdef"
	if !execReservationPath.MatchString(good) {
		t.Fatal("fixed memory-volume path rejected")
	}
	for _, bad := range []string{"/tmp/whr-exec-0123456789abcdef0123456789abcdef", "/whr-exec/../tmp/exec-0123456789abcdef0123456789abcdef", good + "/child"} {
		if execReservationPath.MatchString(bad) {
			t.Fatalf("unsafe path accepted: %s", bad)
		}
	}
	for _, stat := range []unix.Stat_t{{Uid: 0, Gid: 0, Mode: 0o1777}, {Uid: 1000, Gid: 1000, Mode: 0o711}, {Uid: 0, Gid: 1000, Mode: 0o711}, {Uid: 0, Gid: 0, Mode: 0o777}} {
		if reservationParentSafe(stat) {
			t.Fatalf("unsafe parent accepted: %+v", stat)
		}
	}
	if !reservationParentSafe(unix.Stat_t{Uid: 0, Gid: 0, Mode: 0o711}) {
		t.Fatal("planned root-owned parent rejected")
	}
}

func TestMetadataReleaseOnlyRemovesExactEmptyDirectory(t *testing.T) {
	parent := t.TempDir()
	fd, err := unix.Open(parent, metadataParentAccess|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	for _, kind := range []string{"empty", "unknown contents", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(parent, kind)
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "unknown contents" {
					if err := os.WriteFile(filepath.Join(dir, "unknown"), []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := releaseExecAt(fd, filepath.Base(dir))
			if kind == "empty" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("release traversed untrusted contents")
				}
				if _, err := os.Lstat(dir); err != nil {
					t.Fatal("release modified uncertain state")
				}
			}
		})
	}
}

func TestGuestClearOnlyUnlinksKnownFilesAfterCallerJoinsLauncher(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // private test directory needs traversal
		t.Fatal(err)
	}
	for _, name := range []string{"env", "pid", "cancel", "unknown"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := clearExecAt(fd); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"env", "pid", "cancel"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("known file remains: %s %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "unknown")); err != nil {
		t.Fatal("unknown contents removed")
	}
}

func TestGuestCleanupNeverFollowsFileSymlinksOrRecursesDirectories(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"symlink", "directory", "unsafe permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // private test directory needs traversal
				t.Fatal(err)
			}
			known := filepath.Join(dir, "env")
			switch kind {
			case "symlink":
				if err := os.Symlink(outside, known); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(known, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unsafe permissions":
				if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // intentionally unsafe test directory
					t.Fatal(err)
				}
			}
			fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unix.Close(fd) }()
			err = clearExecAt(fd)
			if kind == "symlink" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe cleanup condition accepted")
			}
			data, err := os.ReadFile(outside) //nolint:gosec // disposable test fixture
			if err != nil || string(data) != "keep" {
				t.Fatal("cleanup modified an outside path")
			}
		})
	}
}
