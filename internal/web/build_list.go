package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/util/jsonpath"
)

// build_list.go is the data-assembly layer for the resource-list page: it turns
// the kube client + parsed request inputs into the plain listView. No HTML is
// emitted here; every request-derived href/flag is resolved into the view model.

// listContext is the intermediate data bundle: the merged tables plus the parsed
// request scope. buildListView turns it (together with the request URL) into the
// render-ready listView.
type listContext struct {
	Cluster         string
	Namespace       string
	Plural          string
	IsAllClusters   bool
	IsAllNamespaces bool
	ClusterCount    int
	Tables          []kube.Table
	Errors          []error
	Duration        time.Duration
}

func (lc *listContext) Title() string {
	return lc.Plural + " in " + lc.Cluster
}

func (s *Server) listContext(r *http.Request) (listContext, error) {
	start := s.clock()
	clusterName := r.PathValue("cluster")
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	if namespace != "" && namespace != kube.AllNamespaces && !s.namespaceAllowed(namespace) {
		return listContext{}, statusError{status: http.StatusForbidden, message: "namespace is not allowed"}
	}
	clusters, allClusters, err := s.manager.Select(clusterName)
	if err != nil {
		return listContext{}, err
	}
	isAllNamespaces := namespace == kube.AllNamespaces
	resourceTypes := strings.Split(plural, ",")
	if plural == "all" && namespace != "" {
		resourceTypes = []string{"pods", "services", "daemonsets", "deployments", "replicasets", "statefulsets", "horizontalpodautoscalers", "jobs", "cronjobs"}
	} else if plural == kube.AllNamespaces && namespace != "" {
		resourceTypes = s.unionNamespacedResourceTypes(r, clusters)
	}

	// Fan the per-cluster table assembly out concurrently, bounded by
	// SearchMaxConcurrency. Each cluster builds its own ordered tables + error
	// records into a per-cluster slot; expected per-(cluster,type) failures are
	// RESULT RECORDS (collected into the slot's errs), never errgroup errors --
	// a failing cluster must still render partial results with the partial
	// notice. Results are merged AFTER Wait in fixed cluster order (clusters is
	// name-sorted by manager.Select) regardless of completion order, replaying
	// the exact sequential first-seen MergeTables so the card/row order is
	// deterministic and byte-identical to the former sequential build.
	slots := make([]clusterTableResult, len(clusters))
	g, _ := errgroup.WithContext(r.Context())
	g.SetLimit(s.searchConcurrency())
	for i, cluster := range clusters {
		i, cluster := i, cluster
		g.Go(func() error {
			slots[i] = s.clusterTables(r, cluster, resourceTypes, namespace, isAllNamespaces)
			return nil
		})
	}
	_ = g.Wait()

	var tables []kube.Table
	var errs []error
	byPlural := map[string]int{}
	for _, slot := range slots {
		errs = append(errs, slot.errs...)
		for ti := range slot.tables {
			table := &slot.tables[ti]
			if idx, ok := byPlural[table.Resource.Plural]; ok {
				kube.MergeTables(&tables[idx], table)
			} else {
				byPlural[table.Resource.Plural] = len(tables)
				tables = append(tables, *table)
			}
		}
	}
	return listContext{Cluster: clusterName, Namespace: namespace, Plural: plural, IsAllClusters: allClusters, IsAllNamespaces: isAllNamespaces, ClusterCount: len(clusters), Tables: tables, Errors: errs, Duration: s.clock().Sub(start)}, nil
}

// clusterTableResult is one cluster's fan-out slot: its ordered tables (in
// resourceTypes iteration order) plus the per-(cluster,type) failures collected
// as error records. The caller merges slots across clusters in fixed cluster
// order.
type clusterTableResult struct {
	tables []kube.Table
	errs   []error
}

