package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/icons"
	"github.com/kbelokon/readout/internal/web/templates"
)

// templ_bridge.go is the seam between the package-web handler layer and the
// templ components (internal/web/templates). It (1) maps the request-derived
// layoutView onto the templ LayoutData and (2) renders the single templ page
// shell around a body component.
//
// There is exactly ONE shell. Leaf pages (error / clusters / cluster overview /
// resource-types) pass a real templ body component; the heavy pages
// (resource-list / view / logs / preferences) build their body as an HTML string
// and pass it via templ.Raw, so they ride the same templ layout without a
// parallel render path.

// renderLayout writes the full templ page document (shell + body) to w. The
// layoutView is mapped to templates.LayoutData here so the component package
// stays free of package-web types.
func (s *Server) renderLayout(w http.ResponseWriter, v *layoutView, body templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.Layout(toLayoutData(v), body).Render(context.Background(), w)
}

// toLayoutData maps the package-web layoutView onto the templ LayoutData (and
// its nested navbar/sidebar view models). It is a flat field copy: every value
// was already resolved in buildLayoutView.
func toLayoutData(v *layoutView) templates.LayoutData {
	navbar := toNavbar(&v.Navbar)
	// The mobile hamburger lives in the topbar but reveals the sidebar, so it must
	// render exactly when a sidebar does. ShowMenu is the sidebar-present signal
	// (set for any in-scope cluster, including the all-clusters list where
	// ShowContext is false); carry it onto the navbar so topbarC can gate the
	// .menu-toggle on it.
	navbar.ShowSidebar = v.Sidebar.ShowMenu
	return templates.LayoutData{
		Title:         v.Title,
		ThemeName:     v.ThemeName,
		ThemeExplicit: v.ThemeExplicit,
		Scripts:       v.Scripts,
		CSSHref:       v.CSSHref,
		IconURL:       v.IconURL,
		ExtraHead:     v.ExtraHead,
		Footer:        v.Footer,
		Navbar:        navbar,
		Sidebar:       toSidebar(v.Sidebar),
		// PaletteData is handed to templ.JSONScript verbatim; the package-web
		// paletteFeedView's json tags ARE the pinned wire contract, so the templ
		// component never re-declares the shape -- it just JSON-encodes this value.
		PaletteData: v.Palette,
	}
}

func toNavbar(n *navbarView) templates.Navbar {
	return templates.Navbar{
		ShowContext:     n.ShowContext,
		ContextName:     n.ContextName,
		NamespaceLinks:  toNavItems(n.NamespaceLinks),
		SearchCluster:   n.SearchCluster,
		SearchNamespace: n.SearchNamespace,
		NextTheme:       n.NextTheme,
		ToggleNextURL:   n.ToggleNextURL,
		ThemeExplicit:   n.ThemeExplicit,
	}
}

func toSidebar(s sidebarView) templates.Sidebar {
	out := templates.Sidebar{ShowMenu: s.ShowMenu, Meta: toNavItems(s.Meta)}
	for _, g := range s.Groups {
		out.Groups = append(out.Groups, templates.SidebarGroup{Label: g.Label, Links: toSidebarItems(g.Links)})
	}
	return out
}

func toNavItems(items []navItem) []templates.NavItem {
	if items == nil {
		return nil
	}
	out := make([]templates.NavItem, len(items))
	for i, it := range items {
		out[i] = templates.NavItem{Href: it.Href, Text: it.Text, Active: it.Active}
	}
	return out
}

// toSidebarItems maps the sidebar resource-type nav items, carrying the
// pre-rendered per-entry icon markup (resolved in the handler seam via the
// Unit-1 resolver) so the templ sidebar emits it raw -- the icon string is
// trusted-shape (icons.KindIcon / icons.PluralMonogram escape every
// runtime-derived part).
func toSidebarItems(items []navItem) []templates.NavItem {
	if items == nil {
		return nil
	}
	out := make([]templates.NavItem, len(items))
	for i, it := range items {
		out[i] = templates.NavItem{Href: it.Href, Text: it.Text, Active: it.Active, Icon: string(it.Icon)}
	}
	return out
}

