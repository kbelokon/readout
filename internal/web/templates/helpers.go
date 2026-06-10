package templates

import (
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/kbelokon/readout/internal/web/icons"
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

// htmxConfig is the htmx config object emitted into the head <meta>. htmx reads
// it before processing the DOM (allowEval/allowScriptTags off so script-src
// 'self' needs no unsafe-eval/inline; includeIndicatorStyles off so htmx injects
// no indicator <style> of its own -- the progress bar is styled by the app's
// `.ro-progress` CSS). templ escapes the quotes in the attribute value; the
// rendered attribute decodes back to this JSON.
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

// metaIcon returns the sidebar icon-slot markup for a non-resource Meta entry
// (Resource Types / Events), so it matches the icon + label row shape the
// resource entries get from the Unit-1 resolver. The label -> glyph mapping lives
// in the icons package (icons.MetaGlyph); the returned markup is trusted-shape
// (a constant glyph wrapped in `.ico sm`).
func metaIcon(label string) string {
	return string(icons.MetaGlyph(label))
}

// refreshSeconds maps a persisted ro_prefs refresh mode (D9 string vocabulary:
// "Off", an interval in seconds, future "Live") to polling seconds. "" / "Off"
// / "Live" / junk all yield 0 -- exactly what readout.js's refreshInterval()
// derives from the same cookie, so the SSR'd topbar state and the JS init sync
// always agree (Live gains its own rendering in Unit 27).
func refreshSeconds(mode string) int {
	n, err := strconv.Atoi(mode)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// refreshLabel is the topbar #refresh-label text for a persisted refresh mode:
// "Ns" for an active interval, else "Off" (matching readout.js syncRefreshUI).
func refreshLabel(mode string) string {
	if secs := refreshSeconds(mode); secs > 0 {
		return strconv.Itoa(secs) + "s"
	}
	return "Off"
}

// refreshOptionClass marks the interval option matching the persisted refresh
// mode as is-active ("Off"/none activates the data-interval="0" option, like
// the JS sync does).
func refreshOptionClass(interval, mode string) string {
	if interval == strconv.Itoa(refreshSeconds(mode)) {
		return "refresh-option is-active"
	}
	return "refresh-option"
}

// refreshDropdownClass adds the refresh-on styling hook when a positive
// interval is persisted (the spinning-icon state readout.js otherwise toggles).
func refreshDropdownClass(mode string) string {
	if refreshSeconds(mode) > 0 {
		return "refresh-dropdown refresh-on"
	}
	return "refresh-dropdown"
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

// dotClass is the redesign status-dot class for a phase-strip chip: `ro-dot`
// plus the tone modifier (ok/warn/err/info/mute) when present. An empty tone
// (unmocked kind) yields a bare `ro-dot` with no tone colour.
func dotClass(tone string) string {
	if tone == "" {
		return "ro-dot"
	}
	return "ro-dot " + tone
}

// dotClass2 is dotClass for a status CELL, adding `.pulse` for a transient state.
func dotClass2(tone string, pulse bool) string {
	cls := dotClass(tone)
	if pulse {
		cls += " pulse"
	}
	return cls
}

// cellStatusClass is the redesign status-cell wrapper class (`cell-status` + the
// tone modifier when present).
func cellStatusClass(tone string) string {
	if tone == "" {
		return "cell-status"
	}
	return "cell-status " + tone
}

// pcStatusClass is the mobile card status-pill wrapper class (`pc-status` + the
// tone modifier when present) -- the card-layout twin of cellStatusClass, so the
// card status pill carries the same tone vocabulary the table status cell does.
func pcStatusClass(tone string) string {
	if tone == "" {
		return "pc-status"
	}
	return "pc-status " + tone
}

// nameCell finds the row's loud-identifier cell (the CellName branch) for the
// mobile card's `.pc-name`. ok is false when the kind's Table API exposes no name
// column (the generic engine still lists such a kind; the card then falls back to
// the first cell's value). row is taken by pointer (the TableRow/TableCell structs
// are heavy -- gocritic hugeParam/rangeValCopy).
func nameCell(row *TableRow) (TableCell, bool) {
	for i := range row.Cells {
		if row.Cells[i].Kind == CellName {
			return row.Cells[i], true
		}
	}
	return TableCell{}, false
}

// statusCell finds the row's status cell (the CellStatus branch) for the mobile
// card's `.pc-status`. ok is false when the kind has no status column (a generic
// kind), in which case the card renders no status pill.
func statusCell(row *TableRow) (TableCell, bool) {
	for i := range row.Cells {
		if row.Cells[i].Kind == CellStatus {
			return row.Cells[i], true
		}
	}
	return TableCell{}, false
}

// metaKey is the lowercased column header for a card `.pc-meta` row's `.k` label
// (the mockup shows `ready` / `restarts` / `age`, lowercase). row.Cells align 1:1
// with table.Columns, so the cell index selects its column name; an out-of-range
// index (defensive) yields "".
func metaKey(table *TableData, cellIndex int) string {
	if cellIndex < 0 || cellIndex >= len(table.Columns) {
		return ""
	}
	return strings.ToLower(table.Columns[cellIndex].Name)
}

// pcardValueClass is the semantic colour class a plain card meta value should keep
// from its table cell: the age bucket (`age-*`) and any Bulma tone (`has-text-*`)
// carry meaning the card preserves (the mockup wraps the age value in its
// `bucketClass`), while the table's `num` right-alignment class is layout-only and
// meaningless inside the flex card, so it is dropped. Returns "" when nothing
// colour-bearing remains -> the card renders the bare value with no wrapper span.
func pcardValueClass(cell *TableCell) string {
	var keep []string
	for _, c := range strings.Fields(cell.ColClass + " " + cell.Class) {
		if strings.HasPrefix(c, "age-") || strings.HasPrefix(c, "has-text-") {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, " ")
}

// readyClassRD is the redesign ready-ratio span class (`ready` + full|partial|
// zero). An empty ratio (not an n/d value) yields a bare `ready`.
func readyClassRD(ratio string) string {
	if ratio == "" {
		return "ready"
	}
	return "ready " + ratio
}

// restartsClassRD is the redesign restarts span class (`restarts` + zero|some).
func restartsClassRD(tone string) string {
	if tone == "" {
		return "restarts"
	}
	return "restarts " + tone
}

// thClass is the table header class: the kube column class (e.g. "num" for a
// numeric column) plus the redesign `sorted` modifier on the active sort column.
func thClass(colClass string, sorted bool) string {
	parts := colClass
	if sorted {
		parts = strings.TrimSpace(parts + " sorted")
	}
	return parts
}

// rowClass keeps the existing row-status stripe class on the body row (carried
// through from assembly); empty when the row has no status. The redesign uses it
// only for the selected-row accent, but the class itself stays byte-stable.
func rowClass(statusClass string) string { return statusClass }

// numClass passes the age/created cell class through (it already carries `num`
// when the column is numeric plus the age-* bucket); a no-op join kept for
// symmetry with the other cell class helpers.
func numClass(cls string) string { return cls }

// numColClass ensures a right-aligned ratio/restarts cell carries `num` even when
// the kube column class did not (the ready/restarts columns are string-typed in
// the server Table so GuessColumnClasses does not mark them numeric).
func numColClass(colClass string) string {
	if strings.Contains(" "+colClass+" ", " num ") {
		return colClass
	}
	return strings.TrimSpace("num " + colClass)
}

// cellTdClass is the generic (non-rich) cell <td> class: the augmented cellClass
// + the column class. Truncation is NOT a <td> class -- `.trunc` is
// `display:inline-block` (base.css), which on a <td> drops the cell out of the
// table row and misaligns every following column. The `.trunc` clamp lives on an
// inner <span>/<a> instead (matching the mockup's `<span class="trunc">`).
func cellTdClass(cellClass, colClass string) string {
	return strings.TrimSpace(cellClass + " " + colClass)
}

// capClass is the node capacity-bar wrapper class (`cap` + the lo/mid/hi bucket
// modifier when present). An empty bucket (the no-metrics state) yields a bare
// `cap` with no colour, matching the value-text-only fallback.
func capClass(bucket string) string {
	if bucket == "" {
		return "cap"
	}
	return "cap " + bucket
}

// capWidth is the inline width declaration for the capacity-bar fill `<i>`, as
// `width:N%`. The width is 0 in the no-metrics state (an empty/0-width bar). The
// design contract pins the bar fill to `i[style=width]` (an inline width is the
// only way to express an arbitrary per-row percentage); templ sanitizes the
// returned declaration.
func capWidth(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return "width:" + strconv.Itoa(pct) + "%"
}

// roleClass is the node role-chip class (`ro-role-chip` + `.cp` for the
// control-plane / master roles, which earn the green accent).
func roleClass(role string) string {
	if role == "control-plane" || role == "master" {
		return "ro-role-chip cp"
	}
	return "ro-role-chip"
}

// condPillClass is the node abnormal-condition pill class (`ro-cond-pill` + the
// tone warn/err/ok). An empty tone yields a bare `ro-cond-pill`.
func condPillClass(tone string) string {
	if tone == "" {
		return "ro-cond-pill"
	}
	return "ro-cond-pill " + tone
}

// repNumClass is the deployment replica-count class (`rep-num ready` + the ratio
// tone full|partial|zero). It mirrors readyClassRD's tone vocabulary but carries
// the `rep-num` track-number class the design pins on the replica ratio span.
func repNumClass(ratio string) string {
	if ratio == "" {
		return "rep-num ready"
	}
	return "rep-num ready " + ratio
}

// rolloutClass is the deployment rollout-status class (`rollout` + the state
// done|prog|paused). An empty state yields a bare `rollout`.
func rolloutClass(state string) string {
	if state == "" {
		return "rollout"
	}
	return "rollout " + state
}

// warnIcon is the redesign partial-failure banner glyph (lucide triangle-alert),
// wrapped in the same <svg> shell as the icons package glyphs so it themes via
// `.ico svg`. A static constant: no runtime-derived data crosses it.
func warnIcon() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" class="lucide-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/></svg>`
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

// hintClass is the reason-line class on a state card: `hint` always, plus `mono`
// when the line carries a real (transport) error string so it renders in the
// mono face (matching the mockup's `.hint.mono` for the verbatim error).
func hintClass(mono bool) string {
	if mono {
		return "hint mono"
	}
	return "hint"
}

// stateNamespaceClause is the " in namespace "<ns>"" suffix on a forbidden-state
// title, naming the namespace scope the verb was denied in. Empty for a
// cluster-scoped or all-namespaces request (no single namespace to name).
func stateNamespaceClause(namespace string) string {
	if namespace == "" || namespace == "_all" {
		return ""
	}
	return ` in namespace "` + namespace + `"`
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
