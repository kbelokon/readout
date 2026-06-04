package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"

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

func (s *Server) sidebarResourceLink(r *http.Request, cluster, namespace, plural string) (string, string, bool) {
	if s.manager == nil {
		return sidebarResourceHref(cluster, namespace, plural), sidebarResourceText(plural), true
	}
	clusterObj, ok := s.manager.Get(cluster)
	if !ok {
		return sidebarResourceHref(cluster, namespace, plural), sidebarResourceText(plural), true
	}
	client := s.kubeClient(r, clusterObj)
	var rt kube.ResourceType
	var err error
	if namespace != "" {
		rt, err = client.FindResource(r.Context(), plural, true, "")
		if err == nil {
			return fmt.Sprintf("/clusters/%s/namespaces/%s/%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(rt.Plural)), pluralizeKind(rt.Kind), true
		}
	}
	rt, err = client.FindResource(r.Context(), plural, false, "")
	if err != nil {
		return "", "", false
	}
	return sidebarResourceHref(cluster, "", rt.Plural), pluralizeKind(rt.Kind), true
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

// nodeConditionTone maps a Node condition (type + status) to the semantic pill
// tone class. It is the one detail-summary helper still used after the templ
// migration -- buildNodeSummaryView (build_resource.go) consumes it to resolve
// the Node condition pills; the string render funcs that also used it are gone.
func nodeConditionTone(typ, status string) string {
	switch typ {
	case "Ready":
		if status == "True" {
			return "ro-st-ok"
		}
		return "ro-st-err"
	case "NetworkUnavailable":
		if status == "True" {
			return "ro-st-err"
		}
		return "ro-st-ok"
	case "MemoryPressure", "DiskPressure", "PIDPressure":
		if status == "True" {
			return "ro-st-warn"
		}
		return "ro-st-ok"
	default:
		return "ro-st-neutral"
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
