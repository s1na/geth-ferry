package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/download"
	"github.com/s1na/geth-ferry/pkg/legacy"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

func downloadCmd() *cobra.Command {
	var (
		src, dst     string
		force, quiet bool
		parallelN    int
	)
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download a snapshot from a remote into a datadir",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var progressOut io.Writer = os.Stderr
			if quiet {
				progressOut = nil
			}
			if snapshot.IsLegacyURL(src) {
				return runLegacy(ctx, src, dst, force, progressOut)
			}
			return runManifest(ctx, src, dst, force, progressOut, parallelN)
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "snapshot URL: directory for manifest snapshots, or single file (.tar.lz4 / .tar.zst) for legacy (required)")
	cmd.Flags().StringVar(&dst, "dst", "", "datadir to extract into (required)")
	cmd.Flags().BoolVar(&force, "force", false, "extract into a non-empty datadir")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress periodic progress output")
	cmd.Flags().IntVar(&parallelN, "parallel-parts", 1, "number of snapshot parts to download in parallel (1 = sequential)")
	for _, name := range []string{"src", "dst"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func runManifest(ctx context.Context, src, dst string, force bool, progressOut io.Writer, parallelN int) error {
	rootURL, name, err := snapshot.SplitTrailingSegment(src)
	if err != nil {
		return err
	}
	be, prefix, err := backend.Open(rootURL)
	if err != nil {
		return err
	}
	m, err := download.Run(ctx, be, prefix, download.Options{
		DataDir:       dst,
		Name:          name,
		Force:         force,
		Progress:      progressOut,
		ParallelParts: parallelN,
	})
	if err != nil {
		return err
	}
	fmt.Printf("downloaded %s — %d part(s)\n", m.Name, len(m.Parts))
	return nil
}

func runLegacy(ctx context.Context, src, dst string, force bool, progressOut io.Writer) error {
	rootURL, key, err := snapshot.SplitTrailingSegment(src)
	if err != nil {
		return err
	}
	be, prefix, err := backend.Open(rootURL)
	if err != nil {
		return err
	}
	fullKey := path.Join(prefix, key)
	if err := legacy.Download(ctx, be, fullKey, legacy.Options{
		DataDir:  dst,
		Force:    force,
		Progress: progressOut,
	}); err != nil {
		return err
	}
	fmt.Printf("downloaded legacy %s\n", key)
	return nil
}
