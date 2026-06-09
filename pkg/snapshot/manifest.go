package snapshot

import (
	"encoding/json"
	"fmt"
	"io"
)

const ManifestVersion = 1

const ManifestFilename = "manifest.json"

type Role string

const (
	RoleArchive Role = "archive"
	RoleFull    Role = "full"
)

func (r Role) Valid() bool {
	return r == RoleArchive || r == RoleFull
}

type StateScheme string

const (
	StateSchemePath StateScheme = "path"
	StateSchemeHash StateScheme = "hash"
)

type Codec string

const (
	CodecZstd Codec = "zstd"
)

type Manifest struct {
	Version      int           `json:"version"`
	Name         string        `json:"name"`
	ChainID      uint64        `json:"chain_id"`
	Role         Role          `json:"role"`
	StateScheme  StateScheme   `json:"state_scheme"`
	Head         Head          `json:"head"`
	CreatedAt    int64         `json:"created_at"` // Unix seconds (UTC)
	CreatedBy    string        `json:"created_by"`
	Codec        Codec         `json:"codec"`
	Level        int           `json:"level"`
	Parts        []Part        `json:"parts"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities mirrors the eth_capabilities JSON-RPC response defined in
// https://github.com/ethereum/execution-apis/pull/755. Each resource that
// ferry can determine for the produced snapshot is populated; unpopulated
// resources are omitted entirely (a reader should treat absence as "we
// don't know"). The Logs resource is intentionally not exposed: log-index
// state is observable per-snapshot but tracking its sliding-window
// rendering progress in a way that survives reload is more work than the
// downstream consumer needs right now.
//
// All block numbers are encoded as "0x..." hex strings to match the
// JSON-RPC wire format so a consumer's existing eth_capabilities parser
// reads ferry manifests without translation.
type Capabilities struct {
	Head CapabilityHead `json:"head"`

	// Resource fields, all pointers so unpopulated → JSON omits them.
	Blocks      *CapabilityResource `json:"blocks,omitempty"`
	Receipts    *CapabilityResource `json:"receipts,omitempty"`
	Tx          *CapabilityResource `json:"tx,omitempty"`
	State       *CapabilityResource `json:"state,omitempty"`       // TODO: derive from state freezer
	StateProofs *CapabilityResource `json:"stateproofs,omitempty"` // TODO: derive (same matrix as State)
}

// CapabilityHead is the canonical head this snapshot represents, in the
// same shape eth_capabilities reports it.
type CapabilityHead struct {
	Number string `json:"number"` // "0x..." hex
	Hash   string `json:"hash"`   // "0x..." hex (32 bytes)
}

// CapabilityResource describes one resource's availability. A disabled
// resource omits OldestBlock so it cannot be mistaken for a usable range.
// DeleteStrategy is intentionally not modelled in ferry: it advertises a
// retention policy the producing node was running, which is forward-
// looking intent rather than the static fact captured by the snapshot.
type CapabilityResource struct {
	Disabled    bool   `json:"disabled,omitempty"`
	OldestBlock string `json:"oldestBlock,omitempty"` // "0x..." hex
}

type Head struct {
	Block     uint64 `json:"block"`
	Hash      string `json:"hash,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

type PartKind string

const (
	PartChaindataLive PartKind = "chaindata-live"
	PartAncientChain  PartKind = "ancient-chain"
	PartAncientState  PartKind = "ancient-state"
	PartTriedb        PartKind = "triedb"
)

type Part struct {
	Name             string   `json:"name"`
	Kind             PartKind `json:"kind"`
	UncompressedSize int64    `json:"uncompressed_size"`
	CompressedSize   int64    `json:"compressed_size"`
	SHA256           string   `json:"sha256"`

	// TOC, when non-nil, points to a zstd-compressed sidecar that lists
	// every regular file inside this part. Lets `ferry contents` answer
	// "what's in this snapshot" without downloading the part itself.
	TOC *TOCRef `json:"toc,omitempty"`
}

// TOCRef describes a per-part table-of-contents sidecar. The referenced
// object is a zstd-compressed text stream of "<size> <name>\n" lines, one
// per regular tar entry inside the corresponding part.
type TOCRef struct {
	Name    string `json:"name"`    // e.g. "parts/chaindata-live.toc.zst"
	Size    int64  `json:"size"`    // compressed sidecar size in bytes
	SHA256  string `json:"sha256"`  // sha256 of the compressed bytes
	Entries int    `json:"entries"` // number of files described
}

func (m *Manifest) Validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("manifest version %d unsupported, want %d", m.Version, ManifestVersion)
	}
	if m.Name == "" {
		return fmt.Errorf("manifest name is empty")
	}
	if !m.Role.Valid() {
		return fmt.Errorf("manifest role %q invalid", m.Role)
	}
	if len(m.Parts) == 0 {
		return fmt.Errorf("manifest has no parts")
	}
	seen := make(map[string]bool, len(m.Parts))
	for i, p := range m.Parts {
		if p.Name == "" {
			return fmt.Errorf("part %d has empty name", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("part %q listed twice", p.Name)
		}
		seen[p.Name] = true
		if p.SHA256 == "" {
			return fmt.Errorf("part %q missing sha256", p.Name)
		}
	}
	return nil
}

func (m *Manifest) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

func Decode(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
