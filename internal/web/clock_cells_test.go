package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/util/duration"
)

type clockCellCases struct {
	Durations []struct {
		Ms   int64  `json:"ms"`
		Text string `json:"text"`
	} `json:"durations"`
	Buckets []struct {
		Ms           int64  `json:"ms"`
		GraceSeconds int64  `json:"graceSeconds"`
		Bucket       string `json:"bucket"`
	} `json:"buckets"`
}

// TestClockCellCasesMatchServerFormulas pins the shared fixture to the Go
// formulas; clock-cells.test.ts pins the same fixture to the browser port.
func TestClockCellCasesMatchServerFormulas(t *testing.T) {
	raw, err := os.ReadFile("testdata/clock_cells_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases clockCellCases
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases.Durations {
		if got := duration.HumanDuration(time.Duration(c.Ms) * time.Millisecond); got != c.Text {
			t.Errorf("HumanDuration(%dms) = %q, fixture says %q", c.Ms, got, c.Text)
		}
	}
	for _, c := range cases.Buckets {
		got := elapsedAgeClass(time.Duration(c.Ms)*time.Millisecond, time.Duration(c.GraceSeconds)*time.Second)
		if got != c.Bucket {
			t.Errorf("elapsedAgeClass(%dms, %ds) = %q, fixture says %q", c.Ms, c.GraceSeconds, got, c.Bucket)
		}
	}
}

// TestElapsedAgeClassMatchesServerAgeClass keeps the Age colour the server
// computed before this change (Server.ageClass over the RFC3339 string) and
// the exact-elapsed bucket the clock cells use in agreement.
func TestElapsedAgeClassMatchesServerAgeClass(t *testing.T) {
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC)
	srv := &Server{now: func() time.Time { return now }}
	for _, ago := range []time.Duration{0, 59 * time.Second, 61 * time.Second, 2*time.Hour + 25*time.Minute, 9 * time.Hour, 16 * time.Hour, 23*time.Hour + 59*time.Minute, 30 * time.Hour} {
		created := now.Add(-ago)
		want := srv.ageClass(created.Format(time.RFC3339))
		if got := elapsedAgeClass(ago, ageClassGrace); got != want {
			t.Errorf("%s ago: elapsedAgeClass = %q, Server.ageClass = %q", ago, got, want)
		}
	}
}

