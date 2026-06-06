package kube

import (
	"os"
	"path/filepath"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// fakeCAPEM is arbitrary PEM-looking bytes. clientcmd copies CertificateAuthorityData
// into rest.Config.CAData verbatim and does NOT parse it at ClientConfig() time (the
// x509 parse happens later in HTTPClientFor/TLSConfigFor), so the byte-copy proof
// does not need a real certificate. The real handshake-against-an-extracted-CA proof
// lives in the Unit 7 CA-trust test.
const fakeCAPEM = "-----BEGIN CERTIFICATE-----\nMIIB-fake-ca-data\n-----END CERTIFICATE-----\n"

// TestRESTConfig proves the canonical claim of D1: a Connection built from the
// kubeconfig triple yields a rest.Config that carries every TLS/auth field, with
// zero rest.Config fields set by hand -- clientcmd populates them. Each subtest
// sets one family of fields on api.Cluster/api.AuthInfo and asserts they surface
// on the produced rest.Config.
func TestRESTConfig(t *testing.T) {
	const host = "https://api.example.com"

	t.Run("server CA and server name", func(t *testing.T) {
		conn := &Connection{
			Name:   "c",
			Source: SourceStatic,
			Cluster: &clientcmdapi.Cluster{
				Server:                   host,
				CertificateAuthorityData: []byte(fakeCAPEM),
				TLSServerName:            "custom.sni",
			},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.Host != host {
			t.Fatalf("Host = %q, want %q", cfg.Host, host)
		}
		if string(cfg.CAData) != fakeCAPEM {
			t.Fatalf("CAData = %q, want the configured CA", string(cfg.CAData))
		}
		if cfg.ServerName != "custom.sni" {
			t.Fatalf("ServerName = %q, want custom.sni", cfg.ServerName)
		}
		if cfg.Insecure {
			t.Fatal("Insecure set without InsecureSkipTLSVerify")
		}
	})

	t.Run("insecure skip TLS verify", func(t *testing.T) {
		conn := &Connection{
			Name:    "c",
			Cluster: &clientcmdapi.Cluster{Server: host, InsecureSkipTLSVerify: true},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if !cfg.Insecure {
			t.Fatal("Insecure not set from InsecureSkipTLSVerify")
		}
	})

	t.Run("inline bearer token", func(t *testing.T) {
		conn := &Connection{
			Name:     "c",
			Cluster:  &clientcmdapi.Cluster{Server: host},
			AuthInfo: &clientcmdapi.AuthInfo{Token: "inline-token"},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.BearerToken != "inline-token" {
			t.Fatalf("BearerToken = %q, want inline-token", cfg.BearerToken)
		}
	})

	t.Run("client certificate mTLS", func(t *testing.T) {
		conn := &Connection{
			Name:    "c",
			Cluster: &clientcmdapi.Cluster{Server: host},
			AuthInfo: &clientcmdapi.AuthInfo{
				ClientCertificateData: []byte("cert-data"),
				ClientKeyData:         []byte("key-data"),
			},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if string(cfg.CertData) != "cert-data" || string(cfg.KeyData) != "key-data" {
			t.Fatalf("client cert/key = %q/%q, want cert-data/key-data", cfg.CertData, cfg.KeyData)
		}
	})

	t.Run("impersonation rides the model", func(t *testing.T) {
		conn := &Connection{
			Name:    "c",
			Cluster: &clientcmdapi.Cluster{Server: host},
			AuthInfo: &clientcmdapi.AuthInfo{
				Impersonate:          "system:serviceaccount:ns:robot",
				ImpersonateGroups:    []string{"viewers"},
				ImpersonateUID:       "uid-1",
				ImpersonateUserExtra: map[string][]string{"scope": {"read"}},
			},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig: %v", err)
		}
		if cfg.Impersonate.UserName != "system:serviceaccount:ns:robot" {
			t.Fatalf("Impersonate.UserName = %q", cfg.Impersonate.UserName)
		}
		if len(cfg.Impersonate.Groups) != 1 || cfg.Impersonate.Groups[0] != "viewers" {
			t.Fatalf("Impersonate.Groups = %#v", cfg.Impersonate.Groups)
		}
		if cfg.Impersonate.UID != "uid-1" {
			t.Fatalf("Impersonate.UID = %q", cfg.Impersonate.UID)
		}
	})

	t.Run("exec credential plugin rides the model", func(t *testing.T) {
		// The exec path is what Unit 8 exercises. clientcmd rejects an ExecConfig
		// with an unset InteractiveMode (validation.go: "interactiveMode must be
		// specified"), so a server-side exec connection MUST set Never -- pin that
		// gate here so Unit 8 inherits a documented, working seam.
		conn := &Connection{
			Name:    "c",
			Cluster: &clientcmdapi.Cluster{Server: host},
			AuthInfo: &clientcmdapi.AuthInfo{
				Exec: &clientcmdapi.ExecConfig{
					APIVersion:      "client.authentication.k8s.io/v1",
					Command:         "aws",
					Args:            []string{"eks", "get-token"},
					InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
				},
			},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig with exec: %v", err)
		}
		if cfg.ExecProvider == nil || cfg.ExecProvider.Command != "aws" {
			t.Fatalf("ExecProvider not populated from AuthInfo.Exec: %#v", cfg.ExecProvider)
		}
	})

	t.Run("OIDC auth provider rides the model", func(t *testing.T) {
		conn := &Connection{
			Name:    "c",
			Cluster: &clientcmdapi.Cluster{Server: host},
			AuthInfo: &clientcmdapi.AuthInfo{
				AuthProvider: &clientcmdapi.AuthProviderConfig{
					Name:   "oidc",
					Config: map[string]string{"idp-issuer-url": "https://issuer.example"},
				},
			},
		}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig with auth provider: %v", err)
		}
		if cfg.AuthProvider == nil || cfg.AuthProvider.Name != "oidc" {
			t.Fatalf("AuthProvider not populated from AuthInfo.AuthProvider: %#v", cfg.AuthProvider)
		}
	})

	t.Run("nil AuthInfo is a valid anonymous connection", func(t *testing.T) {
		conn := &Connection{Name: "c", Source: SourceStatic, Cluster: &clientcmdapi.Cluster{Server: host}}
		cfg, err := conn.RESTConfig()
		if err != nil {
			t.Fatalf("RESTConfig with nil AuthInfo: %v", err)
		}
		if cfg.BearerToken != "" || cfg.Username != "" {
			t.Fatalf("anonymous connection carries credentials: token=%q user=%q", cfg.BearerToken, cfg.Username)
		}
	})
}

// TestConnectionTokenFileRotation pins the D8b rotation-wiring contract. The
// file-token path sets AuthInfo.TokenFile ONLY (Token empty). clientcmd then
// reads the file into BearerToken AND keeps BearerTokenFile set
// (client_config.go:277-287) -- BearerTokenFile staying set is what arms
// client-go's ~1-minute file re-read, so a rotated on-disk token is picked up.
//
// NOTE: the plan's parenthetical "BearerToken empty" is factually wrong against
// client-go: clientcmd populates BearerToken from the initial file read. The
// load-bearing rotation invariant is BearerTokenFile == path; BearerToken holds
// the initial content, not "". Asserted accordingly.
func TestConnectionTokenFileRotation(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	conn := &Connection{
		Name:     "c",
		Source:   SourceStatic,
		Cluster:  &clientcmdapi.Cluster{Server: "https://api.example.com"},
		AuthInfo: &clientcmdapi.AuthInfo{TokenFile: tokenPath},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if cfg.BearerTokenFile != tokenPath {
		t.Fatalf("BearerTokenFile = %q, want %q -- rotation refresh is not armed", cfg.BearerTokenFile, tokenPath)
	}
	if cfg.BearerToken != "file-token\n" {
		t.Fatalf("BearerToken = %q, want the initial file read", cfg.BearerToken)
	}
}

func TestSourceString(t *testing.T) {
	cases := map[Source]string{
		SourceStatic:     "static",
		SourceKubeconfig: "kubeconfig",
		SourceInCluster:  "in-cluster",
		SourceSecret:     "secret",
		Source(99):       "unknown",
	}
	for src, want := range cases {
		if got := src.String(); got != want {
			t.Fatalf("Source(%d).String() = %q, want %q", src, got, want)
		}
	}
}
