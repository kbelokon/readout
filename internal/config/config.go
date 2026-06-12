package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	AuthModeNone    = "none"
	AuthModeHeaders = "headers"
	AuthModeOIDC    = "oidc"
)

type Link struct {
	Href  string
	Icon  string
	Title string
}

// ResourceIconKey identifies a Tier-3 icon override by the resource's Kind and
// API group (NOT its plural). Keying on kind+group lets one override target a
// specific CRD family member (e.g. {Cluster, postgresql.cnpg.io}) without
// colliding with a same-named kind in another group.
type ResourceIconKey struct {
	Kind  string
	Group string
}

// SidebarGroup is one ordered sidebar category: a heading label and the
// resource-type plurals listed under it. The sidebar is a slice (not a map) so
// the operator's declared group order is preserved through to the rendered
// navigation; a Go map would re-randomize it on every request.
type SidebarGroup struct {
	Label     string
	Resources []string
}

// ClusterConnection is one statically-configured cluster carrying kubeconfig
// field semantics: the fields map 1:1 onto client-go's api.Cluster/api.AuthInfo,
// so the kube loader builds the canonical connection triple from them and lets
// clientcmd produce the rest.Config (no hand-set TLS/auth). The *Data fields are
// []byte so a YAML string decodes as base64 exactly like kubeconfig's
// certificate-authority-data / client-certificate-data / client-key-data.
type ClusterConnection struct {
	Name                     string
	Server                   string
	CertificateAuthority     string
	CertificateAuthorityData []byte
	InsecureSkipTLSVerify    bool
	TLSServerName            string
	Token                    string
	TokenFile                string
	ClientCertificate        string
	ClientCertificateData    []byte
	ClientKey                string
	ClientKeyData            []byte
	Impersonate              ClusterImpersonation
}

// ClusterImpersonation is the per-cluster static service identity (act-as). It
// maps onto api.AuthInfo.Impersonate / ImpersonateGroups / ImpersonateUID. It is
// mutually exclusive with per-request passthrough: when passthrough fires, the
// kube layer clears it so the request evaluates as the viewer.
type ClusterImpersonation struct {
	User   string
	Groups []string
	UID    string
}

// ArgoCDSource configures the Argo CD cluster-Secret discovery source: the
// kube loader lists Secrets labelled argocd.argoproj.io/secret-type=cluster in a
// host cluster and parses each into a connection. HostCluster names a configured
// cluster (in Clusters) to run the list against, or "" to use the in-cluster
// ServiceAccount. Namespace is where Argo's cluster Secrets live; resolve()
// defaults it to "argocd" when empty.
type ArgoCDSource struct {
	HostCluster string
	Namespace   string
}

// Config is the resolved runtime configuration. Field types match what the
// rest of the service consumes directly (s.cfg.X); the YAML file is parsed into
// the unexported fileConfig and folded into this shape by resolve().
type Config struct {
	Port          int
	MetricsPort   int
	ListenAddress string
	ShowVersion   bool

	IncludeNamespaces []*regexp.Regexp
	ExcludeNamespaces []*regexp.Regexp

	Clusters                   []ClusterConnection
	KubeconfigPath             string
	KubeconfigContexts         []string
	ClusterAuthUseSessionToken bool
	ArgoCD                     *ArgoCDSource
	ShowContainerLogs          bool
	NoAccessLogs               bool
	IncludeSecrets             bool
	Debug                      bool
	TemplatesPath              string
	StaticAssetsPath           string
	ObjectLinks                map[string][]Link
	LabelLinks                 map[string][]Link
	TimestampLinks             map[string][]Link
	ResourceIcons              map[ResourceIconKey]string
	Sidebar                    []SidebarGroup
	SearchDefaultResourceTypes []string
	SearchOfferedResourceTypes []string
	SearchMaxConcurrency       int
	DefaultLabelColumns        map[string]string
	DefaultHiddenColumns       map[string]string
	DefaultCustomColumns       map[string]string
	PreferredAPIVersions       map[string]string
	DefaultTheme               string
	ThemeOptions               []string
	ExternalClusters           map[string]string
	AuthMode                   string
	TrustedHeaderUser          string
	TrustedHeaderEmail         string
	TrustedHeaderGroups        string
	TrustedProxyCIDRs          []netip.Prefix
	OIDCIssuerURL              string
	OIDCClientID               string
	OIDCClientSecret           string
	OAuth2ClientIDFile         string
	OAuth2ClientSecretFile     string
	OIDCRedirectURL            string
	OAuth2AuthorizeURL         string
	OAuth2TokenURL             string
	OAuth2Scope                string
	SessionSecret              string
	SessionSecretFile          string
	PublicURL                  string
	AuthorizationHookURL       string
	// AuthorizationHookIncludeTokens lists which OAuth tokens (access|id|refresh)
	// the authorization hook receives. It defaults to empty: by default the hook
	// gets only identity claims, never the bearer token set, so a buggy or
	// compromised hook cannot harvest tokens. Only the listed tokens are sent.
	AuthorizationHookIncludeTokens []string
	ResourcePrerenderHookURL       string
}

