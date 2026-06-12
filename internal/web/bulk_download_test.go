package web

// Bulk YAML download: the list-level `?download=yaml&names=…`
// GET returns ONE multi-document YAML (`---`-separated, one document per
// requested name in request order), bounded on BOTH sides (the client
// disables the button above 100 selected; the server rejects >100 names with
// 400 BEFORE the table fan-out). Names grammar: bare `name` on
// single-namespace lists, `ns/name` on _all-namespaces lists. A name absent
// from the table renders a `# not found: <name>` comment document, never a
// whole-download failure. These tests drive the REAL handler chain against
// the fakeapi fixtures, plus the bulk-bar wiring flags and the
// hx-boost="false" opt-outs the download surface requires.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/tests/unit/fakeapi"
)

var bulkClock = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

// bulkGet drives one GET through the full handler chain and asserts the
// status; the raw recorder comes back because a YAML attachment is not an
// HTML page (the goquery `get` helper is wrong for it).
func bulkGet(t *testing.T, app *Server, path string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status = %d, want %d\nbody=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

// splitYAMLDocs splits a multi-document YAML body on standalone `---` lines.
func splitYAMLDocs(body string) []string {
	var docs []string
	var current []string
	for _, line := range strings.Split(body, "\n") {
		if line == "---" {
			docs = append(docs, strings.Join(current, "\n"))
			current = nil
			continue
		}
		current = append(current, line)
	}
	return append(docs, strings.Join(current, "\n"))
}

// TestBulkDownloadThreeDocsWithKinds: three requested configmaps (the fixture
// rows carry FULL ConfigMap objects) come back as three `---`-separated
// documents in request order, each with its real kind, under the pinned
// attachment filename `<cluster>_<namespace>_<plural>_bulk.yaml`.
func TestBulkDownloadThreeDocsWithKinds(t *testing.T) {
	app := newServer(t, baseConfig(t), bulkClock)
	rec := bulkGet(t, app,
		"/clusters/test/namespaces/default/configmaps?download=yaml&names=app-config,kube-root-ca.crt,pending-cleanup-marker",
		http.StatusOK)

	if got := rec.Header().Get("Content-Type"); got != "text/vnd.yaml; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=test_default_configmaps_bulk.yaml" {
		t.Fatalf("Content-Disposition = %q", got)
	}

	docs := splitYAMLDocs(rec.Body.String())
	if len(docs) != 3 {
		t.Fatalf("documents = %d, want 3\nbody=%s", len(docs), rec.Body.String())
	}
	for i, name := range []string{"app-config", "kube-root-ca.crt", "pending-cleanup-marker"} {
		if !strings.Contains(docs[i], "kind: ConfigMap") {
			t.Fatalf("doc %d missing kind: ConfigMap\ndoc=%s", i, docs[i])
		}
		if !strings.Contains(docs[i], "name: "+name) {
			t.Fatalf("doc %d is not %s (request order must be preserved)\ndoc=%s", i, name, docs[i])
		}
	}
	if strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("all three objects exist; body must carry no not-found comment\nbody=%s", rec.Body.String())
	}
}

// TestBulkDownloadAllNamespacesGrammar: on an _all-namespaces list the names
// grammar is `ns/name` (the fixture's /api/v1/pods carries default/nginx and
// default/my-app); a bare name violates that grammar and resolves to a
// not-found comment, never an error. The inverse holds on a single-namespace
// list, where `ns/name` is not a valid bare name.
func TestBulkDownloadAllNamespacesGrammar(t *testing.T) {
	app := newServer(t, baseConfig(t), bulkClock)
	rec := bulkGet(t, app,
		"/clusters/test/namespaces/_all/pods?download=yaml&names=default/nginx,default/my-app",
		http.StatusOK)

	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=test__all_pods_bulk.yaml" {
		t.Fatalf("Content-Disposition = %q", got)
	}
	docs := splitYAMLDocs(rec.Body.String())
	if len(docs) != 2 {
		t.Fatalf("documents = %d, want 2\nbody=%s", len(docs), rec.Body.String())
	}
	if !strings.Contains(docs[0], "name: nginx") || !strings.Contains(docs[1], "name: my-app") {
		t.Fatalf("ns/name lookups did not resolve in order\nbody=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("both ns/name lookups exist; got a not-found comment\nbody=%s", rec.Body.String())
	}

	// Bare `nginx` on the _all list: the index keys are ns/name, so the bare
	// name is NOT FOUND under the grammar -- pinned so the grammar can never
	// silently loosen.
	bare := bulkGet(t, app, "/clusters/test/namespaces/_all/pods?download=yaml&names=nginx", http.StatusOK)
	if got := strings.TrimSuffix(bare.Body.String(), "\n"); got != "# not found: nginx" {
		t.Fatalf("bare name on _all list = %q, want the not-found comment", got)
	}

	// And `default/nginx` on the single-namespace list violates ITS grammar.
	qualified := bulkGet(t, app, "/clusters/test/namespaces/default/pods?download=yaml&names=default/nginx", http.StatusOK)
	if got := strings.TrimSuffix(qualified.Body.String(), "\n"); got != "# not found: default/nginx" {
		t.Fatalf("ns/name on single-namespace list = %q, want the not-found comment", got)
	}
}

