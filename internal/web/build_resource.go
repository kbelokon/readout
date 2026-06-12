package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/hooks"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/yamlview"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
)

// resourcePrerenderHook consults the configured JSON resource-prerender hook,
// returning the (possibly extended) link list and an optional replacement
// resource. The hook IO lives in internal/hooks; this adapter maps the inputs
// to the hook's request DTO and folds the result back into the link list.
func (s *Server) resourcePrerenderHook(ctx context.Context, cluster, namespace, plural string, object *kube.Object, links []config.Link) ([]config.Link, map[string]any, error) {
	if s.cfg.ResourcePrerenderHookURL == "" {
		return links, nil, nil
	}
	result, err := s.hooks.Prerender(ctx, s.cfg.ResourcePrerenderHookURL, &hooks.PrerenderRequest{
		Cluster:   cluster,
		Namespace: namespace,
		Plural:    plural,
		Resource:  object.Raw,
		Links:     links,
	})
	if err != nil {
		return nil, nil, err
	}
	// Hook-returned links are attacker-influenceable runtime data: drop and log
	// any with a disallowed scheme (e.g. javascript:/data:) before they reach
	// the link list. The template additionally sanitizes config/hook hrefs via
	// templ.URL, but dropping here keeps an unrenderable link out entirely and
	// surfaces the bad hook response in the logs.
	for _, link := range result.Links {
		if !config.LinkSchemeAllowed(link.Href) {
			slog.Warn("dropping prerender-hook link with disallowed scheme",
				"cluster", cluster, "namespace", namespace, "plural", plural, "href", link.Href)
			continue
		}
		links = append(links, link)
	}
	return links, result.Resource, nil
}

// build_resource.go is the data-assembly layer for the resource-view (object
// detail) page and the related-pods/events sub-data it fetches. It turns the
// kube client + parsed request inputs into the plain detailView; no HTML is
// emitted here.

