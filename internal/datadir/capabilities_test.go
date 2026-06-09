package datadir

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestCapabilitiesBasic exercises the full capabilities derivation against
// a synthetic datadir: chain freezer headers.meta containing a pruned tail
// and a pebble db with TransactionIndexTail set.
func TestCapabilitiesBasic(t *testing.T) {
	tmp := t.TempDir()

	// Pebble db: TransactionIndexTail key only; the rest of the keys
	// readTxIndexTail doesn't touch are absent and that's fine.
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	var tailBytes [8]byte
	binary.BigEndian.PutUint64(tailBytes[:], 12000000)
	mustSet(t, db, []byte("TransactionIndexTail"), tailBytes[:])
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Chain freezer: headers.meta with tail = 15537393 (postmerge prune).
	chainFreezerDir := filepath.Join(chaindata, "ancient", "chain")
	if err := os.MkdirAll(chainFreezerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(chainFreezerDir, "bodies.meta"),
		syntheticFreezerMetaV2(15537393, 0),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	info := &Info{
		HeadBlock:   20000361,
		HeadHash:    [32]byte{0xab, 0xcd},
		StateScheme: "path",
	}
	caps, err := Capabilities(tmp, info, "full", nil)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if caps.Head.Number != "0x1312e69" { // 20_000_361
		t.Errorf("Head.Number = %q, want 0x1312e69", caps.Head.Number)
	}
	if caps.Head.Hash != "0xabcd000000000000000000000000000000000000000000000000000000000000" {
		t.Errorf("Head.Hash = %q", caps.Head.Hash)
	}

	if caps.Blocks == nil || caps.Blocks.OldestBlock != "0xed14f1" { // 15_537_393
		t.Errorf("Blocks.OldestBlock = %v, want 0xed14f1", caps.Blocks)
	}
	if caps.Receipts == nil || caps.Receipts.OldestBlock != "0xed14f1" {
		t.Errorf("Receipts.OldestBlock = %v, want 0xed14f1", caps.Receipts)
	}
	// Tx: max(cutoff=15_537_393, txTail=12_000_000) = 15_537_393.
	if caps.Tx == nil || caps.Tx.OldestBlock != "0xed14f1" {
		t.Errorf("Tx.OldestBlock = %v, want 0xed14f1 (clamped to cutoff)", caps.Tx)
	}

	// PBSS full: state = head + 1 - 128 = 20000234 = 0x1312dea.
	if caps.State == nil || caps.State.OldestBlock != "0x1312dea" {
		t.Errorf("State.OldestBlock = %v, want 0x1312dea", caps.State)
	}
	if caps.StateProofs == nil || caps.StateProofs.OldestBlock != "0x1312dea" {
		t.Errorf("StateProofs.OldestBlock = %v, want 0x1312dea", caps.StateProofs)
	}
}

// TestCapabilitiesHBSSArchive: hash-scheme + archive role → state covers
// from block 0 (every state materialized).
func TestCapabilitiesHBSSArchive(t *testing.T) {
	tmp := t.TempDir()
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	info := &Info{HeadBlock: 1000, StateScheme: "hash"}
	caps, err := Capabilities(tmp, info, "archive", nil)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.State == nil || caps.State.OldestBlock != "0x0" {
		t.Errorf("HBSS archive State.OldestBlock = %v, want 0x0", caps.State)
	}
	if caps.StateProofs == nil || caps.StateProofs.OldestBlock != "0x0" {
		t.Errorf("HBSS archive StateProofs.OldestBlock = %v, want 0x0", caps.StateProofs)
	}
}

// TestCapabilitiesPBSSArchiveOmits: path-scheme + archive role → state
// and stateproofs are nil pending follow-up (we don't yet parse the
// acceleration index blob).
func TestCapabilitiesPBSSArchiveOmits(t *testing.T) {
	tmp := t.TempDir()
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	info := &Info{HeadBlock: 1000, StateScheme: "path"}
	caps, err := Capabilities(tmp, info, "archive", nil)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.State != nil || caps.StateProofs != nil {
		t.Errorf("PBSS archive should omit state/stateproofs until index parsing lands; got state=%v proofs=%v",
			caps.State, caps.StateProofs)
	}
}

// TestCapabilitiesUnpruned: no headers.meta, no TxIndexTail. All resources
// report oldest = 0.
func TestCapabilitiesUnpruned(t *testing.T) {
	tmp := t.TempDir()
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	info := &Info{HeadBlock: 100, HeadHash: [32]byte{0x11}}
	caps, err := Capabilities(tmp, info, "full", nil)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Blocks.OldestBlock != "0x0" {
		t.Errorf("Blocks.OldestBlock = %q, want 0x0", caps.Blocks.OldestBlock)
	}
	if caps.Tx.OldestBlock != "0x0" {
		t.Errorf("Tx.OldestBlock = %q, want 0x0", caps.Tx.OldestBlock)
	}
}

// TestCapabilitiesTxTailDominates: tx tail is higher than the chain
// cutoff, so it determines tx's oldest.
func TestCapabilitiesTxTailDominates(t *testing.T) {
	tmp := t.TempDir()
	chaindata := filepath.Join(tmp, "geth", "chaindata")
	if err := os.MkdirAll(chaindata, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(chaindata, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tailBytes [8]byte
	binary.BigEndian.PutUint64(tailBytes[:], 18000000)
	mustSet(t, db, []byte("TransactionIndexTail"), tailBytes[:])
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	chainFreezerDir := filepath.Join(chaindata, "ancient", "chain")
	if err := os.MkdirAll(chainFreezerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(chainFreezerDir, "bodies.meta"),
		syntheticFreezerMetaV2(15537393, 0),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	info := &Info{HeadBlock: 20000000, HeadHash: [32]byte{}}
	caps, err := Capabilities(tmp, info, "full", nil)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Tx.OldestBlock != "0x112a880" { // 18_000_000
		t.Errorf("Tx.OldestBlock = %q, want 0x112a880", caps.Tx.OldestBlock)
	}
	if caps.Blocks.OldestBlock != "0xed14f1" { // 15_537_393
		t.Errorf("Blocks.OldestBlock = %q, want 0xed14f1", caps.Blocks.OldestBlock)
	}
}

// syntheticFreezerMetaV2 produces the RLP encoding of geth's freezer
// metadata V2 struct: {Version=2, Tail, Offset}. Body always fits in
// the short-list space for our test inputs.
func syntheticFreezerMetaV2(tail, offset uint64) []byte {
	vBytes := rlpEncodeUint(2)
	tBytes := rlpEncodeUint(tail)
	oBytes := rlpEncodeUint(offset)
	bodyLen := len(vBytes) + len(tBytes) + len(oBytes)
	out := make([]byte, 0, 1+bodyLen)
	out = append(out, byte(0xc0+bodyLen))
	out = append(out, vBytes...)
	out = append(out, tBytes...)
	return append(out, oBytes...)
}
