package datadir

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/cockroachdb/pebble"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// triesInMemory mirrors core/state.TriesInMemory from go-ethereum:
// geth's diff layer chain is 128 deep, which is the window
// eth_capabilities reports for any non-archive node. We hard-code it
// because pulling in go-ethereum just for the constant is overkill.
const triesInMemory uint64 = 128

// Capabilities returns the eth_capabilities-shaped availability summary
// for the datadir at path, derived from the head described in info and
// the operator-declared role.
//
// State and StateProofs follow geth's eth_capabilities semantics:
//
//   - Any full node (PBSS or HBSS): the 128-block in-memory diff layer
//     window. The 90k state-freezer reverse-diffs that PBSS keeps for
//     reorg recovery are NOT user-serveable; they become queryable only
//     when an archive-mode acceleration index is built on top.
//   - HBSS archive: every block's state is materialized, oldest = 0.
//   - PBSS archive: the acceleration index's coverage; left omitted
//     for now (parsing the LastStateHistoryIndex blob is a follow-up).
//
// info must be the result of a successful Inspect(path) call on the
// same datadir.
func Capabilities(datadir string, info *Info, role snapshot.Role, warn io.Writer) (*snapshot.Capabilities, error) {
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

	caps := &snapshot.Capabilities{
		Head: snapshot.CapabilityHead{
			Number: hexUint64(info.HeadBlock),
			Hash:   "0x" + hex.EncodeToString(info.HeadHash[:]),
		},
		Blocks:   &snapshot.CapabilityResource{OldestBlock: hexUint64(blocksOldest)},
		Receipts: &snapshot.CapabilityResource{OldestBlock: hexUint64(receiptsOldest)},
		Tx:       &snapshot.CapabilityResource{OldestBlock: hexUint64(txOldest)},
	}

	state, stateProofs := deriveStateRange(datadir, info, role, warn)
	caps.State = state
	caps.StateProofs = stateProofs

	return caps, nil
}

// deriveStateRange returns the state and stateproofs resources, applying
// geth's eth_capabilities role/scheme matrix. On PBSS we also run a
// sanity check against the on-disk LastStateID and log via warn if it
// looks inconsistent (the field is still emitted; geth's TriesInMemory
// is the contract, the validation just surfaces drift).
//
// PBSS archive is intentionally left nil for now: the acceleration index
// (LastStateHistoryIndex) carries the actual range and parsing its blob
// is a separate follow-up. A nil state field on an archive PBSS snapshot
// reads as "unknown" by spec.
func deriveStateRange(datadir string, info *Info, role snapshot.Role, warn io.Writer) (*snapshot.CapabilityResource, *snapshot.CapabilityResource) {
	isArchive := role == snapshot.RoleArchive
	isPBSS := info.StateScheme == "path"

	// HBSS archive: every block's state is materialized.
	if isArchive && !isPBSS {
		zero := &snapshot.CapabilityResource{OldestBlock: hexUint64(0)}
		return zero, zero
	}

	// PBSS archive: the acceleration index's range determines what's
	// serveable. Leaving nil until the LastStateHistoryIndex parser lands.
	if isArchive && isPBSS {
		if warn != nil {
			fmt.Fprintln(warn, "ferry: PBSS archive state.oldestBlock is not yet derived; omitting state/stateproofs from capabilities")
		}
		return nil, nil
	}

	// Full mode (PBSS or HBSS): geth's contract is the TriesInMemory window.
	var oldest uint64
	if info.HeadBlock+1 > triesInMemory {
		oldest = info.HeadBlock + 1 - triesInMemory
	}

	// On PBSS, sanity-check that the disk layer is reasonably close to head.
	// A huge gap would indicate a node that crashed mid-flush or otherwise
	// can't actually honor the 128-block window. We don't refuse to emit;
	// the contract is geth's responsibility.
	if isPBSS {
		if id, ok := readUint64Key(datadir, []byte("LastStateID")); ok && warn != nil {
			gap := info.HeadBlock - id
			const reasonableGap = 10_000
			if id > info.HeadBlock {
				fmt.Fprintf(warn, "ferry: LastStateID (%d) > head (%d); recording state.oldestBlock=%d anyway\n", id, info.HeadBlock, oldest)
			} else if gap > reasonableGap {
				fmt.Fprintf(warn, "ferry: disk-layer is %d blocks behind head (LastStateID=%d, head=%d); 128-block in-memory window may not be fully restorable\n", gap, id, info.HeadBlock)
			}
		}
	} else {
		// HBSS: presence of SnapshotRoot is the equivalent sanity check.
		if !hasKey(datadir, []byte("SnapshotRoot")) && warn != nil {
			fmt.Fprintf(warn, "ferry: HBSS datadir has no SnapshotRoot key; eth_capabilities-style state window may not be honored\n")
		}
	}

	res := &snapshot.CapabilityResource{OldestBlock: hexUint64(oldest)}
	return res, res
}

// readUint64Key fetches an 8-byte big-endian key from the chaindata
// pebble. ok=false means "absent or unreadable"; we don't bubble the
// error because callers treat it as best-effort.
func readUint64Key(datadir string, key []byte) (uint64, bool) {
	chaindataDir := filepath.Join(datadir, "geth", "chaindata")
	db, err := pebble.Open(chaindataDir, &pebble.Options{ReadOnly: true, Logger: discardLogger{}})
	if err != nil {
		return 0, false
	}
	defer db.Close()
	v, closer, err := db.Get(key)
	if err != nil {
		return 0, false
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(v), true
}

func hasKey(datadir string, key []byte) bool {
	chaindataDir := filepath.Join(datadir, "geth", "chaindata")
	db, err := pebble.Open(chaindataDir, &pebble.Options{ReadOnly: true, Logger: discardLogger{}})
	if err != nil {
		return false
	}
	defer db.Close()
	_, closer, err := db.Get(key)
	if err != nil {
		return false
	}
	closer.Close()
	return true
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
