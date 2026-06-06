package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

const discoveryTTL = 60 * time.Second

var ErrResourceTypeNotFound = errors.New("resource type not found")

type Client struct {
	config         *rest.Config
	httpClient     *http.Client
	discovery      discovery.DiscoveryInterface
	dynamic        dynamic.Interface
	core           kubernetes.Interface
	includeSecrets bool
	// denied, when set, makes every request method short-circuit with this error
	// instead of reaching the apiserver. It backs the D8d anonymous-base denial:
	// a denied client is returned by the web layer when passthrough is on, the
	// viewer presented no token, and the base connection is itself anonymous, so
	// serving the request as anonymous (a silent identity downgrade) is refused.
	denied error

	mu              sync.Mutex
	discoveredAt    time.Time
	namespacedTypes []ResourceType
	clusterTypes    []ResourceType
	preferred       map[string]string
}

// errAnonymousDenied is a Forbidden apiserver Status, so kube.IsForbidden
// recognizes it and the web layer renders the standard "forbidden" state rather
// than an opaque error.
var errAnonymousDenied = &kerrors.StatusError{ErrStatus: metav1.Status{
	Status:  metav1.StatusFailure,
	Reason:  metav1.StatusReasonForbidden,
	Code:    http.StatusForbidden,
	Message: "anonymous access denied: no viewer token and the cluster connection has no base identity",
}}

func NewClient(cfg *rest.Config, preferred map[string]string, includeSecrets bool) (*Client, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	disco, err := discovery.NewDiscoveryClientForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	pref := map[string]string{}
	for k, v := range preferred {
		pref[k] = v
	}
	if _, ok := pref["pods"]; !ok {
		pref["pods"] = "v1"
	}
	if _, ok := pref["nodes"]; !ok {
		pref["nodes"] = "v1"
	}
	if _, ok := pref["events"]; !ok {
		pref["events"] = "v1"
	}
	return &Client{
		config:         rest.CopyConfig(cfg),
		httpClient:     httpClient,
		discovery:      disco,
		dynamic:        dyn,
		core:           core,
		includeSecrets: includeSecrets,
		preferred:      pref,
	}, nil
}

func (c *Client) WithBearer(token string) (*Client, error) {
	cfg := rest.CopyConfig(c.config)
	cfg.BearerToken = strings.TrimPrefix(token, "Bearer ")
	cfg.BearerTokenFile = ""
	// Passthrough must evaluate RBAC AS THE VIEWER. rest.CopyConfig propagates a
	// static Impersonate, and client-go's impersonating round-tripper keys off
	// Impersonate.UserName (NOT the Authorization header), so without this clear a
	// passthrough request against a cluster with a static impersonation identity
	// would silently get that identity's RBAC instead of the viewer's (D4).
	cfg.Impersonate = rest.ImpersonationConfig{}
	// Snapshot preferred under the lock: ResourceTypes reassigns c.preferred
	// under c.mu after discovery, so an unguarded read here (NewClient ranges
	// the map) would race a concurrent refresh.
	c.mu.Lock()
	preferred := make(map[string]string, len(c.preferred))
	for k, v := range c.preferred {
		preferred[k] = v
	}
	c.mu.Unlock()
	return NewClient(cfg, preferred, c.includeSecrets)
}

func (c *Client) RESTMapper() *restmapper.DeferredDiscoveryRESTMapper {
	return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(c.discovery))
}

// IsAnonymous reports whether the base connection carries NO authenticating
// credential (bearer token inline/file, client cert, exec, auth-provider, basic
// auth). Impersonation is ignored: it is not a credential to authenticate WITH,
// so an impersonate-only connection with no base credential is still anonymous.
// Used by the web layer's passthrough denial predicate (D8d).
func (c *Client) IsAnonymous() bool {
	cfg := c.config
	if cfg == nil {
		return true
	}
	return cfg.BearerToken == "" && cfg.BearerTokenFile == "" &&
		len(cfg.CertData) == 0 && cfg.CertFile == "" &&
		len(cfg.KeyData) == 0 && cfg.KeyFile == "" &&
		cfg.ExecProvider == nil && cfg.AuthProvider == nil &&
		cfg.Username == "" && cfg.Password == ""
}

