package web

// handlers_stream.go is the server half of Live mode: the read-only
// `GET …/{plural}/_stream` SSE endpoint. It keeps one UNFILTERED per-cluster
// Table snapshot in memory, feeds it from a Table watch
// (kube.WatchTable), and pushes re-renders of the SAME `_table` partial as
// `event: ro-table` frames — `f`/`sort`/columns apply at render time, never
// to the snapshot, so an object that starts (or stops) matching the active
// filter appears (or disappears) on the next push.
//
// The lifecycle is complete by contract: clean watch EOF / non-410 errors
// re-watch from the last seen resourceVersion with capped backoff (an EOF
// storm terminates instead of spinning); 410 relists silently and pushes the
// fresh table; auth expiry, the idle cap, a re-watch failure, and server
// shutdown all emit `event: ro-terminal` with a reason before closing. New
// streams beyond the cap get 429 BEFORE any SSE headers; watch-less kinds get
// 204 (the client falls back to polling). Cleanup is part of the contract:
// the watch reader goroutine and every timer are bound to the request
// context, upstream watch bodies close on every attempt end, and the cap slot
// releases on every handler exit path (deferred at acquisition).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

// Stream tuning for Live mode. The values are pinned by design — they live as package
// vars only so tests can compress time (the idle cap is explicitly
// test-injectable per the design; the defaults are asserted by
// TestStreamBackoffSchedule and never configurable at runtime).
var (
	// streamIdleCap terminates a stream that saw no watch data for this long
	// (`ro-terminal` reason "idle"). 30 minutes, hardcoded; test-injectable.
	streamIdleCap = 30 * time.Minute

	// streamBackoffBase/streamBackoffCap shape the re-watch backoff schedule:
	// base, doubling per attempt, capped (250ms → 500ms → 1s → … → 10s).
	streamBackoffBase = 250 * time.Millisecond
	streamBackoffCap  = 10 * time.Second

	// streamHealthyReset: a watch attempt that lived this long resets the
	// backoff schedule to its base.
	streamHealthyReset = time.Minute

	// streamImmediateWindow: a watch attempt that ends faster than this
	// without delivering a single event counts as an "immediate EOF" toward
	// the storm terminal.
	streamImmediateWindow = time.Second

	// streamMetricsPoll is the ?join=metrics usage sub-poll interval.
	streamMetricsPoll = 30 * time.Second

	// streamMaxLifetime bounds a stream's TOTAL lifetime in trusted-headers /
	// none auth modes (`ro-terminal` reason "idle"): there is no per-session
	// expiry in those modes, and the idle cap resets on every watch event, so
	// without this bound a stream lives forever. In OIDC mode the session's
	// own expiry bounds the stream instead (reason "auth") — the connect-time
	// cookie check is the only auth check an SSE stream ever gets, and the
	// stream must not outlive the session that authorized it. 12 hours,
	// hardcoded; test-injectable.
	streamMaxLifetime = 12 * time.Hour

	// streamWriteTimeout is the per-frame write deadline: a connected peer
	// that stopped READING would otherwise wedge Fprintf/Flush forever once
	// TCP buffers fill — the handler never returns to its select loop, no
	// timer can fire, and the deferred cap slot leaks until restart. A
	// deadline error is treated as client-gone (the normal exit path). 30
	// seconds, hardcoded; test-injectable.
	streamWriteTimeout = 30 * time.Second
)

const (
	// streamCapMax bounds concurrent Live streams; the next stream beyond it
	// gets 429 before SSE headers. Hardcoded by design — no config knob.
	streamCapMax = 32

	// streamMaxImmediateEOFs consecutive immediate EOFs are a re-watch
	// failure (`ro-terminal` reason "watch-failed") — an EOF storm must not
	// spin re-watch attempts forever.
	streamMaxImmediateEOFs = 5

	// streamMinPushGap / streamMaxPushLatency are the pacing bounds: pushes
	// are at least 300ms apart, and while events pend a push happens at most
	// 2s after the previous one.
	streamMinPushGap     = 300 * time.Millisecond
	streamMaxPushLatency = 2 * time.Second

	// High-churn detection: at least streamChurnEvents data events inside the
	// trailing streamChurnWindow (>~5 events/s sustained) degrades pushes to
	// the fixed streamMaxPushLatency interval — the apiserver-side cost
	// argument does not cover readout's own render/transfer/morph cost.
	streamChurnWindow = 2 * time.Second
	streamChurnEvents = 10
)

