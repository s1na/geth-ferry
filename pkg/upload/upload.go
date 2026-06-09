// Package upload implements the upload pipeline: for each snapshot part it
// streams (tar -> zstd -> backend.Put) while computing sha256 and size
// counters, then writes manifest.json last.
package upload

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/codec"
	"github.com/s1na/geth-ferry/pkg/progress"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// Options configures an upload run.
type Options struct {
	// DataDir is the path to the geth datadir (the dir containing geth/, keystore/, …).
	DataDir string

	// Name is the snapshot identifier (e.g. geth-1-archive-23456789-1746014400).
	Name string

	// Role and Block are recorded in the manifest.
	Role  snapshot.Role
	Block uint64

	// HeadHash and HeadTimestamp identify the same head Block in
	// chain-canonical terms. They're populated automatically by the CLI
	// from datadir.Inspect when --block is auto-detected (or when an
	// explicit --block matches the chaindata's head). Zero values are
	// omitted from the manifest.
	HeadHash      string
	HeadTimestamp int64

	// Capabilities, when non-nil, is recorded verbatim under the
	// manifest's "capabilities" key. The CLI builds this from the
	// datadir alongside Inspect; tests and library callers can leave
	// it nil to skip the field entirely.
	Capabilities *snapshot.Capabilities

	// ChainID is recorded in the manifest. 0 means "unset" (allowed for now;
	// will default to mainnet when --chain-id flag plumbing lands).
	ChainID uint64

	// StateScheme is recorded in the manifest. Empty triggers a fallback
	// stat-of-<gethDir>/triedb/ heuristic — usable but unreliable (the
	// directory only exists after a graceful PBSS shutdown). Callers that
	// can determine the scheme authoritatively (e.g. via datadir.Inspect
	// reading the chaindata's pebble) should populate this explicitly.
	StateScheme snapshot.StateScheme

	// Level is the zstd encoder level. Zero falls back to codec.DefaultZstdLevel.
	Level int

	// Threads is the zstd encoder thread count. Zero uses the library default.
	Threads int

	// CreatedBy is recorded in the manifest. Callers should set this to
	// their tool name and version, e.g. "ferry/0.2.0"; unset becomes
	// "ferry/unknown".
	CreatedBy string

	// Force skips the LOCK / .ipc safety check.
	Force bool

	// Overwrite allows the upload to proceed when a snapshot with the same
	// Name already exists at the destination (i.e. its manifest.json is
	// present). Default behavior is to refuse — re-using a name silently
	// would replace published bytes that downstream consumers may have
	// pinned by sha256.
	Overwrite bool

	// Progress, when non-nil, receives a label per part and a stream wrap
	// callback. Used by the CLI to emit periodic "[label] X bytes, Y/s"
	// lines on stderr. Tests typically leave this nil.
	Progress io.Writer

	// ParallelParts controls how many snapshot parts upload concurrently.
	// Values ≤ 1 keep the historical sequential behavior. Each in-flight
	// part owns its own multipart-upload buffer set; peak memory scales
	// linearly. Tune carefully on memory-constrained hosts.
	ParallelParts int
}

// Stats captures wall-clock and per-part timings from a single Run.
// Returned alongside the manifest so the CLI (or any caller) can render
// a roll-up summary without recomputing the math. Durations are not
// stored in the manifest itself — they're a property of the operation
// that produced the snapshot, not of the snapshot.
type Stats struct {
	// Elapsed is the total wall-clock from Run entry to manifest commit.
	// With ParallelParts > 1, this is the outer wall-clock — typically
	// less than the sum of PartStats.Elapsed.
	Elapsed time.Duration

	// Parts is one entry per uploaded part, in canonical manifest order.
	// Includes only parts that actually uploaded (skipped HBSS-optional
	// parts don't appear).
	Parts []PartStats
}

// PartStats records how long one part took to stream and upload.
type PartStats struct {
	Kind    snapshot.PartKind
	Name    string        // e.g. "parts/chaindata-live.tar.zst"
	Elapsed time.Duration // includes tar walk + zstd encode + S3 multipart
}

