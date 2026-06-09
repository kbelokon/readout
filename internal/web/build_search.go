package web

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// build_search.go is the data-assembly layer for the search page: it turns the
// kube client + parsed request inputs into the searchView the search.templ
// component consumes -- the offered resource-type checkbox set, the per-(type,
// cluster) search results, the per-cluster scope chips (each carrying its own
// failure status/reason/retry href), and the result count. The per-cluster search
// fans out concurrently (errgroup + SearchMaxConcurrency); the slots are merged in
// fixed cluster order so output is deterministic regardless of completion order.

// searchDefaultResourceTypes are the resource types searched when the request
// carries no explicit ?type=. ReplicaSet/DaemonSet/Pod/Node are intentionally
// NOT default.
var searchDefaultResourceTypes = []string{"namespaces", "deployments", "services", "ingresses", "statefulsets", "cronjobs"}

// searchOfferedResourceTypes are the resource types offered as checkboxes on the
// search form.
var searchOfferedResourceTypes = []string{"namespaces", "deployments", "replicasets", "services", "ingresses", "daemonsets", "statefulsets", "cronjobs", "pods", "nodes"}

func (s *Server) buildSearchView(r *http.Request) (searchView, requestKubeClients, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	clusterParam := strings.Join(r.URL.Query()["cluster"], ",")
	namespaces := searchScopeValues(r.URL.Query()["namespace"])
	namespace := strings.Join(namespaces, ",")
	clusters, allClusters, err := s.manager.Select(clusterParam)
	if err != nil {
		return searchView{}, nil, err
	}
	clients := s.kubeClients(r, clusters)
	selector, filterQuery := splitSearchQuery(q)
	if extra := r.URL.Query().Get("selector"); extra != "" {
		if selector != "" {
			selector += ","
		}
		selector += extra
	}
	types := r.URL.Query()["type"]
	if len(types) == 0 {
		types = firstSlice(s.cfg.SearchDefaultResourceTypes, searchDefaultResourceTypes)
	}
	isAllNamespaces := len(namespaces) == 0 || (len(namespaces) == 1 && namespaces[0] == kube.AllNamespaces)
	shellCluster := ""
	if !allClusters && len(clusters) == 1 {
		shellCluster = clusters[0].Name
	}
	shellNamespace := ""
	if len(namespaces) == 1 && namespaces[0] != kube.AllNamespaces {
		shellNamespace = namespaces[0]
	}
	start := s.clock()

	view := searchView{
		Query:             q,
		Cluster:           clusterParam,
		Namespace:         namespace,
		ShellCluster:      shellCluster,
		ShellNamespace:    shellNamespace,
		IsAllClusters:     allClusters,
		IsAllNamespaces:   isAllNamespaces,
		SelectedTypeCount: len(types),
		SelectedTypes:     types,
	}
	for _, cluster := range clusters {
		view.ScopeClusters = append(view.ScopeClusters, searchScopeCluster{Name: cluster.Name})
	}

	// searchable: plural -> Kind, accumulated as types resolve against the
	// clusters. It seeds the checkbox set.
	searchable := map[string]string{}

	// Fan the per-cluster search out concurrently, bounded by
	// SearchMaxConcurrency. Each cluster searches all requested types into its
	// own ordered slot; expected per-(cluster,type) failures are RESULT RECORDS
	// (searchErrorRecord), never errgroup errors -- a failing cluster still renders
	// partial results. After Wait the slots are merged in fixed cluster order
	// (clusters is name-sorted by manager.Select) regardless of completion order
	// so Results, the per-cluster ScopeClusters status, and the searchable
	// first-wins set are all deterministic; the final sortResults gives Results
	// total order.
	slots := make([]clusterSearchResult, len(clusters))
	g, _ := errgroup.WithContext(r.Context())
	g.SetLimit(s.searchConcurrency())
	for i, cluster := range clusters {
		i, cluster := i, cluster
		g.Go(func() error {
			slots[i] = s.clusterSearch(r, clients[cluster.Name], cluster, types, namespaces, selector, filterQuery, isAllNamespaces)
			return nil
		})
	}
	_ = g.Wait()

	var failedClusters []string
	for i, slot := range slots {
		view.Results = append(view.Results, slot.results...)
		for _, sc := range slot.searchable {
			if _, ok := searchable[sc.plural]; !ok {
				searchable[sc.plural] = sc.kind
			}
		}
		// Per-cluster scope-chip status (D11): the chip is `.err` when the cluster
		// produced any error record (it failed to fully answer) and `.ok`
		// otherwise; the result count rides on the `.ok` chip. The RetryHref re-runs
		// the SAME search scoped to just this cluster -- a read-only GET, never a
		// write path -- so a failed cluster can be retried in place. ScopeClusters
		// is index-aligned with slots (both follow the name-sorted clusters slice).
		view.ScopeClusters[i].ResultCount = len(slot.results)
		if len(slot.errs) > 0 {
			view.ScopeClusters[i].Failed = true
			view.ScopeClusters[i].Reason = searchScopeReason(slot.errs)
			view.ScopeClusters[i].RetryHref = addQuery(r.URL, "cluster", clusters[i].Name)
			failedClusters = append(failedClusters, clusters[i].Name)
		}
	}
	// RetryFailedHref re-runs the SAME search scoped to the comma-joined set of
	// failed clusters (a read-only GET; manager.Select parses the CSV). The
	// partial-failure banner's "Retry failed" action uses it; empty when nothing
	// failed so the banner is hidden.
	if len(failedClusters) > 0 {
		view.RetryFailedHref = addQuery(r.URL, "cluster", strings.Join(failedClusters, ","))
	}

	// Cluster hits: when searching all clusters, a cluster whose name or any
	// label value contains the query becomes a result card.
	if q != "" && allClusters {
		needle := strings.ToLower(q)
		for _, cluster := range s.manager.Clusters() {
			if clusterMatches(cluster, needle) {
				view.Results = append(view.Results, searchResult{
					Title:   cluster.Name,
					Kind:    "Cluster",
					Link:    "/clusters/" + url.PathEscape(cluster.Name),
					Cluster: cluster.Name,
					Labels:  cluster.Labels,
				})
			}
		}
	}

	// Resolve any offered type not already searched, so the checkbox list shows
	// the full offered set.
	offered := firstSlice(s.cfg.SearchOfferedResourceTypes, searchOfferedResourceTypes)
	for _, typ := range offered {
		if _, ok := searchable[typ]; ok {
			continue
		}
		for _, cluster := range clusters {
			if rt, _, err := findSearchResource(r, clients[cluster.Name], typ); err == nil {
				searchable[rt.Plural] = rt.Kind
				break
			}
		}
	}

	view.OfferedTypes = buildTypeOptions(searchable, types)
	sortResults(view.Results, q)
	view.Duration = s.clock().Sub(start)
	return view, clients, nil
}

