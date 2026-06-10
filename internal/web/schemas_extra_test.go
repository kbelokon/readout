package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
)

// schemas_extra_test.go pins the Unit-11 rich schemas (D5, SPEC §7.8–7.10):
// the curated column decorators for services / configmaps / secrets /
// ingresses and the per-kind cell wiring onto the Unit-10 constructors.
// Every expectation is an INDEPENDENT fact about how the Kubernetes object /
// printer cell maps onto the redesign vocabulary (ClusterIP None verbatim,
// ExternalName target in External-IP, <pending> LB pulses, multi-port +N,
// keys as `name · size` chips with DECODED secret sizes, ingress TLS from
// spec.tls) -- driven through the REAL pipeline (applyTableOptions /
// buildCellView / buildListView / templ render), never re-implemented here.
// The secret-safety law gets the render-and-search proof: a known fixture
// value and its base64 form must appear NOWHERE in the rendered list HTML.

// serviceObject builds a Service row object carrying the spec/status the rich
// service cells and decorateServiceColumns read.
func serviceObject(name, svcType string, spec map[string]any, status map[string]any) map[string]any {
	spec["type"] = svcType
	obj := map[string]any{
		"kind":       "Service",
		"apiVersion": "v1",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         "default",
			"creationTimestamp": "2026-06-01T00:00:00Z",
		},
		"spec": spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

// servicesTable builds a crafted services kube.Table in the REAL printer shape
// (5 columns, full objects) so decorateServiceColumns has columns to append.
func servicesTable(rows []kube.Row) *kube.Table {
	return &kube.Table{
		Resource: kube.ResourceType{Plural: "services", Kind: "Service", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Type"}, {Name: "Cluster-IP"}, {Name: "Port(s)"}, {Name: "Age"},
		},
		Rows: rows,
	}
}

// schemaCellView runs the real buildCellView for one cell of a crafted table,
// the same end-to-end seam the deployment/node cell tests use.
func schemaCellView(t *testing.T, table *kube.Table, row kube.Row, colIdx int) cellView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/"+table.Resource.Plural, nil)
	name := nestedString(row.Object, "metadata", "name")
	return app.buildCellView(req, table, row, colIdx, row.Cells[colIdx], "default", name)
}

