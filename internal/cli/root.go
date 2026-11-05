// Package cli wires the pathgauge subcommands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/agoodley/pathgauge/internal/version"
)

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		var exit exitError
		if errors.As(err, &exit) {
			return exit.code
		}
		return 1
	}
	return 0
}

// exitError carries a specific exit code out of a command.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pathgauge",
		Short: "Two-ended network diagnostics: throughput, latency, path, bufferbloat and more",
		Long: `PathGauge measures the network path between two Linux machines.

Install it on both ends: one runs the server (dashboard, API, scheduler),
the other runs an agent. Use "pathgauge doctor" on each box to check it
is ready for testing.

Only test networks you are authorised to test. Throughput tests use a lot
of data and can disrupt other traffic while they run.`,
		Version:       version.Get().Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		newVersionCmd(),
		newDoctorCmd(),
		newServerCmd(),
		newAgentCmd(),
		newEndpointCmd(),
		newTestCmd(),
		newReportCmd(),
		newEnrichCmd(),
		newTuneCmd(),
	)
	return root
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
