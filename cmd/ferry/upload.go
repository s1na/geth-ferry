package main

import (
	"context"
	"fmt"
	"io"
	"os"
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
		level, threads int
		force, quiet   bool
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
	for _, name := range []string{"src", "dst", "role"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}
