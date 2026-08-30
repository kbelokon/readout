package fakekube_test

// These tests pin the control-surface semantics the e2e suite and the
// downstream units (all-clusters states, list states, live updates, watch
// playback) rely on: a scripted watch event mutates the in-memory list state
// so subsequent LIST responses reflect it, fail-lists renders real apiserver
// Status payloads, watch-401 is a one-shot, ?limit returns the chunked-list
// shape, and reset restores the seeded fixtures.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	fakeapi "github.com/kbelokon/readout/internal/fakekube"
)

const (
	podsPath        = "/api/v1/namespaces/default/pods"
	tableAccept     = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"
	failedPodObject = `{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {"name": "nginx", "namespace": "default", "uid": "00000000-0000-0000-0000-000000000001"},
		"status": {"phase": "Failed"}
	}`
)

func newServer(t *testing.T) *fakeapi.Server {
	t.Helper()
	srv, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, accept string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s response: %v body=%s", url, err, body)
	}
	return res.StatusCode, doc
}

func postScript(t *testing.T, srv *fakeapi.Server, script string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(srv.URL+"/__control/watch-script", "application/json", bytes.NewReader([]byte(script)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, doc
}

func podRow(t *testing.T, table map[string]any, name string) []any {
	t.Helper()
	rows, _ := table["rows"].([]any)
	for _, item := range rows {
		row := item.(map[string]any)
		cells := row["cells"].([]any)
		if len(cells) > 0 && cells[0] == name {
			return cells
		}
	}
	return nil
}

func podItem(table map[string]any, name string) map[string]any {
	items, _ := table["items"].([]any)
	for _, item := range items {
		obj := item.(map[string]any)
		meta := obj["metadata"].(map[string]any)
		if meta["name"] == name {
			return obj
		}
	}
	return nil
}

// TestWatchScriptMutatesListState pins the acceptance semantic four downstream
// units consume: applying a scripted status change makes a SUBSEQUENT LIST
// response (both the Table and the plain List form) reflect it.
func TestWatchScriptMutatesListState(t *testing.T) {
	srv := newServer(t)

	code, table := get(t, srv.URL+podsPath, tableAccept)
	if code != http.StatusOK {
		t.Fatalf("seed table status = %d", code)
	}
	if cells := podRow(t, table, "nginx"); len(cells) < 3 || cells[2] != "Running" {
		t.Fatalf("seed nginx row = %v, want status cell Running", cells)
	}
	seedRV, _ := table["metadata"].(map[string]any)["resourceVersion"].(string)

	script := fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","cells":["nginx","0/1","Error","3","10m"],"object":%s}]}`, podsPath, failedPodObject)
	code, ack := postScript(t, srv, script)
	if code != http.StatusOK || ack["queued"] != float64(1) {
		t.Fatalf("watch-script status = %d body = %v", code, ack)
	}

	code, table = get(t, srv.URL+podsPath, tableAccept)
	if code != http.StatusOK {
		t.Fatalf("table after script status = %d", code)
	}
	cells := podRow(t, table, "nginx")
	if len(cells) < 3 || cells[2] != "Error" || cells[1] != "0/1" {
		t.Fatalf("nginx row after script = %v, want [nginx 0/1 Error ...]", cells)
	}
	if rv, _ := table["metadata"].(map[string]any)["resourceVersion"].(string); rv == seedRV {
		t.Fatalf("collection resourceVersion did not advance from %q", seedRV)
	}

	code, list := get(t, srv.URL+podsPath, "")
	if code != http.StatusOK {
		t.Fatalf("list after script status = %d", code)
	}
	nginx := podItem(list, "nginx")
	if nginx == nil {
		t.Fatalf("nginx missing from list after script: %v", list)
	}
	if status, _ := nginx["status"].(map[string]any); status["phase"] != "Failed" {
		t.Fatalf("nginx status after script = %v, want phase Failed", nginx["status"])
	}

	// The all-namespaces route shares the same state by design.
	code, shared := get(t, srv.URL+"/api/v1/pods", tableAccept)
	if code != http.StatusOK || podRow(t, shared, "nginx")[2] != "Error" {
		t.Fatalf("shared /api/v1/pods state did not reflect the mutation: %d %v", code, podRow(t, shared, "nginx"))
	}
}

// TestWatchScriptAddAndDelete pins ADDED/DELETED upsert-remove semantics and
// the ADDED-needs-cells validation on table-backed collections.
func TestWatchScriptAddAndDelete(t *testing.T) {
	srv := newServer(t)

	noCells := fmt.Sprintf(`{"events":[{"path":%q,"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"extra","namespace":"default"}}}]}`, podsPath)
	if code, body := postScript(t, srv, noCells); code != http.StatusBadRequest {
		t.Fatalf("ADDED without cells status = %d body = %v, want 400", code, body)
	}

	added := fmt.Sprintf(`{"events":[{"path":%q,"type":"ADDED","cells":["extra","1/1","Running","0","1m"],"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"extra","namespace":"default"},"status":{"phase":"Running"}}}]}`, podsPath)
	if code, body := postScript(t, srv, added); code != http.StatusOK {
		t.Fatalf("ADDED status = %d body = %v", code, body)
	}
	_, table := get(t, srv.URL+podsPath, tableAccept)
	if cells := podRow(t, table, "extra"); cells == nil {
		t.Fatal("ADDED pod missing from table rows")
	}
	_, list := get(t, srv.URL+podsPath, "")
	if podItem(list, "extra") == nil {
		t.Fatal("ADDED pod missing from list items")
	}

	deleted := fmt.Sprintf(`{"events":[{"path":%q,"type":"DELETED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"extra","namespace":"default"}}}]}`, podsPath)
	if code, body := postScript(t, srv, deleted); code != http.StatusOK {
		t.Fatalf("DELETED status = %d body = %v", code, body)
	}
	_, table = get(t, srv.URL+podsPath, tableAccept)
	if cells := podRow(t, table, "extra"); cells != nil {
		t.Fatalf("DELETED pod still in table rows: %v", cells)
	}
	_, list = get(t, srv.URL+podsPath, "")
	if podItem(list, "extra") != nil {
		t.Fatal("DELETED pod still in list items")
	}
}

// TestWatchScriptModifiedWithoutCellsKeepsRowCells pins the object-only
// update: a MODIFIED without cells keeps the existing Table row cells
// verbatim, while the List form reflects the new object state.
func TestWatchScriptModifiedWithoutCellsKeepsRowCells(t *testing.T) {
	srv := newServer(t)

	_, before := get(t, srv.URL+podsPath, tableAccept)
	seedCells := podRow(t, before, "nginx")
	if len(seedCells) == 0 {
		t.Fatal("seed table has no nginx row")
	}

	script := fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","object":%s}]}`, podsPath, failedPodObject)
	if code, body := postScript(t, srv, script); code != http.StatusOK {
		t.Fatalf("MODIFIED without cells status = %d body = %v", code, body)
	}

	_, table := get(t, srv.URL+podsPath, tableAccept)
	if cells := podRow(t, table, "nginx"); !reflect.DeepEqual(cells, seedCells) {
		t.Fatalf("MODIFIED without cells changed the row cells: %v, want the seeded %v", cells, seedCells)
	}
	_, list := get(t, srv.URL+podsPath, "")
	nginx := podItem(list, "nginx")
	if nginx == nil {
		t.Fatal("nginx missing from list after object-only MODIFIED")
	}
	if status, _ := nginx["status"].(map[string]any); status["phase"] != "Failed" {
		t.Fatalf("nginx status after object-only MODIFIED = %v, want phase Failed", nginx["status"])
	}
}

// TestWatchScriptDelayMsHoldsApplication pins the race-test hold: a delayed
// event is NOT visible immediately after the POST, then lands.
func TestWatchScriptDelayMsHoldsApplication(t *testing.T) {
	srv := newServer(t)

	script := fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","delayMs":150,"cells":["nginx","0/1","Error","3","10m"],"object":%s}]}`, podsPath, failedPodObject)
	if code, body := postScript(t, srv, script); code != http.StatusOK {
		t.Fatalf("delayed script status = %d body = %v", code, body)
	}
	_, table := get(t, srv.URL+podsPath, tableAccept)
	if cells := podRow(t, table, "nginx"); cells[2] != "Running" {
		t.Fatalf("delayed event applied immediately: %v", cells)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, table = get(t, srv.URL+podsPath, tableAccept)
		if podRow(t, table, "nginx")[2] == "Error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delayed event never applied")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWatchScriptSnapshotDuringDelayedApply pins that GET
// /__control/watch-script can be polled while a delayed event applies. The
// snapshot encoder serializes the queued event maps AFTER watches.mu is
// released, so queued maps must be write-never-after-enqueue: the apply works
// on a deep copy and writes only the scalar applied/resourceVersion back.
// Under -race this catches any aliasing between the queue and the store
// mutation (the resourceVersion stamp used to write the queued object map).
func TestWatchScriptSnapshotDuringDelayedApply(t *testing.T) {
	srv := newServer(t)

	for round := range 3 {
		script := fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","delayMs":25,"cells":["nginx","0/1","Error","3","10m"],"object":%s}]}`, podsPath, failedPodObject)
		if code, body := postScript(t, srv, script); code != http.StatusOK {
			t.Fatalf("delayed script status = %d body = %v", code, body)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			code, snap := get(t, srv.URL+"/__control/watch-script", "")
			if code != http.StatusOK {
				t.Fatalf("snapshot status = %d", code)
			}
			events, _ := snap["events"].([]any)
			if len(events) != round+1 {
				t.Fatalf("snapshot events = %d, want %d", len(events), round+1)
			}
			last, _ := events[round].(map[string]any)
			if applied, _ := last["applied"].(bool); applied {
				if rv, _ := last["resourceVersion"].(string); rv == "" {
					t.Fatalf("applied snapshot entry carries no resourceVersion: %v", last)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("delayed event never reported applied in the snapshot")
			}
		}
	}
}

// TestWatchHistoryIsBounded pins the long-running demo contract: applied
// control history and reconnect replay stay fixed-size, aliases of one list
// state share an expiry floor, and a reconnect older than that floor gets a
// connect-time Kubernetes 410 instead of an incomplete replay.
func TestWatchHistoryIsBounded(t *testing.T) {
	srv := newServer(t)
	_, seed := get(t, srv.URL+podsPath, tableAccept)
	seedRV, _ := seed["metadata"].(map[string]any)["resourceVersion"].(string)
	if seedRV == "" {
		t.Fatal("seed table has no resourceVersion")
	}

	const applied = 1100
	for i := range applied {
		err := srv.Apply(fakeapi.ScriptEvent{
			Path:  podsPath,
			Type:  "MODIFIED",
			Cells: []any{"nginx", "1/1", "Running", strconv.Itoa(i), "10m"},
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   map[string]any{"name": "nginx", "namespace": "default"},
			},
		})
		if err != nil {
			t.Fatalf("Apply event %d: %v", i, err)
		}
	}

	code, snap := get(t, srv.URL+"/__control/watch-script", "")
	if code != http.StatusOK {
		t.Fatalf("watch snapshot status = %d", code)
	}
	if got := int(snap["retainedHistory"].(float64)); got != 1024 {
		t.Fatalf("retained history = %d, want 1024", got)
	}
	if got := int(snap["retainedReplay"].(float64)); got != 1024 {
		t.Fatalf("retained replay = %d, want 1024", got)
	}
	if got := int(snap["droppedHistory"].(float64)); got != applied-1024 {
		t.Fatalf("dropped history = %d, want %d", got, applied-1024)
	}
	if got := int(snap["droppedReplay"].(float64)); got != applied-1024 {
		t.Fatalf("dropped replay = %d, want %d", got, applied-1024)
	}
	if got := len(snap["events"].([]any)); got != 1024 {
		t.Fatalf("snapshot events = %d, want retained window 1024", got)
	}
	if snap["pendingEvents"] != float64(0) || snap["pendingTimers"] != float64(0) {
		t.Fatalf("synchronous events left pending state: %v", snap)
	}
	floors, _ := snap["replayFloors"].(map[string]any)
	floor := floors[podsPath]
	if floor == nil || floors["/api/v1/pods"] != floor {
		t.Fatalf("shared pod routes do not expose one replay floor: %v", floors)
	}

	res, err := http.Get(srv.URL + "/api/v1/pods?watch=true&resourceVersion=" + seedRV)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusGone {
		_ = res.Body.Close()
		t.Fatalf("expired alias watch status = %d, want 410", res.StatusCode)
	}
	var status map[string]any
	if err := json.NewDecoder(res.Body).Decode(&status); err != nil {
		_ = res.Body.Close()
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if status["reason"] != "Expired" || status["code"] != float64(http.StatusGone) {
		t.Fatalf("expired watch body = %v, want Kubernetes Expired Status", status)
	}

	// Exactly at the floor is still replayable: only events strictly above the
	// requested RV are required, and all of them remain in the ring.
	floorRV, _ := floor.(string)
	res, err = http.Get(srv.URL + "/api/v1/pods?watch=true&resourceVersion=" + floorRV)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watch at replay floor status = %d, want 200", res.StatusCode)
	}
}

// TestWatchPendingTimersAreRemoved proves fired delayed events do not leave
// timer objects or pending queue entries behind. The snapshot polling also
// exercises the lock/copy path concurrently with timer callbacks under -race.
func TestWatchPendingTimersAreRemoved(t *testing.T) {
	srv := newServer(t)
	const events = 128
	for i := range events {
		if err := srv.Apply(fakeapi.ScriptEvent{
			Path:    podsPath,
			Type:    "MODIFIED",
			DelayMs: 5 + i%5,
			Cells:   []any{"nginx", "1/1", "Running", strconv.Itoa(i), "10m"},
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   map[string]any{"name": "nginx", "namespace": "default"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, snap := get(t, srv.URL+"/__control/watch-script", "")
		if snap["pendingEvents"] == float64(0) && snap["pendingTimers"] == float64(0) {
			if snap["retainedHistory"] != float64(events) || snap["retainedReplay"] != float64(events) {
				t.Fatalf("applied delayed history/replay = %v", snap)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delayed timers were not removed: %v", snap)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWatchPendingAdmissionIsAtomic pins both denial bounds on the public
// control surface: an oversized delayed batch queues nothing, and an oversized
// request body is rejected before decoding can retain arbitrary input.
func TestWatchPendingAdmissionIsAtomic(t *testing.T) {
	srv := newServer(t)
	const pendingLimit = 1024
	events := make([]fakeapi.ScriptEvent, pendingLimit+1)
	for i := range events {
		events[i] = fakeapi.ScriptEvent{
			Path:    podsPath,
			Type:    "MODIFIED",
			DelayMs: 60_000,
			Cells:   []any{"nginx", "1/1", "Running", strconv.Itoa(i), "10m"},
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   map[string]any{"name": "nginx", "namespace": "default"},
			},
		}
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		t.Fatal(err)
	}
	code, response := postScript(t, srv, string(body))
	if code != http.StatusTooManyRequests {
		t.Fatalf("oversized pending batch status = %d body=%v, want 429", code, response)
	}
	_, snap := get(t, srv.URL+"/__control/watch-script", "")
	if snap["pendingEvents"] != float64(0) || snap["pendingTimers"] != float64(0) || len(snap["events"].([]any)) != 0 {
		t.Fatalf("rejected batch partially queued state: %v", snap)
	}

	oversizedBody := `{"events":[]}` + strings.Repeat(" ", (4<<20)+1)
	code, response = postScript(t, srv, oversizedBody)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized script body status = %d body=%v, want 413", code, response)
	}
}

// TestFailListsModes pins the fail-lists contract: 403 renders the real
// apiserver Forbidden Status naming verb/resource/namespace, 500 an
// InternalError Status, object GETs stay healthy, and mode=off untoggles.
func TestFailListsModes(t *testing.T) {
	srv := newServer(t)

	if code, _ := get(t, srv.URL+"/__control/fail-lists?mode=nope", ""); code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want 400", code)
	}

	if code, _ := get(t, srv.URL+"/__control/fail-lists?mode=403", ""); code != http.StatusOK {
		t.Fatalf("arm 403 status = %d", code)
	}
	code, status := get(t, srv.URL+podsPath, tableAccept)
	if code != http.StatusForbidden {
		t.Fatalf("list status = %d, want 403", code)
	}
	if status["kind"] != "Status" || status["reason"] != "Forbidden" || status["code"] != float64(403) {
		t.Fatalf("403 body is not a Forbidden Status: %v", status)
	}
	message, _ := status["message"].(string)
	if !strings.Contains(message, `cannot list resource "pods"`) || !strings.Contains(message, `in the namespace "default"`) {
		t.Fatalf("403 message does not name verb/resource/namespace: %q", message)
	}
	code, status = get(t, srv.URL+"/api/v1/nodes", "")
	if code != http.StatusForbidden {
		t.Fatalf("cluster-scope list status = %d, want 403", code)
	}
	if status["kind"] != "Status" || status["reason"] != "Forbidden" {
		t.Fatalf("cluster-scope 403 body is not a Forbidden Status: %v", status)
	}
	message, _ = status["message"].(string)
	if !strings.Contains(message, "at the cluster scope") {
		t.Fatalf("cluster-scope 403 message lacks the scope clause: %q", message)
	}
	if code, obj := get(t, srv.URL+podsPath+"/nginx", ""); code != http.StatusOK || obj["kind"] != "Pod" {
		t.Fatalf("object GET affected by fail-lists: %d %v", code, obj["kind"])
	}

	if code, _ := get(t, srv.URL+"/__control/fail-lists?mode=500", ""); code != http.StatusOK {
		t.Fatalf("arm 500 status = %d", code)
	}
	code, status = get(t, srv.URL+podsPath, "")
	if code != http.StatusInternalServerError || status["reason"] != "InternalError" {
		t.Fatalf("500 mode response = %d %v", code, status)
	}

	if code, _ := get(t, srv.URL+"/__control/fail-lists?mode=off", ""); code != http.StatusOK {
		t.Fatalf("untoggle status = %d", code)
	}
	if code, _ = get(t, srv.URL+podsPath, ""); code != http.StatusOK {
		t.Fatalf("list after untoggle status = %d, want 200", code)
	}
}

// TestWatch401IsOneShot pins the one-shot 401: the armed flag fails exactly
// the next watch request, then CLEARS — a second watch request streams 200 —
// and leaves plain lists untouched.
// TestWatch401PathScopeIsHonoured pins the ?path= scope: an arm naming one
// collection route is consumed ONLY by a watch on that exact route, so a spec
// can aim the 401 at the consumer it means to break. Path aliases
// (/api/v1/pods and its namespaced spelling) share one list state and are woken
// by the same events, so an unscoped arm would be a race between their
// reconnects.
func TestWatch401PathScopeIsHonoured(t *testing.T) {
	srv := newServer(t)
	const aliasPath = "/api/v1/pods"

	if code, _ := get(t, srv.URL+"/__control/watch-401?path="+podsPath, ""); code != http.StatusOK {
		t.Fatal("arming scoped watch-401 failed")
	}

	// The alias route shares the list state but is a different route: it must
	// NOT consume an arm aimed at the namespaced spelling.
	res, err := http.Get(srv.URL + aliasPath + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alias watch status = %d, want 200 (it consumed another route's arm)", res.StatusCode)
	}

	// The named route still gets it, and the one-shot then clears.
	code, status := get(t, srv.URL+podsPath+"?watch=true", "")
	if code != http.StatusUnauthorized || status["reason"] != "Unauthorized" {
		t.Fatalf("scoped watch response = %d %v, want 401 Unauthorized Status", code, status)
	}
	res, err = http.Get(srv.URL + podsPath + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("second watch status = %d, want 200 (scoped one-shot did not clear)", res.StatusCode)
	}
}

func TestWatch401IsOneShot(t *testing.T) {
	srv := newServer(t)

	if code, _ := get(t, srv.URL+"/__control/watch-401", ""); code != http.StatusOK {
		t.Fatal("arming watch-401 failed")
	}
	code, status := get(t, srv.URL+podsPath+"?watch=true", "")
	if code != http.StatusUnauthorized || status["reason"] != "Unauthorized" {
		t.Fatalf("armed watch response = %d %v, want 401 Unauthorized Status", code, status)
	}
	if code, _ := get(t, srv.URL+podsPath, ""); code != http.StatusOK {
		t.Fatalf("plain list after one-shot 401 status = %d, want 200", code)
	}

	// The one-shot must have disarmed: a second watch request gets 200 headers
	// immediately (serveWatch writes and flushes them, then holds the stream
	// open); closing the body releases the held connection.
	res, err := http.Get(srv.URL + podsPath + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("second watch status = %d, want 200 (one-shot 401 did not clear)", res.StatusCode)
	}
}

// TestLimitReturnsRemainingItemCount pins the chunked-list shape the sidebar
// counts consume: limit=1 over a 2-item collection leaves remainingItemCount 1
// and a continue token, on both the List and Table forms.
func TestLimitReturnsRemainingItemCount(t *testing.T) {
	srv := newServer(t)

	_, list := get(t, srv.URL+podsPath+"?limit=1", "")
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("limited list items = %d, want 1", len(items))
	}
	meta := list["metadata"].(map[string]any)
	if meta["remainingItemCount"] != float64(1) || meta["continue"] == "" {
		t.Fatalf("limited list metadata = %v, want remainingItemCount 1 + continue", meta)
	}

	_, table := get(t, srv.URL+podsPath+"?limit=1", tableAccept)
	rows, _ := table["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("limited table rows = %d, want 1", len(rows))
	}
	if table["metadata"].(map[string]any)["remainingItemCount"] != float64(1) {
		t.Fatalf("limited table metadata = %v", table["metadata"])
	}

	_, full := get(t, srv.URL+podsPath+"?limit=10", "")
	fullMeta := full["metadata"].(map[string]any)
	if _, present := fullMeta["remainingItemCount"]; present {
		t.Fatalf("limit >= total must not set remainingItemCount: %v", fullMeta)
	}
	if items, _ = full["items"].([]any); len(items) != 2 {
		t.Fatalf("limit >= total items = %d, want 2", len(items))
	}
}

// TestResetRestoresSeededState pins /__control/reset: mutations and flags are
// rolled back to the embedded fixture state.
func TestResetRestoresSeededState(t *testing.T) {
	srv := newServer(t)

	script := fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","cells":["nginx","0/1","Error","3","10m"],"object":%s}]}`, podsPath, failedPodObject)
	if code, _ := postScript(t, srv, script); code != http.StatusOK {
		t.Fatal("script failed")
	}
	if code, _ := get(t, srv.URL+"/__control/fail-lists?mode=500", ""); code != http.StatusOK {
		t.Fatal("arming fail-lists failed")
	}

	if code, _ := get(t, srv.URL+"/__control/reset", ""); code != http.StatusOK {
		t.Fatal("reset failed")
	}
	code, table := get(t, srv.URL+podsPath, tableAccept)
	if code != http.StatusOK {
		t.Fatalf("list after reset status = %d", code)
	}
	if cells := podRow(t, table, "nginx"); cells[2] != "Running" {
		t.Fatalf("nginx row after reset = %v, want seeded Running", cells)
	}
}

// TestResetExpiresPreResetWatchCursors pins the other half of /__control/reset:
// the fresh collections restart ABOVE every resourceVersion the old ones
// issued, so a consumer that reconnects across the reset with its old cursor is
// answered 410 Expired and relists rather than silently resuming against a
// collection it no longer describes. This is what a real apiserver does once
// its watch cache no longer covers the cursor, and it is what makes `reset`
// mean isolation for a consumer (readout's WatchHub) whose watches outlive the
// browser that opened them. A watch taken at the POST-reset collection RV is
// still served: the floor expires stale cursors, not fresh ones.
func TestResetExpiresPreResetWatchCursors(t *testing.T) {
	srv := newServer(t)
	_, seed := get(t, srv.URL+podsPath, tableAccept)
	staleRV, _ := seed["metadata"].(map[string]any)["resourceVersion"].(string)
	if staleRV == "" {
		t.Fatal("seed table has no resourceVersion")
	}
	// Advance the collection so the pre-reset cursor is above the seeded value
	// -- a floor that only compared against the seed would not catch it.
	if err := srv.Apply(fakeapi.ScriptEvent{
		Path:  podsPath,
		Type:  "MODIFIED",
		Cells: []any{"nginx", "0/1", "Error", "3", "10m"},
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "nginx", "namespace": "default"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, advanced := get(t, srv.URL+podsPath, tableAccept)
	advancedRV, _ := advanced["metadata"].(map[string]any)["resourceVersion"].(string)
	if advancedRV == staleRV {
		t.Fatal("applied event did not advance the collection resourceVersion")
	}

	if code, _ := get(t, srv.URL+"/__control/reset", ""); code != http.StatusOK {
		t.Fatal("reset failed")
	}

	for _, rv := range []string{staleRV, advancedRV} {
		res, err := http.Get(srv.URL + podsPath + "?watch=true&resourceVersion=" + rv)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusGone {
			t.Fatalf("watch from pre-reset cursor %s = %d, want 410", rv, res.StatusCode)
		}
	}

	// The relisting consumer's own cursor is honoured: the fresh collection
	// reports the new baseline, and a watch from it is served.
	_, fresh := get(t, srv.URL+podsPath, tableAccept)
	freshRV, _ := fresh["metadata"].(map[string]any)["resourceVersion"].(string)
	if freshRV == staleRV || freshRV == advancedRV {
		t.Fatalf("post-reset collection resourceVersion = %s, want a fresh baseline", freshRV)
	}
	res, err := http.Get(srv.URL + podsPath + "?watch=true&resourceVersion=" + freshRV)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watch from the post-reset cursor = %d, want 200", res.StatusCode)
	}
}

// TestWithListenAddressListenFailure pins the WithListenAddress error path: a
// busy address is a constructor error naming the address. New closes the
// default httptest listener BEFORE attempting the custom listen, so this
// failure leaks no socket.
func TestWithListenAddressListenFailure(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()

	srv, err := fakeapi.New(fakeapi.WithListenAddress(blocker.Addr().String()))
	if err == nil {
		srv.Close()
		t.Fatal("New with a busy listen address succeeded, want error")
	}
	if !strings.Contains(err.Error(), blocker.Addr().String()) {
		t.Fatalf("listen error %q does not name the address", err)
	}
}

// TestUnknownScriptPathRejected pins POST-time validation: a typo'd path is a
// 400, not a silent no-op.
func TestUnknownScriptPathRejected(t *testing.T) {
	srv := newServer(t)
	script := `{"events":[{"path":"/api/v1/namespaces/default/nonexistent","type":"MODIFIED","object":{"metadata":{"name":"x"}}}]}`
	code, body := postScript(t, srv, script)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown path status = %d body = %v, want 400", code, body)
	}
}