// Denied returns a clone of the client whose every request method refuses with a
// Forbidden error (D8d). It shares the underlying clients (which it never uses,
// since the request methods short-circuit) and takes a fresh mutex, so it copies
// no lock value.
func (c *Client) Denied() *Client {
	c.mu.Lock()
	preferred := make(map[string]string, len(c.preferred))
	for k, v := range c.preferred {
		preferred[k] = v
	}
	c.mu.Unlock()
	return &Client{
		config:         c.config,
		httpClient:     c.httpClient,
		discovery:      c.discovery,
		dynamic:        c.dynamic,
		core:           c.core,
		includeSecrets: c.includeSecrets,
		denied:         errAnonymousDenied,
		preferred:      preferred,
	}
}

func (c *Client) ResourceTypes(ctx context.Context) ([]ResourceType, []ResourceType, error) {
	if c.denied != nil {
		return nil, nil, c.denied
	}
	c.mu.Lock()
	fresh := !c.discoveredAt.IsZero() && time.Since(c.discoveredAt) < discoveryTTL && (len(c.namespacedTypes) > 0 || len(c.clusterTypes) > 0)
	if fresh {
		ns := append([]ResourceType(nil), c.namespacedTypes...)
		cluster := append([]ResourceType(nil), c.clusterTypes...)
		c.mu.Unlock()
		return ns, cluster, nil
	}
	// Snapshot the configured/learned preferred map under the lock into a LOCAL
	// copy. All discovery work below runs against this local map, never the
	// shared c.preferred field -- concurrent cold/expired callers used to read
	// (range) and write c.preferred unlocked here, racing on it. The local is
	// assigned back to c.preferred under the lock at the end.
	preferred := make(map[string]string, len(c.preferred))
	for k, v := range c.preferred {
		preferred[k] = v
	}
	c.mu.Unlock()

	_, lists, err := c.discovery.ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		return nil, nil, err
	}

	namespaced := []ResourceType{metricsResourceType(true)}
	cluster := []ResourceType{metricsResourceType(false)}
	seenPreferred := map[string]bool{}
	for plural := range preferred {
		seenPreferred[plural] = true
	}

	for _, list := range lists {
		group, version := SplitAPIVersion(list.GroupVersion)
		for ri := range list.APIResources {
			res := &list.APIResources[ri]
			if !isListableResource(res) {
				continue
			}
			rt := ResourceType{
				Group:       group,
				Version:     version,
				APIVersion:  list.GroupVersion,
				Kind:        res.Kind,
				Plural:      res.Name,
				Singular:    res.SingularName,
				Namespaced:  res.Namespaced,
				ShortNames:  append([]string(nil), res.ShortNames...),
				Categories:  append([]string(nil), res.Categories...),
				Verbs:       append([]string(nil), res.Verbs...),
				LastRefresh: time.Now(),
			}
			if !c.includeSecrets && rt.APIVersion == "v1" && rt.Kind == "Secret" {
				continue
			}
			if !seenPreferred[rt.Plural] {
				preferred[rt.Plural] = rt.APIVersion
				seenPreferred[rt.Plural] = true
			}
			if rt.Namespaced {
				namespaced = append(namespaced, rt)
			} else {
				cluster = append(cluster, rt)
			}
		}
	}
	sortResourceTypes(namespaced, preferred)
	sortResourceTypes(cluster, preferred)

	c.mu.Lock()
	c.preferred = preferred
	c.namespacedTypes = namespaced
	c.clusterTypes = cluster
	c.discoveredAt = time.Now()
	ns := append([]ResourceType(nil), c.namespacedTypes...)
	cl := append([]ResourceType(nil), c.clusterTypes...)
	c.mu.Unlock()
	return ns, cl, err
}

func (c *Client) NamespacedResourceTypes(ctx context.Context) ([]ResourceType, error) {
	ns, _, err := c.ResourceTypes(ctx)
	return ns, err
}

func (c *Client) ClusterResourceTypes(ctx context.Context) ([]ResourceType, error) {
	_, cluster, err := c.ResourceTypes(ctx)
	return cluster, err
}

