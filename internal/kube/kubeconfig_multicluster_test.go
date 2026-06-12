package kube

import (
	"path/filepath"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// These tests pin the maintainer's primary deployment pattern -- a multi-context
// kubeconfig rendered into a Secret and mounted, with readout pointed at it via
// kubeconfigPath -- against the security changes that landed earlier: strict
// passthrough (Unit 6) and the source-aware exec credential-plugin gate (Unit 4).
// They run under DEFAULT policy (no overrides): the kubeconfig source defaults to
// the cloud-plugin Allowlist, so a static-token file loads every context and an
// allowlisted exec context keeps its ExecProvider.

// writeExecKubeconfig writes a kubeconfig with one static-token context plus one
// context whose user authenticates via an exec credential plugin running
// execCommand, and returns the path. It mirrors writeKubeconfig but adds the exec
// auth shape so the credential-plugin gate has something to act on.
func writeExecKubeconfig(t *testing.T, staticName, staticServer, execName, execServer, execCommand string) string {
	t.Helper()
	raw := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			staticName: {Server: staticServer},
			execName:   {Server: execServer},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			staticName: {Token: "static-tok"},
			execName: {Exec: &clientcmdapi.ExecConfig{
				APIVersion:      "client.authentication.k8s.io/v1",
				Command:         execCommand,
				Args:            []string{"eks", "get-token"},
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			}},
		},
		Contexts: map[string]*clientcmdapi.Context{
			staticName: {Cluster: staticName, AuthInfo: staticName},
			execName:   {Cluster: execName, AuthInfo: execName},
		},
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(raw, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMultiContextStaticKubeconfigLoadsAllContexts is non-regression (a): a
// multi-context kubeconfig whose contexts use STATIC tokens loads every context
// and yields a usable Config under the DEFAULT gate. Strict passthrough being off
// and the no-exec default do not touch a static-token file -- it must still serve.
func TestMultiContextStaticKubeconfigLoadsAllContexts(t *testing.T) {
	kubeconfigPath := writeKubeconfig(t, map[string]string{
		"prod-eks":    "https://prod-eks.example",
		"staging-gke": "https://staging-gke.example",
		"dev-kind":    "https://dev-kind.example",
	})
	// DEFAULT policy: empty policy string + nil allowlist = source-aware default.
	cfg := &appconfig.Config{KubeconfigPath: kubeconfigPath}
	gate := resolveCredentialPluginGate("", nil)

	discovered, err := discoverKubeconfig(cfg, gate)
	if err != nil {
		t.Fatalf("discoverKubeconfig: %v", err)
	}
	if len(discovered) != 3 {
		t.Fatalf("expected all 3 contexts to load, got %d: %#v", len(discovered), discovered)
	}
	for _, dc := range discovered {
		if dc.Err != nil {
			t.Fatalf("static-token context %q must load cleanly under default policy, got err: %v", dc.Name, dc.Err)
		}
		if dc.Config == nil {
			t.Fatalf("static-token context %q must carry a usable Config", dc.Name)
		}
		if dc.Config.BearerToken != "t" {
			t.Fatalf("static-token context %q lost its token: %q", dc.Name, dc.Config.BearerToken)
		}
	}
}

// TestMultiContextExecKubeconfigPreservesAllowlistedExec is non-regression (b): a
// kubeconfig context whose user is an exec plugin running an ALLOWLISTED command
// (aws) keeps its ExecProvider after the credential-plugin gate runs on the
// kubeconfig source under the DEFAULT (source-aware Allowlist seeded with the cloud
// plugins) policy. The gate must NOT strip or reject it. The sibling static-token
// context still loads. This verifies the exec CONFIG survives the policy; it does
// NOT exercise a live connection (no plugin binary in the test env).
func TestMultiContextExecKubeconfigPreservesAllowlistedExec(t *testing.T) {
	kubeconfigPath := writeExecKubeconfig(t,
		"prod-static", "https://prod-static.example",
		"prod-eks", "https://prod-eks.example", "aws")
	cfg := &appconfig.Config{KubeconfigPath: kubeconfigPath}
	gate := resolveCredentialPluginGate("", nil)

	discovered, err := discoverKubeconfig(cfg, gate)
	if err != nil {
		t.Fatalf("discoverKubeconfig: %v", err)
	}

	byName := map[string]discoveredCluster{}
	for _, dc := range discovered {
		byName[dc.Name] = dc
	}

	exec, ok := byName["prod-eks"]
	if !ok {
		t.Fatalf("exec context prod-eks not discovered: %#v", discovered)
	}
	if exec.Err != nil {
		t.Fatalf("allowlisted aws exec context must NOT be rejected under the default kubeconfig Allowlist: %v", exec.Err)
	}
	if exec.Config == nil {
		t.Fatal("allowlisted exec context must carry a built Config (not dropped)")
	}
	// The core assertion: the exec credential survives the gate intact, never
	// stripped to an anonymous connection.
	if exec.Config.ExecProvider == nil {
		t.Fatal("the gate must PRESERVE ExecProvider for an allowlisted command, not strip it")
	}
	if got := exec.Config.ExecProvider.Command; got != "aws" {
		t.Fatalf("preserved exec command changed: got %q, want aws", got)
	}

	static, ok := byName["prod-static"]
	if !ok {
		t.Fatalf("static context prod-static not discovered: %#v", discovered)
	}
	if static.Err != nil || static.Config == nil {
		t.Fatalf("sibling static-token context must still load alongside the exec context: %#v", static)
	}
}
