package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/icons"
	"github.com/kbelokon/readout/internal/web/templates"
)

// pageComponent renders a page whose body is a real templ component (no string
// bridge) through the single templ layout shell. The heavy pages (resource-list
// / view / logs / preferences) call this so they no longer round their body
// through templ.Raw(<built string>) -- the body is a typed templ component all
// the way down. The namespace-override variant is
// pageComponentWithNamespace.
func (s *Server) pageComponent(w http.ResponseWriter, r *http.Request, title string, body templ.Component) {
	s.pageComponentWithNamespace(w, r, title, nil, body)
}

func (s *Server) pageComponentWithNamespace(w http.ResponseWriter, r *http.Request, title string, namespaceOverride *string, body templ.Component) {
	v := s.buildLayoutView(r, title, namespaceOverride)
	s.renderLayout(w, &v, body)
}

// pageComponentWithScope renders a templ-body page whose cluster/namespace scope
// is supplied explicitly (not from path values). The param-less /search route
// uses it so the shell sidebar + navbar context render from the ?cluster= /
// ?namespace= query, matching a cluster-scoped page.
func (s *Server) pageComponentWithScope(w http.ResponseWriter, r *http.Request, title, cluster, namespace string, body templ.Component) {
	v := s.buildLayoutViewScoped(r, title, cluster, namespace)
	s.renderLayout(w, &v, body)
}

func effectiveNamespace(r *http.Request, namespaceOverride *string) string {
	if namespaceOverride != nil {
		return *namespaceOverride
	}
	return r.PathValue("namespace")
}

// sidebarLink is one resolved sidebar resource-type entry: the list href, the
// display text, and -- when a kube.ResourceType was resolved (HasKind) -- the
// {Kind, Group, Plural, IsCRD} the icon resolver needs. When discovery is absent
// (s.manager == nil) or the cluster is unknown, HasKind stays false and only the
// plural is known, so the renderer uses the no-discovery monogram fallback.
type sidebarLink struct {
	Href    string
	Text    string
	Kind    string
	Group   string
	Plural  string
	IsCRD   bool
	HasKind bool
}

func (s *Server) sidebarResourceLink(r *http.Request, cluster, namespace, plural string) (sidebarLink, bool) {
	if s.manager == nil {
		return sidebarLink{Href: sidebarResourceHref(cluster, namespace, plural), Text: sidebarResourceText(plural), Plural: plural}, true
	}
	clusterObj, ok := s.manager.Get(cluster)
	if !ok {
		return sidebarLink{Href: sidebarResourceHref(cluster, namespace, plural), Text: sidebarResourceText(plural), Plural: plural}, true
	}
	client := s.kubeClient(r, clusterObj)
	var rt kube.ResourceType
	var err error
	if namespace != "" {
		rt, err = client.FindResource(r.Context(), plural, true, "")
		if err == nil {
			return sidebarLinkFromResource(
				fmt.Sprintf("/clusters/%s/namespaces/%s/%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(rt.Plural)), &rt), true
		}
	}
	rt, err = client.FindResource(r.Context(), plural, false, "")
	if err != nil {
		return sidebarLink{}, false
	}
	return sidebarLinkFromResource(sidebarResourceHref(cluster, "", rt.Plural), &rt), true
}

// sidebarLinkFromResource builds a sidebarLink carrying the resolved
// {Kind, Group, Plural, IsCRD} (HasKind=true) so the icon resolver runs against
// the same kube.ResourceType the link already resolved.
func sidebarLinkFromResource(href string, rt *kube.ResourceType) sidebarLink {
	return sidebarLink{
		Href:    href,
		Text:    pluralizeKind(rt.Kind),
		Kind:    rt.Kind,
		Group:   rt.Group,
		Plural:  rt.Plural,
		IsCRD:   isCRD(rt.APIVersion),
		HasKind: true,
	}
}

// sidebarNavIcon resolves a sidebar nav entry's icon markup: the Unit-1
// icons.KindIcon when the kube.ResourceType is known (with the Tier-3
// cfg.ResourceIcons override looked up by kind+group), else the no-discovery
// icons.PluralMonogram fallback. Shared by the templ sidebar and the palette
// feed so both emit the same glyph.
func sidebarNavIcon(s *Server, item *navItem) template.HTML {
	if !item.HasKind {
		return icons.PluralMonogram(item.Plural)
	}
	override := s.cfg.ResourceIcons[config.ResourceIconKey{Kind: item.Kind, Group: item.Group}]
	return icons.KindIcon(item.Kind, item.Group, item.IsCRD, override)
}

func sidebarResourceHref(cluster, namespace, plural string) string {
	if namespace == "" || plural == "namespaces" || plural == "nodes" || plural == "persistentvolumes" {
		return fmt.Sprintf("/clusters/%s/%s", url.PathEscape(cluster), url.PathEscape(plural))
	}
	return fmt.Sprintf("/clusters/%s/namespaces/%s/%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(plural))
}

func sidebarResourceText(plural string) string {
	switch plural {
	case "namespaces":
		return "Namespaces"
	case "nodes":
		return "Nodes"
	case "persistentvolumes":
		return "PersistentVolumes"
	case "deployments":
		return "Deployments"
	case "cronjobs":
		return "CronJobs"
	case "jobs":
		return "Jobs"
	case "daemonsets":
		return "DaemonSets"
	case "statefulsets":
		return "StatefulSets"
	case "ingresses":
		return "Ingresses"
	case "services":
		return "Services"
	case "pods":
		return "Pods"
	case "configmaps":
		return "ConfigMaps"
	default:
		return plural
	}
}

// nodeConditionTone maps a Node condition (type + status) to the redesign pill
// tone token (ok/warn/err/mute). buildNodeSummaryView (build_resource.go)
// consumes it to resolve the detail-page Node condition pills, which render under
// the .ro-rd marker as `.ro-cond-pill.<tone>` with a `.ro-dot` -- matching the
// redesign detail CSS. (The list-cell node conditions use nodeConditionListTone,
// which shares this healthy/abnormal polarity.)
func nodeConditionTone(typ, status string) string {
	switch typ {
	case "Ready":
		if status == "True" {
			return "ok"
		}
		return "err"
	case "NetworkUnavailable":
		if status == "True" {
			return "err"
		}
		return "ok"
	case "MemoryPressure", "DiskPressure", "PIDPressure":
		if status == "True" {
			return "warn"
		}
		return "ok"
	default:
		return "mute"
	}
}

// commandPalette renders the templ CommandPalette component to a string. The
// palette markup now lives in templ (components.templ); this thin adapter keeps
// the existing string-returning call sites/tests working while the single
// source of the palette DOM is the templ component.
func commandPalette() string {
	var b strings.Builder
	_ = templates.CommandPalette().Render(context.Background(), &b)
	return b.String()
}

// icon returns inline Lucide SVG chrome for the hand-written render path (the
// heavy pages still building HTML strings). The SVG map is shared with the templ
// components via the internal/web/icons package so both render paths emit the
// same markup; this thin wrapper keeps the existing call sites + tests working.
func icon(name string) string {
	return icons.SVG(name)
}
