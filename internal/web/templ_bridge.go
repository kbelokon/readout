package web

import (
	"context"
	"encoding/json"
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
		// PaletteJSON is the palette feed pre-encoded to a JSON string; the layout
		// emits it into a hidden <div> (NOT a <script>, which htmx would strip on
		// swap). The package-web paletteFeedView's json tags ARE the pinned wire
		// contract.
		PaletteJSON: paletteFeedJSONString(&v.Palette),
	}
}

// paletteFeedJSONString encodes the ⌘K palette feed to a compact JSON string for
// the #ro-palette-data hidden <div>. A marshal error (not expected for this
// plain struct) degrades to an empty object so the palette opens empty rather
// than the layout failing to render.
func paletteFeedJSONString(feed *paletteFeedView) string {
	b, err := json.Marshal(feed)
	if err != nil {
		return "{}"
	}
	return string(b)
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
		RefreshMode:     n.RefreshMode,
		LiveDisabled:    n.LiveDisabled,
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
	for i := range items {
		it := &items[i]
		// Count/HasCount ride along for the sidebar Meta entries (the Events
		// meta carries a count, D13); namespace nav items never set them.
		out[i] = templates.NavItem{Href: it.Href, Text: it.Text, Active: it.Active, Count: it.Count, HasCount: it.HasCount}
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
	for i := range items {
		it := &items[i]
		out[i] = templates.NavItem{Href: it.Href, Text: it.Text, Active: it.Active, Icon: string(it.Icon), Count: it.Count, HasCount: it.HasCount}
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
	if v.FilterBar != nil {
		fb := &templates.FilterBarData{Plural: v.FilterBar.Plural, FilterIcon: icon("filter")}
		for _, chip := range v.FilterBar.Chips {
			fb.Chips = append(fb.Chips, templates.EditorChip{
				Field:      chip.Field,
				Op:         chip.Op,
				Value:      chip.Value,
				Label:      chip.Label,
				RemoveHref: chip.RemoveHref,
			})
		}
		d.FilterBar = fb
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
		Cluster:   s.Cluster,
		Verb:      s.Verb,
		Resource:  s.Resource,
		Namespace: s.Namespace,
		Hint:      s.Hint,
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
		Kind:               t.Kind,
		Count:              len(t.Table.Rows),
		DownloadTSVHref:    t.DownloadTSVHref,
		SearchHref:         t.SearchHref,
		DownloadIcon:       icon("download"),
		ToolsIcon:          icon("caret-square-down"),
		SearchIcon:         icon("search"),
		Tools:              toTableTools(&t.Tools),
		ShowMetricsHref:    t.ShowMetricsHref,
		PhaseRows:          len(t.Table.Rows),
		MultiCluster:       len(t.Table.Clusters) > 1,
		ColumnCount:        len(t.Table.Columns),
		CreatedHref:        t.CreatedHref,
		CreatedIcon:        t.CreatedIcon,
		CreatedSorted:      t.CreatedIcon != "",
		CreatedPartialHref: t.CreatedPartialHref,
		HideCreated:        t.HideCreated,
		EmptyGlyph:         icon("inbox"),
		ClearHref:          t.ClearHref,
	}
	// The ⊞ column-visibility popover (D8): single-type pages only (buildListView
	// fills ColumnVis under the D1 gate). It absorbs the tools form's labelcols +
	// selector inputs, so it reuses the resolved toolsView values + hidden-input
	// round-trip; nil keeps the v1 toggle-tools chrome.
	if len(t.ColumnVis) > 0 {
		pop := &templates.ColsPopover{
			Plural: t.Table.Resource.Plural,
			Icon:   icon("columns-3"),
			Tools:  toTableTools(&t.Tools),
		}
		// The popover did NOT absorb the v1 visible filter input (the chips
		// editor replaced it on single-type pages), so an active legacy
		// ?filter= must round-trip its GET submit as a hidden input here --
		// without it, applying a selector wipes the filter. POPOVER-ONLY by
		// construction: buildToolsView's shared round-trip key list must not
		// gain "filter", because the v1 multi-type tools form still renders
		// the visible same-named input, and a hidden twin would precede it in
		// form order and shadow every user edit (q.Get returns the first
		// occurrence). The `?f=` chips have no hidden input at all: their raw
		// OR-commas cannot survive form urlencoding (filter.go splits on raw
		// commas) -- readout.js merges them into the submit URL byte-exact.
		if t.Tools.FilterVal != "" {
			pop.Tools.HiddenInputs = append(pop.Tools.HiddenInputs,
				templates.HiddenInput{Name: "filter", Value: t.Tools.FilterVal})
		}
		for _, entry := range t.ColumnVis {
			pop.Entries = append(pop.Entries, templates.ColsEntry{Name: entry.Name, Hidden: entry.Hidden, Identity: entry.Identity})
		}
		td.Cols = pop
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
			PartialHref: t.Columns[i].PartialHref,
			Hint:        t.Columns[i].Hint,
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
			Key:          row.Key,
			DomID:        rowDomID(row.Key),
			Name:         row.Name,
			OpenHref:     row.OpenHref,
			YAMLHref:     row.YAMLHref,
			LogsHref:     row.LogsHref,
			DownloadHref: row.DownloadHref,
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
				More:         cell.More,
				EvKind:       cell.EvKind,
				EvName:       cell.EvName,
				EvAgeRest:    cell.EvAgeRest,
			}
			for _, cond := range cell.Conds {
				tc.Conds = append(tc.Conds, templates.Cond{Name: cond.Name, Tone: cond.Tone})
			}
			for _, seg := range cell.RepSegments {
				tc.RepSegments = append(tc.RepSegments, templates.RepSegment{State: seg.State})
			}
			for _, chip := range cell.Chips {
				tc.Chips = append(tc.Chips, templates.RowChip{Key: chip.Key, Val: chip.Val, Href: chip.Href})
			}
			for _, key := range cell.Keys {
				tc.Keys = append(tc.Keys, templates.KeyChip{Name: key.Name, Size: key.Size})
			}
			switch cell.Kind {
			case cellRollout:
				tc.RolloutIcon = icon(rolloutIconName(cell.RolloutState))
			case cellTLS:
				// The earned-green lock (SPEC §4.13), pre-rendered only for a
				// terminated cell (the "—" fallback carries no icon).
				if cell.Value != "" {
					tc.CellIcon = icon("lock")
				}
			case cellEvObj:
				// The events Object kind icon: the same 3-tier resolver every
				// kind surface uses (group unknown on an event ref -> monogram
				// hue keys on the kind, exactly like the reference kindIcon).
				tc.CellIcon = string(icons.KindIcon(cell.EvKind, "", false, ""))
			}
			tr.Cells = append(tr.Cells, tc)
		}
		td.Rows = append(td.Rows, tr)
	}
	return td
}

