package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes content to a temp readout.yaml and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "readout.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const sampleConfig = `
port: 9090
includeNamespaces:
  - default
  - prod-.*
excludeNamespaces:
  - kube-.*
clusters:
  - name: one
    url: https://one
  - name: two
    url: https://two
kubeconfigContexts: [ctx-a, ctx-b]
clusterLabelSelector:
  region: fra1
objectLinks:
  pods:
    - href: https://pods/{name}
      icon: box
      title: Pod link
labelLinks:
  app:
    - href: https://apps/{value}
labelColumns:
  pods: app
hiddenColumns:
  pods: Age
customColumns:
  pods: "Owner:{.metadata.ownerReferences[0].name}"
  nodes: "Zone:{.metadata.labels.zone}"
preferredApiVersions:
  ingresses: networking.k8s.io/v1
search:
  defaultResourceTypes: [pods, services]
  offeredResourceTypes: [pods, nodes]
themeOptions: [light, dark]
externalClusters:
  - name: prod
    url: https://readout.example
showContainerLogs: true
includeSecrets: true
sidebar:
  - label: Workloads
    resources: [pods, deployments]
  - label: Cluster
    resources: [nodes]
auth:
  mode: headers
  oidc:
    clientId: file-client
  trustedHeaders:
    user: X-User
`

func TestParseLoadsYAMLConfigEnvOverridesAndDefaults(t *testing.T) {
	t.Setenv("READOUT_OIDC_CLIENT_ID", "env-client")
	t.Setenv("READOUT_SESSION_SECRET", "env-secret")

	path := writeConfig(t, sampleConfig)
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Port != 9090 || cfg.AuthMode != AuthModeHeaders || cfg.TrustedHeaderUser != "X-User" {
		t.Fatalf("unexpected scalar config: %#v", cfg)
	}
	if cfg.TrustedHeaderEmail != "X-Forwarded-Email" || cfg.DefaultTheme != "dark" {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
	// env must WIN over a non-empty file value: the fixture sets auth.oidc.clientId
	// to "file-client", so seeing "env-client" proves precedence, not mere fallback.
	// (If firstNonEmpty's arg order were flipped to file-wins, this would catch it.)
	if cfg.OIDCClientID != "env-client" || cfg.SessionSecret != "env-secret" {
		t.Fatalf("env override not applied over file value: client=%q secret=%q", cfg.OIDCClientID, cfg.SessionSecret)
	}
	if len(cfg.IncludeNamespaces) != 2 || !cfg.IncludeNamespaces[1].MatchString("prod-api") || !cfg.ExcludeNamespaces[0].MatchString("kube-system") {
		t.Fatalf("namespace regexes not compiled: include=%v exclude=%v", cfg.IncludeNamespaces, cfg.ExcludeNamespaces)
	}
	if cfg.Clusters["two"] != "https://two" || cfg.KubeconfigContexts[1] != "ctx-b" {
		t.Fatalf("clusters/contexts not resolved: %#v %#v", cfg.Clusters, cfg.KubeconfigContexts)
	}
	if cfg.ClusterLabelSelector["region"] != "fra1" || cfg.ObjectLinks["pods"][0].Icon != "box" || cfg.LabelLinks["app"][0].Title != "External link" {
		t.Fatalf("selectors/links not resolved: %#v %#v %#v", cfg.ClusterLabelSelector, cfg.ObjectLinks, cfg.LabelLinks)
	}
	if cfg.DefaultCustomColumns["pods"] != "Owner:{.metadata.ownerReferences[0].name}" || cfg.PreferredAPIVersions["ingresses"] != "networking.k8s.io/v1" {
		t.Fatalf("custom columns/preferred versions not resolved: %#v %#v", cfg.DefaultCustomColumns, cfg.PreferredAPIVersions)
	}
	if cfg.SearchDefaultResourceTypes[0] != "pods" || cfg.SearchOfferedResourceTypes[1] != "nodes" {
		t.Fatalf("search resource types not resolved: %#v", cfg)
	}
	if cfg.ExternalClusters["prod"] != "https://readout.example" || !cfg.ShowContainerLogs || !cfg.IncludeSecrets {
		t.Fatalf("external/boolean config not resolved: %#v", cfg)
	}
}

// TestParseSidebarKeepsDeclaredOrder pins that sidebar groups are an ordered
// slice and the iterated order matches the file (NOT alphabetical -- "Workloads"
// is declared before "Cluster", which would flip under alphabetical sorting).
func TestParseSidebarKeepsDeclaredOrder(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sidebar) != 2 {
		t.Fatalf("sidebar groups = %d, want 2: %#v", len(cfg.Sidebar), cfg.Sidebar)
	}
	if cfg.Sidebar[0].Label != "Workloads" || cfg.Sidebar[1].Label != "Cluster" {
		t.Fatalf("sidebar order = [%q %q], want [Workloads Cluster] (declaration order, not alphabetical)", cfg.Sidebar[0].Label, cfg.Sidebar[1].Label)
	}
	if len(cfg.Sidebar[0].Resources) != 2 || cfg.Sidebar[0].Resources[1] != "deployments" {
		t.Fatalf("sidebar resources not resolved in order: %#v", cfg.Sidebar[0].Resources)
	}
}

// TestParsePortFlagOverridesFile pins that the bootstrap --port flag overrides
// the file value, and that an empty/omitted --config still resolves defaults.
func TestParsePortFlagOverridesFileAndEmptyConfigDefaults(t *testing.T) {
	path := writeConfig(t, "port: 9090\n")
	cfg, err := Parse([]string{"--config", path, "--port", "7000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 7000 {
		t.Fatalf("--port did not override file: %d", cfg.Port)
	}

	cfg, err = Parse([]string{"--debug"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 || cfg.AuthMode != AuthModeNone || cfg.DefaultTheme != "dark" || !cfg.Debug {
		t.Fatalf("empty-config defaults wrong: %#v", cfg)
	}
}

func TestResolveReadsOAuthSecretFilesAndValidatesErrors(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "id")
	secretFile := filepath.Join(dir, "secret")
	if err := os.WriteFile(idFile, []byte("client-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretFile, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "auth:\n  oidc:\n    clientIdFile: "+idFile+"\n    clientSecretFile: "+secretFile+"\n")
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDCClientID != "client-id" || cfg.OIDCClientSecret != "client-secret" {
		t.Fatalf("secret files not read: %#v", cfg)
	}

	missing := filepath.Join(dir, "missing")
	for _, content := range []string{
		"auth:\n  mode: bad\n",
		"search:\n  maxConcurrency: 0\n",
		"includeNamespaces: ['[']\n",
		"auth:\n  oidc:\n    clientIdFile: " + missing + "\n",
	} {
		p := writeConfig(t, content)
		if _, err := Parse([]string{"--config", p}); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", content)
		}
	}

	// Unknown field rejected (strict parse) and a missing --config file errors.
	if _, err := Parse([]string{"--config", writeConfig(t, "bogusField: 1\n")}); err == nil {
		t.Fatal("unknown config field should be rejected")
	}
	if _, err := Parse([]string{"--config", missing}); err == nil {
		t.Fatal("missing config file should error")
	}
}

func TestAddress(t *testing.T) {
	if got := Address(9090); got != ":9090" {
		t.Fatalf("Address = %q", got)
	}
}
