package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/icons"
)

// defaultSidebarGroups is the built-in sidebar layout used when the config file
// declares no sidebar. The order here is the rendered order: sidebar groups
// follow declaration order, not alphabetical.
func defaultSidebarGroups() []config.SidebarGroup {
	return []config.SidebarGroup{
		{Label: "Cluster Resources", Resources: []string{"namespaces", "nodes", "persistentvolumes"}},
		{Label: "Controllers", Resources: []string{"deployments", "cronjobs", "jobs", "daemonsets", "statefulsets"}},
		// Secrets close the §6.2 Pod Management group (D13). With the secret
		// barrier down (IncludeSecrets=false, the default) discovery filters the
		// Secret type out, sidebarResourceLink fails to resolve it, and the entry
		// simply does not render -- so the curated entry only appears when the
		// operator opted into secrets.
		{Label: "Pod Management", Resources: []string{"ingresses", "services", "pods", "configmaps", "secrets"}},
	}
}

// layout_view.go holds the request-derived view model for the page shell:
// the templ layout component (layout.templ) consumes plain resolved data, never
// a *http.Request. Every value the navbar/sidebar/head needs -- theme, the
// explicit-theme flag, cluster/namespace context, the namespace dropdown links,
// the grouped sidebar links with active flags, the head asset hrefs, and the
// trusted raw extrahead/footer partials -- is precomputed here in the handler
// seam and carried into the component, consistent with the Unit-6 view-model
// contract (no live request inside the renderer).

// layoutView is the resolved input to the templ page shell. Scripts/Links carry
// only the assets with a non-empty hashed href (matching the current head loop),
// so the component emits no empty <script>/<link>. ExtraHead/Footer are the
// trusted operator-supplied partials (TemplatesPath, via loadPartials) emitted
// raw; FooterDefault is the built-in footer used when no custom footer is set.
type layoutView struct {
	Title         string
	ThemeName     string
	ThemeExplicit bool

	Scripts []string // head <script src> hrefs, in order, non-empty only
	CSSHref string   // readout.css hashed href ("" => omit)
	IconURL string   // favicon.png hashed href ("" => omit)

	ExtraHead string // raw partials/extrahead.html (TemplatesPath)
	Footer    string // raw partials/footer.html (TemplatesPath); "" => default

	Navbar  navbarView
	Sidebar sidebarView
	Palette paletteFeedView
}

// navbarView is the resolved navbar contract: the cluster/namespace context
// pill (rendered only when ShowContext), the namespace dropdown links, the
// hidden search inputs, and the theme-toggle next URL + next theme.
type navbarView struct {
	ShowContext    bool
	ContextName    string
	NamespaceLinks []navItem

	SearchCluster   string
	SearchNamespace string

	NextTheme     string
	ToggleNextURL string
	ThemeExplicit bool

	// RefreshMode is the persisted auto-refresh mode from the ro_prefs cookie
	// (D9): "" (no preference) / "Off" / an interval in seconds as a string /
	// the future "Live". The topbar renders the refresh label + active option
	// from it at SSR so the persisted choice paints without the JS sync flash;
	// readout.js re-derives the same state from the same cookie on init.
	RefreshMode string
}

// sidebarView is the resolved sidebar: the grouped resource-type links (each
// with its active flag) plus the trailing Meta links. Rendered only when a
// cluster is in scope (ShowMenu).
type sidebarView struct {
	ShowMenu bool
	Groups   []sidebarGroup
	Meta     []navItem
}

type sidebarGroup struct {
	Label string
	Links []navItem
}

// navItem is one resolved navigation link: a target href, its text, and whether
// it is the active (current-path) item. Sidebar resource-type entries also carry
// the resolved {Kind, Group, Plural, IsCRD} so the renderer can ask the Unit-1
// icon resolver for a per-entry glyph (icons.KindIcon) without a second
// discovery call; non-resource nav items (namespaces, Meta) leave them zero and
// fall back to the no-discovery monogram.
type navItem struct {
	Href   string
	Text   string
	Active bool

	// Kind/Group/Plural/IsCRD describe the resource type behind a sidebar entry,
	// already resolved by sidebarResourceLink from the kube.ResourceType. They are
	// empty for non-resource nav items.
	Kind    string
	Group   string
	Plural  string
	IsCRD   bool
	HasKind bool // true once a kube.ResourceType was resolved (vs the no-discovery fallback)

	// Resource is the full resolved kube.ResourceType behind the entry (zero
	// unless HasKind). The sidebar counts fetch needs the parts the flat fields
	// above drop -- APIVersion (the count cache key) and Namespaced (whether the
	// count is namespace- or cluster-scoped).
	Resource kube.ResourceType

	// Count/HasCount carry the per-kind object count (D13): the mono
	// `.menu-count` value. HasCount distinguishes a real "0" (rendered) from a
	// failed or never-attempted fetch (no count shown).
	Count    string
	HasCount bool

	// Icon is the pre-rendered per-entry glyph from the Unit-1 resolver
	// (icons.KindIcon when HasKind, else icons.PluralMonogram), resolved in the
	// handler seam so the templ sidebar and the palette feed share one markup
	// string and the renderer stays request-free. Empty for non-resource items.
	Icon template.HTML
}