// clusterTables builds one cluster's ordered tables for the requested resource
// types (with per-type FindResource/Table failures collected as error records,
// not raised) so the per-cluster work can run as a single fan-out task.
func (s *Server) clusterTables(r *http.Request, cluster *kube.Cluster, resourceTypes []string, namespace string, isAllNamespaces bool) clusterTableResult {
	var tables []kube.Table
	var errs []error
	for _, typ := range resourceTypes {
		typ = strings.TrimSpace(typ)
		if typ == "" {
			continue
		}
		rt, err := s.kubeClient(r, cluster).FindResource(r.Context(), typ, namespace != "", apiVersionParam(r))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", cluster.Name, typ, err))
			continue
		}
		listNS := namespace
		if isAllNamespaces {
			listNS = ""
		}
		table, err := s.kubeClient(r, cluster).Table(r.Context(), &rt, kube.ListOptions{
			Namespace:     listNS,
			LabelSelector: r.URL.Query().Get("selector"),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", cluster.Name, typ, err))
			continue
		}
		table.Clusters = []string{cluster.Name}
		for i := range table.Rows {
			table.Rows[i].Cluster = cluster.Name
		}
		s.applyTableOptions(r, cluster, &table, namespace, isAllNamespaces)
		tables = append(tables, table)
	}
	return clusterTableResult{tables: tables, errs: errs}
}

// unionNamespacedResourceTypes resolves the sorted union of namespaced resource
// plurals across the clusters (the `_all`-namespaces case). The per-cluster
// discovery fans out concurrently; the result is a set then sort.Strings, so it
// is order-independent and deterministic regardless of completion order.
func (s *Server) unionNamespacedResourceTypes(r *http.Request, clusters []*kube.Cluster) []string {
	perCluster := make([][]string, len(clusters))
	g, _ := errgroup.WithContext(r.Context())
	g.SetLimit(s.searchConcurrency())
	for i, cluster := range clusters {
		i, cluster := i, cluster
		g.Go(func() error {
			types, _ := s.kubeClient(r, cluster).NamespacedResourceTypes(r.Context())
			plurals := make([]string, 0, len(types))
			for ti := range types {
				plurals = append(plurals, types[ti].Plural)
			}
			perCluster[i] = plurals
			return nil
		})
	}
	_ = g.Wait()
	set := map[string]bool{}
	for _, plurals := range perCluster {
		for _, plural := range plurals {
			set[plural] = true
		}
	}
	resourceTypes := make([]string, 0, len(set))
	for plural := range set {
		resourceTypes = append(resourceTypes, plural)
	}
	sort.Strings(resourceTypes)
	return resourceTypes
}

func (s *Server) applyTableOptions(r *http.Request, cluster *kube.Cluster, table *kube.Table, namespace string, allNamespaces bool) {
	q := r.URL.Query()
	hide := first(q.Get("hidecols"), q.Get("hide-columns"), s.cfg.DefaultHiddenColumns[table.Resource.Plural])
	kube.RemoveColumns(table, hide)
	labels := first(q.Get("labelcols"), q.Get("label-columns"), s.cfg.DefaultLabelColumns[table.Resource.Plural])
	kube.AddLabelColumns(table, labels)
	if table.Resource.Plural == "nodes" {
		// Nodes get rich capacity/pods/conditions columns (read from each row's
		// status). The CPU/Memory capacity columns are added here ONLY when metrics
		// are not joined; with ?join=metrics the joinMetrics CPU/Memory Usage columns
		// below carry the usage overlay instead (one CPU and one Memory column
		// either way, never both).
		decorateNodeColumns(table, q.Get("join") == "metrics")
	}
	if table.Resource.Plural == "deployments" {
		// Deployments get a synthetic Rollout column derived from each row's
		// status/spec (the server-side Table has no rollout column). The Ready column
		// the Table already provides becomes the rich replica track in buildCellView.
		decorateDeploymentColumns(table)
	}
	if table.Resource.Plural == "namespaces" {
		// Namespaces get a synthetic Labels column (chips read from metadata.labels);
		// the Status column the Table already provides reuses the status-dot cell. No
		// pods-count column is added -- a Namespace object has no pod count and readout
		// has no per-namespace pod-count seam.
		decorateNamespaceColumns(table)
	}
	if q.Get("join") == "metrics" && (table.Resource.Plural == "pods" || table.Resource.Plural == "nodes") {
		s.joinMetrics(r.Context(), s.kubeClient(r, cluster), table, namespace, allNamespaces, q.Get("selector"))
	}
	custom := first(q.Get("customcols"), q.Get("custom-columns"), s.cfg.DefaultCustomColumns[table.Resource.Plural])
	if custom != "" {
		s.joinCustomColumns(r.Context(), s.kubeClient(r, cluster), table, namespace, allNamespaces, custom, q)
	}
	kube.FilterRowsByNamespace(table, s.cfg.IncludeNamespaces, s.cfg.ExcludeNamespaces)
	kube.FilterTable(table, q.Get("filter"), false)
	kube.GuessColumnClasses(table)
	kube.SortTable(table, q.Get("sort"))
	if limit := q.Get("limit"); limit != "" {
		n, _ := strconv.Atoi(limit)
		if n >= 0 && n < len(table.Rows) {
			table.Rows = table.Rows[:n]
		}
	}
}

