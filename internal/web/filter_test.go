package web

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
)

// filter_test.go pins the Filters v2 server engine (D7): the `?f=` chip
// grammar exactly as pinned in the design decision. Every case here resolves
// one grammar ambiguity that was a review finding -- the raw-comma OR split,
// the first-operator field/value split, the quantity alias binding, the
// kubectl-age two-unit tokens -- so none of them can silently regress.

// filterTestTable is a crafted pods table mirroring the prototype's
// corner-case rows (docs/design_handoff_readout_v2/js/data-extra.js): a
// decorated restarts cell, a thousands-separator restart count, one- and
// two-unit age tokens, an Init:N/M status (a value containing the `:`
// operator), a multi-word column, a comma-bearing cell, and row-object labels.
func filterTestTable() kube.Table {
	row := func(name, ready, status, restarts, age, node, selector string, labels map[string]any) kube.Row {
		meta := map[string]any{"name": name, "namespace": "default"}
		if labels != nil {
			meta["labels"] = labels
		}
		return kube.Row{
			Cells:   []any{name, ready, status, restarts, age, node, selector},
			Object:  map[string]any{"metadata": meta},
			Cluster: "test",
		}
	}
	return kube.Table{
		Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true},
		Columns: []kube.Column{
			{Name: "Name"},
			{Name: "Ready"},
			{Name: "Status"},
			{Name: "Restarts"},
			{Name: "Age"},
			{Name: "Nominated Node"},
			{Name: "Selector"},
		},
		Rows: []kube.Row{
			row("api-1", "1/1", "Running", "0", "59s", "worker-1", "app=a,env=b", map[string]any{"app.kubernetes.io/name": "api"}),
			row("crash-1", "0/1", "CrashLoopBackOff", "3 (4m ago)", "3h", "worker-2", "app=a", map[string]any{"app.kubernetes.io/name": "crashy"}),
			row("pending-1", "0/1", "Pending", "0", "4m12s", "", "", nil),
			row("old-1", "1/1", "Running", "1,047 (4m ago)", "2d3h", "worker-2", "", map[string]any{"app": "old"}),
			row("init-1", "0/3", "Init:1/2", "0", "5m33s", "", "", nil),
			row("noage-1", "1/1", "Running", "0", "<unknown>", "", "", nil),
		},
	}
}

// runFilter drives one query through the REAL applyTableOptions pipeline
// (legacy params, f chips, sort, limit -- in production order) and returns the
// surviving row names. Callers hand over a fresh table per call (the pipeline
// mutates it).
func runFilter(t *testing.T, plural, query string, table *kube.Table) []string {
	t.Helper()
	app := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/"+plural+"?"+query, nil)
	r.SetPathValue("plural", plural)
	app.applyTableOptions(r, nil, table, "default", false)
	names := make([]string, 0, len(table.Rows))
	for _, row := range table.Rows {
		names = append(names, cellDisplayString(row.Cells[0]))
	}
	return names
}