// paletteFeedView is the resolved data the ⌘K palette consumes (D10): the
// current scope plus the cluster / namespace / kind / action lists, all drawn
// from the SAME server context the layout already built (the cluster registry,
// the navbar namespace links, the sidebar resource types) -- no new discovery or
// API call. It is serialized verbatim into the #ro-palette-data JSON blob by the
// templ shell; Unit 4's JS reads it. The JSON tags ARE the pinned wire contract.
type paletteFeedView struct {
	CurrentCluster   *string             `json:"currentCluster"`
	CurrentNamespace *string             `json:"currentNamespace"`
	Clusters         []paletteLinkFeed   `json:"clusters"`
	Namespaces       []paletteLinkFeed   `json:"namespaces"`
	Kinds            []paletteKindFeed   `json:"kinds"`
	Actions          []paletteActionFeed `json:"actions"`
}

// paletteLinkFeed is a {name, href} jump target (a cluster or a namespace).
// Display carries the SPEC §4.2 middle-truncated form when -- and only when --
// the name overruns the 42-rune identifier budget (D5/D21: truncation is
// SERVER-side, in this feed builder, via the shared MiddleTruncate); the JS
// renders Display and keeps the full Name in the row title.
type paletteLinkFeed struct {
	Name    string `json:"name"`
	Href    string `json:"href"`
	Display string `json:"display,omitempty"`
}

// paletteKindFeed is a resource-type jump target: the kind label + plural +
// API group + the namespaced/cluster scope + the list href + the pre-rendered
// (HTML-escaped via JSON encoding) icon markup from the Unit-1 resolver.
// Display is the truncated label form, exactly as on paletteLinkFeed.
type paletteKindFeed struct {
	Kind       string `json:"kind"`
	Plural     string `json:"plural"`
	Group      string `json:"group"`
	Namespaced bool   `json:"namespaced"`
	Href       string `json:"href"`
	Icon       string `json:"icon"`
	Display    string `json:"display,omitempty"`
}

// paletteActionFeed is a labelled action: an href (navigate) or a named client
// action the palette JS interprets (Unit 4). Both keys are emitted so the JS can
// branch; only one is populated per entry.
type paletteActionFeed struct {
	Label  string `json:"label"`
	Href   string `json:"href,omitempty"`
	Action string `json:"action,omitempty"`
}

func (s *Server) buildLayoutViewWithClients(r *http.Request, title string, namespaceOverride *string, clients requestKubeClients) layoutView {
	return s.buildLayoutViewScopedWithClients(r, title, r.PathValue("cluster"), effectiveNamespace(r, namespaceOverride), clients)
}

// buildLayoutViewScopedWithClients resolves the page shell with the cluster +
// namespace scope supplied explicitly rather than read from path values. The
// param-less /search route has no {cluster}/{namespace} path segments, so its
// handler passes the QUERY (?cluster= / ?namespace=) here. With a concrete
// cluster this renders that cluster's sidebar + navbar context; with an empty or
// all-clusters scope the existing buildSidebarView/buildNavbarView gates emit no
// sidebar, as before.
func (s *Server) buildLayoutViewScopedWithClients(r *http.Request, title, cluster, namespace string, clients requestKubeClients) layoutView {
	themeName := theme(r, &s.cfg)
	explicit := themeExplicit(r)

	var scripts []string
	for _, name := range []string{"htmx.min.js", "idiomorph-ext.min.js", "preload.min.js", "readout.js"} {
		if href := s.assetURL(name); href != "" {
			scripts = append(scripts, href)
		}
	}

	navbar := s.buildNavbarView(r, cluster, namespace, themeName, explicit, clients)
	sidebar := s.buildSidebarView(r, cluster, namespace, clients)
	return layoutView{
		Title:         title,
		ThemeName:     themeName,
		ThemeExplicit: explicit,
		Scripts:       scripts,
		CSSHref:       s.assetURL("readout.css"),
		IconURL:       s.assetURL("favicon.png"),
		ExtraHead:     s.partials["partials/extrahead.html"],
		Footer:        s.partials["partials/footer.html"],
		Navbar:        navbar,
		Sidebar:       sidebar,
		Palette:       s.buildPaletteFeed(r, cluster, namespace, clients, &navbar, &sidebar),
	}
}

