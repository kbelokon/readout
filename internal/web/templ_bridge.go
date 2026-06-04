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
	return templates.LayoutData{
		Title:         v.Title,
		ThemeName:     v.ThemeName,
		ThemeExplicit: v.ThemeExplicit,
		Scripts:       v.Scripts,
		CSSHref:       v.CSSHref,
		IconURL:       v.IconURL,
		ExtraHead:     v.ExtraHead,
		Footer:        v.Footer,
		Navbar:        toNavbar(&v.Navbar),
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
	return d
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
		d.Annotations = append(d.Annotations, templates.AnnotationChip{Key: chip.Key, Val: chip.Val})
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

// toSearchData maps the package-web searchView onto the templ SearchData: a
// field copy plus the breadcrumb branches, the kind-abbrev tag, the formatted
// created meta, the snippet tuples, the label chips, the "type all" flag, the
// count-footer sentence, and the per-cluster error articles. Every value was
// resolved in buildSearchView; the icons are pre-rendered to raw strings.
func toSearchData(v *searchView) templates.SearchData {
	hasQuery := v.Query != ""
	offeredCount := len(v.OfferedTypes)
	d := templates.SearchData{
		Breadcrumb:        toSearchBreadcrumb(v),
		Query:             v.Query,
		Cluster:           v.Cluster,
		Namespace:         v.Namespace,
		IsAllNamespaces:   v.IsAllNamespaces,
		SearchIcon:        icon("search"),
		UnselectIcon:      icon("times"),
		IsAllClusters:     v.IsAllClusters,
		SelectedTypeCount: v.SelectedTypeCount,
		AllTypes:          offeredCount > 0 && v.SelectedTypeCount >= offeredCount,
		HasQuery:          hasQuery,
		AllNamespacesHref: v.AllNamespacesHref,
	}
	for _, opt := range v.OfferedTypes {
		d.OfferedTypes = append(d.OfferedTypes, templates.SearchTypeOption{Plural: opt.Plural, Kind: opt.Kind, Checked: opt.Checked})
	}
	for _, c := range v.ScopeClusters {
		d.ScopeClusters = append(d.ScopeClusters, templates.SearchScopeChip{Name: c.Name})
	}
	for _, res := range v.Results {
		card := templates.SearchResultCard{
			Kind:    res.Kind,
			KindTag: templates.KindAbbrev(res.Kind),
			KindLow: strings.ToLower(res.Kind),
			Title:   res.Title,
			Link:    res.Link,
		}
		if res.Created != "" {
			card.Created = formatTimestamp(res.Created)
		}
		for _, snip := range res.Matches {
			card.Snippets = append(card.Snippets, templates.SearchSnippet{Pre: snip.Pre, Match: snip.Match, Post: snip.Post})
		}
		for _, chip := range res.LabelChips {
			card.Chips = append(card.Chips, templates.Chip{Href: chip.Href, Class: chip.Class, Key: chip.Key, Val: chip.Val})
		}
		d.Results = append(d.Results, card)
	}
	if hasQuery {
		d.ResultSummary = searchSummary(len(v.Results), v.SelectedTypeCount, v.SearchedClusterCount, v.Duration.Seconds())
	}
	for _, cluster := range v.ErrorClusterOrder {
		errs := v.ClusterErrors[cluster]
		ce := templates.SearchClusterErrors{Cluster: cluster, Header: searchErrorHeader(cluster, len(errs))}
		for _, e := range errs {
			ce.Lines = append(ce.Lines, "Failed to search "+e.ResourceType+": "+e.Message)
		}
		d.ClusterErrors = append(d.ClusterErrors, ce)
	}
	return d
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

// searchSummary is the count-footer sentence ("N result(s) found. Searched M
// resource types in K cluster(s) in T seconds."), with the pluralization
// (results: !=1 -> "s"; clusters: >1 -> "s") and %.3f timing.
func searchSummary(resultCount, typeCount, clusterCount int, seconds float64) string {
	results := "results"
	if resultCount == 1 {
		results = "result"
	}
	clusters := "cluster"
	if clusterCount > 1 {
		clusters = "clusters"
	}
	return fmt.Sprintf("%d %s found. Searched %d resource types in %d %s in %.3f seconds.",
		resultCount, results, typeCount, clusterCount, clusters, seconds)
}

// searchErrorHeader is the danger-article heading ("Error(s) for cluster
// <name>"), singular for one error and "Errors" for more than one.
func searchErrorHeader(cluster string, count int) string {
	label := "Error"
	if count > 1 {
		label = "Errors"
	}
	return label + " for cluster " + cluster
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