// buildDetailView fetches the object plus its related data and resolves every
// request-derived flag/href into a render-ready detailView. It returns the view
// and, separately, the raw object (the handler still needs it for the
// download=yaml short-circuit) — but the byte-producing path runs through the
// returned view. A nil *detailView signals the handler already wrote a response
// (download or hook error) or hit an error it surfaced.
func (s *Server) buildDetailView(w http.ResponseWriter, r *http.Request, client *kube.Client, cluster *kube.Cluster) (*detailView, bool) {
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	name := r.PathValue("name")
	if namespace != "" && !s.namespaceAllowed(namespace) {
		s.error(w, r, statusError{status: http.StatusForbidden, message: "namespace is not allowed"})
		return nil, false
	}
	legacyNamespaceObjectPath := namespace != "" && plural == "namespaces" && name == namespace
	resourceNamespaced := namespace != "" && !legacyNamespaceObjectPath
	rt, err := client.FindResource(r.Context(), plural, resourceNamespaced, apiVersionParam(r))
	if err != nil {
		if state := s.detailState(r, cluster, plural, name, namespace, "get", err); state != nil {
			return state, true
		}
		s.error(w, r, err)
		return nil, false
	}
	getNamespace := namespace
	if !rt.Namespaced {
		getNamespace = ""
	}
	obj, err := client.Get(r.Context(), &rt, getNamespace, name)
	if err != nil {
		if state := s.detailState(r, cluster, plural, name, namespace, "get", err); state != nil {
			return state, true
		}
		s.error(w, r, err)
		return nil, false
	}
	object := kube.NewObject(&rt, obj)
	// The Secret data view reads only the key NAMES, and it must read them from
	// the typed corev1.Secret BEFORE maskSecret overwrites the values with the
	// non-base64 mask sentinel (which would make the corev1.Secret base64 decode
	// fail). So resolve the key set here, then mask the raw object for rendering.
	var secretView *secretDataView
	if object.Kind() == "Secret" {
		secretView = buildSecretDataView(&object)
		maskSecret(object.Raw)
	}
	if r.URL.Query().Get("download") == "yaml" {
		s.downloadYAML(w, r, object.Raw)
		return nil, false
	}
	renderNamespace := namespace
	if object.Kind() == "Namespace" {
		renderNamespace = object.Name()
	}
	links := s.objectLinks(cluster.Name, renderNamespace, &object)
	if hookLinks, replacement, err := s.resourcePrerenderHook(r.Context(), cluster.Name, renderNamespace, plural, &object, links); err != nil {
		s.error(w, r, err)
		return nil, false
	} else {
		links = hookLinks
		if replacement != nil {
			object.Raw = replacement
		}
	}
	relatedPods := s.relatedPods(r, client, cluster, &object, namespace)
	events := s.events(r, client, &object, namespace)
	owners := s.ownerLinks(r, client, cluster, &object)

	pageTitle := object.Name() + " (" + object.Kind() + ")"
	if renderNamespace != "" && object.Kind() != "Namespace" {
		pageTitle = object.Name() + " (" + object.Kind() + " in " + renderNamespace + ")"
	}

	viewParam := r.URL.Query().Get("view")
	v := &detailView{
		Cluster:      cluster.Name,
		Namespace:    renderNamespace,
		Object:       object,
		Title:        pageTitle,
		DownloadHref: objectDownloadYAMLHref(cluster.Name, renderNamespace, &object),
		Links:        links,
		IsYAMLView:   viewParam == "yaml",
		IsEventsView: viewParam == "events",
		Owners:       owners,
	}
	v.YAMLTab = v.IsYAMLView
	v.EventsTab = v.IsEventsView
	v.DefaultTab = !v.IsYAMLView && !v.IsEventsView
	if renderNamespace != "" && (object.Kind() == "Pod" || object.Kind() == "Deployment" || object.Kind() == "ReplicaSet" || object.Kind() == "DaemonSet" || object.Kind() == "StatefulSet") {
		v.LogsHref = fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s/logs", url.PathEscape(cluster.Name), url.PathEscape(renderNamespace), url.PathEscape(object.Resource.Plural), url.PathEscape(object.Name()))
	}
	if v.IsYAMLView {
		data, _ := yamlview.Marshal(object.Raw)
		v.HighlightedYAML = s.highlightYAML(cluster.Name, renderNamespace, &object, "", string(data))
	}
	if object.Kind() == "Namespace" {
		v.ShowNamespaceLinks = true
		v.AllObjectsHref = fmt.Sprintf("/clusters/%s/namespaces/%s/all", url.PathEscape(cluster.Name), url.PathEscape(renderNamespace))
		v.ResourceTypesHref = fmt.Sprintf("/clusters/%s/namespaces/%s/_resource-types", url.PathEscape(cluster.Name), url.PathEscape(renderNamespace))
	}
	if relatedPods != nil {
		v.RelatedPods = s.buildSubtableView(r, relatedPods, renderNamespace)
	}
	v.Events = s.buildEventViews(events)

	// Resolve the detail render data (labels/annotations/node summary/secret/
	// YAML cards) so the templ component reads plain data. The YAML view's
	// highlighted block was already resolved into v.HighlightedYAML above.
	v.CreatedMeta = formatTimestamp(object.CreationTimestamp())
	v.Version = nestedString(object.Raw, "metadata", "resourceVersion")
	v.NameHead, v.NameTail, v.NameTitle = detailNameParts(&object)
	if v.DefaultTab {
		v.Labels = buildLabelChips(cluster.Name, renderNamespace, &object)
		v.Annotations, v.AnnotationsLong = buildAnnotationChips(&object)
		if object.Kind() == "Node" {
			v.Node = buildNodeSummaryView(&object)
		}
		if object.Kind() == "Pod" {
			v.Containers = s.buildContainersView(&object, s.podContainerMetrics(r, client, renderNamespace, object.Name()))
		}
		v.Secret = secretView
		v.YAMLCards = s.buildYAMLCards(cluster.Name, renderNamespace, &object)
	}
	return v, true
}

// detailNameParts resolves the detail H1's head/tail split: the same
// splitObjectName + MiddleTruncate pair the table name cells apply,
// so a pod/replicaset hash tail renders faint even in the title
// and an over-42-char head middle-truncates with the FULL name riding
// in the title= tooltip (nameTitle non-empty only then).
func detailNameParts(object *kube.Object) (head, tail, nameTitle string) {
	head, tail = splitObjectName(object.Resource.Plural, object.Name())
	if display, truncated := MiddleTruncate(head, nameHeadMax, nameHeadLead, nameHeadTrail); truncated {
		head = display
		nameTitle = object.Name()
	}
	return head, tail, nameTitle
}

