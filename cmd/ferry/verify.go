package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

func verifyCmd() *cobra.Command {
	var (
		src   string
		quick bool
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check snapshot parts against manifest (deep sha256 by default, --quick for size-only)",
		Long: `Default mode re-fetches each part and recomputes the sha256, costing
one full-snapshot-worth of egress per run.

--quick HEADs each part instead and compares the remote object's size
against the manifest's compressed_size. Catches "part missing or
truncated" cheaply (a few kilobytes of metadata per part); does not
catch silent corruption; only the deep mode does that.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
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
			mode := "deep"
			if quick {
				mode = "quick"
			}
			fmt.Fprintf(out, "verifying %s: %d part(s), %s mode\n", m.Name, len(m.Parts), mode)
			var failed []string
			for _, p := range m.Parts {
				partKey := path.Join(prefix, name, p.Name)
				if quick {
					obj, err := be.Stat(ctx, partKey)
					if err != nil {
						fmt.Fprintf(out, "  %s  ERROR: %v\n", p.Name, err)
						failed = append(failed, p.Name)
						continue
					}
					if obj.Size != p.CompressedSize {
						fmt.Fprintf(out, "  %s  SIZE MISMATCH (got %d, want %d)\n",
							p.Name, obj.Size, p.CompressedSize)
						failed = append(failed, p.Name)
						continue
					}
					fmt.Fprintf(out, "  %s  OK (size %d)\n", p.Name, obj.Size)
					continue
				}
				got, err := hashPart(ctx, be, partKey)
				if err != nil {
					fmt.Fprintf(out, "  %s  ERROR: %v\n", p.Name, err)
					failed = append(failed, p.Name)
					continue
				}
				if got != p.SHA256 {
					fmt.Fprintf(out, "  %s  MISMATCH (got %s, want %s)\n", p.Name, got, p.SHA256)
					failed = append(failed, p.Name)
					continue
				}
				fmt.Fprintf(out, "  %s  OK (%s)\n", p.Name, got)
			}
			if len(failed) > 0 {
				return fmt.Errorf("verification failed: %d/%d parts bad: %s",
					len(failed), len(m.Parts), strings.Join(failed, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "snapshot URL (required)")
	cmd.Flags().BoolVar(&quick, "quick", false, "HEAD-only size check; skip the full sha256 re-download")
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
