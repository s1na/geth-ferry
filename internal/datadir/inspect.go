// Package datadir reads metadata from a stopped geth datadir without
// invoking geth. ferry uses it to populate the snapshot manifest's
// chain-id, head-block, and state-scheme fields automatically, turning
// `--name` / `--block` / `--chain-id` from required flags into optional
// overrides.
//
// Implementation: open the pebble database read-only, read a few well-known
// keys from go-ethereum's rawdb schema:
//
//	"LastBlock"                            32-byte head block hash
//	"H" + hash                             8-byte big-endian head block number
//	"ethereum-config-" + genesisHash       JSON-encoded ChainConfig
//	"A"                                    account trie root, path scheme (PBSS marker)
//	"LastStateID"                          persistent state id (PBSS marker)
//
// Geth must be stopped (pebble's flock is exclusive even for a read-only
// open). Pebble's on-disk format is stable across recent versions, so the
// version we link against doesn't have to match geth's exactly.
package datadir

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cockroachdb/pebble"
)

// Info summarizes the metadata ferry needs from a datadir.
type Info struct {
	HeadBlock     uint64
	HeadHash      [32]byte
	HeadTimestamp uint64 // unix seconds from the head block header
	GenesisHash   [32]byte
	ChainID       uint64
	StateScheme   string // "path" (PBSS) or "hash" (HBSS)
}

// Inspect reads <datadir>/geth/chaindata and returns its head/chain metadata.
// Geth must not be running on this datadir.
func Inspect(datadir string) (*Info, error) {
	chaindataDir := filepath.Join(datadir, "geth", "chaindata")
	db, err := pebble.Open(chaindataDir, &pebble.Options{
		ReadOnly: true,
		Logger:   discardLogger{},
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", chaindataDir, err)
	}
	defer db.Close()

	info := &Info{}

	headHash, err := readHash(db, []byte("LastBlock"))
	if err != nil {
		return nil, fmt.Errorf("read LastBlock: %w", err)
	}
	info.HeadHash = headHash

	numKey := append([]byte{'H'}, headHash[:]...)
	nb, closer, err := db.Get(numKey)
	if err != nil {
		return nil, fmt.Errorf("read head block number: %w", err)
	}
	if len(nb) != 8 {
		closer.Close()
		return nil, fmt.Errorf("head block number value is %d bytes, want 8", len(nb))
	}
	info.HeadBlock = binary.BigEndian.Uint64(nb)
	closer.Close()

	headTimestamp, err := readHeaderTimestamp(db, info.HeadBlock, info.HeadHash)
	if err != nil {
		return nil, fmt.Errorf("read head timestamp: %w", err)
	}
	info.HeadTimestamp = headTimestamp

	genesisHash, chainID, err := readChainConfig(db)
	if err != nil {
		return nil, fmt.Errorf("read chain config: %w", err)
	}
	info.GenesisHash = genesisHash
	info.ChainID = chainID

	scheme, err := readStateScheme(db)
	if err != nil {
		return nil, fmt.Errorf("read state scheme: %w", err)
	}
	info.StateScheme = scheme

	return info, nil
}

// readStateScheme determines whether the chaindata holds PBSS (path) or
// HBSS (hash) state. Mirrors go-ethereum's rawdb.ReadStateScheme:
//
//  1. Probe for the account trie root in path scheme: key "A" (single
//     byte; rawdb.TrieNodeAccountPrefix with empty path). Present on
//     any populated PBSS chain.
//  2. Probe for "LastStateID": persistent state id, set by pathdb even
//     when the root was wiped during snap-sync.
//  3. Fall back to "hash". (A pre-v0.2.1 ferry stat'd <datadir>/geth/triedb/
//     which only exists after a graceful PBSS shutdown writes
//     merkle.journal, false-negatives mis-tagged actual PBSS nodes as
//     HBSS, exactly the bug this replaces.)
//
// This is purely descriptive: ferry doesn't change behavior based on
// the value, it just records it in the manifest so downloaders can
// sanity-check before pulling a multi-TB snapshot.
func readStateScheme(db *pebble.DB) (string, error) {
	if has, err := keyExists(db, []byte("A")); err != nil {
		return "", err
	} else if has {
		return "path", nil
	}
	if has, err := keyExists(db, []byte("LastStateID")); err != nil {
		return "", err
	} else if has {
		return "path", nil
	}
	return "hash", nil
}

func keyExists(db *pebble.DB, key []byte) (bool, error) {
	_, closer, err := db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return true, nil
}

func readHash(db *pebble.DB, key []byte) ([32]byte, error) {
	var h [32]byte
	v, closer, err := db.Get(key)
	if err != nil {
		return h, err
	}
	defer closer.Close()
	if len(v) != 32 {
		return h, fmt.Errorf("value is %d bytes, want 32", len(v))
	}
	copy(h[:], v)
	return h, nil
}

func readChainConfig(db *pebble.DB) ([32]byte, uint64, error) {
	var genesis [32]byte

	// Genesis hash = canonical hash of block 0. Read it directly via the
	// canonical-chain key rather than iterating the chain-config namespace;
	// this asserts we pick up the chain we've actually been syncing rather
	// than whichever ethereum-config-* key happens to sort first.
	//
	// Key shape: "h" + uint64-be(number) + "n"  →  32-byte canonical hash.
	canonicalKey := append(
		append([]byte{'h'}, make([]byte, 8)...), // 8 zero bytes encode block 0
		'n',
	)
	gh, err := readHash(db, canonicalKey)
	if err != nil {
		return genesis, 0, fmt.Errorf("read canonical hash of block 0: %w", err)
	}
	genesis = gh

	// Now look up the chain config keyed by that exact genesis hash.
	cfgKey := append([]byte("ethereum-config-"), genesis[:]...)
	cfgBytes, closer, err := db.Get(cfgKey)
	if err != nil {
		return genesis, 0, fmt.Errorf("read chain config for genesis 0x%x: %w", genesis, err)
	}
	defer closer.Close()

	// We only need chainId. Geth's ChainConfig has many fields and they
	// shift across releases; decoding into a tiny shim avoids brittleness.
	var cfg struct {
		ChainID *uint64 `json:"chainId"`
	}
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return genesis, 0, fmt.Errorf("decode chain config json: %w", err)
	}
	if cfg.ChainID == nil {
		return genesis, 0, fmt.Errorf("chain config has no chainId")
	}
	return genesis, *cfg.ChainID, nil
}

// discardLogger silences pebble's WAL-replay messages and the like.
// We hold a read-only handle for ~ms; the operator doesn't care.
type discardLogger struct{}

func (discardLogger) Infof(string, ...interface{})  {}
func (discardLogger) Errorf(string, ...interface{}) {}
func (discardLogger) Fatalf(format string, args ...interface{}) {
	// Pebble only calls Fatalf for unrecoverable bugs; surface as a panic
	// rather than silently dropping it.
	panic(fmt.Sprintf(format, args...))
}