func TestClockColumnForTrustsOnlyPrinterColumns(t *testing.T) {
	core := func(plural string) kube.ResourceType { return kube.ResourceType{Plural: plural} }
	batch := func(plural string) kube.ResourceType { return kube.ResourceType{Group: "batch", Plural: plural} }
	crd := kube.ResourceType{Group: "example.com", Plural: "widgets"}
	printer := func(name string) kube.Column {
		return kube.Column{Name: name, Type: "string", Description: "printer column"}
	}
	age := kube.Column{Name: "Age", Type: "string", Description: creationTimestampDoc}

	cases := []struct {
		name string
		rt   kube.ResourceType
		col  kube.Column
		want clockColumn
	}{
		{"built-in Age", core("pods"), age, clockColumnAge},
		{"CRD default Age", crd, kube.Column{Name: "Age", Type: "date", Description: crdCreationTimestampDoc}, clockColumnAge},
		{"CRD Age over another path", crd, kube.Column{Name: "Age", Type: "date", Description: "Custom resource definition column (in JSONPath format): .status.startedAt"}, clockColumnNone},
		{"custom column named Age", core("pods"), kube.Column{Name: "Age"}, clockColumnNone},
		{"label column named Age", core("pods"), kube.Column{Name: "Age", Label: "age", Description: creationTimestampDoc}, clockColumnNone},
		{"pod Restarts", core("pods"), printer("Restarts"), clockColumnRestarts},
		{"Restarts on another kind", core("replicationcontrollers"), printer("Restarts"), clockColumnNone},
		{"Restarts on a CRD named pods", kube.ResourceType{Group: "example.com", Plural: "pods"}, printer("Restarts"), clockColumnNone},
		{"Job Duration", batch("jobs"), printer("Duration"), clockColumnJobDuration},
		{"Duration on a CRD", kube.ResourceType{Group: "example.com", Plural: "jobs"}, printer("Duration"), clockColumnNone},
		{"CronJob Last Schedule", batch("cronjobs"), printer("Last Schedule"), clockColumnLastSchedule},
		{"core Event Last Seen", core("events"), printer("Last Seen"), clockColumnEventLastSeen},
		{"events.k8s.io First Seen", kube.ResourceType{Group: "events.k8s.io", Plural: "events"}, printer("First Seen"), clockColumnEventFirstSeen},
		{"custom Last Seen", core("events"), kube.Column{Name: "Last Seen"}, clockColumnNone},
	}
	for _, c := range cases {
		if got := clockColumnFor(&c.rt, &c.col); got != c.want {
			t.Errorf("%s: clockColumnFor = %d, want %d", c.name, got, c.want)
		}
	}
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// TestRefreshClockCellsRederivesFromObjectTimestamps feeds deliberately stale
// printed values and checks every clock column is re-derived from its object,
// while values the printer left without a timestamp stay as printed.
func TestRefreshClockCellsRederivesFromObjectTimestamps(t *testing.T) {
	now := time.Date(2026, 11, 4, 16, 2, 22, 0, time.UTC)
	created := now.Add(-118 * time.Second)

	pods := &kube.Table{
		Resource: kube.ResourceType{Plural: "pods"},
		Columns: []kube.Column{
			{Name: "Name"},
			{Name: "Restarts", Description: "restarts"},
			{Name: "Age", Description: creationTimestampDoc},
		},
		Rows: []kube.Row{
			{
				Cells: []any{"restarted", "3 (5s ago)", "36s"},
				Object: map[string]any{
					"metadata": map[string]any{"name": "restarted", "creationTimestamp": rfc(created)},
					"status": map[string]any{"containerStatuses": []any{map[string]any{
						"name": "app", "restartCount": int64(3),
						"lastState": map[string]any{"terminated": map[string]any{"finishedAt": rfc(now.Add(-5 * time.Minute))}},
					}}},
				},
			},
			{
				Cells:  []any{"steady", "0", "36s"},
				Object: map[string]any{"metadata": map[string]any{"name": "steady"}},
			},
		},
	}
	refreshClockCells(pods, now)
	if got := pods.Rows[0].Cells; got[1] != "3 (5m ago)" || got[2] != "118s" {
		t.Fatalf("restarted pod cells = %v, want restarts \"3 (5m ago)\" and age \"118s\"", got)
	}
	if got := pods.Rows[1].Cells; got[1] != "0" || got[2] != "36s" {
		t.Fatalf("pod without a restart suffix or timestamp = %v, want the printed cells kept", got)
	}

	jobs := &kube.Table{
		Resource: kube.ResourceType{Group: "batch", Plural: "jobs"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Duration", Description: "duration"}},
		Rows: []kube.Row{
			{Cells: []any{"running", "4s"}, Object: map[string]any{"status": map[string]any{"startTime": rfc(now.Add(-3 * time.Minute))}}},
			{Cells: []any{"done", "4s"}, Object: map[string]any{"status": map[string]any{
				"startTime": rfc(now.Add(-3 * time.Minute)), "completionTime": rfc(now.Add(-time.Minute)),
			}}},
		},
	}
	refreshClockCells(jobs, now)
	if jobs.Rows[0].Cells[1] != "3m" || jobs.Rows[1].Cells[1] != "4s" {
		t.Fatalf("job durations = %v / %v, want the running one re-derived and the completed one fixed", jobs.Rows[0].Cells[1], jobs.Rows[1].Cells[1])
	}

	cronjobs := &kube.Table{
		Resource: kube.ResourceType{Group: "batch", Plural: "cronjobs"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Last Schedule", Description: "last"}},
		Rows: []kube.Row{
			{Cells: []any{"ran", "10s"}, Object: map[string]any{"status": map[string]any{"lastScheduleTime": rfc(now.Add(-2 * time.Hour))}}},
			{Cells: []any{"never", "<none>"}, Object: map[string]any{"status": map[string]any{}}},
		},
	}
	refreshClockCells(cronjobs, now)
	if cronjobs.Rows[0].Cells[1] != "120m" || cronjobs.Rows[1].Cells[1] != "<none>" {
		t.Fatalf("last schedules = %v / %v", cronjobs.Rows[0].Cells[1], cronjobs.Rows[1].Cells[1])
	}

	events := &kube.Table{
		Resource: kube.ResourceType{Plural: "events"},
		Columns:  []kube.Column{{Name: "Last Seen", Description: "last"}, {Name: "First Seen", Description: "first"}},
		Rows: []kube.Row{{
			Cells: []any{"1s", "1s"},
			Object: map[string]any{
				"firstTimestamp": rfc(now.Add(-time.Hour)),
				"lastTimestamp":  rfc(now.Add(-90 * time.Second)),
			},
		}},
	}
	refreshClockCells(events, now)
	if events.Rows[0].Cells[0] != "90s" || events.Rows[0].Cells[1] != "60m" {
		t.Fatalf("event cells = %v", events.Rows[0].Cells)
	}
}

func TestPodLastRestartFollowsThePrinter(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	terminated := func(at time.Time) map[string]any {
		return map[string]any{"terminated": map[string]any{"finishedAt": rfc(at), "exitCode": int64(1)}}
	}
	status := func(name string, last time.Time, state map[string]any) map[string]any {
		return map[string]any{"name": name, "restartCount": int64(1), "lastState": terminated(last), "state": state}
	}
	done := map[string]any{"terminated": map[string]any{"exitCode": int64(0), "finishedAt": rfc(t0)}}
	running := map[string]any{"running": map[string]any{}}
	always := "Always"

	cases := []struct {
		name string
		pod  map[string]any
		want time.Time
	}{
		{
			name: "latest of the regular containers",
			pod: map[string]any{"status": map[string]any{"containerStatuses": []any{
				status("a", t0.Add(time.Minute), running),
				status("b", t0.Add(3*time.Minute), running),
			}}},
			want: t0.Add(3 * time.Minute),
		},
		{
			name: "an initialized pod drops plain init restarts and keeps the sidecar's",
			pod: map[string]any{
				"spec": map[string]any{"initContainers": []any{
					map[string]any{"name": "migrate"},
					map[string]any{"name": "proxy", "restartPolicy": always},
				}},
				"status": map[string]any{
					"conditions": []any{map[string]any{"type": "Initialized", "status": "True"}},
					"initContainerStatuses": []any{
						status("migrate", t0.Add(9*time.Minute), done),
						func() map[string]any {
							s := status("proxy", t0.Add(4*time.Minute), running)
							s["started"] = true
							return s
						}(),
					},
					"containerStatuses": []any{status("app", t0.Add(2*time.Minute), running)},
				},
			},
			want: t0.Add(4 * time.Minute),
		},
		{
			name: "a pod stuck initializing counts init containers up to the blocking one",
			pod: map[string]any{
				"spec": map[string]any{"initContainers": []any{map[string]any{"name": "first"}, map[string]any{"name": "second"}}},
				"status": map[string]any{
					"initContainerStatuses": []any{
						status("first", t0.Add(time.Minute), map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}),
						status("second", t0.Add(7*time.Minute), map[string]any{"waiting": map[string]any{"reason": "PodInitializing"}}),
					},
					"containerStatuses": []any{status("app", t0.Add(8*time.Minute), map[string]any{"waiting": map[string]any{"reason": "PodInitializing"}})},
				},
			},
			want: t0.Add(time.Minute),
		},
	}
	for _, c := range cases {
		got, ok := podLastRestart(c.pod)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%s: podLastRestart = %v, %v; want %v", c.name, got, ok, c.want)
		}
	}
	if _, ok := podLastRestart(map[string]any{"status": map[string]any{}}); ok {
		t.Error("a pod without terminated containers must have no restart date")
	}
}

func clockPodTable(printedAge string, created time.Time) *kube.Table {
	return &kube.Table{
		Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name", Description: "name"},
			{Name: "Status", Description: "status"},
			{Name: "Age", Description: creationTimestampDoc},
		},
		Rows: []kube.Row{{
			Cluster: "test",
			Cells:   []any{"web", "Running", printedAge},
			Object: map[string]any{
				"kind": "Pod", "apiVersion": "v1",
				"metadata": map[string]any{"name": "web", "namespace": "default", "resourceVersion": "7", "creationTimestamp": rfc(created)},
			},
		}},
	}
}