// detailState classifies a detail-page fetch failure into the forbidden state
// (a 403 naming the verb/resource/namespace) or the unreachable state (a
// transport/dial failure or an apiserver 5xx Status, shown with the REAL error
// string in the mono errdetail block), returning a state-only detailView
// the handler renders at 200. It returns nil for any other failure (a NotFound
// object -> a real 404, a 4xx Status, the policy 403), so the caller falls
// through to s.error and the existing status-code page. The breadcrumb is built
// from the request path (cluster/namespace/plural/name) so no fetched object is
// needed -- the fetch is exactly what failed.
func (s *Server) detailState(r *http.Request, cluster *kube.Cluster, plural, name, namespace, verb string, err error) *detailView {
	kind, ok := failureListState(kube.ClassifyError(err), err)
	if !ok {
		return nil
	}
	state := &detailStateView{
		Cluster:   cluster.Name,
		Verb:      verb,
		Resource:  plural,
		Name:      name,
		Namespace: namespace,
		RetryHref: r.URL.String(),
		BackHref:  "/clusters",
	}
	if kind == stateForbidden {
		state.Kind = stateForbidden
		state.Hint = forbiddenStateHint
		state.Detail = "403 Forbidden · " + err.Error()
	} else {
		state.Kind = stateUnreachable
		state.Hint = unreachableStateHint(kube.IsAPIStatusError(err))
		state.Detail = err.Error()
	}
	title := name + " (" + plural + ")"
	return &detailView{Cluster: cluster.Name, Namespace: namespace, Title: title, State: state}
}

// buildLabelChips resolves the object's label chips: sorted keys, the
// per-chip selector href (namespaced or cluster-scoped) and the full ro-chip
// class.
func buildLabelChips(cluster, namespace string, object *kube.Object) []labelChipView {
	labels := object.Labels()
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]labelChipView, 0, len(keys))
	for _, key := range keys {
		val := labels[key]
		// Label-chip click-to-filter: the chip navigates to this
		// KIND's list in the same cluster/namespace with the `label:key=value`
		// chip applied (`?f=`), riding the same grammar the list filter engine
		// parses. The chip text is QueryEscape'd whole, so '/', '=' and any comma
		// in the value survive as literal characters (a %2C is a literal comma
		// inside one alternative, never an OR split).
		chip := url.QueryEscape("label:" + key + "=" + val)
		var href string
		if namespace != "" {
			href = fmt.Sprintf("/clusters/%s/namespaces/%s/%s?f=%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(object.Resource.Endpoint()), chip)
		} else {
			href = fmt.Sprintf("/clusters/%s/%s?f=%s", url.PathEscape(cluster), url.PathEscape(object.Resource.Endpoint()), chip)
		}
		out = append(out, labelChipView{
			Href: href,
			Key:  key,
			Val:  val,
		})
	}
	return out
}

// annotationLongThreshold is the annotation chip/block split: an annotation
// value over this many bytes (last-applied-configuration and friends) is no
// chip — it renders as a collapsed `key · size` toggle expanding to a
// scrollable <pre>. At or under it, the value stays a chip (40-char display
// cut, full value in the title= tooltip).
const annotationLongThreshold = 120

// buildAnnotationChips resolves the annotations (sorted keys) into the two
// chip/block forms. Chips (≤120 chars): Val is the value truncated to 40 for
// the clipped chip body; Full is the complete "key: value" string for the
// title= tooltip. Long values (>120 chars): the key + humanBytes size for the
// collapsed toggle, plus the full value for the expandable <pre> — a long
// value never gets a chip OR a tooltip-only rendering.
func buildAnnotationChips(object *kube.Object) ([]annotationChipView, []annotationLongView) {
	annotations := object.Annotations()
	if len(annotations) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var chips []annotationChipView
	var long []annotationLongView
	for _, key := range keys {
		value := annotations[key]
		if len(value) > annotationLongThreshold {
			long = append(long, annotationLongView{
				Key:   key,
				Size:  humanBytes(int64(len(value))),
				Value: value,
			})
			continue
		}
		chips = append(chips, annotationChipView{
			Key:  key,
			Val:  truncate(value, 40),
			Full: key + ": " + value,
		})
	}
	return chips, long
}

// podContainerMetrics fetches the pod's PodMetrics object and resolves its
// per-container usage map. Availability detection mirrors fetchMetricsUsage: a
// cluster without metrics-server fails FindResourceByKind (discovery, cached
// 60s) and a too-young pod fails the Get — both yield nil, which renders every
// CPU/Memory cell as the faint "—" (real values only when the metrics
// join is live; never zeros invented for a dead join).
func (s *Server) podContainerMetrics(r *http.Request, client *kube.Client, namespace, name string) map[string]kube.ContainerUsage {
	rt, err := client.FindResourceByKind(r.Context(), "metrics.k8s.io/v1beta1", "PodMetrics", true)
	if err != nil {
		return nil
	}
	obj, err := client.Get(r.Context(), &rt, namespace, name)
	if err != nil {
		return nil
	}
	return kube.PodContainerUsage(obj.Object)
}

