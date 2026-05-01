// Package legacy implements the read path for single-file legacy snapshots.
// These predate the manifest-based layout and look like:
//
//	chaindata-<block>.tar.lz4
//	archive-pbss-<block>-<YYYYMMDD>.tar.zst
//
// They sit directly in a bucket prefix (no <name>/ subdirectory, no
// manifest.json). Tar entries inside are rooted at "chaindata/...", so they
// extract cleanly into <datadir>/geth/.
//
// Legacy is read-only. We never produce this format.
package legacy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/codec"
	"github.com/s1na/geth-ferry/pkg/progress"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// Options configures a legacy download run.
type Options struct {
	// DataDir is where the snapshot is extracted. Contents land under
	// <DataDir>/geth/.
	DataDir string

	// Force allows extraction into a non-empty <DataDir>/geth/.
	Force bool

	// Progress, when non-nil, is the destination for periodic progress
	// lines (typically os.Stderr).
	Progress io.Writer
}

// Download fetches the legacy single-file snapshot at key from src and
// extracts it into <DataDir>/geth/. The codec is detected by suffix.
func Download(ctx context.Context, src backend.Backend, key string, opts Options) error {
	if opts.DataDir == "" {
		return fmt.Errorf("DataDir is required")
	}

	gethDir := filepath.Join(opts.DataDir, "geth")
	if err := snapshot.EnsureEmpty(gethDir, opts.Force); err != nil {
		return err
	}
	if err := os.MkdirAll(gethDir, 0o755); err != nil {
		return err
	}

	r, err := src.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer r.Close()

	var src2 io.Reader = r
	if opts.Progress != nil {
		tracker := (&progress.Tracker{Label: "legacy", Out: opts.Progress}).Start()
		defer tracker.Stop()
		src2 = io.TeeReader(r, tracker.Writer())
	}

	dec, err := decode(src2, key)
	if err != nil {
		return err
	}
	defer dec.Close()

	return snapshot.Untar(dec, gethDir)
}

func decode(r io.Reader, key string) (io.ReadCloser, error) {
	lower := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lower, ".tar.zst"):
		return codec.NewZstdDecoder(r)
	case strings.HasSuffix(lower, ".tar.lz4"):
		return codec.NewLz4Decoder(r)
	default:
		return nil, fmt.Errorf("legacy: unrecognized extension on %q", key)
	}
}
