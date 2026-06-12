package config

import "testing"

// TestNoAuthNoListenAddressDefaultsLoopback proves the safe default: a no-auth
// config (auth.mode defaults to none) with no explicit listenAddress binds the
// loopback host, so a default binary does not serve unauthenticated cluster
// data on every interface. Address() then yields a loopback-pinned address.
func TestNoAuthNoListenAddressDefaultsLoopback(t *testing.T) {
	path := writeConfig(t, "port: 8080\n")
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModeNone {
		t.Fatalf("expected default auth mode none, got %q", cfg.AuthMode)
	}
	if cfg.ListenAddress != "127.0.0.1" {
		t.Fatalf("no-auth + no listenAddress should default to loopback, got %q", cfg.ListenAddress)
	}
	if got := Address(cfg.ListenAddress, cfg.Port); got != "127.0.0.1:8080" {
		t.Fatalf("Address = %q, want 127.0.0.1:8080", got)
	}
	if !cfg.EnforceLoopbackHostAllowlist() {
		t.Fatalf("loopback no-auth bind must enforce the Host allowlist")
	}
}

// TestExplicitListenAddressHonored proves an explicit listenAddress always wins
// over the loopback default, even under no-auth, and is threaded into Address.
func TestExplicitListenAddressHonored(t *testing.T) {
	path := writeConfig(t, "listenAddress: 0.0.0.0\nport: 9090\n")
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "0.0.0.0" {
		t.Fatalf("explicit listenAddress must win, got %q", cfg.ListenAddress)
	}
	if got := Address(cfg.ListenAddress, cfg.Port); got != "0.0.0.0:9090" {
		t.Fatalf("Address = %q, want 0.0.0.0:9090", got)
	}
	// Non-loopback bind => no Host allowlist (operator reaches by real name).
	if cfg.EnforceLoopbackHostAllowlist() {
		t.Fatalf("non-loopback bind must not enforce the Host allowlist")
	}
}

// TestNonNoAuthEmptyListenKeepsAllInterfaces proves the loopback default is
// scoped to no-auth: with auth enabled an empty listenAddress keeps the
// historical all-interfaces bind (":port"). This is not a startup failure.
func TestNonNoAuthEmptyListenKeepsAllInterfaces(t *testing.T) {
	path := writeConfig(t, "port: 8080\nauth:\n  mode: headers\n")
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "" {
		t.Fatalf("auth-enabled empty listenAddress should stay empty, got %q", cfg.ListenAddress)
	}
	if got := Address(cfg.ListenAddress, cfg.Port); got != ":8080" {
		t.Fatalf("Address = %q, want :8080", got)
	}
	if cfg.EnforceLoopbackHostAllowlist() {
		t.Fatalf("auth-enabled bind must not enforce the Host allowlist")
	}
}

// TestNoAuthExplicitNonLoopbackStartsNoError proves binding a network address
// under no-auth is allowed (no startup gate): Parse succeeds and the address is
// the operator's, with no Host allowlist layered on.
func TestNoAuthExplicitNonLoopbackStartsNoError(t *testing.T) {
	path := writeConfig(t, "listenAddress: 10.0.0.5\nport: 8080\n")
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatalf("no-auth + explicit non-loopback must NOT error: %v", err)
	}
	if cfg.ListenAddress != "10.0.0.5" {
		t.Fatalf("explicit listenAddress lost, got %q", cfg.ListenAddress)
	}
	if cfg.EnforceLoopbackHostAllowlist() {
		t.Fatalf("non-loopback no-auth bind must not enforce the Host allowlist")
	}
}

func TestAddressJoinsHostAndPort(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"", 8080, ":8080"},
		{"127.0.0.1", 8080, "127.0.0.1:8080"},
		{"::1", 8080, "[::1]:8080"},
		{"0.0.0.0", 9090, "0.0.0.0:9090"},
	}
	for _, c := range cases {
		if got := Address(c.host, c.port); got != c.want {
			t.Fatalf("Address(%q,%d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loop := []string{"127.0.0.1", "127.0.0.2", "::1", "localhost"}
	for _, h := range loop {
		if !IsLoopbackHost(h) {
			t.Fatalf("IsLoopbackHost(%q) = false, want true", h)
		}
	}
	notLoop := []string{"", "0.0.0.0", "10.0.0.5", "example.com", "::"}
	for _, h := range notLoop {
		if IsLoopbackHost(h) {
			t.Fatalf("IsLoopbackHost(%q) = true, want false", h)
		}
	}
}

func TestAllowedHost(t *testing.T) {
	ok := []string{"localhost", "localhost:8080", "127.0.0.1", "127.0.0.1:8080", "[::1]", "[::1]:8080", "LOCALHOST"}
	for _, h := range ok {
		if !AllowedHost(h) {
			t.Fatalf("AllowedHost(%q) = false, want true", h)
		}
	}
	bad := []string{"evil.com", "evil.com:8080", "10.0.0.5", "example.com:443", ""}
	for _, h := range bad {
		if AllowedHost(h) {
			t.Fatalf("AllowedHost(%q) = true, want false", h)
		}
	}
}
