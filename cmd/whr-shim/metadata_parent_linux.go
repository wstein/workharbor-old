//go:build linux

package main

import "golang.org/x/sys/unix"

// O_PATH preserves an fd identity without granting/listing directory contents.
// Guest cleanup must traverse the root-owned 0711 parent without read access.
const metadataParentAccess = unix.O_PATH
