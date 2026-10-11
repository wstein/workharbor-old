//go:build linux && (amd64 || arm64)

package btrfsquota

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReaderMeasuresIdentitiesFromFixedReadCommands(t *testing.T) {
	var commands []uint
	r := kernelReader{ioctl: func(fd int, command uint, b []byte) error {
		if fd != 42 || !bytes.Equal(b, make([]byte, len(b))) {
			t.Fatal("unexpected fd or nonzero input")
		}
		commands = append(commands, command)
		switch command {
		case fsInfoCommand:
			if len(b) != 1024 {
				t.Fatal("wrong FS_INFO layout")
			}
			copy(b[16:32], []byte("measured-fsid-12"))
		case subvolInfoCommand:
			if len(b) != 504 {
				t.Fatal("wrong GET_SUBVOL_INFO layout")
			}
			binary.NativeEndian.PutUint64(b, 300)
			binary.NativeEndian.PutUint64(b[280:], 77)
			copy(b[296:312], []byte("measured-subuuid"))
		default:
			t.Fatal("unexpected command", command)
		}
		return nil
	}}
	fs, err := r.filesystem(context.Background(), 42)
	if err != nil || string(fs.uuid[:]) != "measured-fsid-12" {
		t.Fatal("FS identity not measured", fs, err)
	}
	sub, err := r.subvolume(context.Background(), 42)
	if err != nil || sub.id != 300 || sub.generation != 77 || string(sub.uuid[:]) != "measured-subuuid" {
		t.Fatal("subvolume identity not measured", sub, err)
	}
	if len(commands) != 2 || commands[0] != 0x8400941f || commands[1] != 0x81f8943c {
		t.Fatal("wrong allowlist", commands)
	}
}

func quotaReaderFixture(t *testing.T, records []treeItem) kernelReader {
	t.Helper()
	slices.SortFunc(records, func(a, b treeItem) int { return keyCompare(a.key, b.key) })
	return kernelReader{ioctl: func(_ int, cmd uint, b []byte) error {
		n := binary.NativeEndian
		if cmd != searchCommand || n.Uint64(b[40:]) != 0 || n.Uint64(b[48:]) != math.MaxUint64 {
			t.Fatal("not a fixed unfiltered read")
		}
		lower := treeKey{n.Uint64(b[8:]), n.Uint32(b[56:]), n.Uint64(b[24:])}
		upper := treeKey{n.Uint64(b[16:]), n.Uint32(b[60:]), n.Uint64(b[32:])}
		if lower.kind == rootRefKey && (n.Uint64(b) != 1 || n.Uint32(b[64:]) != 1) {
			t.Fatal("ROOT_REF must be fixed tree1 single-child observation")
		}
		if lower.kind != rootRefKey && n.Uint64(b) != 8 {
			t.Fatal("not quota tree8")
		}
		n.PutUint32(b[64:], 0)
		for _, item := range records {
			if keyCompare(item.key, lower) >= 0 && keyCompare(item.key, upper) <= 0 {
				n.PutUint32(b[64:], 1)
				copy(b[112:], encodeItem(n, item.key, item.data))
				break // Deliberately short pages; must still establish exhaustion.
			}
		}
		return nil
	}}
}

func TestReaderQuotaRequiresMeasuredExactRecordsAndExhaustiveTopology(t *testing.T) {
	parent := uint64(1<<48 | 10)
	good := []treeItem{
		{key: treeKey{0, statusKey, 0}, data: quotaWords(1, 77, 1, 999, 456)},
		{key: treeKey{0, limitKey, parent}, data: quotaWords(1, 1000, 0, 0, 0)},
		{key: treeKey{parent, relationKey, 256}},
		{key: treeKey{parent, relationKey, 257}},
		{key: treeKey{256, relationKey, parent}},
		{key: treeKey{257, relationKey, parent}},
	}
	for _, name := range []string{"missing status", "missing limit", "missing reciprocal", "extra leaf", "extra ancestor", "nested child"} {
		t.Run(name, func(t *testing.T) {
			records := slices.Clone(good)
			switch name {
			case "missing status":
				records = records[1:]
			case "missing limit":
				records = append(records[:1], records[2:]...)
			case "missing reciprocal":
				records = records[:5]
			case "extra leaf":
				records = append(records, treeItem{key: treeKey{parent, relationKey, 258}})
			case "extra ancestor":
				records = append(records, treeItem{key: treeKey{256, relationKey, parent + 1}})
			case "nested child":
				records = append(records, treeItem{key: treeKey{256, rootRefKey, 300}, data: append(quotaWords(256, 1), 1, 0, 'x')})
			}
			if _, err := quotaReaderFixture(t, records).quota(context.Background(), 42, parent, []uint64{256, 257}, 1024); !errors.Is(err, errIncomplete) {
				t.Fatal("incomplete observation accepted", err)
			}
		})
	}
	got, err := quotaReaderFixture(t, slices.Clone(good)).quota(context.Background(), 42, parent, []uint64{256, 257}, 1024)
	if err != nil || got.maxReferenced != 1000 || got.status.generation != 77 || got.status.rescan != 999 || got.status.enableGeneration != 456 {
		t.Fatal("metadata not measured", got, err)
	}
}

