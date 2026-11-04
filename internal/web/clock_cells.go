package web

import (
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
)

// clock_cells.go owns the list cells whose text is a function of "now": Age,
// a pod's "(5m ago)" restart suffix, a running Job's Duration, a CronJob's Last
// Schedule and an Event's Last/First Seen. The apiserver prints each of them
// once, at Table conversion time, so a printed value is only as fresh as the
// LIST or watch event that carried it. A Live list reads a cached revision that
// changes only when its object does, which froze these cells.
//
// The fix keeps the object's own timestamp as the source of truth:
//   - refreshClockCells rewrites the raw cells from that timestamp on the
//     render-local table, before filters, sort and TSV read them;
//   - the cell view carries the timestamp (cellClock) into the markup, where
//     readout.js advances the text and the age colour every second;
//   - the Live row digest keeps the timestamp but drops the generated text and
//     colour (resourceStateTableRow), so the clock alone never pushes a row.
//
// Only columns whose meaning is proven get a clock: the printer's own column
// (its Description is the apiserver's), on the built-in resource that prints
// it. A label, custom or CRD column that merely shares a name keeps the
// printed text.

type clockColumn uint8

const (
	clockColumnNone clockColumn = iota
	clockColumnAge
	clockColumnRestarts
	clockColumnJobDuration
	clockColumnLastSchedule
	clockColumnEventLastSeen
	clockColumnEventFirstSeen
)

// clockFormat names how the browser renders one clock-bearing element. The
// values are the data-ro-tf attribute contract with clock-cells.ts.
type clockFormat string

const (
	clockFormatAge        clockFormat = "age"     // "5m" + the age colour (60s grace), Age and First Seen
	clockFormatCreated    clockFormat = "created" // colour only, the absolute Created cell
	clockFormatAgo        clockFormat = "ago"     // "5m ago" + the age colour, CronJob Last Schedule
	clockFormatDuration   clockFormat = "dur"     // "5m", a running Job's Duration
	clockFormatRestartAgo clockFormat = "rago"    // "(5m ago)", the restart suffix
	clockFormatEvent      clockFormat = "ev"      // "5m" + the age colour, an Event's Last Seen
	clockFormatEventFirst clockFormat = "evfirst" // "(first 5m ago)", an Event's second layer
)

// cellClock is the clock descriptor a cell view carries into the markup. A
// zero Format means the cell has no clock. RestSince is the second anchor of a
// two-layer Event age (its "(first … ago)" span); zero when that layer is not
// rendered.
type cellClock struct {
	Since     int64
	Format    clockFormat
	RestSince int64
}

// creationTimestampDoc is the Description every built-in printer gives its Age
// column; crdCreationTimestampDoc is the one the CRD table convertor gives the
// default Age column (a date column over .metadata.creationTimestamp without a
// description of its own).
var (
	creationTimestampDoc    = metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]
	crdCreationTimestampDoc = "Custom resource definition column (in JSONPath format): .metadata.creationTimestamp"
)

// clockColumnFor classifies one column of a printer table. A column without a
// Description (label, custom and synthetic columns, fixtures) never qualifies,
// and a label column never does.
func clockColumnFor(rt *kube.ResourceType, col *kube.Column) clockColumn {
	if col.Label != "" || col.Description == "" {
		return clockColumnNone
	}
	switch {
	case col.Name == "Age":
		if col.Description == creationTimestampDoc || (col.Type == "date" && col.Description == crdCreationTimestampDoc) {
			return clockColumnAge
		}
	case rt.Group == "" && rt.Plural == "pods" && col.Name == "Restarts":
		return clockColumnRestarts
	case rt.Group == "batch" && rt.Plural == "jobs" && col.Name == "Duration":
		return clockColumnJobDuration
	case rt.Group == "batch" && rt.Plural == "cronjobs" && col.Name == "Last Schedule":
		return clockColumnLastSchedule
	case (rt.Group == "" || rt.Group == "events.k8s.io") && rt.Plural == "events":
		switch col.Name {
		case "Last Seen":
			return clockColumnEventLastSeen
		case "First Seen":
			return clockColumnEventFirstSeen
		}
	}
	return clockColumnNone
}

// clockAnchor resolves the timestamp a clock column counts from, read from the
// row's own object. ok is false when the object does not carry it (a Job that
// has not started or has completed, a CronJob that never ran, a pod whose
// printer shows no restart suffix): the printed text then stands.
func clockAnchor(kind clockColumn, row *kube.Row, printed string) (time.Time, bool) {
	obj := row.Object
	switch kind {
	case clockColumnAge:
		return parseTimestamp(nestedString(obj, "metadata", "creationTimestamp"))
	case clockColumnLastSchedule:
		return parseTimestamp(nestedString(obj, "status", "lastScheduleTime"))
	case clockColumnJobDuration:
		if nestedString(obj, "status", "completionTime") != "" {
			return time.Time{}, false
		}
		return parseTimestamp(nestedString(obj, "status", "startTime"))
	case clockColumnRestarts:
		// The printer adds the suffix only when it found a restart date; trust
		// that decision and only re-derive the date behind it.
		if _, ago := splitRestarts(printed); ago == "" {
			return time.Time{}, false
		}
		return podLastRestart(obj)
	case clockColumnEventLastSeen, clockColumnEventFirstSeen:
		item, ok := decodeEventItem(obj)
		if !ok {
			return time.Time{}, false
		}
		if kind == clockColumnEventLastSeen {
			return parseEventTime(item.lastSeen())
		}
		return parseEventTime(item.firstSeen())
	}
	return time.Time{}, false
}

func parseTimestamp(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, value)
	return t, err == nil
}