// toListData maps the package-web listView onto the templ ListData. Every
// request-derived href/flag was already resolved in buildListView; this is a
// field copy plus the per-table/-cell branch translation and the pre-rendering
// of the inline-SVG icons + sort-direction icons into raw strings (templ emits
// those via templ.Raw so the icon markup stays byte-identical to icon()).
func toListData(v *listView) templates.ListData {
	d := templates.ListData{
		IsAllNamespaces:   v.IsAllNamespaces,
		Namespace:         v.Namespace,
		Plural:            v.Plural,
		ClusterCount:      v.ClusterCount,
		TableCount:        len(v.Tables),
		DurationSeconds:   v.Duration.Seconds(),
		AllNamespacesHref: v.AllNamespacesHref,
	}
	for _, err := range v.Errors {
		d.Errors = append(d.Errors, err.Error())
	}
	totalRows := 0
	for i := range v.Tables {
		totalRows += len(v.Tables[i].Table.Rows)
		d.Tables = append(d.Tables, toTableData(&v.Tables[i]))
	}
	d.TotalRows = totalRows
	d.ShowStaleBanner = v.StaleBanner
	if v.State != nil {
		d.State = toListState(v.State)
	}
	return d
}

// toListState maps the package-web whole-list failure state onto the templ
// ListState, pre-rendering the state glyph (lock for forbidden, unplug for
// unreachable) to a raw SVG string.
func toListState(s *listStateView) templates.ListState {
	kind, glyph := stateKindAndGlyph(s.Kind)
	return templates.ListState{
		Kind:      kind,
		Verb:      s.Verb,
		Resource:  s.Resource,
		Namespace: s.Namespace,
		Detail:    s.Detail,
		GlyphIcon: glyph,
		RetryHref: s.RetryHref,
		BackHref:  s.BackHref,
	}
}

// stateKindAndGlyph maps the package-web listStateKind to the templ kind string
// + its pre-rendered glyph (shared by the list + detail state mappers).
func stateKindAndGlyph(kind listStateKind) (string, string) {
	if kind == stateForbidden {
		return "forbidden", icon("lock")
	}
	return "unreachable", icon("unplug")
}

func toTableData(t *tableView) templates.TableData {
	td := templates.TableData{
		Kind:            t.Kind,
		Count:           len(t.Table.Rows),
		DownloadTSVHref: t.DownloadTSVHref,
		SearchHref:      t.SearchHref,
		DownloadIcon:    icon("download"),
		ToolsIcon:       icon("caret-square-down"),
		SearchIcon:      icon("search"),
		Tools:           toTableTools(&t.Tools),
		ShowMetricsHref: t.ShowMetricsHref,
		PhaseRows:       len(t.Table.Rows),
		MultiCluster:    len(t.Table.Clusters) > 1,
		ColumnCount:     len(t.Table.Columns),
		CreatedHref:     t.CreatedHref,
		CreatedIcon:     t.CreatedIcon,
		CreatedSorted:   t.CreatedIcon != "",
		EmptyKind:       t.Table.Resource.Kind,
		EmptyGlyph:      icon("inbox"),
		ClearHref:       t.ClearHref,
	}
	if t.EmptyAction != nil {
		td.EmptyActionHref = t.EmptyAction.Href
		td.EmptyActionLabel = t.EmptyAction.Label
	}
	for _, chip := range t.EmptyFilters {
		td.EmptyFilters = append(td.EmptyFilters, templates.FilterChip{Label: chip.Label, RemoveHref: chip.RemoveHref})
	}
	for _, item := range t.Phase {
		td.Phase = append(td.Phase, templates.PhaseChip{
			Tone:  statusTone(item.Class),
			Label: item.Label,
			Count: strconv.Itoa(item.Count),
		})
	}
	for i, col := range t.Table.Columns {
		td.Columns = append(td.Columns, templates.TableColumn{
			Description: col.Description,
			Class:       col.Class,
			SortHref:    t.Columns[i].SortHref,
			Name:        col.Name,
			SortIcon:    t.Columns[i].SortIcon,
			Sorted:      t.Columns[i].SortIcon != "",
		})
	}
	for i := range t.Rows {
		row := &t.Rows[i]
		tr := templates.TableRow{
			StatusClass:  row.StatusClass,
			ClusterHref:  row.ClusterHref,
			Cluster:      row.Cluster,
			NsHref:       row.NsHref,
			Namespace:    row.Namespace,
			CreatedClass: row.CreatedClass,
			CreatedText:  row.CreatedText,
		}
		if row.CreatedText != "" {
			tr.CreatedTitle = "created " + row.CreatedText
		}
		for ci := range row.Cells {
			cell := &row.Cells[ci]
			tc := templates.TableCell{
				Kind:         templates.CellKind(cell.Kind),
				Value:        cell.Value,
				Class:        cell.Class,
				ColClass:     cell.ColClass,
				Href:         cell.Href,
				Tone:         cell.Tone,
				Ratio:        cell.Ratio,
				Pulse:        cell.Pulse,
				NameHead:     cell.NameHead,
				NameTail:     cell.NameTail,
				Ago:          cell.Ago,
				Trunc:        cell.Trunc,
				Title:        cell.Title,
				CapBucket:    cell.CapBucket,
				CapPct:       cell.CapPct,
				CapBar:       cell.CapBar,
				Roles:        cell.Roles,
				RepNum:       cell.RepNum,
				RolloutState: cell.RolloutState,
			}
			for _, cond := range cell.Conds {
				tc.Conds = append(tc.Conds, templates.Cond{Name: cond.Name, Tone: cond.Tone})
			}
			for _, seg := range cell.RepSegments {
				tc.RepSegments = append(tc.RepSegments, templates.RepSegment{State: seg.State})
			}
			for _, chip := range cell.Chips {
				tc.Chips = append(tc.Chips, templates.RowChip{Class: chip.Class, Text: chip.Text})
			}
			if cell.Kind == cellRollout {
				tc.RolloutIcon = icon(rolloutIconName(cell.RolloutState))
			}
			tr.Cells = append(tr.Cells, tc)
		}
		td.Rows = append(td.Rows, tr)
	}
	return td
}

