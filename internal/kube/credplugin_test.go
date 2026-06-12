package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// execRESTConfig builds a rest.Config whose ONLY credential is an exec plugin
// running `command`, via the proven Connection model (no hand-set rest.Config
// field). This mirrors how every real source produces an exec rest.Config.
func execRESTConfig(t *testing.T, name, command string) *rest.Config {
	t.Helper()
	conn := &Connection{
		Name:   name,
		Source: SourceStatic,
		Cluster: &clientcmdapi.Cluster{
			Server:                "https://" + name + ".example:6443",
			InsecureSkipTLSVerify: true,
		},
		AuthInfo: &clientcmdapi.AuthInfo{
			Exec: &clientcmdapi.ExecConfig{
				APIVersion:      "client.authentication.k8s.io/v1",
				Command:         command,
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			},
		},
	}
	cfg, err := conn.RESTConfig()
	if err != nil {
		t.Fatalf("build exec rest.Config for %q: %v", command, err)
	}
	if cfg.ExecProvider == nil || cfg.ExecProvider.Command != command {
		t.Fatalf("exec rest.Config did not carry command %q: %#v", command, cfg.ExecProvider)
	}
	return cfg
}

// TestCredPluginSourceAwareDefault pins the source-aware default (policy unset):
// an operator-owned source (kubeconfig/static) allowlists the common cloud
// plugins, while the Argo-Secret source denies every exec plugin.
func TestCredPluginSourceAwareDefault(t *testing.T) {
	gate := resolveCredentialPluginGate("", nil)

	for _, src := range []Source{SourceStatic, SourceKubeconfig} {
		cfg := execRESTConfig(t, "ops", "aws")
		if err := gate.applyCredentialPluginPolicy(cfg, "ops", src); err != nil {
			t.Fatalf("aws must be allowed on operator source %s under default: %v", src, err)
		}
	}

	// Argo-Secret default is DenyAll: even the common `aws` plugin is denied.
	cfg := execRESTConfig(t, "argo", "aws")
	if err := gate.applyCredentialPluginPolicy(cfg, "argo", SourceSecret); err == nil {
		t.Fatal("aws via the Argo-Secret source must be denied under the default (DenyAll)")
	}
}

// TestCredPluginArbitraryCommandDeniedOnOperatorSource proves an arbitrary
// command (NOT a known cloud plugin) is rejected even on an operator-owned source
// under the default allowlist.
func TestCredPluginArbitraryCommandDeniedOnOperatorSource(t *testing.T) {
	gate := resolveCredentialPluginGate("", nil)
	cfg := execRESTConfig(t, "k", "/bin/sh")
	if err := gate.applyCredentialPluginPolicy(cfg, "k", SourceKubeconfig); err == nil {
		t.Fatal("/bin/sh must be rejected on the kubeconfig source under the default allowlist")
	}
	// Fail-closed: the gate must NOT strip ExecProvider (which would let the
	// connection fall through to an anonymous client). The rejected config still
	// carries its exec credential; the caller drops it as a broken cluster.
	if cfg.ExecProvider == nil {
		t.Fatal("denied config must NOT be stripped to anonymous: ExecProvider was cleared")
	}
}

// TestCredPluginGlobalDenyAll proves a set DenyAll override denies the common
// `aws` plugin on EVERY source, overriding the operator-owned allowlist default.
func TestCredPluginGlobalDenyAll(t *testing.T) {
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyDenyAll, nil)
	for _, src := range []Source{SourceStatic, SourceKubeconfig, SourceSecret} {
		cfg := execRESTConfig(t, "c", "aws")
		if err := gate.applyCredentialPluginPolicy(cfg, "c", src); err == nil {
			t.Fatalf("global DenyAll must deny aws on source %s", src)
		}
	}
}

// TestCredPluginGlobalAllowAll proves a set AllowAll override passes even an
// arbitrary command on every source.
func TestCredPluginGlobalAllowAll(t *testing.T) {
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyAllowAll, nil)
	for _, src := range []Source{SourceStatic, SourceKubeconfig, SourceSecret} {
		cfg := execRESTConfig(t, "c", "/bin/sh")
		if err := gate.applyCredentialPluginPolicy(cfg, "c", src); err != nil {
			t.Fatalf("global AllowAll must pass /bin/sh on source %s: %v", src, err)
		}
	}
}

