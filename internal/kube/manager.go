package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
}

func NewManager(ctx context.Context, cfg *appconfig.Config) (*Manager, error) {
	m := &Manager{cfg: *cfg, clusters: map[string]*Cluster{}}
	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Reload(ctx context.Context) error {
	discovered, err := discoverClusters(ctx, &m.cfg)
	if err != nil {
		return err
	}
	next := map[string]*Cluster{}
	for _, item := range discovered {
		if !labelsMatch(m.cfg.ClusterLabelSelector, item.Labels) {
			continue
		}
		name := SanitizeClusterName(item.Name)
		client, err := NewClient(item.Config, m.cfg.PreferredAPIVersions, m.cfg.IncludeSecrets)
		if err != nil {
			return fmt.Errorf("cluster %s: %w", item.Name, err)
		}
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

type discoveredCluster struct {
	Name   string
	Config *rest.Config
	Source Source
	Labels map[string]string
	Spec   map[string]any
}

func discoverClusters(ctx context.Context, cfg *appconfig.Config) ([]discoveredCluster, error) {
	switch {
	case len(cfg.Clusters) > 0:
		return discoverStatic(cfg), nil
	case cfg.ClusterRegistryURL != "":
		return discoverRegistry(ctx, cfg)
	case cfg.KubeconfigPath != "":
		return discoverKubeconfig(cfg)
	default:
		inCluster, inErr := rest.InClusterConfig()
		if inErr == nil {
			return []discoveredCluster{{Name: "local", Config: inCluster, Source: SourceInCluster, Labels: map[string]string{}, Spec: map[string]any{}}}, nil
		}
		clusters, err := discoverKubeconfig(cfg)
		if err == nil && len(clusters) > 0 {
			return clusters, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, inErr
	}
}

func discoverStatic(cfg *appconfig.Config) []discoveredCluster {
	var result []discoveredCluster
	for name, host := range cfg.Clusters {
		// Build the rest.Config through the canonical Connection model (D1) rather
		// than a bare rest.Config{Host}. clientcmd produces the config, so a static
		// cluster that later carries CA/TLS/auth fields populates them for free.
		// A malformed host that clientcmd rejects is skipped here; the multi-source
		// loader (D3) replaces this with typed per-context error surfacing.
		conn := &Connection{
			Name:    name,
			Source:  SourceStatic,
			Cluster: &clientcmdapi.Cluster{Server: host},
		}
		restCfg, err := conn.RESTConfig()
		if err != nil {
			continue
		}
		result = append(result, discoveredCluster{
			Name:   name,
			Config: restCfg,
			Source: SourceStatic,
			Labels: map[string]string{},
			Spec:   map[string]any{"api_server_url": host},
		})
	}
	return result
}

func discoverRegistry(ctx context.Context, cfg *appconfig.Config) ([]discoveredCluster, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.ClusterRegistryURL, "/")+"/kubernetes-clusters", nil)
	if err != nil {
		return nil, err
	}
	if cfg.ClusterRegistryOAuth2BearerTokenPath != "" {
		token, err := os.ReadFile(cfg.ClusterRegistryOAuth2BearerTokenPath)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cluster registry returned %s", resp.Status)
	}
	// The registry is a fixed external contract, so its response decodes once
	// into the typed registryResponse below (the known fields: lifecycle_status,
	// alias, api_server_url). The whole row is ALSO captured raw (json.RawMessage
	// -> Raw) because Spec is an opaque pass-through and registryLabels reads
	// extra label-source keys the struct does not enumerate.
	var payload registryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var result []discoveredCluster
	for _, item := range payload.Items {
		if item.LifecycleStatus != "" && item.LifecycleStatus != "ready" {
			continue
		}
		if item.Alias == "" || item.APIServerURL == "" {
			continue
		}
		restCfg := &rest.Config{Host: item.APIServerURL}
		if cfg.ClusterAuthTokenPath != "" {
			if err := applyBearerToken(restCfg, cfg.ClusterAuthTokenPath); err != nil {
				return nil, err
			}
		}
		row := item.row()
		result = append(result, discoveredCluster{Name: item.Alias, Config: restCfg, Labels: registryLabels(row), Spec: row})
	}
	return result, nil
}

// registryResponse is the typed external-contract shape of the cluster-registry
// /kubernetes-clusters response. Each item decodes its known fields into typed
// struct fields and retains the full row (Raw) so the opaque Spec pass-through
// and the registryLabels label-source keys still resolve.
type registryResponse struct {
	Items []registryCluster `json:"items"`
}

type registryCluster struct {
	Alias           string          `json:"alias"`
	APIServerURL    string          `json:"api_server_url"`
	LifecycleStatus string          `json:"lifecycle_status"`
	Raw             json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the known contract fields and stashes the raw item bytes
// so row() can reconstruct the opaque map the Spec/labels path expects.
func (c *registryCluster) UnmarshalJSON(data []byte) error {
	type alias registryCluster
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = registryCluster(a)
	c.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// row returns the registry item as a generic map for the opaque Spec
// pass-through and the registryLabels lookup of label-source keys.
func (c *registryCluster) row() map[string]any {
	row := map[string]any{}
	if len(c.Raw) > 0 {
		_ = json.Unmarshal(c.Raw, &row)
	}
	return row
}

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
		clientCfg := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{CurrentContext: name}, loadingRules)
		restCfg, err := clientCfg.ClientConfig()
		if err != nil {
			return nil, err
		}
		if cfg.ClusterAuthTokenPath != "" {
			if err := applyBearerToken(restCfg, cfg.ClusterAuthTokenPath); err != nil {
				return nil, err
			}
		}
		result = append(result, discoveredCluster{Name: name, Config: restCfg, Source: SourceKubeconfig, Labels: kubeconfigLabels(raw, name), Spec: map[string]any{"context": name}})
	}
	return result, nil
}

func applyBearerToken(cfg *rest.Config, path string) error {
	token, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg.BearerToken = strings.TrimSpace(string(token))
	cfg.BearerTokenFile = ""
	return nil
}

func registryLabels(row map[string]any) map[string]string {
	labels := map[string]string{}
	for _, key := range []string{"id", "channel", "environment", "infrastructure_account", "region"} {
		if val, ok := row[key].(string); ok && val != "" {
			labels[strings.ReplaceAll(key, "_", "-")] = val
		}
	}
	return labels
}

func kubeconfigLabels(raw *clientcmdapi.Config, contextName string) map[string]string {
	ctx := raw.Contexts[contextName]
	if ctx == nil {
		return map[string]string{}
	}
	return map[string]string{}
}

// invalidClusterNameChar matches any character not allowed in a sanitized
// cluster name. Hoisted to a package var so the regexp compiles once at init
// instead of on every SanitizeClusterName call.
var invalidClusterNameChar = regexp.MustCompile(`[^a-zA-Z0-9:_.-]`)

func SanitizeClusterName(name string) string {
	return invalidClusterNameChar.ReplaceAllString(name, ":")
}

func labelsMatch(selector, labels map[string]string) bool {
	for key, expected := range selector {
		if strings.HasSuffix(key, "!") {
			if labels[strings.TrimSuffix(key, "!")] == expected {
				return false
			}
		} else if labels[key] != expected {
			return false
		}
	}
	return true
}
