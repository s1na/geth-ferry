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
	"bufio"
	"bytes"
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

	// Legacy snapshots come in two shapes:
	//   1. tar entries rooted at "chaindata/..." — produced by the runbook
	//      (`tar -C /datadrive/geth/geth -cf - chaindata`). These extract
	//      cleanly into <datadir>/geth/.
	//   2. tar entries flat (e.g. "000016.sst", "ancient/chain/..."), as in
	//      the older `chaindata-<block>.tar.lz4` benchmarker snapshots,
	//      which were tarred from inside the chaindata directory itself.
	//      These need to land at <datadir>/geth/chaindata/.
	//
	// Detect by peeking the first tar header's name and pick the right dst.
	firstName, peeked, err := peekFirstTarEntry(dec)
	if err != nil {
		return fmt.Errorf("peek tar: %w", err)
	}
	dst := gethDir
	if !strings.HasPrefix(firstName, "chaindata/") {
		dst = filepath.Join(gethDir, "chaindata")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	}
	return snapshot.Untar(peeked, dst)
}

// peekFirstTarEntry returns the name of the first tar entry without
// consuming it. The returned io.Reader still presents the full tar stream.
//
// Only the standard 100-byte name field is consulted; PAX-extended headers
// are not unpacked. That's fine for the only two formats ferry has to
// handle (legacy lz4 archive, ferry-produced zstd parts), neither of which
// uses PAX.
func peekFirstTarEntry(r io.Reader) (string, io.Reader, error) {
	buf := bufio.NewReaderSize(r, 4096)
	head, err := buf.Peek(512)
	if err != nil && err != io.EOF {
		return "", buf, err
	}
	if len(head) < 100 {
		return "", buf, fmt.Errorf("tar stream truncated: %d bytes", len(head))
	}
	name := string(bytes.TrimRight(head[:100], "\x00"))
	return name, buf, nil
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