func (s *Server) joinMetrics(ctx context.Context, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool, labelSelector string) {
	table.Columns = append(table.Columns, kube.Column{Name: "CPU Usage"}, kube.Column{Name: "Memory Usage"})
	metricsKind := "NodeMetrics"
	if table.Resource.Namespaced {
		metricsKind = "PodMetrics"
	}
	rt, err := client.FindResourceByKind(ctx, "metrics.k8s.io/v1beta1", metricsKind, table.Resource.Namespaced)
	if err != nil {
		for i := range table.Rows {
			table.Rows[i].Cells = append(table.Rows[i].Cells, 0, 0)
		}
		return
	}
	listNS := namespace
	if allNamespaces {
		listNS = ""
	}
	list, err := client.List(ctx, &rt, kube.ListOptions{Namespace: listNS, LabelSelector: labelSelector})
	if err != nil {
		// The CPU/Memory columns were appended up front; if the metrics LIST call
		// fails AFTER discovery succeeded, every row must still get its two
		// placeholder cells, or the rows are short two cells and the table renders
		// ragged (column count > row cell count). Append the "metrics unknown" zero
		// placeholders so column/row cell counts always match. Never blank or crash.
		for i := range table.Rows {
			table.Rows[i].Cells = append(table.Rows[i].Cells, 0, 0)
		}
		return
	}
	usage := map[string][2]float64{}
	for _, item := range list.Items {
		// kube.MetricsUsage decodes the metrics item (Pod or Node) typed and
		// sums its cpu (cores) / memory (bytes) via resource.Quantity — the seam
		// that replaced the hand-rolled quantity parser.
		key, cpu, mem := kube.MetricsUsage(item.Object)
		usage[key] = [2]float64{cpu, mem}
	}
	for i := range table.Rows {
		key := nestedString(table.Rows[i].Object, "metadata", "namespace") + "/" + nestedString(table.Rows[i].Object, "metadata", "name")
		value := usage[key]
		table.Rows[i].Cells = append(table.Rows[i].Cells, value[0], value[1])
	}
}

// decorateNodeColumns appends the rich node columns (Pods + Conditions always; the
// CPU/Memory capacity columns only when metrics are NOT joined) and fills each
// row's matching cells from the node's status. The capacity/conditions cells carry
// a plain DISPLAY value (capacity quantity / abnormal-condition names / "—") so
// sort, TSV, and the generic fallback stay sensible; the rich renderers re-read the
// row object for the bar fill + pills. Cells are appended for EVERY row in lockstep
// with the columns, so the table never goes ragged.
func decorateNodeColumns(table *kube.Table, metricsJoined bool) {
	type nodeCol struct {
		name string
		cell func(obj map[string]any) any
	}
	var cols []nodeCol
	if !metricsJoined {
		cols = append(cols,
			nodeCol{"CPU", func(obj map[string]any) any { return nodeCapacityText(obj, "cpu") }},
			nodeCol{"Memory", func(obj map[string]any) any { return nodeCapacityText(obj, "memory") }},
		)
	}
	cols = append(cols,
		nodeCol{"Pods", func(obj map[string]any) any { return nestedString(obj, "status", "capacity", "pods") }},
		nodeCol{"Conditions", func(obj map[string]any) any { return nodeConditionsText(obj) }},
	)
	for _, col := range cols {
		if columnIndex(table.Columns, col.name) >= 0 {
			continue // never duplicate a column the server-side Table already had
		}
		table.Columns = append(table.Columns, kube.Column{Name: col.name})
		for i := range table.Rows {
			table.Rows[i].Cells = append(table.Rows[i].Cells, col.cell(table.Rows[i].Object))
		}
	}
}

// decorateDeploymentColumns appends the synthetic Rollout column and fills each
// row's cell with the plain rollout DISPLAY label (derived from the row's
// status/spec) so sort, TSV, and the generic fallback see a sensible value; the
// rich cellRollout renderer re-reads the row object for the `.rollout.<state>`
// class + icon. The cell is appended for EVERY row in lockstep with the column, so
// the table never goes ragged. A Rollout column the server-side Table already
// provided (it does not today) is never duplicated.
func decorateDeploymentColumns(table *kube.Table) {
	if columnIndex(table.Columns, "Rollout") >= 0 {
		return
	}
	table.Columns = append(table.Columns, kube.Column{Name: "Rollout"})
	for i := range table.Rows {
		_, label := rolloutState(table.Rows[i].Object)
		table.Rows[i].Cells = append(table.Rows[i].Cells, label)
	}
}