// fileLink is the on-disk form of Link for the YAML file. sigs.k8s.io/yaml
// routes YAML through JSON, so only json: tags are honoured (yaml: tags would be
// ignored); the file schema is therefore the JSON-subset of YAML.
type fileLink struct {
	Href  string `json:"href"`
	Icon  string `json:"icon"`
	Title string `json:"title"`
}

// fileIconOverride is one Tier-3 per-resource icon override as written in the
// file: a typed {kind, group, icon} object (the ICON_SYSTEM.md shape), keyed by
// kind+group. It deliberately borrows only the typed-struct-with-Icon-field
// pattern from fileLink -- NOT a plural-keyed map -- so an override targets a
// CRD family member precisely. The top-level list is `resources:`.
type fileIconOverride struct {
	Kind  string `json:"kind"`
	Group string `json:"group"`
	Icon  string `json:"icon"`
}

// fileSidebarGroup is one sidebar category as written in the file: an ordered
// list element so the declared order survives parsing.
type fileSidebarGroup struct {
	Label     string   `json:"label"`
	Resources []string `json:"resources"`
}

// fileCluster is one external-readout cross-link (name + base URL) written as a
// list element. It backs `externalClusters` only -- the in-cluster connection
// surface uses the richer fileClusterConn below.
type fileCluster struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// fileClusterConn is one statically-configured cluster connection on the on-disk
// schema, using kubeconfig field names/semantics. It is a list element so a
// duplicate name is an explicit, detectable startup error rather than a
// silent last-write-wins. The *Data fields are []byte: sigs.k8s.io/yaml routes
// YAML through JSON, so a YAML string decodes as base64 -- matching kubeconfig's
// certificate-authority-data / client-certificate-data / client-key-data.
type fileClusterConn struct {
	Name                     string                 `json:"name"`
	Server                   string                 `json:"server"`
	CertificateAuthority     string                 `json:"certificateAuthority"`
	CertificateAuthorityData []byte                 `json:"certificateAuthorityData"`
	InsecureSkipTLSVerify    bool                   `json:"insecureSkipTlsVerify"`
	TLSServerName            string                 `json:"tlsServerName"`
	Token                    string                 `json:"token"`
	TokenFile                string                 `json:"tokenFile"`
	ClientCertificate        string                 `json:"clientCertificate"`
	ClientCertificateData    []byte                 `json:"clientCertificateData"`
	ClientKey                string                 `json:"clientKey"`
	ClientKeyData            []byte                 `json:"clientKeyData"`
	Impersonate              fileClusterImpersonate `json:"impersonate"`
}

// fileClusterImpersonate is the on-disk per-cluster static act-as identity.
type fileClusterImpersonate struct {
	User   string   `json:"user"`
	Groups []string `json:"groups"`
	UID    string   `json:"uid"`
}

// fileArgoCD is the on-disk Argo CD cluster-Secret discovery block. It is a
// pointer in fileConfig so the source is opt-in: absent -> no Secret listing
// happens; present (even empty) -> the loader lists Argo cluster Secrets, by
// default against the in-cluster SA in namespace "argocd".
type fileArgoCD struct {
	HostCluster string `json:"hostCluster"`
	Namespace   string `json:"namespace"`
}

