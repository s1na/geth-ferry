package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/codec"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

func contentsCmd() *cobra.Command {
	var src string
	cmd := &cobra.Command{
		Use:   "contents",
		Short: "List the files inside a snapshot's parts (cheap; reads only TOCs)",
		Long: `Fetches manifest.json plus each part's TOC sidecar and prints the file list.
Network/disk cost: ~kilobytes per part. The parts themselves are not read.

Snapshots produced by older ferry versions (no TOCs) are flagged.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			rootURL, name, err := snapshot.SplitTrailingSegment(src)
			if err != nil {
				return err
			}
			be, prefix, err := backend.Open(rootURL)
			if err != nil {
				return err
			}

			manifestKey := path.Join(prefix, name, snapshot.ManifestFilename)
			r, err := be.Get(ctx, manifestKey)
			if err != nil {
				return fmt.Errorf("get manifest: %w", err)
			}
			m, err := snapshot.Decode(r)
			r.Close()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			for _, p := range m.Parts {
				if p.TOC == nil {
					fmt.Fprintf(out, "# %s — no TOC (snapshot predates ferry's TOC sidecars; rerun upload to add)\n\n", p.Name)
					continue
				}
				if err := printTOC(ctx, be, prefix, name, p, out); err != nil {
					return fmt.Errorf("toc for %s: %w", p.Name, err)
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "snapshot URL (required)")
	_ = cmd.MarkFlagRequired("src")
	return cmd
}

// printTOC fetches one part's TOC sidecar, verifies its sha256, decompresses
// it, and prints the file list to out. Each TOC line is "<size> <name>\n";
// we keep the format as-is so the output is grep-friendly.
func printTOC(ctx context.Context, be backend.Backend, prefix, name string, p snapshot.Part, out io.Writer) error {
	tocKey := path.Join(prefix, name, p.TOC.Name)
	r, err := be.Get(ctx, tocKey)
	if err != nil {
		return err
	}
	defer r.Close()

	hasher := sha256.New()
	teed := io.TeeReader(r, hasher)

	dec, err := codec.NewZstdDecoder(teed)
	if err != nil {
		return err
	}
	defer dec.Close()

	fmt.Fprintf(out, "# %s — %d entries\n", p.Name, p.TOC.Entries)
	if _, err := io.Copy(out, dec); err != nil {
		return err
	}
	// Drain any trailing bytes after the zstd EOF so the sha256 covers the
	// whole compressed object.
	if _, err := io.Copy(io.Discard, teed); err != nil {
		return err
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != p.TOC.SHA256 {
		return fmt.Errorf("sha256 mismatch on %s: got %s, want %s", p.TOC.Name, got, p.TOC.SHA256)
	}
	return nil
}