// refreshClockCells rewrites every clock cell of a printer table from its
// object's timestamp at now, exactly as the printer would have printed it at
// that instant. It runs on the render-local table before any decoration,
// filter or sort, so `Age>5m`, the sort order, TSV and the rendered text all
// read the same current value even when the table came from a cached Live
// revision. The caller owns the table: the Live path passes a render clone.
func refreshClockCells(table *kube.Table, now time.Time) {
	kinds := make([]clockColumn, len(table.Columns))
	found := false
	for i := range table.Columns {
		kinds[i] = clockColumnFor(&table.Resource, &table.Columns[i])
		found = found || kinds[i] != clockColumnNone
	}
	if !found {
		return
	}
	for r := range table.Rows {
		row := &table.Rows[r]
		for c, kind := range kinds {
			if kind == clockColumnNone || c >= len(row.Cells) {
				continue
			}
			printed := cellDisplayString(row.Cells[c])
			since, ok := clockAnchor(kind, row, printed)
			if !ok {
				continue
			}
			age := duration.HumanDuration(now.Sub(since))
			if kind == clockColumnRestarts {
				count, _ := splitRestarts(printed)
				row.Cells[c] = count + " (" + age + " ago)"
				continue
			}
			row.Cells[c] = age
		}
	}
}

// elapsedAgeClass buckets an exact elapsed time into the .age-* colour as a
// fraction of 24h: <10% fresh, <35% recent, <65% day, <100% week, else old.
// grace is subtracted first (the Age column's 60s, see Server.ageClass).
// clock-cells.ts ports this function; the two must stay identical.
func elapsedAgeClass(elapsed, grace time.Duration) string {
	seconds := (elapsed - grace).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	fraction := seconds / (24 * time.Hour).Seconds()
	switch {
	case fraction < 0.10:
		return "age-fresh"
	case fraction < 0.35:
		return "age-recent"
	case fraction < 0.65:
		return "age-day"
	case fraction < 1.0:
		return "age-week"
	}
	return "age-old"
}

// ageClassGrace is the Age column's grace: an object younger than a minute
// past its creation reads as fresh regardless of clock skew.
const ageClassGrace = time.Minute

// replaceAgeClass swaps the .age-* bucket inside a class list.
func replaceAgeClass(classes, bucket string) string {
	fields := strings.Fields(classes)
	out := fields[:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "age-") {
			out = append(out, f)
		}
	}
	out = append(out, bucket)
	return strings.Join(out, " ")
}

// podLastRestart mirrors how the v1.33 pod printer picks the date behind the
// "N (5m ago)" restart suffix (pkg/printers/internalversion/printers.go,
// printPod): init container statuses count up to the first one still
// blocking initialization; once the pod is initialized, only restartable
// (sidecar) init containers keep counting, joined by every regular container.
func podLastRestart(obj map[string]any) (time.Time, bool) {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &pod); err != nil {
		return time.Time{}, false
	}
	restartable := make(map[string]bool, len(pod.Spec.InitContainers))
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		restartable[c.Name] = c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
	}
	var last, lastRestartable time.Time
	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		finished := lastFinished(status)
		if finished.After(last) {
			last = finished
		}
		if restartable[status.Name] && finished.After(lastRestartable) {
			lastRestartable = finished
		}
		if status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
			continue
		}
		if restartable[status.Name] && status.Started != nil && *status.Started {
			continue
		}
		initializing = true
		break
	}
	if !initializing || podInitialized(&pod) {
		last = lastRestartable
		for i := range pod.Status.ContainerStatuses {
			if finished := lastFinished(&pod.Status.ContainerStatuses[i]); finished.After(last) {
				last = finished
			}
		}
	}
	return last, !last.IsZero()
}

func lastFinished(status *corev1.ContainerStatus) time.Time {
	if status.LastTerminationState.Terminated == nil {
		return time.Time{}
	}
	return status.LastTerminationState.Terminated.FinishedAt.Time
}

func podInitialized(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodInitialized && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// attachCellClock gives a resolved cell view its clock descriptor and makes its
// age colour exact for the same instant the text was computed at, so the
// server's first paint and the browser's next tick agree. value is the raw
// cell, already refreshed by refreshClockCells on the list path.
func (s *Server) attachCellClock(cv *cellView, table *kube.Table, i int, row *kube.Row, value string) {
	kind := clockColumnFor(&table.Resource, &table.Columns[i])
	if kind == clockColumnNone {
		return
	}
	since, ok := clockAnchor(kind, row, value)
	if !ok {
		return
	}
	elapsed := s.clock().Sub(since)
	clock := cellClock{Since: since.Unix()}
	switch kind {
	case clockColumnAge, clockColumnEventFirstSeen:
		clock.Format = clockFormatAge
		cv.Class = replaceAgeClass(cv.Class, elapsedAgeClass(elapsed, ageClassGrace))
	case clockColumnJobDuration:
		clock.Format = clockFormatDuration
	case clockColumnRestarts:
		if cv.Kind != cellRestarts || cv.Ago == "" {
			return
		}
		clock.Format = clockFormatRestartAgo
	case clockColumnLastSchedule:
		if cv.Kind != cellLastRun || cv.Value == "" {
			return
		}
		clock.Format = clockFormatAgo
		cv.Class = elapsedAgeClass(elapsed, 0)
	case clockColumnEventLastSeen:
		if cv.Kind != cellEvAge {
			return
		}
		clock.Format = clockFormatEvent
		cv.Class = elapsedAgeClass(elapsed, 0)
		if cv.EvAgeRest != "" {
			if first, ok := clockAnchor(clockColumnEventFirstSeen, row, ""); ok {
				clock.RestSince = first.Unix()
			}
		}
		// The clock replaces the whole-cell volatility: the digest keeps the
		// timestamps and drops only the generated text and colour.
		cv.Volatile = false
	}
	cv.Clock = clock
}
