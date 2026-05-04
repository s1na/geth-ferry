package snapshot

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	// PartsDir is the prefix under a snapshot directory where part files live.
	PartsDir = "parts"

	// ChaindataLivePart holds the live pebble database — every file under
	// <datadir>/geth/chaindata/ except the ancient/ subtree.
	ChaindataLivePart = "parts/chaindata-live.tar.zst"

	// AncientChainPart holds <datadir>/geth/chaindata/ancient/chain/.
	// Always present (the chain freezer is required by every node mode).
	AncientChainPart = "parts/ancient-chain.tar.zst"

	// AncientStatePart holds <datadir>/geth/chaindata/ancient/state/.
	// Optional: PBSS nodes have it (full or archive); HBSS nodes don't.
	AncientStatePart = "parts/ancient-state.tar.zst"

	// TriedbPart is the path of the triedb part within a snapshot.
	// Optional: present only on PBSS nodes (carries merkle.journal).
	TriedbPart = "parts/triedb.tar.zst"
)

// Name describes a snapshot's identifier:
// geth-<chainid>-<role>-<block>-<unix-seconds>.
//
// The trailing component is a Unix timestamp (seconds since epoch, UTC) —
// matches `manifest.created_at`'s format and is unambiguously orderable
// without locale/timezone games.
type Name struct {
	ChainID   uint64
	Role      Role
	Block     uint64
	Timestamp int64 // Unix seconds (UTC)
}

func (n Name) String() string {
	return fmt.Sprintf("geth-%d-%s-%d-%d",
		n.ChainID, n.Role, n.Block, n.Timestamp)
}

// nameRegexp accepts a 9- to 12-digit timestamp tail. 9 digits covers
// dates up to 2001; 12 covers through ~5138. Plenty of room without
// matching arbitrary integers.
var nameRegexp = regexp.MustCompile(`^geth-(\d+)-(archive|full)-(\d+)-(\d{9,12})$`)

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
	ts, err := strconv.ParseInt(m[4], 10, 64)
	if err != nil {
		return Name{}, fmt.Errorf("timestamp in %q: %w", s, err)
	}
	return Name{
		ChainID:   chainID,
		Role:      Role(m[2]),
		Block:     block,
		Timestamp: ts,
	}, nil
}

// Key joins the snapshot name with a child path, normalizing separators.
func Key(name, child string) string {
	return path.Join(name, child)
}

// IsLegacyURL reports whether src points at a single-file legacy snapshot
// (suffix .tar.lz4 or .tar.zst), as opposed to a snapshot directory. The
// check looks at the URL path component only, so query strings like
// `?endpoint=...&region=...` don't defeat detection.
func IsLegacyURL(src string) bool {
	pathPart := src
	if u, err := url.Parse(src); err == nil && u.Path != "" {
		pathPart = u.Path
	}
	s := strings.ToLower(pathPart)
	return strings.HasSuffix(s, ".tar.lz4") || strings.HasSuffix(s, ".tar.zst")
}
