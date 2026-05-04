// Package datadir reads metadata from a stopped geth datadir without
// invoking geth. ferry uses it to populate the snapshot manifest's
// chain-id, head-block, and state-scheme fields automatically — turning
// `--name` / `--block` / `--chain-id` from required flags into optional
// overrides.
//
// Implementation: open the pebble database read-only, read a few well-known
// keys from go-ethereum's rawdb schema:
//
//	"LastBlock"                            32-byte head block hash
//	"H" + hash                             8-byte big-endian head block number
//	"ethereum-config-" + genesisHash       JSON-encoded ChainConfig
//
// Geth must be stopped (pebble's flock is exclusive even for a read-only
// open). Pebble's on-disk format is stable across recent versions, so the
// version we link against doesn't have to match geth's exactly.
package datadir

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble"
)

// Info summarizes the metadata ferry needs from a datadir.
type Info struct {
	HeadBlock   uint64
	HeadHash    [32]byte
	GenesisHash [32]byte
	ChainID     uint64
	StateScheme string // "path" (PBSS) or "hash" (HBSS)
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

	genesisHash, chainID, err := readChainConfig(db)
	if err != nil {
		return nil, fmt.Errorf("read chain config: %w", err)
	}
	info.GenesisHash = genesisHash
	info.ChainID = chainID

	if _, err := os.Stat(filepath.Join(datadir, "geth", "triedb")); err == nil {
		info.StateScheme = "path"
	} else if errors.Is(err, os.ErrNotExist) {
		info.StateScheme = "hash"
	} else {
		return nil, fmt.Errorf("stat triedb: %w", err)
	}

	return info, nil
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
	prefix := []byte("ethereum-config-")

	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: keyUpperBound(prefix),
	})
	if err != nil {
		return genesis, 0, err
	}
	defer iter.Close()

	if !iter.First() {
		return genesis, 0, fmt.Errorf("no chain config found (no %q keys)", prefix)
	}
	key := iter.Key()
	if !bytes.HasPrefix(key, prefix) || len(key) != len(prefix)+32 {
		return genesis, 0, fmt.Errorf("malformed chain config key %q", key)
	}
	copy(genesis[:], key[len(prefix):])

	// We only need chainId. Geth's ChainConfig has many fields and they
	// shift across releases — decoding into a tiny shim avoids brittleness.
	var cfg struct {
		ChainID *uint64 `json:"chainId"`
	}
	if err := json.Unmarshal(iter.Value(), &cfg); err != nil {
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

// keyUpperBound returns the smallest byte slice that's strictly greater
// than every key with prefix b. Returns nil if b is all 0xff (which means
// "no upper bound" — pebble accepts a nil UpperBound for that case).
func keyUpperBound(b []byte) []byte {
	end := make([]byte, len(b))
	copy(end, b)
	for i := len(end) - 1; i >= 0; i-- {
		end[i]++
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}
