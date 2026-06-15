package fakekube

// bigfixtures.go generates the list-virtualization fixtures: a dedicated "big"
// namespace whose pods and events collections carry 600 rows each. The data is
// ORDINARY seeded store state (no injection endpoint): every LIST
// response (first paint, refresh tick, sort swap) serves the complete dataset,
// exactly like the hand-written JSON fixtures, and the /__control/watch-script
// mutation surface applies to it the same way. Generated in Go instead of
// checked-in JSON only because 1200 near-identical rows are noise as files;
// the output is deterministic (no randomness, no clock).

import "fmt"

// bigRowCount is the fixture size: comfortably above the windowing
// threshold (~500) so the client-side virtualizer engages on these lists.
const bigRowCount = 600

// bigPodName renders the deterministic pod name for 1-based row i:
// big-pod-0001 ... big-pod-0600 (zero-padded so name order == numeric order).
func bigPodName(i int) string {
	return fmt.Sprintf("big-pod-%04d", i)
}

// bigTableSkeleton is the shared meta.k8s.io Table envelope.
func bigTableSkeleton(columns []map[string]any, rows []any) map[string]any {
	return map[string]any{
		"kind":              "Table",
		"apiVersion":        "meta.k8s.io/v1",
		"metadata":          map[string]any{"resourceVersion": "12345"},
		"columnDefinitions": columns,
		"rows":              rows,
	}
}

func bigColumn(name, format string) map[string]any {
	return map[string]any{"name": name, "type": "string", "format": format, "priority": 0}
}

// bigPodsTable is the 600-row pods Table for namespace "big" (the kubectl
// printer column set the small pods fixture uses). Statuses mix Running and
// Pending deterministically (every 7th pod Pending) so a Status sort visibly
// reorders the dataset -- the selection-survives-sort e2e depends on the
// selected row MOVING.
func bigPodsTable() map[string]any {
	rows := make([]any, 0, bigRowCount)
	for i := 1; i <= bigRowCount; i++ {
		name := bigPodName(i)
		status, ready, restarts := "Running", "1/1", "0"
		if i%7 == 0 {
			status, ready, restarts = "Pending", "0/1", "0"
		}
		rows = append(rows, map[string]any{
			"cells": []any{name, ready, status, restarts, "10m"},
			"object": map[string]any{
				"kind":       "PartialObjectMetadata",
				"apiVersion": "meta.k8s.io/v1",
				"metadata": map[string]any{
					"name":              name,
					"namespace":         "big",
					"uid":               fmt.Sprintf("00000000-0000-0000-0001-%012d", i),
					"resourceVersion":   "12340",
					"creationTimestamp": "2026-06-08T12:00:00Z",
				},
			},
		})
	}
	return bigTableSkeleton([]map[string]any{
		bigColumn("Name", "name"),
		bigColumn("Ready", ""),
		bigColumn("Status", ""),
		bigColumn("Restarts", ""),
		bigColumn("Age", ""),
	}, rows)
}

// bigEventsTable is the 600-row events Table for namespace "big": every row
// carries a LONG multi-clause message (it would wrap to several lines at the
// 520px msg clamp) so the windowed one-line clamp + title recovery (the
// fixed-height row law) is provable against real material.
func bigEventsTable() map[string]any {
	rows := make([]any, 0, bigRowCount)
	for i := 1; i <= bigRowCount; i++ {
		pod := bigPodName(i)
		evType, reason := "Normal", "Scheduled"
		message := fmt.Sprintf(
			"Successfully assigned big/%s to worker-1 after considering 14 candidate nodes, "+
				"binding took 240ms including volume attachment checks and topology spread scoring "+
				"across three availability zones (event row %d of the big fixture)", pod, i)
		if i%5 == 0 {
			evType, reason = "Warning", "BackOff"
			message = fmt.Sprintf(
				"Back-off restarting failed container app in pod %s_big(00000000-0000-0000-0002-%012d); "+
					"the container exited with code 1 four times in the last ten minutes and the kubelet "+
					"is now waiting 5m0s before the next restart attempt (event row %d of the big fixture)",
				pod, i, i)
		}
		rows = append(rows, map[string]any{
			"cells": []any{"5m", evType, reason, "pod/" + pod, message},
			"object": map[string]any{
				"kind":       "Event",
				"apiVersion": "v1",
				"metadata": map[string]any{
					"name":              pod + ".ev1",
					"namespace":         "big",
					"uid":               fmt.Sprintf("00000000-0000-0000-0003-%012d", i),
					"resourceVersion":   "12340",
					"creationTimestamp": "2026-06-08T12:00:00Z",
				},
				"type":           evType,
				"reason":         reason,
				"message":        message,
				"count":          1,
				"firstTimestamp": "2026-06-08T12:00:00Z",
				"lastTimestamp":  "2026-06-08T12:05:00Z",
				"source":         map[string]any{"component": "default-scheduler"},
				"involvedObject": map[string]any{
					"kind":      "Pod",
					"name":      pod,
					"namespace": "big",
					"uid":       fmt.Sprintf("00000000-0000-0000-0001-%012d", i),
				},
			},
		})
	}
	return bigTableSkeleton([]map[string]any{
		bigColumn("Last Seen", ""),
		bigColumn("Type", ""),
		bigColumn("Reason", ""),
		bigColumn("Object", ""),
		bigColumn("Message", ""),
	}, rows)
}
