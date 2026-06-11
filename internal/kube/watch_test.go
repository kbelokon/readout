package kube

// watch_test.go pins the kube half of D19: Table-format watches resumed from
// a captured list resourceVersion. The fakeapi control surface
// (/__control/watch-script) drives every branch with scripted sequences —
// data events with the first-event-only columnDefinitions rule, bookmarks
// advancing the RV, the typed 410, replay above a captured RV, and a clean
// upstream EOF that stays distinct from caller cancellation — no cluster
// involved.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

const watchPodsPath = "/api/v1/namespaces/default/pods"

const watchFailedPodObject = `{
	"apiVersion": "v1",
	"kind": "Pod",
	"metadata": {"name": "nginx", "namespace": "default"},
	"status": {"phase": "Failed"}
}`

// postWatchScript queues scripted watch events on the fakeapi control
// surface and fails the test on a non-200 ack.
func postWatchScript(t *testing.T, baseURL, script string) {
	t.Helper()
	resp, err := http.Post(baseURL+"/__control/watch-script", "application/json", strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-script status = %d body = %s", resp.StatusCode, body)
	}
}

// numericRV parses a resourceVersion for ordering assertions.
func numericRV(t *testing.T, rv string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(rv, 10, 64)
	if err != nil {
		t.Fatalf("resourceVersion %q is not numeric: %v", rv, err)
	}
	return n
}

func nextWatchEvent(t *testing.T, w *TableWatch) WatchEvent {
	t.Helper()
	ev, err := w.Next()
	if err != nil {
		t.Fatalf("watch Next: %v", err)
	}
	return ev
}

