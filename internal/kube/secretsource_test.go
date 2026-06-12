package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// TestArgoSourceFailureNonFatalToOtherSources pins the invariant that an Argo
// host-list failure is surfaced but NON-FATAL to other sources: a healthy static
// cluster still loads, and the Argo source failure becomes a single broken entry
// rather than aborting the whole reload. A non-existent host cluster forces the
// source error deterministically (no in-cluster env dependency).
func TestArgoSourceFailureNonFatalToOtherSources(t *testing.T) {
	good := newTLSFakeAPIServer(t)
	cfg := &appconfig.Config{
		Clusters: []appconfig.ClusterConnection{
			{Name: "static-good", Server: good.URL, CertificateAuthorityData: serverCAPEM(t, good)},
		},
		ArgoCD: &appconfig.ArgoCDSource{HostCluster: "does-not-exist", Namespace: "argocd"},
	}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatalf("an Argo host failure must not fail NewManager (non-fatal to other sources): %v", err)
	}
	if _, ok := m.Get("static-good"); !ok {
		t.Fatalf("static cluster must still load when the Argo source fails: %#v", m.Clusters())
	}
	broken := m.Broken()
	if len(broken) != 1 || broken[0].Name != "argocd" || broken[0].Source != SourceSecret {
		t.Fatalf("Argo source failure should surface as one broken entry, got %#v", broken)
	}
}

// argoFixturePath points at the captured, redacted Argo CD cluster Secret. The
// parser test derives its Secret Data map from THIS fixture (not from the
// parser's own assumption) so it is not a self-alibi (Accepted Risk: "Argo
// Secret format without a working mirror"). It lives under testdata/ (tracked) --
// NOT a gitignored docs directory -- so the test is hermetic in a fresh clone / CI.
const argoFixturePath = "testdata/argo-cluster-secret.example.yaml"

// loadFixtureSecretData reads the Nth YAML document of the captured fixture and
// returns its Secret Data map exactly as the Kubernetes Secret API would present
// it: the readable stringData keys promoted to data with their raw string bytes
// (the API merges stringData into data on write and the client returns data
// already base64-DECODED). This is the precise map[string][]byte the parser
// consumes at runtime, so the test exercises the real captured shape.
func loadFixtureSecretData(t *testing.T, doc int) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(argoFixturePath))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	docs := splitYAMLDocs(string(raw))
	if doc >= len(docs) {
		t.Fatalf("fixture has %d docs, asked for index %d", len(docs), doc)
	}
	var secret corev1.Secret
	if err := yaml.Unmarshal([]byte(docs[doc]), &secret); err != nil {
		t.Fatalf("unmarshal fixture doc %d: %v", doc, err)
	}
	if len(secret.StringData) == 0 {
		t.Fatalf("fixture doc %d has no stringData", doc)
	}
	data := make(map[string][]byte, len(secret.StringData))
	for k, v := range secret.StringData {
		data[k] = []byte(v)
	}
	return data
}

// splitYAMLDocs splits a multi-document YAML stream on the `---` separator and
// drops empty / comment-only documents.
func splitYAMLDocs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, "\n---\n") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// A document that is only comments has no mapping content.
		hasContent := false
		for _, line := range strings.Split(trimmed, "\n") {
			l := strings.TrimSpace(line)
			if l != "" && !strings.HasPrefix(l, "#") {
				hasContent = true
				break
			}
		}
		if hasContent {
			out = append(out, part)
		}
	}
	return out
}

