package kube

import (
	"fmt"
	"log/slog"
	"path"

	"k8s.io/client-go/rest"
)

// Exec credential-plugin policy values. They mirror
// appconfig.CredentialPluginPolicy* but are duplicated here so internal/kube does
// not depend on the config string constants at the gate site; resolveCredentialPluginPolicy
// translates the operator override into one of these.
type credentialPluginPolicy int

const (
	// credPolicyDenyAll rejects every exec credential plugin: a connection whose
	// only credential is an exec plugin becomes a broken cluster.
	credPolicyDenyAll credentialPluginPolicy = iota
	// credPolicyAllowlist permits only exec commands whose basename (or exact full
	// path) is on the effective allowlist.
	credPolicyAllowlist
	// credPolicyAllowAll permits any exec command (the historical behavior).
	credPolicyAllowAll
)

// defaultExecAllowlist is the source-aware default allowlist seeded for
// operator-owned sources (kubeconfig, static) when no global override is set. It
// names the common cloud exec plugins so EKS/GKE-exec kubeconfig installs keep
// working out of the box. The discovered Argo-Secret source does NOT get this
// seed: a cluster actor can create those Secrets, so its default is DenyAll.
var defaultExecAllowlist = []string{
	"aws",
	"aws-iam-authenticator",
	"gke-gcloud-auth-plugin",
	"kubelogin",
	"kubectl-oidc_login",
}

// credentialPluginGate is the resolved exec-plugin policy applied to every built
// rest.Config. It is derived once from the operator config (resolveCredentialPluginGate)
// and threaded to each of the three connection build sites, so all sources share
// one gate.
type credentialPluginGate struct {
	// override is the global policy when the operator set
	// kube.credentialPluginPolicy; it applies to every source uniformly. When
	// overrideSet is false the gate is SOURCE-AWARE (see policyForSource).
	override    credentialPluginPolicy
	overrideSet bool
	// allowlist is the operator-additive extension applied on top of the
	// source-aware default seed (or as the entire allowlist under a global
	// Allowlist override).
	allowlist []string
}

// resolveCredentialPluginGate translates the operator config (the global policy
// string + the additive allowlist) into the runtime gate. An unrecognized policy
// string is treated as unset (source-aware); the config layer already rejects an
// invalid value at parse, so this is defense in depth, never a second gate.
func resolveCredentialPluginGate(policy string, allowlist []string) credentialPluginGate {
	g := credentialPluginGate{allowlist: allowlist}
	switch policy {
	case "DenyAll":
		g.override, g.overrideSet = credPolicyDenyAll, true
	case "Allowlist":
		g.override, g.overrideSet = credPolicyAllowlist, true
	case "AllowAll":
		g.override, g.overrideSet = credPolicyAllowAll, true
	default:
		g.overrideSet = false
	}
	return g
}

// policyForSource returns the effective policy and effective allowlist for one
// connection source. A global override applies to all sources with the operator
// allowlist as the whole set. Without an override the default is source-aware:
//   - the Argo-Secret source defaults to DenyAll (a cluster actor can create
//     those Secrets -- the real injection vector);
//   - operator-owned sources (kubeconfig, static, in-cluster) default to
//     Allowlist seeded with the common cloud plugins plus the operator additions.
func (g credentialPluginGate) policyForSource(source Source) (credentialPluginPolicy, []string) {
	if g.overrideSet {
		return g.override, g.allowlist
	}
	if source == SourceSecret {
		// Argo-Secret default: deny every exec plugin. The operator allowlist is
		// intentionally ignored here -- extending the deny-default would require a
		// deliberate global Allowlist override, not an additive list.
		return credPolicyDenyAll, nil
	}
	effective := make([]string, 0, len(defaultExecAllowlist)+len(g.allowlist))
	effective = append(effective, defaultExecAllowlist...)
	effective = append(effective, g.allowlist...)
	return credPolicyAllowlist, effective
}

// applyCredentialPluginPolicy is the single exec-plugin gate. It is called at
// every rest.Config build site (kubeconfig, static, Argo) so all three share one
// policy. A config with no exec plugin passes untouched. A configured exec plugin
// is audit-logged (cluster name + command basename only, never args/env) and
// either allowed or REJECTED -- on denial it returns an error so the caller marks
// the connection a broken cluster. It NEVER strips ExecProvider and falls through:
// a context whose only credential is the exec would be silently downgraded to
// anonymous, the exact failure this gate exists to prevent.
func (g credentialPluginGate) applyCredentialPluginPolicy(cfg *rest.Config, name string, source Source) error {
	if cfg == nil || cfg.ExecProvider == nil {
		return nil
	}
	command := cfg.ExecProvider.Command
	base := path.Base(command)
	policy, allowlist := g.policyForSource(source)

	switch policy {
	case credPolicyAllowAll:
		slog.Info("exec credential plugin allowed",
			"cluster", name, "source", source.String(), "command", base, "policy", "AllowAll")
		return nil
	case credPolicyAllowlist:
		if execCommandAllowed(command, base, allowlist) {
			slog.Info("exec credential plugin allowed",
				"cluster", name, "source", source.String(), "command", base, "policy", "Allowlist")
			return nil
		}
	}

	// DenyAll, or Allowlist with no match: reject. Naming the basename (not the
	// full command/args/env) keeps the audit log free of injected material.
	slog.Warn("exec credential plugin denied",
		"cluster", name, "source", source.String(), "command", base)
	return fmt.Errorf("exec credential plugin %q is not permitted for the %s source "+
		"(set kube.credentialPluginPolicy/credentialPluginAllowlist to permit it)", base, source.String())
}

// execCommandAllowed reports whether an exec command is permitted by the effective
// allowlist. An allowlist entry that contains a "/" is an absolute/explicit path
// and must match the full command string exactly; a bare entry matches the
// command's basename. This lets an operator pin an exact binary path while a bare
// name keeps the kubectl-style basename match.
func execCommandAllowed(command, base string, allowlist []string) bool {
	for _, entry := range allowlist {
		if entry == "" {
			continue
		}
		if containsSlash(entry) {
			if command == entry {
				return true
			}
			continue
		}
		if base == entry {
			return true
		}
	}
	return false
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}