// listPodsTable fetches the default pods Table and asserts the decode
// captured the list resourceVersion (the watch has nothing to start from
// without it — the seam D19 needs).
func listPodsTable(t *testing.T, client *Client) (ResourceType, Table) {
	t.Helper()
	rt, err := client.FindResource(context.Background(), "pods", true, "")
	if err != nil {
		t.Fatal(err)
	}
	table, err := client.Table(context.Background(), &rt, ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if table.ResourceVersion == "" {
		t.Fatal("list Table carries no resourceVersion — the watch cannot resume from the list")
	}
	return rt, table
}

// TestWatchTableStreamsScriptedTableEvents pins the data-event protocol:
// ADDED/MODIFIED/DELETED arrive as 1-row Tables, columnDefinitions ride ONLY
// the first event (the consumer caches them), every event advances the
// resourceVersion, and emitted row objects are stamped with the event RV.
func TestWatchTableStreamsScriptedTableEvents(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, list := listPodsTable(t, client)
	listRV := numericRV(t, list.ResourceVersion)

	w, err := client.WatchTable(ctx, &rt, WatchOptions{Namespace: "default", ResourceVersion: list.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	script := fmt.Sprintf(`{"events":[
		{"path":%[1]q,"type":"ADDED","cells":["extra","1/1","Running","0","1m"],"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"extra","namespace":"default"},"status":{"phase":"Running"}}},
		{"path":%[1]q,"type":"MODIFIED","cells":["nginx","0/1","Error","3","10m"],"object":%[2]s},
		{"path":%[1]q,"type":"DELETED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"extra","namespace":"default"}}}
	]}`, watchPodsPath, watchFailedPodObject)
	postWatchScript(t, f.server.URL, script)

	added := nextWatchEvent(t, w)
	if added.Type != WatchAdded {
		t.Fatalf("first event type = %q, want ADDED", added.Type)
	}
	if len(added.Table.Columns) == 0 || added.Table.Columns[0].Name != "Name" {
		t.Fatalf("first watch event must carry columnDefinitions, got %#v", added.Table.Columns)
	}
	if len(added.Table.Rows) != 1 {
		t.Fatalf("watch event rows = %d, want 1", len(added.Table.Rows))
	}
	if cell := added.Table.Rows[0].Cells[0]; cell != "extra" {
		t.Fatalf("ADDED first cell = %#v, want extra", cell)
	}
	addedObj := Object{Raw: added.Table.Rows[0].Object}
	if addedObj.Name() != "extra" {
		t.Fatalf("ADDED row object name = %q, want extra", addedObj.Name())
	}
	addedRV := numericRV(t, added.ResourceVersion)
	if addedRV <= listRV {
		t.Fatalf("ADDED resourceVersion %d did not advance past the list's %d", addedRV, listRV)
	}
	if rv := objectResourceVersion(added.Table.Rows[0].Object); rv != added.ResourceVersion {
		t.Fatalf("emitted row object resourceVersion = %q, want the event's %q", rv, added.ResourceVersion)
	}

	modified := nextWatchEvent(t, w)
	if modified.Type != WatchModified {
		t.Fatalf("second event type = %q, want MODIFIED", modified.Type)
	}
	if len(modified.Table.Columns) != 0 {
		t.Fatalf("subsequent watch events must NOT carry columnDefinitions (the consumer caches the first event's), got %#v", modified.Table.Columns)
	}
	if cell := modified.Table.Rows[0].Cells[2]; cell != "Error" {
		t.Fatalf("MODIFIED status cell = %#v, want Error", cell)
	}
	modifiedRV := numericRV(t, modified.ResourceVersion)
	if modifiedRV <= addedRV {
		t.Fatalf("MODIFIED resourceVersion %d did not advance past %d", modifiedRV, addedRV)
	}

	deleted := nextWatchEvent(t, w)
	if deleted.Type != WatchDeleted {
		t.Fatalf("third event type = %q, want DELETED", deleted.Type)
	}
	deletedObj := Object{Raw: deleted.Table.Rows[0].Object}
	if deletedObj.Name() != "extra" {
		t.Fatalf("DELETED row object name = %q, want extra", deletedObj.Name())
	}
	if cell := deleted.Table.Rows[0].Cells[0]; cell != "extra" {
		t.Fatalf("DELETED event should carry the row's last-known cells, got first cell %#v", cell)
	}
	if rv := numericRV(t, deleted.ResourceVersion); rv <= modifiedRV {
		t.Fatalf("DELETED resourceVersion %d did not advance past %d", rv, modifiedRV)
	}
}

// objectResourceVersion reads metadata.resourceVersion from a row object.
func objectResourceVersion(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	return rv
}

// TestWatchTableBookmarkAdvancesResourceVersion pins BOOKMARK semantics: the
// event carries no rows, only a resourceVersion that advances past the last
// data event's — the consumer's re-watch point moves without any row churn.
func TestWatchTableBookmarkAdvancesResourceVersion(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, list := listPodsTable(t, client)
	w, err := client.WatchTable(ctx, &rt, WatchOptions{Namespace: "default", ResourceVersion: list.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	script := fmt.Sprintf(`{"events":[
		{"path":%[1]q,"type":"MODIFIED","cells":["nginx","0/1","Error","3","10m"],"object":%[2]s},
		{"path":%[1]q,"type":"BOOKMARK"}
	]}`, watchPodsPath, watchFailedPodObject)
	postWatchScript(t, f.server.URL, script)

	modified := nextWatchEvent(t, w)
	if modified.Type != WatchModified {
		t.Fatalf("first event type = %q, want MODIFIED", modified.Type)
	}
	modifiedRV := numericRV(t, modified.ResourceVersion)

	bookmark := nextWatchEvent(t, w)
	if bookmark.Type != WatchBookmark {
		t.Fatalf("second event type = %q, want BOOKMARK", bookmark.Type)
	}
	if len(bookmark.Table.Rows) != 0 {
		t.Fatalf("bookmark event carries rows: %#v", bookmark.Table.Rows)
	}
	if rv := numericRV(t, bookmark.ResourceVersion); rv <= modifiedRV {
		t.Fatalf("bookmark resourceVersion %d did not advance past the data event's %d", rv, modifiedRV)
	}
	if bookmark.Table.ResourceVersion != bookmark.ResourceVersion {
		t.Fatalf("event RV %q != payload table RV %q", bookmark.ResourceVersion, bookmark.Table.ResourceVersion)
	}
}

// TestWatchTableGoneYieldsTypedError pins the 410 contract: an in-stream
// ERROR event with reason Expired surfaces as ErrWatchGone (the caller
// relists), never as a clean EOF or a context error.
func TestWatchTableGoneYieldsTypedError(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, list := listPodsTable(t, client)
	w, err := client.WatchTable(ctx, &rt, WatchOptions{Namespace: "default", ResourceVersion: list.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	postWatchScript(t, f.server.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"GONE"}]}`, watchPodsPath))

	_, err = w.Next()
	if !errors.Is(err, ErrWatchGone) {
		t.Fatalf("scripted 410 returned %v, want ErrWatchGone", err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		t.Fatalf("the typed gone error must not classify as EOF or cancellation: %v", err)
	}
}

// TestWatchTableEOFDistinctFromContextCancel pins the two stream ends apart:
// a scripted upstream EOF is io.EOF (the consumer re-watches from the last
// RV), while caller cancellation is the context error (the consumer stops) —
// conflating them would make Unit 26's lifecycle spin or leak.
func TestWatchTableEOFDistinctFromContextCancel(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)

	rt, list := listPodsTable(t, client)

	// Clean upstream EOF: a data event proves the stream was live, then the
	// scripted EOF ends it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := client.WatchTable(ctx, &rt, WatchOptions{Namespace: "default", ResourceVersion: list.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	script := fmt.Sprintf(`{"events":[
		{"path":%[1]q,"type":"MODIFIED","cells":["nginx","0/1","Error","3","10m"],"object":%[2]s},
		{"path":%[1]q,"type":"EOF"}
	]}`, watchPodsPath, watchFailedPodObject)
	postWatchScript(t, f.server.URL, script)
	if ev := nextWatchEvent(t, w); ev.Type != WatchModified {
		t.Fatalf("pre-EOF event type = %q, want MODIFIED", ev.Type)
	}
	_, err = w.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("clean upstream EOF returned %v, want io.EOF", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("upstream EOF must not classify as a context error: %v", err)
	}

	// Caller cancellation while Next is blocked. Relist first: the fresh RV
	// is past the MODIFIED above, so nothing replays and Next stays blocked.
	_, fresh := listPodsTable(t, client)
	cancelCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	blocked, err := client.WatchTable(cancelCtx, &rt, WatchOptions{Namespace: "default", ResourceVersion: fresh.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocked.Close() }()
	timer := time.AfterFunc(75*time.Millisecond, cancelWatch)
	defer timer.Stop()
	_, err = blocked.Next()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled watch returned %v, want context.Canceled", err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("cancellation must not look like a clean upstream EOF: %v", err)
	}
}

// TestWatchTableReplayFromListResourceVersion pins the relist-then-rewatch
// flow: events applied BEFORE the watch connects replay when the watch starts
// from the pre-mutation list RV (no race between list and watch), and a watch
// from the post-mutation RV sees nothing — already-seen events never repeat.
func TestWatchTableReplayFromListResourceVersion(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, before := listPodsTable(t, client)

	script := fmt.Sprintf(`{"events":[{"path":%q,"type":"ADDED","cells":["extra","1/1","Running","0","1m"],"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"extra","namespace":"default"},"status":{"phase":"Running"}}}]}`, watchPodsPath)
	postWatchScript(t, f.server.URL, script)

	// Watch from the PRE-mutation RV: the applied event replays, and as the
	// connection's first frame it carries columnDefinitions.
	w, err := client.WatchTable(ctx, &rt, WatchOptions{Namespace: "default", ResourceVersion: before.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	replayed := nextWatchEvent(t, w)
	if replayed.Type != WatchAdded {
		t.Fatalf("replayed event type = %q, want ADDED", replayed.Type)
	}
	if cell := replayed.Table.Rows[0].Cells[0]; cell != "extra" {
		t.Fatalf("replayed first cell = %#v, want extra", cell)
	}
	if len(replayed.Table.Columns) == 0 {
		t.Fatal("a replayed first frame must still carry columnDefinitions")
	}

	// Relist: the collection RV advanced past the mutation; a watch from it
	// must NOT replay the already-seen event — only the deadline fires.
	_, after := listPodsTable(t, client)
	if numericRV(t, after.ResourceVersion) <= numericRV(t, before.ResourceVersion) {
		t.Fatalf("relist RV %q did not advance past %q", after.ResourceVersion, before.ResourceVersion)
	}
	quietCtx, quietCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer quietCancel()
	quiet, err := client.WatchTable(quietCtx, &rt, WatchOptions{Namespace: "default", ResourceVersion: after.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = quiet.Close() }()
	ev, err := quiet.Next()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("watch from the relist RV yielded (%#v, %v), want only the caller deadline", ev, err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("caller deadline must not look like a clean upstream EOF: %v", err)
	}
}

// TestWatchTableDeniedClientForbidden pins that the D8d denied clone refuses
// WatchTable like every other request method.
func TestWatchTableDeniedClientForbidden(t *testing.T) {
	base, err := NewClient(&rest.Config{Host: "https://x"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rt := &ResourceType{Version: "v1", Plural: "pods", Namespaced: true}
	if _, err := base.Denied().WatchTable(context.Background(), rt, WatchOptions{Namespace: "ns"}); !IsForbidden(err) {
		t.Fatalf("WatchTable denied err = %v, want forbidden", err)
	}
}

// TestWatchTableUpstreamUnauthorizedTyped pins the auth-expiry seam Unit 26's
// terminal taxonomy branches on: an upstream 401 at connect time is a typed
// Status error IsForbidden recognizes (it covers Unauthorized).
func TestWatchTableUpstreamUnauthorizedTyped(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, list := listPodsTable(t, client)

	resp, err := http.Get(f.server.URL + "/__control/watch-401")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("arming watch-401 status = %d", resp.StatusCode)
	}

	_, err = client.WatchTable(ctx, &rt, WatchOptions{Namespace: "default", ResourceVersion: list.ResourceVersion})
	if err == nil {
		t.Fatal("WatchTable against an armed 401 succeeded, want a typed error")
	}
	if !IsForbidden(err) {
		t.Fatalf("upstream 401 err = %v, want IsForbidden (Unauthorized) classification", err)
	}
}
