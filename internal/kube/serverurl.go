package kube

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"time"
)

// clusterHostResolver resolves a hostname to IP addresses for cluster-server URL
// validation. It defaults to the real DNS resolver but is a package var so tests
// can stub DNS (a hostname that "resolves" to the metadata IP) without depending
// on the network. The signature mirrors net.Resolver.LookupIPAddr.
var clusterHostResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// clusterURLResolveTimeout bounds the DNS lookup done while validating a cluster
// server URL so a slow/hostile resolver cannot stall startup discovery.
const clusterURLResolveTimeout = 5 * time.Second

// validateClusterServerURL rejects a cluster API-server URL whose host is (or
// resolves to) a link-local/metadata address. Its policy is DELIBERATELY
// different from the hook-URL validator (internal/config): a real apiserver
// legitimately sits on a private/RFC1918 IP, on the in-cluster ClusterIP, AND on
// loopback -- kind / minikube / k3d all put the apiserver on 127.0.0.1 in the
// kubeconfig, so loopback is a normal apiserver location for local development.
// This validator therefore ALLOWS private AND loopback ranges and blocks ONLY
// link-local/metadata (169.254.0.0/16, fe80::/10), which is the real SSRF target
// (cloud metadata) and never a legitimate apiserver. The two validators must not
// be merged.
//
// A literal IP host is checked directly. A hostname is RESOLVED and EVERY
// resolved IP is checked, so a name that resolves to 169.254.169.254 is rejected
// (the metadata SSRF vector) while a cluster-internal *.svc name that resolves to
// a normal ClusterIP passes. A name that cannot be resolved is allowed through
// (client-go will fail the dial later): name resolution is an availability
// concern, not the SSRF guard's job, and blocking on lookup failure would turn a
// transient DNS blip into a broken cluster.
//
// Per-connection TOCTOU (a name rebinding to the metadata IP between this check
// and client-go's dial) is an accepted residual and is not closed here.
func validateClusterServerURL(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("invalid server URL %q: %w", server, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("server URL %q has no host", server)
	}

	// Literal IP host: check it directly, no DNS.
	if ip := net.ParseIP(host); ip != nil {
		if reason := rejectedClusterIP(ip); reason != "" {
			return fmt.Errorf("cluster server URL %q targets a %s address", server, reason)
		}
		return nil
	}

	// Hostname: resolve and check every resolved IP.
	ctx, cancel := context.WithTimeout(context.Background(), clusterURLResolveTimeout)
	defer cancel()
	addrs, lookupErr := clusterHostResolver(ctx, host)
	if lookupErr != nil {
		// Unresolvable name is an availability problem, not an SSRF hit -- let
		// client-go fail the dial instead of marking the cluster broken here.
		return nil
	}
	for _, addr := range addrs {
		if reason := rejectedClusterIP(addr.IP); reason != "" {
			return fmt.Errorf("cluster server URL %q resolves to a %s address (%s)", server, reason, addr.IP)
		}
	}
	return nil
}

// rejectedClusterIP reports a non-empty reason when an IP is in the cluster-URL
// reject set: ONLY link-local/metadata (169.254.0.0/16, fe80::/10), which is the
// real SSRF target (cloud metadata) and never a legitimate apiserver. Loopback
// (127.0.0.0/8, ::1) is NOT rejected -- kind/minikube/k3d put the apiserver on
// 127.0.0.1 in the kubeconfig -- and neither is private/RFC1918, where real
// apiservers live. An empty string means the IP is acceptable.
func rejectedClusterIP(ip net.IP) string {
	switch {
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "link-local/metadata"
	default:
		return ""
	}
}

// warnInsecureTLS emits a per-cluster startup WARNING naming the cluster whenever
// insecureSkipTLSVerify is honored for it. It WARNS ONLY -- an insecure-TLS
// cluster still loads (zero blocking gates); the warning makes a disabled TLS
// verification visible to the operator instead of silently honoring it.
func warnInsecureTLS(clusterName string, source Source, insecure bool) {
	if !insecure {
		return
	}
	slog.Warn("cluster TLS verification disabled (insecureSkipTLSVerify honored)",
		"cluster", clusterName, "source", source.String())
}
