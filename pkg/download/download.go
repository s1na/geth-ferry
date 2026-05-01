// Package download implements the download pipeline: fetch manifest.json,
// stream each part through (sha256 verify -> zstd decode -> tar extract)
// into the target datadir.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/codec"
	"github.com/s1na/geth-ferry/pkg/progress"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// Options configures a download run.
type Options struct {
	// DataDir is where the snapshot is extracted. The contents are written
	// under <DataDir>/geth/.
	DataDir string

	// Name is the snapshot identifier — the directory under prefix/.
	Name string

	// Force allows extraction into a non-empty <DataDir>/geth/.
	Force bool

	// Progress, when non-nil, is the destination for periodic progress
	// lines (typically os.Stderr). Tests usually leave nil.
	Progress io.Writer
}

// Run downloads and extracts the snapshot at prefix/name from src.
func Run(ctx context.Context, src backend.Backend, prefix string, opts Options) (*snapshot.Manifest, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("DataDir is required")
	}
	if opts.Name == "" {
		return nil, fmt.Errorf("Name is required")
	}

	gethDir := filepath.Join(opts.DataDir, "geth")
	if err := snapshot.EnsureEmpty(gethDir, opts.Force); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(gethDir, 0o755); err != nil {
		return nil, err
	}

	manifest, err := fetchManifest(ctx, src, prefix, opts.Name)
	if err != nil {
		return nil, err
	}

	for _, p := range manifest.Parts {
		if err := downloadPart(ctx, src, prefix, opts.Name, p, gethDir, opts.Progress); err != nil {
			return nil, fmt.Errorf("part %s: %w", p.Name, err)
		}
	}
	return manifest, nil
}

func fetchManifest(ctx context.Context, src backend.Backend, prefix, name string) (*snapshot.Manifest, error) {
	key := path.Join(prefix, name, snapshot.ManifestFilename)
	r, err := src.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get manifest: %w", err)
	}
	defer r.Close()
	return snapshot.Decode(r)
}

func downloadPart(ctx context.Context, src backend.Backend, prefix, name string, p snapshot.Part, gethDir string, progressOut io.Writer) error {
	key := path.Join(prefix, name, p.Name)
	r, err := src.Get(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()

	hasher := sha256.New()
	var tracker *progress.Tracker
	var src2 io.Reader = r
	if progressOut != nil {
		tracker = (&progress.Tracker{Label: string(p.Kind), Out: progressOut}).Start()
		defer tracker.Stop()
		src2 = io.TeeReader(r, tracker.Writer())
	}
	teed := io.TeeReader(src2, hasher)

	zr, err := codec.NewZstdDecoder(teed)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := snapshot.Untar(zr, gethDir); err != nil {
		return err
	}
	// Drain any trailing zstd bytes (typically zero) so the sha256 covers
	// the full compressed object, not just up to the tar EOF marker.
	if _, err := io.Copy(io.Discard, teed); err != nil {
		return fmt.Errorf("drain remainder: %w", err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != p.SHA256 {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, p.SHA256)
	}
	return nil
}