// clusterSearchResult is one cluster's fan-out slot: its ordered result cards,
// its searchable plural->Kind contributions in first-seen-within-cluster order,
// and its per-type error records. The caller merges slots across clusters in
// fixed cluster order so Results/searchable/scope status stay deterministic.
type clusterSearchResult struct {
	results    []searchResult
	searchable []searchableType
	errs       []searchErrorRecord
}

type searchableType struct {
	plural string
	kind   string
}

type searchErrorRecord struct {
	cluster      string
	resourceType string
	err          error
}

// clusterSearch searches all requested types against one cluster: resolve type
// -> contribute to the searchable set -> when a query is present, Table +
// namespace filter + text filter -> result cards. Per-type failures are
// collected as error records, not raised, so the per-cluster work can run as a
// single fan-out task.
func (s *Server) clusterSearch(r *http.Request, client *kube.Client, cluster *kube.Cluster, types []string, namespaces []string, selector, filterQuery string, isAllNamespaces bool) clusterSearchResult {
	var out clusterSearchResult
	seen := map[string]bool{}
	for _, typ := range types {
		rt, namespaced, err := findSearchResource(r, client, typ)
		if err != nil {
			out.errs = append(out.errs, searchErrorRecord{cluster: cluster.Name, resourceType: typ, err: err})
			continue
		}
		if !seen[rt.Plural] {
			seen[rt.Plural] = true
			out.searchable = append(out.searchable, searchableType{plural: rt.Plural, kind: rt.Kind})
		}
		// Without a search query, only resolve the type: the list is fetched
		// only when there is a selector or filter.
		if selector == "" && filterQuery == "" {
			continue
		}
		listNamespaces := []string{""}
		if !isAllNamespaces && namespaced {
			listNamespaces = namespaces
		}
		for _, listNS := range listNamespaces {
			table, err := client.Table(r.Context(), &rt, kube.ListOptions{Namespace: listNS, LabelSelector: selector})
			if err != nil {
				out.errs = append(out.errs, searchErrorRecord{cluster: cluster.Name, resourceType: typ, err: err})
				continue
			}
			// Respect --include-namespaces/--exclude-namespaces (the same as the
			// list path): drop rows from disallowed namespaces BEFORE the text
			// filter / label columns / result assembly. No-op under default config
			// (both sets empty).
			kube.FilterSearchRowsByNamespace(&table, s.cfg.IncludeNamespaces, s.cfg.ExcludeNamespaces)
			if filterQuery != "" {
				kube.FilterTable(&table, filterQuery, true)
				kube.AddLabelColumns(&table, "*")
			}
			nameIdx := nameColumn(&table)
			for _, row := range table.Rows {
				name := cellString(row, nameIdx)
				ns := nestedString(row.Object, "metadata", "namespace")
				link := resourceHref(cluster.Name, &rt, ns, name)
				labels, _, _ := unstructured.NestedStringMap(row.Object, "metadata", "labels")
				created := nestedString(row.Object, "metadata", "creationTimestamp")
				out.results = append(out.results, searchResult{
					Title:     name,
					Kind:      rt.Kind,
					Group:     apiGroup(rt.APIVersion),
					IsCRD:     isCRD(rt.APIVersion),
					Link:      link,
					Cluster:   cluster.Name,
					Namespace: ns,
					Labels:    labels,
					Created:   formatTimestamp(created),
					AgeClass:  "num " + s.ageClass(created),
				})
			}
		}
	}
	return out
}