func TestReaderQuotaInvalidRegistrationCannotIssueIoctl(t *testing.T) {
	for _, name := range []string{"zero limit", "wrong parent level", "zero parent", "no members", "duplicate member", "wrong leaf level", "invalid leaf"} {
		t.Run(name, func(t *testing.T) {
			r := kernelReader{ioctl: func(int, uint, []byte) error { t.Fatal("invalid configuration reached ioctl"); return nil }}
			parent, members, limit := uint64(1<<48|10), []uint64{256, 257}, uint64(1024)
			switch name {
			case "zero limit":
				limit = 0
			case "wrong parent level":
				parent = 2<<48 | 10
			case "zero parent":
				parent = 1 << 48
			case "no members":
				members = nil
			case "duplicate member":
				members[1] = 256
			case "wrong leaf level":
				members[1] = 1<<48 | 257
			case "invalid leaf":
				members[1] = 0
			}
			if _, err := r.quota(context.Background(), 42, parent, members, limit); !errors.Is(err, errIncomplete) {
				t.Fatal("invalid configuration accepted", err)
			}
		})
	}
}

func TestReaderSearchExhaustsShortPagesWithFixedNativeRanges(t *testing.T) {
	parent := uint64(1<<48 | 10)
	bounds := treeRange{treeKey{parent, relationKey, 0}, treeKey{parent, relationKey, math.MaxUint64}}
	calls := uint64(0)
	r := kernelReader{ioctl: func(fd int, command uint, b []byte) error {
		if fd != 42 || command != 0xc0709411 || len(b) != 112+maxSearchBytes {
			t.Fatal("wrong search layout")
		}
		n := binary.NativeEndian
		if n.Uint64(b) != 8 || n.Uint64(b[8:]) != parent || n.Uint64(b[16:]) != parent || n.Uint64(b[32:]) != math.MaxUint64 || n.Uint64(b[40:]) != 0 || n.Uint64(b[48:]) != math.MaxUint64 || n.Uint32(b[56:]) != relationKey || n.Uint32(b[60:]) != relationKey || n.Uint64(b[104:]) != maxSearchBytes {
			t.Fatal("unexpected search criteria")
		}
		if !bytes.Equal(b[68:104], make([]byte, 36)) {
			t.Fatal("reserved input not zero")
		}
		wantOffset := []uint64{0, 257, 258}[calls]
		if n.Uint64(b[24:]) != wantOffset {
			t.Fatal("nonprogressing pagination", calls, n.Uint64(b[24:]))
		}
		calls++
		if calls == 3 {
			n.PutUint32(b[64:], 0)
			return nil
		}
		// One item despite capacity for more must NOT be interpreted as EOF.
		n.PutUint32(b[64:], 1)
		copy(b[112:], encodeItem(n, treeKey{parent, relationKey, 255 + calls}, nil))
		return nil
	}}
	items, err := r.search(context.Background(), 42, 8, bounds, 3)
	if err != nil || len(items) != 2 || calls != 3 {
		t.Fatal("short page falsely treated as exhaustive", len(items), calls, err)
	}
}

func TestReaderSearchCannotTreatFailuresOrBudgetsAsEOF(t *testing.T) {
	bounds := treeRange{treeKey{256, relationKey, 0}, treeKey{256, relationKey, math.MaxUint64}}
	for _, name := range []string{"permission", "unsupported", "no syscall", "overflow", "too many", "nonprogress", "budget", "bad tree", "bad range", "cancel"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := uint64(0)
			r := kernelReader{ioctl: func(_ int, _ uint, b []byte) error {
				calls++
				switch name {
				case "permission":
					return unix.EPERM
				case "unsupported":
					return unix.ENOTTY
				case "no syscall":
					return unix.ENOSYS
				case "overflow":
					return unix.EOVERFLOW
				}
				n := binary.NativeEndian
				offset := calls
				if name == "nonprogress" {
					offset = 1
				}
				count := uint32(1)
				if name == "too many" {
					count = 257
				}
				n.PutUint32(b[64:], count)
				copy(b[112:], encodeItem(n, treeKey{256, relationKey, offset}, nil))
				return nil
			}}
			tree, query := uint64(8), bounds
			if name == "bad tree" {
				tree = 2
			}
			if name == "bad range" {
				query.max.objectID++
			}
			if name == "cancel" {
				cancel()
			}
			if _, err := r.search(ctx, 42, tree, query, 3); !errors.Is(err, errIncomplete) {
				t.Fatal("incomplete query accepted", err)
			}
			if (name == "bad tree" || name == "bad range" || name == "cancel") && calls != 0 {
				t.Fatal("invalid request reached ioctl")
			}
			if calls > 4 {
				t.Fatal("unbounded calls", calls)
			}
		})
	}
}