// TestServiceColumnsDecoration pins decorateServiceColumns: a printer-shaped
// 5-column Table gains External-IP and Selector columns whose cells are
// reconstructed from each row's spec/status with the UPSTREAM printer's
// encoding -- <none> for a plain ClusterIP service, the externalName target
// for ExternalName, the LB ingress IP when provisioned, the literal <pending>
// while not, and the sorted comma-joined k=v selector. Columns the server
// already provided are never duplicated.
func TestServiceColumnsDecoration(t *testing.T) {
	rows := []kube.Row{
		{Cluster: "test", Object: serviceObject("plain", "ClusterIP", map[string]any{
			"clusterIP": "10.96.0.10",
			"selector":  map[string]any{"app": "plain", "tier": "web"},
		}, nil), Cells: []any{"plain", "ClusterIP", "10.96.0.10", "80/TCP", "12d"}},
		{Cluster: "test", Object: serviceObject("legacy-billing", "ExternalName", map[string]any{
			"externalName": "billing.legacy.internal",
		}, nil), Cells: []any{"legacy-billing", "ExternalName", "", "<none>", "1y"}},
		{Cluster: "test", Object: serviceObject("lb-live", "LoadBalancer", map[string]any{
			"clusterIP": "10.96.44.10",
			"selector":  map[string]any{"app": "ingress"},
		}, map[string]any{
			"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "45.55.107.21"}}},
		}), Cells: []any{"lb-live", "LoadBalancer", "10.96.44.10", "80:31245/TCP", "17d"}},
		{Cluster: "test", Object: serviceObject("lb-cold", "LoadBalancer", map[string]any{
			"clusterIP": "10.96.61.55",
			"selector":  map[string]any{"app": "preview"},
		}, map[string]any{"loadBalancer": map[string]any{}}), Cells: []any{"lb-cold", "LoadBalancer", "10.96.61.55", "80:30412/TCP", "6m"}},
	}
	table := servicesTable(rows)
	decorateServiceColumns(table)

	extIdx := columnIndex(table.Columns, "External-IP")
	selIdx := columnIndex(table.Columns, "Selector")
	if extIdx < 0 || selIdx < 0 {
		t.Fatalf("decorated columns missing: External-IP=%d Selector=%d", extIdx, selIdx)
	}
	wantExt := []string{"<none>", "billing.legacy.internal", "45.55.107.21", "<pending>"}
	wantSel := []string{"app=plain,tier=web", "<none>", "app=ingress", "app=preview"}
	for i, row := range table.Rows {
		if got := cellString(row, extIdx); got != wantExt[i] {
			t.Fatalf("row %d External-IP = %q, want %q", i, got, wantExt[i])
		}
		if got := cellString(row, selIdx); got != wantSel[i] {
			t.Fatalf("row %d Selector = %q, want %q", i, got, wantSel[i])
		}
		// Lockstep invariant: every row gained exactly the two appended cells.
		if len(row.Cells) != len(table.Columns) {
			t.Fatalf("row %d has %d cells for %d columns (table went ragged)", i, len(row.Cells), len(table.Columns))
		}
	}

	// Idempotence / no-duplication: a second pass (or a server that already
	// provides the columns) appends nothing.
	decorateServiceColumns(table)
	names := map[string]int{}
	for _, col := range table.Columns {
		names[col.Name]++
	}
	if names["External-IP"] != 1 || names["Selector"] != 1 {
		t.Fatalf("decoration duplicated columns: %v", names)
	}
}

// TestServiceCells pins the SPEC §7.8 service cell mapping through the real
// buildCellView: ClusterIP None stays the verbatim generic cell, the
// External-IP corner states resolve through the pending cell (none faint /
// pending pulsing / ExternalName target verbatim), the multi-port list shows
// 2 + "+N" with the full list in the tooltip, and the Selector column renders
// neutral chips read from spec.selector with NO click-to-filter href.
func TestServiceCells(t *testing.T) {
	headless := serviceObject("redis-headless", "ClusterIP", map[string]any{
		"clusterIP": "None",
		"selector":  map[string]any{"app.kubernetes.io/name": "redis"},
	}, nil)
	multiPort := serviceObject("observability-metrics", "ClusterIP", map[string]any{
		"clusterIP": "10.96.9.17",
		"selector":  map[string]any{"app.kubernetes.io/part-of": "observability"},
	}, nil)

	table := servicesTable([]kube.Row{
		{Cluster: "test", Object: headless, Cells: []any{"redis-headless", "ClusterIP", "None", "6379/TCP", "17d"}},
		{Cluster: "test", Object: multiPort, Cells: []any{"observability-metrics", "ClusterIP", "10.96.9.17", "9090/TCP,9091/TCP,9100/TCP,8443/TCP,6060/TCP", "12d"}},
	})
	decorateServiceColumns(table)
	extIdx := columnIndex(table.Columns, "External-IP")
	selIdx := columnIndex(table.Columns, "Selector")

	// ClusterIP None: verbatim mono -- the generic plain cell, never reskinned.
	cv := schemaCellView(t, table, table.Rows[0], 2)
	if cv.Kind != cellPlain || cv.Value != "None" {
		t.Fatalf("headless Cluster-IP cell = kind %v value %q, want the verbatim plain None", cv.Kind, cv.Value)
	}

	// External-IP <none>: the pending cell's faint none state (no pulse).
	cv = schemaCellView(t, table, table.Rows[0], extIdx)
	if cv.Kind != cellPending || cv.Value != "" || cv.Pulse {
		t.Fatalf("none External-IP cell = %#v, want cellPending empty value, no pulse", cv)
	}

	// Multi-port: first 2 + "+3", FULL comma-joined list in the tooltip.
	cv = schemaCellView(t, table, table.Rows[1], 3)
	if cv.Kind != cellPorts {
		t.Fatalf("Port(s) cell kind = %v, want cellPorts", cv.Kind)
	}
	if cv.Value != "9090/TCP, 9091/TCP" || cv.More != "+3" {
		t.Fatalf("ports cell = value %q more %q, want first-2 + +3", cv.Value, cv.More)
	}
	if cv.Title != "9090/TCP, 9091/TCP, 9100/TCP, 8443/TCP, 6060/TCP" {
		t.Fatalf("ports tooltip = %q, want the full list", cv.Title)
	}

	// Selector: neutral chips from spec.selector, inert (no filter href --
	// label:k=v filters the services' OWN labels, not what they select).
	cv = schemaCellView(t, table, table.Rows[0], selIdx)
	if cv.Kind != cellChips || len(cv.Chips) != 1 {
		t.Fatalf("selector cell = %#v, want one chip", cv)
	}
	if cv.Chips[0].Key != "app.kubernetes.io/name" || cv.Chips[0].Val != "redis" || cv.Chips[0].Href != "" {
		t.Fatalf("selector chip = %#v, want app.kubernetes.io/name:redis with NO href", cv.Chips[0])
	}
}