// TestClockCellDigestIgnoresTheClockButNotTheObject is the Live contract: the
// same object rendered at two instants (crossing an age colour boundary too)
// projects the same row digest, while a new timestamp or restart count does
// not.
func TestClockCellDigestIgnoresTheClockButNotTheObject(t *testing.T) {
	created := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	render := func(now time.Time, table *kube.Table) (digest [32]byte, cellText, cellClass string) {
		t.Helper()
		app := newServer(t, baseConfig(t), now)
		clone := cloneTableForRender(table)
		req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
		app.applyTableOptions(req, nil, &clone, "default", false)
		lc := &listContext{Cluster: "test", Namespace: "default", Plural: "pods", ClusterCount: 1, Tables: []kube.Table{clone}}
		v := app.buildListView(req, lc)
		data := toListData(&v)
		row := &data.Tables[0].Rows[0]
		var err error
		digest, err = liveProjectionRowDigest(row)
		if err != nil {
			t.Fatal(err)
		}
		own, _ := parseTimestamp(nestedString(table.Rows[0].Object, "metadata", "creationTimestamp"))
		age := row.Cells[2]
		if age.Clock.Format != "age" || age.Clock.Since != own.Unix() {
			t.Fatalf("age cell clock = %+v, want the creationTimestamp %d", age.Clock, own.Unix())
		}
		if row.CreatedSince != own.Unix() {
			t.Fatalf("CreatedSince = %d, want %d", row.CreatedSince, own.Unix())
		}
		return digest, age.Value, age.Class
	}

	cached := clockPodTable("1s", created)
	early, earlyText, earlyClass := render(created.Add(2*time.Hour), cached)
	late, lateText, lateClass := render(created.Add(3*time.Hour), cached)
	if earlyText != "120m" || lateText != "3h" || earlyClass == lateClass {
		t.Fatalf("renders = %q/%q %q/%q, want current text and a crossed colour boundary", earlyText, earlyClass, lateText, lateClass)
	}
	if early != late {
		t.Fatal("a clock-only re-render changed the Live row digest")
	}
	if cached.Rows[0].Cells[2] != "1s" {
		t.Fatalf("rendering mutated the cached table: %v", cached.Rows[0].Cells)
	}

	moved := clockPodTable("1s", created.Add(time.Second))
	if other, _, _ := render(created.Add(2*time.Hour), moved); other == early {
		t.Fatal("a different creationTimestamp must change the digest")
	}

	restarts := func(count string) *kube.Table {
		table := clockPodTable("1s", created)
		table.Columns = append(table.Columns, kube.Column{Name: "Restarts", Description: "restarts"})
		table.Rows[0].Cells = append(table.Rows[0].Cells, count+" (1s ago)")
		table.Rows[0].Object["status"] = map[string]any{"containerStatuses": []any{map[string]any{
			"name": "app", "lastState": map[string]any{"terminated": map[string]any{"finishedAt": rfc(created.Add(time.Minute))}},
		}}}
		return table
	}
	three, _, _ := render(created.Add(2*time.Hour), restarts("3"))
	threeLater, _, _ := render(created.Add(3*time.Hour), restarts("3"))
	four, _, _ := render(created.Add(2*time.Hour), restarts("4"))
	if three != threeLater {
		t.Fatal("a clock-only re-render of a restart suffix changed the digest")
	}
	if three == four {
		t.Fatal("a new restart count must change the digest")
	}
}

// TestClockCellsFilterWithCurrentAges covers the agreed limit's fallback: a
// committed Age filter is evaluated against ages as of the render, so the
// next render of the same cached revision (an unrelated watch event) admits a
// row that has crossed the threshold since.
func TestClockCellsFilterWithCurrentAges(t *testing.T) {
	created := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	cached := clockPodTable("1s", created)
	rows := func(now time.Time) int {
		app := newServer(t, baseConfig(t), now)
		clone := cloneTableForRender(cached)
		req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?f=age%3E5m", nil)
		req.SetPathValue("plural", "pods")
		lc := app.streamListContext(req, nil, "test", &clone, renderOverlays{})
		return len(lc.Tables[0].Rows)
	}
	if got := rows(created.Add(4 * time.Minute)); got != 0 {
		t.Fatalf("at 4m the age>5m filter kept %d rows", got)
	}
	if got := rows(created.Add(6 * time.Minute)); got != 1 {
		t.Fatalf("at 6m the age>5m filter kept %d rows, want the pod re-admitted", got)
	}
	if !strings.Contains(cached.Rows[0].Cells[2].(string), "1s") {
		t.Fatal("filtering mutated the cached table")
	}
}