func searchScopeValues(raw []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

// findSearchResource resolves a plural to a ResourceType, trying namespaced
// first then cluster-scoped. The bool reports whether the resolved type is
// namespaced.
func findSearchResource(r *http.Request, client *kube.Client, typ string) (kube.ResourceType, bool, error) {
	rt, err := client.FindResource(r.Context(), typ, true, "")
	if err == nil {
		return rt, true, nil
	}
	rt, err = client.FindResource(r.Context(), typ, false, "")
	if err != nil {
		return kube.ResourceType{}, false, err
	}
	return rt, false, nil
}

// searchScopeReason condenses a failed cluster's error records into the short
// label shown on the `.ro-scope-chip.err` chip (the full per-error detail rides
// in the `.ro-banner.warn` summary). It classifies the FIRST error: a deadline/
// timeout reads as "timeout", a connection/no-route/refused error as
// "unreachable", a 403 as "forbidden", else a generic "failed". The classifier is
// substring-based over the error string (the apiserver/transport error text),
// kept deliberately small.
func searchScopeReason(errs []searchErrorRecord) string {
	if len(errs) == 0 {
		return "failed"
	}
	msg := strings.ToLower(errs[0].err.Error())
	switch {
	case strings.Contains(msg, "deadline") || strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "no route"):
		return "unreachable"
	case strings.Contains(msg, "forbidden"):
		return "forbidden"
	default:
		return "failed"
	}
}