// buildContainersView resolves the pod containers table. Pod is a fixed
// kind, so the object is decoded once into a corev1.Pod. Rows are driven by
// the pod's declaration order — spec.initContainers first (each badged `init`),
// then spec.containers — so every declared container appears even before the
// kubelet posts a status; each row joins its status.initContainerStatuses /
// status.containerStatuses entry by container name (state, ready, restarts +
// ago) and its PodMetrics containers[] usage when the metrics join is live.
// Image and ports come from the spec side of the join. A pod that declares no
// containers (or fails to decode) yields nil — no section renders.
func (s *Server) buildContainersView(object *kube.Object, usage map[string]kube.ContainerUsage) *containersSectionView {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Raw, &pod); err != nil {
		return nil
	}
	if len(pod.Spec.Containers)+len(pod.Spec.InitContainers) == 0 {
		return nil
	}
	statuses := make(map[string]*corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for i := range pod.Status.InitContainerStatuses {
		statuses[pod.Status.InitContainerStatuses[i].Name] = &pod.Status.InitContainerStatuses[i]
	}
	for i := range pod.Status.ContainerStatuses {
		statuses[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
	}
	v := &containersSectionView{Count: len(pod.Spec.Containers), InitCount: len(pod.Spec.InitContainers)}
	now := s.clock()
	for i := range pod.Spec.InitContainers {
		v.Rows = append(v.Rows, containerRow(&pod.Spec.InitContainers[i], statuses, usage, true, now))
	}
	for i := range pod.Spec.Containers {
		v.Rows = append(v.Rows, containerRow(&pod.Spec.Containers[i], statuses, usage, false, now))
	}
	return v
}

// containerRow resolves one container row from the spec entry + its joined
// status and metrics. The state cell speaks the canonical status vocabulary:
// the word is Running / the terminated reason / the waiting reason (
// CrashLoopBackOff, ImagePullBackOff, Completed, ... ARE the state), toned by
// kube.StatusTone and pulsing only for the transient set (the motion law). An
// absent status renders the faint "—" state and an untoned 0-restart cell —
// the row never invents runtime facts the kubelet has not posted.
func containerRow(spec *corev1.Container, statuses map[string]*corev1.ContainerStatus, usage map[string]kube.ContainerUsage, init bool, now time.Time) containerRowView {
	row := containerRowView{
		Name:     spec.Name,
		Init:     init,
		Ports:    containerPortsText(spec.Ports),
		Image:    spec.Image,
		Restarts: "0",
	}
	if u, ok := usage[spec.Name]; ok {
		row.CPU = cpuFormat(u.CPU)
		row.Mem = memoryMiBFormat(u.Memory) + "Mi"
	}
	status := statuses[spec.Name]
	if status == nil {
		row.RestartsTone = restartsTone(row.Restarts)
		return row
	}
	row.State = containerStateWord(status)
	row.StateTone = kube.StatusTone(row.State)
	row.StatePulse = transientStatus(row.State)
	if !init {
		// The prototype's regular-row ready grammar; init rows keep the faint
		// "—" (an init container's readiness is its completion).
		if status.Ready {
			row.Ready, row.ReadyClass = "ready", "full"
		} else {
			row.Ready, row.ReadyClass = "not ready", "partial"
		}
	}
	row.Restarts = groupThousands(strconv.Itoa(int(status.RestartCount)))
	row.RestartsTone = restartsTone(row.Restarts)
	if status.RestartCount > 0 {
		if t := status.LastTerminationState.Terminated; t != nil && !t.FinishedAt.IsZero() {
			row.Ago = "(" + duration.HumanDuration(now.Sub(t.FinishedAt.Time)) + " ago)"
		}
	}
	return row
}

// containerStateWord derives the status-vocabulary state word from a container status:
// Running, the terminated reason (Completed, Error, OOMKilled, ...), or the
// waiting reason (CrashLoopBackOff, ContainerCreating, ...) — the same words
// the pod list's Status column speaks, so kube.StatusTone owns their tones. A
// reason-less terminated/waiting state falls back to the bare state name; a
// status carrying none of the three yields "" (rendered as the faint "—").
func containerStateWord(status *corev1.ContainerStatus) string {
	switch state := &status.State; {
	case state.Running != nil:
		return "Running"
	case state.Terminated != nil:
		return first(state.Terminated.Reason, "Terminated")
	case state.Waiting != nil:
		return first(state.Waiting.Reason, "Waiting")
	}
	return ""
}

// containerPortsText renders a spec container's ports as the prototype's
// `port/PROTO` list ("1337/TCP", multi-port comma-joined). The protocol
// defaults to TCP when the spec omits it (the API default). No ports yields
// "" — the cell renders the faint "—".
func containerPortsText(ports []corev1.ContainerPort) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		proto := string(p.Protocol)
		if proto == "" {
			proto = "TCP"
		}
		parts = append(parts, strconv.Itoa(int(p.ContainerPort))+"/"+proto)
	}
	return strings.Join(parts, ", ")
}

