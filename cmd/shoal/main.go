// Command shoal is a lightweight container orchestrator. A single binary
// provides the server, the node agent and the CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/internal/runtime/containerd"
)

// version, commit and date are set at build time with -ldflags
// "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// containerd's shim re-executes this binary as the per-container logging
	// driver; handle that before any CLI parsing.
	if len(os.Args) == 3 && os.Args[1] == containerd.LogDriverFlag {
		containerd.RunLogDriver(os.Args[2])
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		// Show the server's message rather than "rpc error: code = ...".
		if st, ok := status.FromError(err); ok {
			fmt.Fprintln(os.Stderr, "error:", st.Message())
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		// Commands such as init return an error that carries its exit code.
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			os.Exit(coded.ExitCode())
		}
		os.Exit(1)
	}
}
