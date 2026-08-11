// Command soapbox runs the Soapbox engine.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	soapbox "github.com/enj/soapbox/tools"
)

func main() {
	os.Exit(run())
}

// run keeps the signal handler teardown ahead of the process exit.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return soapbox.Main(ctx, os.Args[1:])
}
