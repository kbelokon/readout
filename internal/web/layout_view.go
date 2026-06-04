package web

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
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
// it is the active (current-path) item.
type navItem struct {
	Href   string
	Text   string
	Active bool
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

	return layoutView{
		Title:         title,
		ThemeName:     themeName,
		ThemeExplicit: explicit,
		Scripts:       scripts,
		CSSHref:       s.assetURL("readout.css"),
		IconURL:       s.assetURL("favicon.png"),
		ExtraHead:     s.partials["partials/extrahead.html"],
		Footer:        s.partials["partials/footer.html"],
		Navbar:        s.buildNavbarView(r, cluster, namespace, themeName, explicit),
		Sidebar:       s.buildSidebarView(r, cluster, namespace),
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
			href, text, ok := s.sidebarResourceLink(r, cluster, namespace, typ)
			if !ok {
				continue
			}
			links = append(links, navItem{Href: href, Text: text, Active: r.URL.Path == href})
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
