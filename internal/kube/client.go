package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// ErrWatchGone is the typed 410: the watch's resourceVersion fell out of the
// apiserver's history window — either an HTTP 410 response at connect time or
// an in-stream ERROR event with reason Expired/Gone. The caller must relist
// to capture a fresh resourceVersion and re-watch from it.
var ErrWatchGone = errors.New("watch resource version expired")

type Client struct {
	config         *rest.Config
	httpClient     *http.Client
	discovery      discovery.DiscoveryInterface
	dynamic        dynamic.Interface
	core           kubernetes.Interface
	includeSecrets bool
	// denied, when set, makes every request method short-circuit with this error
	// instead of reaching the apiserver. It backs the anonymous-base denial:
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
	cfg.Username = ""
	cfg.Password = ""
	cfg.CertData = nil
	cfg.CertFile = ""
	cfg.KeyData = nil
	cfg.KeyFile = ""
	// Passthrough must evaluate RBAC AS THE VIEWER. rest.CopyConfig propagates a
	// static Impersonate, and client-go's impersonating round-tripper keys off
	// Impersonate.UserName (NOT the Authorization header), so without this clear a
	// passthrough request against a cluster with a static impersonation identity
	// would silently get that identity's RBAC instead of the viewer's.
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
// Used by the web layer's passthrough denial predicate.
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
// Forbidden error. It shares the underlying clients (which it never uses,
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

// discoveryResult carries one outcome of the blocking discovery call back from
// its goroutine.
type discoveryResult struct {
	lists []*metav1.APIResourceList
	err   error
}

// discoverResources runs client-go's ServerGroupsAndResources -- which takes no
// context and blocks until the OS reaps a dead connection (a TCP-blackholed
// cluster can hold it for one to two minutes) -- and races it against ctx so a
// caller's deadline actually cuts the call. On ctx expiry it returns ctx.Err()
// wrapped with %w so the chain still classifies as a timeout.
//
// The discovery call runs in a goroutine that delivers its result on a buffered
// channel, so when ctx wins the race the goroutine can still send and exit
// instead of leaking blocked on the send. The goroutine itself keeps running
// against the dead connection until the OS TCP timeout reaps it; that one leaked
// goroutine per timed-out discovery is an accepted trade -- there is no way to
// interrupt the context-less client-go call, and the alternative (holding the
// caller until the OS gives up) is exactly the hang we are removing.
func (c *Client) discoverResources(ctx context.Context) ([]*metav1.APIResourceList, error) {
	resultCh := make(chan discoveryResult, 1)
	go func() {
		_, lists, err := c.discovery.ServerGroupsAndResources()
		resultCh <- discoveryResult{lists: lists, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("kube discovery: %w", ctx.Err())
	case res := <-resultCh:
		return res.lists, res.err
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

	lists, err := c.discoverResources(ctx)
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
	listOpts := metav1.ListOptions{LabelSelector: opts.LabelSelector, FieldSelector: opts.FieldSelector, Limit: opts.Limit}
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
	if opts.Limit > 0 {
		q.Set("limit", strconv.FormatInt(opts.Limit, 10))
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
	return decodeTable(rt, body)
}

// decodeTable decodes a meta.k8s.io Table document — a LIST response body or a
// watch event's object — into kube.Table. This is the single Table decode
// seam: list metadata is captured here (resourceVersion for watch resumption;
// remainingItemCount for the sidebar counts).
func decodeTable(rt *ResourceType, body []byte) (Table, error) {
	var raw struct {
		Metadata          metav1.ListMeta                `json:"metadata"`
		ColumnDefinitions []metav1.TableColumnDefinition `json:"columnDefinitions"`
		Rows              []struct {
			Cells  []any           `json:"cells"`
			Object json.RawMessage `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Table{}, err
	}
	table := Table{
		Resource:           *rt,
		Clusters:           []string{},
		RemainingItemCount: raw.Metadata.RemainingItemCount,
		ResourceVersion:    raw.Metadata.ResourceVersion,
	}
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

// WatchTable opens a Table-format watch on rt: the same Table-Accept request
// Table() builds, with `watch=true&resourceVersion=<rv>&allowWatchBookmarks=true`
// (rv = the captured list Table.ResourceVersion). The returned stream yields
// decoded events through Next. No client-go informer machinery: the Live list
// screen consumes 1-row Table events, so raw REST against the Table endpoint
// suffices.
func (c *Client) WatchTable(ctx context.Context, rt *ResourceType, opts WatchOptions) (*TableWatch, error) {
	if c.denied != nil {
		return nil, c.denied
	}
	u, err := c.tableURL(rt, opts.Namespace)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("watch", "true")
	q.Set("allowWatchBookmarks", "true")
	q.Set("includeObject", "Object")
	if opts.ResourceVersion != "" {
		q.Set("resourceVersion", opts.ResourceVersion)
	}
	if opts.LabelSelector != "" {
		q.Set("labelSelector", opts.LabelSelector)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		// A 410 at connect time (resourceVersion already outside the history
		// window) carries the same caller contract as the in-stream ERROR
		// event: relist, then re-watch.
		if resp.StatusCode == http.StatusGone {
			return nil, fmt.Errorf("%w: %s", ErrWatchGone, strings.TrimSpace(string(body)))
		}
		return nil, tableResponseError(resp.StatusCode, resp.Status, body)
	}
	return &TableWatch{
		resource: *rt,
		ctx:      ctx,
		body:     resp.Body,
		dec:      json.NewDecoder(resp.Body),
	}, nil
}

// TableWatch is one open Table-format watch stream. Next decodes events until
// the stream ends; the ending error is typed so the consumer's lifecycle
// can branch without string matching:
//
//   - io.EOF — the upstream closed the stream cleanly (re-watch from the last
//     seen resourceVersion);
//   - the context error (context.Canceled / DeadlineExceeded) — the CALLER
//     ended the watch; never conflated with upstream EOF;
//   - ErrWatchGone — the resourceVersion expired (relist, then re-watch);
//   - a *StatusError — the apiserver sent a non-410 ERROR event.
type TableWatch struct {
	resource  ResourceType
	ctx       context.Context
	body      io.ReadCloser
	dec       *json.Decoder
	closeOnce sync.Once
}

// Next blocks for the next watch event. Data and bookmark events decode
// through the same seam as list responses (decodeTable): watch frames carry
// 1-row Tables whose columnDefinitions are populated only in the stream's
// FIRST event — the consumer caches those columns for subsequent events.
func (w *TableWatch) Next() (WatchEvent, error) {
	var frame struct {
		Type   string          `json:"type"`
		Object json.RawMessage `json:"object"`
	}
	if err := w.dec.Decode(&frame); err != nil {
		// The caller ending the watch (cancel/deadline) wins over whatever
		// shape the aborted read takes; a clean upstream close under a live
		// context is io.EOF — the two stream ends stay distinct.
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return WatchEvent{}, ctxErr
		}
		if errors.Is(err, io.EOF) {
			return WatchEvent{}, io.EOF
		}
		return WatchEvent{}, fmt.Errorf("read watch stream: %w", err)
	}
	switch WatchEventType(frame.Type) {
	case WatchError:
		var s metav1.Status
		if err := json.Unmarshal(frame.Object, &s); err != nil {
			return WatchEvent{}, fmt.Errorf("decode watch ERROR status: %w", err)
		}
		if s.Code == http.StatusGone || s.Reason == metav1.StatusReasonExpired || s.Reason == metav1.StatusReasonGone {
			return WatchEvent{}, fmt.Errorf("%w: %s", ErrWatchGone, s.Message)
		}
		return WatchEvent{}, &kerrors.StatusError{ErrStatus: s}
	case WatchAdded, WatchModified, WatchDeleted, WatchBookmark:
		table, err := decodeTable(&w.resource, frame.Object)
		if err != nil {
			return WatchEvent{}, fmt.Errorf("decode %s watch event: %w", frame.Type, err)
		}
		return WatchEvent{Type: WatchEventType(frame.Type), Table: table, ResourceVersion: table.ResourceVersion}, nil
	default:
		return WatchEvent{}, fmt.Errorf("unknown watch event type %q", frame.Type)
	}
}

// Close releases the stream's HTTP body; a blocked Next unblocks with an
// error. Safe to call more than once.
func (w *TableWatch) Close() error {
	var err error
	w.closeOnce.Do(func() { err = w.body.Close() })
	return err
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
// transport error.
func IsAPIStatusError(err error) bool {
	var status kerrors.APIStatus
	return errors.As(err, &status)
}

// IsServerError reports whether err is an apiserver Status with a 5xx code --
// the apiserver was reached but failed to serve the request. The web layer
// folds this into the unreachable whole-list/detail state (the card shows
// the REAL Status message verbatim); 4xx Statuses (bad selectors, conflicts)
// keep their existing handling.
func IsServerError(err error) bool {
	var status kerrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	return status.Status().Code >= 500
}

// FailureKind is the single classification of an upstream failure. It is a
// string so it can double as a metrics label later. The same upstream failure
// classifies to the same kind everywhere, so every presentation path (HTTP
// status, list/detail state, search chip) reads from one source of truth.
type FailureKind string

const (
	// FailureForbidden is an apiserver 403 (RBAC denies the verb on the
	// resource) -- the credentials are valid but not allowed.
	FailureForbidden FailureKind = "forbidden"
	// FailureUnauthorized is an apiserver 401 (the credentials are missing,
	// expired, or rejected). It is kept distinct from forbidden even though the
	// IsForbidden helper folds the two together for callers that want the merge.
	FailureUnauthorized FailureKind = "unauthorized"
	// FailureNotFound is an apiserver 404 (the resource type or object does not
	// exist).
	FailureNotFound FailureKind = "not_found"
	// FailureTimeout is a deadline/timeout: a context deadline or a net.Error
	// that reports Timeout(). The request did not complete in time.
	FailureTimeout FailureKind = "timeout"
	// FailureUnreachable is a transport-level failure that never reached the
	// apiserver: a refused connection or an unroutable/unresolved host.
	FailureUnreachable FailureKind = "unreachable"
	// FailureUpstream5xx is an apiserver Status with a 5xx code: the apiserver
	// was reached but failed to serve the request.
	FailureUpstream5xx FailureKind = "upstream_5xx"
	// FailureInternal is the total-taxonomy fallback: any other apiserver Status
	// (400, 409, 429, 410, ...), a cancelled context (the client went away), and
	// any unrecognized error all fold here.
	FailureInternal FailureKind = "internal"
)

// ClassifyError maps any upstream failure to exactly one FailureKind. The
// taxonomy is total: every error resolves to a kind, and unrecognized errors
// fold to FailureInternal. Classification is by typed checks through
// errors.Is/errors.As (wrapped chains classify correctly) and syscall-level
// transport detection -- never by matching the error string.
//
// Order matters: typed apiserver Statuses are checked before transport
// heuristics so a structured 5xx is not mistaken for a generic failure, and the
// timeout check precedes the refused/no-route check so a dial timeout reads as a
// timeout rather than an unreachable host.
func ClassifyError(err error) FailureKind {
	if err == nil {
		return FailureInternal
	}

	// Typed apiserver Status responses (the request reached the apiserver).
	var status kerrors.APIStatus
	if errors.As(err, &status) {
		switch code := status.Status().Code; {
		case code == http.StatusForbidden:
			return FailureForbidden
		case code == http.StatusUnauthorized:
			return FailureUnauthorized
		case code == http.StatusNotFound:
			return FailureNotFound
		case code >= 500:
			return FailureUpstream5xx
		default:
			// Any other apiserver status (400, 409, 410, 429, ...) folds to
			// internal -- the taxonomy carries no dedicated kind for them.
			return FailureInternal
		}
	}

	// The resource-type-not-found sentinel from FindResource is not an apiserver
	// Status, so it would otherwise fall through to internal -- but it is a
	// not-found (IsNotFound treats it so). The precise sentinel check restores the
	// pre-refactor not-found routing without folding in kerrors 404 (already
	// handled by the APIStatus block above).
	if errors.Is(err, ErrResourceTypeNotFound) {
		return FailureNotFound
	}

	// A context deadline is a timeout; a context cancellation means the client
	// went away and carries no special kind.
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	if errors.Is(err, context.Canceled) {
		return FailureInternal
	}

	// A net.Error that reports a timeout (e.g. an i/o or dial timeout) is a
	// timeout before it is anything else.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return FailureTimeout
	}

	// Syscall-level transport failures: a refused connection or an
	// unroutable/unreachable host never reached the apiserver.
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return FailureUnreachable
	}

	// An unresolved host (DNS) is also a transport-level unreachable failure.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return FailureUnreachable
	}

	return FailureInternal
}