// decorateNamespaceColumns appends the synthetic Labels column and fills each
// row's cell with the plain label DISPLAY value (the sorted comma-joined labels,
// or "—" when unlabeled) so sort, TSV, and the generic fallback see a sensible
// value; the rich cellChips renderer re-reads the row object for the per-label
// chips (the .app accent for app.kubernetes.io/*). A Namespace object carries NO
// pod count and readout has no per-namespace pod-count seam, so NO pods-count
// column is fabricated -- only status (already from the Table), labels, and age.
// The cell is appended for EVERY row in lockstep with the column, so the table
// never goes ragged. A "Labels" column that already exists (e.g. a user's
// labelcols=* produced one) is never duplicated -- the decoration is a no-op then,
// and that user column keeps falling through to the generic cell.
func decorateNamespaceColumns(table *kube.Table) {
	if columnIndex(table.Columns, "Labels") >= 0 {
		return
	}
	table.Columns = append(table.Columns, kube.Column{Name: "Labels"})
	for i := range table.Rows {
		table.Rows[i].Cells = append(table.Rows[i].Cells, namespaceLabelsText(table.Rows[i].Object))
	}
}

// columnIndex reports the index of a named column, or -1. A small local mirror of
// kube.columnIndex (unexported) used by decorateNodeColumns to skip a column the
// server-side Table already provided.
func columnIndex(cols []kube.Column, name string) int {
	for i, col := range cols {
		if col.Name == name {
			return i
		}
	}
	return -1
}

// nodeCapacityText is the plain capacity DISPLAY value for the synthetic CPU/Memory
// columns (the canonical quantity, e.g. "4" / "16Gi"); empty when absent.
func nodeCapacityText(obj map[string]any, key string) string {
	if q, ok := nodeCapacityQuantity(obj, key); ok {
		return q.String()
	}
	return ""
}