// buildPaletteFeed assembles the ⌘K palette data (D10): the cluster registry,
// the navbar namespace links, EVERY discovered resource type (built-ins + CRDs,
// from the cached discovery the sidebar already warmed), and the sidebar meta
// actions. When no cluster is in scope the cluster list still populates (the
// registry is request-independent) while namespaces/kinds are empty, so the
// palette opens everywhere. The icon markup is the Unit-1 resolver's
// already-escaped string carried verbatim.
func (s *Server) buildPaletteFeed(r *http.Request, cluster, namespace string, clients requestKubeClients, navbar *navbarView, sidebar *sidebarView) paletteFeedView {
	feed := paletteFeedView{
		Clusters:   []paletteLinkFeed{},
		Namespaces: []paletteLinkFeed{},
		Kinds:      []paletteKindFeed{},
		Actions:    []paletteActionFeed{},
	}
	if cluster != "" && cluster != kube.AllClusters {
		c := cluster
		feed.CurrentCluster = &c
	}
	if namespace != "" && namespace != kube.AllNamespaces {
		n := namespace
		feed.CurrentNamespace = &n
	}

	if s.manager != nil {
		// The palette cluster list is the topbar's cluster nav, i.e. a
		// cluster-ENTRY surface: each link consumes the persisted
		// namespace-per-cluster pref via clusterEntryHref (D9 -- link
		// construction only, never a redirect).
		nsPrefs := prefsFromRequest(r).Namespaces
		for _, c := range s.manager.Clusters() {
			feed.Clusters = append(feed.Clusters, paletteLinkFeed{
				Name:    c.Name,
				Href:    clusterEntryHref(c.Name, nsPrefs[c.Name]),
				Display: paletteDisplayName(c.Name),
			})
		}
	}

	for i := range navbar.NamespaceLinks {
		ns := &navbar.NamespaceLinks[i]
		feed.Namespaces = append(feed.Namespaces, paletteLinkFeed{
			Name:    ns.Text,
			Href:    ns.Href,
			Display: paletteDisplayName(ns.Text),
		})
	}

	// Resource-type group: ALL discovered types (built-ins + CRDs) so ⌘K jumps to
	// any kind, not only the curated sidebar. Discovery is cached (the sidebar
	// already triggered it); a failure degrades to no kinds and the palette still
	// opens with clusters/namespaces.
	if cluster != "" && cluster != kube.AllClusters {
		if clusterObj, ok := s.manager.Get(cluster); ok {
			client := s.requestKubeClient(r, clients, clusterObj)
			nsTypes, clusterTypes, _ := client.ResourceTypes(r.Context())
			seen := map[string]bool{}
			add := func(rt *kube.ResourceType, ns string) {
				// metrics.k8s.io (PodMetrics/NodeMetrics) is a join source, not a
				// navigable kind -- skip it (and its plural collides with pods/nodes).
				if rt.Group == "metrics.k8s.io" {
					return
				}
				entry := s.paletteKindEntry(cluster, ns, rt)
				if seen[entry.Href] {
					return
				}
				seen[entry.Href] = true
				feed.Kinds = append(feed.Kinds, entry)
			}
			for i := range nsTypes {
				add(&nsTypes[i], namespace)
			}
			for i := range clusterTypes {
				add(&clusterTypes[i], "")
			}
		}
	}

	// On a detail page (the route carries {name}) add jump-to-tab actions for the
	// object in scope -- Default / YAML / Events, plus Logs for a workload -- so
	// ⌘K dives into a view without clicking the tabs. The href base is the BASE
	// detail path rebuilt from the route values, NOT r.URL.Path: on the /logs
	// sub-page r.URL.Path carries the /logs suffix, which yielded broken
	// .../logs?view=events tab links.
	if name := r.PathValue("name"); name != "" {
		ns := r.PathValue("namespace")
		plural := r.PathValue("plural")
		base := fmt.Sprintf("/clusters/%s/%s/%s", url.PathEscape(cluster), url.PathEscape(plural), url.PathEscape(name))
		if ns != "" {
			base = fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s", url.PathEscape(cluster), url.PathEscape(ns), url.PathEscape(plural), url.PathEscape(name))
		}
		feed.Actions = append(feed.Actions,
			paletteActionFeed{Label: "Default view", Href: base},
			paletteActionFeed{Label: "YAML", Href: base + "?view=yaml"},
			paletteActionFeed{Label: "Events", Href: base + "?view=events"},
		)
		if ns != "" && workloadPlural(plural) {
			feed.Actions = append(feed.Actions, paletteActionFeed{Label: "Logs", Href: base + "/logs"})
		}
	}

	// Sidebar meta actions (Resource Types / Events), deduped by label so the
	// object's Events tab above is not doubled by the namespace-level Events meta.
	metaSeen := map[string]bool{}
	for _, a := range feed.Actions {
		metaSeen[a.Label] = true
	}
	for i := range sidebar.Meta {
		meta := &sidebar.Meta[i]
		if metaSeen[meta.Text] {
			continue
		}
		metaSeen[meta.Text] = true
		feed.Actions = append(feed.Actions, paletteActionFeed{Label: meta.Text, Href: meta.Href})
	}
	// "All clusters" navigates; "Toggle theme" carries NO href -- it is a named
	// CLIENT action the palette JS (choosePaletteRow) interprets by clicking the
	// server-POST #btn-theme-toggle (the read-only-safe theme flip). The theme
	// entry serializes as {"label","action":"theme"} with href omitted (omitempty),
	// which the action==="theme" branch keys off.
	feed.Actions = append(feed.Actions,
		paletteActionFeed{Label: "All clusters", Href: "/clusters"},
		paletteActionFeed{Label: "Toggle theme", Action: "theme"},
	)

	return feed
}

