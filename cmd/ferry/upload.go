package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/internal/datadir"
	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/progress"
	"github.com/s1na/geth-ferry/pkg/snapshot"
	"github.com/s1na/geth-ferry/pkg/upload"
)

func uploadCmd() *cobra.Command {
	var (
		src, dst, name                                 string
		role                                           string
		block, chainID                                 uint64
		level, threads                                 int
		force, quiet, overwrite                        bool
		dryRun                                         bool
		multipartSize, multipartConcurrency, parallelN int
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload a stopped node's datadir to a remote",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Inspect the stopped datadir for chain-id / head / state-scheme.
			// We try this unconditionally — even when --name / --block /
			// --chain-id are passed explicitly — because the state-scheme
			// detection is read authoritatively from the chaindata pebble,
			// and we want the manifest to record it correctly regardless of
			// how the other fields were sourced.
			//
			// When all three explicit flags are set, an Inspect failure is
			// tolerated (the operator opted out of auto-detect; upload's
			// stat-based fallback for state scheme runs). When any flag
			// needs auto-detect, Inspect failure is fatal.
			nameSet := cmd.Flags().Changed("name")
			blockSet := cmd.Flags().Changed("block")
			chainIDSet := cmd.Flags().Changed("chain-id")
			needAutoDetect := !nameSet || !blockSet || !chainIDSet

			info, err := datadir.Inspect(src)
			if err != nil {
				if needAutoDetect {
					return fmt.Errorf("auto-detect from %s: %w (pass --name/--block/--chain-id explicitly to bypass)", src, err)
				}
				info = &datadir.Info{} // state scheme stays empty; upload falls back
			}
			if !blockSet {
				block = info.HeadBlock
			}
			if !chainIDSet {
				chainID = info.ChainID
			}
			if !nameSet {
				name = fmt.Sprintf("geth-%d-%s-%d", chainID, role, block)
			}
			fmt.Fprintf(os.Stderr, "auto-detected: name=%s chain_id=%d head=%d state_scheme=%s\n",
				name, chainID, block, info.StateScheme)

			if dryRun {
				return printPlan(os.Stdout, src, dst, name, role, block, chainID, level, threads)
			}

			be, prefix, err := backend.Open(dst,
				backend.WithMultipartPartSize(multipartSize),
				backend.WithMultipartConcurrency(multipartConcurrency),
			)
			if err != nil {
				return err
			}
			var progressOut io.Writer = os.Stderr
			if quiet {
				progressOut = nil
			}
			m, st, err := upload.Run(ctx, be, prefix, upload.Options{
				DataDir:       src,
				Name:          name,
				Role:          snapshot.Role(role),
				Block:         block,
				ChainID:       chainID,
				StateScheme:   snapshot.StateScheme(info.StateScheme),
				Level:         level,
				Threads:       threads,
				CreatedBy:     "ferry/" + version,
				Force:         force,
				Overwrite:     overwrite,
				Progress:      progressOut,
				ParallelParts: parallelN,
			})
			if err != nil {
				return err
			}
			printUploadSummary(cmd.OutOrStdout(), m, st)
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "datadir to upload (required)")
	cmd.Flags().StringVar(&dst, "dst", "", "destination URL (required)")
	cmd.Flags().StringVar(&name, "name", "", "snapshot name (auto-derived from datadir if unset)")
	cmd.Flags().StringVar(&role, "role", "", "archive|full (required)")
	cmd.Flags().Uint64Var(&block, "block", 0, "head block (auto-detected from datadir if unset)")
	cmd.Flags().Uint64Var(&chainID, "chain-id", 1, "EVM chain id (auto-detected from datadir if unset)")
	cmd.Flags().IntVar(&level, "level", 0, "zstd level (0 = default)")
	cmd.Flags().IntVar(&threads, "threads", 0, "zstd encoder threads (0 = library default)")
	cmd.Flags().BoolVar(&force, "force", false, "ignore preflight LOCK / .ipc check")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing snapshot at the same --name (default: refuse)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress periodic progress output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the planned upload (parts, source bytes, destination keys) and exit without writing anything")
	cmd.Flags().IntVar(&multipartSize, "multipart-size", 0, "S3 multipart part size in bytes (0 = backend default, 256 MiB)")
	cmd.Flags().IntVar(&multipartConcurrency, "multipart-concurrency", 0, "max in-flight UploadPart requests per object (0 = backend default, 5)")
	cmd.Flags().IntVar(&parallelN, "parallel-parts", 1, "number of snapshot parts to upload in parallel (1 = sequential)")
	for _, name := range []string{"src", "dst", "role"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// printUploadSummary writes the post-upload roll-up: total elapsed and
// throughput, then one line per part with its own elapsed/rate alongside
// bytes and sha256. Per-part rate uses the part's own wall-clock, so
// parallel parts each report their actual throughput rather than the
// outer-elapsed-shared denominator.
func printUploadSummary(out io.Writer, m *snapshot.Manifest, st *upload.Stats) {
	var totalUncompressed, totalCompressed int64
	for _, p := range m.Parts {
		totalUncompressed += p.UncompressedSize
		totalCompressed += p.CompressedSize
	}
	fmt.Fprintf(out, "uploaded %s — %d part(s), %s → %s in %s (%s)\n",
		m.Name, len(m.Parts),
		progress.HumanBytes(totalUncompressed),
		progress.HumanBytes(totalCompressed),
		fmtDuration(st.Elapsed),
		fmtRate(totalUncompressed, st.Elapsed),
	)
	elapsedByName := map[string]time.Duration{}
	for _, ps := range st.Parts {
		elapsedByName[ps.Name] = ps.Elapsed
	}
	for _, p := range m.Parts {
		e := elapsedByName[p.Name]
		fmt.Fprintf(out, "  %s  %s → %s  in %s (%s)  sha256=%s\n",
			p.Name,
			progress.HumanBytes(p.UncompressedSize),
			progress.HumanBytes(p.CompressedSize),
			fmtDuration(e),
			fmtRate(p.UncompressedSize, e),
			p.SHA256,
		)
	}
}

// fmtDuration formats a duration with second precision and an em-dash
// fallback for the zero value (which shouldn't happen in practice but
// guards against degenerate inputs).
func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return d.Round(time.Second).String()
}

// fmtRate renders bytes-per-second over a duration as "X/s". Returns
// "—/s" for zero or negative duration.
func fmtRate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "—/s"
	}
	return progress.HumanBytes(int64(float64(bytes)/d.Seconds())) + "/s"
}