// buildNodeSummaryView resolves the Node-kind summary blocks (conditions,
// capacity/allocatable, system info). Node is a fixed kind, so the object is
// decoded once into a corev1.Node and the blocks are read off typed fields:
// conditions from Status.Conditions, capacity/allocatable from the typed
// ResourceLists (rendered via Quantity.String(), which round-trips the original
// canonical form), and the system-info rows from NodeSystemInfo.
func buildNodeSummaryView(object *kube.Object) *nodeSummaryView {
	var node corev1.Node
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Raw, &node); err != nil {
		return &nodeSummaryView{}
	}
	status := node.Status
	n := &nodeSummaryView{}
	for _, condition := range status.Conditions {
		typ := string(condition.Type)
		value := string(condition.Status)
		title := typ + " = " + value
		if condition.Reason != "" {
			title += " (" + condition.Reason + ")"
		}
		n.Conditions = append(n.Conditions, nodeConditionView{
			Tone:  nodeConditionTone(typ, value),
			Title: title,
			Type:  typ,
			Value: value,
		})
	}
	capacity := resourceListMap(status.Capacity)
	allocatable := resourceListMap(status.Allocatable)
	if len(capacity) > 0 || len(allocatable) > 0 {
		n.HasCapAlloc = true
		if len(capacity) > 0 {
			n.Capacity = buildKVList(capacity, "")
		}
		if len(allocatable) > 0 {
			n.Allocatable = buildKVList(allocatable, "allocatable ")
		}
	}
	if nodeInfo := nodeInfoMap(&status.NodeInfo); len(nodeInfo) > 0 {
		n.NodeInfo = buildKVList(nodeInfo, "")
	}
	return n
}

// resourceListMap renders a typed ResourceList into the name->string map the KV
// list consumes, using Quantity.String() so each value keeps its original
// canonical spelling (e.g. "1930m", "8047476Ki").
func resourceListMap(list corev1.ResourceList) map[string]any {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]any, len(list))
	for name, quantity := range list {
		out[string(name)] = quantity.String()
	}
	return out
}

// nodeInfoMap converts the typed NodeSystemInfo back into a generic map so the
// system-info block renders the same sorted key/value rows as before (only the
// non-empty fields, matching the fixture's nodeInfo keys).
func nodeInfoMap(info *corev1.NodeSystemInfo) map[string]any {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(info)
	if err != nil {
		return nil
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		if s, ok := value.(string); ok && s != "" {
			out[key] = s
		}
	}
	return out
}

// buildKVList resolves a sorted key/value list (with an optional key prefix).
func buildKVList(values map[string]any, keyPrefix string) *kvListView {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	l := &kvListView{}
	for _, key := range keys {
		l.Rows = append(l.Rows, kvRowView{Key: keyPrefix + key, Val: fmt.Sprint(values[key])})
	}
	return l
}

// buildSecretDataView resolves the masked-Secret data block (key names only).
// Secret is a fixed kind, so the object is decoded into a corev1.Secret and the
// key names are read off Secret.Data — values are never read (they were masked
// upstream; only the sorted key set renders).
func buildSecretDataView(object *kube.Object) *secretDataView {
	if object.Kind() != "Secret" {
		return nil
	}
	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Raw, &secret); err != nil {
		return nil
	}
	if len(secret.Data) == 0 {
		return nil
	}
	names := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		names = append(names, key)
	}
	sort.Strings(names)
	return &secretDataView{KeyCount: len(secret.Data), Keys: names}
}