// workloadPlural reports whether a plural names a pod-log-bearing workload -- the
// kinds buildDetailView gives a LogsHref to -- so the palette only offers Logs
// where the detail page itself does.
func workloadPlural(plural string) bool {
	switch plural {
	case "pods", "deployments", "replicasets", "daemonsets", "statefulsets":
		return true
	}
	return false
}

// paletteKindEntry builds a palette resource-type row for a discovered type: a
// list href (the in-scope namespace for namespaced kinds, else _all; the cluster
// path for cluster-scoped kinds) and the Unit-1 kind icon (with the Tier-3
// per-resource override), matching how the sidebar resolves the same type.
func (s *Server) paletteKindEntry(cluster, namespace string, rt *kube.ResourceType) paletteKindFeed {
	var href string
	if rt.Namespaced {
		ns := namespace
		if ns == "" {
			ns = kube.AllNamespaces
		}
		href = fmt.Sprintf("/clusters/%s/namespaces/%s/%s", url.PathEscape(cluster), url.PathEscape(ns), url.PathEscape(rt.Plural))
	} else {
		href = fmt.Sprintf("/clusters/%s/%s", url.PathEscape(cluster), url.PathEscape(rt.Plural))
	}
	override := s.cfg.ResourceIcons[config.ResourceIconKey{Kind: rt.Kind, Group: rt.Group}]
	kindLabel := pluralizeKind(rt.Kind)
	return paletteKindFeed{
		Kind:       kindLabel,
		Plural:     rt.Plural,
		Group:      rt.Group,
		Namespaced: rt.Namespaced,
		Href:       href,
		Icon:       string(icons.KindIcon(rt.Kind, rt.Group, isCRD(rt.APIVersion), override)),
		Display:    paletteDisplayName(kindLabel),
	}
}

// paletteDisplayName applies the D5/D21 server-side truncation to a palette
// label: the SPEC §4.2 identifier budget (>42 runes -> first 26 + "…" + last
// 12) through the shared MiddleTruncate. Returns "" when the name fits -- the
// omitempty Display field then stays off the wire and the palette JS renders
// the full Name itself.
func paletteDisplayName(name string) string {
	display, truncated := MiddleTruncate(name, nameHeadMax, nameHeadLead, nameHeadTrail)
	if !truncated {
		return ""
	}
	return display
}

func (s *Server) buildNavbarView(r *http.Request, cluster, namespace, themeName string, explicit bool, clients requestKubeClients) navbarView {
	nextTheme := "dark"
	if themeName == "dark" {
		nextTheme = "light"
	}
	v := navbarView{
		SearchCluster:   cluster,
		SearchNamespace: namespace,
		NextTheme:       nextTheme,
		ToggleNextURL:   r.URL.RequestURI(),
		ThemeExplicit:   explicit,
		RefreshMode:     prefsFromRequest(r).Refresh,
	}
	if cluster != "" && cluster != kube.AllClusters {
		if clusterObj, ok := s.manager.Get(cluster); ok {
			client := s.requestKubeClient(r, clients, clusterObj)
			v.ShowContext = true
			v.ContextName = namespace
			if v.ContextName == "" {
				v.ContextName = "None"
			}
			for _, ns := range s.navbarNamespaces(r, client) {
				v.NamespaceLinks = append(v.NamespaceLinks, navItem{
					Href: fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(cluster), url.PathEscape(ns)),
					Text: ns,
				})
			}
		}
	}
	return v
}

