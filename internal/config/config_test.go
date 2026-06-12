package config

import (
	"os"
	"path/filepath"
	"strings"
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
    server: https://one
  - name: two
    server: https://two
    insecureSkipTlsVerify: true
kubeconfigContexts: [ctx-a, ctx-b]
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
	if len(cfg.Clusters) != 2 || cfg.Clusters[1].Name != "two" || cfg.Clusters[1].Server != "https://two" || !cfg.Clusters[1].InsecureSkipTLSVerify || cfg.KubeconfigContexts[1] != "ctx-b" {
		t.Fatalf("clusters/contexts not resolved: %#v %#v", cfg.Clusters, cfg.KubeconfigContexts)
	}
	if cfg.ObjectLinks["pods"][0].Icon != "box" || cfg.LabelLinks["app"][0].Title != "External link" {
		t.Fatalf("links not resolved: %#v %#v", cfg.ObjectLinks, cfg.LabelLinks)
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

// TestHiddenColumnsShipV2DefaultsAndFileOverrides pins the default-hidden
// sets: an empty config ships the noise-off defaults (nodes + pods), a file
// entry for a kind REPLACES that kind's default outright (an explicit empty
// value re-shows everything), and untouched kinds keep their shipped default.
func TestHiddenColumnsShipV2DefaultsAndFileOverrides(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultHiddenColumns["nodes"] != "External-IP,OS-Image,Kernel-Version,Created" {
		t.Fatalf("nodes default = %q, want the v2 noise-off set", cfg.DefaultHiddenColumns["nodes"])
	}
	if cfg.DefaultHiddenColumns["pods"] != "IP,Nominated Node,Readiness Gates" {
		t.Fatalf("pods default = %q, want the v2 noise-off set", cfg.DefaultHiddenColumns["pods"])
	}

	path := writeConfig(t, "hiddenColumns:\n  nodes: \"\"\n  pods: Status\n  secrets: Data\n")
	cfg, err = Parse([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DefaultHiddenColumns["nodes"]; got != "" {
		t.Fatalf("explicit-empty nodes entry = %q, want the default disabled", got)
	}
	if got := cfg.DefaultHiddenColumns["pods"]; got != "Status" {
		t.Fatalf("file pods entry = %q, want it to replace the default outright", got)
	}
	if got := cfg.DefaultHiddenColumns["secrets"]; got != "Data" {
		t.Fatalf("file secrets entry = %q, want the file value", got)
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
	path := writeConfig(t, "port: 9090\nmetricsPort: 9091\n")
	cfg, err := Parse([]string{"--config", path, "--port", "7000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 7000 || cfg.MetricsPort != 9091 {
		t.Fatalf("port config mismatch: port=%d metricsPort=%d", cfg.Port, cfg.MetricsPort)
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

func TestOIDCRequiresRedirectURL(t *testing.T) {
	// In explicit oidc mode, neither redirectUrl nor publicUrl present is a load
	// error naming both as the fix.
	explicitNoRedirect := `
auth:
  mode: oidc
  oidc:
    clientId: client
    issuerUrl: https://issuer.example
`
	_, err := Parse([]string{"--config", writeConfig(t, explicitNoRedirect)})
	if err == nil || !strings.Contains(err.Error(), "auth.oidc.redirectUrl or publicUrl") {
		t.Fatalf("Parse() error = %v, want redirectUrl-or-publicUrl requirement", err)
	}

	ok := `
auth:
  mode: oidc
  oidc:
    clientId: client
    issuerUrl: https://issuer.example
    redirectUrl: https://readout.example/oauth2/callback
`
	if _, err := Parse([]string{"--config", writeConfig(t, ok)}); err != nil {
		t.Fatalf("OIDC config with redirectUrl should parse: %v", err)
	}

	// publicUrl alone satisfies the requirement (it derives the callback at
	// request time), so this is the new pass case with no redirectUrl set.
	okPublic := `
publicUrl: https://readout.example
auth:
  mode: oidc
  oidc:
    clientId: client
    issuerUrl: https://issuer.example
`
	if _, err := Parse([]string{"--config", writeConfig(t, okPublic)}); err != nil {
		t.Fatalf("OIDC config with publicUrl (no redirectUrl) should parse: %v", err)
	}

	t.Setenv("READOUT_OIDC_REDIRECT_URL", "https://env.example/oauth2/callback")
	cfg, err := Parse([]string{"--config", writeConfig(t, `
auth:
  mode: oidc
  oidc:
    clientId: client
    issuerUrl: https://issuer.example
`)})
	if err != nil {
		t.Fatalf("OIDC config with env redirectUrl should parse: %v", err)
	}
	if cfg.OIDCRedirectURL != "https://env.example/oauth2/callback" {
		t.Fatalf("env redirectUrl = %q", cfg.OIDCRedirectURL)
	}
}

// TestOIDCPromotionRemoved pins that OIDC is never auto-enabled from endpoint
// config: a none-mode config carrying OIDC fields (issuer, or the
// authorize/token pair) is a load error naming the one-line fix, while none-mode
// with no OIDC fields, and explicit oidc mode, both still load. The ordering
// sub-case pins that for a promoted config the promotion message wins over the
// redirectUrl-required message.
func TestOIDCPromotionRemoved(t *testing.T) {
	errorCases := []struct {
		name    string
		content string
	}{
		{
			name: "none mode + issuer",
			content: `
auth:
  oidc:
    clientId: client
    issuerUrl: https://issuer.example
`,
		},
		{
			name: "none mode + authorize/token pair",
			content: `
auth:
  oidc:
    clientId: client
    authorizeUrl: https://issuer.example/authorize
    tokenUrl: https://issuer.example/token
`,
		},
	}
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]string{"--config", writeConfig(t, tc.content)})
			if err == nil || !strings.Contains(err.Error(), `auth.mode is "none"`) {
				t.Fatalf("Parse() error = %v, want promotion-removed error", err)
			}
		})
	}

	// none mode with no OIDC fields loads fine (the field is inert).
	if _, err := Parse([]string{"--config", writeConfig(t, "auth:\n  mode: none\n")}); err != nil {
		t.Fatalf("none mode without OIDC fields should load: %v", err)
	}

	// Ordering: a promoted config that ALSO lacks redirectUrl trips both checks;
	// the promotion message must win because it is the actionable one. The
	// redirectUrl set here proves we are not seeing the redirect error by accident.
	ordering := `
auth:
  oidc:
    clientId: client
    issuerUrl: https://issuer.example
    redirectUrl: https://readout.example/oauth2/callback
`
	_, err := Parse([]string{"--config", writeConfig(t, ordering)})
	if err == nil || !strings.Contains(err.Error(), `auth.mode is "none"`) {
		t.Fatalf("Parse() error = %v, want promotion message to win", err)
	}
	if strings.Contains(err.Error(), "redirectUrl or publicUrl is required") {
		t.Fatalf("redirectUrl message leaked ahead of promotion message: %v", err)
	}
}

// TestPublicURLShape pins the origin-only validation of publicUrl: a bare origin
// loads as-is, a single trailing slash is normalized away, and a path-bearing,
// query-bearing, or non-http(s) value is a load error. publicUrl is inert beyond
// shape validation when OIDC is unused.
func TestPublicURLShape(t *testing.T) {
	valid := []struct {
		raw, want string
	}{
		{"https://readout.example", "https://readout.example"},
		{"https://readout.example/", "https://readout.example"},
		{"http://readout.example:8080", "http://readout.example:8080"},
	}
	for _, tc := range valid {
		t.Run("valid "+tc.raw, func(t *testing.T) {
			cfg, err := Parse([]string{"--config", writeConfig(t, "publicUrl: "+tc.raw+"\n")})
			if err != nil {
				t.Fatalf("publicUrl %q should load: %v", tc.raw, err)
			}
			if cfg.PublicURL != tc.want {
				t.Fatalf("publicUrl %q normalized to %q, want %q", tc.raw, cfg.PublicURL, tc.want)
			}
		})
	}

	invalid := []string{
		"https://readout.example/readout", // subpath
		"https://readout.example/?x=1",    // query
		"ftp://readout.example",           // wrong scheme
		"/just/a/path",                    // no scheme/host
	}
	for _, raw := range invalid {
		t.Run("invalid "+raw, func(t *testing.T) {
			if _, err := Parse([]string{"--config", writeConfig(t, "publicUrl: \""+raw+"\"\n")}); err == nil {
				t.Fatalf("publicUrl %q should be rejected", raw)
			}
		})
	}

	// A typo of the key is rejected by strict parse (not silently ignored).
	if _, err := Parse([]string{"--config", writeConfig(t, "publickUrl: https://x.example\n")}); err == nil {
		t.Fatal("typo'd publicUrl key should be rejected by strict parse")
	}
}

// TestSessionSecretFile pins the file-secret lane for the session secret: the
// file is read when no env secret is set, the env var wins over the file, a
// missing file is a load error, and a typo of the key is rejected by strict
// parse.
func TestSessionSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "session-secret")
	if err := os.WriteFile(secretFile, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{"--config", writeConfig(t, "sessionSecretFile: "+secretFile+"\n")})
	if err != nil {
		t.Fatalf("sessionSecretFile should be read: %v", err)
	}
	if cfg.SessionSecret != "file-secret" {
		t.Fatalf("SessionSecret = %q, want file-secret", cfg.SessionSecret)
	}

	// env wins over the file value. Scoped to a subtest so t.Setenv's cleanup
	// restores the env before the missing-file case runs (which needs it unset).
	t.Run("env wins over file", func(t *testing.T) {
		t.Setenv("READOUT_SESSION_SECRET", "env-secret")
		cfg, err := Parse([]string{"--config", writeConfig(t, "sessionSecretFile: "+secretFile+"\n")})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionSecret != "env-secret" {
			t.Fatalf("SessionSecret = %q, want env-secret (env must win over file)", cfg.SessionSecret)
		}
	})

	// A missing file is a load error (same lane as clientSecretFile).
	missing := filepath.Join(dir, "missing")
	if _, err := Parse([]string{"--config", writeConfig(t, "sessionSecretFile: "+missing+"\n")}); err == nil {
		t.Fatal("missing sessionSecretFile should be a load error")
	}

	// A typo of the key is rejected by strict parse, never an inline secret key.
	if _, err := Parse([]string{"--config", writeConfig(t, "sessionSecret: inline\n")}); err == nil {
		t.Fatal("inline sessionSecret key must not exist (only sessionSecretFile)")
	}
}

func TestNamespacePatternAnchored(t *testing.T) {
	cfg, err := Parse([]string{"--config", writeConfig(t, "includeNamespaces: ['prod-.*']\nexcludeNamespaces: ['kube-system']\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IncludeNamespaces[0].MatchString("prod-api") || cfg.IncludeNamespaces[0].MatchString("xprod-api") {
		t.Fatal("include namespace pattern should match whole namespace names only")
	}
	if !cfg.ExcludeNamespaces[0].MatchString("kube-system") || cfg.ExcludeNamespaces[0].MatchString("my-kube-system-2") {
		t.Fatal("exclude namespace pattern should match whole namespace names only")
	}
}

// TestIconOverride pins the Tier-3 per-resource icon schema (LOCKED): a
// top-level `resources:` list of typed {kind, group, icon} objects resolves
// into Config.ResourceIcons keyed by kind+group (NOT a plural-keyed map, and
// NOT the flat sidebar.resources []string). It also pins that the override
// surface inherits the existing reject-unknown-keys contract: a typo inside a
// resources entry (or a stray top-level key) fails fast at parse.
func TestIconOverride(t *testing.T) {
	const overrideConfig = `
resources:
  - kind: Cluster
    group: postgresql.cnpg.io
    icon: /icons/pg.svg
  - kind: Rollout
    group: argoproj.io
    icon: "emoji:🐙"
`
	cfg, err := Parse([]string{"--config", writeConfig(t, overrideConfig)})
	if err != nil {
		t.Fatalf("Parse override config: %v", err)
	}

	// Round-trips into a kind+group keyed map (the locked schema).
	pg := cfg.ResourceIcons[ResourceIconKey{Kind: "Cluster", Group: "postgresql.cnpg.io"}]
	if pg != "/icons/pg.svg" {
		t.Fatalf("Cluster/postgresql.cnpg.io override = %q, want /icons/pg.svg (map: %#v)", pg, cfg.ResourceIcons)
	}
	rollout := cfg.ResourceIcons[ResourceIconKey{Kind: "Rollout", Group: "argoproj.io"}]
	if rollout != "emoji:🐙" {
		t.Fatalf("Rollout/argoproj.io override = %q, want emoji:🐙", rollout)
	}

	// Keyed on kind+GROUP, not kind alone: the same kind in a different group
	// must NOT resolve to this override (proves the key is the pair).
	if got := cfg.ResourceIcons[ResourceIconKey{Kind: "Cluster", Group: "other.example"}]; got != "" {
		t.Fatalf("Cluster in a different group leaked the override: %q", got)
	}

	// An empty config yields a non-nil, empty map (no override) -- callers can
	// index it without a nil check.
	empty, err := Parse([]string{"--debug"})
	if err != nil {
		t.Fatal(err)
	}
	if empty.ResourceIcons == nil {
		t.Fatal("ResourceIcons should be a non-nil empty map when unset")
	}
	if len(empty.ResourceIcons) != 0 {
		t.Fatalf("ResourceIcons should be empty when unset: %#v", empty.ResourceIcons)
	}

	// Reject-unknown-keys still holds for the override surface: a typo'd field
	// inside a resources entry fails fast (strict parse), not silently ignored.
	bad := "resources:\n  - kind: Pod\n    grpup: x\n    icon: pod\n"
	if _, err := Parse([]string{"--config", writeConfig(t, bad)}); err == nil {
		t.Fatal("unknown field inside a resources entry should be rejected")
	}
}

// TestClusterSchemaParsesTripleFields pins the new kubeconfig-semantics cluster
// surface: per-cluster server/CA-data/TLS/token/clientcert/impersonate
// parse into the runtime ClusterConnection, base64 *Data decodes like kubeconfig,
// and the one retained cluster/auth key (clusterAuthUseSessionToken) still parses.
func TestClusterSchemaParsesTripleFields(t *testing.T) {
	// "Zm9vLWNh" is base64("foo-ca"); a YAML string decodes into the []byte field
	// as base64, exactly like kubeconfig's certificate-authority-data.
	const content = `
clusterAuthUseSessionToken: true
clusters:
  - name: prod
    server: https://prod.example
    certificateAuthorityData: Zm9vLWNh
    tlsServerName: prod.internal
    tokenFile: /var/run/secrets/token
    impersonate:
      user: system:serviceaccount:ns:robot
      groups: [viewers]
      uid: uid-1
`
	cfg, err := Parse([]string{"--config", writeConfig(t, content)})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.ClusterAuthUseSessionToken {
		t.Fatal("clusterAuthUseSessionToken not retained/parsed")
	}
	if len(cfg.Clusters) != 1 {
		t.Fatalf("clusters = %#v", cfg.Clusters)
	}
	c := cfg.Clusters[0]
	if c.Name != "prod" || c.Server != "https://prod.example" || c.TLSServerName != "prod.internal" || c.TokenFile != "/var/run/secrets/token" {
		t.Fatalf("scalar cluster fields not resolved: %#v", c)
	}
	if string(c.CertificateAuthorityData) != "foo-ca" {
		t.Fatalf("certificateAuthorityData base64 not decoded: %q", string(c.CertificateAuthorityData))
	}
	if c.Impersonate.User != "system:serviceaccount:ns:robot" || len(c.Impersonate.Groups) != 1 || c.Impersonate.Groups[0] != "viewers" || c.Impersonate.UID != "uid-1" {
		t.Fatalf("impersonate not resolved: %#v", c.Impersonate)
	}
}

// TestClusterDuplicateNameRejected pins the config-parse half: two byte-identical
// cluster names are a startup error naming the cluster, not silent last-write-wins.
func TestClusterDuplicateNameRejected(t *testing.T) {
	const content = `
clusters:
  - name: prod
    server: https://a
  - name: prod
    server: https://b
`
	_, err := Parse([]string{"--config", writeConfig(t, content)})
	if err == nil {
		t.Fatal("duplicate cluster name should be rejected")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Fatalf("duplicate-name error should name the cluster: %v", err)
	}
}

// TestRemovedClusterKeysRejected pins that the old cluster/auth keys dropped in
// the schema redesign are no longer in the schema, so strict parse rejects them rather than
// silently ignoring a stale config.
func TestRemovedClusterKeysRejected(t *testing.T) {
	for _, key := range []string{
		"clusterRegistryUrl: https://reg\n",
		"clusterLabelSelector:\n  region: fra1\n",
		"clusterAuthTokenPath: /token\n",
		"clusterRegistryBearerTokenPath: /token\n",
	} {
		if _, err := Parse([]string{"--config", writeConfig(t, key)}); err == nil {
			t.Fatalf("removed key %q should be rejected by strict parse", key)
		}
	}
}

func TestAddress(t *testing.T) {
	if got := Address(9090); got != ":9090" {
		t.Fatalf("Address = %q", got)
	}
}
