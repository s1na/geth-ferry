package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

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
	)
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download a snapshot from a remote into a datadir",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			var progressOut io.Writer = os.Stderr
			if quiet {
				progressOut = nil
			}
			if snapshot.IsLegacyURL(src) {
				return runLegacy(ctx, src, dst, force, progressOut)
			}
			return runManifest(ctx, src, dst, force, progressOut)
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "snapshot URL: directory for manifest snapshots, or single file (.tar.lz4 / .tar.zst) for legacy (required)")
	cmd.Flags().StringVar(&dst, "dst", "", "datadir to extract into (required)")
	cmd.Flags().BoolVar(&force, "force", false, "extract into a non-empty datadir")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress periodic progress output")
	for _, name := range []string{"src", "dst"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func runManifest(ctx context.Context, src, dst string, force bool, progressOut io.Writer) error {
	rootURL, name, err := splitTrailingSegment(src)
	if err != nil {
		return err
	}
	be, prefix, err := backend.Open(rootURL)
	if err != nil {
		return err
	}
	m, err := download.Run(ctx, be, prefix, download.Options{
		DataDir:  dst,
		Name:     name,
		Force:    force,
		Progress: progressOut,
	})
	if err != nil {
		return err
	}
	fmt.Printf("downloaded %s — %d part(s)\n", m.Name, len(m.Parts))
	return nil
}

func runLegacy(ctx context.Context, src, dst string, force bool, progressOut io.Writer) error {
	rootURL, key, err := splitTrailingSegment(src)
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

// splitTrailingSegment splits a URL like
//
//	scheme://host/parent/name
//
// into ("scheme://host/parent", "name"). Used for both manifest snapshots
// (where "name" is the snapshot directory) and legacy single files (where
// "name" is the .tar.{zst,lz4} object).
func splitTrailingSegment(s string) (string, string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", "", err
	}
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed == "" {
		return "", "", fmt.Errorf("URL %q has no path", s)
	}
	parent, name := path.Split(trimmed)
	if name == "" {
		return "", "", fmt.Errorf("URL %q has no trailing name", s)
	}
	u.Path = strings.TrimRight(parent, "/")
	return u.String(), name, nil
}