// TestServiceCellsPendingAndExternalNameRender drives the LB/ExternalName
// corner rows through the FULL pipeline (decorate -> buildListView -> templ)
// and asserts the DOM: the <pending> LB renders the amber PULSING dot + the
// word "pending" (law §1.3: an in-flight state), the ExternalName target
// renders verbatim in the External-IP column, and a portless service shows
// the muted "—".
func TestServiceCellsPendingAndExternalNameRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	table := servicesTable([]kube.Row{
		{Cluster: "test", Object: serviceObject("preview-env-lb", "LoadBalancer", map[string]any{
			"clusterIP": "10.96.61.55",
			"selector":  map[string]any{"app": "preview"},
		}, map[string]any{"loadBalancer": map[string]any{}}), Cells: []any{"preview-env-lb", "LoadBalancer", "10.96.61.55", "80:30412/TCP", "6m"}},
		{Cluster: "test", Object: serviceObject("legacy-billing", "ExternalName", map[string]any{
			"externalName": "billing.legacy.internal",
		}, nil), Cells: []any{"legacy-billing", "ExternalName", "", "<none>", "1y"}},
	})
	decorateServiceColumns(table)
	lc := &listContext{Cluster: "test", Namespace: "default", Plural: "services", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/services", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	pendingRow := doc.Find(`table.ro-table tr:has(td.cell-name a:contains("preview-env-lb"))`)
	pendingCell := pendingRow.Find(".cell-status.warn")
	if pendingCell.Length() != 1 || normSpace(pendingCell.Text()) != "pending" {
		t.Fatalf("pending LB cell = %q, want the warn 'pending' status cell", normSpace(pendingCell.Text()))
	}
	if pendingCell.Find(".ro-dot.warn.pulse").Length() != 1 {
		t.Fatalf("pending LB dot must PULSE (a transitioning state, law §1.3)")
	}

	extName := doc.Find(`table.ro-table tr:has(td.cell-name a:contains("legacy-billing"))`)
	if !strings.Contains(normSpace(extName.Text()), "billing.legacy.internal") {
		t.Fatalf("ExternalName row missing its target in External-IP: %q", normSpace(extName.Text()))
	}
	// The portless ExternalName service renders the muted "—" in Port(s).
	if !strings.Contains(extName.Text(), "—") {
		t.Fatalf("portless service should render the muted — for Port(s): %q", normSpace(extName.Text()))
	}
	// No status dot anywhere except the single pending pulse: errors/health are
	// not invented for services (only one warn dot in the whole table).
	if got := doc.Find("table.ro-table .ro-dot").Length(); got != 1 {
		t.Fatalf("services table dots = %d, want exactly the one pending pulse", got)
	}
}

