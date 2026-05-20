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
	"path"
	"path/filepath"
	"sync"
	"time"

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

	// ParallelParts controls how many manifest parts download concurrently.
	// Values ≤ 1 keep the historical sequential behavior. Parts have
	// disjoint extraction targets (live in chaindata/ minus ancient/,
	// ancient-chain in chaindata/ancient/chain/, etc.), so concurrent
	// extraction is safe.
	ParallelParts int
}

// Stats captures wall-clock and per-part timings from a single Run.
// Returned alongside the manifest so the CLI (or any caller) can render
// a roll-up summary without recomputing the math.
type Stats struct {
	// Elapsed is the total wall-clock from Run entry to atomic promote.
	// With ParallelParts > 1, this is the outer wall-clock — typically
	// less than the sum of PartStats.Elapsed.
	Elapsed time.Duration

	// Parts is one entry per downloaded part, in manifest order.
	Parts []PartStats
}

// PartStats records how long one part took to fetch, verify, and extract.
type PartStats struct {
	Kind    snapshot.PartKind
	Name    string        // e.g. "parts/chaindata-live.tar.zst"
	Elapsed time.Duration // includes S3 GET + zstd decode + tar extract + sha256
}

// Run downloads and extracts the snapshot at prefix/name from src.
//
// Extraction is atomic: parts are written into a scratch directory next to
// <DataDir>/geth/, then renamed into place only after every part has been
// downloaded and sha256-verified. A failed download leaves no partial
// state behind. When opts.Force is set and <DataDir>/geth/ already
// exists, the original is removed only at promote time — a failure
// midway through doesn't damage the existing tree.
func Run(ctx context.Context, src backend.Backend, prefix string, opts Options) (*snapshot.Manifest, *Stats, error) {
	runStart := time.Now()

	if opts.DataDir == "" {
		return nil, nil, fmt.Errorf("DataDir is required")
	}
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("Name is required")
	}

	gethDir := filepath.Join(opts.DataDir, "geth")
	target, err := snapshot.PrepareAtomic(gethDir, opts.Force)
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			target.Abort()
		}
	}()

	manifest, err := fetchManifest(ctx, src, prefix, opts.Name)
	if err != nil {
		return nil, nil, err
	}

	partStats, err := downloadParts(ctx, src, prefix, opts.Name, manifest.Parts, target.Path, opts.Progress, opts.ParallelParts)
	if err != nil {
		return nil, nil, err
	}
	if err := target.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true
	return manifest, &Stats{
		Elapsed: time.Since(runStart),
		Parts:   partStats,
	}, nil
}

// downloadParts dispatches part fetch+verify+extract through a worker
// pool of size parallelN (clamped to 1..len(parts)). First error cancels
// the run; the caller's Abort on the atomic target wipes the scratch.
// Per-part wall-clocks are captured for the caller's summary.
func downloadParts(ctx context.Context, src backend.Backend, prefix, name string, parts []snapshot.Part, gethDir string, progressOut io.Writer, parallelN int) ([]PartStats, error) {
	if parallelN < 1 {
		parallelN = 1
	}
	if parallelN > len(parts) {
		parallelN = len(parts)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stats := make([]PartStats, len(parts))
	var (
		firstErr error
		errMu    sync.Mutex
	)
	recordErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}

	sem := make(chan struct{}, parallelN)
	var wg sync.WaitGroup
	for i, p := range parts {
		if runCtx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, p snapshot.Part) {
			defer wg.Done()
			defer func() { <-sem }()
			partStart := time.Now()
			if err := downloadPart(runCtx, src, prefix, name, p, gethDir, progressOut); err != nil {
				recordErr(fmt.Errorf("part %s: %w", p.Name, err))
				return
			}
			stats[i] = PartStats{
				Kind:    p.Kind,
				Name:    p.Name,
				Elapsed: time.Since(partStart),
			}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return stats, nil
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

	// sha256 sees compressed bytes from the wire; tracker sees uncompressed
	// bytes leaving zstd. Two separate tees so the ETA denominator
	// (manifest.UncompressedSize) compares apples to apples.
	hasher := sha256.New()
	teed := io.TeeReader(r, hasher)

	zr, err := codec.NewZstdDecoder(teed)
	if err != nil {
		return err
	}
	defer zr.Close()

	var extractIn io.Reader = zr
	var tracker *progress.Tracker
	if progressOut != nil {
		tracker = (&progress.Tracker{
			Label: string(p.Kind),
			Out:   progressOut,
			Total: p.UncompressedSize,
		}).Start()
		defer tracker.Stop()
		extractIn = io.TeeReader(zr, tracker.Writer())
	}

	if err := snapshot.Untar(extractIn, gethDir); err != nil {
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
