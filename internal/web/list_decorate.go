package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"
)

// list_decorate.go is the table-options decoration pass for the resource-list
// page: it runs the per-request column work over each kube.Table -- label and
// synthetic per-kind columns, the metrics-usage join, custom-column JSONPath
// joins, hidden-column removal with its re-offerable visibility universe, then
// the filter/sort/limit shaping. The kind-specific text and chip helpers that
// reconstruct printer-column values live here alongside the decorators that
// add them.

func (s *Server) applyTableOptions(r *http.Request, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool) []columnVis {
	return s.applyTableOptionsWithUsage(r, client, table, namespace, allNamespaces, nil)
}

// applyTableOptionsWithUsage is applyTableOptions with an optional pre-fetched
// metrics overlay: a non-nil metricsUsage feeds the ?join=metrics columns
// instead of a live metrics fetch. The Live stream renders pushes at up
// to ~3/s and refreshes usage on its own 30s sub-poll, so its renders must
// never re-list the metrics API; everything else passes nil and keeps the
// per-request fetch.
func (s *Server) applyTableOptionsWithUsage(r *http.Request, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool, metricsUsage map[string][2]float64) []columnVis {
	q := r.URL.Query()
	// Cookie fill: the ro_prefs colvis/sort prefs stand in for ABSENT URL
	// params (URL always wins; single-type pages only; render-only -- r.URL is
	// never mutated, so rebuilt hrefs and HX-Push-Url keep URL truth).
	fill := prefsListFill(r)
	hide := first(q.Get("hidecols"), q.Get("hide-columns"))
	if hide == "" {
		if fill.HasHide {
			// An explicit cookie hide set -- possibly EMPTY ("show everything"),
			// which must suppress the config default (user override wins).
			hide = fill.Hide
		} else {
			hide = s.cfg.DefaultHiddenColumns[table.Resource.Plural]
		}
	}
	labels := first(q.Get("labelcols"), q.Get("label-columns"), s.cfg.DefaultLabelColumns[table.Resource.Plural])
	kube.AddLabelColumns(table, labels)
	if table.Resource.Plural == "nodes" {
		// Nodes get rich capacity/pods/conditions columns (read from each row's
		// status). The CPU/Memory capacity columns are added here ONLY when metrics
		// are not joined; with ?join=metrics the applyMetricsUsage CPU/Memory Usage
		// columns below carry the usage overlay instead (one CPU and one Memory
		// column either way, never both).
		decorateNodeColumns(table, q.Get("join") == "metrics")
	}
	if table.Resource.Plural == "deployments" {
		// Deployments get a synthetic Rollout column derived from each row's
		// status/spec (the server-side Table has no rollout column). The Ready column
		// the Table already provides becomes the rich replica track in buildCellView.
		decorateDeploymentColumns(table)
	}
	if table.Resource.Plural == "namespaces" {
		// Namespaces get a synthetic Labels column (chips read from metadata.labels);
		// the Status column the Table already provides reuses the status-dot cell. No
		// pods-count column is added -- a Namespace object has no pod count and readout
		// has no per-namespace pod-count seam.
		decorateNamespaceColumns(table)
	}
	if table.Resource.Plural == "services" {
		// Services get the External-IP / Selector columns GUARANTEED (appended from
		// each row's spec/status when a server-side Table omits them); the rich
		// pending/ports/selector-chips cells land in buildCellView.
		decorateServiceColumns(table)
	}
	if table.Resource.Plural == "ingresses" {
		// Ingresses get a synthetic TLS column derived from each row's spec.tls
		// (the server-side Table has no TLS column).
		decorateIngressColumns(table)
	}
	if table.Resource.Plural == "jobs" {
		// Jobs get the verbatim-status guarantee: a printer without
		// the Status column gains one derived from status.conditions, and a bare
		// "Failed" refines to the condition's verbatim reason
		// (BackoffLimitExceeded).
		decorateJobColumns(table)
	}
	if table.Resource.Plural == "events" {
		// Events get the ×N dedupe column: the count decodes from each
		// row's object across BOTH event API shapes and lands before Message so
		// the wrapping msg column stays last. CronJobs and PersistentVolumes need
		// no table-level decoration — their printer columns already carry the v2
		// schema surface (Suspend/Last Schedule cells reskin in buildCellView;
		// the PV Status column rides the generic status cell).
		decorateEventColumns(table)
	}
	if q.Get("join") == "metrics" && (table.Resource.Plural == "pods" || table.Resource.Plural == "nodes") {
		usage := metricsUsage
		if usage == nil {
			usage = s.fetchMetricsUsage(r.Context(), client, table.Resource.Namespaced, namespace, allNamespaces, q.Get("selector"))
		}
		applyMetricsUsage(table, usage)
	}
	custom := first(q.Get("customcols"), q.Get("custom-columns"), s.cfg.DefaultCustomColumns[table.Resource.Plural])
	if custom != "" {
		s.joinCustomColumns(r.Context(), client, table, namespace, allNamespaces, custom, q)
	}
	// Column visibility: applied AFTER the label / synthetic / joined
	// columns land, so the hide spec can remove synthetic columns too (until v2
	// it ran before the decorations, which made node Pods/Conditions, the
	// deployment Rollout, the namespace Labels, and the joined usage columns
	// unhideable) and the captured universe covers everything the table renders.
	// Filters and sort still run AFTER the removal, exactly as before: a hidden
	// column stays unfilterable and unsortable. The identity column is protected
	// here -- never removed, `*` included (the sticky first column, the
	// name-click open, and the row gestures all hang off it).
	vis := applyHiddenColumns(table, hide)
	kube.FilterRowsByNamespace(table, s.cfg.IncludeNamespaces, s.cfg.ExcludeNamespaces)
	kube.FilterTable(table, q.Get("filter"), false)
	// Filters v2: the repeatable `?f=` chips, single-type pages only (the
	// single-type boundary -- the same gate the interaction loop uses). A multi-type
	// page that receives `f` anyway IGNORES it: never a 500, never a
	// surprise-empty table the client UI cannot explain. Chips run on the full
	// dataset BEFORE sort/limit, after the joins/decorations (so the joined
	// metrics and synthetic columns are filterable) and alongside the legacy
	// params -- both grammars AND-combine. Parsed from RawQuery because the
	// OR-comma is RAW on the wire; an encoded %2C is a literal comma inside
	// one alternative (see filter.go).
	if isSingleListType(r.PathValue("plural")) {
		applyFilterChips(table, parseFilterParams(r.URL.RawQuery))
	}
	kube.GuessColumnClasses(table)
	// Node CPU/Memory + usage columns render as rich LEFT-aligned bar cells, so
	// drop the numeric right-alignment GuessColumnClasses infers from their float
	// sort-cells -- otherwise the header sits right while the bar+value sit left.
	if table.Resource.Plural == "nodes" {
		for i := range table.Columns {
			switch table.Columns[i].Name {
			case "CPU", "Memory", "CPU Usage", "Memory Usage":
				table.Columns[i].Class = ""
			}
		}
	}
	// The configmap/secret Data decorators run AFTER GuessColumnClasses for the
	// same header/cell alignment reason: their curation is dropping the numeric
	// class the guesser infers from the server's integer key-count cells (the
	// keys-chips strip is left-aligned; the count cell itself stays untouched as
	// the sort/TSV/filter truth).
	if table.Resource.Plural == "configmaps" {
		decorateConfigMapColumns(table)
	}
	if table.Resource.Plural == "secrets" {
		decorateSecretColumns(table)
	}
	kube.SortTable(table, first(q.Get("sort"), fill.Sort))
	if limit := q.Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 && n < len(table.Rows) {
			table.Rows = table.Rows[:n]
		}
	}
	return vis
}