// printPlan walks the source tree and prints, per planned part, the file
// count and uncompressed byte total — without opening the destination
// backend or writing anything. The eventual destination keys are printed
// for visual confirmation, but the URL is parsed only enough to render
// them; no network or filesystem state is touched.
func printPlan(out io.Writer, src, dst, name, role string, block, chainID uint64, level, threads int) error {
	gethDir := filepath.Join(src, "geth")
	if _, err := os.Stat(gethDir); err != nil {
		return fmt.Errorf("source datadir: %w", err)
	}

	type plannedPart struct {
		key         string
		root        string // for display only
		fileCount   int64
		uncompBytes int64
		skipReason  string // non-empty if this part won't be uploaded
	}

	parts := []plannedPart{
		{
			key:  snapshot.ChaindataLivePart,
			root: filepath.Join(gethDir, "chaindata") + " (excluding ancient/)",
		},
		{
			key:  snapshot.AncientChainPart,
			root: filepath.Join(gethDir, "chaindata", "ancient", "chain"),
		},
		{
			key:  snapshot.AncientStatePart,
			root: filepath.Join(gethDir, "chaindata", "ancient", "state"),
		},
		{
			key:  snapshot.TriedbPart,
			root: filepath.Join(gethDir, "triedb"),
		},
	}

	// Pre-check optional parts so the walks below can run in parallel
	// without each goroutine having to do its own existence check.
	for i := range parts {
		switch parts[i].key {
		case snapshot.AncientStatePart:
			if _, err := os.Stat(filepath.Join(gethDir, "chaindata", "ancient", "state")); err != nil {
				parts[i].skipReason = "no ancient/state/ on disk"
			}
		case snapshot.TriedbPart:
			if _, err := os.Stat(filepath.Join(gethDir, "triedb")); err != nil {
				parts[i].skipReason = "no triedb/ on disk (HBSS)"
			}
		}
	}

	// Walk each part's source tree in parallel — metadata-only, IO-bound
	// on the filesystem. On a 350 GB datadir this turns a serial walk
	// dominated by the live + ancient-chain trees into something closer
	// to max(live, ancient-chain) wall-clock.
	var wg sync.WaitGroup
	for i := range parts {
		if parts[i].skipReason != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch parts[i].key {
			case snapshot.ChaindataLivePart:
				parts[i].fileCount, parts[i].uncompBytes = walkSize(
					filepath.Join(gethDir, "chaindata"),
					func(rel string) bool {
						return rel == "ancient" || strings.HasPrefix(rel, "ancient/")
					})
			case snapshot.AncientChainPart:
				parts[i].fileCount, parts[i].uncompBytes = walkSize(filepath.Join(gethDir, "chaindata", "ancient", "chain"), nil)
			case snapshot.AncientStatePart:
				parts[i].fileCount, parts[i].uncompBytes = walkSize(filepath.Join(gethDir, "chaindata", "ancient", "state"), nil)
			case snapshot.TriedbPart:
				parts[i].fileCount, parts[i].uncompBytes = walkSize(filepath.Join(gethDir, "triedb"), nil)
			}
		}(i)
	}
	wg.Wait()

	if level == 0 {
		level = 5
	}

	fmt.Fprintf(out, "DRY RUN — no bytes written.\n\n")
	fmt.Fprintf(out, "  src           %s\n", src)
	fmt.Fprintf(out, "  dst           %s\n", dst)
	fmt.Fprintf(out, "  name          %s\n", name)
	fmt.Fprintf(out, "  role          %s\n", role)
	fmt.Fprintf(out, "  chain_id      %d\n", chainID)
	fmt.Fprintf(out, "  block         %d\n", block)
	fmt.Fprintf(out, "  zstd level    %d\n", level)
	if threads > 0 {
		fmt.Fprintf(out, "  zstd threads  %d\n", threads)
	}
	fmt.Fprintf(out, "\nplanned parts:\n")
	var totalFiles, totalBytes int64
	for _, p := range parts {
		if p.skipReason != "" {
			fmt.Fprintf(out, "  - %-32s SKIPPED (%s)\n", p.key, p.skipReason)
			continue
		}
		fmt.Fprintf(out, "  - %-32s %12s   %d files\n", p.key, progress.HumanBytes(p.uncompBytes), p.fileCount)
		fmt.Fprintf(out, "      %s\n", p.root)
		totalFiles += p.fileCount
		totalBytes += p.uncompBytes
	}
	fmt.Fprintf(out, "\n  total uncompressed source: %s across %d files\n",
		progress.HumanBytes(totalBytes), totalFiles)

	// Show what the destination keys would look like, without opening the backend.
	fmt.Fprintf(out, "\ndestination keys (relative to dst):\n")
	for _, p := range parts {
		if p.skipReason == "" {
			fmt.Fprintf(out, "  %s\n", path.Join(name, p.key))
		}
	}
	fmt.Fprintf(out, "  %s\n", path.Join(name, snapshot.ManifestFilename))

	return nil
}

// walkSize sums regular-file count and bytes under root. skip, when
// non-nil, returns true for slash-relative paths (relative to root) that
// should be excluded; returning true on a directory skips the subtree.
func walkSize(root string, skip func(rel string) bool) (count, bytes int64) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate permission/transient errors during a planning pass
		}
		rel, _ := filepath.Rel(root, p)
		relSlash := filepath.ToSlash(rel)
		if skip != nil && skip(relSlash) {
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