// toListPageData maps the listView + the request-derived partial URL onto the
// full-page templ ListPageData (breadcrumb + the htmx container's `_table` URL
// + the table fragment).
func toListPageData(v *listView, partialURL string) templates.ListPageData {
	return templates.ListPageData{
		Breadcrumb: templates.ListBreadcrumb{
			IsAllClusters:   v.IsAllClusters,
			Cluster:         v.Cluster,
			ClusterHref:     "/clusters/" + url.PathEscape(v.Cluster),
			Namespace:       v.Namespace,
			NamespaceHref:   "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces/" + url.PathEscape(v.Namespace),
			IsAllNamespaces: v.IsAllNamespaces,
			AllNamespaces:   "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces",
			Plural:          v.Plural,
		},
		PartialURL: partialURL,
		List:       toListData(v),
	}
}

// toDetailData maps the package-web detailView onto the templ DetailData.
// Every field was resolved in buildDetailView; this is a field copy plus the
// breadcrumb/links/owners/node/secret/cards/subtable/events translation and the
// pre-rendering of inline-SVG icons into raw strings.
func toDetailData(v *detailView) templates.DetailData {
	if v.State != nil {
		// State path: the fetch failed, so there is no object -- the breadcrumb is
		// built from the request path the state carries and the body renders the
		// forbidden/unreachable card. No object field is dereferenced.
		kind, glyph := stateKindAndGlyph(v.State.Kind)
		return templates.DetailData{
			Breadcrumb: detailStateBreadcrumb(v),
			Name:       v.State.Name,
			State: templates.DetailState{
				Kind:      kind,
				Verb:      v.State.Verb,
				Resource:  v.State.Resource,
				Name:      v.State.Name,
				Namespace: v.State.Namespace,
				Detail:    v.State.Detail,
				GlyphIcon: glyph,
				RetryHref: v.State.RetryHref,
				BackHref:  v.State.BackHref,
			},
		}
	}
	object := v.Object
	d := templates.DetailData{
		Breadcrumb:         toDetailBreadcrumb(v),
		Name:               object.Name(),
		Kind:               object.Kind(),
		DownloadHref:       v.DownloadHref,
		DownloadIcon:       icon("download"),
		CreatedMeta:        v.CreatedMeta,
		Version:            v.Version,
		DefaultTabActive:   v.DefaultTab,
		YAMLTabActive:      v.YAMLTab,
		EventsTabActive:    v.EventsTab,
		LogsHref:           v.LogsHref,
		IsYAMLView:         v.IsYAMLView,
		IsEventsView:       v.IsEventsView,
		HighlightedYAML:    v.HighlightedYAML,
		ShowNamespaceLinks: v.ShowNamespaceLinks,
		AllObjectsHref:     v.AllObjectsHref,
		ResourceTypesHref:  v.ResourceTypesHref,
	}
	for _, link := range v.Links {
		d.Links = append(d.Links, templates.DetailLink{Href: link.Href, Title: link.Title, Icon: icon(link.Icon)})
	}
	for _, chip := range v.Labels {
		d.Labels = append(d.Labels, templates.DetailLabelChip{Href: chip.Href, Class: chip.Class, Key: chip.Key, Val: chip.Val})
	}
	for _, chip := range v.Annotations {
		d.Annotations = append(d.Annotations, templates.AnnotationChip{Key: chip.Key, Val: chip.Val, Full: chip.Full})
	}
	if v.Node != nil {
		d.Node = toNodeSummary(*v.Node)
	}
	for _, owner := range v.Owners {
		kind, name := splitOwnerTitle(owner.Title)
		if kind != "" && name != "" {
			d.Owners = append(d.Owners, templates.OwnerLink{Href: owner.Href, Kind: kind, Name: name, Split: true})
		} else {
			d.Owners = append(d.Owners, templates.OwnerLink{Href: owner.Href, Title: owner.Title})
		}
	}
	if v.Secret != nil {
		d.Secret = &templates.SecretData{KeyCount: v.Secret.KeyCount, Keys: v.Secret.Keys}
	}
	for _, card := range v.YAMLCards {
		d.YAMLCards = append(d.YAMLCards, templates.YAMLCard{Name: card.Name, Title: card.Title, CopyIcon: icon("copy"), Content: card.Content})
	}
	if v.RelatedPods != nil {
		d.RelatedPods = toSubtable(v.RelatedPods)
	}
	for i := range v.Events {
		ev := &v.Events[i]
		d.Events = append(d.Events, templates.EventRow{Type: ev.Type, Tone: ev.Tone, Reason: ev.Reason, Age: ev.Age, AgeClass: ev.AgeClass, From: ev.From, Message: ev.Message})
	}
	return d
}