// rowDomID derives the row's DOM id from its data-key (D6: idiomorph matches
// rows by id, never position). The id must be safe inside the quoted attribute
// selector idiomorph uses (`[id="…"]`) and as an HTML id, so '%', '"', '\',
// whitespace, and control bytes are percent-escaped; everything else (incl.
// '/') passes through, keeping the common case readable ("row-c/ns/name").
// Percent-escaping '%' itself keeps distinct keys mapping to distinct ids.
// An empty key (a multi-type v1 row) yields an empty id (no attribute emitted).
func rowDomID(key string) string {
	if key == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("row-")
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c <= ' ' || c == '"' || c == '\\' || c == '%' || c == 0x7f {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
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
		SingleType: v.SingleType,
		Bulk: templates.BulkBar{
			DownloadHref:  v.BulkDownloadHref,
			Cluster:       v.Cluster,
			AllNamespaces: v.IsAllNamespaces,
		},
		List: toListData(v),
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
				Cluster:   v.State.Cluster,
				Verb:      v.State.Verb,
				Resource:  v.State.Resource,
				Name:      v.State.Name,
				Namespace: v.State.Namespace,
				Hint:      v.State.Hint,
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
		NameHead:           v.NameHead,
		NameTail:           v.NameTail,
		NameTitle:          v.NameTitle,
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
		d.Labels = append(d.Labels, templates.DetailLabelChip{Href: chip.Href, Key: chip.Key, Val: chip.Val})
	}
	for _, chip := range v.Annotations {
		d.Annotations = append(d.Annotations, templates.AnnotationChip{Key: chip.Key, Val: chip.Val, Full: chip.Full})
	}
	for _, long := range v.AnnotationsLong {
		d.AnnotationsLong = append(d.AnnotationsLong, templates.AnnotationLong{Key: long.Key, Size: long.Size, Value: long.Value, ChevIcon: icon("chevron-down")})
	}
	if v.Node != nil {
		d.Node = toNodeSummary(*v.Node)
	}
	if v.Containers != nil {
		c := &templates.Containers{Count: v.Containers.Count, InitCount: v.Containers.InitCount}
		for i := range v.Containers.Rows {
			row := &v.Containers.Rows[i]
			c.Rows = append(c.Rows, templates.ContainerRow{
				Name:         row.Name,
				Init:         row.Init,
				State:        row.State,
				StateTone:    row.StateTone,
				StatePulse:   row.StatePulse,
				Ready:        row.Ready,
				ReadyClass:   row.ReadyClass,
				Restarts:     row.Restarts,
				RestartsTone: row.RestartsTone,
				Ago:          row.Ago,
				Ports:        row.Ports,
				CPU:          row.CPU,
				Mem:          row.Mem,
				Image:        row.Image,
			})
		}
		d.Containers = c
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
		d.YAMLCards = append(d.YAMLCards, templates.YAMLCard{Name: card.Name, Title: card.Title, CopyIcon: icon("copy"), Content: card.Content, Collapsed: card.Collapsed})
	}
	if v.RelatedPods != nil {
		d.RelatedPods = toSubtable(v.RelatedPods)
	}
	for i := range v.Events {
		ev := &v.Events[i]
		d.Events = append(d.Events, templates.EventRow{
			Type:       ev.Type,
			Tone:       ev.Tone,
			Reason:     ev.Reason,
			Count:      ev.Count,
			CountClass: ev.CountClass,
			Age:        ev.Age,
			AgeClass:   ev.AgeClass,
			AgeRest:    ev.AgeRest,
			AgeTitle:   ev.AgeTitle,
			From:       ev.From,
			Message:    ev.Message,
		})
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
			sr.Cells = append(sr.Cells, templates.SubtableCell{Kind: templates.CellKind(cell.Kind), Value: cell.Value, Class: cell.Class, Href: cell.Href, Tone: cell.Tone})
		}
		st.Rows = append(st.Rows, sr)
	}
	return st
}