// TestServiceListThroughHandler drives the fakeapi services fixture through
// the REAL handler chain: the 5-column printer Table gains the decorated
// External-IP + Selector columns (applyTableOptions calls the decorator), the
// frontend row renders its spec.selector as an inert neutral chip, the
// selectorless kubernetes row shows the muted "—", and the External-IP cells
// show the faint <none> (ClusterIP services).
func TestServiceListThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/services", http.StatusOK)

	headers := p.texts("table.ro-table thead th")
	joined := strings.Join(headers, "|")
	for _, want := range []string{"External-IP", "Selector"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("decorated column %q missing from headers %v", want, headers)
		}
	}

	frontend := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/services/frontend"])`)
	chip := frontend.Find(".ro-chips .ro-chip")
	if chip.Length() != 1 {
		t.Fatalf("frontend selector chips = %d, want 1", chip.Length())
	}
	if got := normSpace(chip.Text()); got != "app:frontend" {
		t.Fatalf("frontend selector chip = %q, want app:frontend", got)
	}
	if chip.Is("a") || chip.Find("a").Length() > 0 {
		t.Fatalf("selector chips must be INERT (no click-to-filter href)")
	}
	// The faint <none> external IP for a plain ClusterIP service.
	if frontend.Find("td span.faint:contains('<none>')").Length() == 0 {
		t.Fatalf("frontend External-IP should render the faint <none>: %q", normSpace(frontend.Text()))
	}

	kubernetes := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/services/kubernetes"])`)
	if !strings.Contains(kubernetes.Text(), "—") {
		t.Fatalf("selectorless kubernetes service should render the muted — selector cell: %q", normSpace(kubernetes.Text()))
	}
}

// TestConfigMapKeysCells pins the configmap keys decode (SPEC §4.10): `data`
// keys sized by their value's byte length, `binaryData` keys by the DECODED
// length (the wire form is base64 -- 88 encoded chars of a 64-byte blob must
// read "64 B"), merged and sorted; an empty configmap yields no chips (the
// renderer's muted "—"). Driven through the real buildCellView.
func TestConfigMapKeysCells(t *testing.T) {
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "configmaps", Kind: "ConfigMap", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Data"}, {Name: "Age"}},
	}
	obj := map[string]any{
		"kind": "ConfigMap", "apiVersion": "v1",
		"metadata": map[string]any{"name": "app-config", "namespace": "default", "creationTimestamp": "2026-06-01T00:00:00Z"},
		"data": map[string]any{
			"logging.conf": "level=info\n",                  // 11 bytes
			"big.yaml":     strings.Repeat("x", 4*1024+205), // 4301 bytes -> 4.2 KiB
		},
		// 64 raw bytes base64-encoded (88 encoded chars): the size must be the
		// DECODED length.
		"binaryData": map[string]any{
			"ca.der": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+Pw==",
		},
	}
	row := kube.Row{Cluster: "test", Object: obj, Cells: []any{"app-config", 3, "17d"}}
	cv := schemaCellView(t, table, row, 1)
	if cv.Kind != cellKeys {
		t.Fatalf("Data cell kind = %v, want cellKeys", cv.Kind)
	}
	want := []keyChipView{
		{Name: "big.yaml", Size: "4.2 KiB"},
		{Name: "ca.der", Size: "64 B"},
		{Name: "logging.conf", Size: "11 B"},
	}
	if len(cv.Keys) != len(want) {
		t.Fatalf("key chips = %#v, want %d sorted chips", cv.Keys, len(want))
	}
	for i, w := range want {
		if cv.Keys[i] != w {
			t.Fatalf("chip %d = %#v, want %#v (sorted, humanBytes sizes, decoded binaryData)", i, cv.Keys[i], w)
		}
	}

	empty := kube.Row{Cluster: "test", Object: map[string]any{
		"kind": "ConfigMap", "apiVersion": "v1",
		"metadata": map[string]any{"name": "empty", "namespace": "default", "creationTimestamp": "2026-06-01T00:00:00Z"},
	}, Cells: []any{"empty", 0, "88d"}}
	cv = schemaCellView(t, table, empty, 1)
	if cv.Kind != cellKeys || len(cv.Keys) != 0 {
		t.Fatalf("empty configmap Data cell = %#v, want cellKeys with NO chips (renders —)", cv)
	}
}

