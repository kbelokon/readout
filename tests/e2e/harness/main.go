// Command harness is the standalone e2e fixture: it starts the fakeapi fake
// apiserver on a fixed port, writes a kubeconfig file pointing at it, and
// launches the BUILT readout binary against that kubeconfig via the KUBECONFIG
// environment variable -- readout has no --kubeconfig flag (config.Parse
// registers only config/port/debug/version) and the kube loader's default
// clientcmd loading rules honor KUBECONFIG when no explicit cluster source is
// configured.
//
// Configuration (environment, all optional):
//
//	READOUT_E2E_PORT  port readout listens on            (default 8090)
//	FAKEAPI_E2E_PORT  port the fake apiserver listens on (default 8091)
//	READOUT_BIN       path to the built readout binary   (default ../../readout,
//	                  i.e. the repo-root build relative to tests/e2e)
//
// The Playwright suite starts this harness through its webServer block and
// drives the fixture state via http://127.0.0.1:$FAKEAPI_E2E_PORT/__control/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	fakeapi "github.com/kbelokon/readout/internal/fakekube"
)

const kubeconfigTemplate = `apiVersion: v1
kind: Config
clusters:
- name: e2e
  cluster:
    server: %s
contexts:
- name: e2e
  context:
    cluster: e2e
    user: e2e
current-context: e2e
users:
- name: e2e
  user: {}
`

// readoutConfig enables the surfaces the suite exercises beyond the zero
// config: container logs (the /logs page renders the disabled notice without
// it; fakeapi serves the pod-log fixture either way).
const readoutConfig = `showContainerLogs: true
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	readoutPort := envInt("READOUT_E2E_PORT", 8090)
	fakeapiPort := envInt("FAKEAPI_E2E_PORT", 8091)

	binary, err := readoutBinary()
	if err != nil {
		return err
	}

	fake, err := fakeapi.New(fakeapi.WithListenAddress(fmt.Sprintf("127.0.0.1:%d", fakeapiPort)))
	if err != nil {
		return err
	}
	defer fake.Close()
	log.Printf("fakeapi listening on %s (control surface at %s/__control/)", fake.URL, fake.URL)

	kubeconfig, configPath, cleanup, err := writeConfigs(fake.URL)
	if err != nil {
		return err
	}
	defer cleanup()

	// SIGINT/SIGTERM (Playwright teardown) cancels the context, which tears
	// the readout child down via Cancel/WaitDelay below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, binary, "--port", strconv.Itoa(readoutPort), "--config", configPath)
	// Duplicate env keys resolve last-wins, so an inherited KUBECONFIG is
	// overridden by the generated one.
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	log.Printf("launching %s --port %d with KUBECONFIG=%s", binary, readoutPort, kubeconfig)
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil // clean signalled shutdown
	}
	return err
}

// readoutBinary resolves the built readout binary and fails with a build hint
// when it is missing, so a bare `npx playwright test` points at the fix.
func readoutBinary() (string, error) {
	path := os.Getenv("READOUT_BIN")
	if path == "" {
		path = filepath.Join("..", "..", "readout")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("readout binary not found at %s -- run `make e2e` (or `go build -o readout ./cmd/readout` at the repo root): %w", abs, err)
	}
	return abs, nil
}

// writeConfigs renders the harness file pair into one temp dir: a
// single-context kubeconfig pointing at the fake apiserver (the context name
// becomes readout's cluster name, so the suite navigates to /clusters/e2e) and
// the readout.yaml the binary is launched with (--config).
func writeConfigs(serverURL string) (kubeconfig, configPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "readout-e2e-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	kubeconfig = filepath.Join(dir, "kubeconfig")
	content := fmt.Sprintf(kubeconfigTemplate, serverURL)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		cleanup()
		return "", "", nil, err
	}
	configPath = filepath.Join(dir, "readout.yaml")
	if err := os.WriteFile(configPath, []byte(readoutConfig), 0o600); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return kubeconfig, configPath, cleanup, nil
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		log.Fatal(errors.New(name + " must be a positive integer, got " + value))
	}
	return parsed
}
