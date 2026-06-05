package datadir

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestInspectSyntheticPebble builds a minimal pebble db with the same key
// schema geth's rawdb uses, then verifies Inspect parses it.
func TestInspectSyntheticPebble(t *testing.T) {
	tmp := t.TempDir()
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}

	// Synthetic head: number 0x0123456789abcdef under hash 0x11..11.
	headHash := bytes32(t, "1111111111111111111111111111111111111111111111111111111111111111")
	headNum := uint64(0x0123456789abcdef)

	// Mainnet genesis hash (real value), so chain-id ↔ genesis path is exercised.
	genesis := bytes32(t, "d4e56740f876aef8c010b86a40d5f56745a118d0906a34e69aec8c0db1cb8fa3")

	mustSet(t, db, []byte("LastBlock"), headHash[:])
	numBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(numBytes, headNum)
	mustSet(t, db, append([]byte{'H'}, headHash[:]...), numBytes)
	mustSet(t, db, headerKey(headNum, headHash), syntheticBlockHeader(1700000000))
	mustSet(t, db, canonicalKey(0), genesis[:])
	mustSet(t, db, append([]byte("ethereum-config-"), genesis[:]...),
		[]byte(`{"chainId": 1, "homesteadBlock": 1150000}`))
	// PBSS marker: account trie root in path scheme (rawdb's
	// TrieNodeAccountPrefix + empty path = single byte "A").
	mustSet(t, db, []byte("A"), []byte{0x00})

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Inspect(tmp)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.HeadBlock != headNum {
		t.Errorf("HeadBlock = %d, want %d", got.HeadBlock, headNum)
	}
	if got.HeadHash != headHash {
		t.Errorf("HeadHash mismatch")
	}
	if got.GenesisHash != genesis {
		t.Errorf("GenesisHash mismatch")
	}
	if got.ChainID != 1 {
		t.Errorf("ChainID = %d, want 1", got.ChainID)
	}
	if got.StateScheme != "path" {
		t.Errorf("StateScheme = %q, want path", got.StateScheme)
	}
	if got.HeadTimestamp != 1700000000 {
		t.Errorf("HeadTimestamp = %d, want 1700000000", got.HeadTimestamp)
	}
}

// TestInspectHBSS confirms the state-scheme detector reports "hash" when
// triedb/ is absent.
func TestInspectHBSS(t *testing.T) {
	tmp := t.TempDir()
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatal(err)
	}

	hash := bytes32(t, "2222222222222222222222222222222222222222222222222222222222222222")
	genesis := bytes32(t, "25a5cc106eea7138acab33231d7160d69cb777ee0c2c553fcddf5138993e6dd9") // sepolia
	mustSet(t, db, []byte("LastBlock"), hash[:])
	numBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(numBytes, 100)
	mustSet(t, db, append([]byte{'H'}, hash[:]...), numBytes)
	mustSet(t, db, headerKey(100, hash), syntheticBlockHeader(1234567890))
	mustSet(t, db, canonicalKey(0), genesis[:])
	mustSet(t, db, append([]byte("ethereum-config-"), genesis[:]...),
		[]byte(`{"chainId": 11155111}`))

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Inspect(tmp)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.StateScheme != "hash" {
		t.Errorf("StateScheme = %q, want hash", got.StateScheme)
	}
	if got.ChainID != 11155111 {
		t.Errorf("ChainID = %d, want 11155111 (sepolia)", got.ChainID)
	}
}

func TestInspectMissingDatadir(t *testing.T) {
	if _, err := Inspect("/nonexistent/path/that/should/not/exist"); err == nil {
		t.Fatalf("expected error for missing datadir")
	}
}

func bytes32(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 32 {
		t.Fatalf("bytes32(%q) is %d bytes, want 32", s, len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func mustSet(t *testing.T, db *pebble.DB, k, v []byte) {
	t.Helper()
	if err := db.Set(k, v, pebble.Sync); err != nil {
		t.Fatal(err)
	}
}

// canonicalKey assembles geth's "h<num-be>n" canonical-hash key.
func canonicalKey(number uint64) []byte {
	out := make([]byte, 1+8+1)
	out[0] = 'h'
	binary.BigEndian.PutUint64(out[1:9], number)
	out[9] = 'n'
	return out
}

// headerKey assembles geth's "h<num-be><hash>" block-header key.
func headerKey(number uint64, hash [32]byte) []byte {
	out := make([]byte, 1+8+32)
	out[0] = 'h'
	binary.BigEndian.PutUint64(out[1:9], number)
	copy(out[9:], hash[:])
	return out
}

// syntheticBlockHeader builds a minimal RLP block header for tests. The
// 11 fields preceding Time are emitted as empty strings (one byte each,
// 0x80); only Time carries a value. This is structurally valid RLP and
// is sufficient for inspect.go's header reader, which skips fields by
// length-prefix and only decodes Time at index 11.
func syntheticBlockHeader(timestamp uint64) []byte {
	timeBytes := rlpEncodeUint(timestamp)
	bodyLen := 11 + len(timeBytes)
	if bodyLen > 55 {
		panic(fmt.Sprintf("synthetic header body too long: %d", bodyLen))
	}
	out := make([]byte, 0, 1+bodyLen)
	out = append(out, byte(0xc0+bodyLen))
	for i := 0; i < 11; i++ {
		out = append(out, 0x80)
	}
	return append(out, timeBytes...)
}

// rlpEncodeUint produces the RLP encoding of a non-negative integer
// as a length-prefixed big-endian byte string.
func rlpEncodeUint(v uint64) []byte {
	if v == 0 {
		return []byte{0x80}
	}
	if v <= 0x7f {
		return []byte{byte(v)}
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	start := 0
	for start < 8 && buf[start] == 0 {
		start++
	}
	body := buf[start:]
	return append([]byte{byte(0x80 + len(body))}, body...)
}
