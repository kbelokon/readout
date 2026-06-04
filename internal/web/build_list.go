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
		v.Tables = append(v.Tables, tv)
	}
	return v
}

// buildCellView resolves one body cell: its render branch, value, classes, and
// any request-derived href.
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
		href := resourceHref(row.Cluster, &table.Resource, ns, name)
		if table.Resource.Plural == "namespaces" {
			href = fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(row.Cluster), url.PathEscape(name))
		}
		cv.Href = href
	case table.Columns[i].Label != "" && table.Columns[i].Label != "*":
		cv.Kind = cellLabel
		cv.Href = addQuery(r.URL, "selector", table.Columns[i].Label+"="+value)
	case colName == "Node":
		cv.Kind = cellNode
		cv.Href = "/clusters/" + url.PathEscape(row.Cluster) + "/nodes/" + url.PathEscape(value)
	case colName == "CPU Usage":
		cv.Kind = cellCPU
		cv.Value = cpuFormat(cell)
	case colName == "Memory Usage":
		cv.Kind = cellMemory
		cv.Value = memoryMiBFormat(cell)
	case colName == "Status":
		cv.Kind = cellStatus
	case colName == "Ready" && strings.Contains(value, "/"):
		cv.Kind = cellReady
	default:
		cv.Kind = cellPlain
	}
	return cv
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
