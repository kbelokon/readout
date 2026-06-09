package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/version"
	"github.com/kbelokon/readout/internal/web"
)

var listenAndServe = func(srv *http.Server) error {
	return srv.ListenAndServe()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.ShowVersion {
		_, _ = fmt.Fprintf(stdout, "readout %s\n", version.Version)
		return 0
	}
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level})))

	ctx := context.Background()
	app, err := web.New(ctx, &cfg)
	if err != nil {
		slog.Error("failed to initialize app", "version", version.Version, "error", err)
		return 1
	}
	addr := config.Address(cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	slog.Info("readout started", "version", version.Version, "addr", addr)
	if err := listenAndServe(srv); err != nil {
		slog.Error("server exited", "error", err)
		return 1
	}
	return 0
}
