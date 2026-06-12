package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/kbelokon/readout/internal/assets"
	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

// style-src carries 'unsafe-inline' because the design pins per-row values as
// inline style attributes the cascade can't express as classes: capacity-bar
// `width:N%`, replica tracks, and kind-tile `--kh:<hue>` (an arbitrary 0-359 hue
// per CRD group). script-src stays strict ('self', no unsafe-inline/eval) -- the
// primary CSP protection (code execution) is unchanged; only inline styling is
// permitted.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"

type Server struct {
	cfg                config.Config
	manager            *kube.Manager
	mux                *http.ServeMux
	static             http.Handler
	assets             map[string]string
	partials           map[string]string
	metrics            *appMetrics
	sessions           *sessionCodec
	passthroughClients *kube.PassthroughClientCache
	// now is the clock for all render-path time (age coloring + the list/search
	// "took X" footer). It defaults to time.Now in New; tests inject a fixed
	// instant for deterministic, bucket-exercising output. Under real time
	// (now == time.Now) every render is byte-identical to a direct time.Now call.
	now func() time.Time

	// counts is the sidebar per-kind count cache: keyed by the exact list
	// each sidebar entry points at, TTL-invalidated against the s.now clock.
	// The zero value is ready; no constructor wiring needed.
	counts countCache

	// streamSlots caps concurrent Live streams: every open `_stream`
	// handler holds one slot for its whole lifetime; when the channel is full
	// the next stream gets 429 BEFORE any SSE headers. The slot releases on
	// every handler exit path (deferred at acquisition).
	streamSlots chan struct{}

	// shutdownCh mirrors the New() context's Done channel: when the process
	// is shutting down, open Live streams emit `event: ro-terminal` (reason
	// "shutdown") and close instead of dying mid-frame.
	shutdownCh <-chan struct{}
}

var withBearerClient = func(client *kube.Client, token string) (*kube.Client, error) {
	return client.WithBearer(token)
}

type requestKubeClients map[string]*kube.Client

func New(ctx context.Context, cfg *config.Config) (*Server, error) {
	manager, err := kube.NewManager(ctx, cfg)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(assets.FS, "static")
	if err != nil {
		return nil, err
	}
	staticFS := fs.FS(sub)
	if cfg.StaticAssetsPath != "" {
		staticFS = os.DirFS(cfg.StaticAssetsPath)
	}
	sessions, err := newSessionCodec(cfg.SessionSecret)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:                *cfg,
		manager:            manager,
		mux:                http.NewServeMux(),
		static:             http.FileServerFS(staticFS),
		assets:             assetHashes(staticFS),
		partials:           loadPartials(cfg.TemplatesPath),
		metrics:            newAppMetrics(),
		sessions:           sessions,
		passthroughClients: kube.NewPassthroughClientCache(0, 0),
		now:                time.Now,
		streamSlots:        make(chan struct{}, streamCapMax),
		shutdownCh:         ctx.Done(),
	}
	s.routes()
	s.warnMissingSessionSecret()
	return s, nil
}

// warnMissingSessionSecret warns at startup when the effective auth mode is
// OIDC but no session secret is configured (READOUT_SESSION_SECRET empty, so
// newSessionCodec generated an ephemeral per-process key). Without a stable
// secret, sessions silently break across restarts and across replicas. Gated
// on effectiveAuthMode (not raw cfg.AuthMode) so the implicit
// AuthModeNone + OIDC-config path is also covered.
func (s *Server) warnMissingSessionSecret() {
	if s.effectiveAuthMode() == config.AuthModeOIDC && s.cfg.SessionSecret == "" {
		slog.Warn("OIDC auth has no session secret; sessions will not survive restarts or span replicas", "env", "READOUT_SESSION_SECRET")
	}
}

func (s *Server) Handler() http.Handler {
	return s.readOnly(s.securityHeaders(s.observeMetrics(s.auth(s.mux))))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /assets/{name...}", s.assetsHandler)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "OK") })
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "OK") })
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "OK") })
	if s.cfg.MetricsPort == 0 {
		s.mux.HandleFunc("GET /metrics", s.metricsHandler)
	} else {
		// Override the catch-all route so the main app listener does not redirect /metrics.
		s.mux.HandleFunc("GET /metrics", http.NotFound)
	}
	s.mux.HandleFunc("GET /oauth2/callback", s.oauth2Callback)
	s.mux.HandleFunc("GET /oauth2/login", s.oauth2Login)
	s.mux.HandleFunc("GET /oauth2/logout", s.oauth2Logout)
	s.mux.HandleFunc("GET /", s.index)
	s.mux.HandleFunc("GET /preferences", s.preferences)
	s.mux.HandleFunc("POST /preferences", s.savePreferences)
	s.mux.HandleFunc("GET /clusters", s.clusters)
	s.mux.HandleFunc("GET /clusters/{cluster}", s.cluster)
	s.mux.HandleFunc("GET /clusters/{cluster}/_resource-types", s.clusterResourceTypes)
	s.mux.HandleFunc("GET /clusters/{cluster}/namespaces/{namespace}/_resource-types", s.namespacedResourceTypes)
	s.mux.HandleFunc("GET /clusters/{cluster}/{plural}/_table", s.resourceListPartial)
	s.mux.HandleFunc("GET /clusters/{cluster}/namespaces/{namespace}/{plural}/_table", s.resourceListPartial)
	s.mux.HandleFunc("GET /clusters/{cluster}/{plural}/_stream", s.resourceStream)
	s.mux.HandleFunc("GET /clusters/{cluster}/namespaces/{namespace}/{plural}/_stream", s.resourceStream)
	s.mux.HandleFunc("GET /clusters/{cluster}/namespaces/{namespace}/{plural}/{name}/logs", s.resourceLogs)
	s.mux.HandleFunc("GET /clusters/{cluster}/{plural}/{name}", s.resourceView)
	s.mux.HandleFunc("GET /clusters/{cluster}/namespaces/{namespace}/{plural}/{name}", s.resourceView)
	s.mux.HandleFunc("GET /clusters/{cluster}/{plural}", s.resourceList)
	s.mux.HandleFunc("GET /clusters/{cluster}/namespaces/{namespace}/{plural}", s.resourceList)
	s.mux.HandleFunc("GET /search", s.search)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/clusters", http.StatusFound)
}

