// Package btrfsquota decodes bounded, incomplete Btrfs metadata observations.
// Persisted quota-tree flags are not live kernel health; reads are not atomic
// and no observation grants admission, mutates quotas or implies freshness.
package btrfsquota

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Layouts and semantic constants are pinned to Linux v6.12 UAPI:
// https://github.com/torvalds/linux/blob/v6.12/include/uapi/linux/btrfs.h
// https://github.com/torvalds/linux/blob/v6.12/include/uapi/linux/btrfs_tree.h
// Ioctl/search headers are native endian; packed quota-tree payloads are LE.
const (
	maxSearchBytes = 64 << 10
	maxSearchItems = 256
	statusKey      = 240
	limitKey       = 244
	relationKey    = 246
	rootRefKey     = 156
	qgroupShift    = 48
)

var (
	errIncomplete        = errors.New("incomplete Btrfs metadata observation")
	errUnsupportedSchema = errors.Join(errIncomplete, errors.New("unsupported Btrfs metadata schema"))
)

type treeKey struct {
	objectID uint64
	kind     uint32
	offset   uint64
}
type treeItem struct {
	key     treeKey
	transid uint64
	data    []byte
}
type treeRange struct{ min, max treeKey }

// persistedStatus deliberately has no live Enabled/Consistent fields. Neither
// generation nor the search header's transid proves freshness or atomicity.
type persistedStatus struct{ generation, flags, rescan, enableGeneration uint64 }

func keyCompare(a, b treeKey) int {
	if a.objectID < b.objectID {
		return -1
	}
	if a.objectID > b.objectID {
		return 1
	}
	if a.kind < b.kind {
		return -1
	}
	if a.kind > b.kind {
		return 1
	}
	if a.offset < b.offset {
		return -1
	}
	if a.offset > b.offset {
		return 1
	}
	return 0
}

func decodeSearch(native binary.ByteOrder, buffer []byte, count uint32, bounds treeRange) ([]treeItem, error) {
	if native == nil || len(buffer) > maxSearchBytes || count > maxSearchItems || keyCompare(bounds.min, bounds.max) > 0 {
		return nil, errIncomplete
	}
	items := make([]treeItem, 0, count)
	for range count {
		if len(buffer) < 32 {
			return nil, errIncomplete
		}
		item := treeItem{transid: native.Uint64(buffer), key: treeKey{native.Uint64(buffer[8:]), native.Uint32(buffer[24:]), native.Uint64(buffer[16:])}}
		size := native.Uint32(buffer[28:])
		buffer = buffer[32:]
		if uint64(size) > uint64(len(buffer)) || item.key.kind > 255 || keyCompare(item.key, bounds.min) < 0 || keyCompare(item.key, bounds.max) > 0 {
			return nil, errIncomplete
		}
		if len(items) > 0 && keyCompare(items[len(items)-1].key, item.key) >= 0 {
			return nil, errIncomplete
		}
		item.data = bytes.Clone(buffer[:size])
		buffer = buffer[size:]
		items = append(items, item)
	}
	// TREE_SEARCH_V2 reports an item count, not a used-byte length. The unused
	// tail of the bounded kernel buffer is never interpreted as another item.
	return items, nil
}

func decodeStatus(data []byte) (persistedStatus, error) {
	// Only the v6.12 40-byte schema is supported. Legacy 32-byte records remain
	// incomplete rather than guessing whether enable_gen is present.
	if len(data) == 32 {
		return persistedStatus{}, errUnsupportedSchema
	}
	if len(data) != 40 {
		return persistedStatus{}, errIncomplete
	}
	version, generation, flags := binary.LittleEndian.Uint64(data), binary.LittleEndian.Uint64(data[8:]), binary.LittleEndian.Uint64(data[16:])
	if version != 1 {
		return persistedStatus{}, errUnsupportedSchema
	}
	if flags != 1 {
		return persistedStatus{}, errIncomplete
	}
	return persistedStatus{generation: generation, flags: flags, rescan: binary.LittleEndian.Uint64(data[24:]), enableGeneration: binary.LittleEndian.Uint64(data[32:])}, nil
}

func decodeLimit(data []byte, requested uint64) (uint64, error) {
	if len(data) != 40 || requested == 0 {
		return 0, errIncomplete
	}
	flags, limit := binary.LittleEndian.Uint64(data), binary.LittleEndian.Uint64(data[8:])
	// Narrow MAX_RFER only: no exclusive/reserved/compressed or unknown modes.
	if flags != 1 || limit == 0 || limit > requested || !bytes.Equal(data[16:], make([]byte, 24)) {
		return 0, errIncomplete
	}
	return limit, nil
}

func observeTopology(items []treeItem, parent uint64, members []uint64) error {
	if parent>>qgroupShift != 1 || parent&((1<<qgroupShift)-1) == 0 || len(members) == 0 || len(members) > 16 || len(items) != 2*len(members) {
		return errIncomplete
	}
	registered := map[uint64]bool{}
	for _, member := range members {
		if member < 256 || member>>qgroupShift != 0 || registered[member] {
			return errIncomplete
		}
		registered[member] = true
	}
	seen := map[treeKey]bool{}
	for _, item := range items {
		k := item.key
		if k.kind != relationKey || len(item.data) != 0 || seen[k] {
			return errIncomplete
		}
		if (k.objectID != parent || !registered[k.offset]) && (!registered[k.objectID] || k.offset != parent) {
			return errIncomplete
		}
		seen[k] = true
	}
	for member := range registered {
		if !seen[treeKey{member, relationKey, parent}] || !seen[treeKey{parent, relationKey, member}] {
			return errIncomplete
		}
	}
	return nil
}

func decodeRootRef(item treeItem, parent uint64) (uint64, error) {
	d := item.data
	if item.key.objectID != parent || item.key.kind != rootRefKey || parent < 256 || item.key.offset < 256 || len(d) < 19 || len(d) > 18+255 {
		return 0, errIncomplete
	}
	size := int(binary.LittleEndian.Uint16(d[16:]))
	if size != len(d)-18 || binary.LittleEndian.Uint64(d) < 256 || binary.LittleEndian.Uint64(d[8:]) == 0 {
		return 0, errIncomplete
	}
	name := d[18:]
	if bytes.ContainsAny(name, "/\x00") || bytes.Equal(name, []byte(".")) || bytes.Equal(name, []byte("..")) {
		return 0, errIncomplete
	}
	return item.key.offset, nil
}

func observeRootRefs(items []treeItem, parent uint64) error {
	if parent < 256 || len(items) > maxSearchItems {
		return errIncomplete
	}
	if len(items) > 0 {
		if _, err := decodeRootRef(items[0], parent); err != nil {
			return err
		}
		return errIncomplete
	}
	// This is only an observation of an exhausted bounded query. It cannot
	// prevent a guest from creating a new nested subvolume after the read.
	return nil
}