// columnVis is one column-visibility entry: a column of the FULLY
// decorated table (label columns, synthetic node/deployment/namespace columns,
// joined usage columns, the template-synthetic Created) plus whether the
// current render hides it and whether it is the protected identity column.
// The ⊞ popover renders one checkbox per entry, so hidden columns stay
// re-offerable even though they are gone from the kube.Table.
type columnVis struct {
	Name     string
	Hidden   bool
	Identity bool
}

// applyHiddenColumns resolves the effective hidecols spec against the
// decorated table: it returns the full popover universe and removes the hidden
// columns. The identity column -- the "Name" column, or the first column for a
// kind whose Table has none (the same nameColumn rule the row keys and the
// sticky column use) -- is NEVER removed, `*` included: a forced
// ?hidecols=Name is ignored server-side. The synthetic Created column is
// template-rendered, not a kube column, so it joins the universe here and
// hides via the render flag rather than kube.RemoveColumns.
func applyHiddenColumns(table *kube.Table, spec string) []columnVis {
	hidden := map[string]bool{}
	for _, name := range strings.Split(spec, ",") {
		if name = strings.TrimSpace(name); name != "" {
			hidden[name] = true
		}
	}
	hides := func(name string) bool { return hidden["*"] || hidden[name] }
	protected := ""
	if len(table.Columns) > 0 {
		protected = table.Columns[nameColumn(table)].Name
	}
	vis := make([]columnVis, 0, len(table.Columns)+1)
	var remove []string
	for _, col := range table.Columns {
		entry := columnVis{Name: col.Name, Identity: col.Name == protected}
		if !entry.Identity && hides(col.Name) {
			entry.Hidden = true
			remove = append(remove, col.Name)
		}
		vis = append(vis, entry)
	}
	vis = append(vis, columnVis{Name: "Created", Hidden: hides("Created")})
	if len(remove) > 0 {
		// k8s printer-column names never contain commas (the same invariant the
		// cookie's comma-joined fill relies on), so the explicit name list is a
		// safe RemoveColumns spec -- and `*` never reaches kube unexpanded.
		kube.RemoveColumns(table, strings.Join(remove, ","))
	}
	return vis
}

