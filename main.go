// Command vid-scraper finds and downloads the videos linked from a webpage.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/shearn89/vid-scraper/internal/cli"
)

func main() {
	// Cancelling on interrupt lets in-flight downloads stop cleanly, leaving
	// their .part files in place so the next run resumes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
