package kube

import (
	"regexp"
	"testing"
)

// res compiles pattern strings into the []*regexp.Regexp the namespace filters
// take directly (the patterns are compiled once at config load).
func res(patterns ...string) []*regexp.Regexp {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

func TestTableBranchCoverageForFiltersStatusAndInsertion(t *testing.T) {
	table := sampleTable()
	SortTable(&table, "Missing")
	if got := table.Rows[0].Cells[0]; got != "api" {
		t.Fatalf("missing column sort should fall back to first cell, got %v", got)
	}
	RemoveColumns(&table, "*")
	if len(table.Columns) != 0 || len(table.Rows[0].Cells) != 0 {
		t.Fatalf("remove all columns failed: %#v %#v", table.Columns, table.Rows[0].Cells)
	}

	table = sampleTable()
	FilterTable(&table, "Status!=Running", false)
	if len(table.Rows) != 2 {
		t.Fatalf("not-equals filter rows = %#v", table.Rows)
	}
	table = sampleTable()
	FilterTable(&table, "Missing=value", false)
	if len(table.Rows) != 0 {
		t.Fatalf("unknown equals column should clear rows: %#v", table.Rows)
	}
	table = sampleTable()
	FilterTable(&table, "Missing!=value", false)
	if len(table.Rows) != 0 {
		t.Fatalf("unknown not-equals column should clear rows: %#v", table.Rows)
	}

	if !namespaceAllowed("prod-api", res("prod-.*"), res("kube-.*")) {
		t.Fatal("namespace should be allowed by include")
	}
	if namespaceAllowed("kube-system", nil, res("kube-.*")) {
		t.Fatal("namespace should be excluded")
	}
	if namespaceAllowed("dev", res("prod-.*"), nil) {
		t.Fatal("namespace should miss include")
	}

	table = Table{
		Resource: ResourceType{Plural: "events", Kind: "Event"},
		Columns:  []Column{{Name: "Type"}, {Name: "Reason"}, {Name: "Ready"}, {Name: "Restarts"}},
		Rows: []Row{
			{Cells: []any{"Warning", "FailedScheduling", "0/2", "4"}},
			{Cells: []any{"Normal", "Started", "2/2", "0"}},
			{Cells: []any{"", "", "1/2", "1"}},
			{Cells: []any{"", "", "", ""}},
		},
	}
	if RowStatusClass(&table, table.Rows[0]) != "row-status-err" {
		t.Fatalf("event error class = %q", RowStatusClass(&table, table.Rows[0]))
	}
	summary := PhaseSummary(&table)
	if len(summary) != 3 || summary[0].Label != "Error" || summary[1].Label != "OK" || summary[2].Label != "Neutral" {
		t.Fatalf("phase summary = %#v", summary)
	}
	if statusNeutral.label() != "Neutral" || statusNeutral != 0 || cellStatus("pods", "Other", "value") != statusNeutral {
		t.Fatal("status fallback mismatch")
	}
	if cellStatus("pods", "Restarts", "bad") != statusNeutral || cellStatus("pods", "Status", "Pending") != statusWarn {
		t.Fatal("status warning mismatch")
	}
	if CellClass("pods", "Restarts", "0") != "" {
		t.Fatal("a non-numeric restart count should stay unformatted")
	}
	if CellClass("pods", "Restarts", 0) != "has-text-grey" || CellClass("pods", "Restarts", 3) != "has-text-warning" || CellClass("pods", "Restarts", 4) != "has-text-danger" {
		t.Fatal("numeric restart count formatting mismatch")
	}

	table = Table{Columns: []Column{{Name: "Name"}}, Rows: []Row{{Cells: []any{"x"}, Object: map[string]any{"metadata": map[string]any{"labels": map[string]any{"z": "last", "a": "first"}}}}}}
	AddLabelColumns(&table, "first,second,third")
	if len(table.Columns) != 4 || len(table.Rows[0].Cells) != 4 {
		t.Fatalf("append label columns failed: %#v %#v", table.Columns, table.Rows[0].Cells)
	}
	if titleLabel("") != "" || firstCell(Row{}) != "" {
		t.Fatal("empty helper mismatch")
	}
}

// TestFilterSearchRowsByNamespace pins the search-path namespace filter: a
// Namespace-kind row is filtered by its OWN name, a cluster-scoped row (no
// metadata.namespace) is always allowed even with an include set, and a
// namespaced row is filtered by metadata.namespace. Both-sets-empty is a no-op.
func TestFilterSearchRowsByNamespace(t *testing.T) {
	nsRow := func(name string) Row {
		return Row{Cells: []any{name}, Object: map[string]any{"metadata": map[string]any{"name": name}}}
	}
	podRow := func(name, ns string) Row {
		return Row{Cells: []any{name}, Object: map[string]any{"metadata": map[string]any{"name": name, "namespace": ns}}}
	}
	clusterScopedRow := func(name string) Row { // e.g. a Node: no metadata.namespace key
		return Row{Cells: []any{name}, Object: map[string]any{"metadata": map[string]any{"name": name}}}
	}

	// (1) Namespace kind: filtered by the namespace's OWN name (exclude kube-*).
	nsTable := Table{Resource: ResourceType{Kind: "Namespace", Plural: "namespaces"}, Rows: []Row{nsRow("default"), nsRow("kube-system"), nsRow("prod")}}
	FilterSearchRowsByNamespace(&nsTable, nil, res("kube-.*"))
	gotNS := []string{firstCell(nsTable.Rows[0]), firstCell(nsTable.Rows[1])}
	if len(nsTable.Rows) != 2 || gotNS[0] != "default" || gotNS[1] != "prod" {
		t.Fatalf("Namespace-kind filter = %v, want [default prod]", []string{firstCell(nsTable.Rows[0])})
	}

	// (2) Cluster-scoped object (Node): no metadata.namespace -> always allowed,
	// even when an include set is given (a non-namespaced object is never excluded).
	nodeTable := Table{Resource: ResourceType{Kind: "Node", Plural: "nodes"}, Rows: []Row{clusterScopedRow("worker-1"), clusterScopedRow("worker-2")}}
	FilterSearchRowsByNamespace(&nodeTable, res("prod-.*"), nil)
	if len(nodeTable.Rows) != 2 {
		t.Fatalf("cluster-scoped rows must survive an include filter, got %d", len(nodeTable.Rows))
	}

	// (3) Namespaced object: filtered by metadata.namespace (include prod-*).
	podTable := Table{Resource: ResourceType{Kind: "Pod", Plural: "pods"}, Rows: []Row{podRow("a", "prod-api"), podRow("b", "dev"), podRow("c", "prod-web")}}
	FilterSearchRowsByNamespace(&podTable, res("prod-.*"), nil)
	if len(podTable.Rows) != 2 || firstCell(podTable.Rows[0]) != "a" || firstCell(podTable.Rows[1]) != "c" {
		t.Fatalf("namespaced filter = %v, want [a c]", []string{firstCell(podTable.Rows[0]), firstCell(podTable.Rows[1])})
	}

	// (4) Both sets empty: no-op (default config leaves results untouched).
	noop := Table{Resource: ResourceType{Kind: "Pod", Plural: "pods"}, Rows: []Row{podRow("a", "kube-system"), podRow("b", "dev")}}
	FilterSearchRowsByNamespace(&noop, nil, nil)
	if len(noop.Rows) != 2 {
		t.Fatalf("empty include/exclude must be a no-op, got %d rows", len(noop.Rows))
	}
}