// mergeColumnVis unions two column-visibility universes the way MergeTables
// unions columns: the left order wins and unseen right entries append -- but
// BEFORE the trailing Created entry, so the synthetic column stays last,
// mirroring the rendered header order. A nil left passes right through.
func mergeColumnVis(left, right []columnVis) []columnVis {
	if left == nil {
		return right
	}
	seen := map[string]bool{}
	for _, entry := range left {
		seen[entry.Name] = true
	}
	for _, entry := range right {
		if seen[entry.Name] {
			continue
		}
		seen[entry.Name] = true
		if n := len(left); n > 0 && left[n-1].Name == "Created" {
			left = append(left[:n-1], entry, left[n-1])
		} else {
			left = append(left, entry)
		}
	}
	return left
}

// fetchMetricsUsage resolves the metrics.k8s.io usage overlay for a pods or
// nodes scope: "namespace/name" → [cpu cores, memory bytes], decoded through
// kube.MetricsUsage (the typed quantity seam). nil when discovery or the
// metrics LIST fails — applyMetricsUsage then writes the zero placeholders,
// so a failed fetch never leaves ragged rows. Split from the column apply
// so the Live stream can refresh usage on its own 30s sub-poll instead
// of re-fetching per push.
func (s *Server) fetchMetricsUsage(ctx context.Context, client *kube.Client, namespaced bool, namespace string, allNamespaces bool, labelSelector string) map[string][2]float64 {
	metricsKind := "NodeMetrics"
	if namespaced {
		metricsKind = "PodMetrics"
	}
	rt, err := client.FindResourceByKind(ctx, "metrics.k8s.io/v1beta1", metricsKind, namespaced)
	if err != nil {
		return nil
	}
	listNS := namespace
	if allNamespaces {
		listNS = ""
	}
	list, err := client.List(ctx, &rt, kube.ListOptions{Namespace: listNS, LabelSelector: labelSelector})
	if err != nil {
		return nil
	}
	usage := map[string][2]float64{}
	for _, item := range list.Items {
		key, cpu, mem := kube.MetricsUsage(item.Object)
		usage[key] = [2]float64{cpu, mem}
	}
	return usage
}

// applyMetricsUsage appends the CPU/Memory Usage columns and EVERY row's two
// usage cells from the overlay. A row without metrics — and every row when
// the fetch failed (nil map) — gets the "metrics unknown" zero placeholders,
// so column/row cell counts always match and the table never renders ragged.
func applyMetricsUsage(table *kube.Table, usage map[string][2]float64) {
	table.Columns = append(table.Columns, kube.Column{Name: "CPU Usage"}, kube.Column{Name: "Memory Usage"})
	for i := range table.Rows {
		key := nestedString(table.Rows[i].Object, "metadata", "namespace") + "/" + nestedString(table.Rows[i].Object, "metadata", "name")
		value := usage[key]
		table.Rows[i].Cells = append(table.Rows[i].Cells, value[0], value[1])
	}
}

