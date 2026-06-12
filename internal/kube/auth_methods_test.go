package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Auth-method behavioral tests. Each proves one TLS/auth method at
// the cheapest layer that genuinely exercises it, built through the canonical
// Connection model + RESTConfig (never hand-set rest.Config auth fields) over the
// shared TLS harness in testhelpers_test.go. These ride the same single sink that
// production uses, so a green run is evidence the model wires the method through,
// not just that a struct field copied.

// TestMTLS proves client-certificate mutual TLS end to end: a server demanding
// and verifying a client cert (tls.RequireAndVerifyClientCert) accepts a
// Connection whose AuthInfo carries the matching ClientCertificateData/KeyData,
// and rejects a connection that presents no client cert. The client cert and the
// server's client-CA pool are the SAME generated cert, so a successful handshake
// is proof the cert in the triple was actually presented. Adapts headlamp T2.
func TestMTLS(t *testing.T) {
	clientCert, clientKey := genClientCert(t)

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCert) {
		t.Fatal("failed to add generated client cert to the server's client-CA pool")
	}

	mux := http.NewServeMux()
	discoveryHandlers(mux)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	serverCA := serverCAPEM(t, srv)

	t.Run("matching client cert connects", func(t *testing.T) {
		conn := &Connection{
			Name:   "mtls",
			Source: SourceStatic,
			Cluster: &clientcmdapi.Cluster{
				Server:                   srv.URL,
				CertificateAuthorityData: serverCA,
			},
			AuthInfo: &clientcmdapi.AuthInfo{
				ClientCertificateData: clientCert,
				ClientKeyData:         clientKey,
			},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		client, err := NewClient(cfg, nil, false)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if _, _, err := client.ResourceTypes(context.Background()); err != nil {
			t.Fatalf("mTLS discovery with a valid client cert failed: %v", err)
		}
	})

	t.Run("absent client cert is rejected", func(t *testing.T) {
		// Same server CA so the SERVER is trusted and reachable -- the only thing
		// missing is the client cert, so a failure here is unambiguously the
		// RequireAndVerifyClientCert handshake rejecting an unauthenticated client,
		// not a CA-trust or dial error.
		conn := &Connection{
			Name:    "mtls-nocert",
			Source:  SourceStatic,
			Cluster: &clientcmdapi.Cluster{Server: srv.URL, CertificateAuthorityData: serverCA},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		client, err := NewClient(cfg, nil, false)
		if err != nil {
			return // eager validation is an acceptable rejection point
		}
		if _, _, err := client.ResourceTypes(context.Background()); err == nil {
			t.Fatal("expected the mTLS server to reject a connection presenting no client cert")
		}
	})
}

// TestBearerForward proves the simplest auth path: an inline bearer token on the
// connection's AuthInfo is forwarded as the Authorization header to the
// apiserver. Built through the model + RESTConfig against the auth-capturing TLS
// server.
func TestBearerForward(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	conn := &Connection{
		Name:    "bearer",
		Source:  SourceStatic,
		Cluster: &clientcmdapi.Cluster{Server: srv.URL, CertificateAuthorityData: serverCAPEM(t, srv)},
		AuthInfo: &clientcmdapi.AuthInfo{
			Token: "tok",
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	client, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, _, err := client.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if !rec.seen() {
		t.Fatal("auth-capturing server recorded no request")
	}
	if got := rec.Authorization(); got != "Bearer tok" {
		t.Fatalf("apiserver saw Authorization %q, want Bearer tok", got)
	}
}

func TestWithBearerClearsBasicAuth(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	conn := &Connection{
		Name:    "basic",
		Source:  SourceStatic,
		Cluster: &clientcmdapi.Cluster{Server: srv.URL, CertificateAuthorityData: serverCAPEM(t, srv)},
		AuthInfo: &clientcmdapi.AuthInfo{
			Username: "cluster-user",
			Password: "cluster-password",
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	base, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	viewer, err := base.WithBearer("viewer-token")
	if err != nil {
		t.Fatalf("WithBearer with a basic-auth base config failed: %v", err)
	}
	if viewer.config.Username != "" || viewer.config.Password != "" {
		t.Fatalf("passthrough config kept basic auth username=%q password=%q", viewer.config.Username, viewer.config.Password)
	}
	if _, _, err := viewer.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery with passthrough bearer failed: %v", err)
	}
	if got := rec.Authorization(); got != "Bearer viewer-token" {
		t.Fatalf("apiserver saw Authorization %q, want Bearer viewer-token", got)
	}
}

func TestWithBearerClearsClientCertificateAuth(t *testing.T) {
	clientCert, clientKey := genClientCert(t)

	var (
		mu         sync.Mutex
		seenPeer   bool
		seenBearer string
	)
	inner := http.NewServeMux()
	discoveryHandlers(inner)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenBearer = r.Header.Get("Authorization")
		seenPeer = seenPeer || len(r.TLS.PeerCertificates) > 0
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	conn := &Connection{
		Name:   "mtls-passthrough",
		Source: SourceStatic,
		Cluster: &clientcmdapi.Cluster{
			Server:                   srv.URL,
			CertificateAuthorityData: serverCAPEM(t, srv),
		},
		AuthInfo: &clientcmdapi.AuthInfo{
			ClientCertificateData: clientCert,
			ClientKeyData:         clientKey,
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	base, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	viewer, err := base.WithBearer("viewer-token")
	if err != nil {
		t.Fatalf("WithBearer with a client-cert base config failed: %v", err)
	}
	if len(viewer.config.CertData) != 0 || viewer.config.CertFile != "" || len(viewer.config.KeyData) != 0 || viewer.config.KeyFile != "" {
		t.Fatalf("passthrough config kept client cert auth certData=%d certFile=%q keyData=%d keyFile=%q", len(viewer.config.CertData), viewer.config.CertFile, len(viewer.config.KeyData), viewer.config.KeyFile)
	}
	if _, _, err := viewer.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery with passthrough bearer failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenBearer != "Bearer viewer-token" {
		t.Fatalf("apiserver saw Authorization %q, want Bearer viewer-token", seenBearer)
	}
	if seenPeer {
		t.Fatal("passthrough client presented the base client certificate")
	}
}

// TestTokenRotationWiring asserts the rotation WIRING, scoped honestly per the
// plan: client-go caches the on-disk token for ~1 minute and its clock seam is
// unexported, so a live "write A, request, rewrite B, request sees B" assertion
// in one test would observe A and be flaky. Instead the load-bearing invariant is
// that the file-token path arms client-go's refresh round-tripper -- i.e.
// RESTConfig sets BearerTokenFile (so client-go re-reads the file) AND populates
// BearerToken with the initial read. (Mirrors TestConnectionTokenFileRotation;
// kept focused on the wiring, not a real A->B swap.)
func TestTokenRotationWiring(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("initial-file-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	conn := &Connection{
		Name:     "rotating",
		Source:   SourceStatic,
		Cluster:  &clientcmdapi.Cluster{Server: "https://api.example.com"},
		AuthInfo: &clientcmdapi.AuthInfo{TokenFile: tokenPath},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if cfg.BearerTokenFile != tokenPath {
		t.Fatalf("BearerTokenFile = %q, want %q -- client-go's refresh round-tripper is not armed", cfg.BearerTokenFile, tokenPath)
	}
	if cfg.BearerToken != "initial-file-token" {
		t.Fatalf("BearerToken = %q, want the initial file read", cfg.BearerToken)
	}
}

// TestExecPlugin proves the exec credential-plugin path end to end: a Connection
// whose AuthInfo.Exec points at a real fake plugin script (testdata/exec-plugin.sh)
// returning an ExecCredential JSON over stdout, driven through RESTConfig (the
// supported path), produces a client that presents the plugin's token as the
// Authorization header. InteractiveMode MUST be Never (client-go errors
// "unknown interactiveMode" on an unset mode). The plugin reads its output from
// the TEST_OUTPUT env var the connection injects. Adapts headlamp T3.
func TestExecPlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The .bat variant exists for portability, but the env-var contract this
		// test relies on (plain TEST_OUTPUT) differs on Windows; keep it simple.
		t.Skip("exec-plugin test wires the POSIX testdata/exec-plugin.sh; skipped on windows")
	}

	script, err := filepath.Abs(filepath.Join("testdata", "exec-plugin.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(script); err != nil {
		t.Fatalf("exec plugin script missing: %v", err)
	} else if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec plugin script %s is not executable (mode %v)", script, info.Mode())
	}

	srv, rec := newAuthCapturingTLSServer(t)

	// The ExecCredential JSON the plugin echoes on stdout. client-go parses it and
	// uses status.token as the bearer.
	cred := map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1",
		"kind":       "ExecCredential",
		"status": map[string]any{
			"token": "exec-plugin-token",
		},
	}
	credJSON, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}

	conn := &Connection{
		Name:    "exec",
		Source:  SourceStatic,
		Cluster: &clientcmdapi.Cluster{Server: srv.URL, CertificateAuthorityData: serverCAPEM(t, srv)},
		AuthInfo: &clientcmdapi.AuthInfo{
			Exec: &clientcmdapi.ExecConfig{
				APIVersion:      "client.authentication.k8s.io/v1",
				Command:         script,
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
				Env: []clientcmdapi.ExecEnvVar{
					{Name: "TEST_OUTPUT", Value: string(credJSON)},
				},
			},
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig with exec: %v", err)
	}
	client, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, _, err := client.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery driven by the exec plugin failed: %v", err)
	}
	if !rec.seen() {
		t.Fatal("auth-capturing server recorded no request")
	}
	if got := rec.Authorization(); got != "Bearer exec-plugin-token" {
		t.Fatalf("apiserver saw Authorization %q, want Bearer exec-plugin-token (the exec plugin's token)", got)
	}
}

// TestImpersonation proves the impersonation header is EMITTED from a static
// Impersonate identity on the connection (the Act-As header reaches the
// apiserver), and lightly cross-checks the passthrough clear: WithBearer("viewer") on the
// same client reaches the server as Bearer viewer with NO Impersonate-User. The
// clear's deep proof lives in TestImpersonationClearedOnPassthrough (same
// package); this is a light overlap, not a duplicate.
func TestImpersonation(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	conn := &Connection{
		Name:    "imp",
		Source:  SourceStatic,
		Cluster: &clientcmdapi.Cluster{Server: srv.URL, CertificateAuthorityData: serverCAPEM(t, srv)},
		AuthInfo: &clientcmdapi.AuthInfo{
			// A base credential plus a static impersonation identity: the request
			// authenticates as the token and acts-as "robot".
			Token:             "base-tok",
			Impersonate:       "robot",
			ImpersonateGroups: []string{"viewers"},
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	client, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	t.Run("static impersonate emits Act-As", func(t *testing.T) {
		if _, _, err := client.ResourceTypes(context.Background()); err != nil {
			t.Fatalf("discovery: %v", err)
		}
		if got := rec.ImpersonateUser(); got != "robot" {
			t.Fatalf("apiserver saw Impersonate-User %q, want robot", got)
		}
		if groups := rec.ImpersonateGroups(); len(groups) != 1 || groups[0] != "viewers" {
			t.Fatalf("apiserver saw Impersonate-Group %#v, want [viewers]", groups)
		}
	})

	t.Run("passthrough clears Act-As (impersonation cross-check)", func(t *testing.T) {
		// A fresh server+recorder so the clear is observed on this request alone,
		// not confused with the accumulated impersonating requests above. Build a
		// fresh base client off its own connection (the one above has cached
		// discovery), then WithBearer the viewer.
		srv2, rec2 := newAuthCapturingTLSServer(t)
		conn2 := &Connection{
			Name:    "imp2",
			Source:  SourceStatic,
			Cluster: &clientcmdapi.Cluster{Server: srv2.URL, CertificateAuthorityData: serverCAPEM(t, srv2)},
			AuthInfo: &clientcmdapi.AuthInfo{
				Token:             "base-tok",
				Impersonate:       "robot",
				ImpersonateGroups: []string{"viewers"},
			},
		}
		cfg2, err := conn2.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		base, err := NewClient(cfg2, nil, false)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		wb, err := base.WithBearer("viewer")
		if err != nil {
			t.Fatalf("WithBearer: %v", err)
		}
		if _, _, err := wb.ResourceTypes(context.Background()); err != nil {
			t.Fatalf("discovery: %v", err)
		}
		if got := rec2.Authorization(); got != "Bearer viewer" {
			t.Fatalf("apiserver saw Authorization %q, want Bearer viewer", got)
		}
		if got := rec2.ImpersonateUser(); got != "" {
			t.Fatalf("Impersonate-User leaked through passthrough: %q -- viewer would get the impersonated RBAC", got)
		}
	})
}

// TestImpersonationAndStaticCredsClearedOnPassthrough pins the hardest clear
// combination: a base config carrying BOTH a static impersonation identity AND
// static credentials (basic auth + client cert) at the same time. After
// WithBearer, the passthrough request must reach the apiserver as the viewer's
// Bearer with NO Impersonate-User (Act-As) header -- proving the clear strips
// every static identity, not just one in isolation. This complements the
// single-source clears (basic-auth-only, cert-only, impersonate-only) by
// exercising them together on one base config.
func TestImpersonationAndStaticCredsClearedOnPassthrough(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	clientCert, clientKey := genClientCert(t)

	base, err := NewClient(&rest.Config{
		Host:            srv.URL,
		TLSClientConfig: rest.TLSClientConfig{CAData: serverCAPEM(t, srv), CertData: clientCert, KeyData: clientKey},
		Username:        "cluster-user",
		Password:        "cluster-password",
		Impersonate:     rest.ImpersonationConfig{UserName: "robot", Groups: []string{"admins"}},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	viewer, err := base.WithBearer("viewer-token")
	if err != nil {
		t.Fatalf("WithBearer with combined static creds + impersonation base: %v", err)
	}

	// The passthrough clone must carry none of the base static identities: basic
	// auth, client cert, and impersonation are all cleared in favor of the viewer
	// bearer alone.
	if viewer.config.Username != "" || viewer.config.Password != "" {
		t.Fatalf("passthrough kept basic auth username=%q password=%q", viewer.config.Username, viewer.config.Password)
	}
	if len(viewer.config.CertData) != 0 || len(viewer.config.KeyData) != 0 {
		t.Fatalf("passthrough kept client cert certData=%d keyData=%d", len(viewer.config.CertData), len(viewer.config.KeyData))
	}
	if viewer.config.Impersonate.UserName != "" || len(viewer.config.Impersonate.Groups) != 0 {
		t.Fatalf("passthrough kept impersonation %#v", viewer.config.Impersonate)
	}

	if _, _, err := viewer.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery with passthrough bearer failed: %v", err)
	}
	if got := rec.Authorization(); got != "Bearer viewer-token" {
		t.Fatalf("apiserver saw Authorization %q, want Bearer viewer-token", got)
	}
	if got := rec.ImpersonateUser(); got != "" {
		t.Fatalf("Impersonate-User leaked through passthrough: %q -- viewer would get the impersonated RBAC", got)
	}
}

// TestOIDC exercises the OIDC auth-provider CONSTRUCTION path per the plan: set
// AuthInfo.AuthProvider (name "oidc") on the connection and assert it flows
// through RESTConfig into rest.Config.AuthProvider. A full mock-OIDC token
// exchange (headlamp T4) is optional and meaningfully harder (it needs an
// in-process issuer the oidc provider can reach during a live request); the
// REQUIRED, load-bearing assertion is that the auth-provider wiring rides the
// model -- which is what makes the provider available to client-go's transport at
// all. We do NOT improvise a direct rest.Config.AuthProvider shape outside the
// triple.
func TestOIDC(t *testing.T) {
	conn := &Connection{
		Name:    "oidc",
		Source:  SourceStatic,
		Cluster: &clientcmdapi.Cluster{Server: "https://api.example.com"},
		AuthInfo: &clientcmdapi.AuthInfo{
			AuthProvider: &clientcmdapi.AuthProviderConfig{
				Name: "oidc",
				Config: map[string]string{
					"idp-issuer-url": "https://issuer.example",
					"client-id":      "readout",
				},
			},
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig with OIDC auth provider: %v", err)
	}
	if cfg.AuthProvider == nil {
		t.Fatal("AuthProvider not populated from AuthInfo.AuthProvider -- OIDC wiring does not ride the model")
	}
	if cfg.AuthProvider.Name != "oidc" {
		t.Fatalf("AuthProvider.Name = %q, want oidc", cfg.AuthProvider.Name)
	}
	if got := cfg.AuthProvider.Config["idp-issuer-url"]; got != "https://issuer.example" {
		t.Fatalf("AuthProvider.Config[idp-issuer-url] = %q, want https://issuer.example", got)
	}
}
