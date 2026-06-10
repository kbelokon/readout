package fakeapi_test

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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/tests/unit/fakeapi"
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
	if code, _ := get(t, srv.URL+"/api/v1/nodes", ""); code != http.StatusForbidden {
		t.Fatalf("cluster-scope list status = %d, want 403", code)
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
// the next watch request and leaves plain lists untouched.
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
