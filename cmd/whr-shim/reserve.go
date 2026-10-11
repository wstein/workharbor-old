package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var execReservationPath = regexp.MustCompile(`^/whr-exec/exec-[0-9a-f]{32}$`)

// reserveCmd has deliberately fixed metadata-only grammar. Engine runs this
// trusted helper as root with the environment's existing dropped capabilities;
// no token is passed to or read by it, and no arbitrary command runs as root.
func reserveCmd(args []string) error { return metadataDirectoryCmd("reserve-env", args, reserveExecAt) }

func releaseCmd(args []string) error { return metadataDirectoryCmd("release-env", args, releaseExecAt) }

func metadataDirectoryCmd(command string, args []string, operation func(int, string) error) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := fs.String("dir", "", "generated private exec directory")
	if err := fs.Parse(args); err != nil {
		return errors.New("invalid metadata arguments")
	}
	if fs.NArg() != 0 || !execReservationPath.MatchString(*dir) {
		return errors.New("metadata requires an exact generated exec directory")
	}
	if os.Geteuid() != 0 {
		return errors.New("metadata operation requires the trusted root launcher")
	}
	return withReservationParent(*dir, operation)
}

func reserveExecEnvironment(dir string) error { return withReservationParent(dir, reserveExecAt) }

func reservationParentSafe(stat unix.Stat_t) bool {
	return stat.Uid == 0 && stat.Gid == 0 && stat.Mode&0o7777 == 0o711
}

func withReservationParent(dir string, operation func(int, string) error) error {
	if !execReservationPath.MatchString(dir) {
		return errors.New("metadata requires an exact generated exec directory")
	}
	parent, err := unix.Open(filepath.Dir(dir), metadataParentAccess|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open metadata parent")
	}
	defer func() { _ = unix.Close(parent) }()
	var stat unix.Stat_t
	if err := unix.Fstat(parent, &stat); err != nil || !reservationParentSafe(stat) {
		return errors.New("metadata parent must be root-owned with 0711 permissions")
	}
	return operation(parent, filepath.Base(dir))
}

func metadataNameSafe(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\")
}

// releaseExecAt removes only an empty directory entry. It never traverses a
// guest-owned child or changes unknown contents after an uncertain outcome.
func releaseExecAt(parent int, name string) error {
	if !metadataNameSafe(name) {
		return errors.New("invalid metadata directory name")
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return errors.New("private exec directory is not safely empty; state retained")
	}
	return nil
}

func reserveExecAt(parent int, name string) error {
	if !metadataNameSafe(name) {
		return errors.New("invalid metadata directory name")
	}
	if err := unix.Mkdirat(parent, name, 0o700); err != nil {
		return errors.New("cannot exclusively reserve exec directory")
	}
	success := false
	defer func() {
		if !success {
			_ = unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
		}
	}()
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open reserved exec directory")
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return errors.New("cannot protect reserved exec directory")
	}
	env, err := unix.Openat(fd, "env", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return errors.New("cannot exclusively reserve environment file")
	}
	if err := unix.Fchmod(env, 0o600); err != nil {
		_ = unix.Close(env)
		_ = unix.Unlinkat(fd, "env", 0)
		return errors.New("cannot protect reserved environment file")
	}
	if err := unix.Close(env); err != nil {
		_ = unix.Unlinkat(fd, "env", 0)
		return errors.New("cannot close reserved environment file")
	}
	success = true
	return nil
}

// clearCmd runs as the existing non-root guest user after the trusted caller has
// joined the launcher. It reads no token bytes and removes only known entries.
func clearCmd(args []string) error {
	fs := flag.NewFlagSet("clear-env", flag.ContinueOnError)
	dir := fs.String("dir", "", "generated private exec directory")
	if err := fs.Parse(args); err != nil {
		return errors.New("invalid cleanup arguments")
	}
	if fs.NArg() != 0 || !execReservationPath.MatchString(*dir) {
		return errors.New("cleanup requires an exact generated exec directory")
	}
	if os.Geteuid() == 0 {
		return errors.New("file cleanup requires the non-root guest user")
	}
	return withReservationParent(*dir, func(parent int, name string) error {
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("cannot open private exec cleanup directory")
		}
		defer func() { _ = unix.Close(fd) }()
		return clearExecAt(fd)
	})
}

func clearExecAt(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || int64(stat.Uid) != int64(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
		return errors.New("cleanup directory must belong to the guest with 0700 permissions")
	}
	for _, name := range []string{"env", "pid", "cancel"} {
		if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return errors.New("private exec cleanup incomplete; state retained")
		}
	}
	return nil
}