// streamTablePayload is the pinned `event: ro-table` data frame: the
// client-minted generation echoed verbatim plus the rendered `_table`
// partial. JSON encoding keeps the html on one line (control characters are
// escaped), so a single SSE `data:` line carries the whole frame.
type streamTablePayload struct {
	G    string `json:"g"`
	HTML string `json:"html"`
}

// streamTerminalPayload is the pinned `event: ro-terminal` frame: the echoed
// generation plus the close reason — "idle", "auth", "watch-failed" or
// "shutdown". The client closes without reconnecting and drops to polling.
type streamTerminalPayload struct {
	G      string `json:"g"`
	Reason string `json:"reason"`
}

// streamBackoff is the re-watch delay schedule: streamBackoffBase doubling
// per attempt up to streamBackoffCap. noteAttempt resets the schedule after a
// healthy attempt (one that lived at least streamHealthyReset).
type streamBackoff struct {
	attempt int
}

// next returns the delay before the upcoming re-watch attempt and advances
// the schedule.
func (b *streamBackoff) next() time.Duration {
	d := streamBackoffBase
	for i := 0; i < b.attempt && d < streamBackoffCap; i++ {
		d *= 2
	}
	if d > streamBackoffCap {
		d = streamBackoffCap
	}
	if b.attempt < 63 {
		b.attempt++
	}
	return d
}

// noteAttempt records a finished watch attempt's lifetime: a healthy attempt
// resets the schedule so the next re-watch waits only the base delay again.
func (b *streamBackoff) noteAttempt(lived time.Duration) {
	if lived >= streamHealthyReset {
		b.attempt = 0
	}
}

// watchResult is one delivery from the watch reader goroutine: a decoded
// event, or the error that ended the attempt (io.EOF for a clean upstream
// close — the error taxonomy is kube.TableWatch's).
type watchResult struct {
	ev  kube.WatchEvent
	err error
}

