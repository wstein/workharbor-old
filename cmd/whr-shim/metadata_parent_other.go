//go:build !linux

package main

import "golang.org/x/sys/unix"

// Guest execution is Linux-only. Other test hosts retain a conservative open;
// unavailable access fails closed instead of weakening parent permissions.
const metadataParentAccess = unix.O_RDONLY
