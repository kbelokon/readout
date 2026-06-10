package web

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/yamlview"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

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
	if v.DefaultTab {
		v.Labels = buildLabelChips(cluster.Name, renderNamespace, &object)
		v.Annotations = buildAnnotationChips(&object)
		if object.Kind() == "Node" {
			v.Node = buildNodeSummaryView(&object)
		}
		v.Secret = secretView
		v.YAMLCards = s.buildYAMLCards(cluster.Name, renderNamespace, &object)
	}
	return v, true
}

// detailState classifies a detail-page fetch failure into the forbidden state
// (a 403 naming the verb/resource/namespace) or the unreachable state (a
// transport/dial failure shown with its REAL error string), returning a
// state-only detailView the handler renders at 200. It returns nil for any other
// failure (a NotFound object -> a real 404, a 5xx with a Status, the policy 403),
// so the caller falls through to s.error and the existing status-code page. The
// breadcrumb is built from the request path (cluster/namespace/plural/name) so no
// fetched object is needed -- the fetch is exactly what failed.
func (s *Server) detailState(r *http.Request, cluster *kube.Cluster, plural, name, namespace, verb string, err error) *detailView {
	forbidden := kube.IsForbidden(err)
	unreachable := !forbidden && !kube.IsNotFound(err) && !kube.IsAPIStatusError(err)
	if !forbidden && !unreachable {
		return nil
	}
	state := &detailStateView{
		Verb:      verb,
		Resource:  plural,
		Name:      name,
		Namespace: namespace,
		RetryHref: r.URL.String(),
		BackHref:  "/clusters",
	}
	if forbidden {
		state.Kind = stateForbidden
		state.Detail = "403 Forbidden · " + err.Error()
	} else {
		state.Kind = stateUnreachable
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
		// The selector value is emitted literally (key=value), matching the former
		// renderObjectSummary which only html-escaped it -- it did NOT url-encode
		// the '=' or '/'. templ's attribute escaping reproduces that html-escape.
		selector := key + "=" + val
		var href string
		if namespace != "" {
			href = fmt.Sprintf("/clusters/%s/namespaces/%s/%s?selector=%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(object.Resource.Endpoint()), selector)
		} else {
			href = fmt.Sprintf("/clusters/%s/%s?selector=%s", url.PathEscape(cluster), url.PathEscape(object.Resource.Endpoint()), selector)
		}
		out = append(out, labelChipView{
			Href: href,
			Key:  key,
			Val:  val,
		})
	}
	return out
}

// buildAnnotationChips resolves the annotation chips (sorted keys). Val is the
// value truncated to 40 for the clipped chip body; Full is the complete
// "key: value" string for the title= tooltip, so the chip can clip its body
// while the tooltip still shows the full untruncated value.
func buildAnnotationChips(object *kube.Object) []annotationChipView {
	annotations := object.Annotations()
	if len(annotations) == 0 {
		return nil
	}
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]annotationChipView, 0, len(keys))
	for _, key := range keys {
		out = append(out, annotationChipView{
			Key:  key,
			Val:  truncate(annotations[key], 40),
			Full: key + ": " + annotations[key],
		})
	}
	return out
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
// title, and the highlighted-YAML content.
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
			Name:    key,
			Title:   capitalizeWord(key),
			Content: s.highlightYAML(cluster, namespace, object, key+"-", string(data)),
		})
	}
	return out
}

// eventItem is the local typed view of an event object. The core/v1 `events`
// endpoint dual-writes BOTH the old core/v1 Event shape and the newer
// events.k8s.io/v1 shape into the same list, so this one struct carries both
// spellings and buildEventViews normalizes between them — fixing the "Unknown"
// the new-style fields used to render. Decoded once via FromUnstructured at the
// buildEventViews seam (a fixed, known kind).
type eventItem struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	// Age source, in precedence order: core/v1 lastTimestamp, then
	// events.k8s.io eventTime, then the series last-observed time.
	LastTimestamp string `json:"lastTimestamp"`
	EventTime     string `json:"eventTime"`
	Series        struct {
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
}

func (e *eventItem) timestamp() string {
	return first(e.LastTimestamp, e.EventTime, e.Series.LastObservedTime)
}

func (e *eventItem) message() string {
	return first(e.Message, e.Note)
}

func (e *eventItem) from() string {
	return first(e.Source.Component, e.ReportingController)
}

// buildEventViews flattens raw event objects into render-ready event rows. The
// Type cell tone is the redesign status tone mapped from kube.CellClass("events",
// "Type", <value>) via statusTone, then defaulted to "mute" for a Normal event
// (which carries no kube class) so the redesign dot still reads grey; the Age
// cell shows the resolved timestamp (formatTimestamp: 'T'->' ', strip trailing
// 'Z'; "Unknown" when empty) classed by ageClass (a 1-day window); From is the
// resolved reporting component. The Message cell's static ro-event-msg class is
// emitted in the templ. Each event is decoded into eventItem so both the core/v1
// and events.k8s.io/v1 spellings normalize to one row.
func (s *Server) buildEventViews(events []map[string]any) []eventView {
	out := make([]eventView, 0, len(events))
	for _, raw := range events {
		var event eventItem
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &event); err != nil {
			continue
		}
		timestamp := event.timestamp()
		age := "Unknown"
		if timestamp != "" {
			age = formatTimestamp(timestamp)
		}
		tone := statusTone(kube.CellClass("events", "Type", event.Type))
		if tone == "" {
			tone = "mute"
		}
		out = append(out, eventView{
			Type:     event.Type,
			Tone:     tone,
			Reason:   event.Reason,
			Age:      age,
			AgeClass: s.ageClass(timestamp),
			From:     event.from(),
			Message:  event.message(),
		})
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
