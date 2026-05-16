package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
	// Cancel the root context on SIGINT/SIGTERM. Backends propagate the
	// cancellation: in-flight S3 multipart uploads get aborted (with a
	// fresh context, so the abort RPC still reaches the server), download
	// extraction wipes its scratch directory, no half-state is left on
	// either end.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:           "ferry",
		Short:         "Upload and download geth datadir snapshots",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(uploadCmd(), downloadCmd(), inspectCmd(), listCmd(), verifyCmd(), contentsCmd())

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ferry:", err)
		os.Exit(1)
	}
}
