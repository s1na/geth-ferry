package main

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/progress"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// listEntry is the structured form of one row in `ferry list`. Used for
// JSON output; the text formatter renders it as columns.
type listEntry struct {
	Name string `json:"name"`
	// ChainID / Role / Block are populated when the name parses (i.e.
	// matches the geth-<chain>-<role>-<block>[-<ts>] shape).
	ChainID uint64 `json:"chain_id,omitempty"`
	Role    string `json:"role,omitempty"`
	Block   uint64 `json:"block,omitempty"`
	// Timestamp is the snapshot's creation time as Unix seconds. Sourced
	// from the name's trailing -<unix-seconds> if present (legacy form);
	// otherwise from the manifest object's modification time as a
	// best-effort approximation. Zero only when neither is available.
	Timestamp int64 `json:"timestamp,omitempty"`
	TotalSize int64 `json:"total_size"` // sum of object sizes under the snapshot prefix
}

func listCmd() *cobra.Command {
	var (
		src     string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List snapshots under a remote prefix",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be, prefix, err := backend.Open(src)
			if err != nil {
				return err
			}
			objs, err := be.List(ctx, prefix)
			if err != nil {
				return err
			}

			entries := groupSnapshots(objs, prefix)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			defer tw.Flush()
			fmt.Fprintln(tw, "NAME\tCHAIN\tROLE\tBLOCK\tDATE\tSIZE")
			for _, e := range entries {
				// An unparseable name leaves Role empty; render dashes
				// for everything except name + size in that case.
				if e.Role == "" {
					fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t%s\n", e.Name, progress.HumanBytes(e.TotalSize))
					continue
				}
				date := "-"
				if e.Timestamp != 0 {
					date = time.Unix(e.Timestamp, 0).UTC().Format("2006-01-02")
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n",
					e.Name, e.ChainID, e.Role, e.Block,
					date,
					progress.HumanBytes(e.TotalSize))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "remote prefix URL (required)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of tab-aligned columns")
	_ = cmd.MarkFlagRequired("src")
	return cmd
}

// groupSnapshots reduces the flat object list into one row per snapshot.
// A "snapshot" is identified by the presence of a manifest.json under
// <prefix>/<name>/; objects under <prefix>/<name>/parts/* contribute to
// that row's TotalSize.
func groupSnapshots(objs []backend.Object, prefix string) []listEntry {
	byName := map[string]*listEntry{}

	// Pass 1: discover snapshot names via manifest.json sightings.
	for _, o := range objs {
		rel := strings.TrimPrefix(o.Key, prefix)
		if path.Base(rel) != snapshot.ManifestFilename {
			continue
		}
		name := path.Base(path.Dir(rel))
		if name == "" || name == "." || name == "/" {
			continue
		}
		e := &listEntry{Name: name}
		if meta, err := snapshot.ParseName(name); err == nil {
			e.ChainID = meta.ChainID
			e.Role = string(meta.Role)
			e.Block = meta.Block
			e.Timestamp = meta.Timestamp
		}
		// Fall back to the manifest object's mtime when the name doesn't
		// carry a timestamp (new 4-part naming). Best-effort: not every
		// backend populates ModTime.
		if e.Timestamp == 0 && !o.ModTime.IsZero() {
			e.Timestamp = o.ModTime.Unix()
		}
		byName[name] = e
	}

	// Pass 2: attribute object sizes back to the discovered snapshots.
	// Object keys look like "<prefix><name>/manifest.json" or
	// "<prefix><name>/parts/<file>"; the first slash-segment after the
	// prefix is the snapshot name.
	for _, o := range objs {
		rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, prefix), "/")
		name, _, found := strings.Cut(rel, "/")
		if !found || name == "" {
			continue
		}
		if e, ok := byName[name]; ok {
			e.TotalSize += o.Size
		}
	}

	out := make([]listEntry, 0, len(byName))
	for _, e := range byName {
		out = append(out, *e)
	}
	return out
}
