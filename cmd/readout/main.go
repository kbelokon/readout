package main

import (
	"context"
	"errors"
	"flag"
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

// configUsage is printed to stderr when `config` is invoked bare or with an
// unknown sub-subcommand. It names the only supported action and the
// environment-overlay caveat so the operator knows validate is faithful to a
// real startup, not a pure file-shape check.
const configUsage = `usage: readout config validate [--config <path>]

validate loads the config exactly as startup would: strict parsing, semantic
checks, and the same READOUT_* environment overlay and referenced-file
resolution. Environment variables set in the calling shell therefore affect the
result. It performs no cluster or network calls.
`

// runConfig dispatches the `config` subcommand. Only `validate` exists; a bare
// `config` or an unknown action prints usage to stderr and exits 2.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "validate":
		return runConfigValidate(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown config subcommand %q\n\n%s", args[0], configUsage)
		return 2
	}
}

// runConfigValidate loads the config through the same loader startup uses and
// reports whether it would pass. Exit 0 + "config OK" on success; exit 1 with
// the loader's own error text on failure -- byte-identical to what startup
// would print. No network or cluster access happens: config loading is purely
// local, and OIDC discovery is deferred to request time in the auth layer.
func runConfigValidate(args []string, stdout, stderr io.Writer) int {
	var configPath string
	fs := flag.NewFlagSet("readout config validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", "", "Path to readout.yaml config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected argument %q\n\n%s", fs.Arg(0), configUsage)
		return 2
	}

	// Reconstruct the bootstrap arg shape config.Parse expects so the identical
	// loader (strict parse, env overlay, file-field precedence, semantic checks)
	// runs -- the validate result is faithful to startup by construction.
	var loadArgs []string
	if configPath != "" {
		loadArgs = []string{"--config", configPath}
	}
	if _, err := config.Parse(loadArgs); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "config OK")
	return 0
}

func run(args []string, stdout, stderr io.Writer) int {
	// The `config` keyword routes to the offline subcommand handler before the
	// normal start path parses bootstrap flags, so `config validate` never falls
	// through to server startup.
	if len(args) > 0 && args[0] == "config" {
		return runConfig(args[1:], stdout, stderr)
	}
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