// fileConfig is the on-disk readout.yaml schema. It is a clean nested shape
// (lists/maps of structs). resolve() folds it into the runtime Config.
type fileConfig struct {
	Port        int `json:"port"`
	MetricsPort int `json:"metricsPort"`

	// ListenAddress pins the bind host for BOTH the app and metrics listeners
	// (the port stays per-listener). An explicit value always wins. When it is
	// empty AND auth.mode is "none", resolve() defaults the bind to the loopback
	// host 127.0.0.1 so a default no-auth binary does not expose unauthenticated
	// cluster data on every interface; under any other auth mode an empty value
	// keeps the historical all-interfaces bind.
	ListenAddress string `json:"listenAddress"`

	// PublicURL pins the externally-visible origin readout is reached at
	// (scheme + host, no path). When set it is the base for the OIDC redirect URL
	// in place of the per-request X-Forwarded reconstruction. SessionSecretFile
	// reads the session-signing secret from a mounted file when the
	// READOUT_SESSION_SECRET env var is unset. Both are top-level keys so they sit
	// next to port, not under auth.
	PublicURL         string `json:"publicUrl"`
	SessionSecretFile string `json:"sessionSecretFile"`

	IncludeNamespaces []string `json:"includeNamespaces"`
	ExcludeNamespaces []string `json:"excludeNamespaces"`

	Clusters                   []fileClusterConn `json:"clusters"`
	KubeconfigPath             string            `json:"kubeconfigPath"`
	KubeconfigContexts         []string          `json:"kubeconfigContexts"`
	ClusterAuthUseSessionToken bool              `json:"clusterAuthUseSessionToken"`
	ArgoCD                     *fileArgoCD       `json:"argoCD"`

	ShowContainerLogs bool   `json:"showContainerLogs"`
	NoAccessLogs      bool   `json:"noAccessLogs"`
	IncludeSecrets    bool   `json:"includeSecrets"`
	TemplatesPath     string `json:"templatesPath"`
	StaticAssetsPath  string `json:"staticAssetsPath"`

	ObjectLinks    map[string][]fileLink `json:"objectLinks"`
	LabelLinks     map[string][]fileLink `json:"labelLinks"`
	TimestampLinks map[string][]fileLink `json:"timestampLinks"`

	Sidebar []fileSidebarGroup `json:"sidebar"`

	// ResourceIcons are Tier-3 per-resource icon overrides, keyed by kind+group.
	// The top-level YAML key is `resources:` (a typed list, distinct from the
	// plural strings under sidebar.resources).
	ResourceIcons []fileIconOverride `json:"resources"`

	Search struct {
		DefaultResourceTypes []string `json:"defaultResourceTypes"`
		OfferedResourceTypes []string `json:"offeredResourceTypes"`
		MaxConcurrency       *int     `json:"maxConcurrency"`
	} `json:"search"`

	LabelColumns         map[string]string `json:"labelColumns"`
	HiddenColumns        map[string]string `json:"hiddenColumns"`
	CustomColumns        map[string]string `json:"customColumns"`
	PreferredAPIVersions map[string]string `json:"preferredApiVersions"`

	DefaultTheme     string        `json:"defaultTheme"`
	ThemeOptions     []string      `json:"themeOptions"`
	ExternalClusters []fileCluster `json:"externalClusters"`

	Auth struct {
		Mode           string `json:"mode"`
		TrustedHeaders struct {
			User              string   `json:"user"`
			Email             string   `json:"email"`
			Groups            string   `json:"groups"`
			TrustedProxyCIDRs []string `json:"trustedProxyCidrs"`
		} `json:"trustedHeaders"`
		OIDC struct {
			IssuerURL        string `json:"issuerUrl"`
			ClientID         string `json:"clientId"`
			ClientIDFile     string `json:"clientIdFile"`
			ClientSecretFile string `json:"clientSecretFile"`
			RedirectURL      string `json:"redirectUrl"`
			AuthorizeURL     string `json:"authorizeUrl"`
			TokenURL         string `json:"tokenUrl"`
			Scope            string `json:"scope"`
		} `json:"oidc"`
	} `json:"auth"`

	Hooks struct {
		AuthorizationURL string `json:"authorizationUrl"`
		// AuthorizationIncludeTokens opts the authorization hook into receiving
		// specific OAuth tokens. It is a list drawn from access|id|refresh; an
		// empty/omitted list (the default) sends identity claims only.
		AuthorizationIncludeTokens []string `json:"authorizationIncludeTokens"`
		ResourcePrerenderURL       string   `json:"resourcePrerenderUrl"`
	} `json:"hooks"`
}