// TestArgoSecretParsesNestedConfig proves the parser core: the nested Argo
// `data.config` shape (server+name top-level; TLS under tlsClientConfig with
// base64 caData/certData/keyData; bearerToken auth) parses into the canonical
// Connection, and the produced rest.Config carries the decoded CA, the token, and
// the client cert -- via the proven RESTConfig, with no hand-set fields.
func TestArgoSecretParsesNestedConfig(t *testing.T) {
	data := loadFixtureSecretData(t, 0) // the bearerToken + TLS prod cluster

	conn, err := parseArgoClusterSecret(data)
	if err != nil {
		t.Fatalf("parseArgoClusterSecret: %v", err)
	}

	if conn.Source != SourceSecret {
		t.Fatalf("Source = %v, want SourceSecret", conn.Source)
	}
	if conn.Name != "prod-example" {
		t.Fatalf("Name = %q, want prod-example", conn.Name)
	}
	if conn.Cluster.Server != "https://prod.example.com:6443" {
		t.Fatalf("Cluster.Server = %q", conn.Cluster.Server)
	}
	if conn.Cluster.InsecureSkipTLSVerify {
		t.Fatal("InsecureSkipTLSVerify set, fixture has insecure:false")
	}

	// caData is base64 INSIDE the JSON; the parser must decode it to raw PEM.
	wantCA, err := base64.StdEncoding.DecodeString(
		"LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tClJFREFDVEVELUNMVVNURVItQ0EKLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo=")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(conn.Cluster.CertificateAuthorityData, wantCA) {
		t.Fatalf("CertificateAuthorityData = %q, want decoded PEM %q",
			conn.Cluster.CertificateAuthorityData, wantCA)
	}
	if !strings.Contains(string(conn.Cluster.CertificateAuthorityData), "BEGIN CERTIFICATE") {
		t.Fatalf("CA was not base64-decoded: %q", conn.Cluster.CertificateAuthorityData)
	}

	if conn.AuthInfo == nil {
		t.Fatal("AuthInfo nil, fixture carries a bearerToken")
	}
	if conn.AuthInfo.Token != "REDACTED-SERVICE-ACCOUNT-TOKEN" {
		t.Fatalf("AuthInfo.Token = %q", conn.AuthInfo.Token)
	}
	if !strings.Contains(string(conn.AuthInfo.ClientCertificateData), "BEGIN CERTIFICATE") {
		t.Fatalf("client cert not decoded: %q", conn.AuthInfo.ClientCertificateData)
	}
	if !strings.Contains(string(conn.AuthInfo.ClientKeyData), "PRIVATE KEY") {
		t.Fatalf("client key not decoded: %q", conn.AuthInfo.ClientKeyData)
	}

	// The proven RESTConfig must carry the same material -- no rest.Config
	// field is set by hand in the primitive.
	restCfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if restCfg.Host != "https://prod.example.com:6443" {
		t.Fatalf("rest.Config.Host = %q", restCfg.Host)
	}
	if restCfg.BearerToken != "REDACTED-SERVICE-ACCOUNT-TOKEN" {
		t.Fatalf("rest.Config.BearerToken = %q", restCfg.BearerToken)
	}
	if !bytes.Equal(restCfg.CAData, wantCA) {
		t.Fatalf("rest.Config.CAData = %q, want decoded CA", restCfg.CAData)
	}
	if len(restCfg.CertData) == 0 || len(restCfg.KeyData) == 0 {
		t.Fatalf("rest.Config client cert/key not carried: cert=%q key=%q", restCfg.CertData, restCfg.KeyData)
	}
}

// TestArgoSecretParsesExecProviderVariant proves the second fixture variant (an
// EKS-style cluster using execProviderConfig instead of a bearer token) maps onto
// AuthInfo.Exec with InteractiveMode forced to Never, so its RESTConfig() does not
// fail clientcmd's exec validation.
func TestArgoSecretParsesExecProviderVariant(t *testing.T) {
	data := loadFixtureSecretData(t, 1) // the exec-plugin EKS cluster

	conn, err := parseArgoClusterSecret(data)
	if err != nil {
		t.Fatalf("parseArgoClusterSecret: %v", err)
	}
	if conn.AuthInfo == nil || conn.AuthInfo.Exec == nil {
		t.Fatalf("exec auth not parsed: %#v", conn.AuthInfo)
	}
	if conn.AuthInfo.Exec.Command != "aws" {
		t.Fatalf("exec command = %q, want aws", conn.AuthInfo.Exec.Command)
	}
	if conn.AuthInfo.Token != "" {
		t.Fatalf("exec variant should not carry a bearer token, got %q", conn.AuthInfo.Token)
	}
	// RESTConfig must succeed: an unset InteractiveMode would make clientcmd reject
	// the exec config. The parser pins Never, so this proves the seam works.
	restCfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig for exec variant: %v", err)
	}
	if restCfg.ExecProvider == nil || restCfg.ExecProvider.Command != "aws" {
		t.Fatalf("ExecProvider not carried onto rest.Config: %#v", restCfg.ExecProvider)
	}
}

// TestArgoSecretSourceForbiddenHostIsSourceError proves the source-level error
// model: when the host LIST is RBAC-forbidden (or the host is down), the source
// returns a non-nil error rather than a partial set -- which the caller surfaces
// without blanking the other sources.
func TestArgoSecretSourceForbiddenHostIsSourceError(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "secrets",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "", Resource: "secrets"}, "", nil)
		})

	got, err := discoverArgoSecrets(context.Background(), client, "argocd")
	if err == nil {
		t.Fatalf("forbidden host list must be a source-level error, got results %#v", got)
	}
	if got != nil {
		t.Fatalf("source error must return nil results, got %#v", got)
	}
	if !strings.Contains(err.Error(), "argocd") {
		t.Fatalf("source error should name the namespace: %v", err)
	}
}

