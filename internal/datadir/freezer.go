package datadir

import (
	"errors"
	"fmt"
	"os"
)

// chainFreezerTail returns the lowest block number still present in the
// chain freezer. After geth's prune-history runs, this advances past
// the genesis; on an unpruned datadir it's 0.
//
// We read it from <datadir>/geth/chaindata/ancient/chain/headers.meta:
// headers is the always-present table in the chain freezer (bodies and
// receipts can be pruned, headers can't), and all tables in a freezer
// group share the same virtualTail after a prune.
func chainFreezerTail(datadir string) (uint64, error) {
	metaPath := datadir + "/geth/chaindata/ancient/chain/headers.meta"
	return readFreezerMetaTail(metaPath)
}

// readFreezerMetaTail parses a freezer table's .meta file (RLP-encoded
// metadata struct) and returns the virtualTail field. Supports both V1
// ([version, tail]) and V2 ([version, tail, offset]) layouts; the field
// at index 1 of the outer list is Tail in both versions.
func readFreezerMetaTail(path string) (uint64, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No .meta file means the freezer table has never been written;
			// treat as "empty freezer, tail at 0".
			return 0, nil
		}
		return 0, fmt.Errorf("read freezer meta %s: %w", path, err)
	}
	off, _, err := enterRLPList(buf, 0)
	if err != nil {
		return 0, fmt.Errorf("freezer meta %s: %w", path, err)
	}
	off, err = skipRLPItem(buf, off) // skip Version
	if err != nil {
		return 0, fmt.Errorf("freezer meta %s: skip version: %w", path, err)
	}
	tail, _, err := readRLPUint(buf, off)
	if err != nil {
		return 0, fmt.Errorf("freezer meta %s: read tail: %w", path, err)
	}
	return tail, nil
}
