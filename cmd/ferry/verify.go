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
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

func verifyCmd() *cobra.Command {
	var src string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Re-fetch each part and check sha256 against manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			rootURL, name, err := splitTrailingSegment(src)
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
			fmt.Fprintf(out, "verifying %s — %d part(s)\n", m.Name, len(m.Parts))
			ok := true
			for _, p := range m.Parts {
				partKey := path.Join(prefix, name, p.Name)
				got, err := hashPart(ctx, be, partKey)
				if err != nil {
					fmt.Fprintf(out, "  %s  ERROR: %v\n", p.Name, err)
					ok = false
					continue
				}
				if got != p.SHA256 {
					fmt.Fprintf(out, "  %s  MISMATCH (got %s, want %s)\n", p.Name, got, p.SHA256)
					ok = false
					continue
				}
				fmt.Fprintf(out, "  %s  OK (%s)\n", p.Name, got)
			}
			if !ok {
				return fmt.Errorf("verification failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "snapshot URL (required)")
	_ = cmd.MarkFlagRequired("src")
	return cmd
}

func hashPart(ctx context.Context, be backend.Backend, key string) (string, error) {
	r, err := be.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