// nodeConditionsText is the plain DISPLAY value for the synthetic Conditions column:
// the comma-joined abnormal-condition names, or "—" when the node is clean (so the
// generic fallback / TSV / sort sees the same "—" the rich pill renderer shows).
func nodeConditionsText(obj map[string]any) string {
	pills := nodeAbnormalConditions(obj)
	if len(pills) == 0 {
		return "—"
	}
	names := make([]string, len(pills))
	for i, p := range pills {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func (s *Server) joinCustomColumns(ctx context.Context, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool, spec string, query url.Values) {
	parts := strings.Split(spec, ";")
	type compiled struct {
		name string
		expr *jsonpath.JSONPath
	}
	var expressions []compiled
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, expr, ok := strings.Cut(part, "=")
		if !ok {
			expr = name
			name = humanTitle(expr)
		}
		jp := jsonpath.New(name).AllowMissingKeys(true)
		if err := jp.Parse(relaxJSONPath(expr)); err != nil {
			continue
		}
		expressions = append(expressions, compiled{name: name, expr: jp})
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	if len(expressions) == 0 {
		return
	}
	var nodes map[string]map[string]any
	if query.Get("join") == "nodes" && table.Resource.Kind == "Pod" {
		if nodeRT, err := client.FindResource(ctx, "nodes", false, ""); err == nil {
			if nodeList, err := client.List(ctx, &nodeRT, kube.ListOptions{}); err == nil {
				nodes = map[string]map[string]any{}
				for _, item := range nodeList.Items {
					nodes[nestedString(item.Object, "metadata", "name")] = item.Object
				}
			}
		}
	}
	listNS := namespace
	if allNamespaces {
		listNS = ""
	}
	list, err := client.List(ctx, &table.Resource, kube.ListOptions{Namespace: listNS, LabelSelector: query.Get("selector")})
	if err != nil {
		for i := range table.Rows {
			for range expressions {
				table.Rows[i].Cells = append(table.Rows[i].Cells, nil)
			}
		}
		return
	}
	objects := map[string]map[string]any{}
	for _, item := range list.Items {
		key := nestedString(item.Object, "metadata", "namespace") + "/" + nestedString(item.Object, "metadata", "name")
		objects[key] = item.Object
	}
	joined := map[int]bool{}
	for i := range table.Rows {
		key := nestedString(table.Rows[i].Object, "metadata", "namespace") + "/" + nestedString(table.Rows[i].Object, "metadata", "name")
		obj := objects[key]
		if obj == nil {
			continue
		}
		for _, expr := range expressions {
			if table.Resource.Kind == "Secret" {
				table.Rows[i].Cells = append(table.Rows[i].Cells, kube.SecretContentHidden)
				continue
			}
			searchObj := obj
			if nodes != nil {
				searchObj = map[string]any{}
				for k, v := range obj {
					searchObj[k] = v
				}
				searchObj["node"] = nodes[nestedString(obj, "spec", "nodeName")]
			}
			table.Rows[i].Cells = append(table.Rows[i].Cells, evalJSONPath(expr.expr, searchObj))
		}
		joined[i] = true
	}
	for i := range table.Rows {
		if joined[i] {
			continue
		}
		for range expressions {
			table.Rows[i].Cells = append(table.Rows[i].Cells, nil)
		}
	}
}

// relaxJSONPath makes a custom-column expression kubectl-ergonomic. The raw
// jsonpath engine is a {...}-delimited template and does not auto-wrap a bare
// path, so a bare `spec.containers[*].image` is wrapped as
// `{.spec.containers[*].image}`. An expression that already contains a `{` is
// taken as an explicit template and passed through unchanged. (This follows the
// idea of kubectl's RelaxedJSONPathExpression, which lives in k8s.io/kubectl and
// is not a dependency here.)
func relaxJSONPath(expr string) string {
	if strings.Contains(expr, "{") {
		return expr
	}
	// Strip an optional leading "." like kubectl's RelaxedJSONPathExpression
	// does. Without this, a leading-dot expression (e.g. ".metadata.name")
	// becomes "{..metadata.name}", and the engine reads the leading ".." as the
	// recursive-descent operator — which matches same-named keys nested anywhere
	// in the object and leaks ghost values into the cell.
	return "{." + strings.TrimPrefix(expr, ".") + "}"
}

// evalJSONPath runs a parsed custom-column template against one object and
// returns the cell value as a string. Multiple matches (e.g. every container
// image) render space-joined ("nginx:1 redis:7"), matching `kubectl get -o
// jsonpath`; a missing path yields "" because the engine is built with
// AllowMissingKeys(true). Any execution error degrades to "" so a single bad
// row never aborts the join.
func evalJSONPath(jp *jsonpath.JSONPath, obj any) string {
	var buf bytes.Buffer
	if err := jp.Execute(&buf, obj); err != nil {
		return ""
	}
	return buf.String()
}

// buildListView turns a listContext + the request URL into the render-ready
// listView, resolving every request-derived href and flag here so render never
// touches *http.Request.
func (s *Server) buildListView(r *http.Request, lc *listContext) listView {
	q := r.URL.Query()
	sortValue := q.Get("sort")
	joinValue := q.Get("join")

	v := listView{
		Cluster:         lc.Cluster,
		Namespace:       lc.Namespace,
		Plural:          lc.Plural,
		IsAllClusters:   lc.IsAllClusters,
		IsAllNamespaces: lc.IsAllNamespaces,
		ClusterCount:    lc.ClusterCount,
		Duration:        lc.Duration,
		Errors:          lc.Errors,
	}
	if lc.Namespace != "" && lc.Plural != "namespaces" && !lc.IsAllNamespaces {
		v.AllNamespacesHref = fmt.Sprintf("/clusters/%s/namespaces/_all/%s?%s", url.PathEscape(lc.Cluster), url.PathEscape(lc.Plural), r.URL.RawQuery)
	}
	// The client-side stale path (D11) needs its markup hooks in the first server
	// response: a hidden `.ro-banner.warn` readout.js reveals on an auto-refresh
	// error, dimming the rows in #resource-list-content (the morph target) instead
	// of blanking them. The server never decides "stale" -- there is no last-good
	// cache; the client does, on a refresh error that keeps the rows.
	v.StaleBanner = true
	v.StaleDimTarget = "resource-list-content"

	// Active filters drive the empty-FILTERED state (removable chips + Clear) vs
	// the plainly-empty state (broad action). Resolved once for the page (the
	// filter/selector params are page-wide, not per-table).
	filterChips := buildFilterChips(r)
	clearHref := ""
	if len(filterChips) > 0 {
		clearHref = delQuery(r.URL, "filter", "selector", "labelcols", "label-columns")
	}

	for ti := range lc.Tables {
		table := &lc.Tables[ti]
		tv := tableView{
			Table:           *table,
			Kind:            pluralizeKind(table.Resource.Kind),
			DownloadTSVHref: addQuery(resourceListBaseURL(r.URL), "download", "tsv"),
			SearchHref:      fmt.Sprintf("/search?cluster=%s&namespace=%s&type=%s", url.QueryEscape(strings.Join(table.Clusters, ",")), url.QueryEscape(lc.Namespace), url.QueryEscape(table.Resource.Plural)),
			Tools:           s.buildToolsView(r, table),
			Phase:           kube.PhaseSummary(table),
		}
		if (table.Resource.Plural == "pods" || table.Resource.Plural == "nodes") && joinValue == "" {
			tv.ShowMetricsHref = addQuery(r.URL, "join", "metrics")
		}
		for _, col := range table.Columns {
			sortParam := col.Name
			if sortValue == col.Name {
				sortParam = col.Name + ":desc"
			}
			tv.Columns = append(tv.Columns, columnView{
				SortHref: addQuery(r.URL, "sort", sortParam),
				SortIcon: sortIcon(sortValue, col.Name),
			})
		}
		tv.CreatedHref = addQuery(r.URL, "sort", createdSortParam(sortValue))
		tv.CreatedIcon = sortIcon(sortValue, "Created")

		multiCluster := len(table.Clusters) > 1
		for _, row := range table.Rows {
			ns := nestedString(row.Object, "metadata", "namespace")
			name := cellString(row, nameColumn(table))
			rv := rowView{
				StatusClass:  kube.RowStatusClass(table, row),
				Cluster:      row.Cluster,
				Namespace:    ns,
				CreatedClass: s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")),
				CreatedText:  formatTimestamp(nestedString(row.Object, "metadata", "creationTimestamp")),
			}
			if multiCluster {
				rv.ClusterHref = "/clusters/" + url.PathEscape(row.Cluster)
			}
			if lc.IsAllNamespaces {
				rv.NsHref = fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(row.Cluster), url.PathEscape(ns))
			}
			for i, cell := range row.Cells {
				rv.Cells = append(rv.Cells, s.buildCellView(r, table, row, i, cell, ns, name))
			}
			tv.Rows = append(tv.Rows, rv)
		}
		// Empty-state enrichment: a zero-row table either offers a broad next
		// action (plainly empty) or, when an active filter is what hid the rows,
		// the removable filter chips + Clear (empty-filtered). Both are wired only
		// when the table is actually empty so a populated table is untouched.
		if len(tv.Rows) == 0 {
			if len(filterChips) > 0 {
				tv.EmptyFilters = filterChips
				tv.ClearHref = clearHref
			} else if v.AllNamespacesHref != "" {
				tv.EmptyAction = &emptyActionView{Href: v.AllNamespacesHref, Label: "Show " + lc.Plural + " across all namespaces"}
			}
		}
		v.Tables = append(v.Tables, tv)
	}
	// Whole-list failure state (D11): a SINGLE-cluster list that produced no
	// tables at all but did collect a FORBIDDEN or UNREACHABLE error renders that
	// state in place of the table -- and its per-cluster error is NOT surfaced as
	// the all-cluster partial-failure banner (the invariant: a single-cluster list
	// never says some-clusters-failed). An all-cluster list, a single-cluster list
	// that still produced a table (a partial multi-type list), or a single-cluster
	// failure that is neither forbidden nor unreachable (e.g. a missing resource
	// type -- the secret-barrier path, which must keep surfacing "resource type
	// not found") keeps the existing behaviour.
	if !lc.IsAllClusters && lc.ClusterCount == 1 && len(v.Tables) == 0 && len(lc.Errors) > 0 {
		if state := s.buildListState(r, lc); state != nil {
			v.State = state
			v.Errors = nil
		}
	}
	return v
}

// buildFilterChips resolves the removable active-filter chips for the
// empty-filtered state from the request: the free-text filter, the label
// selector, and the labelcols column spec. Each chip's ✕ drops just that one
// param (a read-only GET) so the user can peel filters off one at a time.
func buildFilterChips(r *http.Request) []filterChipView {
	q := r.URL.Query()
	var chips []filterChipView
	if filter := q.Get("filter"); filter != "" {
		chips = append(chips, filterChipView{Label: "filter: " + filter, RemoveHref: delQuery(r.URL, "filter")})
	}
	if selector := q.Get("selector"); selector != "" {
		chips = append(chips, filterChipView{Label: "selector: " + selector, RemoveHref: delQuery(r.URL, "selector")})
	}
	if labelCols := first(q.Get("labelcols"), q.Get("label-columns")); labelCols != "" {
		chips = append(chips, filterChipView{Label: "labels: " + labelCols, RemoveHref: delQuery(r.URL, "labelcols", "label-columns")})
	}
	return chips
}

// buildListState classifies a single-cluster whole-list failure into the
// forbidden state (an apiserver 403 naming the verb/resource/namespace) or the
// unreachable state (a transport/dial failure that never reached the apiserver
// -- shown with its REAL error string, never a cute message, Principles §11). It
// returns nil for any other failure (a missing resource type, a 5xx with a
// Status), so those keep the existing partial-error banner. The retry is the
// same list URL (a read-only GET); Back to clusters is /clusters.
func (s *Server) buildListState(r *http.Request, lc *listContext) *listStateView {
	err := lc.Errors[0]
	forbidden := kube.IsForbidden(err)
	unreachable := !forbidden && !kube.IsNotFound(err) && !kube.IsAPIStatusError(err)
	if !forbidden && !unreachable {
		return nil
	}
	state := &listStateView{
		Verb:      "list",
		Resource:  lc.Plural,
		Namespace: lc.Namespace,
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
	return state
}

// buildCellView resolves one body cell: its render branch, value, classes, and
// any request-derived href. The rich per-kind presentation (pod-name split,
// status-dot tone + transient pulse, ready/restart tones, secondary-text
// truncation tooltip) is resolved here too so the templ renderer reads plain data
// and emits the redesign vocabulary directly. Recognized columns of the existing
// k8s Table schema are ADAPTED in place -- a user-added label/custom column falls
// through to the generic (plain/truncated) cell, so hidecols/labelcols/customcols/
// sort/TSV are untouched.
func (s *Server) buildCellView(r *http.Request, table *kube.Table, row kube.Row, i int, cell any, ns, name string) cellView {
	value := cellDisplayString(cell)
	colName := table.Columns[i].Name
	cls := cellClass(table, i, cell)
	if colName == "Age" || colName == "First Seen" {
		cls = strings.TrimSpace(cls + " " + s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")))
	}
	cv := cellView{Value: value, Class: cls, ColClass: table.Columns[i].Class}
	switch {
	case colName == "Name":
		cv.Kind = cellName
		cv.NameHead, cv.NameTail = splitObjectName(table.Resource.Plural, value)
		href := resourceHref(row.Cluster, &table.Resource, ns, name)
		if table.Resource.Plural == "namespaces" {
			href = fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(row.Cluster), url.PathEscape(name))
		}
		cv.Href = href
	case table.Columns[i].Label != "" && table.Columns[i].Label != "*":
		// A user-added single label column: still a selector link, but the label
		// VALUE is secondary free-text, so it truncates with a tooltip.
		cv.Kind = cellLabel
		cv.Href = addQuery(r.URL, "selector", table.Columns[i].Label+"="+value)
		cv.Trunc, cv.Title = true, value
	case colName == "Node":
		cv.Kind = cellNode
		cv.Href = "/clusters/" + url.PathEscape(row.Cluster) + "/nodes/" + url.PathEscape(value)
	case table.Resource.Plural == "nodes" && (colName == "CPU Usage" || colName == "Memory Usage"):
		// Nodes reskin the joined metrics usage column as a capacity bar: the cell
		// carries the usage (cores/bytes from joinMetrics), the node's capacity
		// comes from status.capacity, and the bucket + fill come from usage/capacity.
		usage, haveUsage := numericCell(cell)
		cv = capacityCellView(row.Object, nodeCapacityKey(colName), usage, haveUsage)
	case table.Resource.Plural == "nodes" && (colName == "CPU" || colName == "Memory"):
		// No-metrics node capacity column: capacity value text, no usage overlay.
		cv = capacityCellView(row.Object, nodeCapacityKey(colName), 0, false)
	case table.Resource.Plural == "nodes" && colName == "Roles":
		cv.Kind = cellRoles
		cv.Roles = nodeRoles(row.Object)
	case table.Resource.Plural == "nodes" && colName == "Conditions":
		cv.Kind = cellConditions
		cv.Conds = nodeAbnormalConditions(row.Object)
	case table.Resource.Plural == "deployments" && colName == "Ready":
		// Deployments reskin the Ready column as the replica track: the segment
		// states + the ready/desired ratio come from the deployment status/spec
		// (readyReplicas / updatedReplicas / spec.replicas), capped at
		// replicaTrackCap so a high-replica deployment never explodes the DOM.
		cv.Kind = cellReplicas
		desired, ready, updated := deploymentReplicas(row.Object)
		cv.RepSegments, cv.RepNum = replicaTrack(desired, ready, updated)
		cv.Ratio = readyRatioClass(cv.RepNum)
	case table.Resource.Plural == "deployments" && colName == "Rollout":
		// The synthetic Rollout column (added by decorateDeploymentColumns) renders
		// the rollout status pill; the state + label come from the deployment
		// status/conditions/spec.paused.
		cv.Kind = cellRollout
		cv.RolloutState, cv.Value = rolloutState(row.Object)
	case table.Resource.Plural == "namespaces" && colName == "Labels" && table.Columns[i].Label == "":
		// The synthetic Labels column (added by decorateNamespaceColumns) renders the
		// namespace label chips read from metadata.labels (the .app accent for
		// app.kubernetes.io/*). The Label=="" guard keeps a user-added labelcols
		// "Labels" column (which carries a Label tag) on the generic path instead.
		cv.Kind = cellChips
		cv.Chips = namespaceLabelChips(row.Object)
	case colName == "CPU Usage":
		cv.Kind = cellCPU
		cv.Value = cpuFormat(cell)
	case colName == "Memory Usage":
		cv.Kind = cellMemory
		cv.Value = memoryMiBFormat(cell)
	case colName == "Status":
		cv.Kind = cellStatus
		cv.Tone = statusTone(cls)
		if table.Resource.Plural == "pods" {
			cv.Pulse = transientPodPhase(value)
		}
	case colName == "Ready" && strings.Contains(value, "/"):
		cv.Kind = cellReady
		cv.Ratio = readyRatioClass(value)
	case colName == "Restarts":
		cv.Kind = cellRestarts
		cv.Value, cv.Ago = splitRestarts(value)
		cv.Tone = restartsTone(cv.Value)
	default:
		cv.Kind = cellPlain
		if isSecondaryTextColumn(colName) {
			cv.Trunc, cv.Title = true, value
		}
	}
	if colName == "Age" || colName == "First Seen" {
		// The age cell carries the short bucketed value; the full timestamp moves
		// into the tooltip (no redundant full-timestamp column).
		if ts := formatTimestamp(nestedString(row.Object, "metadata", "creationTimestamp")); ts != "" {
			cv.Title = "created " + ts
		}
	}
	return cv
}

// secondaryTextColumns are the recognized k8s Table columns whose value is
// free-text rather than an identifier -- they truncate with a tooltip. Identifier
// columns (Name, Node, IP, Namespace, Ports, container names, counts) are never
// listed here, so they fall through to a plain never-truncated cell.
var secondaryTextColumns = map[string]bool{
	"Image":     true,
	"Images":    true,
	"Selector":  true,
	"Labels":    true,
	"Message":   true,
	"Reason":    true,
	"Data":      true,
	"Provider":  true,
	"Resources": true,
}

func isSecondaryTextColumn(colName string) bool {
	return secondaryTextColumns[colName]
}

// buildToolsView resolves the resource-list tools form state from the request.
func (s *Server) buildToolsView(r *http.Request, table *kube.Table) toolsView {
	q := r.URL.Query()
	active := q.Get("labelcols") != "" || q.Get("selector") != "" || q.Get("filter") != ""
	if !active {
		active = q.Get("label-columns") != "" || q.Get("custom-columns") != "" || q.Get("hide-columns") != ""
	}
	tv := toolsView{
		Active:       active,
		LabelColsVal: first(q.Get("labelcols"), q.Get("label-columns"), s.cfg.DefaultLabelColumns[table.Resource.Plural]),
		SelectorVal:  q.Get("selector"),
		FilterVal:    q.Get("filter"),
	}
	for _, key := range []string{"join", "sort", "customcols", "hidecols"} {
		if value := q.Get(key); value != "" {
			tv.HiddenInputs = append(tv.HiddenInputs, hiddenInput{Name: key, Value: value})
		}
	}
	return tv
}
