package datadir

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cockroachdb/pebble"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// Capabilities returns the eth_capabilities-shaped availability summary
// for the datadir at path, derived from the head described in info.
//
// Currently populated: Head, Blocks, Receipts, Tx. State and StateProofs
// are intentionally left nil; their derivation requires mapping the state
// freezer's state-id-indexed tail back to a block number, which is its
// own follow-up (the spec accepts these fields being absent).
//
// info must be the result of a successful Inspect(path) call on the same
// datadir; the head block / hash are taken from there.
func Capabilities(datadir string, info *Info) (*snapshot.Capabilities, error) {
	if info == nil {
		return nil, errors.New("Capabilities: info is nil")
	}

	chainTail, err := chainFreezerTail(datadir)
	if err != nil {
		return nil, fmt.Errorf("chain freezer tail: %w", err)
	}

	txTail, err := readTxIndexTail(datadir)
	if err != nil {
		return nil, fmt.Errorf("tx index tail: %w", err)
	}

	// chain freezer tail is the cutoff for blocks/receipts. For tx,
	// the index can only go back as far as its own tail and never
	// beyond the chain cutoff; max(cutoff, txTail) gives the effective
	// oldest indexed block.
	blocksOldest := chainTail
	receiptsOldest := chainTail
	txOldest := chainTail
	if txTail > txOldest {
		txOldest = txTail
	}

	return &snapshot.Capabilities{
		Head: snapshot.CapabilityHead{
			Number: hexUint64(info.HeadBlock),
			Hash:   "0x" + hex.EncodeToString(info.HeadHash[:]),
		},
		Blocks:   &snapshot.CapabilityResource{OldestBlock: hexUint64(blocksOldest)},
		Receipts: &snapshot.CapabilityResource{OldestBlock: hexUint64(receiptsOldest)},
		Tx:       &snapshot.CapabilityResource{OldestBlock: hexUint64(txOldest)},
		// State, StateProofs: omitted until the state freezer derivation lands.
	}, nil
}

// readTxIndexTail opens the chaindata pebble read-only and returns the
// value at "TransactionIndexTail" as a uint64 block number. Returns 0
// when the key is absent (a node that indexes the full chain doesn't
// write this key).
func readTxIndexTail(datadir string) (uint64, error) {
	chaindataDir := filepath.Join(datadir, "geth", "chaindata")
	db, err := pebble.Open(chaindataDir, &pebble.Options{
		ReadOnly: true,
		Logger:   discardLogger{},
	})
	if err != nil {
		return 0, fmt.Errorf("open chaindata: %w", err)
	}
	defer db.Close()

	v, closer, err := db.Get([]byte("TransactionIndexTail"))
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get TransactionIndexTail: %w", err)
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0, fmt.Errorf("TransactionIndexTail value is %d bytes, want 8", len(v))
	}
	return binary.BigEndian.Uint64(v), nil
}

func hexUint64(v uint64) string {
	return fmt.Sprintf("0x%x", v)
}
