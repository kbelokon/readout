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
		{Label: "Pod Management", Resources: []string{"ingresses", "services", "pods", "configmaps"}},
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
type paletteLinkFeed struct {
	Name string `json:"name"`
	Href string `json:"href"`
}

// paletteKindFeed is a resource-type jump target: the kind label + plural +
// API group + the list href + the pre-rendered (HTML-escaped via JSON encoding)
// icon markup from the Unit-1 resolver.
type paletteKindFeed struct {
	Kind       string `json:"kind"`
	Plural     string `json:"plural"`
	Group      string `json:"group"`
	Namespaced bool   `json:"namespaced"`
	Href       string `json:"href"`
	Icon       string `json:"icon"`
}

// paletteActionFeed is a labelled action: an href (navigate) or a named client
// action the palette JS interprets (Unit 4). Both keys are emitted so the JS can
// branch; only one is populated per entry.
type paletteActionFeed struct {
	Label  string `json:"label"`
	Href   string `json:"href,omitempty"`
	Action string `json:"action,omitempty"`
}

// buildLayoutView resolves every request-derived input the page shell needs.
// namespaceOverride lets the detail page pass the object's namespace explicitly;
// title is the page's <title> stem. The cluster/namespace scope comes from the
// path values.
func (s *Server) buildLayoutView(r *http.Request, title string, namespaceOverride *string) layoutView {
	return s.buildLayoutViewScoped(r, title, r.PathValue("cluster"), effectiveNamespace(r, namespaceOverride))
}

// buildLayoutViewScoped is buildLayoutView with the cluster + namespace scope
// supplied explicitly rather than read from the path values. The param-less
// /search route has no {cluster}/{namespace} path segments, so its handler
// passes the QUERY (?cluster= / ?namespace=) here. With a concrete cluster this
// renders that cluster's sidebar + navbar context; with an empty or
// all-clusters scope the existing buildSidebarView/buildNavbarView
// gates (cluster != "" && cluster != AllClusters) emit no sidebar, as before.
func (s *Server) buildLayoutViewScoped(r *http.Request, title, cluster, namespace string) layoutView {
	themeName := theme(r, &s.cfg)
	explicit := themeExplicit(r)

	var scripts []string
	for _, name := range []string{"htmx.min.js", "idiomorph-ext.min.js", "preload.min.js", "readout.js"} {
		if href := s.assetURL(name); href != "" {
			scripts = append(scripts, href)
		}
	}

	navbar := s.buildNavbarView(r, cluster, namespace, themeName, explicit)
	sidebar := s.buildSidebarView(r, cluster, namespace)
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
		Palette:       s.buildPaletteFeed(r, cluster, namespace, &navbar, &sidebar),
	}
}

// buildPaletteFeed assembles the ⌘K palette data (D10): the cluster registry,
// the navbar namespace links, EVERY discovered resource type (built-ins + CRDs,
// from the cached discovery the sidebar already warmed), and the sidebar meta
// actions. When no cluster is in scope the cluster list still populates (the
// registry is request-independent) while namespaces/kinds are empty, so the
// palette opens everywhere. The icon markup is the Unit-1 resolver's
// already-escaped string carried verbatim.
func (s *Server) buildPaletteFeed(r *http.Request, cluster, namespace string, navbar *navbarView, sidebar *sidebarView) paletteFeedView {
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
		for _, c := range s.manager.Clusters() {
			feed.Clusters = append(feed.Clusters, paletteLinkFeed{
				Name: c.Name,
				Href: "/clusters/" + url.PathEscape(c.Name),
			})
		}
	}

	for _, ns := range navbar.NamespaceLinks {
		feed.Namespaces = append(feed.Namespaces, paletteLinkFeed{Name: ns.Text, Href: ns.Href})
	}

	// Resource-type group: ALL discovered types (built-ins + CRDs) so ⌘K jumps to
	// any kind, not only the curated sidebar. Discovery is cached (the sidebar
	// already triggered it); a failure degrades to no kinds and the palette still
	// opens with clusters/namespaces.
	if cluster != "" && cluster != kube.AllClusters {
		if clusterObj, ok := s.manager.Get(cluster); ok {
			nsTypes, clusterTypes, _ := s.kubeClient(r, clusterObj).ResourceTypes(r.Context())
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
	for _, meta := range sidebar.Meta {
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
	return paletteKindFeed{
		Kind:       pluralizeKind(rt.Kind),
		Plural:     rt.Plural,
		Group:      rt.Group,
		Namespaced: rt.Namespaced,
		Href:       href,
		Icon:       string(icons.KindIcon(rt.Kind, rt.Group, isCRD(rt.APIVersion), override)),
	}
}

func (s *Server) buildNavbarView(r *http.Request, cluster, namespace, themeName string, explicit bool) navbarView {
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
	}
	if cluster != "" && cluster != kube.AllClusters {
		if clusterObj, ok := s.manager.Get(cluster); ok {
			v.ShowContext = true
			v.ContextName = namespace
			if v.ContextName == "" {
				v.ContextName = "None"
			}
			for _, ns := range s.navbarNamespaces(r, clusterObj) {
				v.NamespaceLinks = append(v.NamespaceLinks, navItem{
					Href: fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(cluster), url.PathEscape(ns)),
					Text: ns,
				})
			}
		}
	}
	return v
}

func (s *Server) buildSidebarView(r *http.Request, cluster, namespace string) sidebarView {
	if cluster == "" {
		return sidebarView{}
	}
	v := sidebarView{ShowMenu: true}

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
			link, ok := s.sidebarResourceLink(r, cluster, namespace, typ)
			if !ok {
				continue
			}
			item := navItem{
				Href:    link.Href,
				Text:    link.Text,
				Active:  r.URL.Path == link.Href,
				Kind:    link.Kind,
				Group:   link.Group,
				Plural:  link.Plural,
				IsCRD:   link.IsCRD,
				HasKind: link.HasKind,
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
		for _, link := range []navItem{
			{Text: "Resource Types", Href: fmt.Sprintf("/clusters/%s/namespaces/%s/_resource-types", cluster, namespace)},
			{Text: "Events", Href: fmt.Sprintf("/clusters/%s/namespaces/%s/events", cluster, namespace)},
		} {
			link.Active = r.URL.Path == link.Href
			v.Meta = append(v.Meta, link)
		}
	} else if cluster != "" {
		href := fmt.Sprintf("/clusters/%s/_resource-types", cluster)
		v.Meta = append(v.Meta, navItem{Text: "Resource Types", Href: href, Active: r.URL.Path == href})
	}
	return v
}
