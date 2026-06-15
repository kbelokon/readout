package demo

// breathing.go is the shared looping driver that produces gentle,
// referentially-safe churn so readout's Live/SSE surface looks alive in the
// demo. It drives each fakekube engine through Server.Apply (NOT the /__control/
// surface, which the demo omits): on every tick it emits one MODIFIED event per
// engine that bumps the restart counter of an already-seeded healthy pod and
// then settles it back, so a list reflects the change synchronously (DelayMs ==
// 0) and every open watch receives a frame.
//
// The churn is deliberately gentle and referentially safe: it only MUTATES an
// existing seeded pod (its Restarts cell + status), never introducing a new
// object or a dangling reference. All visitors share the one engine state, so
// the breathing all sees the same beat.
//
// The driver is pausable and stoppable so a later snapshot unit can freeze the
// engines for a deterministic capture: Pause() halts the ticker (state stays at
// whatever the last tick left), Stop() halts it permanently and releases the
// goroutine.

import (
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
// matches an existing row rather than inventing one.
type breathTarget struct {
	server    *fakekube.Server
	listPath  string
	name      string
	namespace string
	app       string
}

// breathTargetsFor maps each demo cluster's running engine to the healthy pod
// the breathing loop pulses. The names mirror scenario.go's healthy serving
// stories: prod's shop `web` deployment and staging's apps `web` deployment.
// A cluster whose name is not mapped simply does not breathe (the loop skips
// it) rather than fabricating an object.
func breathTargetsFor(servers []*fakekube.Server, names []string) []breathTarget {
	type spec struct {
		namespace string
		name      string
		app       string
	}
	byCluster := map[string]spec{
		"prod":    {namespace: "shop", name: "web-7c9-aaa", app: "web"},
		"staging": {namespace: "apps", name: "web-aa1-x", app: "web"},
	}
	var targets []breathTarget
	for i, srv := range servers {
		if i >= len(names) {
			break
		}
		s, ok := byCluster[names[i]]
		if !ok {
			continue
		}
		targets = append(targets, breathTarget{
			server:    srv,
			listPath:  "/api/v1/namespaces/" + s.namespace + "/pods",
			name:      s.name,
			namespace: s.namespace,
			app:       s.app,
		})
	}
	return targets
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

// pulse emits one MODIFIED event per target, alternating the Restarts cell up
// and back down so the churn is a gentle two-state breath. The event mutates an
// existing seeded pod (matched by name+namespace), so no dangling reference is
// introduced; DelayMs == 0 makes the change visible to the next LIST and to
// open watches synchronously.
func (d *BreathingDriver) pulse() {
	d.mu.Lock()
	d.beat++
	restarts := d.beat % 2 // 0,1,0,1,... — a quiet breath
	targets := d.targets
	d.mu.Unlock()

	for _, t := range targets {
		ev := fakekube.ScriptEvent{
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
		// Apply errors only on a malformed event or unknown path; the targets
		// are derived from the seeded scenario, so a breathing pulse is
		// best-effort and never fails the demo if a path drifts.
		_ = t.server.Apply(ev)
	}
}