func (c *Client) FindResource(ctx context.Context, plural string, namespaced bool, apiVersion string) (ResourceType, error) {
	ns, cluster, err := c.ResourceTypes(ctx)
	if err != nil && len(ns) == 0 && len(cluster) == 0 {
		return ResourceType{}, err
	}
	var types []ResourceType
	if namespaced {
		types = ns
	} else {
		types = cluster
	}
	for i := range types {
		if types[i].Plural == plural && (apiVersion == "" || types[i].APIVersion == apiVersion) {
			return types[i], nil
		}
	}
	return ResourceType{}, fmt.Errorf("%w: %s namespaced=%t", ErrResourceTypeNotFound, plural, namespaced)
}

func (c *Client) FindResourceByKind(ctx context.Context, apiVersion, kind string, namespaced bool) (ResourceType, error) {
	ns, cluster, err := c.ResourceTypes(ctx)
	if err != nil && len(ns) == 0 && len(cluster) == 0 {
		return ResourceType{}, err
	}
	var types []ResourceType
	if namespaced {
		types = ns
	} else {
		types = cluster
	}
	for i := range types {
		if types[i].APIVersion == apiVersion && types[i].Kind == kind {
			return types[i], nil
		}
	}
	return ResourceType{}, fmt.Errorf("%w: %s %s namespaced=%t", ErrResourceTypeNotFound, apiVersion, kind, namespaced)
}

func (c *Client) List(ctx context.Context, rt *ResourceType, opts ListOptions) (*unstructured.UnstructuredList, error) {
	if c.denied != nil {
		return nil, c.denied
	}
	listOpts := metav1.ListOptions{LabelSelector: opts.LabelSelector, FieldSelector: opts.FieldSelector}
	if rt.Namespaced && opts.Namespace != "" && opts.Namespace != AllNamespaces {
		return c.dynamic.Resource(rt.GVR()).Namespace(opts.Namespace).List(ctx, listOpts)
	}
	return c.dynamic.Resource(rt.GVR()).List(ctx, listOpts)
}

