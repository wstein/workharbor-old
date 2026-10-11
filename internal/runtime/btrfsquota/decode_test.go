package btrfsquota

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func encodeItem(order binary.ByteOrder, key treeKey, payload []byte) []byte {
	size := len(payload)
	if size < 0 || size > maxSearchBytes {
		panic("oversized fixture")
	}
	b := make([]byte, 32+size)
	order.PutUint64(b, 17)
	order.PutUint64(b[8:], key.objectID)
	order.PutUint64(b[16:], key.offset)
	order.PutUint32(b[24:], key.kind)
	order.PutUint32(b[28:], uint32(size))
	copy(b[32:], payload)
	return b
}

func quotaWords(values ...uint64) []byte {
	b := make([]byte, len(values)*8)
	for i, v := range values {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	return b
}

func TestSearchRejectsTruncationOrderingAndBounds(t *testing.T) {
	bounds := treeRange{treeKey{0, 240, 0}, treeKey{0, 240, 0}}
	for _, name := range []string{"header", "payload", "overflow length", "duplicate", "outside range", "too many", "oversize buffer", "invalid range", "expanded type"} {
		t.Run(name, func(t *testing.T) {
			count := uint32(1)
			b := encodeItem(binary.LittleEndian, bounds.min, quotaWords(1, 2, 1, 0, 0))
			query := bounds
			switch name {
			case "header":
				b = b[:31]
			case "payload":
				b = b[:len(b)-1]
			case "overflow length":
				binary.LittleEndian.PutUint32(b[28:], math.MaxUint32)
			case "duplicate":
				b = append(b, b...)
				count = 2
			case "outside range":
				binary.LittleEndian.PutUint64(b[8:], 1)
			case "too many":
				count = 257
			case "oversize buffer":
				b = make([]byte, 65537)
			case "invalid range":
				query.min.objectID = 1
			case "expanded type":
				binary.LittleEndian.PutUint32(b[24:], 256)
			}
			if _, err := decodeSearch(binary.LittleEndian, b, count, query); !errors.Is(err, errIncomplete) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
}

func TestSearchNativeHeadersLittleEndianPayloadAndCopies(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		key := treeKey{0, 240, 0}
		payload := quotaWords(1, 123, 1, 0, 0)
		b := encodeItem(order, key, payload)
		items, err := decodeSearch(order, b, 1, treeRange{key, key})
		if err != nil || len(items) != 1 {
			t.Fatalf("decode=%+v %v", items, err)
		}
		status, err := decodeStatus(items[0].data)
		if err != nil || status.generation != 123 || status.flags != 1 {
			t.Fatalf("payload endian=%+v %v", status, err)
		}
		b[32] = 99
		if items[0].data[0] != 1 {
			t.Fatal("item data aliases kernel buffer")
		}
	}
}

func TestPersistedStatusAndLimitRefuseUnknownOrUnenforcedData(t *testing.T) {
	for _, name := range []string{"legacy size", "unknown version", "disabled", "rescan", "inconsistent", "simple", "unknown flags", "exclusive only", "no limit", "zero limit", "oversize limit", "reserved limit"} {
		t.Run(name, func(t *testing.T) {
			status := quotaWords(1, 123, 1, 0, 0)
			limit := quotaWords(1, 1024, 0, 0, 0)
			whichStatus := true
			switch name {
			case "legacy size":
				status = status[:32]
			case "unknown version":
				status[0] = 2
			case "disabled":
				status[16] = 0
			case "rescan":
				status[16] = 3
			case "inconsistent":
				status[16] = 5
			case "simple":
				status[16] = 9
			case "unknown flags":
				status[16] = 17
			case "exclusive only":
				limit = quotaWords(2, 0, 1024, 0, 0)
				whichStatus = false
			case "no limit":
				limit = quotaWords(0, 1024, 0, 0, 0)
				whichStatus = false
			case "zero limit":
				limit = quotaWords(1, 0, 0, 0, 0)
				whichStatus = false
			case "oversize limit":
				limit = quotaWords(1, 1025, 0, 0, 0)
				whichStatus = false
			case "reserved limit":
				limit = quotaWords(4, 1024, 0, 1, 0)
				whichStatus = false
			}
			var err error
			if whichStatus {
				_, err = decodeStatus(status)
			} else {
				_, err = decodeLimit(limit, 1024)
			}
			if !errors.Is(err, errIncomplete) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
	if got, err := decodeLimit(quotaWords(1, 1024, 0, 0, 0), 1024); err != nil || got != 1024 {
		t.Fatal(got, err)
	}
}

func TestTopologyRequiresExactReciprocalLevelOneMembers(t *testing.T) {
	parent := uint64(1<<48 | 100)
	members := []uint64{256, 257}
	good := []treeItem{{key: treeKey{256, 246, parent}}, {key: treeKey{257, 246, parent}}, {key: treeKey{parent, 246, 256}}, {key: treeKey{parent, 246, 257}}}
	for _, name := range []string{"missing reciprocal", "extra child", "extra parent", "intermediate", "duplicate", "payload", "duplicate registration"} {
		t.Run(name, func(t *testing.T) {
			items := append([]treeItem{}, good...)
			expected := append([]uint64{}, members...)
			switch name {
			case "missing reciprocal":
				items = items[:3]
			case "extra child":
				items = append(items, treeItem{key: treeKey{parent, 246, 258}})
			case "extra parent":
				items = append(items, treeItem{key: treeKey{256, 246, 1<<48 | 101}})
			case "intermediate":
				items[0].key.objectID = 2<<48 | 256
			case "duplicate":
				items = append(items, items[0])
			case "payload":
				items[0].data = []byte{1}
			case "duplicate registration":
				expected[1] = expected[0]
			}
			if err := observeTopology(items, parent, expected); !errors.Is(err, errIncomplete) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
	if err := observeTopology(good, parent, members); err != nil {
		t.Fatal(err)
	}
}

func TestRootRefsCannotClaimNoNestedSubvolumeFromMalformedData(t *testing.T) {
	for _, name := range []string{"nested child", "truncated", "bad name length", "wrong parent", "wrong kind"} {
		t.Run(name, func(t *testing.T) {
			data := make([]byte, 19)
			binary.LittleEndian.PutUint64(data, 256)
			binary.LittleEndian.PutUint64(data[8:], 1)
			binary.LittleEndian.PutUint16(data[16:], 1)
			data[18] = 'x'
			item := treeItem{key: treeKey{256, 156, 300}, data: data}
			switch name {
			case "truncated":
				item.data = data[:17]
			case "bad name length":
				item.data[16] = 2
			case "wrong parent":
				item.key.objectID = 257
			case "wrong kind":
				item.key.kind = 144
			}
			if err := observeRootRefs([]treeItem{item}, 256); !errors.Is(err, errIncomplete) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
	if err := observeRootRefs(nil, 256); err != nil {
		t.Fatal(err)
	}
}

func TestRootReferencePayloadDecodesWithoutExportingGuestNames(t *testing.T) {
	data := make([]byte, 19)
	binary.LittleEndian.PutUint64(data, 256)
	binary.LittleEndian.PutUint64(data[8:], 9)
	binary.LittleEndian.PutUint16(data[16:], 1)
	data[18] = 'x'
	item := treeItem{key: treeKey{256, rootRefKey, 300}, data: data}
	if child, err := decodeRootRef(item, 256); err != nil || child != 300 {
		t.Fatal("valid ROOT_REF not decoded", child, err)
	}
	for _, name := range []string{"zero dirid", "zero sequence", "zero child", "slash", "nul", "dot"} {
		t.Run(name, func(t *testing.T) {
			bad := item
			bad.data = append([]byte{}, data...)
			switch name {
			case "zero dirid":
				binary.LittleEndian.PutUint64(bad.data, 0)
			case "zero sequence":
				binary.LittleEndian.PutUint64(bad.data[8:], 0)
			case "zero child":
				bad.key.offset = 0
			case "slash":
				bad.data[18] = '/'
			case "nul":
				bad.data[18] = 0
			case "dot":
				bad.data[18] = '.'
			}
			if _, err := decodeRootRef(bad, 256); !errors.Is(err, errIncomplete) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
}

func TestSearchOrderingUsesObjectTypeOffsetWithoutArithmeticOverflow(t *testing.T) {
	keys := []treeKey{{0, 240, math.MaxUint64}, {1, 1, 0}, {math.MaxUint64, 255, math.MaxUint64}}
	var buffer []byte
	for _, key := range keys {
		buffer = append(buffer, encodeItem(binary.BigEndian, key, nil)...)
	}
	if items, err := decodeSearch(binary.BigEndian, buffer, 3, treeRange{keys[0], keys[2]}); err != nil || len(items) != 3 {
		t.Fatal("valid lexicographic search refused", items, err)
	}
	reversed := append(encodeItem(binary.BigEndian, keys[1], nil), encodeItem(binary.BigEndian, keys[0], nil)...)
	if _, err := decodeSearch(binary.BigEndian, reversed, 2, treeRange{keys[0], keys[2]}); !errors.Is(err, errIncomplete) {
		t.Fatal("decreasing keys accepted", err)
	}
}

func TestPersistedStatusRecordsSavedProgressWithoutInventingLiveHealth(t *testing.T) {
	status, err := decodeStatus(quotaWords(1, 123, 1, 999, 456))
	if err != nil || status.generation != 123 || status.rescan != 999 || status.enableGeneration != 456 {
		t.Fatalf("saved fields incorrectly rejected: %+v %v", status, err)
	}
}

func TestQuotaPayloadSchemaLengthsAreExact(t *testing.T) {
	if _, err := decodeStatus(make([]byte, 32)); !errors.Is(err, errUnsupportedSchema) {
		t.Fatal("legacy schema misclassified", err)
	}
	for _, size := range []int{0, 31, 39, 41, 48} {
		if _, err := decodeStatus(make([]byte, size)); !errors.Is(err, errIncomplete) {
			t.Fatal("unknown status size accepted", size, err)
		}
		if _, err := decodeLimit(make([]byte, size), 1024); !errors.Is(err, errIncomplete) {
			t.Fatal("unknown limit size accepted", size, err)
		}
	}
}