// Run executes the upload to dst at prefix. prefix is the in-backend key
// prefix returned by backend.Open; the snapshot's manifest and parts are
// written under prefix/<name>/.
func Run(ctx context.Context, dst backend.Backend, prefix string, opts Options) (*snapshot.Manifest, *Stats, error) {
	runStart := time.Now()

	if err := opts.validate(); err != nil {
		return nil, nil, err
	}
	if opts.Level == 0 {
		opts.Level = codec.DefaultZstdLevel
	}
	if opts.CreatedBy == "" {
		opts.CreatedBy = "ferry/unknown"
	}

	gethDir := filepath.Join(opts.DataDir, "geth")
	if err := preflight(gethDir, opts.Force); err != nil {
		return nil, nil, err
	}

	if err := checkOverwrite(ctx, dst, prefix, opts.Name, opts.Overwrite); err != nil {
		return nil, nil, err
	}

	stateScheme := opts.StateScheme
	if stateScheme == "" {
		var err error
		stateScheme, err = detectStateScheme(gethDir)
		if err != nil {
			return nil, nil, err
		}
	}

	manifest := &snapshot.Manifest{
		Version:     snapshot.ManifestVersion,
		Name:        opts.Name,
		ChainID:     opts.ChainID,
		Role:        opts.Role,
		StateScheme: stateScheme,
		Head: snapshot.Head{
			Block:     opts.Block,
			Hash:      opts.HeadHash,
			Timestamp: opts.HeadTimestamp,
		},
		Capabilities: opts.Capabilities,
		CreatedAt:   time.Now().Unix(),
		CreatedBy:   opts.CreatedBy,
		Codec:       snapshot.CodecZstd,
		Level:       opts.Level,
	}

	if err := validateAncientLayout(filepath.Join(gethDir, "chaindata", "ancient")); err != nil {
		return nil, nil, err
	}

	planned, err := planParts(gethDir, prefix, opts)
	if err != nil {
		return nil, nil, err
	}
	parts, partStats, err := uploadParts(ctx, dst, planned, opts.ParallelParts)
	if err != nil {
		return nil, nil, err
	}
	manifest.Parts = parts

	if err := manifest.Validate(); err != nil {
		return nil, nil, fmt.Errorf("manifest invalid: %w", err)
	}
	if err := writeManifest(ctx, dst, prefix, opts.Name, manifest); err != nil {
		return nil, nil, fmt.Errorf("write manifest: %w", err)
	}
	return manifest, &Stats{
		Elapsed: time.Since(runStart),
		Parts:   partStats,
	}, nil
}

func (o Options) validate() error {
	if o.DataDir == "" {
		return fmt.Errorf("DataDir is required")
	}
	if o.Name == "" {
		return fmt.Errorf("Name is required")
	}
	if !o.Role.Valid() {
		return fmt.Errorf("Role %q invalid", o.Role)
	}
	if err := snapshot.ValidateNamePathSafety(o.Name); err != nil {
		return err
	}
	return nil
}

// checkOverwrite refuses to proceed when a manifest.json already exists at
// the destination, unless the caller opted in via Options.Overwrite.
//
// The manifest is the integrity marker for "snapshot exists" (per the
// design — it's written last, so its absence means an interrupted upload
// or no prior snapshot at this name). Re-using a name silently would
// replace bytes that downstream consumers may have pinned by sha256, so
// the default is loud refusal.
//
// Partial leftover state (parts/ exist but no manifest) is NOT treated as
// an existing snapshot — those bytes will be overwritten naturally as
// the new upload's parts go through their multipart-Put paths.
func checkOverwrite(ctx context.Context, dst backend.Backend, prefix, name string, overwrite bool) error {
	if overwrite {
		return nil
	}
	key := path.Join(prefix, name, snapshot.ManifestFilename)
	_, err := dst.Stat(ctx, key)
	if err == nil {
		return fmt.Errorf("snapshot %q already exists at destination; pass --overwrite to replace it", name)
	}
	if !errors.Is(err, backend.ErrNotExist) {
		return fmt.Errorf("check for existing snapshot %q: %w", name, err)
	}
	return nil
}

// preflight refuses to upload when the node looks live, unless force is set.
// LOCK, geth.ipc are the give-aways.
func preflight(gethDir string, force bool) error {
	if force {
		return nil
	}
	for _, child := range []string{"LOCK", "geth.ipc"} {
		p := filepath.Join(gethDir, child)
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		// LOCK exists when geth is running; size 0 either way, but pebble
		// keeps a flock on it. We can't tell from userspace whether the
		// flock is held without trying to acquire it, so be conservative:
		// presence is enough to refuse without --force.
		if info.Mode().IsRegular() || (info.Mode()&os.ModeSocket) != 0 {
			return fmt.Errorf("preflight: %s exists; geth may be running. Stop geth or pass --force", p)
		}
	}
	return nil
}