// TestConfigMapListRendersKeyChips drives the fakeapi configmaps fixture
// through the real handler: the app-config row renders one `name · size` chip
// per key with the first keysCellMax visible and the overflow behind the
// `+2 keys` button (the SPEC §4.10 in-cell expand the e2e spec clicks), the
// single-key row has no button, the empty row shows the muted "—", and the
// decorated Data column is NOT right-aligned (decorateConfigMapColumns
// dropped the count-guessed num class).
func TestConfigMapListRendersKeyChips(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/configmaps", http.StatusOK)

	appConfig := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/configmaps/app-config"])`)
	strip := appConfig.Find(".ro-chips")
	chips := strip.Find(".ro-chip").Not(".more")
	if chips.Length() != 5 {
		t.Fatalf("app-config key chips = %d, want 5 (data + binaryData)", chips.Length())
	}
	// Sorted keys: application.yaml, ca.der, cors.yaml, feature-gates.json,
	// logging.conf -- the first three visible, the rest hidden behind .xtra.
	if got := strip.Find(".ro-chip.xtra").Length(); got != 2 {
		t.Fatalf("hidden overflow chips = %d, want 2 (5 keys past keysCellMax=3)", got)
	}
	first := chips.First()
	if got := normSpace(first.Find(".cv").Text()); got != "application.yaml" {
		t.Fatalf("first chip key = %q, want application.yaml (sorted)", got)
	}
	if got := normSpace(first.Find(".ck").Text()); got != "40 B" {
		t.Fatalf("application.yaml size = %q, want 40 B", got)
	}
	// The binaryData chip carries the DECODED size (64 B, not the 88-char b64).
	caDer := strip.Find(`.ro-chip:has(.cv:contains("ca.der"))`)
	if got := normSpace(caDer.Find(".ck").Text()); got != "64 B" {
		t.Fatalf("ca.der size = %q, want the DECODED 64 B", got)
	}
	// The +N button: a real keyboard-reachable <button> with the data-more
	// hook, collapsed face "+2 keys", expanded face "less" (CSS swap).
	more := strip.Find("button.ro-chip.more[data-more]")
	if more.Length() != 1 {
		t.Fatalf("app-config must render exactly one +N keys button, got %d", more.Length())
	}
	if got, _ := more.Attr("aria-expanded"); got != "false" {
		t.Fatalf("+N button aria-expanded = %q, want false at render", got)
	}
	if got := normSpace(more.Find(".more-n").Text()); got != "+2 keys" {
		t.Fatalf("+N face = %q, want +2 keys", got)
	}
	if got := normSpace(more.Find(".more-less").Text()); got != "less" {
		t.Fatalf("expanded face = %q, want less", got)
	}

	// Single-key row: one chip, no overflow button.
	rootCA := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/configmaps/kube-root-ca.crt"])`)
	if got := rootCA.Find(".ro-chips .ro-chip").Not(".more").Length(); got != 1 {
		t.Fatalf("kube-root-ca.crt chips = %d, want 1", got)
	}
	if rootCA.Find("[data-more]").Length() != 0 {
		t.Fatalf("a 1-key configmap must not render the +N button")
	}
	if got := normSpace(rootCA.Find(".ro-chip .ck").Text()); got != "94 B" {
		t.Fatalf("ca.crt size = %q, want 94 B", got)
	}

	// Empty data: the muted "—".
	marker := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/configmaps/pending-cleanup-marker"])`)
	if marker.Find("td span.faint:contains('—')").Length() == 0 {
		t.Fatalf("empty configmap should render the muted —: %q", normSpace(marker.Text()))
	}

	// The Data header is NOT right-aligned: the chips strip is left-aligned,
	// so decorateConfigMapColumns drops the num class the integer count cells
	// made GuessColumnClasses infer.
	if p.doc.Find(`table.ro-table thead th.num:contains("Data")`).Length() != 0 {
		t.Fatalf("Data header kept the num class -- decorateConfigMapColumns must clear the count alignment")
	}
}