// TestBulkDownloadMissingNameComment: one bogus name among real ones yields
// the two real documents plus a `# not found:` comment DOCUMENT in its
// requested position -- the download itself never fails.
func TestBulkDownloadMissingNameComment(t *testing.T) {
	app := newServer(t, baseConfig(t), bulkClock)
	rec := bulkGet(t, app,
		"/clusters/test/namespaces/default/configmaps?download=yaml&names=app-config,bogus,kube-root-ca.crt",
		http.StatusOK)

	docs := splitYAMLDocs(rec.Body.String())
	if len(docs) != 3 {
		t.Fatalf("documents = %d, want 3 (two objects + one comment doc)\nbody=%s", len(docs), rec.Body.String())
	}
	if !strings.Contains(docs[0], "name: app-config") || !strings.Contains(docs[2], "name: kube-root-ca.crt") {
		t.Fatalf("real objects missing around the comment doc\nbody=%s", rec.Body.String())
	}
	if got := strings.TrimSuffix(docs[1], "\n"); got != "# not found: bogus" {
		t.Fatalf("middle doc = %q, want exactly the not-found comment", got)
	}
	if n := strings.Count(rec.Body.String(), "kind: ConfigMap"); n != 2 {
		t.Fatalf("object documents = %d, want 2", n)
	}
}

// TestBulkDownloadMasksSecretValues pins the secret-masking law (secret VALUES are never
// serialized) on the BULK path: the single-object download masks the fetched
// object before marshaling (buildDetailView -> maskSecret), and the bulk
// multi-document download serializes the table's row objects, so it must
// apply the SAME treatment. Every data value of every requested Secret comes
// back as the mask sentinel; neither the base64 wire form nor its decoded
// plaintext appears anywhere in the response (the fixture's
// render_secrets_table.json carries the exact pairs below). Key NAMES survive
// (the mask replaces values, never keys), and non-secret kinds keep their
// data untouched -- the mask is Secret-only. Needs IncludeSecrets=true: under
// the default config the Secret type is not even discoverable (the secret
// barrier, TestBehaviorSecretBarrierDefaultOff).
func TestBulkDownloadMasksSecretValues(t *testing.T) {
	app := newServer(t, withSecrets(t), bulkClock)
	rec := bulkGet(t, app,
		"/clusters/test/namespaces/default/secrets?download=yaml&names=my-secret,parse-prod",
		http.StatusOK)

	body := rec.Body.String()
	if docs := splitYAMLDocs(body); len(docs) != 2 {
		t.Fatalf("documents = %d, want 2\nbody=%s", len(docs), body)
	}
	if n := strings.Count(body, "kind: Secret"); n != 2 {
		t.Fatalf("kind: Secret documents = %d, want 2\nbody=%s", n, body)
	}
	// The fixture's base64 wire values and their decoded plaintexts. The
	// decoded "token" (dG9rZW4=) is deliberately absent from this list: it is
	// a substring of the surviving "api-token" KEY, so only its base64 form
	// is assertable.
	for _, leak := range []string{
		"c3VwZXItc2VjcmV0LXZhbHVl", "super-secret-value",
		"dG9rZW4=",
		"bW9uZ29kYi5pbnRlcm5hbC5leGFtcGxl", "mongodb.internal.example",
		"aHVudGVyMi1leHBvcnQtZ3JhZGU=", "hunter2-export-grade",
		"bWFzdGVyLWtleS1zZW50aW5lbC0weENBRkU=", "master-key-sentinel-0xCAFE",
		"QUtJQUZBS0VBQ0NFU1NLRVk=", "AKIAFAKEACCESSKEY",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("bulk YAML leaks secret material %q\nbody=%s", leak, body)
		}
	}
	// Every one of the 2+4 data values is the sentinel, and the key names stay.
	if n := strings.Count(body, kube.SecretContentHidden); n != 6 {
		t.Fatalf("mask sentinel occurrences = %d, want 6 (one per data value)\nbody=%s", n, body)
	}
	for _, key := range []string{"password:", "api-token:", "MONGODB_HOST:", "PARSE_MASTER_KEY:"} {
		if !strings.Contains(body, key) {
			t.Fatalf("masked secret lost its key %q\nbody=%s", key, body)
		}
	}
	// The same maskSecret treatment hides annotations too (the detail path's
	// exact transformation, helpers.go maskSecret).
	if !strings.Contains(body, "annotations-hidden") {
		t.Fatalf("bulk secret docs miss the annotations-hidden marker\nbody=%s", body)
	}

	// Control: a CONFIGMAP bulk download keeps its data values verbatim --
	// the mask applies to Secrets only, never to other kinds.
	cm := bulkGet(t, app,
		"/clusters/test/namespaces/default/configmaps?download=yaml&names=app-config",
		http.StatusOK)
	if got := cm.Body.String(); !strings.Contains(got, "port: 1337") || strings.Contains(got, kube.SecretContentHidden) {
		t.Fatalf("configmap bulk download must keep data values unmasked\nbody=%s", got)
	}
}