// validateAncientLayout enforces that <chaindata>/ancient/ contains only
// the two known freezer namespaces (chain/ and state/). Any extra entry is
// a sign of a geth version we don't understand or a bytestream we'd silently
// drop on the floor — fail fast rather than ship an incomplete snapshot.
//
// A missing ancient/ directory is fine (e.g. an empty/fresh datadir): the
// caller's per-part stat checks handle that.
func validateAncientLayout(ancientDir string) error {
	entries, err := os.ReadDir(ancientDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read ancient/: %w", err)
	}
	allowed := map[string]bool{"chain": true, "state": true}
	var unexpected []string
	for _, e := range entries {
		if !allowed[e.Name()] {
			unexpected = append(unexpected, e.Name())
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return fmt.Errorf("ancient/ contains unexpected entries %v; ferry only knows about chain/ and state/. Refusing to upload an incomplete snapshot",
			unexpected)
	}
	return nil
}

// detectStateScheme is the legacy stat-based fallback used only when
// Options.StateScheme is empty. It checks for <gethDir>/triedb/, which
// is created by geth only on a graceful PBSS shutdown (it holds the
// merkle.journal). A running or hard-rebooted PBSS node has no triedb/
// yet, so this can mis-tag actual PBSS chains as HBSS.
//
// Callers with access to the chaindata pebble (via internal/datadir.Inspect)
// should pass the authoritative scheme through Options.StateScheme to
// bypass this fallback.
func detectStateScheme(gethDir string) (snapshot.StateScheme, error) {
	if _, err := os.Stat(filepath.Join(gethDir, "triedb")); err == nil {
		return snapshot.StateSchemePath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return snapshot.StateSchemeHash, nil
}

type partRequest struct {
	Prefix   string
	Name     string
	PartPath string // e.g. "parts/chaindata-live.tar.zst"
	Kind     snapshot.PartKind
	SrcRoot  string // e.g. <datadir>/geth
	SrcSub   string // e.g. "chaindata"
	// Skip, when non-nil, returns true for relative slash paths (relative to
	// SrcRoot) that should be excluded. Returning true on a directory skips
	// its entire subtree.
	Skip func(rel string) bool

	// UncompressedTotal is the expected sum of regular-file sizes under
	// SrcRoot/SrcSub (minus Skip), pre-walked at planning time. Used to
	// give the progress tracker a denominator for ETAs. Zero means
	// "unknown"; the part still uploads, just without ETA output.
	UncompressedTotal int64

	Level    int
	Threads  int
	Progress io.Writer
}

// plannedPart is a partRequest plus the kind, kept paired so error
// messages can name the part regardless of dispatch order.
type plannedPart struct {
	kind snapshot.PartKind
	req  partRequest
}

// planParts builds the list of part requests for a snapshot. It skips
// optional parts whose source directories don't exist (HBSS nodes have no
// triedb/, no ancient/state/). Returned slice is in the canonical part
// order: live, ancient-chain, ancient-state, triedb.
func planParts(gethDir, prefix string, opts Options) ([]plannedPart, error) {
	common := partRequest{
		Prefix:   prefix,
		Name:     opts.Name,
		SrcRoot:  gethDir,
		Level:    opts.Level,
		Threads:  opts.Threads,
		Progress: opts.Progress,
	}

	planned := []plannedPart{
		{
			kind: snapshot.PartChaindataLive,
			req: withPart(common, snapshot.ChaindataLivePart, snapshot.PartChaindataLive,
				"chaindata", func(rel string) bool {
					return rel == "chaindata/ancient" || strings.HasPrefix(rel, "chaindata/ancient/")
				}),
		},
		{
			kind: snapshot.PartAncientChain,
			req:  withPart(common, snapshot.AncientChainPart, snapshot.PartAncientChain, "chaindata/ancient/chain", nil),
		},
	}

	// Optional parts: include only if the source directory exists.
	for _, opt := range []struct {
		kind snapshot.PartKind
		path string
		sub  string
		dir  string
	}{
		{snapshot.PartAncientState, snapshot.AncientStatePart, "chaindata/ancient/state", filepath.Join(gethDir, "chaindata", "ancient", "state")},
		{snapshot.PartTriedb, snapshot.TriedbPart, "triedb", filepath.Join(gethDir, "triedb")},
	} {
		if _, err := os.Stat(opt.dir); err == nil {
			planned = append(planned, plannedPart{
				kind: opt.kind,
				req:  withPart(common, opt.path, opt.kind, opt.sub, nil),
			})
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", opt.dir, err)
		}
	}

	// Pre-walk each part's source tree to seed UncompressedTotal. This
	// metadata-only pass is cheap relative to the streaming tar (seconds
	// vs. hours on a multi-TB datadir) and gives the progress tracker
	// the denominator it needs for ETAs.
	for i := range planned {
		_, total := walkSize(filepath.Join(gethDir, planned[i].req.SrcSub), planned[i].req.Skip)
		planned[i].req.UncompressedTotal = total
	}
	return planned, nil
}

// walkSize sums regular-file count and bytes under root. skip, when
// non-nil, returns true for slash-relative paths (relative to root) that
// should be excluded; returning true on a directory skips the subtree.
// Tolerates transient permission/IO errors during the walk by skipping
// the offending entry — appropriate for a best-effort planning pass.
//
// The skip predicate here uses paths relative to root, NOT to SrcRoot.
// That's the same shape tarTree expects, so chaindata-live's
// skip("chaindata/ancient/...") matches by inserting the SrcSub prefix.
func walkSize(root string, skip func(rel string) bool) (count, bytes int64) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Tar entry names are relative to SrcRoot (one level above root),
		// so reconstruct that shape for the skip predicate.
		rel, _ := filepath.Rel(filepath.Dir(root), p)
		if skip != nil && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		count++
		bytes += info.Size()
		return nil
	})
	return
}