// decorateNodeColumns appends the rich node columns (Pods + Conditions always; the
// CPU/Memory capacity columns only when metrics are NOT joined) and fills each
// row's matching cells from the node's status. The capacity/conditions cells carry
// a plain DISPLAY value (capacity quantity / abnormal-condition names / "—") so
// sort, TSV, and the generic fallback stay sensible; the rich renderers re-read the
// row object for the bar fill + pills. Cells are appended for EVERY row in lockstep
// with the columns, so the table never goes ragged.
func decorateNodeColumns(table *kube.Table, metricsJoined bool) {
	type nodeCol struct {
		name string
		cell func(obj map[string]any) any
	}
	var cols []nodeCol
	if !metricsJoined {
		cols = append(cols,
			nodeCol{"CPU", func(obj map[string]any) any { return nodeCapacityText(obj, "cpu") }},
			nodeCol{"Memory", func(obj map[string]any) any { return nodeCapacityText(obj, "memory") }},
		)
	}
	cols = append(cols,
		nodeCol{"Pods", func(obj map[string]any) any { return nestedString(obj, "status", "capacity", "pods") }},
		nodeCol{"Conditions", func(obj map[string]any) any { return nodeConditionsText(obj) }},
	)
	for _, col := range cols {
		if columnIndex(table.Columns, col.name) >= 0 {
			continue // never duplicate a column the server-side Table already had
		}
		table.Columns = append(table.Columns, kube.Column{Name: col.name})
		for i := range table.Rows {
			table.Rows[i].Cells = append(table.Rows[i].Cells, col.cell(table.Rows[i].Object))
		}
	}
}

// decorateDeploymentColumns appends the synthetic Rollout column and fills each
// row's cell with the plain rollout DISPLAY label (derived from the row's
// status/spec) so sort, TSV, and the generic fallback see a sensible value; the
// rich cellRollout renderer re-reads the row object for the `.rollout.<state>`
// class + icon. The cell is appended for EVERY row in lockstep with the column, so
// the table never goes ragged. A Rollout column the server-side Table already
// provided (it does not today) is never duplicated.
func decorateDeploymentColumns(table *kube.Table) {
	if columnIndex(table.Columns, "Rollout") >= 0 {
		return
	}
	table.Columns = append(table.Columns, kube.Column{Name: "Rollout"})
	for i := range table.Rows {
		_, label := rolloutState(table.Rows[i].Object)
		table.Rows[i].Cells = append(table.Rows[i].Cells, label)
	}
}

// decorateNamespaceColumns appends the synthetic Labels column and fills each
// row's cell with the plain label DISPLAY value (the sorted comma-joined labels,
// or "—" when unlabeled) so sort, TSV, and the generic fallback see a sensible
// value; the rich cellChips renderer re-reads the row object for the per-label
// chips (the .app accent for app.kubernetes.io/*). A Namespace object carries NO
// pod count and readout has no per-namespace pod-count seam, so NO pods-count
// column is fabricated -- only status (already from the Table), labels, and age.
// The cell is appended for EVERY row in lockstep with the column, so the table
// never goes ragged. A "Labels" column that already exists (e.g. a user's
// labelcols=* produced one) is never duplicated -- the decoration is a no-op then,
// and that user column keeps falling through to the generic cell.
func decorateNamespaceColumns(table *kube.Table) {
	if columnIndex(table.Columns, "Labels") >= 0 {
		return
	}
	table.Columns = append(table.Columns, kube.Column{Name: "Labels"})
	for i := range table.Rows {
		table.Rows[i].Cells = append(table.Rows[i].Cells, namespaceLabelsText(table.Rows[i].Object))
	}
}

// decorateServiceColumns guarantees the v2 services schema surface:
// the External-IP and Selector columns are appended -- read from each row's
// spec/status with the SAME value encoding the upstream printer uses
// (`<none>`/`<pending>`/ExternalName target; sorted comma-joined `k=v`) -- when
// a server-side Table omits them. A standard apiserver provides both (Selector
// at priority 1, which readout keeps), so there the columnIndex guards make
// this a no-op; the append covers trimmed-down printers and keeps the rich
// External-IP/Selector cells kind-invariant. Cells are appended for EVERY row
// in lockstep with the columns, so the table never goes ragged.
func decorateServiceColumns(table *kube.Table) {
	type svcCol struct {
		name string
		cell func(obj map[string]any) any
	}
	cols := []svcCol{
		{"External-IP", func(obj map[string]any) any { return serviceExternalIPText(obj) }},
		{"Selector", func(obj map[string]any) any { return serviceSelectorText(obj) }},
	}
	for _, col := range cols {
		if columnIndex(table.Columns, col.name) >= 0 {
			continue // never duplicate a column the server-side Table already had
		}
		table.Columns = append(table.Columns, kube.Column{Name: col.name})
		for i := range table.Rows {
			table.Rows[i].Cells = append(table.Rows[i].Cells, col.cell(table.Rows[i].Object))
		}
	}
}