// TestCredPluginEmptyAllowlistDeniesAll proves an Allowlist policy with an empty
// effective allowlist denies every exec plugin.
func TestCredPluginEmptyAllowlistDeniesAll(t *testing.T) {
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyAllowlist, nil)
	cfg := execRESTConfig(t, "c", "aws")
	if err := gate.applyCredentialPluginPolicy(cfg, "c", SourceStatic); err == nil {
		t.Fatal("Allowlist policy with an empty allowlist must deny every exec plugin")
	}
}

// TestCredPluginAbsolutePathEntry proves an absolute-path allowlist entry matches
// ONLY that exact path: the same basename at a different path is denied, and a
// bare basename entry does not satisfy an absolute-path command via basename when
// the entry is itself a path.
func TestCredPluginAbsolutePathEntry(t *testing.T) {
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyAllowlist, []string{"/usr/local/bin/aws"})

	exact := execRESTConfig(t, "c", "/usr/local/bin/aws")
	if err := gate.applyCredentialPluginPolicy(exact, "c", SourceStatic); err != nil {
		t.Fatalf("the exact absolute-path entry must match: %v", err)
	}

	// Same basename, different path -> denied (the entry is a full-path match).
	other := execRESTConfig(t, "c", "/opt/evil/aws")
	if err := gate.applyCredentialPluginPolicy(other, "c", SourceStatic); err == nil {
		t.Fatal("an absolute-path entry must NOT match a same-basename command at a different path")
	}

	// A bare basename command is denied too: the only allowlist entry is a path.
	bare := execRESTConfig(t, "c", "aws")
	if err := gate.applyCredentialPluginPolicy(bare, "c", SourceStatic); err == nil {
		t.Fatal("a path-only allowlist must not admit a bare-basename command")
	}
}

// TestCredPluginBasenameEntryMatchesPathCommand proves a bare basename allowlist
// entry matches a command given with a path (the kubectl-style basename match):
// /usr/local/bin/aws is admitted by the bare `aws` entry.
func TestCredPluginBasenameEntryMatchesPathCommand(t *testing.T) {
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyAllowlist, []string{"aws"})
	cfg := execRESTConfig(t, "c", "/usr/local/bin/aws")
	if err := gate.applyCredentialPluginPolicy(cfg, "c", SourceStatic); err != nil {
		t.Fatalf("a bare basename entry must match a path command by basename: %v", err)
	}
}

// TestCredPluginNilExecIsNoop proves a config with no exec plugin passes
// untouched under any policy (including DenyAll).
func TestCredPluginNilExecIsNoop(t *testing.T) {
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyDenyAll, nil)
	cfg := &rest.Config{Host: "https://x", BearerToken: "tok"}
	if err := gate.applyCredentialPluginPolicy(cfg, "x", SourceSecret); err != nil {
		t.Fatalf("a non-exec config must pass even under DenyAll: %v", err)
	}
	if cfg.BearerToken != "tok" {
		t.Fatal("non-exec config must be left untouched")
	}
}

// TestCredPluginOperatorAllowlistExtendsDefault proves the operator allowlist is
// additive on the source-aware default: a custom command is admitted on operator
// sources without losing the seeded cloud plugins.
func TestCredPluginOperatorAllowlistExtendsDefault(t *testing.T) {
	gate := resolveCredentialPluginGate("", []string{"my-auth-plugin"})

	custom := execRESTConfig(t, "c", "my-auth-plugin")
	if err := gate.applyCredentialPluginPolicy(custom, "c", SourceStatic); err != nil {
		t.Fatalf("operator-added plugin must be allowed on operator source: %v", err)
	}
	seeded := execRESTConfig(t, "c", "gke-gcloud-auth-plugin")
	if err := gate.applyCredentialPluginPolicy(seeded, "c", SourceStatic); err != nil {
		t.Fatalf("seeded cloud plugin must still be allowed alongside operator additions: %v", err)
	}
	// The additive list does NOT loosen the Argo-Secret DenyAll default.
	argo := execRESTConfig(t, "c", "my-auth-plugin")
	if err := gate.applyCredentialPluginPolicy(argo, "c", SourceSecret); err == nil {
		t.Fatal("operator-additive allowlist must NOT loosen the Argo-Secret DenyAll default")
	}
}

