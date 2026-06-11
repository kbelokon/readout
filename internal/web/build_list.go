package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	Clients         requestKubeClients

	// ColVis maps each table's plural to its column-visibility universe (D8):
	// every column of the fully-decorated table plus the synthetic Created, with
	// hidden/identity flags -- captured at the removal point in applyTableOptions
	// (the removed columns are gone from Tables, so the popover needs this to
	// re-offer them) and merged across clusters like MergeTables merges columns.
	ColVis map[string][]columnVis
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
	clients := s.kubeClients(r, clusters)
	isAllNamespaces := namespace == kube.AllNamespaces
	resourceTypes := strings.Split(plural, ",")
	if plural == "all" && namespace != "" {
		resourceTypes = []string{"pods", "services", "daemonsets", "deployments", "replicasets", "statefulsets", "horizontalpodautoscalers", "jobs", "cronjobs"}
	} else if plural == kube.AllNamespaces && namespace != "" {
		resourceTypes = s.unionNamespacedResourceTypes(r, clusters, clients)
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
			slots[i] = s.clusterTables(r, clients[cluster.Name], cluster, resourceTypes, namespace, isAllNamespaces)
			return nil
		})
	}
	_ = g.Wait()

	var tables []kube.Table
	var errs []error
	byPlural := map[string]int{}
	colVis := map[string][]columnVis{}
	for _, slot := range slots {
		errs = append(errs, slot.errs...)
		for plural, vis := range slot.colVis {
			colVis[plural] = mergeColumnVis(colVis[plural], vis)
		}
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
	return listContext{Cluster: clusterName, Namespace: namespace, Plural: plural, IsAllClusters: allClusters, IsAllNamespaces: isAllNamespaces, ClusterCount: len(clusters), Tables: tables, Errors: errs, Duration: s.clock().Sub(start), Clients: clients, ColVis: colVis}, nil
}

// clusterTableResult is one cluster's fan-out slot: its ordered tables (in
// resourceTypes iteration order), the per-plural column-visibility universes
// captured by applyTableOptions, plus the per-(cluster,type) failures collected
// as error records. The caller merges slots across clusters in fixed cluster
// order.
type clusterTableResult struct {
	tables []kube.Table
	colVis map[string][]columnVis
	errs   []error
}