// decorateIngressColumns appends the synthetic TLS column and fills each row's
// cell with the plain DISPLAY value ("tls" when spec.tls terminates at least
// one host, "—" otherwise) so sort, TSV, filter, and the generic fallback see
// a sensible value; the rich tlsCellView renderer re-reads the row object for
// the earned-green lock. The cell is appended for EVERY row in
// lockstep with the column, so the table never goes ragged. A TLS column the
// server-side Table already provided (it does not today) is never duplicated.
func decorateIngressColumns(table *kube.Table) {
	if columnIndex(table.Columns, "TLS") >= 0 {
		return
	}
	table.Columns = append(table.Columns, kube.Column{Name: "TLS"})
	for i := range table.Rows {
		table.Rows[i].Cells = append(table.Rows[i].Cells, ingressTLSText(table.Rows[i].Object))
	}
}

// insertTableColumn inserts a named column at idx (appending when idx is out
// of range) and gives EVERY row its matching cell from cell(obj), keeping
// columns and cells in lockstep so the table never goes ragged. The shared
// seam under the jobs Status and events Count decorators, which must place
// their column mid-table (after Name / before Message) rather than append.
func insertTableColumn(table *kube.Table, idx int, name string, cell func(obj map[string]any) any) {
	if idx < 0 || idx > len(table.Columns) {
		idx = len(table.Columns)
	}
	table.Columns = append(table.Columns, kube.Column{})
	copy(table.Columns[idx+1:], table.Columns[idx:])
	table.Columns[idx] = kube.Column{Name: name}
	for i := range table.Rows {
		row := &table.Rows[i]
		value := cell(row.Object)
		if idx >= len(row.Cells) {
			row.Cells = append(row.Cells, value)
			continue
		}
		row.Cells = append(row.Cells, nil)
		copy(row.Cells[idx+1:], row.Cells[idx:])
		row.Cells[idx] = value
	}
}

// decorateJobColumns guarantees the jobs verbatim-status surface.
// A printer WITHOUT the Status column (pre-1.30 apiservers) gains a synthetic
// one right after the identity column, derived from status.conditions
// (jobStatusText). A printer WITH it keeps its cell as the truth — except a
// bare "Failed", which refines to the Failed condition's verbatim reason
// (`BackoffLimitExceeded`, never a paraphrase) so display, sort, TSV, and
// filter all speak the full status name. A Status column the decorator already
// added (or any non-Failed printer word) is never rewritten, so the pass is
// idempotent.
func decorateJobColumns(table *kube.Table) {
	idx := columnIndex(table.Columns, "Status")
	if idx < 0 {
		at := columnIndex(table.Columns, "Name")
		if at < 0 {
			// No Name column: append, never displace the identity (first) column.
			at = len(table.Columns)
		} else {
			at++
		}
		insertTableColumn(table, at, "Status", func(obj map[string]any) any { return jobStatusText(obj) })
		return
	}
	for i := range table.Rows {
		row := &table.Rows[i]
		if idx >= len(row.Cells) || cellDisplayString(row.Cells[idx]) != "Failed" {
			continue
		}
		if reason := jobFailedReason(row.Object); reason != "" {
			row.Cells[idx] = reason
		}
	}
}

// jobStatusText derives the jobs Status DISPLAY value from the object for
// printers that lack the column: the first True condition wins — a Failed
// condition surfaces its verbatim reason (BackoffLimitExceeded), any other
// condition its type (Complete, Suspended, FailureTarget, …) — and a job with
// no true condition is still Running.
func jobStatusText(obj map[string]any) string {
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok || nestedString(cond, "status") != "True" {
			continue
		}
		typ := nestedString(cond, "type")
		if typ == "Failed" {
			if reason := nestedString(cond, "reason"); reason != "" {
				return reason
			}
		}
		if typ != "" {
			return typ
		}
	}
	return "Running"
}

// jobFailedReason is the verbatim reason of a job's true Failed condition
// ("" when the job has none) — the refinement source for a printer's bare
// "Failed" Status cell.
func jobFailedReason(obj map[string]any) string {
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if nestedString(cond, "type") == "Failed" && nestedString(cond, "status") == "True" {
			return nestedString(cond, "reason")
		}
	}
	return ""
}

