// Command statuspage runs the status page server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "time/tzdata" // time zone database for Windows

	"github.com/szilvasidave/status-page-tool/internal/app"
	"github.com/szilvasidave/status-page-tool/internal/config"
)

// version is set at build time with -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	dataDir := flag.String("data", config.DefaultDataDir(), "data directory (config.json, SQLite database, logs)")
	listen := flag.String("listen", "", "listen address, overrides config.json (e.g. :8080)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, app.Options{DataDir: *dataDir, Listen: *listen, Version: version, Log: log, OpenBrowser: true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
