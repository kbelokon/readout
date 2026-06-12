package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/kube"
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

	// ColVis maps each table's plural to its column-visibility universe:
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
	// RESULT RECORDS (collected into the slot's errs), never task errors --
	// a failing cluster must still render partial results with the partial
	// notice. Results are merged AFTER the fan-out in fixed cluster order
	// (clusters is name-sorted by manager.Select) regardless of completion
	// order, replaying the exact sequential first-seen MergeTables so the
	// card/row order is deterministic and byte-identical to the former
	// sequential build.
	//
	// The total fan-out time budget wraps the request ctx HERE, in the caller,
	// so the helper stays mechanism-only: a dead or hung cluster trips the
	// deadline and its per-(cluster,type) fetch returns a deadline error that
	// lands in the slot's error lane (the same partial-failure path a 500
	// takes), so it can never hold the page until the client gives up. A
	// still-queued cluster starts with an already-expired ctx and fails
	// immediately the same way -- every cluster gets a slot, the page renders.
	fanoutCtx, cancel := context.WithTimeout(r.Context(), s.listBudget)
	defer cancel()
	slots := fanoutSlots(fanoutCtx, clusters, s.searchConcurrency(), func(ctx context.Context, cluster *kube.Cluster) clusterTableResult {
		return s.clusterTables(ctx, r, clients[cluster.Name], cluster, resourceTypes, namespace, isAllNamespaces)
	})

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
func (s *Server) clusterTables(ctx context.Context, r *http.Request, client *kube.Client, cluster *kube.Cluster, resourceTypes []string, namespace string, isAllNamespaces bool) clusterTableResult {
	var tables []kube.Table
	var errs []error
	colVis := map[string][]columnVis{}
	for _, typ := range resourceTypes {
		typ = strings.TrimSpace(typ)
		if typ == "" {
			continue
		}
		rt, err := client.FindResource(ctx, typ, namespace != "", apiVersionParam(r))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", cluster.Name, typ, err))
			continue
		}
		listNS := namespace
		if isAllNamespaces {
			listNS = ""
		}
		table, err := client.Table(ctx, &rt, kube.ListOptions{
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
// listContext shape the `_table` partial renders from: cluster tags,
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
	// Pure fan-out mechanics over the same bounded worker pool the list/search
	// assemblies use: per-cluster discovery is independent, per-cluster errors
	// are already ignored (a cluster that fails discovery contributes no
	// plurals), and the result is a set then sort -- so no time budget and no
	// slot ordering matter here. The request ctx is used as-is.
	perCluster := fanoutSlots(r.Context(), clusters, s.searchConcurrency(), func(ctx context.Context, cluster *kube.Cluster) []string {
		types, _ := clients[cluster.Name].NamespacedResourceTypes(ctx)
		plurals := make([]string, 0, len(types))
		for ti := range types {
			plurals = append(plurals, types[ti].Plural)
		}
		return plurals
	})
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