// TestArgoSecretSourceMalformedSecretSkipped proves the per-Secret skip-with-error
// model: a fake clientset with one well-formed and one malformed cluster Secret
// yields BOTH as discoveredCluster -- the good one with Config set and Err nil,
// the bad one with Err set and Config nil -- so a single bad Secret never blanks
// its siblings.
func TestArgoSecretSourceMalformedSecretSkipped(t *testing.T) {
	good := argoClusterSecret("good", map[string]string{
		"name":   "good-cluster",
		"server": "https://good.example.com",
		"config": `{"bearerToken":"tok","tlsClientConfig":{"insecure":true}}`,
	})
	// Malformed: config is not valid JSON, so parseArgoClusterSecret errors.
	bad := argoClusterSecret("bad", map[string]string{
		"name":   "bad-cluster",
		"server": "https://bad.example.com",
		"config": `{not-json`,
	})

	client := fake.NewClientset(good, bad)

	got, err := discoverArgoSecrets(context.Background(), client, "argocd")
	if err != nil {
		t.Fatalf("a malformed sibling must not be a source error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both secrets returned, got %d: %#v", len(got), got)
	}

	byName := map[string]discoveredCluster{}
	for _, dc := range got {
		byName[dc.Name] = dc
	}

	// The good Secret parses to a named connection (conn.Name = "good-cluster")
	// with Config set and no error.
	g, ok := byName["good-cluster"]
	if !ok {
		t.Fatalf("good cluster missing from results: %#v", got)
	}
	if g.Err != nil {
		t.Fatalf("good cluster carried an error: %v", g.Err)
	}
	if g.Config == nil || g.Config.Host != "https://good.example.com" {
		t.Fatalf("good cluster Config not built: %#v", g.Config)
	}
	if g.Source != SourceSecret {
		t.Fatalf("good cluster Source = %v", g.Source)
	}
	if g.Spec["argo_secret"] != "good" {
		t.Fatalf("good cluster Spec.argo_secret = %v, want the Secret name 'good'", g.Spec["argo_secret"])
	}

	// The bad Secret is keyed by the Secret name (parse failed before a cluster
	// name was known) with Err set and Config nil.
	b, ok := byName["bad"]
	if !ok {
		t.Fatalf("bad secret missing from results (should be skip-with-error): %#v", got)
	}
	if b.Err == nil {
		t.Fatal("bad secret must carry a typed error")
	}
	if b.Config != nil {
		t.Fatalf("bad secret must have nil Config, got %#v", b.Config)
	}
	var cle *ContextLoadError
	if !errors.As(b.Err, &cle) {
		t.Fatalf("bad secret error should be a *ContextLoadError, got %T: %v", b.Err, b.Err)
	}
}

// argoClusterSecret builds a labelled Argo cluster Secret with the given Data
// (raw string bytes, as the Secret API presents stringData) for the fake
// clientset.
func argoClusterSecret(name string, stringData map[string]string) *corev1.Secret {
	data := make(map[string][]byte, len(stringData))
	for k, v := range stringData {
		data[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "argocd",
			Labels:    map[string]string{"argocd.argoproj.io/secret-type": "cluster"},
		},
		Data: data,
	}
}

// TestArgoSecretRejectsUnsupportedAndEmptyName pins the two fork-review fixes:
// an awsAuthConfig-only Secret (readout ships no aws binary) is skip-with-error
// rather than a silently-anonymous "healthy" cluster, and an empty name (the one
// untrusted-content identifier) is skip-with-error rather than an empty-keyed
// cluster that collides with siblings.
func TestArgoSecretRejectsUnsupportedAndEmptyName(t *testing.T) {
	t.Run("awsAuthConfig sole credential is skip-with-error", func(t *testing.T) {
		data := map[string][]byte{
			"name":   []byte("eks-prod"),
			"server": []byte("https://eks.example"),
			"config": []byte(`{"awsAuthConfig":{"clusterName":"eks-prod","roleARN":"arn:aws:iam::1:role/r"},"tlsClientConfig":{"insecure":false}}`),
		}
		if _, err := parseArgoClusterSecret(data); err == nil {
			t.Fatal("awsAuthConfig-only secret must be skipped with an error, not parsed anonymous")
		}
	})
	t.Run("empty name is skip-with-error", func(t *testing.T) {
		data := map[string][]byte{
			"server": []byte("https://no-name.example"),
			"config": []byte(`{"bearerToken":"tok","tlsClientConfig":{"insecure":true}}`),
		}
		if _, err := parseArgoClusterSecret(data); err == nil {
			t.Fatal("empty-name secret must be skipped with an error")
		}
	})
	t.Run("awsAuthConfig alongside bearerToken still parses via the bearer", func(t *testing.T) {
		data := map[string][]byte{
			"name":   []byte("eks-prod"),
			"server": []byte("https://eks.example"),
			"config": []byte(`{"bearerToken":"tok","awsAuthConfig":{"clusterName":"eks-prod"},"tlsClientConfig":{"insecure":true}}`),
		}
		conn, err := parseArgoClusterSecret(data)
		if err != nil {
			t.Fatalf("bearerToken + awsAuthConfig should parse via the bearer: %v", err)
		}
		if conn.AuthInfo == nil || conn.AuthInfo.Token != "tok" {
			t.Fatalf("expected bearer token, got %#v", conn.AuthInfo)
		}
	})
}