// buildYAMLCards resolves the per-section YAML cards: sorted top-level keys
// (excluding metadata/apiVersion/kind and the Secret data key), the capitalized
// title, and the highlighted-YAML content. The status card starts Collapsed
// (Spec open, Status collapsed by default — status is the
// machine-noise section on every kind); the readout.js fold toggle reopens it.
func (s *Server) buildYAMLCards(cluster, namespace string, object *kube.Object) []yamlCardView {
	keys := make([]string, 0, len(object.Raw))
	for key := range object.Raw {
		if key == "metadata" || key == "apiVersion" || key == "kind" || (object.Kind() == "Secret" && key == "data") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]yamlCardView, 0, len(keys))
	for _, key := range keys {
		data, _ := yamlview.Marshal(object.Raw[key])
		out = append(out, yamlCardView{
			Name:      key,
			Title:     capitalizeWord(key),
			Content:   s.highlightYAML(cluster, namespace, object, key+"-", string(data)),
			Collapsed: key == "status",
		})
	}
	return out
}

// eventItem is the local typed view of an event object. The core/v1 `events`
// endpoint dual-writes BOTH the old core/v1 Event shape and the newer
// events.k8s.io/v1 shape into the same list (and an events.k8s.io list spells
// the legacy fields `deprecated*`), so this one struct carries every spelling
// and the accessors below normalize between them with the PINNED
// precedence. Decoded once via FromUnstructured at the decodeEventItem seam
// (a fixed, known kind) — the detail events tab (buildEventViews) and the
// events list cells (buildCellView) share it.
type eventItem struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	// Timestamps, both API spellings. eventTime is a metav1.MicroTime
	// (RFC3339 with fractional seconds — parseEventTime handles both).
	FirstTimestamp           string `json:"firstTimestamp"`
	DeprecatedFirstTimestamp string `json:"deprecatedFirstTimestamp"`
	LastTimestamp            string `json:"lastTimestamp"`
	DeprecatedLastTimestamp  string `json:"deprecatedLastTimestamp"`
	EventTime                string `json:"eventTime"`
	// Counts, both API spellings, plus the series aggregate.
	Count           int64 `json:"count"`
	DeprecatedCount int64 `json:"deprecatedCount"`
	Series          struct {
		Count            int64  `json:"count"`
		LastObservedTime string `json:"lastObservedTime"`
	} `json:"series"`
	// Message: core/v1 `message`, events.k8s.io `note`.
	Message string `json:"message"`
	Note    string `json:"note"`
	// From: core/v1 source.component, events.k8s.io reportingController.
	Source struct {
		Component string `json:"component"`
	} `json:"source"`
	ReportingController string `json:"reportingController"`
	// Object reference: core/v1 involvedObject, events.k8s.io regarding.
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	Regarding struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"regarding"`
}

// decodeEventItem decodes a raw event object into the dual-shape eventItem.
// Whole-number JSON floats convert cleanly into the int64 count fields
// (runtime.DefaultUnstructuredConverter truncates only fractionless floats).
func decodeEventItem(raw map[string]any) (*eventItem, bool) {
	var event eventItem
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &event); err != nil {
		return nil, false
	}
	return &event, true
}

// eventCount is the pinned count precedence: series.count → count →
// deprecatedCount. An event that decodes no explicit count occurred once
// (the ×1 faint cell), never zero.
func (e *eventItem) eventCount() int64 {
	for _, n := range []int64{e.Series.Count, e.Count, e.DeprecatedCount} {
		if n > 0 {
			return n
		}
	}
	return 1
}

// firstSeen is the pinned first-seen precedence: firstTimestamp →
// deprecatedFirstTimestamp → eventTime.
func (e *eventItem) firstSeen() string {
	return first(e.FirstTimestamp, e.DeprecatedFirstTimestamp, e.EventTime)
}

// lastSeen is the pinned last-seen precedence: series.lastObservedTime →
// lastTimestamp → deprecatedLastTimestamp → eventTime.
func (e *eventItem) lastSeen() string {
	return first(e.Series.LastObservedTime, e.LastTimestamp, e.DeprecatedLastTimestamp, e.EventTime)
}

func (e *eventItem) message() string {
	return first(e.Message, e.Note)
}

func (e *eventItem) from() string {
	return first(e.Source.Component, e.ReportingController)
}

func (e *eventItem) refKind() string {
	return first(e.InvolvedObject.Kind, e.Regarding.Kind)
}

func (e *eventItem) refName() string {
	return first(e.InvolvedObject.Name, e.Regarding.Name)
}

// parseEventTime parses an event timestamp: RFC3339, with the fractional
// seconds a metav1.MicroTime (eventTime / series.lastObservedTime) carries
// accepted by the same layout (Go parses an in-input fraction even when the
// layout has none).
func parseEventTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, value)
	return t, err == nil
}

// eventAgeText builds the two-layer event age: the compressed kubectl
// duration since last-seen, plus the `(first <dur> ago)` second layer when
// count > 1 AND last − first > 60s (the pinned threshold — a tight burst
// stays single-layer because both layers would read the same). No last-seen
// at all yields "" so callers keep their own fallback.
func eventAgeText(e *eventItem, now time.Time) string {
	last, ok := parseEventTime(e.lastSeen())
	if !ok {
		return ""
	}
	age := duration.HumanDuration(now.Sub(last))
	if e.eventCount() > 1 {
		if firstT, ok := parseEventTime(e.firstSeen()); ok && last.Sub(firstT) > time.Minute {
			age += " (first " + duration.HumanDuration(now.Sub(firstT)) + " ago)"
		}
	}
	return age
}

// buildEventViews flattens raw event objects into render-ready event rows for
// the detail Events tab, which inherits the events-list cells: the Type
// cell tone is the redesign status tone mapped from kube.CellClass("events",
// "Type", <value>) via statusTone, then defaulted to "mute" for a Normal event
// (which carries no kube class) so the redesign dot still reads grey; the
// Count cell is the ×N countCellView (≥20 amber, 1 faint); the Age cell is the
// two-layer eventAgeText split by evAgeCellView (bucket-coloured lead token +
// faint remainder), with the full last-seen timestamp in the tooltip and
// "Unknown" (age-old) when no timestamp decodes; From is the resolved
// reporting component. The Message cell's static ro-event-msg class is emitted
// in the templ. Each event is decoded into eventItem so the core/v1,
// events.k8s.io/v1, and deprecated-* spellings normalize to one row.
func (s *Server) buildEventViews(events []map[string]any) []eventView {
	now := s.clock()
	out := make([]eventView, 0, len(events))
	for _, raw := range events {
		event, ok := decodeEventItem(raw)
		if !ok {
			continue
		}
		tone := statusTone(kube.CellClass("events", "Type", event.Type))
		if tone == "" {
			tone = "mute"
		}
		countCell := countCellView(int(event.eventCount()))
		ev := eventView{
			Type:       event.Type,
			Tone:       tone,
			Reason:     event.Reason,
			Count:      countCell.Value,
			CountClass: countCell.Class,
			Age:        "Unknown",
			AgeClass:   "age-old",
			From:       event.from(),
			Message:    event.message(),
		}
		if text := eventAgeText(event, now); text != "" {
			ageCell := evAgeCellView(text)
			ev.Age = ageCell.Value
			ev.AgeRest = ageCell.EvAgeRest
			ev.AgeClass = ageCell.Class
			ev.AgeTitle = "last seen " + formatTimestamp(event.lastSeen())
		}
		out = append(out, ev)
	}
	return out
}

// buildSubtableView resolves the related-pods subtable's request-derived sort
// hrefs and per-row cell render data.
func (s *Server) buildSubtableView(r *http.Request, table *kube.Table, namespace string) *subtableView {
	v := &subtableView{
		Table:       *table,
		Namespace:   namespace,
		CreatedHref: addQuery(r.URL, "sort", "Created"),
	}
	for _, column := range table.Columns {
		v.Columns = append(v.Columns, subtableColumn{
			Description: column.Description,
			SortHref:    addQuery(r.URL, "sort", column.Name),
			Name:        column.Name,
		})
	}
	clusterFallback := firstSlice(table.Clusters, []string{""})[0]
	for _, row := range table.Rows {
		rowNamespace := nestedString(row.Object, "metadata", "namespace")
		rowName := nestedString(row.Object, "metadata", "name")
		rowCluster := first(row.Cluster, clusterFallback)
		sr := subtableRow{
			StatusClass:  kube.RowStatusClass(table, row),
			ShowNs:       namespace == "",
			Namespace:    rowNamespace,
			CreatedClass: s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")),
			CreatedText:  formatTimestamp(nestedString(row.Object, "metadata", "creationTimestamp")),
		}
		if namespace == "" {
			sr.NsHref = fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(rowCluster), url.PathEscape(rowNamespace))
		}
		for idx, cell := range row.Cells {
			columnName := ""
			if idx < len(table.Columns) {
				columnName = table.Columns[idx].Name
			}
			sc := subtableCell{Value: cellDisplayString(cell)}
			if idx == 0 {
				sc.Kind = cellName
				sc.Value = cellString(row, idx)
				if rowNamespace != "" {
					sc.Href = fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s", url.PathEscape(rowCluster), url.PathEscape(rowNamespace), url.PathEscape(table.Resource.Plural), url.PathEscape(rowName))
				} else {
					sc.Href = fmt.Sprintf("/clusters/%s/%s/%s", url.PathEscape(rowCluster), url.PathEscape(table.Resource.Plural), url.PathEscape(rowName))
				}
				sr.Cells = append(sr.Cells, sc)
				continue
			}
			classes := []string{cellClass(table, idx, cell)}
			if columnName == "Age" || columnName == "First Seen" {
				classes = append(classes, s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")))
			}
			sc.Class = strings.Join(classes, " ")
			switch columnName {
			case "Node":
				sc.Kind = cellNode
				sc.Href = "/clusters/" + url.PathEscape(rowCluster) + "/nodes/" + url.PathEscape(sc.Value)
			case "Status":
				sc.Kind = cellStatus
				sc.Tone = statusTone(cellClass(table, idx, cell))
			default:
				sc.Kind = cellPlain
			}
			sr.Cells = append(sr.Cells, sc)
		}
		v.Rows = append(v.Rows, sr)
	}
	return v
}

func (s *Server) relatedPods(r *http.Request, client *kube.Client, cluster *kube.Cluster, object *kube.Object, namespace string) *kube.Table {
	var labelSelector, fieldSelector string
	if object.Kind() == "Node" {
		fieldSelector = "spec.nodeName=" + object.Name()
	} else if labels := matchLabels(object.Raw); len(labels) > 0 {
		labelSelector = selectorString(labels)
	}
	if labelSelector == "" && fieldSelector == "" {
		return nil
	}
	podRT, err := client.FindResource(r.Context(), "pods", true, "")
	if err != nil {
		return nil
	}
	table, err := client.Table(r.Context(), &podRT, kube.ListOptions{Namespace: namespace, LabelSelector: labelSelector, FieldSelector: fieldSelector})
	if err != nil {
		return nil
	}
	table.Clusters = []string{cluster.Name}
	for i := range table.Rows {
		table.Rows[i].Cluster = cluster.Name
	}
	kube.GuessColumnClasses(&table)
	return &table
}

func (s *Server) events(r *http.Request, client *kube.Client, object *kube.Object, namespace string) []map[string]any {
	eventRT, err := client.FindResource(r.Context(), "events", true, "v1")
	if err != nil {
		return nil
	}
	ns := namespace
	if ns == "" {
		ns = object.Namespace()
	}
	fieldSelector := selectorString(map[string]string{
		"involvedObject.name":      object.Name(),
		"involvedObject.namespace": ns,
		"involvedObject.kind":      object.Kind(),
		"involvedObject.uid":       object.UID(),
	})
	list, err := client.List(r.Context(), &eventRT, kube.ListOptions{Namespace: ns, FieldSelector: fieldSelector})
	if err != nil {
		return nil
	}
	var events []map[string]any
	for _, item := range list.Items {
		events = append(events, item.Object)
	}
	return events
}

func (s *Server) podsForSelector(r *http.Request, client *kube.Client, object *kube.Object, namespace string) []kube.Object {
	labels := matchLabels(object.Raw)
	if len(labels) == 0 {
		return nil
	}
	podRT, err := client.FindResource(r.Context(), "pods", true, "")
	if err != nil {
		return nil
	}
	list, err := client.List(r.Context(), &podRT, kube.ListOptions{Namespace: namespace, LabelSelector: selectorString(labels)})
	if err != nil {
		return nil
	}
	var pods []kube.Object
	for i := range list.Items {
		pods = append(pods, kube.NewObject(&podRT, &list.Items[i]))
	}
	return pods
}

func (s *Server) objectLinks(cluster, namespace string, object *kube.Object) []config.Link {
	var links []config.Link
	for _, link := range s.cfg.ObjectLinks[object.Resource.Endpoint()] {
		links = append(links, expandLink(link, cluster, namespace, object.Name(), "", ""))
	}
	for label, value := range object.Labels() {
		for _, link := range s.cfg.LabelLinks[label] {
			links = append(links, expandLink(link, cluster, namespace, object.Name(), label, value))
		}
	}
	return links
}

func expandLink(link config.Link, cluster, namespace, name, label, labelValue string) config.Link {
	repl := strings.NewReplacer("{cluster}", cluster, "{namespace}", namespace, "{name}", name, "{label}", label, "{label_value}", labelValue, "{labelValue}", labelValue)
	link.Href = repl.Replace(link.Href)
	link.Title = repl.Replace(link.Title)
	return link
}
