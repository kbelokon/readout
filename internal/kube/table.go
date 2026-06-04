package kube

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// nestedString reads a string at the given path from a generic object map via
// the apimachinery accessor (empty when absent or non-string). A thin wrapper so
// the row-object reads in this file stay one-liners.
func nestedString(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func SortTable(table *Table, sortParam string) {
	if sortParam == "" {
		return
	}
	field, dir, _ := strings.Cut(sortParam, ":")
	reverse := dir == "desc"
	var less func(a, b Row) bool
	switch field {
	case "Created":
		less = func(a, b Row) bool { return created(a).Before(created(b)) }
	case "Age":
		less = func(a, b Row) bool { return created(a).After(created(b)) }
		if reverse {
			less = func(a, b Row) bool { return created(a).Before(created(b)) }
		}
		reverse = false
	default:
		idx := columnIndex(table.Columns, field)
		if idx < 0 {
			idx = 0
		}
		less = func(a, b Row) bool {
			av, bv := "", ""
			if idx < len(a.Cells) {
				av = fmt.Sprint(a.Cells[idx])
			}
			if idx < len(b.Cells) {
				bv = fmt.Sprint(b.Cells[idx])
			}
			if av == bv {
				return firstCell(a) < firstCell(b)
			}
			return av < bv
		}
	}
	sort.SliceStable(table.Rows, func(i, j int) bool {
		if reverse {
			return less(table.Rows[j], table.Rows[i])
		}
		return less(table.Rows[i], table.Rows[j])
	})
}

func AddLabelColumns(table *Table, spec string) {
	labels := splitCSV(spec)
	for i, label := range labels {
		name := titleLabel(label)
		if label == "*" {
			name = "Labels"
		}
		col := Column{Name: name, Description: label + " label", Label: label}
		insertColumn(table, i+1, &col)
	}
	for rowIdx := range table.Rows {
		labelsMap, _, _ := unstructured.NestedStringMap(table.Rows[rowIdx].Object, "metadata", "labels")
		for i, label := range labels {
			value := ""
			if label == "*" {
				var parts []string
				for k, v := range labelsMap {
					parts = append(parts, k+"="+v)
				}
				sort.Strings(parts)
				value = strings.Join(parts, ",")
			} else {
				value = labelsMap[label]
			}
			table.Rows[rowIdx].Cells = insertCell(table.Rows[rowIdx].Cells, i+1, value)
		}
	}
}

func titleLabel(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func RemoveColumns(table *Table, spec string) {
	hide := map[string]bool{}
	for _, name := range splitCSV(spec) {
		hide[name] = true
	}
	if len(hide) == 0 {
		return
	}
	var remove []int
	for i, col := range table.Columns {
		if hide["*"] || hide[col.Name] {
			remove = append(remove, i)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(remove)))
	for _, idx := range remove {
		table.Columns = append(table.Columns[:idx], table.Columns[idx+1:]...)
		for rowIdx := range table.Rows {
			if idx < len(table.Rows[rowIdx].Cells) {
				table.Rows[rowIdx].Cells = append(table.Rows[rowIdx].Cells[:idx], table.Rows[rowIdx].Cells[idx+1:]...)
			}
		}
	}
}

func FilterTable(table *Table, spec string, matchLabels bool) {
	if spec == "" {
		return
	}
	equals := map[string]string{}
	notEquals := map[string]map[string]bool{}
	var text []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			text = append(text, strings.ToLower(part))
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if strings.HasSuffix(key, "!") {
			key = strings.TrimSuffix(key, "!")
			if notEquals[key] == nil {
				notEquals[key] = map[string]bool{}
			}
			notEquals[key][val] = true
		} else {
			equals[key] = val
		}
	}

	eqIdx := map[int]string{}
	neqIdx := map[int]map[string]bool{}
	for name, val := range equals {
		idx := columnIndex(table.Columns, name)
		if idx < 0 {
			table.Rows = nil
			return
		}
		eqIdx[idx] = val
	}
	for name, vals := range notEquals {
		idx := columnIndex(table.Columns, name)
		if idx < 0 {
			table.Rows = nil
			return
		}
		neqIdx[idx] = vals
	}

	filtered := table.Rows[:0]
	for _, row := range table.Rows {
		if rowMatches(row, eqIdx, neqIdx, text, matchLabels) {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
}

func FilterRowsByNamespace(table *Table, include, exclude []*regexp.Regexp) {
	if len(include) == 0 && len(exclude) == 0 {
		return
	}
	filtered := table.Rows[:0]
	for _, row := range table.Rows {
		ns := nestedString(row.Object, "metadata", "namespace")
		if namespaceAllowed(ns, include, exclude) {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
}

// FilterSearchRowsByNamespace applies the include/exclude namespace filter the
// search path uses:
//   - both sets empty -> no-op (default config leaves results untouched);
//   - Kind == "Namespace" -> the row is filtered by its OWN name
//     (metadata.name), since a Namespace object's namespace is itself;
//   - a row with no metadata.namespace (cluster-scoped object) -> always
//     allowed (a non-namespaced object is never excluded by namespace);
//   - otherwise -> filtered by metadata.namespace.
//
// The per-namespace decision reuses namespaceAllowed (exclude-then-include),
// keeping the namespaced case consistent with the list path's
// FilterRowsByNamespace.
func FilterSearchRowsByNamespace(table *Table, include, exclude []*regexp.Regexp) {
	if len(include) == 0 && len(exclude) == 0 {
		return
	}
	isNamespaceKind := table.Resource.Kind == "Namespace"
	filtered := table.Rows[:0]
	for _, row := range table.Rows {
		meta, _ := row.Object["metadata"].(map[string]any)
		var allowed bool
		switch {
		case isNamespaceKind:
			allowed = namespaceAllowed(nestedString(row.Object, "metadata", "name"), include, exclude)
		case !hasKey(meta, "namespace"):
			// Cluster-scoped object: not namespaced, never namespace-excluded.
			allowed = true
		default:
			allowed = namespaceAllowed(nestedString(row.Object, "metadata", "namespace"), include, exclude)
		}
		if allowed {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
}

// hasKey reports whether m contains key (m may be nil). Used to distinguish a
// cluster-scoped object (no metadata.namespace key) from a namespaced object
// whose namespace happens to be empty.
func hasKey(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func MergeTables(left, right *Table) bool {
	if left.Resource.Plural != right.Resource.Plural {
		return false
	}
	leftNames := columnNames(left.Columns)
	rightNames := columnNames(right.Columns)
	if equalStrings(leftNames, rightNames) {
		left.Rows = append(left.Rows, right.Rows...)
		left.Clusters = append(left.Clusters, right.Clusters...)
		return true
	}
	for _, col := range right.Columns {
		if columnIndex(left.Columns, col.Name) < 0 {
			left.Columns = append(left.Columns, col)
		}
	}
	index := map[string]int{}
	for i, col := range left.Columns {
		index[col.Name] = i
	}
	for rowIdx := range left.Rows {
		for len(left.Rows[rowIdx].Cells) < len(left.Columns) {
			left.Rows[rowIdx].Cells = append(left.Rows[rowIdx].Cells, nil)
		}
	}
	for _, row := range right.Rows {
		newCells := make([]any, len(left.Columns))
		for i, name := range rightNames {
			if i < len(row.Cells) {
				newCells[index[name]] = row.Cells[i]
			}
		}
		row.Cells = newCells
		left.Rows = append(left.Rows, row)
	}
	left.Clusters = append(left.Clusters, right.Clusters...)
	return true
}

func GuessColumnClasses(table *Table) {
	if len(table.Rows) == 0 {
		return
	}
	for i, cell := range table.Rows[0].Cells {
		switch cell.(type) {
		case int, int64, float64, float32:
			if i < len(table.Columns) {
				table.Columns[i].Class = "num"
			}
		}
	}
}

// status is the row/cell status as a typed enum. The iota order IS the strength
// rank (neutral weakest, err strongest), so the strongest-wins comparison in
// rowStatus is a plain `>` and there is no separate rank map to keep in sync.
type status int

const (
	statusNeutral status = iota
	statusOK
	statusInfo
	statusWarn
	statusErr
)

// slug is the lowercase wire name used in the `row-status-<slug>` CSS class on a
// table row (consumed by readout.css's tr.row-status-* stripe rules); it must
// stay byte-stable.
func (s status) slug() string {
	switch s {
	case statusOK:
		return "ok"
	case statusInfo:
		return "info"
	case statusWarn:
		return "warn"
	case statusErr:
		return "err"
	default:
		return "neutral"
	}
}

// class is the Bulma text-color class for the phase-summary chip dot.
func (s status) class() string {
	switch s {
	case statusOK:
		return "has-text-success"
	case statusInfo:
		return "has-text-info"
	case statusWarn:
		return "has-text-warning"
	case statusErr:
		return "has-text-danger"
	default:
		return "has-text-grey"
	}
}

// label is the human-readable phase-summary chip label.
func (s status) label() string {
	switch s {
	case statusOK:
		return "OK"
	case statusInfo:
		return "Info"
	case statusWarn:
		return "Warning"
	case statusErr:
		return "Error"
	default:
		return "Neutral"
	}
}

// PhaseCount is one phase-summary tally: the chip's text-color class, its label,
// and the row/status count it represents.
type PhaseCount struct {
	Class string
	Label string
	Count int
}

// rowStatus is the strongest cell status across a row (the iota rank means the
// strongest wins by `>`). RowStatusClass and the generic PhaseSummary path both
// build on it.
func rowStatus(table *Table, row Row) status {
	strongest := statusNeutral
	for i, cell := range row.Cells {
		if i >= len(table.Columns) {
			continue
		}
		if s := cellStatus(table.Resource.Plural, table.Columns[i].Name, cell); s > strongest {
			strongest = s
		}
	}
	return strongest
}

func RowStatusClass(table *Table, row Row) string {
	s := rowStatus(table, row)
	if s == statusNeutral {
		return ""
	}
	return "row-status-" + s.slug()
}

func PhaseSummary(table *Table) []PhaseCount {
	if !hasCellFormatting(table.Resource.Plural) {
		return nil
	}
	if table.Resource.Plural == "pods" {
		statusIdx := columnIndex(table.Columns, "Status")
		if statusIdx >= 0 {
			counts := map[string]int{}
			classes := map[string]string{}
			var order []string
			for _, row := range table.Rows {
				label := "Unknown"
				if statusIdx < len(row.Cells) {
					raw := strings.TrimSpace(fmt.Sprint(row.Cells[statusIdx]))
					if raw != "" && raw != "<nil>" {
						label = raw
					}
				}
				if _, ok := counts[label]; !ok {
					order = append(order, label)
					classes[label] = CellClass(table.Resource.Plural, "Status", label)
				}
				counts[label]++
			}
			result := make([]PhaseCount, 0, len(order))
			for _, label := range order {
				result = append(result, PhaseCount{Class: classes[label], Label: label, Count: counts[label]})
			}
			return result
		}
	}
	counts := map[status]int{}
	for _, row := range table.Rows {
		counts[rowStatus(table, row)]++
	}
	var result []PhaseCount
	for _, s := range []status{statusErr, statusWarn, statusInfo, statusOK, statusNeutral} {
		if counts[s] > 0 {
			result = append(result, PhaseCount{Class: s.class(), Label: s.label(), Count: counts[s]})
		}
	}
	return result
}

func hasCellFormatting(plural string) bool {
	switch plural {
	case "events", "persistentvolumeclaims", "persistentvolumes", "nodes", "namespaces", "deployments", "pods":
		return true
	default:
		return false
	}
}

func created(row Row) time.Time {
	ts := nestedString(row.Object, "metadata", "creationTimestamp")
	t, _ := time.Parse(time.RFC3339, ts)
	return t
}

func firstCell(row Row) string {
	if len(row.Cells) == 0 {
		return ""
	}
	return fmt.Sprint(row.Cells[0])
}

func columnIndex(cols []Column, name string) int {
	for i, col := range cols {
		if col.Name == name {
			return i
		}
	}
	return -1
}

func insertColumn(table *Table, idx int, col *Column) {
	if idx >= len(table.Columns) {
		table.Columns = append(table.Columns, *col)
		return
	}
	table.Columns = append(table.Columns, Column{})
	copy(table.Columns[idx+1:], table.Columns[idx:])
	table.Columns[idx] = *col
}

func insertCell(cells []any, idx int, value any) []any {
	if idx >= len(cells) {
		return append(cells, value)
	}
	cells = append(cells, nil)
	copy(cells[idx+1:], cells[idx:])
	cells[idx] = value
	return cells
}

func rowMatches(row Row, eq map[int]string, neq map[int]map[string]bool, text []string, matchLabels bool) bool {
	for idx, expected := range eq {
		if idx >= len(row.Cells) || fmt.Sprint(row.Cells[idx]) != expected {
			return false
		}
	}
	for idx, forbidden := range neq {
		if idx < len(row.Cells) && forbidden[fmt.Sprint(row.Cells[idx])] {
			return false
		}
	}
	for _, needle := range text {
		found := false
		for _, cell := range row.Cells {
			if strings.Contains(strings.ToLower(fmt.Sprint(cell)), needle) {
				found = true
				break
			}
		}
		if !found && matchLabels {
			labels, _, _ := unstructured.NestedStringMap(row.Object, "metadata", "labels")
			for _, val := range labels {
				if strings.Contains(strings.ToLower(val), needle) {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// namespaceAllowed applies the exclude-then-include namespace decision against
// patterns that were compiled once at config load (config.Config holds the
// []*regexp.Regexp); there is no per-call compilation and no module-level cache.
func namespaceAllowed(ns string, include, exclude []*regexp.Regexp) bool {
	for _, re := range exclude {
		if re.MatchString(ns) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, re := range include {
		if re.MatchString(ns) {
			return true
		}
	}
	return false
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	var result []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func columnNames(cols []Column) []string {
	result := make([]string, len(cols))
	for i, col := range cols {
		result[i] = col.Name
	}
	return result
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func CellClass(plural, col string, cell any) string {
	value := strings.TrimSpace(fmt.Sprint(cell))
	switch plural {
	case "events":
		if col == "Type" && value == "Warning" {
			return "has-text-warning"
		}
		if col == "Reason" {
			switch value {
			case "BackOff", "BackoffLimitExceeded", "DeadlineExceeded", "Failed", "FailedComputeMetricsReplicas", "FailedGetResourceMetric", "FailedScheduling", "Preempted", "SystemOOM", "Unhealthy":
				return "has-text-danger"
			case "Killing", "Pulling":
				return "has-text-warning"
			case "Created", "Pulled", "Scheduled", "Started", "SuccessfulCreate":
				return "has-text-success"
			case "SawCompletedJob", "TriggeredScaleUp":
				return "has-text-info"
			}
		}
	case "persistentvolumeclaims":
		if col == "Status" {
			switch value {
			case "Pending":
				return "has-text-warning"
			case "Bound":
				return "has-text-success"
			}
		}
	case "persistentvolumes":
		if col == "Status" {
			switch value {
			case "Terminating":
				return "has-text-danger"
			case "Bound":
				return "has-text-success"
			}
		}
	case "nodes":
		if col == "Status" && value == "Ready" {
			return "has-text-success"
		}
	case "namespaces":
		if col == "Status" {
			switch value {
			case "Active":
				return "has-text-success"
			case "Terminating":
				// A stuck-Terminating namespace is operationally a warning; map it to
				// the warn tone so the redesign status dot reads `.ro-dot.warn`
				// (statusTone "has-text-warning" -> "warn").
				return "has-text-warning"
			}
		}
	case "deployments":
		if col == "Available" && value == "0" {
			return "has-text-danger"
		}
	case "pods":
		switch col {
		case "CPU Usage", "Memory Usage":
			if value == "0" {
				return "has-text-grey"
			}
		case "Restarts":
			restarts, ok := numericValue(cell)
			if !ok {
				return ""
			}
			if restarts < 1 {
				return "has-text-grey"
			}
			if restarts < 4 {
				return "has-text-warning"
			}
			return "has-text-danger"
		case "Status":
			switch value {
			case "Completed":
				return "has-text-info"
			case "ContainerCreating", "Init:0/1", "Pending", "PodInitializing", "Terminating":
				return "has-text-warning"
			case "CrashLoopBackOff", "CreateContainerConfigError", "ErrImagePull", "Error", "Evicted", "ImagePullBackOff", "Init:CrashLoopBackOff", "Init:CreateContainerConfigError", "Init:Error", "InvalidImageName", "OOMKilled", "OutOfcpu":
				return "has-text-danger"
			case "Running":
				return "has-text-success"
			}
		}
	}
	return ""
}

func numericValue(cell any) (float64, bool) {
	switch value := cell.(type) {
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	case float32:
		return float64(value), true
	case float64:
		return value, true
	default:
		return 0, false
	}
}

func cellStatus(plural, col string, cell any) status {
	switch CellClass(plural, col, cell) {
	case "has-text-success":
		return statusOK
	case "has-text-info":
		return statusInfo
	case "has-text-warning":
		return statusWarn
	case "has-text-danger":
		return statusErr
	default:
		return statusNeutral
	}
}
