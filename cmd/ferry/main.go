package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	// Backend implementations register themselves via init().
	_ "github.com/s1na/geth-ferry/pkg/backend/file"
	_ "github.com/s1na/geth-ferry/pkg/backend/s3"
)

// version is the binary's reported version, used both by `ferry --version`
// and as the `created_by` field on uploaded manifests. Override at build
// time with `-ldflags "-X main.version=<v>"`; defaults to "dev" so a plain
// `go build` produces something obviously not a release.
var version = "dev"

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
