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

	// ChaindataLivePart holds the live pebble database (every file under
	// <datadir>/geth/chaindata/ except the ancient/ subtree).
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

// Name describes a snapshot's canonical identifier:
// geth-<chainid>-<role>-<block>. This is the shape ferry generates when
// --name isn't supplied.
//
// Names are no longer required to match this shape: operators can pass
// any path-safe string via --name. ParseName is kept as a utility for
// reading the canonical shape (e.g. when displaying info derived from
// the name alone), but ferry's source of truth for chain/role/block/
// created_at is the manifest.json; list fetches it per snapshot.
type Name struct {
	ChainID uint64
	Role    Role
	Block   uint64
}

func (n Name) String() string {
	return fmt.Sprintf("geth-%d-%s-%d", n.ChainID, n.Role, n.Block)
}

// nameRegexp matches the canonical 4-part name shape. Older ferry
// (≤ v0.1.0) appended a -<unix-seconds> tail; v0.2.0+ doesn't generate
// that form and no longer parses it either. Snapshots still carrying
// the legacy tail in their on-bucket name are reachable via the URL
// just fine (download/inspect/verify treat names as opaque), but
// ParseName will return an error on them; fetch the manifest instead.
var nameRegexp = regexp.MustCompile(`^geth-(\d+)-(archive|full)-(\d+)$`)

// ParseName parses a snapshot name into its components. Returns an error
// when the input doesn't match the canonical shape. Use this only when
// you need the structured info from the name itself; for authoritative
// chain/role/block/created_at, fetch the manifest.
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
	return Name{
		ChainID: chainID,
		Role:    Role(m[2]),
		Block:   block,
	}, nil
}

// nameSafetyRegexp is the conservative charset ferry insists on for
// --name. Allows letters, digits, dashes, dots, underscores. Forbids
// slashes (would create unintended sub-prefixes), query/fragment
// separators (URL footguns), and whitespace.
var nameSafetyRegexp = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateNamePathSafety enforces the minimum constraints ferry needs to
// use a snapshot name as a path segment: non-empty, no slashes, no URL
// metacharacters, no whitespace. Stricter validation (the canonical
// geth-chain-role-block shape) is no longer enforced: operators are
// free to pick whatever fits their pipeline.
func ValidateNamePathSafety(name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("snapshot name %q is reserved", name)
	}
	if !nameSafetyRegexp.MatchString(name) {
		return fmt.Errorf("snapshot name %q has invalid characters; allowed: letters, digits, '-', '.', '_'", name)
	}
	return nil
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
