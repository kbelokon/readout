package fakekube_test

// These tests pin the exported in-process mutation primitive Server.Apply: the
// breathing-loop / demo path that must drive the engine WITHOUT the /__control/
// surface (the demo strips that prefix). They prove a mutation applied through
// Apply reaches open watch streams AND subsequent LIST responses, and that each
// applied event advances the collection resourceVersion monotonically — the
// ordering invariant watch replay relies on.

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	fakeapi "github.com/kbelokon/readout/internal/fakekube"
)

// newServerNoControl builds the engine the way the demo does: with NO
// /__control/ routes, so Apply is the only mutation path available.
func newServerNoControl(t *testing.T) *fakeapi.Server {
	t.Helper()
	srv, err := fakeapi.New(fakeapi.WithoutControl())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// readWatchFrame reads one watch-wire frame (one JSON object per line) from a
// streaming watch response, failing if none arrives before the deadline.
func readWatchFrame(t *testing.T, sc *bufio.Scanner) map[string]any {
	t.Helper()
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			t.Fatalf("reading watch frame: %v", err)
		}
		t.Fatal("watch stream closed before a frame arrived")
	}
	var frame map[string]any
	if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
		t.Fatalf("decode watch frame %q: %v", sc.Text(), err)
	}
	return frame
}

// frameRow returns the cells of the single Table row carried by a watch frame
// (data frames carry exactly one row).
func frameRow(t *testing.T, frame map[string]any) []any {
	t.Helper()
	obj, ok := frame["object"].(map[string]any)
	if !ok {
		t.Fatalf("watch frame has no object: %v", frame)
	}
	rows, _ := obj["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("watch frame rows = %v, want exactly one", rows)
	}
	row, _ := rows[0].(map[string]any)
	cells, _ := row["cells"].([]any)
	return cells
}

// TestApplyInProcess proves the acceptance semantic: on a server built WITHOUT
// the control surface, a mutation applied through Server.Apply (no HTTP control)
// is emitted to an open watch AND reflected by the next LIST.
func TestApplyInProcess(t *testing.T) {
	srv := newServerNoControl(t)

	// The control surface is absent — Apply is the only mutation path. (Use a
	// raw GET: a no-control server answers /__control/ with a plain-text 404,
	// which the JSON get helper cannot decode.)
	probe, err := http.Get(srv.URL + "/__control/watch-script")
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusNotFound {
		t.Fatalf("/__control/ reachable on a no-control server: status %d", probe.StatusCode)
	}

	res, err := http.Get(srv.URL + podsPath + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d, want 200", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)

	ev := fakeapi.ScriptEvent{
		Path:   podsPath,
		Type:   "MODIFIED",
		Cells:  []any{"nginx", "0/1", "Error", "3", "10m"},
		Object: failedPodMap(t),
	}
	if err := srv.Apply(ev); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	// The frame must reach the open watch — proving Apply drives the same
	// emit machinery as the control surface, with no /__control/ involved.
	frame := readWatchFrame(t, sc)
	if frame["type"] != "MODIFIED" {
		t.Fatalf("watch frame type = %v, want MODIFIED", frame["type"])
	}
	if cells := frameRow(t, frame); len(cells) < 3 || cells[2] != "Error" {
		t.Fatalf("watch frame cells = %v, want status cell Error", cells)
	}

	// The mutation must persist into subsequent LISTs (Table and plain List).
	_, table := get(t, srv.URL+podsPath, tableAccept)
	if cells := podRow(t, table, "nginx"); len(cells) < 3 || cells[2] != "Error" {
		t.Fatalf("table after Apply = %v, want status cell Error", cells)
	}
	_, list := get(t, srv.URL+podsPath, "")
	nginx := podItem(list, "nginx")
	if nginx == nil {
		t.Fatal("nginx missing from list after Apply")
	}
	if status, _ := nginx["status"].(map[string]any); status["phase"] != "Failed" {
		t.Fatalf("nginx status after Apply = %v, want phase Failed", nginx["status"])
	}
}

// TestApplyResourceVersion pins rv monotonicity: every applied event advances
// the collection resourceVersion strictly above the previous one, so watch
// replay ordering holds for the in-process driver.
func TestApplyResourceVersion(t *testing.T) {
	srv := newServerNoControl(t)

	_, seed := get(t, srv.URL+podsPath, tableAccept)
	prev := collectionRV(t, seed)

	for i := range 3 {
		ev := fakeapi.ScriptEvent{
			Path:   podsPath,
			Type:   "MODIFIED",
			Cells:  []any{"nginx", "0/1", "Error", strconv.Itoa(i), "10m"},
			Object: failedPodMap(t),
		}
		if err := srv.Apply(ev); err != nil {
			t.Fatalf("Apply round %d error: %v", i, err)
		}
		_, table := get(t, srv.URL+podsPath, tableAccept)
		got := collectionRV(t, table)
		if got <= prev {
			t.Fatalf("resourceVersion did not advance: round %d got %d, prev %d", i, got, prev)
		}
		prev = got
	}
}

// TestApplyRejectsUnknownPath pins that Apply validates: an event for a path no
// fixture serves returns an error and never mutates state (the same guard the
// control surface enforces, now on the in-process path).
func TestApplyRejectsUnknownPath(t *testing.T) {
	srv := newServerNoControl(t)

	err := srv.Apply(fakeapi.ScriptEvent{
		Path:   "/api/v1/namespaces/default/widgets",
		Type:   "MODIFIED",
		Object: map[string]any{"metadata": map[string]any{"name": "x"}},
	})
	if err == nil {
		t.Fatal("Apply accepted an event for an unknown path, want error")
	}
}

func collectionRV(t *testing.T, doc map[string]any) int64 {
	t.Helper()
	meta, _ := doc["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	n, err := strconv.ParseInt(rv, 10, 64)
	if err != nil {
		t.Fatalf("collection resourceVersion %q not numeric: %v", rv, err)
	}
	return n
}

func failedPodMap(t *testing.T) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(failedPodObject), &obj); err != nil {
		t.Fatalf("decode failedPodObject: %v", err)
	}
	return obj
}