func toDetailBreadcrumb(v *detailView) templates.DetailBreadcrumb {
	return objectBreadcrumb(v.Cluster, v.Namespace, &v.Object)
}

// detailStateBreadcrumb builds the resource-view breadcrumb for a failure state
// from the request-path fields the state carries (no fetched object exists -- the
// fetch is what failed). It mirrors objectBreadcrumb's cluster/namespace/plural/
// name crumbs.
func detailStateBreadcrumb(v *detailView) templates.DetailBreadcrumb {
	s := v.State
	b := templates.DetailBreadcrumb{
		ClusterHref: "/clusters/" + url.PathEscape(v.Cluster),
		Cluster:     v.Cluster,
		Name:        s.Name,
	}
	if s.Namespace != "" && s.Resource != "namespaces" {
		b.ShowNamespace = true
		b.NamespaceHref = "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces/" + url.PathEscape(s.Namespace)
		b.Namespace = s.Namespace
		b.PluralHref = "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces/" + url.PathEscape(s.Namespace) + "/" + url.PathEscape(s.Resource)
	} else {
		b.PluralHref = "/clusters/" + url.PathEscape(v.Cluster) + "/" + url.PathEscape(s.Resource)
	}
	b.Plural = s.Resource
	return b
}

// objectBreadcrumb builds the object breadcrumb (cluster [+ namespace + plural]
// + the active object name) shared by the resource-view and logs pages.
func objectBreadcrumb(cluster, namespace string, object *kube.Object) templates.DetailBreadcrumb {
	b := templates.DetailBreadcrumb{
		ClusterHref: "/clusters/" + url.PathEscape(cluster),
		Cluster:     cluster,
		Name:        object.Name(),
	}
	if namespace != "" && object.Kind() != "Namespace" {
		b.ShowNamespace = true
		b.NamespaceHref = "/clusters/" + url.PathEscape(cluster) + "/namespaces/" + url.PathEscape(namespace)
		b.Namespace = namespace
		b.PluralHref = "/clusters/" + url.PathEscape(cluster) + "/namespaces/" + url.PathEscape(namespace) + "/" + url.PathEscape(object.Resource.Plural)
		b.Plural = object.Resource.Plural
	} else {
		b.PluralHref = "/clusters/" + url.PathEscape(cluster) + "/" + url.PathEscape(object.Resource.Plural)
		b.Plural = object.Resource.Plural
	}
	return b
}

func toNodeSummary(n nodeSummaryView) *templates.NodeSummary {
	out := &templates.NodeSummary{HasCapAlloc: n.HasCapAlloc}
	for _, cond := range n.Conditions {
		out.Conditions = append(out.Conditions, templates.NodeCondition{Tone: cond.Tone, Title: cond.Title, Type: cond.Type, Value: cond.Value})
	}
	out.Capacity = toKVList(n.Capacity)
	out.Allocatable = toKVList(n.Allocatable)
	out.NodeInfo = toKVList(n.NodeInfo)
	return out
}

