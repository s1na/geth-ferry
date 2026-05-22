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
// geth-<chainid>-<role>-<block>.
//
// Earlier versions of ferry appended a -<unix-seconds> tail for
// collision avoidance; that is now provided by the upload's
// "snapshot already exists" preflight (see Options.Overwrite). The
// parser still accepts the legacy 5-part form so existing buckets stay
// readable — when the timestamp is present in the name, it's stored on
// Name.Timestamp; otherwise that field is zero and callers should fall
// back to the manifest's CreatedAt or the object's modification time.
type Name struct {
	ChainID   uint64
	Role      Role
	Block     uint64
	Timestamp int64 // Unix seconds (UTC); 0 for names without an embedded timestamp
}

func (n Name) String() string {
	if n.Timestamp != 0 {
		return fmt.Sprintf("geth-%d-%s-%d-%d",
			n.ChainID, n.Role, n.Block, n.Timestamp)
	}
	return fmt.Sprintf("geth-%d-%s-%d", n.ChainID, n.Role, n.Block)
}

// nameRegexp accepts the current 4-part form and the legacy 5-part form
// with an optional 9-12 digit timestamp tail (9 digits covers dates from
// 2001 onward; 12 covers through ~5138, so we never match arbitrary
// trailing integers as timestamps).
var nameRegexp = regexp.MustCompile(`^geth-(\d+)-(archive|full)-(\d+)(?:-(\d{9,12}))?$`)

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
	var ts int64
	if m[4] != "" {
		ts, err = strconv.ParseInt(m[4], 10, 64)
		if err != nil {
			return Name{}, fmt.Errorf("timestamp in %q: %w", s, err)
		}
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

// IsURL reports whether s starts with one of the schemes ferry understands
// as a remote reference: file://, s3://, http://, https://. Used by
// commands that accept either a local path or a URL.
func IsURL(s string) bool {
	for _, scheme := range []string{"file://", "s3://", "http://", "https://"} {
		if strings.HasPrefix(s, scheme) {
			return true
		}
	}
	return false
}

// SplitTrailingSegment splits a URL like
//
//	scheme://host/parent/name?query
//
// into the root URL ("scheme://host/parent?query") and the trailing name
// ("name"). Used for snapshot URLs where the last path component is either
// the snapshot directory or a legacy .tar.{zst,lz4} object.
func SplitTrailingSegment(s string) (root, name string, err error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", "", err
	}
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed == "" {
		return "", "", fmt.Errorf("URL %q has no path", s)
	}
	parent, leaf := path.Split(trimmed)
	if leaf == "" {
		return "", "", fmt.Errorf("URL %q has no trailing name", s)
	}
	u.Path = strings.TrimRight(parent, "/")
	return u.String(), leaf, nil
}