// Parse reads the bootstrap flags (--config/--port/--debug/--version), loads
// the optional --config file and resolves it into the runtime Config, then
// applies the secret env overrides (READOUT_*). The config file is read here,
// BEFORE main.go acts on ShowVersion, so `--config x --version` exercises the
// real loader end to end.
func Parse(args []string) (Config, error) {
	var configPath string
	var port int
	var debug, showVersion bool

	fs := flag.NewFlagSet("readout", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "", "Path to readout.yaml config file")
	fs.IntVar(&port, "port", 0, "TCP port to start webserver on (overrides config)")
	fs.BoolVar(&debug, "debug", false, "Run with debug logging")
	fs.BoolVar(&showVersion, "version", false, "Print version and exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	var file fileConfig
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.UnmarshalStrict(data, &file); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", configPath, err)
		}
	}

	cfg, err := resolve(&file)
	if err != nil {
		return Config{}, err
	}

	cfg.ShowVersion = showVersion
	if debug {
		cfg.Debug = true
	}
	if port != 0 {
		cfg.Port = port
	}
	return cfg, nil
}

// resolve folds the parsed file schema into the runtime Config: it compiles the
// namespace regexps, builds the link/column/ordered-sidebar structures, applies
// defaults, layers the READOUT_* secret env vars over the file (env wins), and
// validates. Secrets never live in the file -- only in env or referenced files.
func resolve(file *fileConfig) (Config, error) {
	clusters, err := resolveClusterConnections(file.Clusters)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Port:                           firstNonZero(file.Port, 8080),
		MetricsPort:                    file.MetricsPort,
		ListenAddress:                  strings.TrimSpace(file.ListenAddress),
		KubeconfigPath:                 file.KubeconfigPath,
		KubeconfigContexts:             file.KubeconfigContexts,
		ClusterAuthUseSessionToken:     file.ClusterAuthUseSessionToken,
		ShowContainerLogs:              file.ShowContainerLogs,
		NoAccessLogs:                   file.NoAccessLogs,
		IncludeSecrets:                 file.IncludeSecrets,
		TemplatesPath:                  file.TemplatesPath,
		StaticAssetsPath:               file.StaticAssetsPath,
		SearchDefaultResourceTypes:     file.Search.DefaultResourceTypes,
		SearchOfferedResourceTypes:     file.Search.OfferedResourceTypes,
		SearchMaxConcurrency:           100,
		DefaultLabelColumns:            mapOrEmpty(file.LabelColumns),
		DefaultHiddenColumns:           overlayMap(v2DefaultHiddenColumns, file.HiddenColumns),
		DefaultCustomColumns:           mapOrEmpty(file.CustomColumns),
		PreferredAPIVersions:           mapOrEmpty(file.PreferredAPIVersions),
		DefaultTheme:                   firstNonEmpty(file.DefaultTheme, "dark"),
		ThemeOptions:                   file.ThemeOptions,
		Clusters:                       clusters,
		ArgoCD:                         resolveArgoCD(file.ArgoCD),
		ExternalClusters:               clusterMap(file.ExternalClusters),
		ObjectLinks:                    resolveLinks(file.ObjectLinks),
		LabelLinks:                     resolveLinks(file.LabelLinks),
		TimestampLinks:                 resolveLinks(file.TimestampLinks),
		ResourceIcons:                  resolveResourceIcons(file.ResourceIcons),
		Sidebar:                        resolveSidebar(file.Sidebar),
		AuthMode:                       firstNonEmpty(file.Auth.Mode, AuthModeNone),
		TrustedHeaderUser:              firstNonEmpty(file.Auth.TrustedHeaders.User, "X-Forwarded-User"),
		TrustedHeaderEmail:             firstNonEmpty(file.Auth.TrustedHeaders.Email, "X-Forwarded-Email"),
		TrustedHeaderGroups:            firstNonEmpty(file.Auth.TrustedHeaders.Groups, "X-Forwarded-Groups"),
		OIDCIssuerURL:                  file.Auth.OIDC.IssuerURL,
		OIDCClientID:                   file.Auth.OIDC.ClientID,
		OAuth2ClientIDFile:             file.Auth.OIDC.ClientIDFile,
		OAuth2ClientSecretFile:         file.Auth.OIDC.ClientSecretFile,
		OIDCRedirectURL:                file.Auth.OIDC.RedirectURL,
		OAuth2AuthorizeURL:             file.Auth.OIDC.AuthorizeURL,
		OAuth2TokenURL:                 file.Auth.OIDC.TokenURL,
		OAuth2Scope:                    file.Auth.OIDC.Scope,
		SessionSecretFile:              file.SessionSecretFile,
		PublicURL:                      file.PublicURL,
		AuthorizationHookURL:           file.Hooks.AuthorizationURL,
		AuthorizationHookIncludeTokens: file.Hooks.AuthorizationIncludeTokens,
		ResourcePrerenderHookURL:       file.Hooks.ResourcePrerenderURL,
	}
	if file.Search.MaxConcurrency != nil {
		cfg.SearchMaxConcurrency = *file.Search.MaxConcurrency
	}

	if cfg.TrustedProxyCIDRs, err = parseCIDRs(file.Auth.TrustedHeaders.TrustedProxyCIDRs); err != nil {
		return Config{}, fmt.Errorf("auth.trustedHeaders.trustedProxyCidrs: %w", err)
	}

	// Safe loopback default: a no-auth binary with no explicit listenAddress
	// binds loopback so unauthenticated cluster data is not served on every
	// interface. An explicit listenAddress always wins; any other auth mode
	// keeps the historical all-interfaces bind (empty host -> ":port").
	if cfg.ListenAddress == "" && cfg.AuthMode == AuthModeNone {
		cfg.ListenAddress = loopbackHost
	}

	// READOUT_* env overrides the file for secrets and OIDC endpoint config.
	cfg.OIDCIssuerURL = firstNonEmpty(os.Getenv("READOUT_OIDC_ISSUER_URL"), cfg.OIDCIssuerURL)
	cfg.OIDCClientID = firstNonEmpty(os.Getenv("READOUT_OIDC_CLIENT_ID"), cfg.OIDCClientID)
	cfg.OIDCClientSecret = firstNonEmpty(os.Getenv("READOUT_OIDC_CLIENT_SECRET"), cfg.OIDCClientSecret)
	cfg.OIDCRedirectURL = firstNonEmpty(os.Getenv("READOUT_OIDC_REDIRECT_URL"), cfg.OIDCRedirectURL)
	cfg.SessionSecret = firstNonEmpty(os.Getenv("READOUT_SESSION_SECRET"), cfg.SessionSecret)
	cfg.AuthorizationHookURL = firstNonEmpty(os.Getenv("READOUT_AUTHORIZATION_HOOK_URL"), cfg.AuthorizationHookURL)
	cfg.ResourcePrerenderHookURL = firstNonEmpty(os.Getenv("READOUT_RESOURCE_PRERENDER_HOOK_URL"), cfg.ResourcePrerenderHookURL)

	// The authorization hook is an outbound call to an external endpoint, so its
	// URL is validated as a config-syntax check (NOT a security startup gate): it
	// must be https, or http only for a loopback dev target, and never point at a
	// link-local/cloud-metadata host. This is a SEPARATE policy from the
	// cluster-server URL validator -- a hook is external by design, a cluster
	// server may legitimately be a private IP -- so the two are kept as distinct
	// helpers and must not be merged.
	if cfg.AuthorizationHookURL != "" {
		if err := validateHookURL(cfg.AuthorizationHookURL); err != nil {
			return Config{}, fmt.Errorf("hooks.authorizationUrl: %w", err)
		}
	}
	if cfg.AuthorizationHookIncludeTokens, err = normalizeIncludeTokens(cfg.AuthorizationHookIncludeTokens); err != nil {
		return Config{}, fmt.Errorf("hooks.authorizationIncludeTokens: %w", err)
	}

	if cfg.IncludeNamespaces, err = compilePatterns(file.IncludeNamespaces); err != nil {
		return Config{}, fmt.Errorf("includeNamespaces: %w", err)
	}
	if cfg.ExcludeNamespaces, err = compilePatterns(file.ExcludeNamespaces); err != nil {
		return Config{}, fmt.Errorf("excludeNamespaces: %w", err)
	}

	if cfg.OIDCClientID == "" && cfg.OAuth2ClientIDFile != "" {
		if cfg.OIDCClientID, err = readSecretFile(cfg.OAuth2ClientIDFile); err != nil {
			return Config{}, fmt.Errorf("oidc clientIdFile: %w", err)
		}
	}
	if cfg.OIDCClientSecret == "" && cfg.OAuth2ClientSecretFile != "" {
		if cfg.OIDCClientSecret, err = readSecretFile(cfg.OAuth2ClientSecretFile); err != nil {
			return Config{}, fmt.Errorf("oidc clientSecretFile: %w", err)
		}
	}
	// The env var wins; the file is consulted only when the secret is still empty
	// after env application, mirroring the clientIdFile/clientSecretFile lane.
	if cfg.SessionSecret == "" && cfg.SessionSecretFile != "" {
		if cfg.SessionSecret, err = readSecretFile(cfg.SessionSecretFile); err != nil {
			return Config{}, fmt.Errorf("sessionSecretFile: %w", err)
		}
	}

	if cfg.PublicURL != "" {
		if cfg.PublicURL, err = normalizePublicURL(cfg.PublicURL); err != nil {
			return Config{}, err
		}
	}

	if cfg.AuthMode != AuthModeNone && cfg.AuthMode != AuthModeHeaders && cfg.AuthMode != AuthModeOIDC {
		return Config{}, fmt.Errorf("invalid auth mode %q", cfg.AuthMode)
	}
	// OIDC is never auto-enabled from endpoint config: a config that carries OIDC
	// settings but leaves auth.mode at "none" is a misconfiguration, not a silent
	// promotion. Fail fast and name the one-line fix. This check runs BEFORE the
	// redirectUrl/publicUrl-required check so that a promoted config (which would
	// also trip that check) reports the actionable mode fix instead.
	if cfg.AuthMode == AuthModeNone &&
		(cfg.OIDCIssuerURL != "" || (cfg.OAuth2AuthorizeURL != "" && cfg.OAuth2TokenURL != "")) {
		return Config{}, errors.New(`oidc settings present but auth.mode is "none"; set auth.mode: oidc (implicit promotion was removed)`)
	}
	if oidcEnabled(&cfg) && cfg.OIDCRedirectURL == "" && cfg.PublicURL == "" {
		return Config{}, errors.New("auth.oidc.redirectUrl or publicUrl is required when OIDC is enabled")
	}
	if cfg.SearchMaxConcurrency <= 0 {
		return Config{}, errors.New("search maxConcurrency must be positive")
	}
	if cfg.MetricsPort < 0 {
		return Config{}, errors.New("metricsPort must be non-negative")
	}
	return cfg, nil
}

