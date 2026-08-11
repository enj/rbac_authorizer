// Package soapbox is the narrow public entry point of the Soapbox engine.
//
// Template derived repositories depend on this package from a small nested
// tools module that pins an immutable tools/vX.Y.Z engine release. Everything
// else lives under internal/, so the engine can evolve without changing the
// contract a generated repository compiles against.
package soapbox

import (
	"context"
	"io"
	"os"

	"github.com/enj/soapbox/tools/internal/cli"
)

// Exit codes returned by Main and Run.
const (
	// ExitOK reports success.
	ExitOK = cli.ExitOK
	// ExitFailure reports an unexpected runtime failure.
	ExitFailure = cli.ExitFailure
	// ExitUsage reports a malformed command line.
	ExitUsage = cli.ExitUsage
	// ExitCheck reports that the command ran and found policy violations.
	ExitCheck = cli.ExitCheck
	// ExitCanceled reports that the context ended before the command did.
	ExitCanceled = cli.ExitCanceled
)

// Version is the engine version.
const Version = cli.Version

// Main runs one soapbox command line against the process streams and returns
// the process exit code. It never calls os.Exit, so callers stay in control.
func Main(ctx context.Context, args []string) int {
	dir, err := os.Getwd()
	if err != nil {
		dir = ""
	}
	return Run(ctx, args, os.Stdout, os.Stderr, dir)
}

// Run executes one soapbox command line against injected streams and returns
// the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, dir string) int {
	return cli.Run(ctx, cli.Env{Stdout: stdout, Stderr: stderr, Dir: dir}, args)
}