// decorateEventColumns appends the events ×N dedupe column: the count
// decodes from each row's object with the pinned dual-API precedence
// (series.count → count → deprecatedCount, defaulting to 1) and the cell
// carries the plain int64 so sort, TSV, and filter see the numeric truth; the
// rich ×N cell re-decodes at cell-build time. The column lands BEFORE Message
// so the wrapping msg column stays last. A Count column the server-side Table
// already provided (a priority-1 printer column) is never duplicated — the
// rich cell still re-decodes from the object either way.
func decorateEventColumns(table *kube.Table) {
	if columnIndex(table.Columns, "Count") >= 0 {
		return
	}
	insertTableColumn(table, columnIndex(table.Columns, "Message"), "Count", func(obj map[string]any) any {
		if item, ok := decodeEventItem(obj); ok {
			return item.eventCount()
		}
		return int64(1)
	})
}

// decorateConfigMapColumns curates the configmaps Data column for the keys
// cell. NO synthetic column is needed -- the server's integer
// key-count cell stays in place as the sort/TSV/filter truth and the
// `name · size` chips re-read the row object at cell-build time
// (configMapKeyChips) -- but the count cell makes GuessColumnClasses
// right-align the column, while the chips strip is left-aligned; the curation
// here drops that alignment so the header never sits right of its cells (the
// same mismatch the node capacity columns clear above).
func decorateConfigMapColumns(table *kube.Table) {
	clearDataColumnAlignment(table)
}

// decorateSecretColumns curates the secrets Data column exactly like
// decorateConfigMapColumns (the secret key chips decode from `data` with
// DECODED byte sizes in secretKeyChips; the count cell stays the plain truth):
// the only table-level curation is dropping the guessed numeric alignment.
func decorateSecretColumns(table *kube.Table) {
	clearDataColumnAlignment(table)
}

// clearDataColumnAlignment drops the numeric right-alignment from a Data
// column (shared by the configmap/secret decorators).
func clearDataColumnAlignment(table *kube.Table) {
	for i := range table.Columns {
		if table.Columns[i].Name == "Data" {
			table.Columns[i].Class = ""
		}
	}
}

// serviceExternalIPText reconstructs the services External-IP printer column
// from a Service object, mirroring the upstream printer's encoding:
// ExternalName -> the spec.externalName target; LoadBalancer -> the
// status.loadBalancer ingress IPs/hostnames + any spec.externalIPs, or the
// literal `<pending>` while unprovisioned; every other type -> spec.externalIPs
// or the literal `<none>`.
func serviceExternalIPText(obj map[string]any) string {
	svcType := nestedString(obj, "spec", "type")
	if svcType == "ExternalName" {
		return nestedString(obj, "spec", "externalName")
	}
	externalIPs, _, _ := unstructured.NestedStringSlice(obj, "spec", "externalIPs")
	if svcType == "LoadBalancer" {
		var ips []string
		ingress, _, _ := unstructured.NestedSlice(obj, "status", "loadBalancer", "ingress")
		for _, item := range ingress {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if ip := first(nestedString(entry, "ip"), nestedString(entry, "hostname")); ip != "" {
				ips = append(ips, ip)
			}
		}
		ips = append(ips, externalIPs...)
		if len(ips) > 0 {
			return strings.Join(ips, ",")
		}
		return "<pending>"
	}
	if len(externalIPs) > 0 {
		return strings.Join(externalIPs, ",")
	}
	return "<none>"
}

// serviceSelectorText is the plain Selector DISPLAY value (the printer's
// sorted comma-joined `k=v` encoding, `<none>` for a selectorless service);
// the rich chips re-read spec.selector via selectorChips.
func serviceSelectorText(obj map[string]any) string {
	selector, _, _ := unstructured.NestedStringMap(obj, "spec", "selector")
	if len(selector) == 0 {
		return "<none>"
	}
	return formatLabels(selector)
}