// TestSecretKeysDecodedSizes pins the secret keys decode: the chip size is
// the DECODED byte length (base64 -> raw; the rawPassword fixture value is 24
// encoded chars of an 18-byte value, so the chip must read "18 B"), keys
// sorted, and by construction the view model carries ONLY name + size.
func TestSecretKeysDecodedSizes(t *testing.T) {
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "secrets", Kind: "Secret", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Type"}, {Name: "Data"}, {Name: "Age"}},
	}
	obj := map[string]any{
		"kind": "Secret", "apiVersion": "v1",
		"metadata": map[string]any{"name": "my-secret", "namespace": "default", "creationTimestamp": "2024-03-01T10:00:00Z"},
		"type":     "Opaque",
		"data": map[string]any{
			"password":  rawPassword, // base64("super-secret-value"): 18 decoded bytes
			"api-token": rawToken,    // base64("token"): 5 decoded bytes
		},
	}
	row := kube.Row{Cluster: "test", Object: obj, Cells: []any{"my-secret", "Opaque", 2, "5m"}}
	cv := schemaCellView(t, table, row, 2)
	if cv.Kind != cellKeys || len(cv.Keys) != 2 {
		t.Fatalf("secret Data cell = %#v, want cellKeys with 2 chips", cv)
	}
	if cv.Keys[0] != (keyChipView{Name: "api-token", Size: "5 B"}) {
		t.Fatalf("chip 0 = %#v, want api-token · 5 B (sorted, DECODED size)", cv.Keys[0])
	}
	if cv.Keys[1] != (keyChipView{Name: "password", Size: "18 B"}) {
		t.Fatalf("chip 1 = %#v, want password · 18 B (24 b64 chars decode to 18 bytes)", cv.Keys[1])
	}
}