func TestFilterChipParsing(t *testing.T) {
	cases := []struct {
		name     string
		rawQuery string
		want     []filterChip
	}{
		{
			"substring", "f=status:Run",
			[]filterChip{{Field: "status", Op: opContains, Values: []string{"Run"}, Raw: "status:Run"}},
		},
		{
			"not equals", "f=status!=Running",
			[]filterChip{{Field: "status", Op: opNotContains, Values: []string{"Running"}, Raw: "status!=Running"}},
		},
		{
			"encoded not equals", "f=status%21%3DRunning",
			[]filterChip{{Field: "status", Op: opNotContains, Values: []string{"Running"}, Raw: "status%21%3DRunning"}},
		},
		{
			"greater", "f=restarts>0",
			[]filterChip{{Field: "restarts", Op: opGreater, Values: []string{"0"}, Raw: "restarts>0"}},
		},
		{
			"less raw", "f=age<1h",
			[]filterChip{{Field: "age", Op: opLess, Values: []string{"1h"}, Raw: "age<1h"}},
		},
		{
			"less encoded", "f=age%3C1h",
			[]filterChip{{Field: "age", Op: opLess, Values: []string{"1h"}, Raw: "age%3C1h"}},
		},
		{
			"first operator splits, value keeps =", "f=label:app.kubernetes.io/name=api",
			[]filterChip{{Field: "label", Op: opContains, Values: []string{"app.kubernetes.io/name=api"}, Raw: "label:app.kubernetes.io/name=api"}},
		},
		{
			"raw comma is OR", "f=status:Running,Pending",
			[]filterChip{{Field: "status", Op: opContains, Values: []string{"Running", "Pending"}, Raw: "status:Running,Pending"}},
		},
		{
			"%2C is a literal comma inside one alternative", "f=note:a%2Cb",
			[]filterChip{{Field: "note", Op: opContains, Values: []string{"a,b"}, Raw: "note:a%2Cb"}},
		},
		{
			"plus decodes to space in field", "f=nominated+node:worker",
			[]filterChip{{Field: "nominated node", Op: opContains, Values: []string{"worker"}, Raw: "nominated+node:worker"}},
		},
		{
			"trailing comma dropped", "f=status:Running,",
			[]filterChip{{Field: "status", Op: opContains, Values: []string{"Running"}, Raw: "status:Running,"}},
		},
		{
			"empty value is a match-all no-op", "f=status:",
			[]filterChip{{Field: "status", Op: opContains, Values: []string{""}, Raw: "status:"}},
		},
		{
			"no operator means malformed", "f=oops",
			[]filterChip{{Op: "", Values: []string{"oops"}, Raw: "oops"}},
		},
		{
			"repeatable param", "f=a:1&sort=Name&f=b:2",
			[]filterChip{
				{Field: "a", Op: opContains, Values: []string{"1"}, Raw: "a:1"},
				{Field: "b", Op: opContains, Values: []string{"2"}, Raw: "b:2"},
			},
		},
		{"empty and bare f skipped", "f=&f", nil},
		{"other keys never match", "ref=x&filter=y", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseFilterParams(tc.rawQuery)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseFilterParams(%q) = %#v, want %#v", tc.rawQuery, got, tc.want)
			}
		})
	}
}