// loopbackHost is the bind host used as the safe default under auth.mode=none
// with no explicit listenAddress.
const loopbackHost = "127.0.0.1"

// Address builds a listen address from a bind host and a port. An empty host
// yields the historical ":port" (all interfaces); a non-empty host pins the
// bind, e.g. Address("127.0.0.1", 8080) -> "127.0.0.1:8080". The host threads
// through BOTH the app and metrics listeners so they bind the same interface.
func Address(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// loopbackHosts is the Host-header allowlist enforced ONLY when the resolved
// bind is loopback under auth.mode=none: it rejects a forged-Host DNS-rebinding
// request while always admitting the operator's own loopback access.
var loopbackHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
}

// IsLoopbackHost reports whether a bind host is a loopback address. The empty
// host (all interfaces) is NOT loopback.
func IsLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// EnforceLoopbackHostAllowlist reports whether request Host headers must be
// checked against the loopback allowlist: true only when the resolved bind is
// loopback AND auth is disabled. A non-loopback bind accepts any Host (the
// operator reaches readout by its real name); any auth mode other than none
// already gates access, so no Host check is layered on.
func (c *Config) EnforceLoopbackHostAllowlist() bool {
	return c.AuthMode == AuthModeNone && IsLoopbackHost(c.ListenAddress)
}