// withPart returns a copy of common with the part-specific fields filled.
func withPart(common partRequest, partPath string, kind snapshot.PartKind, sub string, skip func(string) bool) partRequest {
	req := common
	req.PartPath = partPath
	req.Kind = kind
	req.SrcSub = sub
	req.Skip = skip
	return req
}

// uploadParts dispatches the planned parts through a worker pool of size
// parallelN (clamped to 1..len(planned)). Results land in the returned
// slice in canonical (planned) order regardless of completion order.
// First error cancels in-flight workers; remaining parts unwind cleanly
// via the existing Abort path on context cancellation.
//
// Per-part wall-clocks are captured into a parallel slice and returned
// alongside the parts, so the caller can report durations in the final
// summary without re-instrumenting the pipeline.
func uploadParts(ctx context.Context, dst backend.Backend, planned []plannedPart, parallelN int) ([]snapshot.Part, []PartStats, error) {
	if parallelN < 1 {
		parallelN = 1
	}
	if parallelN > len(planned) {
		parallelN = len(planned)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]snapshot.Part, len(planned))
	stats := make([]PartStats, len(planned))
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
	for i, pp := range planned {
		if runCtx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, pp plannedPart) {
			defer wg.Done()
			defer func() { <-sem }()
			partStart := time.Now()
			p, err := uploadPart(runCtx, dst, pp.req)
			if err != nil {
				recordErr(fmt.Errorf("upload %s: %w", pp.kind, err))
				return
			}
			results[i] = p
			stats[i] = PartStats{
				Kind:    pp.kind,
				Name:    pp.req.PartPath,
				Elapsed: time.Since(partStart),
			}
		}(i, pp)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return results, stats, nil
}

func uploadPart(ctx context.Context, be backend.Backend, req partRequest) (snapshot.Part, error) {
	key := path.Join(req.Prefix, req.Name, req.PartPath)

	bw, err := be.Put(ctx, key)
	if err != nil {
		return snapshot.Part{}, err
	}
	// Abort by default; only commit (Close) once we've finished writing and
	// validated everything. Without this, a tar / zstd / fs error mid-stream
	// would still flush whatever bytes were buffered and CompleteMultipart-
	// Upload them — committing a corrupt object to the bucket.
	committed := false
	defer func() {
		if !committed {
			bw.Abort()
		}
	}()

	hasher := sha256.New()
	compressedCounter := &countWriter{w: io.MultiWriter(bw, hasher)}

	zstdEnc, err := codec.NewZstdEncoder(compressedCounter, req.Level, req.Threads)
	if err != nil {
		return snapshot.Part{}, err
	}
	uncompressedCounter := &countWriter{w: zstdEnc}

	// Progress tracking sits ABOVE zstd so the bytes/sec figure reflects
	// source throughput (matches the UncompressedTotal denominator and is
	// what an operator wants to extrapolate — "how long until this 2 TB
	// part is done"). The tracker hands back an io.Writer that simply
	// counts; it doesn't consume bytes from the pipeline.
	var trackerWriter io.Writer = io.Discard
	var tracker *progress.Tracker
	if req.Progress != nil {
		tracker = (&progress.Tracker{
			Label: string(req.Kind),
			Out:   req.Progress,
			Total: req.UncompressedTotal,
		}).Start()
		defer tracker.Stop()
		trackerWriter = tracker.Writer()
	}
	tw := tar.NewWriter(io.MultiWriter(uncompressedCounter, trackerWriter))

	var entries []tocEntry
	onFile := func(name string, size int64) {
		entries = append(entries, tocEntry{Name: name, Size: size})
	}
	if err := tarTree(tw, req.SrcRoot, req.SrcSub, req.Skip, onFile); err != nil {
		return snapshot.Part{}, err
	}
	if err := tw.Close(); err != nil {
		return snapshot.Part{}, fmt.Errorf("close tar: %w", err)
	}
	if err := zstdEnc.Close(); err != nil {
		return snapshot.Part{}, fmt.Errorf("close zstd: %w", err)
	}
	if err := bw.Close(); err != nil {
		return snapshot.Part{}, fmt.Errorf("close backend writer: %w", err)
	}
	committed = true

	tocRef, err := uploadTOC(ctx, be, req, entries)
	if err != nil {
		return snapshot.Part{}, fmt.Errorf("upload toc: %w", err)
	}

	return snapshot.Part{
		Name:             req.PartPath,
		Kind:             req.Kind,
		UncompressedSize: uncompressedCounter.n,
		CompressedSize:   compressedCounter.n,
		SHA256:           hex.EncodeToString(hasher.Sum(nil)),
		TOC:              tocRef,
	}, nil
}