// TestSecretListNeverRendersValues is the values-never-in-DOM proof (SPEC
// §4.10, D5): the masked-on secrets LIST renders the key names and DECODED
// sizes -- and NEITHER the raw fixture values NOR their base64 encodings
// appear ANYWHERE in the rendered HTML (table cells, mobile cards, tooltips,
// attributes: the search is over the whole body, both encodings). The raw
// "token" value is asserted only in its base64 form because the KEY NAME
// "api-token" legitimately contains the substring "token".
func TestSecretListNeverRendersValues(t *testing.T) {
	app := newServer(t, withSecrets(t), time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/secrets", http.StatusOK)

	// The keys surface: names + decoded sizes render as chips.
	if got := strings.Join(p.texts(`table.ro-table tr:has(td.cell-name a[href$="/secrets/my-secret"]) .ro-chips .cv`), "|"); got != "api-token|password" {
		t.Fatalf("my-secret key chips = %q, want api-token|password", got)
	}
	if got := strings.Join(p.texts(`table.ro-table tr:has(td.cell-name a[href$="/secrets/my-secret"]) .ro-chips .ck`), "|"); got != "5 B|18 B" {
		t.Fatalf("my-secret chip sizes = %q, want the DECODED 5 B|18 B", got)
	}
	// parse-prod: 4 keys -> 3 shown + the +1 keys overflow.
	parseProd := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/secrets/parse-prod"])`)
	if got := parseProd.Find(".ro-chips .ro-chip").Not(".more").Length(); got != 4 {
		t.Fatalf("parse-prod key chips = %d, want 4", got)
	}
	if got := normSpace(parseProd.Find("[data-more] .more-n").Text()); got != "+1 keys" {
		t.Fatalf("parse-prod overflow = %q, want +1 keys", got)
	}
	// The empty rotation marker renders the muted "—".
	empty := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/secrets/rotation-marker-empty"])`)
	if empty.Find("td span.faint:contains('—')").Length() == 0 {
		t.Fatalf("empty secret should render the muted —: %q", normSpace(empty.Text()))
	}

	// THE LAW: no secret VALUE -- raw or base64 -- anywhere in the rendered
	// HTML. Each pair is (raw value, its exact base64 wire form).
	for _, leak := range []string{
		"super-secret-value", rawPassword, // my-secret password
		rawToken,                                               // my-secret api-token (b64; raw "token" collides with the key name)
		"hunter2-export-grade", "aHVudGVyMi1leHBvcnQtZ3JhZGU=", // parse-prod MONGODB_PASSWORD
		"mongodb.internal.example", "bW9uZ29kYi5pbnRlcm5hbC5leGFtcGxl", // parse-prod MONGODB_HOST
		"master-key-sentinel-0xCAFE", "bWFzdGVyLWtleS1zZW50aW5lbC0weENBRkU=", // parse-prod PARSE_MASTER_KEY
		"AKIAFAKEACCESSKEY", "QUtJQUZBS0VBQ0NFU1NLRVk=", // parse-prod S3_ACCESS_KEY
	} {
		p.wantBodyExcludes(leak)
	}
}

// ingressObject builds an Ingress row object; tls toggles a spec.tls entry.
func ingressObject(name string, tls bool) map[string]any {
	spec := map[string]any{"ingressClassName": "nginx"}
	if tls {
		spec["tls"] = []any{map[string]any{"hosts": []any{"example.com"}, "secretName": "tls-example"}}
	}
	return map[string]any{
		"kind":       "Ingress",
		"apiVersion": "networking.k8s.io/v1",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         "default",
			"creationTimestamp": "2026-06-01T00:00:00Z",
		},
		"spec": spec,
	}
}

// ingressesTable builds a crafted ingresses kube.Table in the real printer
// shape (Name/Class/Hosts/Address/Ports/Age).
func ingressesTable(rows []kube.Row) *kube.Table {
	return &kube.Table{
		Resource: kube.ResourceType{Group: "networking.k8s.io", Plural: "ingresses", Kind: "Ingress", Namespaced: true, Version: "v1", APIVersion: "networking.k8s.io/v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Class"}, {Name: "Hosts"}, {Name: "Address"}, {Name: "Ports"}, {Name: "Age"},
		},
		Rows: rows,
	}
}