func (c *Client) Get(ctx context.Context, rt *ResourceType, namespace, name string) (*unstructured.Unstructured, error) {
	if c.denied != nil {
		return nil, c.denied
	}
	if rt.Namespaced {
		return c.dynamic.Resource(rt.GVR()).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	return c.dynamic.Resource(rt.GVR()).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) Table(ctx context.Context, rt *ResourceType, opts ListOptions) (Table, error) {
	if c.denied != nil {
		return Table{}, c.denied
	}
	u, err := c.tableURL(rt, opts.Namespace)
	if err != nil {
		return Table{}, err
	}
	q := u.Query()
	q.Set("includeObject", "Object")
	if opts.LabelSelector != "" {
		q.Set("labelSelector", opts.LabelSelector)
	}
	if opts.FieldSelector != "" {
		q.Set("fieldSelector", opts.FieldSelector)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Table{}, err
	}
	req.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Table{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Table{}, err
	}
	if resp.StatusCode >= 400 {
		return Table{}, tableResponseError(resp.StatusCode, resp.Status, body)
	}
	var raw struct {
		ColumnDefinitions []metav1.TableColumnDefinition `json:"columnDefinitions"`
		Rows              []struct {
			Cells  []any           `json:"cells"`
			Object json.RawMessage `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Table{}, err
	}
	table := Table{Resource: *rt, Clusters: []string{}}
	for _, col := range raw.ColumnDefinitions {
		table.Columns = append(table.Columns, Column{
			Name:        col.Name,
			Type:        col.Type,
			Format:      col.Format,
			Description: col.Description,
		})
	}
	if raw.Rows == nil {
		table.Rows = []Row{}
	}
	for _, row := range raw.Rows {
		obj := map[string]any{}
		if len(row.Object) > 0 {
			_ = json.Unmarshal(row.Object, &obj)
		}
		table.Rows = append(table.Rows, Row{Cells: row.Cells, Object: obj})
	}
	return table, nil
}

// tableResponseError turns a >=400 Table response into a typed error. The Table
// path is a hand-rolled HTTP request (not the dynamic client), so without this it
// returned a plain string error that IsForbidden/IsNotFound/IsAPIStatusError
// could not classify -- which left a 403 on a list looking "unreachable". When
// the body is a parseable apiserver Status (the normal case) we return the
// matching *StatusError, so the error classifies exactly like a dynamic-client
// error and its message names the verb/resource/namespace; a non-Status body (an
// HTML/text error page, a proxy 502) falls back to the descriptive string so
// odd responses stay readable.
func tableResponseError(statusCode int, status string, body []byte) error {
	var s metav1.Status
	if err := json.Unmarshal(body, &s); err == nil && s.Kind == "Status" && s.Status == metav1.StatusFailure {
		if s.Code == 0 {
			s.Code = int32(statusCode)
		}
		return &kerrors.StatusError{ErrStatus: s}
	}
	return fmt.Errorf("kubernetes table request failed: %s: %s", status, strings.TrimSpace(string(body)))
}

func (c *Client) Logs(ctx context.Context, opts LogOptions) (string, error) {
	if c.denied != nil {
		return "", c.denied
	}
	req := c.core.CoreV1().Pods(opts.Namespace).GetLogs(opts.Pod, &corev1.PodLogOptions{
		Container:  opts.Container,
		Timestamps: opts.Timestamps,
		TailLines:  &opts.TailLines,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Client) tableURL(rt *ResourceType, namespace string) (*url.URL, error) {
	base, err := url.Parse(c.config.Host)
	if err != nil {
		return nil, err
	}
	parts := []string{""}
	if rt.Group == "" {
		parts = append(parts, "api", rt.Version)
	} else {
		parts = append(parts, "apis", rt.Group, rt.Version)
	}
	if rt.Namespaced && namespace != "" && namespace != AllNamespaces {
		parts = append(parts, "namespaces", namespace)
	}
	parts = append(parts, rt.Plural)
	segments := append([]string{base.Path}, parts[1:]...)
	base.Path = path.Join(segments...)
	if !strings.HasPrefix(base.Path, "/") {
		base.Path = "/" + base.Path
	}
	return base, nil
}

func isListableResource(res *metav1.APIResource) bool {
	if strings.Contains(res.Name, "/") {
		return false
	}
	hasGet := false
	hasList := false
	for _, verb := range res.Verbs {
		switch verb {
		case "get":
			hasGet = true
		case "list":
			hasList = true
		}
	}
	return hasGet && hasList
}

func metricsResourceType(namespaced bool) ResourceType {
	if namespaced {
		return ResourceType{
			Group:      "metrics.k8s.io",
			Version:    "v1beta1",
			APIVersion: "metrics.k8s.io/v1beta1",
			Kind:       "PodMetrics",
			Plural:     "pods",
			Namespaced: true,
			Verbs:      []string{"get", "list"},
		}
	}
	return ResourceType{
		Group:      "metrics.k8s.io",
		Version:    "v1beta1",
		APIVersion: "metrics.k8s.io/v1beta1",
		Kind:       "NodeMetrics",
		Plural:     "nodes",
		Namespaced: false,
		Verbs:      []string{"get", "list"},
	}
}

func sortResourceTypes(types []ResourceType, preferred map[string]string) {
	sort.SliceStable(types, func(i, j int) bool {
		ip := preferred[types[i].Plural] == types[i].APIVersion
		jp := preferred[types[j].Plural] == types[j].APIVersion
		if ip != jp {
			return ip
		}
		if types[i].Kind != types[j].Kind {
			return types[i].Kind < types[j].Kind
		}
		return types[i].APIVersion < types[j].APIVersion
	})
}

func IsNotFound(err error) bool {
	return kerrors.IsNotFound(err) || errors.Is(err, ErrResourceTypeNotFound)
}

// IsForbidden reports whether err is an apiserver 403 (RBAC denial). It mirrors
// IsNotFound so the web layer can split a single-cluster list/detail failure
// into the "not allowed" state (a 403 naming the verb/resource/namespace) versus
// the "unreachable" state (any other transport/discovery error), without the web
// package importing k8s.io/apimachinery error helpers directly.
func IsForbidden(err error) bool {
	return kerrors.IsForbidden(err) || kerrors.IsUnauthorized(err)
}

// IsAPIStatusError reports whether err carries a structured apiserver Status
// response -- i.e. the request reached the API server and got a typed error
// back (any HTTP status). A transport-level failure (dial/timeout/no-such-host)
// is NOT an API status error, so the web layer treats `!IsAPIStatusError` as
// "unreachable" (the cluster could not be reached at all) and shows the real
// transport error, while a 5xx WITH a Status stays on the redacted error page.
func IsAPIStatusError(err error) bool {
	var status kerrors.APIStatus
	return errors.As(err, &status)
}
