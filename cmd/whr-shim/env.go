package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var execEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// readExecEnvironment consumes a private one-use file without interpreting shell
// syntax or putting values into argv. Errors never include its contents.
func readExecEnvironment(name string) ([]string, error) {
	parent, err := os.Lstat(filepath.Dir(name))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0o700 {
		return nil, errors.New("env file needs a private directory")
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open private env file")
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("env file must be a regular 0600 file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return nil, errors.New("env file belongs to another user")
	} //nolint:gosec // uid fits int on supported hosts
	if err := os.Remove(name); err != nil {
		return nil, errors.New("cannot consume private env file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("cannot read env file or file exceeds limit")
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		key, value, ok := strings.Cut(string(line), "=")
		if !ok || !execEnvKey.MatchString(key) || seen[key] || strings.ContainsAny(value, "\r\x00") {
			return nil, errors.New("invalid env file entry")
		}
		seen[key] = true
		out = append(out, string(line))
	}
	return out, nil
}

// resolveExecPath applies the child's effective PATH before exec.Command can
// resolve a bare command using the launcher's inherited environment.
func resolveExecPath(name string, env []string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		return name, nil
	}
	search := ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			search = value
		}
	}
	for _, dir := range filepath.SplitList(search) {
		candidate := filepath.Join(dir, name)
		absolute := filepath.IsAbs(candidate)
		if !absolute {
			candidate = "." + string(os.PathSeparator) + candidate
		}
		resolved, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if !absolute {
			return "", errors.New("cannot execute a relative PATH entry")
		}
		return resolved, nil
	}
	return "", errors.New("cannot find executable in effective PATH")
}
