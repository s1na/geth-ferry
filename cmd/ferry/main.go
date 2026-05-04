package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	// Backend implementations register themselves via init().
	_ "github.com/s1na/geth-ferry/pkg/backend/file"
	_ "github.com/s1na/geth-ferry/pkg/backend/s3"
)

const version = "0.1.0"

func main() {
	root := &cobra.Command{
		Use:           "ferry",
		Short:         "Upload and download geth datadir snapshots",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(uploadCmd(), downloadCmd(), inspectCmd(), listCmd(), verifyCmd(), contentsCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ferry:", err)
		os.Exit(1)
	}
}