func (s *Server) buildSidebarView(r *http.Request, cluster, namespace string, clients requestKubeClients) sidebarView {
	if cluster == "" {
		return sidebarView{}
	}
	v := sidebarView{ShowMenu: true}
	var client *kube.Client
	if s.manager != nil {
		if clusterObj, ok := s.manager.Get(cluster); ok {
			client = s.requestKubeClient(r, clients, clusterObj)
		}
	}

	groups := s.cfg.Sidebar
	if len(groups) == 0 {
		groups = defaultSidebarGroups()
	}
	for _, group := range groups {
		if len(group.Resources) == 0 {
			continue
		}
		var links []navItem
		for _, typ := range group.Resources {
			link, ok := s.sidebarResourceLink(r, client, cluster, namespace, typ)
			if !ok {
				continue
			}
			item := navItem{
				Href:     link.Href,
				Text:     link.Text,
				Active:   r.URL.Path == link.Href,
				Kind:     link.Kind,
				Group:    link.Group,
				Plural:   link.Plural,
				IsCRD:    link.IsCRD,
				HasKind:  link.HasKind,
				Resource: link.Resource,
			}
			item.Icon = sidebarNavIcon(s, &item)
			links = append(links, item)
		}
		if len(links) == 0 {
			continue
		}
		v.Groups = append(v.Groups, sidebarGroup{Label: group.Label, Links: links})
	}

	if cluster != "" && namespace != "" {
		metaLinks := []navItem{
			{Text: "Resource Types", Href: fmt.Sprintf("/clusters/%s/namespaces/%s/_resource-types", url.PathEscape(cluster), url.PathEscape(namespace))},
			{Text: "Events", Href: fmt.Sprintf("/clusters/%s/namespaces/%s/events", url.PathEscape(cluster), url.PathEscape(namespace))},
		}
		for i := range metaLinks {
			metaLinks[i].Active = r.URL.Path == metaLinks[i].Href
		}
		v.Meta = append(v.Meta, metaLinks...)
	} else if cluster != "" {
		href := fmt.Sprintf("/clusters/%s/_resource-types", cluster)
		v.Meta = append(v.Meta, navItem{Text: "Resource Types", Href: href, Active: r.URL.Path == href})
	}

	// Per-kind counts (D13): collected AFTER the view is fully assembled so the
	// target pointers into v.Groups/v.Meta stay valid, fetched concurrently
	// under one short deadline. With no concrete client (multi-cluster `_all`,
	// unknown cluster, no manager) no targets resolve and the sidebar renders
	// no counts.
	s.attachSidebarCounts(r.Context(), client, cluster, s.sidebarCountTargets(r, client, namespace, &v))
	return v
}

// sidebarCountTargets collects the sidebar entries that receive a count: every
// group entry with a resolved resource type, plus the namespace-scope Events
// meta entry (the §6.2 prototype shows a count on Events but not on Resource
// Types, which is not a kind). Namespaced kinds count within the in-scope
// namespace; cluster-scoped kinds count cluster-wide.
func (s *Server) sidebarCountTargets(r *http.Request, client *kube.Client, namespace string, v *sidebarView) []countTarget {
	if client == nil {
		return nil
	}
	var targets []countTarget
	for gi := range v.Groups {
		for li := range v.Groups[gi].Links {
			item := &v.Groups[gi].Links[li]
			if !item.HasKind {
				continue
			}
			ns := ""
			if item.Resource.Namespaced {
				ns = namespace
			}
			targets = append(targets, countTarget{item: item, resource: item.Resource, namespace: ns})
		}
	}
	if namespace != "" {
		for mi := range v.Meta {
			if v.Meta[mi].Text != "Events" {
				continue
			}
			// The Events meta link lists the preferred events kind in the scoped
			// namespace; resolve the same type for its count. Discovery is already
			// warm (the groups above resolved through it); a failure just leaves
			// the Events entry uncounted.
			if rt, err := client.FindResource(r.Context(), "events", true, ""); err == nil {
				targets = append(targets, countTarget{item: &v.Meta[mi], resource: rt, namespace: namespace})
			}
		}
	}
	return targets
}
