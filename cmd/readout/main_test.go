package main

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeConfig writes content to a temp readout.yaml and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "readout.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunVersionAndParseError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// --version still loads --config first (the file read precedes the version
	// exit), so point it at a valid file and assert exit 0 + the version line.
	cfgPath := writeConfig(t, "port: 8080\n")
	if code := run([]string{"--config", cfgPath, "--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "readout") {
		t.Fatalf("version output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	badCfg := writeConfig(t, "auth:\n  mode: bogus\n")
	if code := run([]string{"--config", badCfg}, &stdout, &stderr); code != 2 {
		t.Fatalf("parse error exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid auth mode") {
		t.Fatalf("parse error stderr = %q", stderr.String())
	}
}

func TestRunServerInitAndListenErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	initCfg := writeConfig(t, "kubeconfigPath: /path/that/does/not/exist\n")
	if code := run([]string{"--config", initCfg}, &stdout, &stderr); code != 1 {
		t.Fatalf("init error exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "failed to initialize app") {
		t.Fatalf("init error stderr = %q", stderr.String())
	}

	oldListenAndServe := listenAndServe
	t.Cleanup(func() { listenAndServe = oldListenAndServe })
	assertTimeouts := func(t *testing.T, srv *http.Server) {
		t.Helper()
		if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 30*time.Second || srv.IdleTimeout != 120*time.Second {
			t.Fatalf("server timeouts = header %v read %v idle %v, want 10s/30s/120s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
		}
		if srv.WriteTimeout != 0 {
			t.Fatalf("WriteTimeout = %v, want unset", srv.WriteTimeout)
		}
	}
	listenAndServe = func(srv *http.Server) error {
		if srv.Addr != "127.0.0.1:9091" {
			t.Fatalf("addr = %q, want 127.0.0.1:9091", srv.Addr)
		}
		if srv.Handler == nil {
			t.Fatal("handler is nil")
		}
		assertTimeouts(t, srv)
		return errors.New("listen failed")
	}
	stdout.Reset()
	stderr.Reset()
	listenCfg := writeConfig(t, "clusters:\n  - name: test\n    server: https://example.invalid\n")
	if code := run([]string{"--config", listenCfg, "--port", "9091"}, &stdout, &stderr); code != 1 {
		t.Fatalf("listen error exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "server exited") {
		t.Fatalf("listen error stderr = %q", stderr.String())
	}

	servers := make(chan *http.Server, 2)
	listenAndServe = func(srv *http.Server) error {
		servers <- srv
		return errors.New("listen failed")
	}
	stdout.Reset()
	stderr.Reset()
	metricsCfg := writeConfig(t, "metricsPort: 9092\nclusters:\n  - name: test\n    server: https://example.invalid\n")
	if code := run([]string{"--config", metricsCfg, "--port", "9091"}, &stdout, &stderr); code != 1 {
		t.Fatalf("metrics listen exit code = %d, want 1", code)
	}
	seen := map[string]*http.Server{}
	for len(seen) < 2 {
		select {
		case srv := <-servers:
			seen[srv.Addr] = srv
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for main and metrics servers, saw %v", seen)
		}
	}
	for _, addr := range []string{"127.0.0.1:9091", "127.0.0.1:9092"} {
		srv := seen[addr]
		if srv == nil {
			t.Fatalf("server %s was not started; saw %v", addr, seen)
		}
		if srv.Handler == nil {
			t.Fatalf("server %s handler is nil", addr)
		}
		assertTimeouts(t, srv)
	}
	rec := httptest.NewRecorder()
	seen["127.0.0.1:9092"].Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# HELP readout_up") {
		t.Fatalf("metrics server handler status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRunGracefulShutdownOnSignal pins the shutdown terminal's reachability
// (waves E+F review): run() must catch SIGTERM/SIGINT and drive
// http.Server.Shutdown — with a plain context.Background() the app's
// shutdownCh never fires, open Live streams never get their `ro-terminal`
// "shutdown" frame, and the process dies mid-write. The listenAndServe seam
// serves a real listener on an ephemeral port so Shutdown's listener close is
// observable as Serve returning (run exiting 0). A REAL signal is sent to the
// test process: run() arms signal.NotifyContext before listenAndServe runs,
// so the started channel ordering guarantees the handler is installed (an
// unhandled SIGTERM would kill the test binary — the pre-fix failure shape).
func TestRunGracefulShutdownOnSignal(t *testing.T) {
	oldListenAndServe := listenAndServe
	t.Cleanup(func() { listenAndServe = oldListenAndServe })
	started := make(chan struct{}, 1)
	listenAndServe = func(srv *http.Server) error {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		started <- struct{}{}
		return srv.Serve(ln)
	}

	cfgPath := writeConfig(t, "clusters:\n  - name: test\n    server: https://example.invalid\n")
	var stdout, stderr bytes.Buffer
	codes := make(chan int, 1)
	go func() { codes <- run([]string{"--config", cfgPath, "--port", "9093"}, &stdout, &stderr) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server never started")
	}
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-codes:
		if code != 0 {
			t.Fatalf("graceful shutdown exit code = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after SIGTERM — graceful shutdown never engaged")
	}
}