func TestFilterRowMatching(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"substring is case-insensitive partial", "f=status:Run", "api-1,old-1,noage-1"},
		{"substring lowercase input", "f=status:run", "api-1,old-1,noage-1"},
		{"value containing the : operator survives", "f=status:Init:1/2", "init-1"},
		{"comma is OR", "f=status:Running,Pending", "api-1,pending-1,old-1,noage-1"},
		{"literal %2C comma stays inside one alternative", "f=selector:app%3Da%2Cenv%3Db", "api-1"},
		{"same text with raw comma splits into OR", "f=selector:app=a,env=b", "api-1,crash-1"},
		{"!= negates the substring", "f=status!=Running", "crash-1,pending-1,init-1"},
		{"restarts>0 parses the decorated leading token", "f=restarts>0", "crash-1,old-1"},
		{"thousands separator stripped (1,047)", "f=restarts>100", "old-1"},
		{"age<1h: one-unit and two-unit cells, unparseable excluded", "f=age<1h", "api-1,pending-1,init-1"},
		{"age<3h: 2d3h is 51h, not 3h-ish; boundary not <", "f=age<3h", "api-1,pending-1,init-1"},
		{"age>1d: two-unit token 2d3h compares as 51h", "f=age>1d", "old-1"},
		{"label key=value", "f=label:app.kubernetes.io/name=api", "api-1"},
		{"label bare key is existence", "f=label:app.kubernetes.io/name", "api-1,crash-1"},
		{"label != negates", "f=label!=app.kubernetes.io/name=api", "crash-1,pending-1,old-1,init-1,noage-1"},
		{"multi-word column with dash", "f=nominated-node:worker-2", "crash-1,old-1"},
		{"multi-word column with space", "f=nominated+node:worker-1", "api-1"},
		{"field resolution is case-insensitive", "f=NAME:API", "api-1"},
		{"unknown field matches zero rows", "f=bogus:x", ""},
		{"malformed chip matches zero rows", "f=oops", ""},
		{"chips AND-combine", "f=status:Run&f=restarts>0", "old-1"},
		{"empty value is a no-op chip", "f=status:", "api-1,crash-1,pending-1,old-1,init-1,noage-1"},
		{"sort runs after filtering", "f=status:Run&sort=Name", "api-1,noage-1,old-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := filterTestTable()
			got := strings.Join(runFilter(t, "pods", tc.query, &table), ",")
			if got != tc.want {
				t.Fatalf("%s rows = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestFilterQuantityAlias pins the cpu/memory alias contract: the fields bind
// ONLY to the joined "CPU Usage"/"Memory Usage" float columns (RHS converted
// via resource.ParseQuantity: 500m -> 0.5 cores, 100Mi -> bytes), NEVER to the
// nodes' plain "CPU"/"Memory" capacity columns; with no metrics join the chip
// is an unknown field and matches zero rows.
func TestFilterQuantityAlias(t *testing.T) {
	metricsRow := func(name string, cpu, mem float64) kube.Row {
		return kube.Row{
			Cells:  []any{name, cpu, mem},
			Object: map[string]any{"metadata": map[string]any{"name": name, "namespace": "default"}},
		}
	}
	metricsJoined := func() kube.Table {
		return kube.Table{
			Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true},
			Columns:  []kube.Column{{Name: "Name"}, {Name: "CPU Usage"}, {Name: "Memory Usage"}},
			Rows: []kube.Row{
				metricsRow("hot", 0.75, 200*1024*1024),
				metricsRow("cold", 0.25, 50*1024*1024),
			},
		}
	}
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"cpu>500m converts millicores", "f=cpu>500m", "hot"},
		{"cpu<500m converts millicores", "f=cpu<500m", "cold"},
		{"memory>100Mi converts bytes", "f=memory>100Mi", "hot"},
		{"memory<100Mi converts bytes", "f=memory<100Mi", "cold"},
		{"alias is case-insensitive", "f=Memory>100Mi", "hot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := metricsJoined()
			got := strings.Join(runFilter(t, "pods", tc.query, &table), ",")
			if got != tc.want {
				t.Fatalf("%s rows = %q, want %q", tc.query, got, tc.want)
			}
		})
	}

	// Metrics join absent: the alias is an unknown field -> zero rows, even
	// though the substring grammar could have matched something.
	noMetrics := filterTestTable()
	if got := runFilter(t, "pods", "f=cpu>0", &noMetrics); len(got) != 0 {
		t.Fatalf("cpu>0 without a metrics join matched %v, want zero rows (unknown field)", got)
	}

	// A nodes table with ONLY capacity columns: `cpu`/`memory` must NOT bind
	// to them. Capacity 8 cores / 16Gi would satisfy >0 if the alias mis-bound.
	capacityNode := kube.Table{
		Resource: kube.ResourceType{Plural: "nodes", Kind: "Node"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "CPU"}, {Name: "Memory"}},
		Rows: []kube.Row{{
			Cells:  []any{"big", "8", "16Gi"},
			Object: map[string]any{"metadata": map[string]any{"name": "big"}},
		}},
	}
	for _, query := range []string{"f=cpu>0", "f=memory>0"} {
		table := capacityNode
		if got := runFilter(t, "nodes", query, &table); len(got) != 0 {
			t.Fatalf("%s on a capacity-only nodes table matched %v, want zero rows (alias never binds capacity)", query, got)
		}
	}

	// A nodes table carrying BOTH capacity and usage columns: the alias binds
	// the usage column (0.4 cores), never capacity (8 cores). cpu<500m matches
	// through usage and would not through capacity; cpu>500m is the inverse.
	both := func() kube.Table {
		return kube.Table{
			Resource: kube.ResourceType{Plural: "nodes", Kind: "Node"},
			Columns:  []kube.Column{{Name: "Name"}, {Name: "CPU"}, {Name: "Memory"}, {Name: "CPU Usage"}, {Name: "Memory Usage"}},
			Rows: []kube.Row{{
				Cells:  []any{"big", "8", "16Gi", 0.4, 1024.0 * 1024 * 1024},
				Object: map[string]any{"metadata": map[string]any{"name": "big"}},
			}},
		}
	}
	lessTable := both()
	if got := runFilter(t, "nodes", "f=cpu<500m", &lessTable); strings.Join(got, ",") != "big" {
		t.Fatalf("cpu<500m bound the wrong column: rows = %v, want big via the 0.4-core usage cell", got)
	}
	greaterTable := both()
	if got := runFilter(t, "nodes", "f=cpu>500m", &greaterTable); len(got) != 0 {
		t.Fatalf("cpu>500m matched %v: the alias bound the 8-core CAPACITY column, want zero rows via the 0.4-core usage cell", got)
	}
}

// TestFilterFullDatasetBeforeLimit proves f filtering runs on the full dataset
// BEFORE any limit: limit=1 keeps the first MATCHING row, not the first row.
func TestFilterFullDatasetBeforeLimit(t *testing.T) {
	table := filterTestTable()
	got := runFilter(t, "pods", "f=status:Pending&limit=1", &table)
	if strings.Join(got, ",") != "pending-1" {
		t.Fatalf("f=status:Pending&limit=1 rows = %v, want pending-1 (filter must run before limit)", got)
	}
}

