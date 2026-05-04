package datadir

import (
	"encoding/binary"
	"encoding/hex"
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
	mustSet(t, db, canonicalKey(0), genesis[:])
	mustSet(t, db, append([]byte("ethereum-config-"), genesis[:]...),
		[]byte(`{"chainId": 1, "homesteadBlock": 1150000}`))

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Add a triedb/ to exercise the PBSS branch.
	if err := os.MkdirAll(filepath.Join(tmp, "geth", "triedb"), 0o755); err != nil {
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
