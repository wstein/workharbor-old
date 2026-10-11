package main

import (
	"errors"
	"os"
	"path/filepath"
)

// cancellationRequested is checked both before spawn and after PID publication.
// A kill that arrives between those points leaves a marker the launcher sees.
func cancellationRequested(file string) bool {
	if file == "" {
		return false
	}
	_, err := os.Lstat(file)
	return !os.IsNotExist(err)
}

func markCancelled(marker, pidfile string) error {
	if marker == "" {
		return nil
	}
	if !filepath.IsAbs(marker) || filepath.Dir(marker) != filepath.Dir(pidfile) {
		return errors.New("cancellation marker must be beside the absolute pidfile")
	}
	parent, err := os.Lstat(filepath.Dir(marker))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0o700 {
		return errors.New("cancellation marker needs a private directory")
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // private per-exec path, exclusive creation refuses existing links
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("cannot create cancellation marker")
	}
	return f.Close()
}
