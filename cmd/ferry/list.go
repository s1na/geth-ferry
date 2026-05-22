package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/progress"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

// listEntry is the structured form of one row in `ferry list`. Fields
// other than Name and TotalSize come from the manifest.json (fetched
// per snapshot). When a manifest fails to fetch or decode, those fields
// stay zero/empty and the text formatter renders dashes.
type listEntry struct {
	Name        string `json:"name"`
	ChainID     uint64 `json:"chain_id,omitempty"`
	Role        string `json:"role,omitempty"`
	Block       uint64 `json:"block,omitempty"`
	StateScheme string `json:"state_scheme,omitempty"`
	Timestamp   int64  `json:"timestamp,omitempty"` // manifest.created_at, Unix seconds
	TotalSize   int64  `json:"total_size"`          // sum of object sizes under the snapshot prefix
}

// manifestFetchConcurrency bounds how many manifest GETs are in flight
// at once during `ferry list`. Each manifest is ~2 KiB, so the bottleneck
// is round-trip latency rather than bandwidth — moderate parallelism is
// fine even from a workstation.
const manifestFetchConcurrency = 8

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
			fetchManifests(ctx, be, prefix, entries)
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
				// No manifest info → dashes in everything except name + size.
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
// that row's TotalSize. Per-snapshot metadata (chain/role/block/date)
// is filled in by fetchManifests on the returned slice.
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
		byName[name] = &listEntry{Name: name}
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

// fetchManifests populates ChainID/Role/Block/StateScheme/Timestamp on
// each entry by fetching <prefix>/<name>/manifest.json from the backend.
// Snapshots whose manifest fails to fetch or decode keep the zero values
// and get rendered as dashes by the text formatter; this matches the
// graceful-degradation behavior the previous name-parsing path had.
//
// Bounded parallelism (manifestFetchConcurrency); cancels on context.
func fetchManifests(ctx context.Context, be backend.Backend, prefix string, entries []listEntry) {
	sem := make(chan struct{}, manifestFetchConcurrency)
	var wg sync.WaitGroup
	for i := range entries {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			m, err := fetchManifest(ctx, be, prefix, entries[i].Name)
			if err != nil {
				return // leave zero values; renderer shows dashes
			}
			entries[i].ChainID = m.ChainID
			entries[i].Role = string(m.Role)
			entries[i].Block = m.Head.Block
			entries[i].StateScheme = string(m.StateScheme)
			entries[i].Timestamp = m.CreatedAt
		}(i)
	}
	wg.Wait()
}

func fetchManifest(ctx context.Context, be backend.Backend, prefix, name string) (*snapshot.Manifest, error) {
	key := path.Join(prefix, name, snapshot.ManifestFilename)
	r, err := be.Get(ctx, key)
	if err != nil {
		// ErrNotExist shouldn't happen (groupSnapshots only listed names
		// whose manifest.json appeared in the bucket listing), but stay
		// graceful in case the bucket changed between list and get.
		if errors.Is(err, backend.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("get manifest for %s: %w", name, err)
	}
	defer r.Close()
	return snapshot.Decode(r)
}