// watchReader pumps TableWatch.Next into out until the attempt ends. It is
// bound to the request context twice over: a canceled request closes the
// watch body (unblocking Next), and the send select frees the goroutine if
// the session stopped draining.
func watchReader(ctx context.Context, w *kube.TableWatch, out chan<- watchResult) {
	for {
		ev, err := w.Next()
		select {
		case out <- watchResult{ev: ev, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// resourceStream serves `GET …/{plural}/_stream` for Live mode. Order is load-
// bearing: the scope/namespace checks are free and run first; the cap slot is
// acquired before any upstream work and before SSE headers (a cap-exceeded
// stream 429s without ever connecting); discovery then classifies watch-less
// kinds (204). Only after the initial list succeeds do the SSE headers go
// out — every failure before that point is a plain HTTP status, every
// failure after it is an in-stream `ro-terminal`.
func (s *Server) resourceStream(w http.ResponseWriter, r *http.Request) {
	clusterName := r.PathValue("cluster")
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	// Live scope cut: Live covers single-type AND single-cluster lists only.
	// Multi-type pages (plural "all"/"_all"/CSV) and multi-cluster scope
	// (cluster "_all"/CSV) get 404 — the dropdown renders the option disabled.
	if !isSingleListType(plural) || clusterName == kube.AllClusters || strings.Contains(clusterName, ",") {
		http.Error(w, "live streams cover single-type, single-cluster lists only", http.StatusNotFound)
		return
	}
	if namespace != "" && namespace != kube.AllNamespaces && !s.namespaceAllowed(namespace) {
		http.Error(w, "namespace is not allowed", http.StatusForbidden)
		return
	}
	cluster, ok := s.manager.Get(clusterName)
	if !ok {
		http.Error(w, "cluster not found", http.StatusNotFound)
		return
	}
	// Stream cap: acquire before SSE headers and any upstream call; release
	// on EVERY exit path. The deferred receive is the single release point —
	// 204/initial-list-failure/terminal/client-gone all pass through it.
	select {
	case s.streamSlots <- struct{}{}:
	default:
		http.Error(w, "too many live streams", http.StatusTooManyRequests)
		return
	}
	defer func() { <-s.streamSlots }()

	ctx := r.Context()
	client := s.kubeClient(r, cluster)
	rt, err := client.FindResource(ctx, plural, namespace != "", apiVersionParam(r))
	if err != nil {
		http.Error(w, "resource type not found", http.StatusNotFound)
		return
	}
	// Watch-less kinds (no watch verb — componentstatuses, the metrics
	// pseudo-types) cannot stream: 204 tells the client to fall back to
	// polling silently.
	if !slices.Contains(rt.Verbs, "watch") {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	listNS := namespace
	if namespace == kube.AllNamespaces {
		listNS = ""
	}
	lifetime, lifetimeReason := s.streamLifetime(r)
	sess := &streamSession{
		srv:            s,
		w:              w,
		rc:             http.NewResponseController(w),
		renderReq:      streamRenderRequest(r),
		client:         client,
		rt:             rt,
		cluster:        clusterName,
		listNS:         listNS,
		selector:       r.URL.Query().Get("selector"),
		gen:            r.URL.Query().Get("g"),
		wantMetrics:    r.URL.Query().Get("join") == "metrics" && (plural == "pods" || plural == "nodes"),
		lifetime:       lifetime,
		lifetimeReason: lifetimeReason,
	}
	sess.run(ctx)
}

// streamLifetime resolves the stream's total-lifetime bound at connect time
// (the only auth check an SSE stream ever gets — the idle cap resets on watch
// data, so without this a revoked/expired session keeps receiving cluster
// state indefinitely). OIDC mode: the session cookie's own Expires, terminal
// reason "auth" (the client's no-reconnect taxonomy). Trusted-headers / none
// modes have no per-session expiry: the hard streamMaxLifetime cap applies,
// terminal reason "idle".
func (s *Server) streamLifetime(r *http.Request) (time.Duration, string) {
	if s.auth.EffectiveAuthMode() == config.AuthModeOIDC {
		if session, ok := s.auth.Session(r); ok {
			return time.Until(time.Unix(session.Expires, 0)), "auth"
		}
	}
	return streamMaxLifetime, "idle"
}

// streamSession is one open Live stream: the unfiltered snapshot, the cached
// metrics overlay, and the pacing state. All fields are owned by the handler
// goroutine — the only other goroutine (the watch reader) communicates
// exclusively over its channel.
type streamSession struct {
	srv       *Server
	w         http.ResponseWriter
	rc        *http.ResponseController
	renderReq *http.Request
	client    *kube.Client
	rt        kube.ResourceType
	cluster   string
	listNS    string
	selector  string
	gen       string

	// snapshot is the per-cluster UNFILTERED Table for the stream's scope
	// (namespace + label selector — apiserver-level params). The readout-side
	// `f`/`filter`/`sort` params apply at render time on a clone, never here,
	// so filter-transition pushes work by construction.
	snapshot kube.Table
	// lastRV is the last seen resourceVersion — the re-watch point after a
	// clean EOF (and the replay floor, so already-seen events never repeat).
	lastRV string

	wantMetrics bool
	metrics     map[string][2]float64

	// lifetime / lifetimeReason bound the stream's TOTAL lifetime (resolved
	// at connect by streamLifetime; the loop arms a single never-reset timer).
	lifetime       time.Duration
	lifetimeReason string

	dirty      bool
	lastPush   time.Time
	eventTimes []time.Time
}

// streamHandshakeStatus maps an initial-list failure to the plain HTTP status
// the handshake fails with: a 403 for a forbidden/unauthorized denial, a 404 for
// a missing resource, and a 502 for everything else (the cluster could not serve
// the snapshot). It classifies once through the shared classifier and maps the
// kind. The stream never half-connects, so this is the whole response.
func streamHandshakeStatus(err error) int {
	return failureHandshakeStatus(kube.ClassifyError(err))
}

// run fetches the initial snapshot, completes the SSE handshake with the
// initial full push, and hands off to the event loop. A failure before the
// handshake stays a plain HTTP status — the stream never half-connects.
func (st *streamSession) run(ctx context.Context) {
	table, err := st.list(ctx)
	if err != nil {
		http.Error(st.w, "initial list failed", streamHandshakeStatus(err))
		return
	}
	st.snapshot = table
	st.lastRV = table.ResourceVersion
	if st.wantMetrics {
		st.metrics = st.fetchMetrics(ctx)
	}

	h := st.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	st.w.WriteHeader(http.StatusOK)
	if err := st.push(ctx); err != nil {
		return
	}
	st.loop(ctx)
}

// list fetches the stream's pristine scope Table: namespace + label selector
// apply (apiserver-level), readout-side filters and sort do NOT — the
// snapshot stays unfiltered by contract.
func (st *streamSession) list(ctx context.Context) (kube.Table, error) {
	return st.client.Table(ctx, &st.rt, kube.ListOptions{Namespace: st.listNS, LabelSelector: st.selector})
}

// fetchMetrics wraps the shared usage fetch, normalizing a failed fetch to an
// empty non-nil map so renders never fall back to a live per-push fetch
// inside applyTableOptionsWithUsage (nil there means "fetch now").
func (st *streamSession) fetchMetrics(ctx context.Context) map[string][2]float64 {
	usage := st.srv.fetchMetricsUsage(ctx, st.client, st.rt.Namespaced, st.listNS, false, st.selector)
	if usage == nil {
		usage = map[string][2]float64{}
	}
	return usage
}

// loop is the stream's single event loop: watch lifecycle (connect, re-watch
// with backoff, relist on 410, terminal taxonomy), push pacing, the metrics
// sub-poll, and the idle cap all live in one select so no state needs locks.
func (st *streamSession) loop(ctx context.Context) {
	idleTimer := time.NewTimer(streamIdleCap)
	defer idleTimer.Stop()
	// The total-lifetime bound (session expiry in OIDC mode, the hard cap
	// otherwise). NEVER reset — unlike the idle timer, watch data must not
	// extend it.
	lifetimeTimer := time.NewTimer(st.lifetime)
	defer lifetimeTimer.Stop()
	pushTimer := time.NewTimer(time.Hour)
	pushTimer.Stop()
	defer pushTimer.Stop()
	// The zero-delay first fire connects the initial watch through the same
	// path every re-watch takes.
	rewatchTimer := time.NewTimer(0)
	defer rewatchTimer.Stop()
	var metricsCh <-chan time.Time
	if st.wantMetrics {
		ticker := time.NewTicker(streamMetricsPoll)
		defer ticker.Stop()
		metricsCh = ticker.C
	}

	var (
		cur             *kube.TableWatch
		events          chan watchResult
		attemptStart    time.Time
		attemptSawEvent bool
		backoff         streamBackoff
		immediateEOFs   int
	)
	defer func() {
		if cur != nil {
			_ = cur.Close()
		}
	}()

	// endAttempt classifies a finished watch attempt: 410 relists and
	// re-watches immediately; upstream 401/403 is terminal "auth"; everything
	// else (clean EOF included) re-watches from lastRV with backoff — unless
	// it is the streamMaxImmediateEOFs-th consecutive immediate end, which is
	// the re-watch failure terminal. Returns false when the stream must end.
	endAttempt := func(err error) bool {
		if cur != nil {
			_ = cur.Close()
			cur = nil
		}
		events = nil
		lived := time.Since(attemptStart)
		switch {
		case errors.Is(err, kube.ErrWatchGone):
			// 410: the RV fell out of the apiserver history window. Silent
			// relist + full push, then re-watch from the fresh RV at once —
			// a resync, not a failure, so the failure counters reset.
			if !st.relist(ctx) {
				st.terminal("watch-failed")
				return false
			}
			backoff = streamBackoff{}
			immediateEOFs = 0
			st.schedulePush(pushTimer)
			rewatchTimer.Reset(0)
			return true
		case kube.IsForbidden(err):
			// Upstream 401/403 — e.g. session token expiry in passthrough
			// mode. The stream cannot recover by retrying.
			st.terminal("auth")
			return false
		}
		if !attemptSawEvent && lived < streamImmediateWindow {
			immediateEOFs++
			if immediateEOFs >= streamMaxImmediateEOFs {
				st.terminal("watch-failed")
				return false
			}
		} else {
			immediateEOFs = 0
		}
		backoff.noteAttempt(lived)
		rewatchTimer.Reset(backoff.next())
		return true
	}

	for {
		select {
		case <-ctx.Done():
			// The client went away (or the request ended): nobody is left to
			// write a terminal to. Deferred cleanup releases everything.
			return
		case <-st.srv.shutdownCh:
			st.terminal("shutdown")
			return
		case <-rewatchTimer.C:
			w, err := st.client.WatchTable(ctx, &st.rt, kube.WatchOptions{
				Namespace:       st.listNS,
				LabelSelector:   st.selector,
				ResourceVersion: st.lastRV,
			})
			attemptStart = time.Now()
			attemptSawEvent = false
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if !endAttempt(err) {
					return
				}
				continue
			}
			cur = w
			events = make(chan watchResult)
			go watchReader(ctx, w, events)
		case res := <-events:
			if res.err != nil {
				if ctx.Err() != nil {
					return
				}
				if !endAttempt(res.err) {
					return
				}
				continue
			}
			attemptSawEvent = true
			immediateEOFs = 0
			if res.ev.ResourceVersion != "" {
				st.lastRV = res.ev.ResourceVersion
			}
			if res.ev.Type == kube.WatchBookmark {
				// Bookmarks advance the re-watch point only; their rows are
				// NEVER read (the real apiserver may attach one).
				continue
			}
			mergeTableEvent(&st.snapshot, &res.ev)
			st.dirty = true
			st.noteEvent(time.Now())
			idleTimer.Reset(streamIdleCap)
			st.schedulePush(pushTimer)
		case <-pushTimer.C:
			if st.dirty {
				if err := st.push(ctx); err != nil {
					return
				}
			}
		case <-metricsCh:
			usage := st.fetchMetrics(ctx)
			if !maps.Equal(usage, st.metrics) {
				st.metrics = usage
				st.dirty = true
				st.schedulePush(pushTimer)
			}
		case <-idleTimer.C:
			st.terminal("idle")
			return
		case <-lifetimeTimer.C:
			st.terminal(st.lifetimeReason)
			return
		}
	}
}

// relist refreshes the snapshot after a 410 and marks a full push pending.
func (st *streamSession) relist(ctx context.Context) bool {
	table, err := st.list(ctx)
	if err != nil {
		return false
	}
	st.snapshot = table
	st.lastRV = table.ResourceVersion
	st.dirty = true
	return true
}

// noteEvent records a data-event arrival for churn detection and prunes the
// trailing window.
func (st *streamSession) noteEvent(now time.Time) {
	cutoff := now.Add(-streamChurnWindow)
	keep := st.eventTimes[:0]
	for _, t := range st.eventTimes {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	st.eventTimes = keep
}

// highChurn reports sustained churn: at least streamChurnEvents data events
// inside the trailing streamChurnWindow (>~5 events/s sustained).
func (st *streamSession) highChurn(now time.Time) bool {
	cutoff := now.Add(-streamChurnWindow)
	n := 0
	for _, t := range st.eventTimes {
		if t.After(cutoff) {
			n++
		}
	}
	return n >= streamChurnEvents
}

// schedulePush arms the push timer for the pending changes: at least
// streamMinPushGap after the previous push (immediately once that gap has
// passed), degraded to the fixed streamMaxPushLatency interval under
// sustained churn — so while events pend, a push is never further than
// streamMaxPushLatency from the previous one and never closer than
// streamMinPushGap.
func (st *streamSession) schedulePush(timer *time.Timer) {
	if !st.dirty {
		return
	}
	now := time.Now()
	target := st.lastPush.Add(streamMinPushGap)
	if st.highChurn(now) {
		target = st.lastPush.Add(streamMaxPushLatency)
	}
	if target.Before(now) {
		target = now
	}
	timer.Reset(target.Sub(now))
}

// push renders the current snapshot through the `_table` partial pipeline and
// writes one `ro-table` frame. The write error is the caller's signal that
// the client is gone.
func (st *streamSession) push(ctx context.Context) error {
	clone := cloneTableForRender(&st.snapshot)
	lc := st.srv.streamListContext(st.renderReq, st.client, st.cluster, &clone, st.metrics)
	view := st.srv.buildListView(st.renderReq, &lc)
	var buf bytes.Buffer
	if err := templates.ResourceTable(toListData(&view)).Render(ctx, &buf); err != nil {
		return err
	}
	st.dirty = false
	st.lastPush = time.Now()
	return st.writeEvent("ro-table", streamTablePayload{G: st.gen, HTML: buf.String()})
}

// terminal writes the named `ro-terminal` frame. Write errors are ignored —
// the stream is closing either way.
func (st *streamSession) terminal(reason string) {
	_ = st.writeEvent("ro-terminal", streamTerminalPayload{G: st.gen, Reason: reason})
}

// writeEvent writes one SSE frame and flushes it — per-message flush is part
// of the Live stream plumbing (statusWriter forwards Flush; the anti-buffering header
// set at the handshake keeps proxies honest). Every frame is bounded by a
// write deadline (via statusWriter's Unwrap → http.ResponseController): a
// connected-but-not-reading peer otherwise blocks the write forever once TCP
// buffers fill, wedging the handler outside its select loop with the cap slot
// held. A deadline error surfaces as the write/flush error — the normal
// client-gone exit. The deadline disarms after a successful frame (pushes can
// be arbitrarily far apart, and the next frame re-arms it anyway); deadline
// (dis)arming itself is best-effort — an unsupported writer just keeps the
// old unbounded behavior.
func (st *streamSession) writeEvent(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_ = st.rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if _, err := fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	if err := st.rc.Flush(); err != nil {
		return err
	}
	_ = st.rc.SetWriteDeadline(time.Time{})
	return nil
}

// mergeTableEvent folds one watch data event into the unfiltered snapshot:
// ADDED/MODIFIED upsert the row by object identity (namespace/name), DELETED
// removes it. Watch frames carry columnDefinitions only in the stream's
// first event; the snapshot keeps the initial list's columns and adopts event
// columns only if the list somehow had none — cells align either way because
// both come from the same printer.
func mergeTableEvent(snapshot *kube.Table, ev *kube.WatchEvent) {
	if len(snapshot.Columns) == 0 && len(ev.Table.Columns) > 0 {
		snapshot.Columns = ev.Table.Columns
	}
	for _, row := range ev.Table.Rows {
		name := nestedString(row.Object, "metadata", "name")
		if name == "" {
			continue
		}
		ns := nestedString(row.Object, "metadata", "namespace")
		idx := -1
		for i := range snapshot.Rows {
			obj := snapshot.Rows[i].Object
			if nestedString(obj, "metadata", "name") == name && nestedString(obj, "metadata", "namespace") == ns {
				idx = i
				break
			}
		}
		switch ev.Type {
		case kube.WatchDeleted:
			if idx >= 0 {
				snapshot.Rows = append(snapshot.Rows[:idx], snapshot.Rows[idx+1:]...)
			}
		default: // ADDED / MODIFIED
			if idx >= 0 {
				snapshot.Rows[idx] = row
			} else {
				snapshot.Rows = append(snapshot.Rows, row)
			}
		}
	}
}

// cloneTableForRender deep-copies the snapshot's table STRUCTURE (columns,
// rows, cells slices) so the render pipeline's mutations — decorations,
// hidecols removal, filters, sort — never touch the live snapshot. Row
// objects are shared by reference: the render path reads them without
// mutating, and the merge loop replaces objects wholesale rather than editing
// in place, so a pushed frame can never see a half-merged object.
func cloneTableForRender(t *kube.Table) kube.Table {
	clone := *t
	clone.Columns = append([]kube.Column(nil), t.Columns...)
	clone.Clusters = append([]string(nil), t.Clusters...)
	clone.Rows = make([]kube.Row, len(t.Rows))
	for i := range t.Rows {
		clone.Rows[i] = kube.Row{
			Cells:   append([]any(nil), t.Rows[i].Cells...),
			Object:  t.Rows[i].Object,
			Cluster: t.Rows[i].Cluster,
		}
	}
	return clone
}

// streamRenderRequest derives the render-path request: the canonical LIST
// page URL (path minus `/_stream`) without the stream-only `g` param, so
// every href buildListView resolves matches what a `_table` partial bakes —
// byte-identical fragments morph cleanly client-side. The shallow request
// copy keeps the context and the mux path values (the same pattern
// buildListView's canonicalization uses); `g` is stripped RAW because an `f`
// chip's OR-comma is raw on the wire and a url.Values round-trip would
// re-encode it (see filter.go).
func streamRenderRequest(r *http.Request) *http.Request {
	clone := *r
	u := *r.URL
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/_stream")
	u.RawQuery = stripRawQueryParam(u.RawQuery, "g")
	clone.URL = &u
	return &clone
}

// stripRawQueryParam removes every `key=` pair from a RAW query string
// without decoding or re-encoding the surviving pairs.
func stripRawQueryParam(rawQuery, key string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := strings.Split(rawQuery, "&")
	kept := pairs[:0]
	for _, pair := range pairs {
		k, _, _ := strings.Cut(pair, "=")
		if k != key {
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, "&")
}