// AllowedHost reports whether a request Host (with any :port stripped) is in
// the loopback allowlist. It is consulted only when EnforceLoopbackHostAllowlist
// is true.
func AllowedHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return loopbackHosts[strings.ToLower(host)]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// oidcEnabled reports whether OIDC login is active. With implicit promotion
// removed, this is exactly the explicit mode: a none-mode config carrying OIDC
// fields is rejected at load before this is consulted.
func oidcEnabled(cfg *Config) bool {
	return cfg.AuthMode == AuthModeOIDC
}

func mapOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// v2DefaultHiddenColumns ships the per-kind noise-off defaults: these
// columns render HIDDEN unless something more specific speaks -- a config
// file entry for the kind (overlayMap: file wins per key, an explicit empty
// value re-shows everything), a user column preference in the ro_prefs cookie,
// or an explicit ?hidecols= URL param. "Created" names the synthetic
// render-time Created column, not a kube Table column.
var v2DefaultHiddenColumns = map[string]string{
	"nodes": "External-IP,OS-Image,Kernel-Version,Created",
	"pods":  "IP,Nominated Node,Readiness Gates",
}

// overlayMap merges the file map over the shipped defaults: every default key
// applies unless the file carries that key, in which case the file value wins
// outright -- including an explicit empty value, which disables the default.
// Neither input map is mutated.
func overlayMap(defaults, file map[string]string) map[string]string {
	merged := make(map[string]string, len(defaults)+len(file))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range file {
		merged[k] = v
	}
	return merged
}