// clusterMatches reports whether a cluster's name or any label value contains
// the (already lower-cased) needle.
func clusterMatches(cluster *kube.Cluster, needle string) bool {
	if strings.Contains(strings.ToLower(cluster.Name), needle) {
		return true
	}
	for _, val := range cluster.Labels {
		if strings.Contains(strings.ToLower(val), needle) {
			return true
		}
	}
	return false
}

// buildTypeOptions turns the searchable plural->Kind map into the checkbox
// options, sorted by plural; Checked marks the plurals in the current ?type=
// selection.
func buildTypeOptions(searchable map[string]string, selected []string) []searchTypeOption {
	selectedSet := make(map[string]bool, len(selected))
	for _, typ := range selected {
		selectedSet[typ] = true
	}
	plurals := make([]string, 0, len(searchable))
	for plural := range searchable {
		plurals = append(plurals, plural)
	}
	sort.Strings(plurals)
	options := make([]searchTypeOption, 0, len(plurals))
	for _, plural := range plurals {
		options = append(options, searchTypeOption{
			Plural:  plural,
			Kind:    searchable[plural],
			Checked: selectedSet[plural],
		})
	}
	return options
}

func splitSearchQuery(q string) (selector string, filter string) {
	var selectors, filters []string
	for _, word := range strings.Fields(q) {
		if strings.Contains(word, "=") {
			selectors = append(selectors, word)
		} else {
			filters = append(filters, word)
		}
	}
	return strings.Join(selectors, ","), strings.Join(filters, " ")
}

// sortResults orders search results by score DESC, then Title ASC, then Kind
// ASC, then Link ASC. The Kind tiebreak (between Title and Link) is
// load-bearing: for equal-name/equal-score hits (e.g. the three exact
// "redpanda" matches) it orders Namespace < Service < StatefulSet.
func sortResults(results []searchResult, query string) {
	sort.SliceStable(results, func(i, j int) bool {
		scoreI := searchScore(results[i].Title, results[i].Labels, query)
		scoreJ := searchScore(results[j].Title, results[j].Labels, query)
		if scoreI != scoreJ {
			return scoreI > scoreJ
		}
		if results[i].Title != results[j].Title {
			return results[i].Title < results[j].Title
		}
		if results[i].Kind != results[j].Kind {
			return results[i].Kind < results[j].Kind
		}
		return results[i].Link < results[j].Link
	})
}

// searchScore is the result rank: +10 when the (lowercased) query equals the
// title, else +2 when it is a substring of the title, plus +1 ONCE if the query
// is one of the label VALUES. The label check compares the RAW (case-sensitive)
// label values against the LOWERCASED query, added a single time: it breaks
// after the first hit (adding +1 once, never once-per-matching-label).
func searchScore(title string, labels map[string]string, query string) int {
	score := 0
	query = strings.ToLower(query)
	lowerTitle := strings.ToLower(title)
	if query != "" && lowerTitle == query {
		score += 10
	} else if query != "" && strings.Contains(lowerTitle, query) {
		score += 2
	}
	for _, value := range labels {
		if strings.ToLower(value) == query {
			score++
			break
		}
	}
	return score
}

// matchLabels extracts a controller's pod selector from spec.selector. The
// selector is read off varying controller kinds, so this is a PARTIAL typed
// read: a standard metav1.LabelSelector pulls the matchLabels of
// Deployment/ReplicaSet/StatefulSet/DaemonSet, and the bare map[string]string
// form (Service-style spec.selector, no matchLabels wrapper) is read via the
// apimachinery accessor as a fallback.
func matchLabels(obj map[string]any) map[string]string {
	raw, ok, _ := unstructured.NestedMap(obj, "spec", "selector")
	if !ok {
		return map[string]string{}
	}
	var selector metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &selector); err == nil && len(selector.MatchLabels) > 0 {
		return selector.MatchLabels
	}
	labels, _, _ := unstructured.NestedStringMap(obj, "spec", "selector")
	return labels
}

func selectorString(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}