// toSearchData maps the package-web searchView onto the redesign templ
// SearchData: the breadcrumb branches, the form round-trip (query + hidden
// cluster/namespace inputs + the restored resource-type checkboxes), the
// scope-opts labels, the partial-failure banner, the per-cluster
// `.ro-scope-chip.ok|err` chips (with the read-only retry hrefs), the totals
// strip, the per-cluster result groups (rows carrying the resolved kind icon,
// the pn-head/pn-tail + mark name segments, and the age-bucket cell), and the
// no-results copy. Every value was resolved in buildSearchView; the search +
// kind icons are pre-rendered to raw strings.
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
		ScopeClusterLabel: searchScopeClusterLabel(v),
		NamespaceLabel:    searchNamespaceLabel(v),
		TypeLabel:         searchTypeLabel(allTypes, v.SelectedTypeCount),
		Banner:            searchBanner(v),
	}
	for _, opt := range v.OfferedTypes {
		d.Types = append(d.Types, templates.SearchTypeOption{Plural: opt.Plural, Kind: opt.Kind, Checked: opt.Checked})
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
	for gi := range v.Groups {
		group := &v.Groups[gi]
		tone := "ok"
		if group.Failed {
			tone = "err"
		}
		tg := templates.SearchGroup{Cluster: group.Cluster, DotTone: tone, Count: len(group.Results)}
		for i := range group.Results {
			res := &group.Results[i]
			tg.Rows = append(tg.Rows, templates.SearchResultRow{
				Namespace: res.Namespace,
				NsHref:    "/clusters/" + url.PathEscape(res.Cluster) + "/namespaces/" + url.PathEscape(res.Namespace),
				HasNs:     res.Namespace != "",
				Kind:      res.Kind,
				KindIcon:  string(icons.KindIcon(res.Kind, res.Group, res.IsCRD, "")),
				NamePre:   res.NamePre,
				NameMark:  res.NameMark,
				NamePost:  res.NamePost,
				NameTail:  res.NameTail,
				NameTitle: res.NameTitle,
				Link:      res.Link,
				Age:       res.Created,
				AgeClass:  res.AgeClass,
			})
		}
		d.Groups = append(d.Groups, tg)
	}
	if hasQuery {
		d.TotalsLine, d.TotalsMeta = searchTotals(v)
		d.EmptyTitle = "Nothing matched “" + v.Query + "”"
		d.EmptyText = fmt.Sprintf("Searched names across %d cluster%s and %d resource type%s.",
			len(v.ScopeClusters), pluralS(len(v.ScopeClusters)), v.SelectedTypeCount, pluralS(v.SelectedTypeCount))
	}
	return d
}

// searchTotals builds the totals strip (D12): the tally line counts what the
// results ACTUALLY span -- "N objects · M clusters · K kinds" where M is the
// clusters that contributed results (the group count) -- and the meta reports
// what the search COVERED: "searched M clusters in T s" over every cluster in
// scope, with the same %.3f timing the list footers use.
func searchTotals(v *searchView) (line, meta string) {
	line = fmt.Sprintf("%d object%s · %d cluster%s · %d kind%s",
		len(v.Results), pluralS(len(v.Results)),
		len(v.Groups), pluralS(len(v.Groups)),
		v.KindCount, pluralS(v.KindCount))
	meta = fmt.Sprintf("searched %d cluster%s in %.3fs",
		len(v.ScopeClusters), pluralS(len(v.ScopeClusters)), v.Duration.Seconds())
	return line, meta
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
// the selected namespace count/name).
func searchNamespaceLabel(v *searchView) string {
	if v.IsAllNamespaces || v.Namespace == "" {
		return "all namespaces"
	}
	namespaces := searchScopeValues([]string{v.Namespace})
	if len(namespaces) > 1 {
		return fmt.Sprintf("%d namespaces", len(namespaces))
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
	namespaces := searchScopeValues([]string{v.Namespace})
	if v.IsAllNamespaces {
		b.ShowAllNamespace = true
	} else if len(namespaces) == 1 {
		b.ShowNamespace = true
		b.Namespace = namespaces[0]
		b.NamespaceHref = "/clusters/" + url.PathEscape(v.Cluster) + "/namespaces/" + url.PathEscape(namespaces[0])
	}
	return b
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
