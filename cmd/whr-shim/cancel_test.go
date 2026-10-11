package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCancellationBeforePIDPublicationPreventsLaterLaunch(t *testing.T) {
	shim := buildShim(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // private directory
		t.Fatal(err)
	} //nolint:gosec // private directory
	marker := filepath.Join(dir, "cancel")
	pidfile := filepath.Join(dir, "pid")
	kill := exec.CommandContext(context.Background(), shim, "kill", "-pidfile", pidfile, "-cancel-file", marker) //nolint:gosec // test-owned shim and state directory
	if err := kill.Run(); err != nil {
		t.Fatalf("early cancel failed: %v", err)
	}
	ran := filepath.Join(dir, "ran")
	run := exec.CommandContext(context.Background(), shim, "run", "-pidfile", pidfile, "-cancel-file", marker, "--", "touch", ran) //nolint:gosec // test-owned shim and sentinel
	err := run.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("cancelled launch exit = %v", err)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Fatal("guest command launched after cancellation")
	}
}

func TestCancellationMarkerEndsPublishedProcessGroup(t *testing.T) {
	shim := buildShim(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // private directory
		t.Fatal(err)
	}
	marker, pidfile := filepath.Join(dir, "cancel"), filepath.Join(dir, "pid")
	run := exec.CommandContext(context.Background(), shim, "run", "-pidfile", pidfile, "-cancel-file", marker, "--", "sleep", "30") //nolint:gosec // test helper
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Process.Kill(); _ = run.Wait() })
	pid := waitForPID(t, pidfile)
	kill := exec.CommandContext(context.Background(), shim, "kill", "-pidfile", pidfile, "-cancel-file", marker, "-grace", "100ms") //nolint:gosec // test helper
	if err := kill.Run(); err != nil {
		t.Fatal(err)
	}
	_ = run.Wait()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("guest process survived cancellation")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("marker vanished before caller cleanup")
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatal("pidfile remained after cancellation")
	}
}
