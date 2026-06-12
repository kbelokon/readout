package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// Shared TLS test harness. These helpers are reused by the multi-source
// loader, viewer-identity, and auth-method tests. They upgrade the fake
// apiserver from the plain-HTTP newFakeAPIServer to real TLS so a genuine
// handshake + first discovery request runs end to end.

// discoveryHandlers registers the minimal discovery/version endpoints client-go
// hits on connect, so NewClient(...).ResourceTypes(...) completes a real request
// against the server.
func discoveryHandlers(mux *http.ServeMux) {
	j := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("/version", j(`{"major":"1","minor":"30","gitVersion":"v1.30.0"}`))
	mux.HandleFunc("/api", j(`{"kind":"APIVersions","versions":["v1"]}`))
	mux.HandleFunc("/api/v1", j(`{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`))
	mux.HandleFunc("/apis", j(`{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`))
}

// newTLSFakeAPIServer is an httptest TLS server answering discovery/version. Its
// self-signed cert is its own CA; serverCAPEM extracts it for CAData.
func newTLSFakeAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	discoveryHandlers(mux)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serverCAPEM returns the server's self-signed certificate as PEM, suitable for
// rest.Config.TLSClientConfig.CAData. An httptest TLS cert is self-signed, so it
// is its own trust anchor.
func serverCAPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("server has no TLS certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// newDistinctTLSServer is a live TLS apiserver using a FRESHLY GENERATED
// self-signed certificate -- distinct from the single built-in cert every
// httptest.NewTLSServer shares. It exists so the CA-trust negative case has a
// genuinely different, reachable trust anchor: without it, two httptest TLS
// servers present the SAME cert and a "wrong CA" would silently equal the right
// one. Returns the server (kept open) and its CA PEM.
func newDistinctTLSServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{Organization: []string{"Readout Test Distinct CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	discoveryHandlers(mux)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// authRecorder captures the auth-relevant inbound headers on a fake apiserver.
// It records BOTH Authorization (bearer forwarding) AND the impersonation
// headers (Act-As) so the viewer-identity tests can prove a passthrough request
// carries the viewer token and NO static Impersonate-User.
//
// client-go fans out one discovery request per (group,version) in parallel, so
// the recorder ACCUMULATES every request rather than keeping only the last: all
// requests on one connection carry identical auth, so the representative
// accessors (first non-empty value) are deterministic in VALUE regardless of
// goroutine scheduling. Do NOT use this to assert per-request header DIFFERENCES.
type authRecorder struct {
	mu       sync.Mutex
	requests []recordedAuth
}

type recordedAuth struct {
	authorization     string
	impersonateUser   string
	impersonateGroups []string
}

func (a *authRecorder) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, recordedAuth{
		authorization:     r.Header.Get("Authorization"),
		impersonateUser:   r.Header.Get("Impersonate-User"),
		impersonateGroups: r.Header.Values("Impersonate-Group"),
	})
}

// seen reports whether any request reached the server.
func (a *authRecorder) seen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests) > 0
}

// Authorization returns the Authorization header common to the connection's
// requests: the first non-empty value seen, or "" if no request carried one.
func (a *authRecorder) Authorization() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, req := range a.requests {
		if req.authorization != "" {
			return req.authorization
		}
	}
	return ""
}

// ImpersonateUser returns the Impersonate-User (Act-As) header: the first
// non-empty value seen, or "" if NO request carried one (the passthrough proof
// that static impersonation was cleared).
func (a *authRecorder) ImpersonateUser() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, req := range a.requests {
		if req.impersonateUser != "" {
			return req.impersonateUser
		}
	}
	return ""
}

func (a *authRecorder) ImpersonateGroups() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, req := range a.requests {
		if len(req.impersonateGroups) > 0 {
			return append([]string(nil), req.impersonateGroups...)
		}
	}
	return nil
}