// TestIngressCells pins the SPEC §7.10 ingress mapping through the real
// decorator + buildCellView: the synthetic TLS column derives from spec.tls
// ("tls" plain cell / earned-green view only when terminated), the Hosts cell
// shows the first host + "+N hosts" with the full newline-joined list in the
// tooltip, and the <pending> Address pulses.
func TestIngressCells(t *testing.T) {
	withTLS := ingressObject("slr-www", true)
	noTLS := ingressObject("preview-env", false)
	table := ingressesTable([]kube.Row{
		{Cluster: "test", Object: withTLS, Cells: []any{"slr-www", "nginx", "sexlikereal.com,www.sexlikereal.com,m.sexlikereal.com,cdn.sexlikereal.com", "45.55.107.21", "80, 443", "17d"}},
		{Cluster: "test", Object: noTLS, Cells: []any{"preview-env", "nginx", "preview-8842.dev.slr", "<pending>", "80", "6m"}},
	})
	decorateIngressColumns(table)
	tlsIdx := columnIndex(table.Columns, "TLS")
	if tlsIdx < 0 {
		t.Fatalf("decorateIngressColumns did not append the TLS column")
	}
	// The plain DISPLAY cells (sort/TSV/filter truth) mirror the lock view.
	if got := cellString(table.Rows[0], tlsIdx); got != "tls" {
		t.Fatalf("terminated TLS plain cell = %q, want tls", got)
	}
	if got := cellString(table.Rows[1], tlsIdx); got != "—" {
		t.Fatalf("unterminated TLS plain cell = %q, want —", got)
	}

	// TLS view: the earned green ONLY when spec.tls terminates (D3).
	cv := schemaCellView(t, table, table.Rows[0], tlsIdx)
	if cv.Kind != cellTLS || cv.Value != "tls" || cv.Tone != "ok" {
		t.Fatalf("terminated TLS cell = %#v, want the ok tls lock", cv)
	}
	cv = schemaCellView(t, table, table.Rows[1], tlsIdx)
	if cv.Kind != cellTLS || cv.Value != "" {
		t.Fatalf("unterminated TLS cell = %#v, want the empty (—) state", cv)
	}

	// Hosts: first host + "+3 hosts", full list newline-joined in the tooltip.
	cv = schemaCellView(t, table, table.Rows[0], 2)
	if cv.Kind != cellHosts || cv.Value != "sexlikereal.com" || cv.More != "+3 hosts" {
		t.Fatalf("hosts cell = %#v, want first host + +3 hosts", cv)
	}
	if cv.Title != "sexlikereal.com\nwww.sexlikereal.com\nm.sexlikereal.com\ncdn.sexlikereal.com" {
		t.Fatalf("hosts tooltip = %q, want the newline-joined full list", cv.Title)
	}
	// A single host: no overflow.
	cv = schemaCellView(t, table, table.Rows[1], 2)
	if cv.Value != "preview-8842.dev.slr" || cv.More != "" {
		t.Fatalf("single-host cell = %#v, want the bare host", cv)
	}

	// Address: the literal <pending> pulses amber.
	cv = schemaCellView(t, table, table.Rows[1], 3)
	if cv.Kind != cellPending || cv.Value != "pending" || cv.Tone != "warn" || !cv.Pulse {
		t.Fatalf("pending address cell = %#v, want the pulsing warn pending state", cv)
	}
}

// TestIngressListThroughHandler drives the fakeapi ingresses fixture through
// the real handler: the synthetic TLS column lands in the header, the
// terminated rows render the green lock cell, the unterminated row the muted
// "—", the pending address pulses, and the 4-host row overflows to +3 hosts.
func TestIngressListThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/ingresses", http.StatusOK)

	if !strings.Contains(strings.Join(p.texts("table.ro-table thead th"), "|"), "TLS") {
		t.Fatalf("TLS column missing from ingress headers")
	}

	slr := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/ingresses/slr-www"])`)
	lock := slr.Find(`.cell-status.ok[title="TLS terminated"]`)
	if lock.Length() != 1 || normSpace(lock.Text()) != "tls" {
		t.Fatalf("slr-www TLS cell = %q, want the ok lock + tls", normSpace(lock.Text()))
	}
	if lock.Find("svg").Length() == 0 {
		t.Fatalf("TLS lock cell missing its icon svg")
	}
	if !strings.Contains(normSpace(slr.Text()), "+3 hosts") {
		t.Fatalf("slr-www should overflow its 4 hosts to +3 hosts: %q", normSpace(slr.Text()))
	}

	admin := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/ingresses/admin-internal"])`)
	if admin.Find(".cell-status.ok").Length() != 0 {
		t.Fatalf("admin-internal terminates no TLS -- it must NOT earn the green lock")
	}
	if !strings.Contains(admin.Text(), "—") {
		t.Fatalf("admin-internal TLS cell should be the muted —: %q", normSpace(admin.Text()))
	}

	preview := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/ingresses/preview-env"])`)
	if preview.Find(".cell-status.warn .ro-dot.warn.pulse").Length() != 1 {
		t.Fatalf("preview-env <pending> address must render the pulsing warn dot")
	}
}
