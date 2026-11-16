// Command beacon is the Beacon server and its command-line tools.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	// Embed the time zone database, so schedules work on hosts and images
	// that do not ship one.
	_ "time/tzdata"

	"github.com/thanoslabs/beacon/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Commands stop cleanly when the context is cancelled by SIGINT or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
}
