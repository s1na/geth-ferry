package snapshot

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// PartsDir is the prefix under a snapshot directory where part files live.
	PartsDir = "parts"

	// ChaindataPart is the path of the chaindata part within a snapshot.
	ChaindataPart = "parts/chaindata.tar.zst"

	// TriedbPart is the path of the triedb part within a snapshot. Optional.
	TriedbPart = "parts/triedb.tar.zst"
)

// Name describes a snapshot's identifier: geth-<chainid>-<role>-<block>-<YYYYMMDD>.
type Name struct {
	ChainID uint64
	Role    Role
	Block   uint64
	Date    time.Time // UTC, day precision
}

func (n Name) String() string {
	return fmt.Sprintf("geth-%d-%s-%d-%s",
		n.ChainID, n.Role, n.Block, n.Date.UTC().Format("20060102"))
}

var nameRegexp = regexp.MustCompile(`^geth-(\d+)-(archive|full)-(\d+)-(\d{8})$`)

// ParseName parses a snapshot name into its components. Returns an error if
// the input doesn't match the expected shape.
func ParseName(s string) (Name, error) {
	m := nameRegexp.FindStringSubmatch(s)
	if m == nil {
		return Name{}, fmt.Errorf("invalid snapshot name %q", s)
	}
	chainID, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return Name{}, fmt.Errorf("chain id in %q: %w", s, err)
	}
	block, err := strconv.ParseUint(m[3], 10, 64)
	if err != nil {
		return Name{}, fmt.Errorf("block in %q: %w", s, err)
	}
	date, err := time.Parse("20060102", m[4])
	if err != nil {
		return Name{}, fmt.Errorf("date in %q: %w", s, err)
	}
	return Name{
		ChainID: chainID,
		Role:    Role(m[2]),
		Block:   block,
		Date:    date,
	}, nil
}

// Key joins the snapshot name with a child path, normalizing separators.
func Key(name, child string) string {
	return path.Join(name, child)
}

// IsLegacyURL reports whether src points at a single-file legacy snapshot
// (suffix .tar.lz4 or .tar.zst), as opposed to a snapshot directory.
func IsLegacyURL(src string) bool {
	s := strings.ToLower(src)
	return strings.HasSuffix(s, ".tar.lz4") || strings.HasSuffix(s, ".tar.zst")
}
