package demo

// breathing.go is the shared looping driver that produces gentle,
// referentially-safe churn so readout's Live/SSE surface looks alive in the
// demo. It drives each fakekube engine through Server.Apply (NOT the /__control/
// surface, which the demo omits): on every tick it emits one MODIFIED event per
// engine that bumps the restart counter of an already-seeded healthy pod and
// then settles it back, so a list reflects the change synchronously (DelayMs ==
// 0) and every open watch receives a frame.
//
// The churn is deliberately gentle and referentially safe AND non-destructive:
// each target's FULL seeded row (its cells + complete object — containers,
// creationTimestamp, labels, the works) is captured once at startup, and every
// pulse deep-copies that row, bumps only the container restartCount + the
// Restarts cell, and posts the FULL object back. A pulse therefore never strips
// the pod down to a metadata stub (which would blank its CREATED cell in the
// list and its containers section on the detail page). All visitors share the
// one engine state, so the breathing all sees the same beat.
//
// The driver is pausable and stoppable so a later snapshot unit can freeze the
// engines for a deterministic capture: Pause() halts the ticker (state stays at
// whatever the last tick left), Stop() halts it permanently and releases the
// goroutine.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	fakekube "github.com/kbelokon/readout/internal/fakekube"
)

// breathInterval is the tick period of the breathing loop. Slow enough that the
// churn reads as a gentle pulse (not a strobe), fast enough that a Live viewer
// sees motion within a couple of seconds.
const breathInterval = 3 * time.Second

// breathTarget names one engine's breathing victim: the pods list route to
// MODIFY and the already-seeded pod (by name+namespace) whose Restarts cell the
// loop pulses. The pod must exist in the seeded cluster, so the MODIFIED upsert
// matches an existing row rather than inventing one. baseCells / baseObject hold
// the pod's FULL seeded row captured at startup; each pulse deep-copies them so
// the emitted MODIFIED event preserves the complete object (containers,
// creationTimestamp, labels) and only the restart count changes.
type breathTarget struct {
	server    *fakekube.Server
	listPath  string
	name      string
	namespace string
	app       string

	// baseCells / baseObject are the pod's seeded Table row, captured once. When
	// baseObject is nil (the row could not be read), the target falls back to a
	// minimal stub pulse rather than not breathing at all.
	baseCells  []any
	baseObject map[string]any
}

// breathTargetsFor pairs each demo cluster's engine with a healthy pod to pulse,
// DISCOVERED at startup from the cluster's busy frontend namespace rather than
// hardcoded — so the target survives scenario changes (a renamed pod no longer
// silently stops the breath). A cluster with no healthy pod there simply does
// not breathe rather than fabricating an object.
func breathTargetsFor(servers []*fakekube.Server, names []string) []breathTarget {
	nsByCluster := map[string]string{
		"prod":    "storefront",
		"staging": "storefront",
	}
	var targets []breathTarget
	for i, srv := range servers {
		if i >= len(names) {
			break
		}
		ns, ok := nsByCluster[names[i]]
		if !ok {
			continue
		}
		listPath := "/api/v1/namespaces/" + ns + "/pods"
		name, cells, object := firstRunningPod(srv, listPath)
		if name == "" {
			continue
		}
		targets = append(targets, breathTarget{
			server:     srv,
			listPath:   listPath,
			name:       name,
			namespace:  ns,
			app:        "storefront-web",
			baseCells:  cells,
			baseObject: object,
		})
	}
	return targets
}

