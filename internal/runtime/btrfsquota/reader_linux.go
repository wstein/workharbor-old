//go:build linux && (amd64 || arm64)

package btrfsquota

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fsInfoCommand     = 0x8400941f
	subvolInfoCommand = 0x81f8943c
	searchCommand     = 0xc0709411
)

// The v6.12 UAPI layouts and asm-generic ioctl encoding below are supported
// only on Linux amd64/arm64 (64-bit, little-endian, u64 alignment 8). No ioctl
// direction-bit heuristic is used: QGROUP_LIMIT has _IOR but mutates quotas.
// https://github.com/torvalds/linux/blob/v6.12/include/uapi/asm-generic/ioctl.h
type (
	kernelReader   struct{ ioctl func(int, uint, []byte) error }
	filesystemInfo struct{ uuid [16]byte }
	subvolumeInfo  struct {
		id, generation uint64
		uuid           [16]byte
	}
)

type quotaMetadata struct {
	status        persistedStatus
	maxReferenced uint64
}

// quota reads a non-atomic metadata observation. It neither proves live quota
// health/dedication nor prevents later nested subvolume creation.
func (r kernelReader) quota(ctx context.Context, fd int, parent uint64, members []uint64, requested uint64) (quotaMetadata, error) {
	if parent>>qgroupShift != 1 || parent&((1<<qgroupShift)-1) == 0 || len(members) == 0 || len(members) > 16 || requested == 0 {
		return quotaMetadata{}, errIncomplete
	}
	seen := map[uint64]bool{}
	for _, member := range members {
		if member < 256 || member>>qgroupShift != 0 || seen[member] {
			return quotaMetadata{}, errIncomplete
		}
		seen[member] = true
	}
	statusBytes, err := r.point(ctx, fd, treeKey{0, statusKey, 0})
	if err != nil {
		return quotaMetadata{}, err
	}
	status, err := decodeStatus(statusBytes)
	if err != nil {
		return quotaMetadata{}, err
	}
	limitBytes, err := r.point(ctx, fd, treeKey{0, limitKey, parent})
	if err != nil {
		return quotaMetadata{}, err
	}
	limit, err := decodeLimit(limitBytes, requested)
	if err != nil {
		return quotaMetadata{}, err
	}
	var relations []treeItem
	for _, group := range append([]uint64{parent}, members...) {
		bounds := treeRange{treeKey{group, relationKey, 0}, treeKey{group, relationKey, math.MaxUint64}}
		items, err := r.search(ctx, fd, 8, bounds, len(members))
		if err != nil {
			return quotaMetadata{}, err
		}
		relations = append(relations, items...)
	}
	if err := observeTopology(relations, parent, members); err != nil {
		return quotaMetadata{}, err
	}
	for _, member := range members {
		bounds := treeRange{treeKey{member, rootRefKey, 0}, treeKey{member, rootRefKey, math.MaxUint64}}
		items, err := r.search(ctx, fd, 1, bounds, 1)
		if err != nil {
			return quotaMetadata{}, err
		}
		if err := observeRootRefs(items, member); err != nil {
			return quotaMetadata{}, err
		}
	}
	return quotaMetadata{status: status, maxReferenced: limit}, nil
}

func (r kernelReader) point(ctx context.Context, fd int, key treeKey) ([]byte, error) {
	items, err := r.search(ctx, fd, 8, treeRange{key, key}, 1)
	if err != nil {
		return nil, err
	}
	if len(items) != 1 {
		return nil, errIncomplete
	}
	return items[0].data, nil
}

// readIoctl is an internal semantic allowlist, not an RPC/raw ioctl API. Its
// caller must retain the protected registered directory FD across all reads.
func readIoctl(fd int, command uint, b []byte) error {
	valid := fd >= 0 && ((command == fsInfoCommand && len(b) == 1024) ||
		(command == subvolInfoCommand && len(b) == 504) ||
		(command == searchCommand && len(b) == 112+maxSearchBytes))
	if !valid {
		return errIncomplete
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(command), uintptr(unsafe.Pointer(&b[0]))) //nolint:gosec // nonempty fixed-size UAPI buffer; syscall consumes pointer synchronously, never stores it
	runtime.KeepAlive(b)
	if errno != 0 {
		return errIncomplete
	}
	return nil
}

func (r kernelReader) call(ctx context.Context, fd int, command uint, b []byte) error {
	if ctx.Err() != nil || fd < 0 || r.ioctl == nil {
		return errors.Join(errIncomplete, ctx.Err())
	}
	if err := r.ioctl(fd, command, b); err != nil {
		// Paths, names and arbitrary injected errors never enter diagnostics.
		return errors.Join(errIncomplete, ctx.Err())
	}
	// Cancellation is checked between bounded reads. A kernel ioctl already in
	// flight is not claimed interruptible; no goroutine abandons/reuses its FD.
	if ctx.Err() != nil {
		return errors.Join(errIncomplete, ctx.Err())
	}
	return nil
}

