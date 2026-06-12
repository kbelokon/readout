package kube

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
)

// stubResolver installs a fake DNS resolver mapping host -> IPs for the duration
// of a test and restores the real one on cleanup, so cluster-URL validation does
// not depend on real network DNS.
func stubResolver(t *testing.T, table map[string][]string) {
	t.Helper()
	prev := clusterHostResolver
	clusterHostResolver = func(_ context.Context, host string) ([]net.IPAddr, error) {
		ips, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		addrs := make([]net.IPAddr, 0, len(ips))
		for _, s := range ips {
			addrs = append(addrs, net.IPAddr{IP: net.ParseIP(s)})
		}
		return addrs, nil
	}
	t.Cleanup(func() { clusterHostResolver = prev })
}

// TestValidateClusterServerURL_LiteralIPs pins the literal-IP policy: loopback and
// link-local/metadata are rejected; RFC1918/private is ACCEPTED (real apiservers
// live there -- this is where the policy DIFFERS from the hook validator).
func TestValidateClusterServerURL_LiteralIPs(t *testing.T) {
	cases := []struct {
		name    string
		server  string
		wantErr bool
	}{
		{"loopback v4", "https://127.0.0.1:6443", true},
		{"loopback v4 range", "https://127.5.5.5:6443", true},
		{"loopback v6", "https://[::1]:6443", true},
		{"metadata", "https://169.254.169.254", true},
		{"link-local v4", "https://169.254.0.1:6443", true},
		{"link-local v6", "https://[fe80::1]:6443", true},
		{"rfc1918 10.x ACCEPTED", "https://10.0.0.1:6443", false},
		{"rfc1918 172.16 ACCEPTED", "https://172.16.0.1:6443", false},
		{"rfc1918 192.168 ACCEPTED", "https://192.168.1.1:6443", false},
		{"public ACCEPTED", "https://203.0.113.10:6443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClusterServerURL(tc.server)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateClusterServerURL(%q) err=%v, wantErr=%v", tc.server, err, tc.wantErr)
			}
		})
	}
}

// TestValidateClusterServerURL_ResolveAndCheck proves a HOSTNAME that resolves to
// the metadata IP is rejected (resolve-and-check, not just the literal string),
// while a *.svc cluster-internal name resolving to a normal ClusterIP passes.
func TestValidateClusterServerURL_ResolveAndCheck(t *testing.T) {
	stubResolver(t, map[string][]string{
		"evil.example.com":       {"169.254.169.254"},
		"sneaky.example.com":     {"10.0.0.5", "169.254.169.254"}, // one bad IP poisons the set
		"kubernetes.default.svc": {"10.96.0.1"},
		"apiserver.internal":     {"172.20.0.1"},
		"loopback-name.example":  {"127.0.0.1"},
	})

	cases := []struct {
		name    string
		server  string
		wantErr bool
	}{
		{"name -> metadata rejected", "https://evil.example.com", true},
		{"name -> one bad IP rejected", "https://sneaky.example.com:6443", true},
		{"name -> loopback rejected", "https://loopback-name.example:6443", true},
		{"svc -> clusterIP accepted", "https://kubernetes.default.svc", false},
		{"private name accepted", "https://apiserver.internal:6443", false},
		{"unresolvable name accepted (availability not SSRF)", "https://nope.example.com:6443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClusterServerURL(tc.server)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateClusterServerURL(%q) err=%v, wantErr=%v", tc.server, err, tc.wantErr)
			}
		})
	}
}

// TestValidateClusterServerURL_Malformed covers a missing-host / unparseable URL:
// a server URL with no host is an error (it cannot be validated).
func TestValidateClusterServerURL_Malformed(t *testing.T) {
	cases := []struct {
		name    string
		server  string
		wantErr bool
	}{
		{"no host", "https://", true},
		{"empty", "", true},
		{"scheme only path", "not-a-url-with-no-host", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClusterServerURL(tc.server)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateClusterServerURL(%q) err=%v, wantErr=%v", tc.server, err, tc.wantErr)
			}
		})
	}
}

// TestStaticServerURLRejectedMarksBroken proves the wiring end-to-end through
// discoverStatic: a static cluster whose server URL targets the metadata IP (or
// loopback) is marked BROKEN (Config nil, typed Err) -- the broken-cluster
// mechanism, not a usable connection -- while an RFC1918 apiserver is ACCEPTED
// (Config built). Insecure TLS does not block: an RFC1918 cluster with
// insecureSkipTLSVerify still loads.
func TestStaticServerURLRejectedMarksBroken(t *testing.T) {
	cfg := &appconfig.Config{
		Clusters: []appconfig.ClusterConnection{
			{Name: "metadata", Server: "https://169.254.169.254"},
			{Name: "loopback", Server: "https://127.0.0.1:6443"},
			{Name: "private", Server: "https://10.0.0.1:6443"},
			{Name: "private-insecure", Server: "https://10.0.0.2:6443", InsecureSkipTLSVerify: true},
		},
	}
	got := discoverStatic(cfg, credentialPluginGate{})
	byName := map[string]discoveredCluster{}
	for _, dc := range got {
		byName[dc.Name] = dc
	}

	for _, bad := range []string{"metadata", "loopback"} {
		dc, ok := byName[bad]
		if !ok {
			t.Fatalf("%s cluster missing from results: %#v", bad, got)
		}
		if dc.Err == nil {
			t.Fatalf("%s server URL must mark the cluster broken (typed error)", bad)
		}
		if dc.Config != nil {
			t.Fatalf("%s broken cluster must have nil Config, got %#v", bad, dc.Config)
		}
	}

	for _, good := range []string{"private", "private-insecure"} {
		dc, ok := byName[good]
		if !ok {
			t.Fatalf("%s cluster missing from results: %#v", good, got)
		}
		if dc.Err != nil {
			t.Fatalf("RFC1918 server URL must be ACCEPTED (real apiservers live there): %v", dc.Err)
		}
		if dc.Config == nil {
			t.Fatalf("%s cluster must carry a built Config", good)
		}
	}
}

// TestWarnInsecureTLS_NamesCluster proves the insecure-TLS warning is emitted and
// NAMES the cluster when honored, and is silent when TLS verification is on. The
// warning is WARN-ONLY: it does not block (no error return exists).
func TestWarnInsecureTLS_NamesCluster(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	warnInsecureTLS("prod-cluster", SourceSecret, true)
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a WARN log, got: %q", out)
	}
	if !strings.Contains(out, "prod-cluster") {
		t.Fatalf("warning must name the cluster, got: %q", out)
	}

	buf.Reset()
	warnInsecureTLS("safe-cluster", SourceStatic, false)
	if buf.Len() != 0 {
		t.Fatalf("no warning expected when TLS verification is on, got: %q", buf.String())
	}
}