func TestReaderSearchMaximumOffsetCannotWrap(t *testing.T) {
	bounds := treeRange{treeKey{256, relationKey, math.MaxUint64}, treeKey{256, relationKey, math.MaxUint64}}
	calls := uint64(0)
	r := kernelReader{ioctl: func(_ int, _ uint, b []byte) error {
		calls++
		binary.NativeEndian.PutUint32(b[64:], 1)
		copy(b[112:], encodeItem(binary.NativeEndian, bounds.max, nil))
		return nil
	}}
	items, err := r.search(context.Background(), 42, 8, bounds, 1)
	if err != nil || calls != 1 || len(items) != 1 {
		t.Fatal("inclusive maximum not consumed safely", calls, err)
	}
}

func TestReaderRawBoundaryRejectsMutationsAndWrongLayoutsBeforeSyscall(t *testing.T) {
	// Invalid commands/layouts must never reach the kernel (fd42 is a fixture,
	// not an inspected target). In particular _IOR does NOT mean read-only.
	for _, tc := range []struct {
		command uint
		size    int
	}{
		{0x8030942b, 48}, // BTRFS_IOC_QGROUP_LIMIT: mutating despite _IOR
		{0xc0109428, 16}, // QUOTA_CTL
		{0x40189429, 24}, // QGROUP_ASSIGN
		{0, 0}, {fsInfoCommand, 1023}, {subvolInfoCommand, 503}, {searchCommand, 112},
	} {
		if err := readIoctl(42, tc.command, make([]byte, tc.size)); !errors.Is(err, errIncomplete) {
			t.Fatal("raw boundary accepted mutation/layout", tc, err)
		}
	}
	if err := readIoctl(-1, fsInfoCommand, make([]byte, 1024)); !errors.Is(err, errIncomplete) {
		t.Fatal("negative descriptor accepted", err)
	}
}

func TestReaderCancellationAndArbitraryErrorsStaySanitized(t *testing.T) {
	for _, name := range []string{"already canceled", "during ioctl", "backend error", "between pages"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := uint64(0)
			r := kernelReader{ioctl: func(_ int, _ uint, b []byte) error {
				calls++
				if name == "backend error" {
					return errors.New("private host path and credential bytes")
				}
				cancel()
				if name == "between pages" {
					binary.NativeEndian.PutUint32(b[64:], 1)
					copy(b[112:], encodeItem(binary.NativeEndian, treeKey{256, relationKey, 1}, nil))
				} else {
					b[16] = 1
				}
				return nil
			}}
			if name == "already canceled" {
				cancel()
			}
			var err error
			if name == "between pages" {
				_, err = r.search(ctx, 42, 8, treeRange{treeKey{256, relationKey, 0}, treeKey{256, relationKey, math.MaxUint64}}, 2)
			} else {
				_, err = r.filesystem(ctx, 42)
			}
			if !errors.Is(err, errIncomplete) || (name != "backend error" && !errors.Is(err, context.Canceled)) {
				t.Fatal("cancellation/error lost", err)
			}
			if name == "backend error" && err.Error() != errIncomplete.Error() {
				t.Fatal("backend detail leaked", err)
			}
			if calls > 1 || (name == "already canceled" && calls != 0) {
				t.Fatal("reads continued after cancellation", calls)
			}
		})
	}
}

func TestReaderQueryCapAndUnknownIdentitiesRemainIncomplete(t *testing.T) {
	calls := uint64(0)
	r := kernelReader{ioctl: func(_ int, cmd uint, b []byte) error {
		calls++
		if cmd == searchCommand {
			binary.NativeEndian.PutUint32(b[64:], 1)
			copy(b[112:], encodeItem(binary.NativeEndian, treeKey{256, relationKey, calls}, nil))
		}
		return nil
	}}
	if _, err := r.search(context.Background(), 42, 8, treeRange{treeKey{256, relationKey, 0}, treeKey{256, relationKey, math.MaxUint64}}, 100); !errors.Is(err, errIncomplete) || calls != 32 {
		t.Fatal("query cap falsely established EOF", calls, err)
	}
	if _, err := r.filesystem(context.Background(), 42); !errors.Is(err, errIncomplete) {
		t.Fatal("zero FSUUID accepted", err)
	}
	if _, err := r.subvolume(context.Background(), 42); !errors.Is(err, errIncomplete) {
		t.Fatal("zero subvolume identity accepted", err)
	}
	for _, bad := range []string{"reserved tail", "invalid id", "zero UUID"} {
		r.ioctl = func(_ int, _ uint, b []byte) error {
			binary.NativeEndian.PutUint64(b, 256)
			b[296] = 1
			switch bad {
			case "reserved tail":
				b[440] = 1
			case "invalid id":
				binary.NativeEndian.PutUint64(b, 1<<48|256)
			case "zero UUID":
				b[296] = 0
			}
			return nil
		}
		if _, err := r.subvolume(context.Background(), 42); !errors.Is(err, errIncomplete) {
			t.Fatal("unknown subvolume data accepted", bad, err)
		}
	}
}