// selectorChips builds the service Selector chips from spec.selector (sorted
// by key for a stable order). The chips are NEUTRAL and deliberately carry NO
// click-to-filter href: a `label:key=value` chip filters by the SERVICES' OWN
// metadata.labels, while a selector names the labels of the pods the service
// selects -- linking the two would filter on the wrong object's labels.
func selectorChips(obj map[string]any) []chipView {
	selector, _, _ := unstructured.NestedStringMap(obj, "spec", "selector")
	if len(selector) == 0 {
		return nil
	}
	keys := make([]string, 0, len(selector))
	for key := range selector {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	chips := make([]chipView, 0, len(keys))
	for _, key := range keys {
		chips = append(chips, chipView{Key: key, Val: selector[key]})
	}
	return chips
}

// ingressTLSTerminated reports whether an Ingress terminates TLS: spec.tls
// lists at least one entry (ingress TLS comes from spec.tls).
func ingressTLSTerminated(obj map[string]any) bool {
	tls, _, _ := unstructured.NestedSlice(obj, "spec", "tls")
	return len(tls) > 0
}

// ingressTLSText is the plain DISPLAY value for the synthetic TLS column
// ("tls" / "—"), matching what the rich lock cell shows so the generic
// fallback / TSV / sort / filter see the same truth.
func ingressTLSText(obj map[string]any) string {
	if ingressTLSTerminated(obj) {
		return "tls"
	}
	return "—"
}

// commaListValues splits a printer's comma-joined list cell (service ports
// "80/TCP,443/TCP", ingress hosts "a.com,b.com") into its values, dropping
// the literal `<none>` a portless service prints. nil means an empty list
// (the ports/hosts cells render the muted "—").
func commaListValues(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" && part != "<none>" {
			out = append(out, part)
		}
	}
	return out
}

// configMapKeyChips builds the configmap Data key chips from the row object:
// `data` key names sized by their value's byte length, `binaryData` key names
// sized by the DECODED byte length (the wire form is base64). Only key names
// and sizes leave this function.
func configMapKeyChips(obj map[string]any) []keyChipView {
	data, _, _ := unstructured.NestedStringMap(obj, "data")
	binary, _, _ := unstructured.NestedStringMap(obj, "binaryData")
	sizes := make(map[string]int64, len(data)+len(binary))
	for key, value := range data {
		sizes[key] = int64(len(value))
	}
	for key, value := range binary {
		sizes[key] = base64DecodedLen(value)
	}
	return keyChips(sizes)
}

// secretKeyChips builds the secret Data key chips from the row object: `data`
// key names with the DECODED byte size (base64 -> raw length). The secret
// VALUE bytes never reach the view model -- only the length survives
// base64DecodedLen, and keyChipView has no value field by construction.
func secretKeyChips(obj map[string]any) []keyChipView {
	data, _, _ := unstructured.NestedStringMap(obj, "data")
	sizes := make(map[string]int64, len(data))
	for key, value := range data {
		sizes[key] = base64DecodedLen(value)
	}
	return keyChips(sizes)
}

// base64DecodedLen is the raw byte length of a base64 value. ONLY the length
// escapes; the decoded bytes are dropped here so a secret value cannot travel
// further than this stack frame. A value that fails to decode (never the case
// for apiserver-encoded data) falls back to its encoded length -- still a
// size, never content.
func base64DecodedLen(encoded string) int64 {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return int64(len(encoded))
	}
	return int64(len(raw))
}

// keyChips renders a key->size map as sorted `name · size` chips (humanBytes
// sizes), the shared tail of configMapKeyChips / secretKeyChips. nil for no
// keys, so the renderer shows the muted "—".
func keyChips(sizes map[string]int64) []keyChipView {
	if len(sizes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(sizes))
	for key := range sizes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	chips := make([]keyChipView, 0, len(keys))
	for _, key := range keys {
		chips = append(chips, keyChipView{Name: key, Size: humanBytes(sizes[key])})
	}
	return chips
}

// columnIndex reports the index of a named column, or -1. A small local mirror of
// kube.columnIndex (unexported) used by decorateNodeColumns to skip a column the
// server-side Table already provided.
func columnIndex(cols []kube.Column, name string) int {
	for i, col := range cols {
		if col.Name == name {
			return i
		}
	}
	return -1
}

// nodeCapacityText is the plain capacity DISPLAY value for the synthetic CPU/Memory
// columns (the canonical quantity, e.g. "4" / "16Gi"); empty when absent.
func nodeCapacityText(obj map[string]any, key string) string {
	if q, ok := nodeCapacityQuantity(obj, key); ok {
		return q.String()
	}
	return ""
}