// clusterTables builds one cluster's ordered tables for the requested resource
// types (with per-type FindResource/Table failures collected as error records,
// not raised) so the per-cluster work can run as a single fan-out task.
func (s *Server) clusterTables(r *http.Request, client *kube.Client, cluster *kube.Cluster, resourceTypes []string, namespace string, isAllNamespaces bool) clusterTableResult {
	var tables []kube.Table
	var errs []error
	colVis := map[string][]columnVis{}
	for _, typ := range resourceTypes {
		typ = strings.TrimSpace(typ)
		if typ == "" {
			continue
		}
		rt, err := client.FindResource(r.Context(), typ, namespace != "", apiVersionParam(r))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", cluster.Name, typ, err))
			continue
		}
		listNS := namespace
		if isAllNamespaces {
			listNS = ""
		}
		table, err := client.Table(r.Context(), &rt, kube.ListOptions{
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
		vis := s.applyTableOptions(r, client, &table, namespace, isAllNamespaces)
		colVis[table.Resource.Plural] = mergeColumnVis(colVis[table.Resource.Plural], vis)
		tables = append(tables, table)
	}
	return clusterTableResult{tables: tables, colVis: colVis, errs: errs}
}

// streamListContext wraps ONE cluster's pristine snapshot table into the same
// listContext shape the `_table` partial renders from (D19): cluster tags,
// then the full applyTableOptions pass — decorations, hidecols, the legacy
// filter params AND the `?f=` chips, sort — exactly like clusterTables, so
// the pushed fragment is byte-shaped like a `_table` response and morphs
// cleanly. The ?join=metrics columns are fed from the stream's cached 30s
// sub-poll (metricsUsage) instead of a per-push live fetch. The snapshot
// arrives as a render clone; every mutation this pass makes stays in it.
func (s *Server) streamListContext(r *http.Request, client *kube.Client, cluster string, table *kube.Table, metricsUsage map[string][2]float64) listContext {
	start := s.clock()
	namespace := r.PathValue("namespace")
	isAllNamespaces := namespace == kube.AllNamespaces
	table.Clusters = []string{cluster}
	for i := range table.Rows {
		table.Rows[i].Cluster = cluster
	}
	vis := s.applyTableOptionsWithUsage(r, client, table, namespace, isAllNamespaces, metricsUsage)
	return listContext{
		Cluster:         cluster,
		Namespace:       namespace,
		Plural:          r.PathValue("plural"),
		IsAllNamespaces: isAllNamespaces,
		ClusterCount:    1,
		Tables:          []kube.Table{*table},
		ColVis:          map[string][]columnVis{table.Resource.Plural: vis},
		Duration:        s.clock().Sub(start),
	}
}

// unionNamespacedResourceTypes resolves the sorted union of namespaced resource
// plurals across the clusters (the `_all`-namespaces case). The per-cluster
// discovery fans out concurrently; the result is a set then sort.Strings, so it
// is order-independent and deterministic regardless of completion order.
func (s *Server) unionNamespacedResourceTypes(r *http.Request, clusters []*kube.Cluster, clients requestKubeClients) []string {
	perCluster := make([][]string, len(clusters))
	g, _ := errgroup.WithContext(r.Context())
	g.SetLimit(s.searchConcurrency())
	for i, cluster := range clusters {
		i, cluster := i, cluster
		g.Go(func() error {
			types, _ := clients[cluster.Name].NamespacedResourceTypes(r.Context())
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

func (s *Server) applyTableOptions(r *http.Request, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool) []columnVis {
	return s.applyTableOptionsWithUsage(r, client, table, namespace, allNamespaces, nil)
}

// applyTableOptionsWithUsage is applyTableOptions with an optional pre-fetched
// metrics overlay: a non-nil metricsUsage feeds the ?join=metrics columns
// instead of a live metrics fetch. The Live stream (D19) renders pushes at up
// to ~3/s and refreshes usage on its own 30s sub-poll, so its renders must
// never re-list the metrics API; everything else passes nil and keeps the
// per-request fetch.
func (s *Server) applyTableOptionsWithUsage(r *http.Request, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool, metricsUsage map[string][2]float64) []columnVis {
	q := r.URL.Query()
	// D9 cookie fill: the ro_prefs colvis/sort prefs stand in for ABSENT URL
	// params (URL always wins; single-type pages only; render-only -- r.URL is
	// never mutated, so rebuilt hrefs and HX-Push-Url keep URL truth).
	fill := prefsListFill(r)
	hide := first(q.Get("hidecols"), q.Get("hide-columns"))
	if hide == "" {
		if fill.HasHide {
			// An explicit cookie hide set -- possibly EMPTY ("show everything"),
			// which must suppress the config default (user override wins, D8).
			hide = fill.Hide
		} else {
			hide = s.cfg.DefaultHiddenColumns[table.Resource.Plural]
		}
	}
	labels := first(q.Get("labelcols"), q.Get("label-columns"), s.cfg.DefaultLabelColumns[table.Resource.Plural])
	kube.AddLabelColumns(table, labels)
	if table.Resource.Plural == "nodes" {
		// Nodes get rich capacity/pods/conditions columns (read from each row's
		// status). The CPU/Memory capacity columns are added here ONLY when metrics
		// are not joined; with ?join=metrics the applyMetricsUsage CPU/Memory Usage
		// columns below carry the usage overlay instead (one CPU and one Memory
		// column either way, never both).
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
	if table.Resource.Plural == "services" {
		// Services get the External-IP / Selector columns GUARANTEED (appended from
		// each row's spec/status when a server-side Table omits them); the rich
		// pending/ports/selector-chips cells land in buildCellView.
		decorateServiceColumns(table)
	}
	if table.Resource.Plural == "ingresses" {
		// Ingresses get a synthetic TLS column derived from each row's spec.tls
		// (the server-side Table has no TLS column, SPEC §7.10).
		decorateIngressColumns(table)
	}
	if table.Resource.Plural == "jobs" {
		// Jobs get the verbatim-status guarantee (SPEC §7.11): a printer without
		// the Status column gains one derived from status.conditions, and a bare
		// "Failed" refines to the condition's verbatim reason
		// (BackoffLimitExceeded).
		decorateJobColumns(table)
	}
	if table.Resource.Plural == "events" {
		// Events get the ×N dedupe column (D15): the count decodes from each
		// row's object across BOTH event API shapes and lands before Message so
		// the wrapping msg column stays last. CronJobs and PersistentVolumes need
		// no table-level decoration — their printer columns already carry the v2
		// schema surface (Suspend/Last Schedule cells reskin in buildCellView;
		// the PV Status column rides the generic status cell).
		decorateEventColumns(table)
	}
	if q.Get("join") == "metrics" && (table.Resource.Plural == "pods" || table.Resource.Plural == "nodes") {
		usage := metricsUsage
		if usage == nil {
			usage = s.fetchMetricsUsage(r.Context(), client, table.Resource.Namespaced, namespace, allNamespaces, q.Get("selector"))
		}
		applyMetricsUsage(table, usage)
	}
	custom := first(q.Get("customcols"), q.Get("custom-columns"), s.cfg.DefaultCustomColumns[table.Resource.Plural])
	if custom != "" {
		s.joinCustomColumns(r.Context(), client, table, namespace, allNamespaces, custom, q)
	}
	// Column visibility (D8): applied AFTER the label / synthetic / joined
	// columns land, so the hide spec can remove synthetic columns too (until v2
	// it ran before the decorations, which made node Pods/Conditions, the
	// deployment Rollout, the namespace Labels, and the joined usage columns
	// unhideable) and the captured universe covers everything the table renders.
	// Filters and sort still run AFTER the removal, exactly as before: a hidden
	// column stays unfilterable and unsortable. The identity column is protected
	// here -- never removed, `*` included (the sticky first column, the
	// name-click open, and the row gestures all hang off it).
	vis := applyHiddenColumns(table, hide)
	kube.FilterRowsByNamespace(table, s.cfg.IncludeNamespaces, s.cfg.ExcludeNamespaces)
	kube.FilterTable(table, q.Get("filter"), false)
	// Filters v2 (D7): the repeatable `?f=` chips, single-type pages only (the
	// D1 boundary -- the same gate the interaction loop uses). A multi-type
	// page that receives `f` anyway IGNORES it: never a 500, never a
	// surprise-empty table the client UI cannot explain. Chips run on the full
	// dataset BEFORE sort/limit, after the joins/decorations (so the joined
	// metrics and synthetic columns are filterable) and alongside the legacy
	// params -- both grammars AND-combine. Parsed from RawQuery because the
	// OR-comma is RAW on the wire; an encoded %2C is a literal comma inside
	// one alternative (see filter.go).
	if isSingleListType(r.PathValue("plural")) {
		applyFilterChips(table, parseFilterParams(r.URL.RawQuery))
	}
	kube.GuessColumnClasses(table)
	// Node CPU/Memory + usage columns render as rich LEFT-aligned bar cells, so
	// drop the numeric right-alignment GuessColumnClasses infers from their float
	// sort-cells -- otherwise the header sits right while the bar+value sit left.
	if table.Resource.Plural == "nodes" {
		for i := range table.Columns {
			switch table.Columns[i].Name {
			case "CPU", "Memory", "CPU Usage", "Memory Usage":
				table.Columns[i].Class = ""
			}
		}
	}
	// The configmap/secret Data decorators run AFTER GuessColumnClasses for the
	// same header/cell alignment reason: their curation is dropping the numeric
	// class the guesser infers from the server's integer key-count cells (the
	// keys-chips strip is left-aligned; the count cell itself stays untouched as
	// the sort/TSV/filter truth).
	if table.Resource.Plural == "configmaps" {
		decorateConfigMapColumns(table)
	}
	if table.Resource.Plural == "secrets" {
		decorateSecretColumns(table)
	}
	kube.SortTable(table, first(q.Get("sort"), fill.Sort))
	if limit := q.Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 && n < len(table.Rows) {
			table.Rows = table.Rows[:n]
		}
	}
	return vis
}

// columnVis is one column-visibility entry (D8): a column of the FULLY
// decorated table (label columns, synthetic node/deployment/namespace columns,
// joined usage columns, the template-synthetic Created) plus whether the
// current render hides it and whether it is the protected identity column.
// The ⊞ popover renders one checkbox per entry, so hidden columns stay
// re-offerable even though they are gone from the kube.Table.
type columnVis struct {
	Name     string
	Hidden   bool
	Identity bool
}

// applyHiddenColumns resolves the effective hidecols spec against the
// decorated table: it returns the full popover universe and removes the hidden
// columns. The identity column -- the "Name" column, or the first column for a
// kind whose Table has none (the same nameColumn rule the row keys and the
// sticky column use) -- is NEVER removed, `*` included: a forced
// ?hidecols=Name is ignored server-side (D8). The synthetic Created column is
// template-rendered, not a kube column, so it joins the universe here and
// hides via the render flag rather than kube.RemoveColumns.
func applyHiddenColumns(table *kube.Table, spec string) []columnVis {
	hidden := map[string]bool{}
	for _, name := range strings.Split(spec, ",") {
		if name = strings.TrimSpace(name); name != "" {
			hidden[name] = true
		}
	}
	hides := func(name string) bool { return hidden["*"] || hidden[name] }
	protected := ""
	if len(table.Columns) > 0 {
		protected = table.Columns[nameColumn(table)].Name
	}
	vis := make([]columnVis, 0, len(table.Columns)+1)
	var remove []string
	for _, col := range table.Columns {
		entry := columnVis{Name: col.Name, Identity: col.Name == protected}
		if !entry.Identity && hides(col.Name) {
			entry.Hidden = true
			remove = append(remove, col.Name)
		}
		vis = append(vis, entry)
	}
	vis = append(vis, columnVis{Name: "Created", Hidden: hides("Created")})
	if len(remove) > 0 {
		// k8s printer-column names never contain commas (the same invariant the
		// cookie's comma-joined fill relies on), so the explicit name list is a
		// safe RemoveColumns spec -- and `*` never reaches kube unexpanded.
		kube.RemoveColumns(table, strings.Join(remove, ","))
	}
	return vis
}

// mergeColumnVis unions two column-visibility universes the way MergeTables
// unions columns: the left order wins and unseen right entries append -- but
// BEFORE the trailing Created entry, so the synthetic column stays last,
// mirroring the rendered header order. A nil left passes right through.
func mergeColumnVis(left, right []columnVis) []columnVis {
	if left == nil {
		return right
	}
	seen := map[string]bool{}
	for _, entry := range left {
		seen[entry.Name] = true
	}
	for _, entry := range right {
		if seen[entry.Name] {
			continue
		}
		seen[entry.Name] = true
		if n := len(left); n > 0 && left[n-1].Name == "Created" {
			left = append(left[:n-1], entry, left[n-1])
		} else {
			left = append(left, entry)
		}
	}
	return left
}

// fetchMetricsUsage resolves the metrics.k8s.io usage overlay for a pods or
// nodes scope: "namespace/name" → [cpu cores, memory bytes], decoded through
// kube.MetricsUsage (the typed quantity seam). nil when discovery or the
// metrics LIST fails — applyMetricsUsage then writes the zero placeholders,
// so a failed fetch never leaves ragged rows. Split from the column apply
// so the Live stream (D19) can refresh usage on its own 30s sub-poll instead
// of re-fetching per push.
func (s *Server) fetchMetricsUsage(ctx context.Context, client *kube.Client, namespaced bool, namespace string, allNamespaces bool, labelSelector string) map[string][2]float64 {
	metricsKind := "NodeMetrics"
	if namespaced {
		metricsKind = "PodMetrics"
	}
	rt, err := client.FindResourceByKind(ctx, "metrics.k8s.io/v1beta1", metricsKind, namespaced)
	if err != nil {
		return nil
	}
	listNS := namespace
	if allNamespaces {
		listNS = ""
	}
	list, err := client.List(ctx, &rt, kube.ListOptions{Namespace: listNS, LabelSelector: labelSelector})
	if err != nil {
		return nil
	}
	usage := map[string][2]float64{}
	for _, item := range list.Items {
		key, cpu, mem := kube.MetricsUsage(item.Object)
		usage[key] = [2]float64{cpu, mem}
	}
	return usage
}

// applyMetricsUsage appends the CPU/Memory Usage columns and EVERY row's two
// usage cells from the overlay. A row without metrics — and every row when
// the fetch failed (nil map) — gets the "metrics unknown" zero placeholders,
// so column/row cell counts always match and the table never renders ragged.
func applyMetricsUsage(table *kube.Table, usage map[string][2]float64) {
	table.Columns = append(table.Columns, kube.Column{Name: "CPU Usage"}, kube.Column{Name: "Memory Usage"})
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

// decorateServiceColumns guarantees the v2 services schema surface (SPEC §7.8):
// the External-IP and Selector columns are appended -- read from each row's
// spec/status with the SAME value encoding the upstream printer uses
// (`<none>`/`<pending>`/ExternalName target; sorted comma-joined `k=v`) -- when
// a server-side Table omits them. A standard apiserver provides both (Selector
// at priority 1, which readout keeps), so there the columnIndex guards make
// this a no-op; the append covers trimmed-down printers and keeps the rich
// External-IP/Selector cells kind-invariant. Cells are appended for EVERY row
// in lockstep with the columns, so the table never goes ragged.
func decorateServiceColumns(table *kube.Table) {
	type svcCol struct {
		name string
		cell func(obj map[string]any) any
	}
	cols := []svcCol{
		{"External-IP", func(obj map[string]any) any { return serviceExternalIPText(obj) }},
		{"Selector", func(obj map[string]any) any { return serviceSelectorText(obj) }},
	}
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

// decorateIngressColumns appends the synthetic TLS column and fills each row's
// cell with the plain DISPLAY value ("tls" when spec.tls terminates at least
// one host, "—" otherwise) so sort, TSV, filter, and the generic fallback see
// a sensible value; the rich tlsCellView renderer re-reads the row object for
// the earned-green lock (SPEC §4.13). The cell is appended for EVERY row in
// lockstep with the column, so the table never goes ragged. A TLS column the
// server-side Table already provided (it does not today) is never duplicated.
func decorateIngressColumns(table *kube.Table) {
	if columnIndex(table.Columns, "TLS") >= 0 {
		return
	}
	table.Columns = append(table.Columns, kube.Column{Name: "TLS"})
	for i := range table.Rows {
		table.Rows[i].Cells = append(table.Rows[i].Cells, ingressTLSText(table.Rows[i].Object))
	}
}

// insertTableColumn inserts a named column at idx (appending when idx is out
// of range) and gives EVERY row its matching cell from cell(obj), keeping
// columns and cells in lockstep so the table never goes ragged. The shared
// seam under the jobs Status and events Count decorators, which must place
// their column mid-table (after Name / before Message) rather than append.
func insertTableColumn(table *kube.Table, idx int, name string, cell func(obj map[string]any) any) {
	if idx < 0 || idx > len(table.Columns) {
		idx = len(table.Columns)
	}
	table.Columns = append(table.Columns, kube.Column{})
	copy(table.Columns[idx+1:], table.Columns[idx:])
	table.Columns[idx] = kube.Column{Name: name}
	for i := range table.Rows {
		row := &table.Rows[i]
		value := cell(row.Object)
		if idx >= len(row.Cells) {
			row.Cells = append(row.Cells, value)
			continue
		}
		row.Cells = append(row.Cells, nil)
		copy(row.Cells[idx+1:], row.Cells[idx:])
		row.Cells[idx] = value
	}
}

// decorateJobColumns guarantees the jobs verbatim-status surface (SPEC §7.11).
// A printer WITHOUT the Status column (pre-1.30 apiservers) gains a synthetic
// one right after the identity column, derived from status.conditions
// (jobStatusText). A printer WITH it keeps its cell as the truth — except a
// bare "Failed", which refines to the Failed condition's verbatim reason
// (`BackoffLimitExceeded`, never a paraphrase) so display, sort, TSV, and
// filter all speak the full status name. A Status column the decorator already
// added (or any non-Failed printer word) is never rewritten, so the pass is
// idempotent.
func decorateJobColumns(table *kube.Table) {
	idx := columnIndex(table.Columns, "Status")
	if idx < 0 {
		at := columnIndex(table.Columns, "Name")
		if at < 0 {
			// No Name column: append, never displace the identity (first) column.
			at = len(table.Columns)
		} else {
			at++
		}
		insertTableColumn(table, at, "Status", func(obj map[string]any) any { return jobStatusText(obj) })
		return
	}
	for i := range table.Rows {
		row := &table.Rows[i]
		if idx >= len(row.Cells) || cellDisplayString(row.Cells[idx]) != "Failed" {
			continue
		}
		if reason := jobFailedReason(row.Object); reason != "" {
			row.Cells[idx] = reason
		}
	}
}

// jobStatusText derives the jobs Status DISPLAY value from the object for
// printers that lack the column: the first True condition wins — a Failed
// condition surfaces its verbatim reason (BackoffLimitExceeded), any other
// condition its type (Complete, Suspended, FailureTarget, …) — and a job with
// no true condition is still Running.
func jobStatusText(obj map[string]any) string {
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok || nestedString(cond, "status") != "True" {
			continue
		}
		typ := nestedString(cond, "type")
		if typ == "Failed" {
			if reason := nestedString(cond, "reason"); reason != "" {
				return reason
			}
		}
		if typ != "" {
			return typ
		}
	}
	return "Running"
}

// jobFailedReason is the verbatim reason of a job's true Failed condition
// ("" when the job has none) — the refinement source for a printer's bare
// "Failed" Status cell.
func jobFailedReason(obj map[string]any) string {
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if nestedString(cond, "type") == "Failed" && nestedString(cond, "status") == "True" {
			return nestedString(cond, "reason")
		}
	}
	return ""
}

// decorateEventColumns appends the events ×N dedupe column (D15): the count
// decodes from each row's object with the pinned dual-API precedence
// (series.count → count → deprecatedCount, defaulting to 1) and the cell
// carries the plain int64 so sort, TSV, and filter see the numeric truth; the
// rich ×N cell re-decodes at cell-build time. The column lands BEFORE Message
// so the wrapping msg column stays last. A Count column the server-side Table
// already provided (a priority-1 printer column) is never duplicated — the
// rich cell still re-decodes from the object either way.
func decorateEventColumns(table *kube.Table) {
	if columnIndex(table.Columns, "Count") >= 0 {
		return
	}
	insertTableColumn(table, columnIndex(table.Columns, "Message"), "Count", func(obj map[string]any) any {
		if item, ok := decodeEventItem(obj); ok {
			return item.eventCount()
		}
		return int64(1)
	})
}

// decorateConfigMapColumns curates the configmaps Data column for the keys
// cell (SPEC §4.10). NO synthetic column is needed -- the server's integer
// key-count cell stays in place as the sort/TSV/filter truth and the
// `name · size` chips re-read the row object at cell-build time
// (configMapKeyChips) -- but the count cell makes GuessColumnClasses
// right-align the column, while the chips strip is left-aligned; the curation
// here drops that alignment so the header never sits right of its cells (the
// same mismatch the node capacity columns clear above).
func decorateConfigMapColumns(table *kube.Table) {
	clearDataColumnAlignment(table)
}

// decorateSecretColumns curates the secrets Data column exactly like
// decorateConfigMapColumns (the secret key chips decode from `data` with
// DECODED byte sizes in secretKeyChips; the count cell stays the plain truth):
// the only table-level curation is dropping the guessed numeric alignment.
func decorateSecretColumns(table *kube.Table) {
	clearDataColumnAlignment(table)
}

// clearDataColumnAlignment drops the numeric right-alignment from a Data
// column (shared by the configmap/secret decorators).
func clearDataColumnAlignment(table *kube.Table) {
	for i := range table.Columns {
		if table.Columns[i].Name == "Data" {
			table.Columns[i].Class = ""
		}
	}
}

// serviceExternalIPText reconstructs the services External-IP printer column
// from a Service object, mirroring the upstream printer's encoding:
// ExternalName -> the spec.externalName target; LoadBalancer -> the
// status.loadBalancer ingress IPs/hostnames + any spec.externalIPs, or the
// literal `<pending>` while unprovisioned; every other type -> spec.externalIPs
// or the literal `<none>`.
func serviceExternalIPText(obj map[string]any) string {
	svcType := nestedString(obj, "spec", "type")
	if svcType == "ExternalName" {
		return nestedString(obj, "spec", "externalName")
	}
	externalIPs, _, _ := unstructured.NestedStringSlice(obj, "spec", "externalIPs")
	if svcType == "LoadBalancer" {
		var ips []string
		ingress, _, _ := unstructured.NestedSlice(obj, "status", "loadBalancer", "ingress")
		for _, item := range ingress {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if ip := first(nestedString(entry, "ip"), nestedString(entry, "hostname")); ip != "" {
				ips = append(ips, ip)
			}
		}
		ips = append(ips, externalIPs...)
		if len(ips) > 0 {
			return strings.Join(ips, ",")
		}
		return "<pending>"
	}
	if len(externalIPs) > 0 {
		return strings.Join(externalIPs, ",")
	}
	return "<none>"
}

// serviceSelectorText is the plain Selector DISPLAY value (the printer's
// sorted comma-joined `k=v` encoding, `<none>` for a selectorless service);
// the rich chips re-read spec.selector via selectorChips.
func serviceSelectorText(obj map[string]any) string {
	selector, _, _ := unstructured.NestedStringMap(obj, "spec", "selector")
	if len(selector) == 0 {
		return "<none>"
	}
	return formatLabels(selector)
}

// selectorChips builds the service Selector chips from spec.selector (sorted
// by key for a stable order). The chips are NEUTRAL and deliberately carry NO
// click-to-filter href: a `label:key=value` chip filters by the SERVICES' OWN
// metadata.labels, while a selector names the labels of the pods the service
// selects -- linking the two would filter on the wrong object's labels.
func selectorChips(obj map[string]any) []chipView {
	selector, _, _ := unstructured.NestedStringMap(obj, "spec", "selector")
	if len(selector) == 0 {
		return nil
	}
	keys := make([]string, 0, len(selector))
	for key := range selector {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	chips := make([]chipView, 0, len(keys))
	for _, key := range keys {
		chips = append(chips, chipView{Key: key, Val: selector[key]})
	}
	return chips
}

// ingressTLSTerminated reports whether an Ingress terminates TLS: spec.tls
// lists at least one entry (SPEC §7.10 "ingress TLS from spec.tls").
func ingressTLSTerminated(obj map[string]any) bool {
	tls, _, _ := unstructured.NestedSlice(obj, "spec", "tls")
	return len(tls) > 0
}

// ingressTLSText is the plain DISPLAY value for the synthetic TLS column
// ("tls" / "—"), matching what the rich lock cell shows so the generic
// fallback / TSV / sort / filter see the same truth.
func ingressTLSText(obj map[string]any) string {
	if ingressTLSTerminated(obj) {
		return "tls"
	}
	return "—"
}

// commaListValues splits a printer's comma-joined list cell (service ports
// "80/TCP,443/TCP", ingress hosts "a.com,b.com") into its values, dropping
// the literal `<none>` a portless service prints. nil means an empty list
// (the ports/hosts cells render the muted "—").
func commaListValues(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" && part != "<none>" {
			out = append(out, part)
		}
	}
	return out
}

// configMapKeyChips builds the configmap Data key chips from the row object:
// `data` key names sized by their value's byte length, `binaryData` key names
// sized by the DECODED byte length (the wire form is base64). Only key names
// and sizes leave this function (SPEC §4.10).
func configMapKeyChips(obj map[string]any) []keyChipView {
	data, _, _ := unstructured.NestedStringMap(obj, "data")
	binary, _, _ := unstructured.NestedStringMap(obj, "binaryData")
	sizes := make(map[string]int64, len(data)+len(binary))
	for key, value := range data {
		sizes[key] = int64(len(value))
	}
	for key, value := range binary {
		sizes[key] = base64DecodedLen(value)
	}
	return keyChips(sizes)
}

// secretKeyChips builds the secret Data key chips from the row object: `data`
// key names with the DECODED byte size (base64 -> raw length). The secret
// VALUE bytes never reach the view model -- only the length survives
// base64DecodedLen, and keyChipView has no value field by construction.
func secretKeyChips(obj map[string]any) []keyChipView {
	data, _, _ := unstructured.NestedStringMap(obj, "data")
	sizes := make(map[string]int64, len(data))
	for key, value := range data {
		sizes[key] = base64DecodedLen(value)
	}
	return keyChips(sizes)
}

// base64DecodedLen is the raw byte length of a base64 value. ONLY the length
// escapes; the decoded bytes are dropped here so a secret value cannot travel
// further than this stack frame. A value that fails to decode (never the case
// for apiserver-encoded data) falls back to its encoded length -- still a
// size, never content.
func base64DecodedLen(encoded string) int64 {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return int64(len(encoded))
	}
	return int64(len(raw))
}

// keyChips renders a key->size map as sorted `name · size` chips (humanBytes
// sizes), the shared tail of configMapKeyChips / secretKeyChips. nil for no
// keys, so the renderer shows the muted "—".
func keyChips(sizes map[string]int64) []keyChipView {
	if len(sizes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(sizes))
	for key := range sizes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	chips := make([]keyChipView, 0, len(keys))
	for _, key := range keys {
		chips = append(chips, keyChipView{Name: key, Size: humanBytes(sizes[key])})
	}
	return chips
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
	// Canonicalize the request URL FIRST (D6 state coherence): this builder
	// serves BOTH the full page and the `_table` partial, and every href it
	// resolves (sort headers, metrics join, label-selector links, filter chips,
	// retry) must point at the canonical LIST PAGE -- never at the partial.
	// Without this, a fragment rendered by the partial handler baked
	// `…/_table?sort=…` into its hrefs, so the first navigation after a refresh
	// tick landed on the bare fragment. The shallow request clone keeps
	// context/path values intact; only the URL is rewritten.
	canonical := *r
	canonical.URL = resourceListBaseURL(r.URL)
	r = &canonical
	q := r.URL.Query()
	sortValue := q.Get("sort")
	joinValue := q.Get("join")
	// D9 cookie fill: with no ?sort= in the URL the persisted sort drives the
	// render (applyTableOptions sorted the rows with the same fill), so the
	// th.sorted highlight, the sort icons, and the asc/desc header toggle must
	// see the EFFECTIVE sort. The fill never touches r.URL: header hrefs SET
	// sort explicitly, every other rebuilt href and the partial handler's
	// HX-Push-Url keep carrying only what the user explicitly chose -- pushed
	// URLs stay user-truth (back-button parity), and the cookie keeps filling
	// them identically on reload.
	if sortValue == "" {
		sortValue = prefsListFill(r).Sort
	}

	// The D1 surface boundary: the v2 interaction loop (partial sort headers,
	// row identity, location-derived ticks) applies to single-resource-type
	// pages only. partialSortURL is the `_table` base the header hx-get links
	// sort against; nil disables the whole loop for multi-type pages.
	single := isSingleListType(lc.Plural)
	var partialSortURL *url.URL
	if single {
		clone := *r.URL
		clone.Path = strings.TrimRight(clone.Path, "/") + "/_table"
		partialSortURL = &clone
	}

	v := listView{
		Cluster:         lc.Cluster,
		Namespace:       lc.Namespace,
		Plural:          lc.Plural,
		IsAllClusters:   lc.IsAllClusters,
		IsAllNamespaces: lc.IsAllNamespaces,
		ClusterCount:    lc.ClusterCount,
		Duration:        lc.Duration,
		Errors:          lc.Errors,
		SingleType:      single,
	}
	if lc.Namespace != "" && lc.Plural != "namespaces" && !lc.IsAllNamespaces {
		v.AllNamespacesHref = fmt.Sprintf("/clusters/%s/namespaces/_all/%s?%s", url.PathEscape(lc.Cluster), url.PathEscape(lc.Plural), r.URL.RawQuery)
	}
	// Bulk download surface (D11 / Unit 17): single-type AND single-cluster
	// lists get the clean `?download=yaml` base href baked onto the bulk bar;
	// multi-cluster scope leaves it empty, which renders the Download button
	// disabled with the explanatory title (the names grammar carries no
	// cluster segment, so a cross-cluster bulk URL would be ambiguous).
	if single && !lc.IsAllClusters && lc.ClusterCount == 1 {
		v.BulkDownloadHref = bulkDownloadHref(r.URL)
	}
	// The client-side stale path (D11) needs its markup hooks in the first server
	// response: a hidden `.ro-banner.warn` readout.js reveals on an auto-refresh
	// error, dimming the rows in #resource-list-content (the morph target) instead
	// of blanking them. The server never decides "stale" -- there is no last-good
	// cache; the client does, on a refresh error that keeps the rows. The dim
	// target id is owned by readout.js (hardcoded), so it is not threaded here.
	v.StaleBanner = true

	// Active filters drive the empty-FILTERED state (removable chips + Clear) vs
	// the plainly-empty state (broad action). Resolved once for the page (the
	// filter/selector params are page-wide, not per-table).
	filterChips := buildFilterChips(r)
	clearHref := ""
	if len(filterChips) > 0 {
		clearHref = delQuery(r.URL, "filter", "selector", "labelcols", "label-columns", "f")
	}
	// The chips editor (D7): single-type pages only, mirroring the `?f=` gate in
	// applyTableOptions. The chips ride the morphed fragment, so a shareable URL
	// lands with its chips visible and a chip-committing partial re-renders them.
	if single {
		v.FilterBar = &filterBarView{Plural: lc.Plural, Chips: buildFilterBarChips(r)}
	}

	for ti := range lc.Tables {
		table := &lc.Tables[ti]
		tv := tableView{
			Table:           *table,
			Kind:            pluralizeKind(table.Resource.Kind),
			DownloadTSVHref: downloadTSVHref(r.URL, table.Resource.Plural),
			SearchHref:      fmt.Sprintf("/search?cluster=%s&namespace=%s&type=%s", url.QueryEscape(strings.Join(table.Clusters, ",")), url.QueryEscape(lc.Namespace), url.QueryEscape(table.Resource.Plural)),
			Tools:           s.buildToolsView(r, table),
			Phase:           kube.PhaseSummary(table),
		}
		if (table.Resource.Plural == "pods" || table.Resource.Plural == "nodes") && joinValue == "" {
			tv.ShowMetricsHref = addQuery(r.URL, "join", "metrics")
		}
		// Column visibility (D8): the synthetic Created column hides through a
		// render flag (it is not a kube column), and the popover universe rides
		// only on single-type pages -- the same D1 gate the loop, the chips
		// editor, and the cookie fill share. A hand-built listContext without
		// ColVis (tests) keeps Created shown.
		if vis, ok := lc.ColVis[table.Resource.Plural]; ok {
			for _, entry := range vis {
				if entry.Name == "Created" {
					tv.HideCreated = entry.Hidden
				}
			}
			if single {
				tv.ColumnVis = vis
			}
		}
		for _, col := range table.Columns {
			sortParam := col.Name
			if sortValue == col.Name {
				sortParam = col.Name + ":desc"
			}
			cv := columnView{
				SortHref: addQuery(r.URL, "sort", sortParam),
				SortIcon: sortIcon(sortValue, col.Name),
			}
			if partialSortURL != nil {
				cv.PartialHref = addQuery(partialSortURL, "sort", sortParam)
				// Filterable-field marker for the chips editor (single-type only,
				// same gate as the loop): the autocomplete reads these headers.
				cv.Hint = filterFieldHint(&col)
			}
			tv.Columns = append(tv.Columns, cv)
		}
		tv.CreatedHref = addQuery(r.URL, "sort", createdSortParam(sortValue))
		tv.CreatedIcon = sortIcon(sortValue, "Created")
		if partialSortURL != nil {
			tv.CreatedPartialHref = addQuery(partialSortURL, "sort", createdSortParam(sortValue))
		}

		multiCluster := len(table.Clusters) > 1
		for _, row := range table.Rows {
			ns := nestedString(row.Object, "metadata", "namespace")
			name := cellString(row, nameColumn(table))
			rv := rowView{
				// The row stripe (SPEC §3: err/warn rows only) derives from the same
				// kube.StatusTone table the status dot uses, via RowStatusClass.
				StatusClass:  kube.RowStatusClass(table, row),
				Cluster:      row.Cluster,
				Namespace:    ns,
				CreatedClass: s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")),
				CreatedText:  formatTimestamp(nestedString(row.Object, "metadata", "creationTimestamp")),
			}
			if single {
				rv.Key = rowKey(row.Cluster, ns, name)
				// Per-row gesture targets (Unit 16 / D10): server-resolved hrefs
				// the context menu + bulk actions read off the <tr>. OpenHref
				// mirrors the name-cell link exactly (buildCellView's cellName
				// branch is the twin), including the namespaces drill-down to
				// that namespace's pods list; YAML/download/logs always target
				// the object's DETAIL route (a namespace row's YAML view is the
				// namespace object, not the pods list). Logs are pods-only --
				// every other kind leaves LogsHref empty and the menu item hides.
				rv.Name = name
				detail := resourceHref(row.Cluster, &table.Resource, ns, name)
				rv.OpenHref = detail
				if table.Resource.Plural == "namespaces" {
					rv.OpenHref = fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(row.Cluster), url.PathEscape(name))
				}
				rv.YAMLHref = detail + "?view=yaml"
				rv.DownloadHref = detail + "?download=yaml"
				if table.Resource.Plural == "pods" {
					rv.LogsHref = detail + "/logs"
				}
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

// isSingleListType reports whether the {plural} path segment names exactly ONE
// resource type -- the D1 surface boundary for the v2 interaction loop (D6).
// Multi-type pages ("all", the "_all" union, and CSV lists) keep the v1
// behavior: boosted sort links, no row identity, the baked partial URL.
func isSingleListType(plural string) bool {
	return plural != "" && plural != "all" && plural != kube.AllNamespaces && !strings.Contains(plural, ",")
}

// rowKey is the stable row object identity "cluster/ns/name" with empty
// segments collapsed (a cluster-scoped object yields "cluster/name") -- the D6
// data-key contract that morphs, selection, and j/k focus key on.
func rowKey(cluster, namespace, name string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{cluster, namespace, name} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
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
	// Filters v2 chips (D7): one removable chip per `?f=` param, single-type
	// pages only (the same gate applyTableOptions filters under -- a multi-type
	// page ignores `f`, so its emptiness must never be blamed on it). The ✕
	// removes exactly that raw occurrence so sibling chips keep their raw
	// OR-comma encoding byte-for-byte.
	if isSingleListType(r.PathValue("plural")) {
		chips = append(chips, buildFilterBarChips(r)...)
	}
	return chips
}

// buildFilterBarChips resolves the `?f=` chips for the chips editor (and the
// f-leg of the empty-filtered state): one chip per raw occurrence, with the
// editor's Field/Op/Value display split and a ✕ href that removes exactly that
// raw occurrence (sibling chips keep their wire encoding byte-for-byte). A
// malformed chip (no operator) keeps Field empty so it renders whole -- it can
// still be removed even though it matches no row.
func buildFilterBarChips(r *http.Request) []filterChipView {
	var chips []filterChipView
	for _, chip := range parseFilterParams(r.URL.RawQuery) {
		chips = append(chips, filterChipView{
			Label:      chip.display(),
			RemoveHref: delQueryRawValue(r.URL, "f", chip.Raw),
			Field:      chip.Field,
			Op:         string(chip.Op),
			Value:      strings.Join(chip.Values, ","),
		})
	}
	return chips
}

// filterFieldHint maps a column to its chips-editor autocomplete type hint.
// Every real Table column is filterable (resolveFilterColumn binds any of
// them); the hint only describes which `>`/`<` mode the value compares in:
// kubectl-age columns as durations, numeric columns (incl. the decorated
// restarts "3 (4m ago)" cells, whose leading token is numeric) as numbers,
// everything else as text.
func filterFieldHint(col *kube.Column) string {
	switch col.Name {
	case "Age", "First Seen", "Last Seen", "Duration", "Last Schedule":
		return "duration"
	case "Restarts":
		return "number"
	}
	switch col.Type {
	case "integer", "number":
		return "number"
	}
	return "text"
}

// buildListState classifies a single-cluster whole-list failure into the
// forbidden state (an apiserver 403 naming the verb/resource/namespace) or the
// unreachable state (a transport/dial failure that never reached the apiserver,
// OR an apiserver 5xx Status -- both shown with the REAL error string in the
// mono errdetail block, never a cute message, SPEC §1.5/D16). It returns nil
// for any other failure (a missing resource type, a 4xx Status such as a bad
// selector), so those keep the existing partial-error banner. The retry is the
// same list URL (a read-only GET); Back to clusters is /clusters.
func (s *Server) buildListState(r *http.Request, lc *listContext) *listStateView {
	err := lc.Errors[0]
	forbidden := kube.IsForbidden(err)
	apiStatus := kube.IsAPIStatusError(err)
	unreachable := !forbidden && !kube.IsNotFound(err) && (!apiStatus || kube.IsServerError(err))
	if !forbidden && !unreachable {
		return nil
	}
	state := &listStateView{
		Cluster:   lc.Cluster,
		Verb:      "list",
		Resource:  lc.Plural,
		Namespace: lc.Namespace,
		RetryHref: r.URL.String(),
		BackHref:  "/clusters",
		SourceErr: err,
	}
	if forbidden {
		state.Kind = stateForbidden
		state.Hint = forbiddenStateHint
		state.Detail = "403 Forbidden · " + err.Error()
	} else {
		state.Kind = stateUnreachable
		state.Hint = unreachableStateHint(apiStatus)
		state.Detail = err.Error()
	}
	return state
}

// forbiddenStateHint is the one plain-language line of the forbidden state
// (prototype VIEW.states copy, D16); the verbatim 403 Status rides below it in
// the mono errdetail block.
const forbiddenStateHint = "Your credentials can browse this cluster, but RBAC denies this view."

// unreachableStateHint is the plain-language line of the unreachable state.
// The prototype copy ("the request never made it") is literal for a transport
// failure; an apiserver 5xx DID reach the apiserver, so it gets a truthful
// variant -- the verbatim Status message below carries the real detail either
// way (SPEC §1.5).
func unreachableStateHint(apiAnswered bool) string {
	if apiAnswered {
		return "The apiserver answered with an error."
	}
	return "The request never made it to the apiserver."
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
	if i >= len(table.Columns) {
		return cellView{Kind: cellPlain, Value: value}
	}
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
		// SPEC §4.2 middle truncation: a head longer than 42 chars displays as
		// `first26…last12` with the FULL name in the tooltip. The hash tail is
		// never touched, so a cron pod's job/pod suffix stays unique on screen.
		if display, truncated := MiddleTruncate(cv.NameHead, nameHeadMax, nameHeadLead, nameHeadTrail); truncated {
			cv.NameHead = display
			cv.Title = value
		}
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
		// carries the usage (cores/bytes from applyMetricsUsage), the node's
		// capacity comes from status.capacity, and the bucket + fill come from
		// usage/capacity.
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
		// Label-chip click-to-filter (D7 / SPEC §8.1): on a single-type page each
		// chip links to THIS list with the `label:key=value` chip appended to its
		// `?f=` set (the same gate the filter engine applies under -- a multi-type
		// page ignores `f`, so its chips stay inert spans).
		if isSingleListType(r.PathValue("plural")) {
			for ci := range cv.Chips {
				cv.Chips[ci].Href = addFilterChipHref(r.URL, "label:"+cv.Chips[ci].Key+"="+cv.Chips[ci].Val)
			}
		}
	case (table.Resource.Plural == "services" && colName == "External-IP") ||
		(table.Resource.Plural == "ingresses" && colName == "Address"):
		// SPEC §4.12 pending cell: the printer's `<none>` (or an empty address) is
		// the faint none; the literal `<pending>` of an unprovisioned LB/ingress is
		// the amber pulsing in-flight state; an ExternalName target / provisioned
		// address renders verbatim.
		cv = pendingCellView(value)
	case table.Resource.Plural == "services" && colName == "Port(s)":
		// SPEC §4.11 ports cell over the printer's comma-joined list: first 2 +
		// faint "+N", the full list in the tooltip; portless (`<none>`) -> "—".
		cv = portsCellView(commaListValues(value))
	case table.Resource.Plural == "ingresses" && colName == "Hosts":
		// SPEC §4.11 hosts cell: the first host + faint "+N hosts" with the full
		// newline-joined list in the tooltip.
		cv = hostsCellView(commaListValues(value))
	case table.Resource.Plural == "services" && colName == "Selector" && table.Columns[i].Label == "":
		// The services Selector column renders neutral chips read from
		// spec.selector (SPEC §7.8). Deliberately NO click-to-filter href (see
		// selectorChips); the Label=="" guard keeps a user-added labelcols
		// "Selector" column on the label path.
		cv.Kind = cellChips
		cv.Chips = selectorChips(row.Object)
	case table.Resource.Plural == "ingresses" && colName == "TLS":
		// The synthetic TLS column (added by decorateIngressColumns) renders the
		// earned-green lock only when spec.tls terminates (SPEC §4.13), else "—".
		cv = tlsCellView(ingressTLSTerminated(row.Object))
	case table.Resource.Plural == "configmaps" && colName == "Data":
		// The configmap Data column renders `name · size` key chips decoded from
		// the row object's data/binaryData (SPEC §4.10); the server's count cell
		// stays in the kube.Table for sort/TSV/filter.
		cv = keysCellView(configMapKeyChips(row.Object))
	case table.Resource.Plural == "secrets" && colName == "Data":
		// The secret Data column renders key chips with DECODED byte sizes; the
		// VALUE bytes never reach the view model (SPEC §4.10, secretKeyChips).
		cv = keysCellView(secretKeyChips(row.Object))
	case table.Resource.Plural == "cronjobs" && colName == "Suspend":
		// The cronjob Suspend cell renders the prototype's status vocabulary:
		// the printer's boolean maps false→Active (ok, live health) /
		// true→Suspended (mute, SPEC §3) with the tone owned by kube.StatusTone
		// via CellClass — display-only; the kube.Table cell keeps the printer
		// boolean for sort/TSV/filter. Neither word is transient, so no pulse.
		label := "Active"
		if strings.EqualFold(value, "true") {
			label = "Suspended"
		}
		cv.Kind = cellStatus
		cv.Value = label
		cv.Tone = statusTone(kube.CellClass(table.Resource.Plural, "Status", label))
	case table.Resource.Plural == "cronjobs" && colName == "Last Schedule":
		// SPEC §4.14 lastrun cell: the printer's compressed duration gains the
		// age-bucket colour + " ago"; a cronjob that never ran prints the
		// literal <none> on the wire — that IS the empty case → faint <never>.
		if value == "<none>" {
			value = ""
		}
		cv = lastRunCellView(value)
	case table.Resource.Plural == "jobs" && colName == "Completions" && strings.Contains(value, "/"):
		// SPEC §4.4: completions share the ready-ratio grammar (full green when
		// n==m, partial amber, zero faint).
		cv.Kind = cellReady
		cv.Ratio = readyRatioClass(value)
	case table.Resource.Plural == "events" && colName == "Type":
		// The events Type cell is a status cell whose vocabulary IS SPEC §3
		// (Normal→mute, Warning→warn — never an invented stronger severity);
		// CellClass("events","Type",…) delegates to kube.StatusTone. Neither
		// word is transient, so no pulse.
		cv.Kind = cellStatus
		cv.Tone = statusTone(cls)
	case table.Resource.Plural == "events" && colName == "Object":
		// SPEC §4 evobj: kind icon + faint "Kind/" + the 20…8 middle-truncated
		// name, decoded from involvedObject (core/v1) or regarding
		// (events.k8s.io/v1). An undecodable ref keeps the printer's plain
		// "kind/name" cell.
		if item, ok := decodeEventItem(row.Object); ok && item.refName() != "" {
			cv = evObjCellView(item.refKind(), item.refName())
		} else {
			cv.Kind = cellPlain
		}
	case table.Resource.Plural == "events" && colName == "Count":
		// SPEC §4.15 ×N cell over the D15 dual-API count decode (≥20 amber, 1
		// faint). Re-decoded from the row object so a server-provided Count
		// column shows the same pinned-precedence truth as the decorated one.
		n := 1
		if item, ok := decodeEventItem(row.Object); ok {
			n = int(item.eventCount())
		}
		cv = countCellView(n)
	case table.Resource.Plural == "events" && colName == "Last Seen":
		// SPEC §4 evage: the two-layer age built from the D15 timestamp decode
		// (last-seen lead token bucket-coloured; "(first <dur> ago)" faint when
		// count > 1 and the spread exceeds 60s). When no timestamp decodes the
		// printer's own Last Seen duration stays as the single layer.
		text := value
		if item, ok := decodeEventItem(row.Object); ok {
			if t := eventAgeText(item, s.clock()); t != "" {
				text = t
			}
		}
		cv = evAgeCellView(text)
	case table.Resource.Plural == "events" && colName == "Message":
		// SPEC §4.16 msg: THE only wrapping column in the system (the 520px
		// clamp lives in CSS on td.ro-event-msg).
		cv = msgCellView(value)
	case colName == "CPU Usage":
		cv.Kind = cellCPU
		cv.Value = cpuFormat(cell)
	case colName == "Memory Usage":
		cv.Kind = cellMemory
		cv.Value = memoryMiBFormat(cell)
	case colName == "Status":
		cv.Kind = cellStatus
		// cls is kube.CellClass's encoding of kube.StatusTone (SPEC §3, the single
		// value->tone owner), so the dot tone always exists (fallback mute).
		cv.Tone = statusTone(cls)
		// Pulse the transient set for ANY kind's status cell (law §1.3) -- the set
		// itself gates (steady and err states never pulse), so a Terminating
		// namespace pulses exactly like a Terminating pod.
		cv.Pulse = transientStatus(value)
	case colName == "Ready" && strings.Contains(value, "/"):
		cv.Kind = cellReady
		cv.Ratio = readyRatioClass(value)
	case colName == "Restarts":
		cv.Kind = cellRestarts
		cv.Value, cv.Ago = splitRestarts(value)
		cv.Tone = restartsTone(cv.Value)
		// SPEC §4.5: the restart count gets a thousands separator (1047 ->
		// 1,047). Applied after the tone (which keys on the raw "0") and safe
		// for any non-numeric cell (groupThousands passes those through).
		cv.Value = groupThousands(cv.Value)
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

// ---------------------------------------------------------------------------
// SPEC §4 cookbook cell constructors (Unit 10). Each builds the resolved
// cellView for one corner-case cell type over plain data; the kind-specific
// schema decorators that read row objects and CALL these land with the
// services/ingress/configmap/secret/cronjob/job columns (Unit 11) and the
// events columns (Unit 12). Display-only: the kube.Table cell keeps its raw
// value for sort/filter/TSV.
// ---------------------------------------------------------------------------

// portsCellMax / hostsCellMax / keysCellMax / chipsCellMax are the SPEC §4
// in-cell overflow thresholds: 2 ports, 1 host, 3 data keys, 2 label/selector
// chips shown before the faint +N (ports/hosts) or the +N expand button
// (keys/chips).
const (
	portsCellMax = 2
	hostsCellMax = 1
	keysCellMax  = 3
	chipsCellMax = 2
)

// pendingCellView resolves a service External-IP / ingress Address cell (SPEC
// §4.12): empty -- including the printer's literal `<none>`, which IS the
// empty case on the wire -- -> the faint `<none>`, the literal `<pending>` ->
// an amber PULSING dot + the word "pending" (an in-flight state, law §1.3),
// anything else -> the plain address.
func pendingCellView(value string) cellView {
	cv := cellView{Kind: cellPending, Value: value}
	switch value {
	case "", "<none>":
		cv.Value = ""
	case "<pending>":
		cv.Value = "pending"
		cv.Tone = "warn"
		cv.Pulse = true
	}
	return cv
}

// portsCellView resolves a service Ports cell (SPEC §4.11): the first 2 ports
// joined ", ", a faint "+N" for the rest, and the FULL comma-joined list in the
// tooltip. No ports -> the muted "—" (empty Value).
func portsCellView(ports []string) cellView {
	cv := cellView{Kind: cellPorts}
	if len(ports) == 0 {
		return cv
	}
	shown := ports
	if len(ports) > portsCellMax {
		shown = ports[:portsCellMax]
		cv.More = "+" + strconv.Itoa(len(ports)-portsCellMax)
	}
	cv.Value = strings.Join(shown, ", ")
	cv.Title = strings.Join(ports, ", ")
	return cv
}

// hostsCellView resolves an ingress Hosts cell (SPEC §4.11): the first host +
// a faint "+N hosts", with the full newline-joined list in the tooltip. No
// hosts -> the muted "—" (empty Value).
func hostsCellView(hosts []string) cellView {
	cv := cellView{Kind: cellHosts}
	if len(hosts) == 0 {
		return cv
	}
	cv.Value = hosts[0]
	if len(hosts) > hostsCellMax {
		cv.More = "+" + strconv.Itoa(len(hosts)-hostsCellMax) + " hosts"
		cv.Title = strings.Join(hosts, "\n")
	}
	return cv
}

// tlsCellView resolves an ingress TLS cell (SPEC §4.13): the green lock +
// "tls" ONLY when TLS is terminated (an EARNED green: live protection, D3),
// else the muted "—".
func tlsCellView(terminated bool) cellView {
	cv := cellView{Kind: cellTLS}
	if terminated {
		cv.Value = "tls"
		cv.Tone = "ok"
	}
	return cv
}

// lastRunCellView resolves a cronjob Last Schedule cell (SPEC §4.14): the
// age-bucket colour (the value is already a kubectl compressed duration) +
// a " ago" suffix; a cronjob that never ran -> the faint `<never>` (empty
// Value).
func lastRunCellView(value string) cellView {
	cv := cellView{Kind: cellLastRun}
	if value == "" {
		return cv
	}
	cv.Value = value + " ago"
	cv.Class = durationAgeClass(value)
	return cv
}

// keysCellView resolves a configmap/secret Data cell (SPEC §4.10): one
// `name · size` chip per key, the first keysCellMax shown, the rest behind the
// `+N keys` in-cell expand. The keyChipView carries ONLY the key name + byte
// size -- secret values are structurally absent from the view model. Empty
// data -> the muted "—".
func keysCellView(keys []keyChipView) cellView {
	return cellView{Kind: cellKeys, Keys: keys}
}

// countCellView resolves an events Count cell (SPEC §4.15): `×N` with a
// thousands separator; ≥20 reads chronic (the amber .restarts.some ink), a
// 0/1 count fades. The class strings are final span classes lifted from the
// reference countCell.
func countCellView(n int) cellView {
	cv := cellView{Kind: cellCount, Value: groupThousands(strconv.Itoa(n))}
	switch {
	case n >= 20:
		cv.Class = "restarts some"
	case n > 1:
		cv.Class = ""
	default:
		cv.Class = "faint"
	}
	return cv
}

// evObjCellView resolves an events Object cell (SPEC §4 evobj): the kind icon
// (pre-rendered in the bridge) + the faint "Kind/" prefix + the 20…8
// middle-truncated object name, full name in the tooltip when truncated
// (SPEC §4.2 -- the truncation rule beats the reference DOM, which dropped the
// tooltip).
func evObjCellView(kind, name string) cellView {
	cv := cellView{Kind: cellEvObj, Value: name, EvKind: kind, EvName: name}
	if display, truncated := MiddleTruncate(name, evObjNameMax, evObjLead, evObjTrail); truncated {
		cv.EvName = display
		cv.Title = name
	}
	return cv
}

// evAgeCellView resolves an events Age cell (SPEC §4 evage): the leading age
// token carries the age-bucket colour; any remainder ("(first 41h ago)")
// renders as the faint 11px second layer.
func evAgeCellView(value string) cellView {
	cv := cellView{Kind: cellEvAge}
	first, rest, _ := strings.Cut(strings.TrimSpace(value), " ")
	cv.Value = first
	cv.EvAgeRest = strings.TrimSpace(rest)
	if first != "" {
		cv.Class = durationAgeClass(first)
	}
	return cv
}

// msgCellView resolves an events Message cell (SPEC §4.16): the ONLY wrapping
// column in the system (td.ro-event-msg, max-width 520px in CSS). The value is
// plain text; templ escapes it at render.
func msgCellView(value string) cellView {
	return cellView{Kind: cellMsg, Value: value}
}

// secondaryTextColumns are the recognized k8s Table columns whose value is long
// free-text rather than an identifier -- they truncate with a `title=` tooltip
// (Principles §3: "secondary free-text — truncate WITH a tooltip", e.g. images,
// labels, selectors, node selectors, messages). The design keeps an ALLOW-LIST
// here, not the inverse, because identifiers are sacred: an unlisted column stays
// FULL and the table wrapper scrolls horizontally under the pinned name column
// (the design's escape valve), which is always safe -- whereas truncating by
// default would clip a short identifier/enum (Type, Cluster-IP, Port(s)) that must
// stay readable. Identifier columns (Name, Node, IP, Namespace, Ports, container
// names, counts) are deliberately never listed here.
var secondaryTextColumns = map[string]bool{
	"Image":         true,
	"Images":        true,
	"Selector":      true,
	"Node Selector": true,
	"Labels":        true,
	"Message":       true,
	"Reason":        true,
	"Data":          true,
	"Provider":      true,
	"Resources":     true,
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
	for _, key := range []string{"join", "sort", "customcols", "custom-columns", "hidecols", "hide-columns", "apiVersion", "api_version", "limit", "label-columns"} {
		if value := q.Get(key); value != "" {
			tv.HiddenInputs = append(tv.HiddenInputs, hiddenInput{Name: key, Value: value})
		}
	}
	return tv
}
