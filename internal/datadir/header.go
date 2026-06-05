package datadir

import (
	"encoding/binary"
	"fmt"

	"github.com/cockroachdb/pebble"
)

// readHeaderTimestamp reads the geth block header stored under the
// canonical-by-hash key ("h" + uint64-be(number) + 32-byte hash) and
// returns its Time field.
//
// We intentionally avoid pulling in github.com/ethereum/go-ethereum's
// rlp package (it would drag in the entire go-ethereum module). The
// header RLP is a list whose 12th element (index 11) is Time as an
// unsigned integer. Earlier elements are length-prefixed and skippable
// without interpreting their contents.
func readHeaderTimestamp(db *pebble.DB, blockNum uint64, blockHash [32]byte) (uint64, error) {
	key := make([]byte, 1+8+32)
	key[0] = 'h'
	binary.BigEndian.PutUint64(key[1:9], blockNum)
	copy(key[9:], blockHash[:])

	headerBytes, closer, err := db.Get(key)
	if err != nil {
		return 0, fmt.Errorf("read header at block %d: %w", blockNum, err)
	}
	defer closer.Close()

	off, _, err := enterRLPList(headerBytes, 0)
	if err != nil {
		return 0, fmt.Errorf("header: %w", err)
	}
	for i := 0; i < 11; i++ {
		off, err = skipRLPItem(headerBytes, off)
		if err != nil {
			return 0, fmt.Errorf("header field %d: %w", i, err)
		}
	}
	t, _, err := readRLPUint(headerBytes, off)
	if err != nil {
		return 0, fmt.Errorf("header Time field: %w", err)
	}
	return t, nil
}

// enterRLPList consumes the list-header prefix at off and returns the
// offset of the first item inside the list, plus the total body length.
func enterRLPList(buf []byte, off int) (int, int, error) {
	if off >= len(buf) {
		return 0, 0, fmt.Errorf("RLP truncated at %d", off)
	}
	b := buf[off]
	switch {
	case b >= 0xc0 && b <= 0xf7:
		return off + 1, int(b - 0xc0), nil
	case b >= 0xf8:
		lenlen := int(b - 0xf7)
		if off+1+lenlen > len(buf) {
			return 0, 0, fmt.Errorf("RLP truncated list header at %d", off)
		}
		bodyLen, err := readBEUint(buf[off+1 : off+1+lenlen])
		if err != nil {
			return 0, 0, err
		}
		return off + 1 + lenlen, bodyLen, nil
	default:
		return 0, 0, fmt.Errorf("expected RLP list at %d, got 0x%02x", off, b)
	}
}

// skipRLPItem advances past one RLP-encoded item (string or list) and
// returns the offset of the next item.
func skipRLPItem(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return 0, fmt.Errorf("RLP truncated at %d", off)
	}
	b := buf[off]
	switch {
	case b <= 0x7f:
		return off + 1, nil
	case b <= 0xb7:
		return off + 1 + int(b-0x80), nil
	case b <= 0xbf:
		lenlen := int(b - 0xb7)
		if off+1+lenlen > len(buf) {
			return 0, fmt.Errorf("RLP truncated long-string header at %d", off)
		}
		bodyLen, err := readBEUint(buf[off+1 : off+1+lenlen])
		if err != nil {
			return 0, err
		}
		return off + 1 + lenlen + bodyLen, nil
	case b <= 0xf7:
		return off + 1 + int(b-0xc0), nil
	default:
		lenlen := int(b - 0xf7)
		if off+1+lenlen > len(buf) {
			return 0, fmt.Errorf("RLP truncated long-list header at %d", off)
		}
		bodyLen, err := readBEUint(buf[off+1 : off+1+lenlen])
		if err != nil {
			return 0, err
		}
		return off + 1 + lenlen + bodyLen, nil
	}
}

// readRLPUint decodes a non-negative integer encoded as an RLP string
// of big-endian bytes.
func readRLPUint(buf []byte, off int) (uint64, int, error) {
	if off >= len(buf) {
		return 0, 0, fmt.Errorf("RLP truncated at %d", off)
	}
	b := buf[off]
	switch {
	case b == 0x80: // empty string = zero
		return 0, off + 1, nil
	case b <= 0x7f:
		return uint64(b), off + 1, nil
	case b <= 0xb7:
		sLen := int(b - 0x80)
		if sLen > 8 {
			return 0, 0, fmt.Errorf("RLP uint string is %d bytes, want ≤ 8", sLen)
		}
		if off+1+sLen > len(buf) {
			return 0, 0, fmt.Errorf("RLP truncated uint string at %d", off)
		}
		v, err := readBEUint(buf[off+1 : off+1+sLen])
		if err != nil {
			return 0, 0, err
		}
		return uint64(v), off + 1 + sLen, nil
	default:
		return 0, 0, fmt.Errorf("expected RLP string at %d, got 0x%02x", off, b)
	}
}

// readBEUint reads a big-endian unsigned int from a byte slice.
// Caller is expected to keep the slice ≤ 8 bytes.
func readBEUint(b []byte) (int, error) {
	if len(b) > 8 {
		return 0, fmt.Errorf("BE int has %d bytes, want ≤ 8", len(b))
	}
	v := 0
	for _, x := range b {
		v = (v << 8) | int(x)
	}
	return v, nil
}
