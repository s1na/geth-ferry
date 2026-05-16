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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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

	// ChainID is recorded in the manifest. 0 means "unset" (allowed for now;
	// will default to mainnet when --chain-id flag plumbing lands).
	ChainID uint64

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

	// Progress, when non-nil, receives a label per part and a stream wrap
	// callback. Used by the CLI to emit periodic "[label] X bytes, Y/s"
	// lines on stderr. Tests typically leave this nil.
	Progress io.Writer
}

// Run executes the upload to dst at prefix. prefix is the in-backend key
// prefix returned by backend.Open; the snapshot's manifest and parts are
// written under prefix/<name>/.
func Run(ctx context.Context, dst backend.Backend, prefix string, opts Options) (*snapshot.Manifest, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.Level == 0 {
		opts.Level = codec.DefaultZstdLevel
	}
	if opts.CreatedBy == "" {
		opts.CreatedBy = "ferry/unknown"
	}

	gethDir := filepath.Join(opts.DataDir, "geth")
	if err := preflight(gethDir, opts.Force); err != nil {
		return nil, err
	}

	stateScheme, err := detectStateScheme(gethDir)
	if err != nil {
		return nil, err
	}

	manifest := &snapshot.Manifest{
		Version:     snapshot.ManifestVersion,
		Name:        opts.Name,
		ChainID:     opts.ChainID,
		Role:        opts.Role,
		StateScheme: stateScheme,
		Head:        snapshot.Head{Block: opts.Block},
		CreatedAt:   time.Now().Unix(),
		CreatedBy:   opts.CreatedBy,
		Codec:       snapshot.CodecZstd,
		Level:       opts.Level,
	}

	if err := validateAncientLayout(filepath.Join(gethDir, "chaindata", "ancient")); err != nil {
		return nil, err
	}

	// Live pebble: tar the chaindata/ tree but skip the ancient/ subtree —
	// it goes into its own parts below.
	livePart, err := uploadPart(ctx, dst, partRequest{
		Prefix:   prefix,
		Name:     opts.Name,
		PartPath: snapshot.ChaindataLivePart,
		Kind:     snapshot.PartChaindataLive,
		SrcRoot:  gethDir,
		SrcSub:   "chaindata",
		Skip: func(rel string) bool {
			return rel == "chaindata/ancient" || strings.HasPrefix(rel, "chaindata/ancient/")
		},
		Level:    opts.Level,
		Threads:  opts.Threads,
		Progress: opts.Progress,
	})
	if err != nil {
		return nil, fmt.Errorf("upload chaindata-live: %w", err)
	}
	manifest.Parts = append(manifest.Parts, livePart)

	// Ancient chain freezer: always present.
	chainPart, err := uploadPart(ctx, dst, partRequest{
		Prefix:   prefix,
		Name:     opts.Name,
		PartPath: snapshot.AncientChainPart,
		Kind:     snapshot.PartAncientChain,
		SrcRoot:  gethDir,
		SrcSub:   "chaindata/ancient/chain",
		Level:    opts.Level,
		Threads:  opts.Threads,
		Progress: opts.Progress,
	})
	if err != nil {
		return nil, fmt.Errorf("upload ancient-chain: %w", err)
	}
	manifest.Parts = append(manifest.Parts, chainPart)

	// Ancient state freezer: present on PBSS nodes (full or archive).
	if _, err := os.Stat(filepath.Join(gethDir, "chaindata", "ancient", "state")); err == nil {
		statePart, err := uploadPart(ctx, dst, partRequest{
			Prefix:   prefix,
			Name:     opts.Name,
			PartPath: snapshot.AncientStatePart,
			Kind:     snapshot.PartAncientState,
			SrcRoot:  gethDir,
			SrcSub:   "chaindata/ancient/state",
			Level:    opts.Level,
			Threads:  opts.Threads,
			Progress: opts.Progress,
		})
		if err != nil {
			return nil, fmt.Errorf("upload ancient-state: %w", err)
		}
		manifest.Parts = append(manifest.Parts, statePart)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat ancient/state: %w", err)
	}

	// Triedb: present only on PBSS nodes.
	if _, err := os.Stat(filepath.Join(gethDir, "triedb")); err == nil {
		triedbPart, err := uploadPart(ctx, dst, partRequest{
			Prefix:   prefix,
			Name:     opts.Name,
			PartPath: snapshot.TriedbPart,
			Kind:     snapshot.PartTriedb,
			SrcRoot:  gethDir,
			SrcSub:   "triedb",
			Level:    opts.Level,
			Threads:  opts.Threads,
			Progress: opts.Progress,
		})
		if err != nil {
			return nil, fmt.Errorf("upload triedb: %w", err)
		}
		manifest.Parts = append(manifest.Parts, triedbPart)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat triedb: %w", err)
	}

	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("manifest invalid: %w", err)
	}
	if err := writeManifest(ctx, dst, prefix, opts.Name, manifest); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	return manifest, nil
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
	if _, err := snapshot.ParseName(o.Name); err != nil {
		return err
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

// detectStateScheme infers PBSS vs HBSS from the presence of triedb/.
// We never write HBSS snapshots so the caller can rely on this for the
// manifest's state_scheme field without an explicit flag.
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
	Skip     func(rel string) bool
	Level    int
	Threads  int
	Progress io.Writer
}

func uploadPart(ctx context.Context, be backend.Backend, req partRequest) (snapshot.Part, error) {
	key := path.Join(req.Prefix, req.Name, req.PartPath)

	bw, err := be.Put(ctx, key)
	if err != nil {
		return snapshot.Part{}, err
	}
	// On any error we close bw to release whatever resources it holds; on
	// success we return the bw.Close error to the caller (so they see e.g.
	// a multipart finalize failure).
	closed := false
	defer func() {
		if !closed {
			_ = bw.Close()
		}
	}()

	hasher := sha256.New()
	writers := []io.Writer{bw, hasher}
	var tracker *progress.Tracker
	if req.Progress != nil {
		tracker = (&progress.Tracker{Label: string(req.Kind), Out: req.Progress}).Start()
		defer tracker.Stop()
		writers = append(writers, tracker.Writer())
	}
	compressedCounter := &countWriter{w: io.MultiWriter(writers...)}

	zstdEnc, err := codec.NewZstdEncoder(compressedCounter, req.Level, req.Threads)
	if err != nil {
		return snapshot.Part{}, err
	}
	uncompressedCounter := &countWriter{w: zstdEnc}
	tw := tar.NewWriter(uncompressedCounter)

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
	closed = true
	if err := bw.Close(); err != nil {
		return snapshot.Part{}, fmt.Errorf("close backend writer: %w", err)
	}

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
	closed := false
	defer func() {
		if !closed {
			_ = bw.Close()
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
	closed = true
	if err := bw.Close(); err != nil {
		return nil, fmt.Errorf("close toc backend writer: %w", err)
	}
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
	var buf bytes.Buffer
	if err := m.Encode(&buf); err != nil {
		_ = w.Close()
		return err
	}
	if _, err := io.Copy(w, &buf); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
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