// nodeConditionsText is the plain DISPLAY value for the synthetic Conditions column:
// the comma-joined abnormal-condition names, or "—" when the node is clean (so the
// generic fallback / TSV / sort sees the same "—" the rich pill renderer shows).
func nodeConditionsText(obj map[string]any) string {
	pills := nodeAbnormalConditions(obj)
	if len(pills) == 0 {
		return "—"
	}
	names := make([]string, len(pills))
	for i, p := range pills {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func (s *Server) joinCustomColumns(ctx context.Context, client *kube.Client, table *kube.Table, namespace string, allNamespaces bool, spec string, query url.Values) {
	parts := strings.Split(spec, ";")
	type compiled struct {
		name string
		expr *jsonpath.JSONPath
	}
	var expressions []compiled
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, expr, ok := strings.Cut(part, "=")
		if !ok {
			expr = name
			name = humanTitle(expr)
		}
		jp := jsonpath.New(name).AllowMissingKeys(true)
		if err := jp.Parse(relaxJSONPath(expr)); err != nil {
			continue
		}
		expressions = append(expressions, compiled{name: name, expr: jp})
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	if len(expressions) == 0 {
		return
	}
	var nodes map[string]map[string]any
	if query.Get("join") == "nodes" && table.Resource.Kind == "Pod" {
		if nodeRT, err := client.FindResource(ctx, "nodes", false, ""); err == nil {
			if nodeList, err := client.List(ctx, &nodeRT, kube.ListOptions{}); err == nil {
				nodes = map[string]map[string]any{}
				for _, item := range nodeList.Items {
					nodes[nestedString(item.Object, "metadata", "name")] = item.Object
				}
			}
		}
	}
	listNS := namespace
	if allNamespaces {
		listNS = ""
	}
	list, err := client.List(ctx, &table.Resource, kube.ListOptions{Namespace: listNS, LabelSelector: query.Get("selector")})
	if err != nil {
		for i := range table.Rows {
			for range expressions {
				table.Rows[i].Cells = append(table.Rows[i].Cells, nil)
			}
		}
		return
	}
	objects := map[string]map[string]any{}
	for _, item := range list.Items {
		key := nestedString(item.Object, "metadata", "namespace") + "/" + nestedString(item.Object, "metadata", "name")
		objects[key] = item.Object
	}
	joined := map[int]bool{}
	for i := range table.Rows {
		key := nestedString(table.Rows[i].Object, "metadata", "namespace") + "/" + nestedString(table.Rows[i].Object, "metadata", "name")
		obj := objects[key]
		if obj == nil {
			continue
		}
		for _, expr := range expressions {
			if table.Resource.Kind == "Secret" {
				table.Rows[i].Cells = append(table.Rows[i].Cells, kube.SecretContentHidden)
				continue
			}
			searchObj := obj
			if nodes != nil {
				searchObj = map[string]any{}
				for k, v := range obj {
					searchObj[k] = v
				}
				searchObj["node"] = nodes[nestedString(obj, "spec", "nodeName")]
			}
			table.Rows[i].Cells = append(table.Rows[i].Cells, evalJSONPath(expr.expr, searchObj))
		}
		joined[i] = true
	}
	for i := range table.Rows {
		if joined[i] {
			continue
		}
		for range expressions {
			table.Rows[i].Cells = append(table.Rows[i].Cells, nil)
		}
	}
}

// relaxJSONPath makes a custom-column expression kubectl-ergonomic. The raw
// jsonpath engine is a {...}-delimited template and does not auto-wrap a bare
// path, so a bare `spec.containers[*].image` is wrapped as
// `{.spec.containers[*].image}`. An expression that already contains a `{` is
// taken as an explicit template and passed through unchanged. (This follows the
// idea of kubectl's RelaxedJSONPathExpression, which lives in k8s.io/kubectl and
// is not a dependency here.)
func relaxJSONPath(expr string) string {
	if strings.Contains(expr, "{") {
		return expr
	}
	// Strip an optional leading "." like kubectl's RelaxedJSONPathExpression
	// does. Without this, a leading-dot expression (e.g. ".metadata.name")
	// becomes "{..metadata.name}", and the engine reads the leading ".." as the
	// recursive-descent operator — which matches same-named keys nested anywhere
	// in the object and leaks ghost values into the cell.
	return "{." + strings.TrimPrefix(expr, ".") + "}"
}

// evalJSONPath runs a parsed custom-column template against one object and
// returns the cell value as a string. Multiple matches (e.g. every container
// image) render space-joined ("nginx:1 redis:7"), matching `kubectl get -o
// jsonpath`; a missing path yields "" because the engine is built with
// AllowMissingKeys(true). Any execution error degrades to "" so a single bad
// row never aborts the join.
func evalJSONPath(jp *jsonpath.JSONPath, obj any) string {
	var buf bytes.Buffer
	if err := jp.Execute(&buf, obj); err != nil {
		return ""
	}
	return buf.String()
}