func (s *Server) oneCluster(r *http.Request) (*kube.Cluster, error) {
	name := r.PathValue("cluster")
	cluster, ok := s.manager.Get(name)
	if !ok {
		return nil, fmt.Errorf("cluster %q not found", name)
	}
	return cluster, nil
}

func (s *Server) kubeClient(r *http.Request, cluster *kube.Cluster) *kube.Client {
	if !s.cfg.ClusterAuthUseSessionToken {
		return cluster.Client
	}
	token := s.requestBearer(r)
	if token == "" {
		// No viewer token. Fall through to the base identity -- an in-cluster
		// SA, a token-file, or a static cluster with its own credential is a real
		// identity, not silent anonymous. Deny ONLY when the base is itself
		// anonymous: serving that as anonymous would be a silent downgrade.
		if cluster.Client.IsAnonymous() {
			return cluster.Client.Denied()
		}
		return cluster.Client
	}
	var (
		client *kube.Client
		err    error
	)
	if s.passthroughClients != nil {
		client, err = s.passthroughClients.Get(cluster.Client, token, withBearerClient)
	} else {
		client, err = withBearerClient(cluster.Client, token)
	}
	if err != nil {
		slog.Error("passthrough client build failed", "cluster", cluster.Name, "error", err)
		return cluster.Client.Denied()
	}
	return client
}

func (s *Server) kubeClients(r *http.Request, clusters []*kube.Cluster) requestKubeClients {
	clients := make(requestKubeClients, len(clusters))
	for _, cluster := range clusters {
		clients[cluster.Name] = s.kubeClient(r, cluster)
	}
	return clients
}

func (s *Server) requestKubeClient(r *http.Request, clients requestKubeClients, cluster *kube.Cluster) *kube.Client {
	if clients != nil {
		if client := clients[cluster.Name]; client != nil {
			return client
		}
	}
	client := s.kubeClient(r, cluster)
	if clients != nil {
		clients[cluster.Name] = client
	}
	return client
}

func (s *Server) namespaceAllowed(namespace string) bool {
	for _, exclude := range s.cfg.ExcludeNamespaces {
		if exclude.MatchString(namespace) {
			return false
		}
	}
	if len(s.cfg.IncludeNamespaces) == 0 {
		return true
	}
	for _, include := range s.cfg.IncludeNamespaces {
		if include.MatchString(namespace) {
			return true
		}
	}
	return false
}

func (s *Server) navbarNamespaces(r *http.Request, client *kube.Client) []string {
	rt, err := client.FindResource(r.Context(), "namespaces", false, "")
	if err != nil {
		return nil
	}
	list, err := client.List(r.Context(), &rt, kube.ListOptions{})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		object := kube.NewObject(&rt, &list.Items[i])
		if name := object.Name(); name != "" && s.namespaceAllowed(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// clock returns the server's injected clock, defaulting to time.Now when unset
// so a zero-value Server stays behavior-identical to real time.
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// searchConcurrency is the errgroup.SetLimit for the multi-cluster list/search
// fan-out, wired to SearchMaxConcurrency. Config validates it > 0; the
// guard keeps a zero-value/test Server (which skips config validation) from
// passing 0 to SetLimit (which would deadlock the group).
func (s *Server) searchConcurrency() int {
	if s.cfg.SearchMaxConcurrency > 0 {
		return s.cfg.SearchMaxConcurrency
	}
	return 100
}

func (s *Server) assetURL(name string) string {
	hash, ok := s.assets[name]
	if !ok {
		return ""
	}
	return "/assets/" + name + "?v=" + hash
}

func (s *Server) assetsHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.assets[name]; ok {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.StripPrefix("/assets/", s.static).ServeHTTP(w, r)
}

func (s *Server) error(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	if kube.IsNotFound(err) {
		status = http.StatusNotFound
	}
	var httpErr interface{ StatusCode() int }
	if errors.As(err, &httpErr) {
		status = httpErr.StatusCode()
	}
	w.WriteHeader(status)
	statusText := http.StatusText(status)
	// 5xx error detail (raw apiserver/Go strings) can leak cluster-internal
	// names/hosts into the client page, so log it server-side and render a
	// generic body. 4xx (not-found/forbidden) keep their specific message so
	// the viewer still learns what went wrong. The gate is strictly >= 500,
	// computed after the status-derivation above — an "is-not-found-or-has-
	// StatusCode" predicate is NOT equivalent (a 4xx StatusCode would wrongly
	// hit it).
	message := err.Error()
	if status >= 500 {
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "error", err)
		message = "Internal server error — see server logs"
	}
	s.pageComponentWithScope(w, r, statusText, "", "", templates.ErrorBody(statusText, message))
}

type statusError struct {
	status  int
	message string
}

func (err statusError) Error() string {
	return err.message
}

func (err statusError) StatusCode() int {
	return err.status
}