// TestBulkDownloadCapBound pins the server half of the double-sided bound:
// 101 names reject with 400 BEFORE any table fan-out reaches the cluster
// (the recorder sees zero unlimited list fetches), while exactly 100 names
// pass the cap and produce 100 documents.
func TestBulkDownloadCapBound(t *testing.T) {
	var fanouts atomic.Int64
	fake, err := fakeapi.New(fakeapi.WithListRecorder(func(r *http.Request) {
		// The page's table fan-out lists are unlimited; the sidebar-count
		// probes carry limit=1 and are not the fan-out under test.
		if r.URL.Query().Get("limit") == "" {
			fanouts.Add(1)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	app := newServer(t, configForFixture(fake.URL), bulkClock)

	names := make([]string, 101)
	for i := range names {
		names[i] = fmt.Sprintf("ghost-%d", i)
	}
	bulkGet(t, app, "/clusters/test/namespaces/default/pods?download=yaml&names="+strings.Join(names, ","), http.StatusBadRequest)
	if got := fanouts.Load(); got != 0 {
		t.Fatalf("over-cap request reached the cluster: %d list fetches, want 0 (the bound must reject before the fan-out)", got)
	}

	// Exactly 100 names is INSIDE the bound (the cap is >100): 100 documents,
	// found or not.
	rec := bulkGet(t, app, "/clusters/test/namespaces/default/pods?download=yaml&names="+strings.Join(names[:100], ","), http.StatusOK)
	if fanouts.Load() == 0 {
		t.Fatal("the in-bound request issued no table fan-out")
	}
	if docs := splitYAMLDocs(rec.Body.String()); len(docs) != 100 {
		t.Fatalf("documents = %d, want 100", len(docs))
	}
}

// TestBulkDownloadScopeRejections: the bulk GET is single-type +
// single-cluster + non-empty names; everything else is a 400, mirroring the
// surfaces where the bulk button exists at all.
func TestBulkDownloadScopeRejections(t *testing.T) {
	app := newServer(t, baseConfig(t), bulkClock)

	// No names parameter / only empty segments.
	bulkGet(t, app, "/clusters/test/namespaces/default/configmaps?download=yaml", http.StatusBadRequest)
	bulkGet(t, app, "/clusters/test/namespaces/default/configmaps?download=yaml&names=,,", http.StatusBadRequest)

	// Multi-cluster scope: the names grammar has no cluster segment, so the
	// server rejects what the disabled button cannot send.
	bulkGet(t, app, "/clusters/_all/namespaces/default/configmaps?download=yaml&names=app-config", http.StatusBadRequest)

	// Multi-type plurals never render a bulk bar; a hand-built URL gets a
	// clean 400, not an ambiguous cross-table lookup.
	bulkGet(t, app, "/clusters/test/namespaces/default/all?download=yaml&names=nginx", http.StatusBadRequest)
	bulkGet(t, app, "/clusters/test/namespaces/default/pods,services?download=yaml&names=nginx", http.StatusBadRequest)
}

// TestBulkBarHrefAndBoundsFlags pins the server-baked bulk-bar wiring: the
// CLEAN data-bulk-href base on single-cluster lists (no filter/sort carried
// -- selection may include filtered-out rows), the ns/name grammar flag +
// cluster prefix on _all-namespaces lists, and the disabled-with-title
// Download button on multi-cluster scope (no data-bulk-href to act on).
func TestBulkBarHrefAndBoundsFlags(t *testing.T) {
	app := newServer(t, baseConfig(t), bulkClock)

	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	p.wantAttr("#ro-bulkbar", "data-bulk-href", "/clusters/test/namespaces/default/pods?download=yaml")
	p.wantAttr("#ro-bulkbar", "data-bulk-cluster", "test")
	p.wantAbsent("#ro-bulkbar[data-bulk-allns]")
	p.wantAbsent("#ro-bulk-download[disabled]")

	// Active filter/sort params do NOT leak into the bulk base.
	filtered := get(t, app, "/clusters/test/namespaces/default/pods?filter=ngi&sort=Name", http.StatusOK)
	filtered.wantAttr("#ro-bulkbar", "data-bulk-href", "/clusters/test/namespaces/default/pods?download=yaml")

	// _all namespaces: grammar flag + the cluster prefix readout.js strips
	// off each selection key to derive ns/name.
	all := get(t, app, "/clusters/test/namespaces/_all/pods", http.StatusOK)
	all.wantAttr("#ro-bulkbar", "data-bulk-href", "/clusters/test/namespaces/_all/pods?download=yaml")
	all.wantAttr("#ro-bulkbar", "data-bulk-allns", "true")
	all.wantAttr("#ro-bulkbar", "data-bulk-cluster", "test")

	// Multi-cluster scope: no bulk href, Download disabled with the
	// explanatory title (bulk YAML download is only offered on single-cluster
	// lists), Copy names untouched.
	multi := get(t, app, "/clusters/_all/namespaces/default/pods", http.StatusOK)
	multi.wantAbsent("#ro-bulkbar[data-bulk-href]")
	multi.wantHas("#ro-bulk-download[disabled]")
	multi.wantAttr("#ro-bulk-download", "title", "Bulk YAML download is unavailable across clusters — open a single cluster's list")
	multi.wantAbsent("#ro-bulk-copy[disabled]")
}

// TestBulkDownloadSurfaceAnchorsOptOutOfBoost pins the hx-boost="false"
// opt-out on the existing download anchors: the body-level boost intercepts a
// plain anchor click and swaps the attachment bytes into <body> instead of
// downloading (htmx 2.0.4, verified live), so every download-serving anchor
// must carry the opt-out. The bulk Download button needs none -- it is a
// <button> navigated via location.assign, which boost never captures.
func TestBulkDownloadSurfaceAnchorsOptOutOfBoost(t *testing.T) {
	app := newServer(t, baseConfig(t), bulkClock)

	list := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	list.wantAttr(`a[title="Download resource list as Tab-Separated-Values (TSV)"]`, "hx-boost", "false")

	detail := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)
	detail.wantAttr(`.ro-detail-actions a[title="Download resource object as YAML"]`, "hx-boost", "false")

	// The Download-logs title action is the third
	// download-serving anchor; it renders only with container logs enabled.
	logsCfg := baseConfig(t)
	logsCfg.ShowContainerLogs = true
	logsApp := newServer(t, logsCfg, bulkClock)
	logs := get(t, logsApp, "/clusters/test/namespaces/default/pods/nginx/logs", http.StatusOK)
	logs.wantAttr(`.ro-detail-actions a[title="Download logs"]`, "hx-boost", "false")
}