// resolveClusterConnections folds the on-disk cluster list into the runtime
// []ClusterConnection. A cluster with an empty name is an error (it cannot be
// addressed), and a byte-identical duplicate name is a startup error
// (config-parse half) -- replacing the old map's silent last-write-wins. The
// post-SanitizeClusterName collision case is caught later in the kube loader.
func resolveClusterConnections(clusters []fileClusterConn) ([]ClusterConnection, error) {
	if len(clusters) == 0 {
		return nil, nil
	}
	result := make([]ClusterConnection, 0, len(clusters))
	seen := make(map[string]bool, len(clusters))
	for i := range clusters {
		c := &clusters[i]
		if c.Name == "" {
			return nil, errors.New("cluster with empty name in clusters list")
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("duplicate cluster name %q in clusters list", c.Name)
		}
		seen[c.Name] = true
		result = append(result, ClusterConnection{
			Name:                     c.Name,
			Server:                   c.Server,
			CertificateAuthority:     c.CertificateAuthority,
			CertificateAuthorityData: c.CertificateAuthorityData,
			InsecureSkipTLSVerify:    c.InsecureSkipTLSVerify,
			TLSServerName:            c.TLSServerName,
			Token:                    c.Token,
			TokenFile:                c.TokenFile,
			ClientCertificate:        c.ClientCertificate,
			ClientCertificateData:    c.ClientCertificateData,
			ClientKey:                c.ClientKey,
			ClientKeyData:            c.ClientKeyData,
			Impersonate: ClusterImpersonation{
				User:   c.Impersonate.User,
				Groups: c.Impersonate.Groups,
				UID:    c.Impersonate.UID,
			},
		})
	}
	return result, nil
}

// resolveArgoCD folds the on-disk Argo CD discovery block into the runtime
// pointer. Absent in the file -> nil (the source is off). Present ->
// non-nil with Namespace defaulted to "argocd" when the operator left it empty,
// matching Argo's default install namespace.
func resolveArgoCD(src *fileArgoCD) *ArgoCDSource {
	if src == nil {
		return nil
	}
	return &ArgoCDSource{
		HostCluster: src.HostCluster,
		Namespace:   firstNonEmpty(src.Namespace, "argocd"),
	}
}

func clusterMap(clusters []fileCluster) map[string]string {
	if len(clusters) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(clusters))
	for _, c := range clusters {
		if c.Name == "" {
			continue
		}
		result[c.Name] = c.URL
	}
	return result
}

func resolveLinks(links map[string][]fileLink) map[string][]Link {
	result := map[string][]Link{}
	for key, defs := range links {
		for _, def := range defs {
			link := Link(def)
			if link.Icon == "" {
				link.Icon = "external-link"
			}
			if link.Title == "" {
				link.Title = "External link"
			}
			result[key] = append(result[key], link)
		}
	}
	return result
}