// newAuthCapturingTLSServer is a TLS fake apiserver that records the auth headers
// of every request before answering discovery. Reused by the viewer-identity and
// auth-method tests.
func newAuthCapturingTLSServer(t *testing.T) (*httptest.Server, *authRecorder) {
	t.Helper()
	rec := &authRecorder{}
	inner := http.NewServeMux()
	discoveryHandlers(inner)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// genClientCert generates an x509 client cert + key (PEM), expiring in 24h, for
// mTLS tests (RequireAndVerifyClientCert). Lifted from client-go's
// rest/exec_test.go (readout is on the same client-go version).
func genClientCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyRaw, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{Organization: []string{"Acme Co"}},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certRaw, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certRaw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyRaw})
}

// TestUpstreamCATrust is the headline new coverage Headlamp lacks: proving that
// trusting the RIGHT CA verifies and connects, and a WRONG CA is rejected as a
// TLS-verification failure on a REACHABLE endpoint (not a dial error). The
// negative case keeps a second live TLS server open and points Host at the GOOD
// (reachable) server while feeding the WRONG server's CA -- so the rejection is
// unambiguously certificate verification, not connection-refused.
func TestUpstreamCATrust(t *testing.T) {
	good := newTLSFakeAPIServer(t)
	goodCA := serverCAPEM(t, good)

	// A second, distinct, STILL-OPEN TLS server with a genuinely different CA
	// (a fresh generated cert -- not httptest's shared built-in). It stays open
	// so the negative case below is unambiguously a verification rejection.
	_, wrongCA := newDistinctTLSServer(t)

	t.Run("correct CA verifies and connects", func(t *testing.T) {
		cfg := &rest.Config{Host: good.URL, TLSClientConfig: rest.TLSClientConfig{CAData: goodCA}}
		client, err := NewClient(cfg, nil, false)
		if err != nil {
			t.Fatalf("NewClient with good CA: %v", err)
		}
		if _, _, err := client.ResourceTypes(context.Background()); err != nil {
			t.Fatalf("verified TLS discovery failed against the trusted server: %v", err)
		}
	})

	t.Run("wrong CA on a reachable endpoint is rejected", func(t *testing.T) {
		// Host = the GOOD, reachable server; CA = the WRONG server's CA.
		cfg := &rest.Config{Host: good.URL, TLSClientConfig: rest.TLSClientConfig{CAData: wrongCA}}
		client, err := NewClient(cfg, nil, false)
		if err != nil {
			return // eager validation is an acceptable rejection point
		}
		_, _, err = client.ResourceTypes(context.Background())
		if err == nil {
			t.Fatal("expected TLS verification failure with the wrong CA on a reachable endpoint")
		}
		if strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("got a dial error, not a TLS verification rejection: %v", err)
		}
		if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("error is not a certificate-verification failure: %v", err)
		}
	})
}

// TestAuthCaptureRecordsHeaders validates the auth-capturing harness the
// viewer-identity and auth-method tests depend on: a base config carrying a
// bearer token AND a static impersonation identity must reach the server with
// both the Authorization header and the Impersonate-User (Act-As) header.
func TestAuthCaptureRecordsHeaders(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	cfg := &rest.Config{
		Host:            srv.URL,
		TLSClientConfig: rest.TLSClientConfig{CAData: serverCAPEM(t, srv)},
		BearerToken:     "base-token",
		Impersonate:     rest.ImpersonationConfig{UserName: "robot", Groups: []string{"viewers"}},
	}
	client, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery against auth-capturing server: %v", err)
	}
	if !rec.seen() {
		t.Fatal("auth-capturing server recorded no request")
	}
	if rec.Authorization() != "Bearer base-token" {
		t.Fatalf("Authorization = %q, want Bearer base-token", rec.Authorization())
	}
	if rec.ImpersonateUser() != "robot" {
		t.Fatalf("Impersonate-User = %q, want robot", rec.ImpersonateUser())
	}
	if groups := rec.ImpersonateGroups(); len(groups) != 1 || groups[0] != "viewers" {
		t.Fatalf("Impersonate-Group = %#v, want [viewers]", groups)
	}
}
