package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecReservationIsExclusiveAndPrivate(t *testing.T) {
	parent, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := os.CreateTemp(parent, "whr-reserve-test-*")
	if err != nil {
		t.Fatal(err)
	}
	dir := placeholder.Name()
	_ = placeholder.Close()
	_ = os.Remove(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := reserveExecEnvironment(dir); err != nil {
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
	if err := reserveExecEnvironment(dir); err == nil {
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
