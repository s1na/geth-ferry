package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/s1na/geth-ferry/pkg/backend"
	"github.com/s1na/geth-ferry/pkg/snapshot"
)

func listCmd() *cobra.Command {
	var src string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List snapshots under a remote prefix",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			be, prefix, err := backend.Open(src)
			if err != nil {
				return err
			}
			objs, err := be.List(ctx, prefix)
			if err != nil {
				return err
			}

			// Find manifest.json files and group by their containing snapshot
			// directory. The snapshot name is the path component immediately
			// before the manifest file.
			type entry struct {
				name      string
				totalSize int64
			}
			byName := map[string]*entry{}
			for _, o := range objs {
				rel := strings.TrimPrefix(o.Key, prefix)
				if path.Base(rel) != snapshot.ManifestFilename {
					continue
				}
				name := path.Base(path.Dir(rel))
				if name == "" || name == "." || name == "/" {
					continue
				}
				byName[name] = &entry{name: name}
			}
			// Second pass: sum part sizes per snapshot.
			for _, o := range objs {
				rel := strings.TrimPrefix(o.Key, prefix)
				dir, _ := path.Split(strings.TrimPrefix(rel, "/"))
				dir = strings.TrimSuffix(dir, "/")
				// dir might be "name" or "name/parts" — strip trailing parts/.
				name := dir
				if i := strings.LastIndex(dir, "/"); i >= 0 {
					name = dir[:i]
				}
				if e, ok := byName[name]; ok {
					e.totalSize += o.Size
				}
			}

			names := make([]string, 0, len(byName))
			for n := range byName {
				names = append(names, n)
			}
			sort.Strings(names)

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			defer tw.Flush()
			fmt.Fprintln(tw, "NAME\tCHAIN\tROLE\tBLOCK\tDATE\tSIZE")
			for _, n := range names {
				e := byName[n]
				meta, err := snapshot.ParseName(n)
				if err != nil {
					fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t%s\n", n, humanBytes(e.totalSize))
					continue
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n",
					n, meta.ChainID, meta.Role, meta.Block,
					time.Unix(meta.Timestamp, 0).UTC().Format("2006-01-02"), humanBytes(e.totalSize))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "remote prefix URL (required)")
	_ = cmd.MarkFlagRequired("src")
	return cmd
}

func humanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = KiB * 1024
		GiB = MiB * 1024
		TiB = GiB * 1024
	)
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(TiB))
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