func toKVList(l *kvListView) *templates.KVList {
	if l == nil {
		return nil
	}
	out := &templates.KVList{}
	for _, row := range l.Rows {
		out.Rows = append(out.Rows, templates.KVRow{Key: row.Key, Val: row.Val})
	}
	return out
}

func toSubtable(v *subtableView) *templates.Subtable {
	emptyColspan := len(v.Table.Columns) + 1
	// The empty-state sentence is precomputed as one trusted string (kind
	// html-escaped + the namespace clause), matching the former renderSubtable
	// `No %s objects %sfound.`; it emits via a single @templ.Raw (a text node
	// after @templ.Raw would be parsed as that call's dropped children).
	emptyTitle := "No " + html.EscapeString(v.Table.Resource.Kind) + " objects " + namespaceEmptyText(v.Namespace, v.Namespace == kube.AllNamespaces) + "found."
	st := &templates.Subtable{
		ShowNamespace: v.Namespace == "",
		CreatedHref:   v.CreatedHref,
		EmptyColspan:  emptyColspan,
		EmptyTitle:    emptyTitle,
	}
	for _, col := range v.Columns {
		st.Columns = append(st.Columns, templates.SubtableColumn{Description: col.Description, SortHref: col.SortHref, Name: col.Name})
	}
	for _, row := range v.Rows {
		sr := templates.SubtableRow{
			StatusClass:  row.StatusClass,
			ShowNs:       row.ShowNs,
			NsHref:       row.NsHref,
			Namespace:    row.Namespace,
			CreatedClass: row.CreatedClass,
			CreatedText:  row.CreatedText,
		}
		for _, cell := range row.Cells {
			sr.Cells = append(sr.Cells, templates.SubtableCell{Kind: templates.CellKind(cell.Kind), Value: cell.Value, Class: cell.Class, Href: cell.Href})
		}
		st.Rows = append(st.Rows, sr)
	}
	return st
}

// toSearchData maps the package-web searchView onto the redesign templ
// SearchData: the breadcrumb branches, the form round-trip (query + hidden
// cluster/namespace/type inputs), the scope-opts labels, the partial-failure
// banner, the per-cluster `.ro-scope-chip.ok|err` chips (with the read-only retry
// hrefs), the results-table rows (with the resolved kind icon + the age-bucket
// cell), and the foundline. Every value was resolved in buildSearchView; the
// search + kind icons are pre-rendered to raw strings.
func toSearchData(v *searchView) templates.SearchData {
	hasQuery := v.Query != ""
	offeredCount := len(v.OfferedTypes)
	allTypes := offeredCount > 0 && v.SelectedTypeCount >= offeredCount
	d := templates.SearchData{
		Breadcrumb:        toSearchBreadcrumb(v),
		Query:             v.Query,
		Cluster:           v.Cluster,
		Namespace:         v.Namespace,
		SearchIcon:        icon("search"),
		HasQuery:          hasQuery,
		HiddenTypes:       v.SelectedTypes,
		ScopeClusterLabel: searchScopeClusterLabel(v),
		NamespaceLabel:    searchNamespaceLabel(v),
		TypeLabel:         searchTypeLabel(allTypes, v.SelectedTypeCount),
		Banner:            searchBanner(v),
	}
	for _, c := range v.ScopeClusters {
		d.ScopeClusters = append(d.ScopeClusters, templates.SearchScopeChip{
			Name:      c.Name,
			Failed:    c.Failed,
			Count:     c.ResultCount,
			Reason:    c.Reason,
			RetryHref: c.RetryHref,
		})
	}
	for i := range v.Results {
		res := &v.Results[i]
		d.Results = append(d.Results, templates.SearchResultRow{
			Cluster:     res.Cluster,
			ClusterHref: "/clusters/" + url.PathEscape(res.Cluster),
			Namespace:   res.Namespace,
			NsHref:      "/clusters/" + url.PathEscape(res.Cluster) + "/namespaces/" + url.PathEscape(res.Namespace),
			HasNs:       res.Namespace != "",
			Kind:        res.Kind,
			KindIcon:    string(icons.KindIcon(res.Kind, res.Group, res.IsCRD, "")),
			Name:        res.Title,
			Link:        res.Link,
			Age:         res.Created,
			AgeClass:    res.AgeClass,
		})
	}
	if hasQuery {
		d.ResultSummary = searchFoundLine(v)
	}
	return d
}

