package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/internal/datadir"
	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/snapshot"
	"github.com/s1na/geth-ferry/pkg/upload"
)

func uploadCmd() *cobra.Command {
	var (
		src, dst, name string
		role           string
		block, chainID uint64
		level, threads   int
		force, quiet     bool
		dryRun           bool
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload a stopped node's datadir to a remote",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			// Auto-detect any of name/block/chain-id that the user didn't pass.
			nameSet := cmd.Flags().Changed("name")
			blockSet := cmd.Flags().Changed("block")
			chainIDSet := cmd.Flags().Changed("chain-id")
			if !nameSet || !blockSet || !chainIDSet {
				info, err := datadir.Inspect(src)
				if err != nil {
					return fmt.Errorf("auto-detect from %s: %w (pass --name/--block/--chain-id explicitly to bypass)", src, err)
				}
				if !blockSet {
					block = info.HeadBlock
				}
				if !chainIDSet {
					chainID = info.ChainID
				}
				if !nameSet {
					name = fmt.Sprintf("geth-%d-%s-%d-%d",
						chainID, role, block, time.Now().Unix())
				}
				fmt.Fprintf(os.Stderr, "auto-detected: name=%s chain_id=%d head=%d state_scheme=%s\n",
					name, chainID, block, info.StateScheme)
			}

			if dryRun {
				return printPlan(os.Stdout, src, dst, name, role, block, chainID, level, threads)
			}

			be, prefix, err := backend.Open(dst)
			if err != nil {
				return err
			}
			var progressOut io.Writer = os.Stderr
			if quiet {
				progressOut = nil
			}
			m, err := upload.Run(ctx, be, prefix, upload.Options{
				DataDir:   src,
				Name:      name,
				Role:      snapshot.Role(role),
				Block:     block,
				ChainID:   chainID,
				Level:     level,
				Threads:   threads,
				CreatedBy: "ferry/" + version,
				Force:     force,
				Progress:  progressOut,
			})
			if err != nil {
				return err
			}
			fmt.Printf("uploaded %s — %d part(s)\n", m.Name, len(m.Parts))
			for _, p := range m.Parts {
				fmt.Printf("  %s  %d → %d bytes  sha256=%s\n",
					p.Name, p.UncompressedSize, p.CompressedSize, p.SHA256)
			}
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
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress periodic progress output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the planned upload (parts, source bytes, destination keys) and exit without writing anything")
	for _, name := range []string{"src", "dst", "role"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
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

	for i := range parts {
		switch parts[i].key {
		case snapshot.ChaindataLivePart:
			parts[i].fileCount, parts[i].uncompBytes = walkSize(filepath.Join(gethDir, "chaindata"), func(rel string) bool {
				return rel == "ancient" || strings.HasPrefix(rel, "ancient/")
			})
		case snapshot.AncientChainPart:
			parts[i].fileCount, parts[i].uncompBytes = walkSize(filepath.Join(gethDir, "chaindata", "ancient", "chain"), nil)
		case snapshot.AncientStatePart:
			dir := filepath.Join(gethDir, "chaindata", "ancient", "state")
			if _, err := os.Stat(dir); err != nil {
				parts[i].skipReason = "no ancient/state/ on disk"
				break
			}
			parts[i].fileCount, parts[i].uncompBytes = walkSize(dir, nil)
		case snapshot.TriedbPart:
			dir := filepath.Join(gethDir, "triedb")
			if _, err := os.Stat(dir); err != nil {
				parts[i].skipReason = "no triedb/ on disk (HBSS)"
				break
			}
			parts[i].fileCount, parts[i].uncompBytes = walkSize(dir, nil)
		}
	}

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
		fmt.Fprintf(out, "  - %-32s %12s   %d files\n", p.key, humanBytesU(p.uncompBytes), p.fileCount)
		fmt.Fprintf(out, "      %s\n", p.root)
		totalFiles += p.fileCount
		totalBytes += p.uncompBytes
	}
	fmt.Fprintf(out, "\n  total uncompressed source: %s across %d files\n",
		humanBytesU(totalBytes), totalFiles)

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

// humanBytesU is a colocated copy of the list.go helper. Different file,
// keep them out of each other's way.
func humanBytesU(n int64) string {
	const (
		KiB = 1024
		MiB = KiB * 1024
		GiB = MiB * 1024
		TiB = GiB * 1024
	)
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(TiB))
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
