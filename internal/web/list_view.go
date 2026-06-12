package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
)

// list_view.go assembles the render-ready listView from a decorated listContext
// and the request URL: it canonicalizes the URL, resolves every sort/metrics/
// filter href and per-row gesture target, builds the active-filter chips and the
// chips editor, and classifies a whole-list failure into its rendered state. No
// per-cell branching happens here -- buildCellView (list_cells.go) owns that.

// buildListView turns a listContext + the request URL into the render-ready
// listView, resolving every request-derived href and flag here so render never
// touches *http.Request.
func (s *Server) buildListView(r *http.Request, lc *listContext) listView {
	// Canonicalize the request URL FIRST (for state coherence): this builder
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
	// Cookie fill: with no ?sort= in the URL the persisted sort drives the
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

	// The single-type surface boundary: the v2 interaction loop (partial sort headers,
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
	// Bulk download surface: single-type AND single-cluster
	// lists get the clean `?download=yaml` base href baked onto the bulk bar;
	// multi-cluster scope leaves it empty, which renders the Download button
	// disabled with the explanatory title (the names grammar carries no
	// cluster segment, so a cross-cluster bulk URL would be ambiguous).
	if single && !lc.IsAllClusters && lc.ClusterCount == 1 {
		v.BulkDownloadHref = bulkDownloadHref(r.URL)
	}
	// The client-side stale path needs its markup hooks in the first server
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
	// The chips editor: single-type pages only, mirroring the `?f=` gate in
	// applyTableOptions. The chips ride the morphed fragment, so a shareable URL
	// lands with its chips visible and a chip-committing partial re-renders them.
	if single {
		v.FilterBar = &filterBarView{Plural: lc.Plural, Chips: buildFilterBarChips(r)}
	}

	for ti := range lc.Tables {
		table := &lc.Tables[ti]
		tv := tableView{
			Table:           *table,
			Kind:            pluralizeKind(table.Resource.Kind, table.Resource.Plural),
			DownloadTSVHref: downloadTSVHref(r.URL, table.Resource.Plural),
			SearchHref:      fmt.Sprintf("/search?cluster=%s&namespace=%s&type=%s", url.QueryEscape(strings.Join(table.Clusters, ",")), url.QueryEscape(lc.Namespace), url.QueryEscape(table.Resource.Plural)),
			Tools:           s.buildToolsView(r, table),
			Phase:           kube.PhaseSummary(table),
		}
		if (table.Resource.Plural == "pods" || table.Resource.Plural == "nodes") && joinValue == "" {
			tv.ShowMetricsHref = addQuery(r.URL, "join", "metrics")
		}
		// Column visibility: the synthetic Created column hides through a
		// render flag (it is not a kube column), and the popover universe rides
		// only on single-type pages -- the same single-type gate the loop, the chips
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
				// The row stripe (err/warn rows only) derives from the same
				// kube.StatusTone table the status dot uses, via RowStatusClass.
				StatusClass:  kube.RowStatusClass(table, row),
				Cluster:      row.Cluster,
				Namespace:    ns,
				CreatedClass: s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")),
				CreatedText:  formatTimestamp(nestedString(row.Object, "metadata", "creationTimestamp")),
			}
			if single {
				rv.Key = rowKey(row.Cluster, ns, name)
				// Per-row gesture targets: server-resolved hrefs
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
	// Whole-list failure state: a SINGLE-cluster list that produced no
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
// resource type -- the single-type surface boundary for the v2 interaction loop.
// Multi-type pages ("all", the "_all" union, and CSV lists) keep the v1
// behavior: boosted sort links, no row identity, the baked partial URL.
func isSingleListType(plural string) bool {
	return plural != "" && plural != "all" && plural != kube.AllNamespaces && !strings.Contains(plural, ",")
}

// rowKey is the stable row object identity "cluster/ns/name" with empty
// segments collapsed (a cluster-scoped object yields "cluster/name") -- the
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
	// Filters v2 chips: one removable chip per `?f=` param, single-type
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
// mono errdetail block, never a cute message). It returns nil
// for any other failure (a missing resource type, a 4xx Status such as a bad
// selector), so those keep the existing partial-error banner. The retry is the
// same list URL (a read-only GET); Back to clusters is /clusters.
func (s *Server) buildListState(r *http.Request, lc *listContext) *listStateView {
	err := lc.Errors[0]
	kind, ok := failureListState(kube.ClassifyError(err), err)
	if !ok {
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
	if kind == stateForbidden {
		state.Kind = stateForbidden
		state.Hint = forbiddenStateHint
		state.Detail = "403 Forbidden · " + err.Error()
	} else {
		state.Kind = stateUnreachable
		state.Hint = unreachableStateHint(kube.IsAPIStatusError(err))
		state.Detail = err.Error()
	}
	return state
}

// forbiddenStateHint is the one plain-language line of the forbidden state
// (the designed states copy); the verbatim 403 Status rides below it in
// the mono errdetail block.
const forbiddenStateHint = "Your credentials can browse this cluster, but RBAC denies this view."

// unreachableStateHint is the plain-language line of the unreachable state.
// The prototype copy ("the request never made it") is literal for a transport
// failure; an apiserver 5xx DID reach the apiserver, so it gets a truthful
// variant -- the verbatim Status message below carries the real detail either
// way (the verbatim-error law).
func unreachableStateHint(apiAnswered bool) string {
	if apiAnswered {
		return "The apiserver answered with an error."
	}
	return "The request never made it to the apiserver."
}
