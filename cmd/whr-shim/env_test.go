package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadExecEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs owner traversal //nolint:gosec // directory must be traversable by its owner
		t.Fatal(err)
	}
	file := filepath.Join(dir, "env")
	const secret = "value with spaces=$literal"
	if err := os.WriteFile(file, []byte("TOKEN="+secret+"\nEMPTY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := readExecEnvironment(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 || env[0] != "TOKEN="+secret || env[1] != "EMPTY=" {
		t.Fatalf("unexpected environment keys/count: %d", len(env))
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("environment file was not consumed")
	}
}

func TestReadExecEnvironmentRefusesUnsafeFilesWithoutEchoingValues(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
	}{
		{"public", "TOKEN=secret-value\n", 0o644},
		{"bad key", "BAD KEY=secret-value\n", 0o600},
		{"missing equals", "secret-value\n", 0o600},
		{"control", "TOKEN=secret-value\x00\n", 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs owner traversal //nolint:gosec // directory must be traversable by its owner
				t.Fatal(err)
			}
			file := filepath.Join(dir, "env")
			if err := os.WriteFile(file, []byte(tc.body), tc.mode); err != nil {
				t.Fatal(err)
			}
			_, err := readExecEnvironment(file)
			if err == nil {
				t.Fatal("accepted unsafe file")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("error disclosed value")
			}
		})
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs owner traversal //nolint:gosec // directory must be traversable by its owner
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("TOKEN=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readExecEnvironment(link); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestShimRunConsumesEnvironmentWithoutShellExpansion(t *testing.T) {
	shim := buildShim(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs owner traversal //nolint:gosec // private directory
		t.Fatal(err)
	}
	file := filepath.Join(dir, "env")
	const value = "literal $HOME with spaces and 'quotes'"
	if err := os.WriteFile(file, []byte("WHR_TEST_VALUE="+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), shim, "run", "-pidfile", filepath.Join(dir, "pid"), "-env-file", file, "--", "sh", "-c", `printf '%s' "$WHR_TEST_VALUE"`) //nolint:gosec // test-owned compiled shim
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("shim execution failed", err)
	}
	if string(out) != value {
		t.Fatal("environment value changed")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("env file remains")
	}
}

func TestShimExecResolvesCommandUsingRequestedPath(t *testing.T) {
	shim := buildShim(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs owner traversal
		t.Fatal(err)
	} //nolint:gosec // private directory
	inherited := filepath.Join(dir, "inherited")
	requested := filepath.Join(dir, "requested")
	for _, bin := range []string{inherited, requested} {
		if err := os.Mkdir(bin, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for bin, text := range map[string]string{inherited: "inherited", requested: "requested"} {
		if err := os.WriteFile(filepath.Join(bin, "utility"), []byte("#!/bin/sh\nprintf "+text), 0o700); err != nil { //nolint:gosec // executable test fixture
			t.Fatal(err)
		} //nolint:gosec // executable test fixture
	}
	file := filepath.Join(dir, "env")
	if err := os.WriteFile(file, []byte("PATH="+requested+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), shim, "run", "-pidfile", filepath.Join(dir, "pid"), "-env-file", file, "--", "utility") //nolint:gosec // compiled test helper
	cmd.Env = append(os.Environ(), "PATH="+inherited)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "requested" {
		t.Fatal("command resolved using inherited rather than requested PATH")
	}
}

func TestExecPathSkipsCandidateWithoutCurrentUserExecuteAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can execute files with any execute bit")
	}
	first, second := t.TempDir(), t.TempDir()
	for _, entry := range []struct {
		dir  string
		mode os.FileMode
	}{{first, 0o010}, {second, 0o700}} {
		if err := os.WriteFile(filepath.Join(entry.dir, "utility"), []byte("#!/bin/sh\nexit 0\n"), entry.mode); err != nil { //nolint:gosec // executable permissions fixture
			t.Fatal(err)
		} //nolint:gosec // intentional executable permissions fixture
	}
	got, err := resolveExecPath("utility", []string{"PATH=" + first + string(os.PathListSeparator) + second})
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(second, "utility") {
		t.Fatal("selected a candidate current user cannot execute")
	}
}
