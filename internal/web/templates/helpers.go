package templates

import (
	"fmt"
	"html"
	"net/url"
	"strconv"
)

// helpers.go holds the small pure-Go helpers the templ components call for class
// strings and attribute values. They match the package-web render helpers so the
// templ output keeps the same class/attribute structure the fact net pins.

// itoa is strconv.Itoa, exposed under a short name for the count expressions.
func itoa(n int) string { return strconv.Itoa(n) }

// escapeHTML is html.EscapeString, exposed under a short name for the search
// page's snippet/no-results helpers that assemble a trusted HTML string (so the
// <em> highlight emits via a single @templ.Raw without a stray text-node child).
func escapeHTML(s string) string { return html.EscapeString(s) }

// itoa64 formats an int64 (the logs tail_lines value) as a base-10 string.
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// clusterHref is the /clusters/<name> link target with the name path-escaped,
// matching the hand-written render's url.PathEscape on the cluster segment.
func clusterHref(name string) string {
	return "/clusters/" + url.PathEscape(name)
}

// namespaceHref is the /clusters/<c>/namespaces/<ns> breadcrumb link target,
// both segments path-escaped (matching renderResourceTypes' breadcrumb).
func namespaceHref(cluster, namespace string) string {
	return "/clusters/" + url.PathEscape(cluster) + "/namespaces/" + url.PathEscape(namespace)
}

// htmxConfig is the strict-CSP htmx config object emitted into the head <meta>.
// htmx reads it before processing the DOM (allowEval/allowScriptTags off so
// script-src 'self' needs no unsafe-eval/inline; includeIndicatorStyles off so
// no inline style is injected under style-src 'self'). templ escapes the quotes
// in the attribute value; the rendered attribute decodes back to this JSON.
const htmxConfig = `{"allowEval": false, "allowScriptTags": false, "includeIndicatorStyles": false}`

// boolStr renders a Go bool as the lowercase "true"/"false" the data-* attribute
// contract expects (matching fmt's %t and the hand-written render).
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// menuItemClass is the sidebar link class: the active (current-path) item gains
// the is-active accent.
func menuItemClass(active bool) string {
	if active {
		return "menu-item is-active"
	}
	return "menu-item"
}

// pluralSuffix returns "" for a count of 1, else "s", for the "N object(s)" /
// "row(s)" / "cluster(s)" footer text.
func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// formatSeconds renders a duration in seconds with the %.3f the list footer +
// phase-total timing used.
func formatSeconds(seconds float64) string {
	return fmt.Sprintf("%.3f", seconds)
}

// activeSuffix is the tools-form is-active class suffix: " is-active" when
// active, else "".
func activeSuffix(active bool) string {
	if active {
		return " is-active"
	}
	return ""
}

// emptyColspan computes the empty-row <td> colspan: the kube.Table column count
// + 1 (the Created column), plus the optional Cluster / Namespace columns.
func emptyColspan(columnCount int, multiCluster, allNamespaces bool) int {
	colspan := columnCount + 1
	if multiCluster {
		colspan++
	}
	if allNamespaces {
		colspan++
	}
	return colspan
}

// emptyNamespaceText is the namespace clause inside the empty-state sentence.
// It returns trusted HTML (the namespace is html-escaped here): `in namespace
// "<ns>" ` (with a trailing space) when scoped to one namespace, else "".
func emptyNamespaceText(namespace string, allNamespaces bool) string {
	if namespace != "" && !allNamespaces {
		return `in namespace "` + html.EscapeString(namespace) + `" `
	}
	return ""
}

// emptyTitle is the full empty-state sentence ("No <Kind> objects <ns-clause>
// found."), returned as one trusted HTML string so the templ card emits it via a
// single @templ.Raw -- a text node after @templ.Raw is parsed as that call's
// children (and templ.Raw drops children), so the sentence must be one piece.
// kind is html-escaped here; the namespace clause is escaped inside
// emptyNamespaceText.
func emptyTitle(kind, namespace string, allNamespaces bool) string {
	return "No " + html.EscapeString(kind) + " objects " + emptyNamespaceText(namespace, allNamespaces) + "found."
}

// ownerLabel is "Owner" for a single owner, "Owners" for more.
func ownerLabel(count int) string {
	if count > 1 {
		return "Owners"
	}
	return "Owner"
}

// foundLine is the resource-list footer summary ("Found N rows for M resource
// types in K clusters in T seconds."), with the pluralization + %.3f timing. Two
// spaces after "for" are intentional (kept from the original format string).
func foundLine(totalRows, tableCount, clusterCount int, seconds float64) string {
	return fmt.Sprintf("Found %d row%s for  %d resource type%s in %d cluster%s in %s seconds.",
		totalRows, pluralSuffix(totalRows),
		tableCount, pluralSuffix(tableCount),
		clusterCount, pluralSuffix(clusterCount),
		formatSeconds(seconds))
}