// TestFilterMultiTypePagesIgnoreF pins the D1 boundary: a multi-type page that
// receives `f` anyway IGNORES it -- no 500, no surprise-empty tables.
func TestFilterMultiTypePagesIgnoreF(t *testing.T) {
	app := newTestServer(t)
	p := get(t, app, "/clusters/test/namespaces/default/pods,services?f=status:NoSuchStatus", http.StatusOK)
	names := p.texts("td.cell-name")
	if !contains(names, "nginx") || !contains(names, "my-app") {
		t.Fatalf("multi-type page applied f: rows = %v, want nginx and my-app kept (f ignored)", names)
	}
}

// TestFilterLegacyCoexistence proves both grammars run together: the legacy
// `?filter=` free text AND an `f` chip both apply, and `?selector=` rides the
// same request as `f` (forwarded to the API + round-tripped into the tools
// form) while f filters the rows.
func TestFilterLegacyCoexistence(t *testing.T) {
	app := newTestServer(t)

	p := get(t, app, "/clusters/test/namespaces/default/pods?filter=nginx&f=status:Run", http.StatusOK)
	if names := p.texts("td.cell-name"); strings.Join(names, ",") != "nginx" {
		t.Fatalf("filter=nginx&f=status:Run rows = %v, want nginx only (both grammars AND)", names)
	}

	p2 := get(t, app, "/clusters/test/namespaces/default/pods?selector=app%3Dnginx&f=name:my", http.StatusOK)
	if names := p2.texts("td.cell-name"); strings.Join(names, ",") != "my-app" {
		t.Fatalf("selector+f rows = %v, want my-app only (f applied alongside selector)", names)
	}
	p2.wantAttr(`form.tools-form input[name="selector"]`, "value", "app=nginx")
	// Both params travel together on the rebuilt sort hrefs.
	if !p2.containsHref("thead th a", "/clusters/test/namespaces/default/pods?f=name%3Amy&selector=app%3Dnginx&sort=Name") {
		t.Fatalf("sort hrefs lost a grammar: %v", p2.attrs("thead th a", "href"))
	}
}

// TestFilterEmptyFilteredStateChips proves a chip-emptied list renders the
// empty-FILTERED state with one removable chip per `f` param: each ✕ drops
// exactly that raw occurrence (the sibling chip stays byte-identical) and
// Clear filters drops the whole set. The unknown `node` field doubles as the
// end-to-end strict zero-rows proof.
func TestFilterEmptyFilteredStateChips(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods?f=status:Gone&f=node:zzz", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-empty-row .ro-empty-lg h3", "No Pod objects match your filters")
	chips := strings.Join(p.texts(".ro-empty-row .ro-scope .ro-scope-chip"), " | ")
	if !strings.Contains(chips, "status:Gone") || !strings.Contains(chips, "node:zzz") {
		t.Fatalf("empty-filtered chips %q do not name both f chips", chips)
	}
	removeHrefs := p.attrs(".ro-empty-row .ro-scope .ro-scope-chip a.retry", "href")
	if !contains(removeHrefs, "/clusters/test/namespaces/default/pods?f=node:zzz") {
		t.Fatalf("removing the first chip must keep the second raw: %v", removeHrefs)
	}
	if !contains(removeHrefs, "/clusters/test/namespaces/default/pods?f=status:Gone") {
		t.Fatalf("removing the second chip must keep the first raw: %v", removeHrefs)
	}
	p.wantAttr(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "href", "/clusters/test/namespaces/default/pods")
}

// TestFilterSortHrefKeepsRawCommas pins the survival of the raw OR-comma
// across server-rebuilt hrefs: a sort-header click must not re-encode the
// comma to %2C (which would collapse the OR into a single literal-comma
// alternative on the next request).
func TestFilterSortHrefKeepsRawCommas(t *testing.T) {
	app := newTestServer(t)
	p := get(t, app, "/clusters/test/namespaces/default/pods?f=status:Running,Pending", http.StatusOK)
	if n := p.count("tbody tr td.cell-name"); n != 2 {
		t.Fatalf("f=status:Running,Pending matched %d rows, want both Running fixture pods", n)
	}
	want := "/clusters/test/namespaces/default/pods?f=status%3ARunning,Pending&sort=Name"
	if !p.containsHref("thead th a", want) {
		t.Fatalf("sort hrefs re-encoded the OR comma: %v", p.attrs("thead th a", "href"))
	}
	// The rebuilt href parses back to the same two-alternative chip.
	chips := parseFilterParams("f=status%3ARunning,Pending")
	if len(chips) != 1 || !reflect.DeepEqual(chips[0].Values, []string{"Running", "Pending"}) {
		t.Fatalf("rebuilt href round-trip = %#v, want one chip with OR values Running|Pending", chips)
	}
}