type tocEntry struct {
	Name string
	Size int64
}

// uploadTOC writes a zstd-compressed sidecar next to the part. Each line is
// "<size> <name>\n" — sortable, grep-able, and `zstd -dc <toc> | head` shows
// it directly. We use a low compression level (3) and one thread because
// the input is plain text and tiny (typically <1 MB even for 30K entries).
func uploadTOC(ctx context.Context, be backend.Backend, req partRequest, entries []tocEntry) (*snapshot.TOCRef, error) {
	tocPath := tocPathFor(req.PartPath)
	key := path.Join(req.Prefix, req.Name, tocPath)

	bw, err := be.Put(ctx, key)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			bw.Abort()
		}
	}()

	hasher := sha256.New()
	counter := &countWriter{w: io.MultiWriter(bw, hasher)}
	enc, err := codec.NewZstdEncoder(counter, 3, 1)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if _, err := fmt.Fprintf(enc, "%d %s\n", e.Size, e.Name); err != nil {
			_ = enc.Close()
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close toc zstd: %w", err)
	}
	if err := bw.Close(); err != nil {
		return nil, fmt.Errorf("close toc backend writer: %w", err)
	}
	committed = true
	return &snapshot.TOCRef{
		Name:    tocPath,
		Size:    counter.n,
		SHA256:  hex.EncodeToString(hasher.Sum(nil)),
		Entries: len(entries),
	}, nil
}

// tocPathFor turns "parts/chaindata-live.tar.zst" into "parts/chaindata-live.toc.zst".
func tocPathFor(partPath string) string {
	return strings.TrimSuffix(partPath, ".tar.zst") + ".toc.zst"
}

// tarTree writes srcRoot/srcSub recursively into tw, with tar entry names
// rooted at srcSub (so "chaindata/MANIFEST-..." rather than the absolute path).
// If skip is non-nil, paths for which it returns true are excluded; returning
// true on a directory skips the entire subtree. If onFile is non-nil, it's
// invoked once per regular file with the tar entry name and its size — used
// by callers to build a per-part TOC alongside the upload.
func tarTree(tw *tar.Writer, srcRoot, srcSub string, skip func(rel string) bool, onFile func(name string, size int64)) error {
	root := filepath.Join(srcRoot, srcSub)
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return err
		}
		entryName := filepath.ToSlash(rel)

		if skip != nil && skip(entryName) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = entryName
		if d.IsDir() {
			hdr.Name += "/"
		}
		// Strip uid/gid/uname/gname for reproducibility — these vary per host
		// and aren't load-bearing for geth.
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if onFile != nil {
			onFile(entryName, info.Size())
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		if cerr := f.Close(); cerr != nil && copyErr == nil {
			copyErr = cerr
		}
		return copyErr
	})
}

func writeManifest(ctx context.Context, be backend.Backend, prefix, name string, m *snapshot.Manifest) error {
	key := path.Join(prefix, name, snapshot.ManifestFilename)
	w, err := be.Put(ctx, key)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			w.Abort()
		}
	}()
	var buf bytes.Buffer
	if err := m.Encode(&buf); err != nil {
		return err
	}
	if _, err := io.Copy(w, &buf); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