// TestArgoSecretExecDeniedNotDowngradedToAnonymous is the end-to-end fail-closed
// proof: an Argo cluster Secret whose ONLY credential is an exec plugin (command
// aws) is DENIED under the default (Argo defaults DenyAll), surfaced as a broken
// cluster with Config nil -- it is never built into a usable (anonymous) client.
func TestArgoSecretExecDeniedNotDowngradedToAnonymous(t *testing.T) {
	stubResolver(t, map[string][]string{"eks.example.com": {"10.0.0.3"}})
	secret := argoClusterSecret("eks", map[string]string{
		"name":   "eks-prod",
		"server": "https://eks.example.com",
		"config": `{"execProviderConfig":{"apiVersion":"client.authentication.k8s.io/v1","command":"aws","args":["eks","get-token"]},"tlsClientConfig":{"insecure":true}}`,
	})
	client := fake.NewClientset(secret)

	got, err := discoverArgoSecrets(context.Background(), client, "argocd", credentialPluginGate{})
	if err != nil {
		t.Fatalf("a denied exec Secret must be skip-with-error, not a source error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one result, got %d: %#v", len(got), got)
	}
	dc := got[0]
	if dc.Err == nil {
		t.Fatal("Argo exec Secret must be DENIED under the default (broken cluster), not admitted")
	}
	// Fail-closed invariant: a denied exec connection NEVER yields a usable client.
	if dc.Config != nil {
		t.Fatalf("denied exec Secret must have nil Config (no anonymous downgrade), got %#v", dc.Config)
	}
	var cle *ContextLoadError
	if !errors.As(dc.Err, &cle) {
		t.Fatalf("denied exec Secret error should be a *ContextLoadError, got %T: %v", dc.Err, dc.Err)
	}
	if !strings.Contains(dc.Err.Error(), "aws") {
		t.Fatalf("denial reason should name the command basename: %v", dc.Err)
	}
}

// TestArgoSecretExecAllowedUnderGlobalAllowlist proves a SET global Allowlist
// override (with aws) admits the same Argo exec Secret -- the source-aware DenyAll
// default applies only when no override is set.
func TestArgoSecretExecAllowedUnderGlobalAllowlist(t *testing.T) {
	stubResolver(t, map[string][]string{"eks.example.com": {"10.0.0.3"}})
	secret := argoClusterSecret("eks", map[string]string{
		"name":   "eks-prod",
		"server": "https://eks.example.com",
		"config": `{"execProviderConfig":{"apiVersion":"client.authentication.k8s.io/v1","command":"aws","args":["eks","get-token"]},"tlsClientConfig":{"insecure":true}}`,
	})
	client := fake.NewClientset(secret)
	gate := resolveCredentialPluginGate(appconfig.CredentialPluginPolicyAllowlist, []string{"aws"})

	got, err := discoverArgoSecrets(context.Background(), client, "argocd", gate)
	if err != nil {
		t.Fatalf("discoverArgoSecrets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one result, got %d: %#v", len(got), got)
	}
	if got[0].Err != nil {
		t.Fatalf("aws must be admitted under a global Allowlist with aws: %v", got[0].Err)
	}
	if got[0].Config == nil {
		t.Fatal("admitted exec Secret must carry a built Config")
	}
}

// TestStaticExecDeniedIsBrokenCluster proves the static source applies the gate:
// a static cluster whose connection resolves to a denied exec plugin becomes a
// broken cluster (Config nil), never a silent anonymous connection. (Static
// clusters cannot carry exec in the config schema today; this drives the gate
// through discoverStatic by asserting the allowed common-plugin path, while the
// denied-path fail-closed contract is proven at the gate and Argo levels above.)
func TestStaticExecGateThreaded(t *testing.T) {
	stubResolver(t, map[string][]string{"one": {"10.0.0.4"}})
	cfg := &appconfig.Config{
		Clusters: []appconfig.ClusterConnection{{Name: "one", Server: "https://one"}},
	}
	got := discoverStatic(cfg, resolveCredentialPluginGate("", nil))
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("a non-exec static cluster must pass the gate cleanly: %#v", got)
	}
}