// resolveResourceIcons folds the typed `resources:` override list into a
// kind+group keyed map. Entries with an empty icon (or no kind) are skipped so a
// stray list element cannot blank an otherwise-resolved icon. The icon string is
// interpreted later by the icon resolver (lucide:/emoji:/local-image/bare-name);
// remote image URLs are dropped there, not here.
func resolveResourceIcons(overrides []fileIconOverride) map[ResourceIconKey]string {
	result := map[ResourceIconKey]string{}
	for _, o := range overrides {
		if o.Kind == "" || o.Icon == "" {
			continue
		}
		result[ResourceIconKey{Kind: o.Kind, Group: o.Group}] = o.Icon
	}
	return result
}

func resolveSidebar(groups []fileSidebarGroup) []SidebarGroup {
	if len(groups) == 0 {
		return nil
	}
	result := make([]SidebarGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, SidebarGroup(g))
	}
	return result
}

// normalizePublicURL validates publicUrl as an ORIGIN: an absolute http(s) URL
// with a host and no path beyond an optional single "/", no query, no fragment.
// A path-bearing value would imply a subpath deployment (readout served under
// /readout on a shared host), which readout cannot serve -- it owns its host
// root -- so that is rejected. The returned value is the origin with any single
// trailing slash stripped, so callers can append a rooted path directly.
func normalizePublicURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("publicUrl is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("publicUrl must be an absolute http(s) URL, got %q", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("publicUrl must include a host, got %q", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("publicUrl must not carry credentials; it is an origin (scheme://host), got %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("publicUrl must not carry a query or fragment, got %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("publicUrl must be an origin (no path); readout serves on its own host, got %q", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// validateHookURL checks an external hook URL at config load. It is intentionally
// strict because the hook is an outbound call carrying identity (and optionally
// tokens): the scheme must be https, with http permitted ONLY for a loopback dev
// target, and the host must not be a link-local / cloud-metadata address
// (169.254.0.0/16, fe80::/10) -- the classic SSRF-to-metadata target. This policy
// is DELIBERATELY different from the cluster-server URL validator: a cluster API
// server may legitimately sit on a private IP, so the two validators stay
// separate and must not be merged.
func validateHookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host, got %q", raw)
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
		// always allowed
	case "http":
		if !isLoopbackURLHost(host) {
			return fmt.Errorf("http hook URL is allowed only for a loopback dev target, got %q", raw)
		}
	default:
		return fmt.Errorf("must be an https URL (http allowed only for loopback), got %q", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("hook URL must not target a link-local/metadata host, got %q", raw)
		}
	}
	return nil
}

// isLoopbackURLHost reports whether a URL host (no port) is a loopback target:
// the literal name "localhost" or any loopback IP literal.
func isLoopbackURLHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// includeTokenValues is the closed enum of tokens the authorization hook may opt
// into receiving.
var includeTokenValues = map[string]bool{
	"access":  true,
	"id":      true,
	"refresh": true,
}

// normalizeIncludeTokens validates the hooks.authorizationIncludeTokens enum
// list: each entry must be one of access|id|refresh (a bad value is a
// config-syntax error). Blank entries are skipped; an empty/omitted list returns
// nil so the auth layer sends identity claims only.
func normalizeIncludeTokens(raw []string) ([]string, error) {
	var result []string
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !includeTokenValues[t] {
			return nil, fmt.Errorf("invalid token %q (allowed: access, id, refresh)", t)
		}
		result = append(result, t)
	}
	return result, nil
}

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// parseCIDRs turns the optional auth.trustedHeaders.trustedProxyCidrs strings
// into netip.Prefix values. A blank entry is skipped; a malformed entry is a
// config-syntax error (surfaced like a bad namespace regex), NOT a security
// startup gate. An empty/omitted list returns nil -- the headers-mode caller
// reads nil as "no proxy gate, trust headers + warn".
func parseCIDRs(raw []string) ([]netip.Prefix, error) {
	var result []netip.Prefix
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		result = append(result, prefix)
	}
	return result, nil
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	var result []*regexp.Regexp
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile("^(?:" + p + ")$")
		if err != nil {
			return nil, err
		}
		result = append(result, re)
	}
	return result, nil
}
