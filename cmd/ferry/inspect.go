package main

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <local-manifest-or-snapshot-url>",
		Short: "Print a snapshot's manifest.json without touching its parts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadManifest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(m)
		},
	}
}

// loadManifest accepts either a local filesystem path (a manifest.json or a
// snapshot directory containing one) or a remote URL pointing at a snapshot
// directory.
func loadManifest(ctx context.Context, ref string) (*snapshot.Manifest, error) {
	if !snapshot.IsURL(ref) {
		return loadManifestLocal(ref)
	}
	rootURL, name, err := snapshot.SplitTrailingSegment(ref)
	if err != nil {
		return nil, err
	}
	be, prefix, err := backend.Open(rootURL)
	if err != nil {
		return nil, err
	}
	key := path.Join(prefix, name, snapshot.ManifestFilename)
	r, err := be.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return snapshot.Decode(r)
}

func loadManifestLocal(p string) (*snapshot.Manifest, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		p = filepath.Join(p, snapshot.ManifestFilename)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return snapshot.Decode(f)
}
