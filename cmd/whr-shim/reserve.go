package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

var execReservationPath = regexp.MustCompile(`^/tmp/whr-exec-[0-9a-f]{32}$`)

// reserveCmd has deliberately fixed metadata-only grammar. Engine runs this
// trusted helper as root with the environment's existing dropped capabilities;
// no token is passed to or read by it, and no arbitrary command runs as root.
func reserveCmd(args []string) error {
	fs := flag.NewFlagSet("reserve-env", flag.ContinueOnError)
	dir := fs.String("dir", "", "generated private exec directory")
	if err := fs.Parse(args); err != nil {
		return errors.New("invalid reservation arguments")
	}
	if fs.NArg() != 0 || !execReservationPath.MatchString(*dir) {
		return errors.New("reservation requires an exact generated exec directory")
	}
	if os.Geteuid() != 0 {
		return errors.New("metadata reservation requires the trusted root launcher")
	}
	return reserveExecEnvironment(*dir)
}

func reserveExecEnvironment(dir string) error {
	parent, err := unix.Open(filepath.Dir(dir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open reservation parent")
	}
	defer func() { _ = unix.Close(parent) }()
	var parentStat unix.Stat_t
	if err := unix.Fstat(parent, &parentStat); err != nil || parentStat.Uid != 0 || parentStat.Gid != 0 || parentStat.Mode&0o7777 != 0o1777 {
		return errors.New("reservation parent must be root-owned with sticky 1777 permissions")
	}
	name := filepath.Base(dir)
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
