package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	appconfig "github.com/kbelokon/readout/internal/config"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Manager struct {
	cfg      appconfig.Config
	clusters map[string]*Cluster
	broken   []BrokenCluster
}

// BrokenCluster is a connection that failed to load: it is skipped (never failing
// its siblings) and surfaced here so the web layer can show a misconfigured /
// unreachable cluster instead of silently blanking it (D3).
type BrokenCluster struct {
	Name   string
	Source Source
	Err    error
}

// ContextLoadError is a typed per-context load failure attached to a discovered
// cluster. When set, the cluster is surfaced as broken and skipped rather than
// aborting the whole reload.
type ContextLoadError struct {
	Name   string
	Source Source
	Err    error
}

func (e *ContextLoadError) Error() string {
	return fmt.Sprintf("cluster %q (%s): %v", e.Name, e.Source, e.Err)
}

func (e *ContextLoadError) Unwrap() error { return e.Err }

func NewManager(ctx context.Context, cfg *appconfig.Config) (*Manager, error) {
	m := &Manager{cfg: *cfg, clusters: map[string]*Cluster{}}
	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// Reload rebuilds the cluster set from every configured source through one sink
// (D3). A source-level failure (e.g. an explicit kubeconfig that cannot be read)
// is fatal and returned. A per-context failure -- a malformed connection, a
// post-sanitization name collision (D8c loader-half), or a client build error --
// is recorded as a BrokenCluster and skipped, never failing its siblings.
func (m *Manager) Reload(ctx context.Context) error {
	discovered, err := discoverAll(ctx, &m.cfg)
	if err != nil {
		return err
	}
	next := map[string]*Cluster{}
	origin := map[string]string{}
	var broken []BrokenCluster
	for _, item := range discovered {
		if item.Err != nil {
			broken = append(broken, BrokenCluster{Name: item.Name, Source: item.Source, Err: item.Err})
			continue
		}
		name := SanitizeClusterName(item.Name)
		// D8c loader-half: two distinct configured names that sanitize to the same
		// key must not silently collapse (the old next[name] last-write-wins).
		if prior, dup := origin[name]; dup {
			broken = append(broken, BrokenCluster{
				Name:   item.Name,
				Source: item.Source,
				Err:    fmt.Errorf("sanitized name %q collides with already-loaded cluster %q", name, prior),
			})
			continue
		}
		client, err := NewClient(item.Config, m.cfg.PreferredAPIVersions, m.cfg.IncludeSecrets)
		if err != nil {
			broken = append(broken, BrokenCluster{Name: item.Name, Source: item.Source, Err: err})
			continue
		}
		origin[name] = item.Name
		next[name] = &Cluster{
			Name:   name,
			URL:    item.Config.Host,
			Source: item.Source,
			Labels: item.Labels,
			Spec:   item.Spec,
			Client: client,
		}
	}
	m.clusters = next
	m.broken = broken
	return nil
}

func (m *Manager) Clusters() []*Cluster {
	out := make([]*Cluster, 0, len(m.clusters))
	for _, cluster := range m.clusters {
		out = append(out, cluster)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Broken returns the clusters that failed to load on the last reload, sorted by
// name. The connection set (Clusters) excludes them; this is the surfaced,
// non-fatal error channel (D3).
func (m *Manager) Broken() []BrokenCluster {
	out := append([]BrokenCluster(nil), m.broken...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) Get(name string) (*Cluster, bool) {
	cluster, ok := m.clusters[name]
	return cluster, ok
}

func (m *Manager) Select(nameCSV string) ([]*Cluster, bool, error) {
	if nameCSV == "" || nameCSV == AllClusters {
		return m.Clusters(), true, nil
	}
	var result []*Cluster
	for _, name := range strings.Split(nameCSV, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cluster, ok := m.Get(name)
		if !ok {
			return nil, false, fmt.Errorf("cluster %q not found", name)
		}
		result = append(result, cluster)
	}
	return result, false, nil
}

// discoveredCluster is one connection produced by a source. Err carries a
// per-context load failure: when set, Config is nil and the cluster is surfaced
// as broken (D3) rather than aborting the reload.
type discoveredCluster struct {
	Name   string
	Config *rest.Config
	Source Source
	Labels map[string]string
	Spec   map[string]any
	Err    error
}

// discoverAll funnels every configured source into one list (D3). Static and
// kubeconfig clusters coexist (no longer mutually exclusive). When neither a
// static list nor an explicit kubeconfig is configured, it falls back to the
// in-cluster ServiceAccount, then the default kubeconfig.
func discoverAll(ctx context.Context, cfg *appconfig.Config) ([]discoveredCluster, error) {
	_ = ctx // reserved for sources that make live calls at discovery (D6)
	var out []discoveredCluster
	explicit := false

	if len(cfg.Clusters) > 0 {
		out = append(out, discoverStatic(cfg)...)
		explicit = true
	}
	if cfg.KubeconfigPath != "" {
		kc, err := discoverKubeconfig(cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, kc...)
		explicit = true
	}
	if explicit {
		return out, nil
	}

	inCluster, inErr := rest.InClusterConfig()
	if inErr == nil {
		return append(out, discoveredCluster{
			Name:   "local",
			Config: inCluster,
			Source: SourceInCluster,
			Labels: map[string]string{},
			Spec:   map[string]any{},
		}), nil
	}
	kc, err := discoverKubeconfig(cfg)
	if err == nil && len(kc) > 0 {
		return append(out, kc...), nil
	}
	if err != nil {
		return nil, err
	}
	return nil, inErr
}

func discoverStatic(cfg *appconfig.Config) []discoveredCluster {
	var result []discoveredCluster
	for i := range cfg.Clusters {
		cc := cfg.Clusters[i]
		dc := discoveredCluster{
			Name:   cc.Name,
			Source: SourceStatic,
			Labels: map[string]string{},
			Spec:   map[string]any{"api_server_url": cc.Server},
		}
		// Non-https hosts make clientcmd skip the TLS/auth merge
		// (IsConfigTransportTLS), silently dropping configured credentials/CA --
		// the exact "silent anonymous" class D8 exists to kill, through a
		// different door. Surface it as a typed per-context error instead.
		if err := guardStaticTransport(cc); err != nil {
			dc.Err = err
			result = append(result, dc)
			continue
		}
		restCfg, err := connectionFromClusterConfig(cc).RESTConfig()
		if err != nil {
			dc.Err = err
			result = append(result, dc)
			continue
		}
		dc.Config = restCfg
		result = append(result, dc)
	}
	return result
}

// guardStaticTransport rejects a static cluster that carries TLS/auth fields on a
// non-https server, where clientcmd would silently drop them. Impersonation is
// excluded: clientcmd applies the impersonation block unconditionally (it is not
// gated on transport TLS), so it is not silently dropped.
func guardStaticTransport(cc appconfig.ClusterConnection) error {
	if strings.HasPrefix(cc.Server, "https://") {
		return nil
	}
	carriesTLSGated := cc.Token != "" || cc.TokenFile != "" ||
		cc.CertificateAuthority != "" || len(cc.CertificateAuthorityData) > 0 ||
		cc.ClientCertificate != "" || len(cc.ClientCertificateData) > 0 ||
		cc.ClientKey != "" || len(cc.ClientKeyData) > 0 ||
		cc.TLSServerName != ""
	if carriesTLSGated {
		return fmt.Errorf("cluster %q sets TLS/auth fields but server %q is not https:// "+
			"(clientcmd would drop them); use an https server", cc.Name, cc.Server)
	}
	return nil
}

// connectionFromClusterConfig maps a configured cluster (kubeconfig field
// semantics) onto the canonical Connection triple. TLS fields go on the
// api.Cluster; auth/impersonation fields go on the api.AuthInfo, which is left
// nil when nothing is configured (the anonymous static case where identity is
// supplied per request). Token and TokenFile pass through as configured: a
// tokenFile-only cluster keeps AuthInfo.TokenFile set so clientcmd arms the
// ~1-minute rotation re-read (D8b); an inline token is used verbatim.
func connectionFromClusterConfig(cc appconfig.ClusterConnection) *Connection {
	cluster := &clientcmdapi.Cluster{
		Server:                   cc.Server,
		CertificateAuthority:     cc.CertificateAuthority,
		CertificateAuthorityData: cc.CertificateAuthorityData,
		InsecureSkipTLSVerify:    cc.InsecureSkipTLSVerify,
		TLSServerName:            cc.TLSServerName,
	}

	auth := &clientcmdapi.AuthInfo{
		Token:                 cc.Token,
		TokenFile:             cc.TokenFile,
		ClientCertificate:     cc.ClientCertificate,
		ClientCertificateData: cc.ClientCertificateData,
		ClientKey:             cc.ClientKey,
		ClientKeyData:         cc.ClientKeyData,
		Impersonate:           cc.Impersonate.User,
		ImpersonateGroups:     cc.Impersonate.Groups,
		ImpersonateUID:        cc.Impersonate.UID,
	}
	if isZeroAuthInfo(auth) {
		auth = nil
	}

	return &Connection{
		Name:     cc.Name,
		Source:   SourceStatic,
		Cluster:  cluster,
		AuthInfo: auth,
	}
}

// isZeroAuthInfo reports whether none of the auth fields readout maps are set, so
// the connection should stay anonymous (nil AuthInfo) rather than carry an empty
// credential block.
func isZeroAuthInfo(a *clientcmdapi.AuthInfo) bool {
	return a.Token == "" && a.TokenFile == "" &&
		a.ClientCertificate == "" && len(a.ClientCertificateData) == 0 &&
		a.ClientKey == "" && len(a.ClientKeyData) == 0 &&
		a.Impersonate == "" && len(a.ImpersonateGroups) == 0 && a.ImpersonateUID == ""
}

// discoverKubeconfig loads kubeconfig contexts as connections. The whole-file
// load failing is a source-level error (returned); a single context that fails
// to resolve is surfaced as a typed per-context error and skipped (D3).
func discoverKubeconfig(cfg *appconfig.Config) ([]discoveredCluster, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.KubeconfigPath != "" {
		loadingRules.ExplicitPath = cfg.KubeconfigPath
	}
	raw, err := loadingRules.Load()
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	for _, ctx := range cfg.KubeconfigContexts {
		selected[ctx] = true
	}
	var result []discoveredCluster
	for name := range raw.Contexts {
		if len(selected) > 0 && !selected[name] {
			continue
		}
		dc := discoveredCluster{
			Name:   name,
			Source: SourceKubeconfig,
			Labels: map[string]string{},
			Spec:   map[string]any{"context": name},
		}
		clientCfg := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{CurrentContext: name}, loadingRules)
		restCfg, err := clientCfg.ClientConfig()
		if err != nil {
			dc.Err = err
			result = append(result, dc)
			continue
		}
		dc.Config = restCfg
		result = append(result, dc)
	}
	return result, nil
}

// invalidClusterNameChar matches any character not allowed in a sanitized
// cluster name. Hoisted to a package var so the regexp compiles once at init
// instead of on every SanitizeClusterName call.
var invalidClusterNameChar = regexp.MustCompile(`[^a-zA-Z0-9:_.-]`)

func SanitizeClusterName(name string) string {
	return invalidClusterNameChar.ReplaceAllString(name, ":")
}