func (r kernelReader) filesystem(ctx context.Context, fd int) (filesystemInfo, error) {
	b := make([]byte, 1024) // flags=0: request only the basic filesystem identity
	if err := r.call(ctx, fd, fsInfoCommand, b); err != nil {
		return filesystemInfo{}, err
	}
	var info filesystemInfo
	copy(info.uuid[:], b[16:32])
	if info.uuid == [16]byte{} {
		return filesystemInfo{}, errIncomplete
	}
	return info, nil
}

func (r kernelReader) subvolume(ctx context.Context, fd int) (subvolumeInfo, error) {
	b := make([]byte, 504)
	if err := r.call(ctx, fd, subvolInfoCommand, b); err != nil {
		return subvolumeInfo{}, err
	}
	info := subvolumeInfo{id: binary.NativeEndian.Uint64(b), generation: binary.NativeEndian.Uint64(b[280:])}
	copy(info.uuid[:], b[296:312])
	// The guest-controlled name and timestamps are neither used nor returned.
	if info.id < 256 || info.id>>qgroupShift != 0 || info.uuid == [16]byte{} || !bytes.Equal(b[440:], make([]byte, 64)) {
		return subvolumeInfo{}, errIncomplete
	}
	return info, nil
}

func allowedSearch(tree uint64, bounds treeRange) bool {
	a, b := bounds.min, bounds.max
	if a.objectID != b.objectID || a.kind != b.kind || a.offset > b.offset {
		return false
	}
	levelOne := func(id uint64) bool { return id>>qgroupShift == 1 && id&((1<<qgroupShift)-1) != 0 }
	leaf := a.objectID >= 256 && a.objectID>>qgroupShift == 0
	switch {
	case tree == 8 && a.kind == statusKey:
		return a.objectID == 0 && a.offset == 0 && b.offset == 0
	case tree == 8 && a.kind == limitKey:
		return a.objectID == 0 && a.offset == b.offset && levelOne(a.offset)
	case tree == 8 && a.kind == relationKey:
		return (leaf || levelOne(a.objectID)) && b.offset == math.MaxUint64
	case tree == 1 && a.kind == rootRefKey:
		return leaf && b.offset == math.MaxUint64
	default:
		return false
	}
}

func (r kernelReader) search(ctx context.Context, fd int, tree uint64, bounds treeRange, maxItems int) ([]treeItem, error) {
	if !allowedSearch(tree, bounds) || maxItems < 1 || maxItems >= maxSearchItems {
		return nil, errIncomplete
	}
	var items []treeItem
	for range 32 { // Nonprogress/cap exhaustion is incomplete, never EOF.
		b := make([]byte, 112+maxSearchBytes)
		n := binary.NativeEndian
		n.PutUint64(b, tree)
		n.PutUint64(b[8:], bounds.min.objectID)
		n.PutUint64(b[16:], bounds.max.objectID)
		n.PutUint64(b[24:], bounds.min.offset)
		n.PutUint64(b[32:], bounds.max.offset)
		// min_transid=0, max_transid=UINT64_MAX: no freshness filtering.
		n.PutUint64(b[48:], math.MaxUint64)
		n.PutUint32(b[56:], bounds.min.kind)
		n.PutUint32(b[60:], bounds.max.kind)
		capacity := maxItems + 1 - len(items) // One extra detects excess membership.
		if tree == 1 {
			capacity = 1 // Any child suffices to reject; no enumeration is needed.
		}
		n.PutUint32(b[64:], uint32(capacity))
		n.PutUint64(b[104:], maxSearchBytes)
		if err := r.call(ctx, fd, searchCommand, b); err != nil {
			return nil, err
		}
		count := n.Uint32(b[64:])
		if count > uint32(capacity) || n.Uint64(b[104:]) > maxSearchBytes {
			return nil, errIncomplete
		}
		page, err := decodeSearch(n, b[112:], count, bounds)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return items, nil // Only a successful zero-item query establishes EOF.
		}
		if tree == 1 {
			return page, nil // Presence observation only, never a no-child claim.
		}
		items = append(items, page...)
		if len(items) > maxItems {
			return nil, errIncomplete
		}
		last := page[len(page)-1].key.offset
		if last == bounds.max.offset {
			return items, nil // Inclusive maximum consumed; never wrap UINT64_MAX.
		}
		bounds.min.offset = last + 1
	}
	return nil, errIncomplete
}