// searchScopeClusterLabel is the scope-opts cluster fragment: "all clusters" for
// an all-clusters search with no resolved scope chips, the single cluster name
// when exactly one was searched, else "N clusters".
func searchScopeClusterLabel(v *searchView) string {
	switch {
	case v.IsAllClusters && len(v.ScopeClusters) == 0:
		return "all clusters"
	case len(v.ScopeClusters) == 1:
		return v.ScopeClusters[0].Name
	default:
		return fmt.Sprintf("%d clusters", len(v.ScopeClusters))
	}
}

// searchNamespaceLabel is the scope-opts namespace fragment ("all namespaces" or
// the single namespace name).
func searchNamespaceLabel(v *searchView) string {
	if v.IsAllNamespaces || v.Namespace == "" {
		return "all namespaces"
	}
	return v.Namespace
}

// searchTypeLabel is the scope-opts resource-type fragment ("all resource types"
// when the selection covers every offered type, else "N resource types").
func searchTypeLabel(allTypes bool, count int) string {
	if allTypes {
		return "all resource types"
	}
	return fmt.Sprintf("%d resource types", count)
}

// searchBanner builds the multi-cluster partial-failure banner (D11), shown only
// when at least one cluster failed to answer: the "Searched N of M clusters — K
// didn't respond" title, a detail line naming the failed clusters + their
// reason, and the read-only "Retry failed" GET href. The all-cluster LIST banner
// (Unit 5) is a DIFFERENT screen; this is the search flavour.
func searchBanner(v *searchView) templates.SearchBanner {
	failed := 0
	var detail []string
	for _, c := range v.ScopeClusters {
		if c.Failed {
			failed++
			detail = append(detail, c.Name+" ("+c.Reason+")")
		}
	}
	if failed == 0 {
		return templates.SearchBanner{}
	}
	total := len(v.ScopeClusters)
	answered := total - failed
	return templates.SearchBanner{
		Show:      true,
		Title:     fmt.Sprintf("Searched %d of %d clusters — %d didn't respond", answered, total, failed),
		Detail:    strings.Join(detail, ", ") + " — results below are from the clusters that answered.",
		RetryHref: v.RetryFailedHref,
	}
}

// toSearchBreadcrumb resolves the search-page breadcrumb branches (all / cluster
// / namespace).
func toSearchBreadcrumb(v *searchView) templates.SearchBreadcrumb {
	b := templates.SearchBreadcrumb{IsAllClusters: v.IsAllClusters}
	if v.IsAllClusters || v.Cluster == "" {
		b.IsAllClusters = true
		return b
	}
	b.Cluster = v.Cluster
	b.ClusterHref = "/clusters/" + url.PathEscape(v.Cluster)
	b.AllNamespaces = "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces"
	if v.IsAllNamespaces {
		b.ShowAllNamespace = true
	} else if v.Namespace != "" {
		b.ShowNamespace = true
		b.Namespace = v.Namespace
		b.NamespaceHref = "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces/" + url.PathEscape(v.Namespace)
	}
	return b
}

// searchFoundLine is the redesign `.ro-foundline` footer sentence ("Found N
// objects matching "<q>" across A of M clusters in T seconds[ · K cluster(s)
// failed].") with object/cluster pluralization, %.3f timing, and the trailing
// failed-cluster clause appended only when a cluster did not answer.
func searchFoundLine(v *searchView) string {
	total := len(v.ScopeClusters)
	failed := 0
	for _, c := range v.ScopeClusters {
		if c.Failed {
			failed++
		}
	}
	answered := total - failed
	line := fmt.Sprintf("Found %d object%s matching %q across %d of %d cluster%s in %.3f seconds",
		len(v.Results), pluralS(len(v.Results)), v.Query, answered, total, pluralS(total), v.Duration.Seconds())
	if failed > 0 {
		line += fmt.Sprintf(" · %d cluster%s failed", failed, pluralS(failed))
	}
	return line + "."
}

func toTableTools(t *toolsView) templates.TableTools {
	tt := templates.TableTools{
		Active:       t.Active,
		LabelColsVal: t.LabelColsVal,
		SelectorVal:  t.SelectorVal,
		FilterVal:    t.FilterVal,
		TableIcon:    icon("table"),
		TagsIcon:     icon("tags"),
		FilterIcon:   icon("filter"),
	}
	for _, in := range t.HiddenInputs {
		tt.HiddenInputs = append(tt.HiddenInputs, templates.HiddenInput{Name: in.Name, Value: in.Value})
	}
	return tt
}