// firstRunningPod queries the engine's pods Table for the namespace and returns
// the first Running pod's name plus its full row (cells + object), so the
// breathing target is taken from the live scenario, not a hardcoded name.
func firstRunningPod(srv *fakekube.Server, listPath string) (string, []any, map[string]any) {
	req, err := http.NewRequest(http.MethodGet, srv.URL+listPath, nil)
	if err != nil {
		return "", nil, nil
	}
	req.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Rows []struct {
			Cells  []any          `json:"cells"`
			Object map[string]any `json:"object"`
		} `json:"rows"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return "", nil, nil
	}
	for _, r := range doc.Rows {
		if len(r.Cells) >= 3 {
			if status, _ := r.Cells[2].(string); status == "Running" {
				name, _ := r.Cells[0].(string)
				return name, r.Cells, r.Object
			}
		}
	}
	return "", nil, nil
}

// BreathingDriver loops over a set of engines emitting one gentle MODIFIED pulse
// per tick. It is safe for the lifetime of a demo process: Pause/Stop are
// idempotent and may be called from any goroutine.
type BreathingDriver struct {
	targets []breathTarget

	mu      sync.Mutex
	ticker  *time.Ticker
	stop    chan struct{}
	running bool
	stopped bool
	// beat counts ticks so the pulse alternates (restarts up on even beats,
	// back down on odd beats) — a gentle two-state breath rather than an
	// ever-climbing counter.
	beat int
}

// NewBreathingDriver builds a driver over the demo engines, pairing each with
// the healthy pod it pulses (servers and clusterNames are positionally aligned,
// as StartEngines returns them). The driver does not tick until Start is called.
func NewBreathingDriver(servers []*fakekube.Server, clusterNames []string) *BreathingDriver {
	return &BreathingDriver{
		targets: breathTargetsFor(servers, clusterNames),
		stop:    make(chan struct{}),
	}
}

// Start begins the breathing loop. It is a no-op if already running or after
// Stop. With no targets the loop never starts (nothing to breathe).
func (d *BreathingDriver) Start() {
	d.mu.Lock()
	if d.running || d.stopped || len(d.targets) == 0 {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.ticker = time.NewTicker(breathInterval)
	ticker := d.ticker
	// Capture this run's stop channel as a local so the goroutine never reads
	// the d.stop field concurrently with a Pause/Stop that replaces it.
	stop := d.stop
	d.mu.Unlock()

	go func() {
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				d.pulse()
			}
		}
	}()
}

// Pause halts the ticker, leaving the engine state at whatever the last tick
// produced. A later Start resumes the loop. Idempotent.
func (d *BreathingDriver) Pause() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.running {
		return
	}
	d.running = false
	if d.ticker != nil {
		d.ticker.Stop()
		d.ticker = nil
	}
	// Replace the stop channel so a paused-then-restarted driver gets a fresh
	// signal lane; the old goroutine already returned on the close below.
	close(d.stop)
	d.stop = make(chan struct{})
}

// Stop halts the loop permanently and releases the goroutine. After Stop, Start
// is a no-op. Idempotent.
func (d *BreathingDriver) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	if d.ticker != nil {
		d.ticker.Stop()
		d.ticker = nil
	}
	if d.running {
		d.running = false
		close(d.stop)
	}
}

// restartsCellIndex is the Restarts column position in the pods Table
// (Name, Ready, Status, Restarts, Age).
const restartsCellIndex = 3

// pulse emits one MODIFIED event per target, alternating the restart count up
// and back down so the churn is a gentle two-state breath. The event carries the
// target pod's FULL captured object (deep-copied per pulse) with only the
// container restartCount bumped, and the FULL row cells with only the Restarts
// cell updated — so a pulse never blanks the pod's CREATED list cell or its
// detail containers section. The MODIFIED upsert matches the existing seeded row
// by name+namespace (no dangling reference); DelayMs == 0 makes the change
// visible to the next LIST and to open watches synchronously.
func (d *BreathingDriver) pulse() {
	d.mu.Lock()
	d.beat++
	restarts := d.beat % 2 // 0,1,0,1,... — a quiet breath
	targets := d.targets
	d.mu.Unlock()

	for _, t := range targets {
		ev := t.pulseEvent(restarts)
		// Apply errors only on a malformed event or unknown path; the targets
		// are derived from the seeded scenario, so a breathing pulse is
		// best-effort and never fails the demo if a path drifts.
		_ = t.server.Apply(ev)
	}
}

// pulseEvent builds one target's MODIFIED event for the given restart count.
// With a captured full row it deep-copies that row and bumps only the restart
// count (object container statuses + the Restarts cell); without one it falls
// back to a minimal stub so the target still breathes.
func (t *breathTarget) pulseEvent(restarts int) fakekube.ScriptEvent {
	if t.baseObject == nil {
		return fakekube.ScriptEvent{
			Path: t.listPath,
			Type: "MODIFIED",
			// Pods table columns: Name, Ready, Status, Restarts, Age.
			Cells: []any{t.name, "1/1", "Running", strconv.Itoa(restarts), "10m"},
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]any{
					"name":      t.name,
					"namespace": t.namespace,
					"labels":    map[string]any{"app": t.app},
				},
				"status": map[string]any{"phase": "Running"},
			},
		}
	}
	object := deepCopyMap(t.baseObject)
	bumpRestartCount(object, restarts)
	cells := deepCopySlice(t.baseCells)
	if restartsCellIndex < len(cells) {
		cells[restartsCellIndex] = strconv.Itoa(restarts)
	}
	return fakekube.ScriptEvent{
		Path:   t.listPath,
		Type:   "MODIFIED",
		Cells:  cells,
		Object: object,
	}
}

// bumpRestartCount sets every container status's restartCount to n on a pod wire
// object, the only field the breath mutates.
func bumpRestartCount(object map[string]any, n int) {
	status, ok := object["status"].(map[string]any)
	if !ok {
		return
	}
	statuses, ok := status["containerStatuses"].([]any)
	if !ok {
		return
	}
	for _, cs := range statuses {
		if m, ok := cs.(map[string]any); ok {
			m["restartCount"] = float64(n)
		}
	}
}

// deepCopyMap / deepCopySlice clone decoded-JSON values via a JSON round-trip so
// a pulse never aliases the captured base row (the base must stay pristine for
// the next pulse).
func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

func deepCopySlice(s []any) []any {
	if s == nil {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var out []any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}
