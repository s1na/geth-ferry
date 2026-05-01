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
	Version     int         `json:"version"`
	Name        string      `json:"name"`
	ChainID     uint64      `json:"chain_id"`
	Role        Role        `json:"role"`
	StateScheme StateScheme `json:"state_scheme"`
	Head        Head        `json:"head"`
	CreatedAt   int64       `json:"created_at"` // Unix seconds (UTC)
	CreatedBy   string      `json:"created_by"`
	Codec       Codec       `json:"codec"`
	Level       int         `json:"level"`
	Parts       []Part      `json:"parts"`
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
