package config

import (
	"errors"
	"flag"
	"fmt"
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

// Config is the resolved runtime configuration. Field types match what the
// rest of the service consumes directly (s.cfg.X); the YAML file is parsed into
// the unexported fileConfig and folded into this shape by resolve().
type Config struct {
	Port        int
	ShowVersion bool

	IncludeNamespaces []*regexp.Regexp
	ExcludeNamespaces []*regexp.Regexp

	Clusters                             map[string]string
	KubeconfigPath                       string
	KubeconfigContexts                   []string
	ClusterRegistryURL                   string
	ClusterRegistryOAuth2BearerTokenPath string
	ClusterLabelSelector                 map[string]string
	ClusterAuthTokenPath                 string
	ClusterAuthUseSessionToken           bool
	ShowContainerLogs                    bool
	NoAccessLogs                         bool
	IncludeSecrets                       bool
	Debug                                bool
	TemplatesPath                        string
	StaticAssetsPath                     string
	ObjectLinks                          map[string][]Link
	LabelLinks                           map[string][]Link
	TimestampLinks                       map[string][]Link
	ResourceIcons                        map[ResourceIconKey]string
	Sidebar                              []SidebarGroup
	SearchDefaultResourceTypes           []string
	SearchOfferedResourceTypes           []string
	SearchMaxConcurrency                 int
	DefaultLabelColumns                  map[string]string
	DefaultHiddenColumns                 map[string]string
	DefaultCustomColumns                 map[string]string
	PreferredAPIVersions                 map[string]string
	DefaultTheme                         string
	ThemeOptions                         []string
	ExternalClusters                     map[string]string
	AuthMode                             string
	TrustedHeaderUser                    string
	TrustedHeaderEmail                   string
	TrustedHeaderGroups                  string
	OIDCIssuerURL                        string
	OIDCClientID                         string
	OIDCClientSecret                     string
	OAuth2ClientIDFile                   string
	OAuth2ClientSecretFile               string
	OIDCRedirectURL                      string
	OAuth2AuthorizeURL                   string
	OAuth2TokenURL                       string
	OAuth2Scope                          string
	SessionSecret                        string
	AuthorizationHookURL                 string
	ResourcePrerenderHookURL             string
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

// fileCluster is one statically-configured external cluster (name + apiserver
// URL) written as a list element rather than a map so duplicate-name intent is
// explicit and the form reads naturally.
type fileCluster struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// fileConfig is the on-disk readout.yaml schema. It is a clean nested shape
// (lists/maps of structs). resolve() folds it into the runtime Config.
type fileConfig struct {
	Port int `json:"port"`

	IncludeNamespaces []string `json:"includeNamespaces"`
	ExcludeNamespaces []string `json:"excludeNamespaces"`

	Clusters                             []fileCluster     `json:"clusters"`
	KubeconfigPath                       string            `json:"kubeconfigPath"`
	KubeconfigContexts                   []string          `json:"kubeconfigContexts"`
	ClusterRegistryURL                   string            `json:"clusterRegistryUrl"`
	ClusterRegistryOAuth2BearerTokenPath string            `json:"clusterRegistryBearerTokenPath"`
	ClusterLabelSelector                 map[string]string `json:"clusterLabelSelector"`
	ClusterAuthTokenPath                 string            `json:"clusterAuthTokenPath"`
	ClusterAuthUseSessionToken           bool              `json:"clusterAuthUseSessionToken"`

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
			User   string `json:"user"`
			Email  string `json:"email"`
			Groups string `json:"groups"`
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
		AuthorizationURL     string `json:"authorizationUrl"`
		ResourcePrerenderURL string `json:"resourcePrerenderUrl"`
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
	cfg := Config{
		Port:                                 firstNonZero(file.Port, 8080),
		KubeconfigPath:                       file.KubeconfigPath,
		KubeconfigContexts:                   file.KubeconfigContexts,
		ClusterRegistryURL:                   file.ClusterRegistryURL,
		ClusterRegistryOAuth2BearerTokenPath: file.ClusterRegistryOAuth2BearerTokenPath,
		ClusterLabelSelector:                 file.ClusterLabelSelector,
		ClusterAuthTokenPath:                 file.ClusterAuthTokenPath,
		ClusterAuthUseSessionToken:           file.ClusterAuthUseSessionToken,
		ShowContainerLogs:                    file.ShowContainerLogs,
		NoAccessLogs:                         file.NoAccessLogs,
		IncludeSecrets:                       file.IncludeSecrets,
		TemplatesPath:                        file.TemplatesPath,
		StaticAssetsPath:                     file.StaticAssetsPath,
		SearchDefaultResourceTypes:           file.Search.DefaultResourceTypes,
		SearchOfferedResourceTypes:           file.Search.OfferedResourceTypes,
		SearchMaxConcurrency:                 100,
		DefaultLabelColumns:                  mapOrEmpty(file.LabelColumns),
		DefaultHiddenColumns:                 mapOrEmpty(file.HiddenColumns),
		DefaultCustomColumns:                 mapOrEmpty(file.CustomColumns),
		PreferredAPIVersions:                 mapOrEmpty(file.PreferredAPIVersions),
		DefaultTheme:                         firstNonEmpty(file.DefaultTheme, "dark"),
		ThemeOptions:                         file.ThemeOptions,
		Clusters:                             clusterMap(file.Clusters),
		ExternalClusters:                     clusterMap(file.ExternalClusters),
		ObjectLinks:                          resolveLinks(file.ObjectLinks),
		LabelLinks:                           resolveLinks(file.LabelLinks),
		TimestampLinks:                       resolveLinks(file.TimestampLinks),
		ResourceIcons:                        resolveResourceIcons(file.ResourceIcons),
		Sidebar:                              resolveSidebar(file.Sidebar),
		AuthMode:                             firstNonEmpty(file.Auth.Mode, AuthModeNone),
		TrustedHeaderUser:                    firstNonEmpty(file.Auth.TrustedHeaders.User, "X-Forwarded-User"),
		TrustedHeaderEmail:                   firstNonEmpty(file.Auth.TrustedHeaders.Email, "X-Forwarded-Email"),
		TrustedHeaderGroups:                  firstNonEmpty(file.Auth.TrustedHeaders.Groups, "X-Forwarded-Groups"),
		OIDCIssuerURL:                        file.Auth.OIDC.IssuerURL,
		OIDCClientID:                         file.Auth.OIDC.ClientID,
		OAuth2ClientIDFile:                   file.Auth.OIDC.ClientIDFile,
		OAuth2ClientSecretFile:               file.Auth.OIDC.ClientSecretFile,
		OIDCRedirectURL:                      file.Auth.OIDC.RedirectURL,
		OAuth2AuthorizeURL:                   file.Auth.OIDC.AuthorizeURL,
		OAuth2TokenURL:                       file.Auth.OIDC.TokenURL,
		OAuth2Scope:                          file.Auth.OIDC.Scope,
		AuthorizationHookURL:                 file.Hooks.AuthorizationURL,
		ResourcePrerenderHookURL:             file.Hooks.ResourcePrerenderURL,
	}
	if file.Search.MaxConcurrency != nil {
		cfg.SearchMaxConcurrency = *file.Search.MaxConcurrency
	}

	// READOUT_* env overrides the file for secrets and OIDC endpoint config.
	cfg.OIDCIssuerURL = firstNonEmpty(os.Getenv("READOUT_OIDC_ISSUER_URL"), cfg.OIDCIssuerURL)
	cfg.OIDCClientID = firstNonEmpty(os.Getenv("READOUT_OIDC_CLIENT_ID"), cfg.OIDCClientID)
	cfg.OIDCClientSecret = firstNonEmpty(os.Getenv("READOUT_OIDC_CLIENT_SECRET"), cfg.OIDCClientSecret)
	cfg.OIDCRedirectURL = firstNonEmpty(os.Getenv("READOUT_OIDC_REDIRECT_URL"), cfg.OIDCRedirectURL)
	cfg.SessionSecret = firstNonEmpty(os.Getenv("READOUT_SESSION_SECRET"), cfg.SessionSecret)
	cfg.AuthorizationHookURL = firstNonEmpty(os.Getenv("READOUT_AUTHORIZATION_HOOK_URL"), cfg.AuthorizationHookURL)
	cfg.ResourcePrerenderHookURL = firstNonEmpty(os.Getenv("READOUT_RESOURCE_PRERENDER_HOOK_URL"), cfg.ResourcePrerenderHookURL)

	var err error
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

	if cfg.AuthMode != AuthModeNone && cfg.AuthMode != AuthModeHeaders && cfg.AuthMode != AuthModeOIDC {
		return Config{}, fmt.Errorf("invalid auth mode %q", cfg.AuthMode)
	}
	if cfg.SearchMaxConcurrency <= 0 {
		return Config{}, errors.New("search maxConcurrency must be positive")
	}
	return cfg, nil
}

func Address(port int) string {
	return ":" + strconv.Itoa(port)
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

func mapOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
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

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	var result []*regexp.Regexp
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		result = append(result, re)
	}
	return result, nil
}
