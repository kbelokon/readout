package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/version"
	"github.com/kbelokon/readout/internal/web"
)

var listenAndServe = func(srv *http.Server) error {
	return srv.ListenAndServe()
}

// shutdownGrace bounds the graceful drain after SIGINT/SIGTERM: long enough
// for open Live streams to flush their `ro-terminal` "shutdown" frames (the
// app's shutdownCh is the same signal context) before the listener dies.
const shutdownGrace = 5 * time.Second

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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

	// The signal context is the app's shutdown signal (web.New wires its Done
	// channel to every open Live stream's `ro-terminal` "shutdown" frame) AND
	// the trigger for the graceful http.Server.Shutdown below — with a plain
	// context.Background() neither ever fired.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := web.New(ctx, &cfg)
	if err != nil {
		slog.Error("failed to initialize app", "version", version.Version, "error", err)
		return 1
	}
	addr := config.Address(cfg.Port)
	if cfg.MetricsPort != 0 {
		metricsAddr := config.Address(cfg.MetricsPort)
		metricsSrv := newHTTPServer(metricsAddr, app.MetricsHandler())
		go func() {
			slog.Info("readout metrics started", "version", version.Version, "addr", metricsAddr)
			if err := listenAndServe(metricsSrv); err != nil {
				slog.Error("metrics server exited", "error", err)
			}
		}()
	}
	srv := newHTTPServer(addr, app.Handler())
	slog.Info("readout started", "version", version.Version, "addr", addr)
	errCh := make(chan error, 1)
	go func() { errCh <- listenAndServe(srv) }()
	select {
	case err := <-errCh:
		// ErrServerClosed only follows a Shutdown call, which lives in the
		// other branch — any error here is a real listen/serve failure.
		if err != nil {
			slog.Error("server exited", "error", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		slog.Info("readout shutting down", "grace", shutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
			return 1
		}
		// Shutdown unblocked ListenAndServe with ErrServerClosed; drain it so
		// the serve goroutine never leaks.
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server exited", "error", err)
			return 1
		}
		return 0
	}
}
