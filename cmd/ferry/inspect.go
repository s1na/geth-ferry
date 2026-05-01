package main

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"strings"

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
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			m, err := loadManifest(ctx, args[0])
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
	if !looksLikeURL(ref) {
		return loadManifestLocal(ref)
	}
	rootURL, name, err := splitTrailingSegment(ref)
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
		p = path.Join(p, snapshot.ManifestFilename)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return snapshot.Decode(f)
}

func looksLikeURL(s string) bool {
	for _, scheme := range []string{"file://", "s3://", "http://", "https://"} {
		if strings.HasPrefix(s, scheme) {
			return true
		}
	}
	return false
}
