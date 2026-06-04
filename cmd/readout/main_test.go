package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	listenAndServe = func(addr string, handler http.Handler) error {
		if addr != ":9091" {
			t.Fatalf("addr = %q, want :9091", addr)
		}
		if handler == nil {
			t.Fatal("handler is nil")
		}
		return errors.New("listen failed")
	}
	stdout.Reset()
	stderr.Reset()
	listenCfg := writeConfig(t, "clusters:\n  - name: test\n    url: https://example.invalid\n")
	if code := run([]string{"--config", listenCfg, "--port", "9091"}, &stdout, &stderr); code != 1 {
		t.Fatalf("listen error exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "server exited") {
		t.Fatalf("listen error stderr = %q", stderr.String())
	}
}
