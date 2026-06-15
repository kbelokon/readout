package fakekube

// bigfixtures.go holds the deterministic generators for the list-virtualization
// "big" namespace: 600 pods and 600 events. The data is ORDINARY seeded store
// state (no injection endpoint): every LIST response (first paint, refresh
// tick, sort swap) serves the complete dataset, and the /__control/watch-script
// mutation surface applies to it the same way. Generated in Go instead of
// checked-in JSON only because 1200 near-identical rows are noise as files; the
// output is deterministic (no randomness, no clock). The base test cluster
// (basedata.go) builds typed Pod/Event objects from these names + messages and
// attaches the explicit Table cells, so the windowing material is identical to
// the historical hand-built Table while flowing through the typed seeder.

import "fmt"

// bigRowCount is the fixture size: comfortably above the windowing threshold
// (~500) so the client-side virtualizer engages on these lists.
const bigRowCount = 600

// bigPodName renders the deterministic pod name for 1-based row i:
// big-pod-0001 ... big-pod-0600 (zero-padded so name order == numeric order).
func bigPodName(i int) string {
	return fmt.Sprintf("big-pod-%04d", i)
}

// bigEventMessageNormal is the long multi-clause Normal-event message for row i
// (it would wrap to several lines at the 520px msg clamp, so the windowed
// one-line clamp + title recovery is provable against real material).
func bigEventMessageNormal(pod string, i int) string {
	return fmt.Sprintf(
		"Successfully assigned big/%s to worker-1 after considering 14 candidate nodes, "+
			"binding took 240ms including volume attachment checks and topology spread scoring "+
			"across three availability zones (event row %d of the big fixture)", pod, i)
}

// bigEventMessageWarning is the long Warning-event message for the every-5th row.
func bigEventMessageWarning(pod string, i int) string {
	return fmt.Sprintf(
		"Back-off restarting failed container app in pod %s_big(00000000-0000-0000-0002-%012d); "+
			"the container exited with code 1 four times in the last ten minutes and the kubelet "+
			"is now waiting 5m0s before the next restart attempt (event row %d of the big fixture)",
		pod, i, i)
}
