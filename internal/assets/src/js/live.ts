// live.ts -- the Live v2 browser transport and its reconnect state machine.
//
// There is exactly ONE automatic update path: this SSE stream. Nothing here
// falls back to polling `/_table` -- when the stream cannot be held, the rows
// become last-known and the transport retries the stream itself (or stops and
// says so). The eight states are the whole vocabulary:
//
//   off          nothing armed, no request, no warning;
//   connecting   a stream request is out, no snapshot committed yet;
//   open         a full snapshot is committed -- the only state that is "Live";
//   reconnecting a retry is armed on the jittered ladder (live-policy.ts);
//   suspended    a user list request owns the page; reopen after it settles;
//   hidden       the tab is hidden; reopen once on visibilitychange;
//   offline      the browser is offline; retries pause until `online`;
//   unavailable  terminal (401/403, the `auth` terminal, a 204/404 gate, or an
//                exhausted protocol resync budget) -- the Reload banner, no retry.
//
// Only a committed full snapshot enters `open` and clears stale state: response
// headers and an accepted body are not enough.
import { clearListValidator } from './list-etag.js';
import { reconnectDelayMs, retryAfterMs, shouldResetBackoff } from './live-policy.js';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    type LiveV2Cursor,
    type LiveV2SnapshotEnvelope,
} from './live-protocol.js';
import { type LiveSSEEvent, LiveSSEParser } from './live-sse.js';
import { liveStreamBaseForURL, mintLiveGeneration } from './live-url.js';
import {
    isLiveEnabled,
    type ListRequestActivity,
    listRequestTrackerSnapshot,
    resetListRequestTracker,
    subscribeListRequests,
} from './refresh.js';
import {
    clearLiveStale,
    markLiveStale,
    markLiveUnavailable,
    noteStaleRetryAt,
    revealLiveStale,
} from './stale.js';

interface LiveSnapshotEventInfo {
    target: Element;
    roLivePush: true;
    roLiveSnapshotTxn: object;
}

interface Htmx {
    swap(
        target: Element,
        content: string,
        swapSpec: { swapStyle: string },
        swapOptions: { contextElement: Element; eventInfo: LiveSnapshotEventInfo },
    ): void;
}
export type LiveStatus =
    | 'off'
    | 'connecting'
    | 'open'
    | 'reconnecting'
    | 'suspended'
    | 'hidden'
    | 'offline'
    | 'unavailable';
interface LiveConnection {
    readonly ctrl: AbortController;
    readonly generation: string;
    readonly base: string;
    readonly cursor: Readonly<LiveV2Cursor> | null;
}
const runtime: {
    status: LiveStatus;
    connection: LiveConnection | null;
    // Assigned by openConnection before every non-Off lifecycle state.
    streamPath?: string;
} = {
    status: 'off',
    connection: null,
};
const counters = {
    connections: 0,
    resyncs: 0,
    reconnects: 0,
    v2Snapshots: 0,
    deltas: 0,
    terminals: 0,
    invalidFrames: 0,
    rawBytes: 0,
    payloadBytes: 0,
    snapshotBytes: 0,
    deltaBytes: 0,
    inserted: 0,
    updated: 0,
    deleted: 0,
    projected: 0,
};
export type LiveDebugStats = Readonly<typeof counters> & {
    state: LiveStatus;
    seq: number;
    attempt: number;
    inFlightRequests: number;
    resyncsInWindow: number;
};
type CounterName = keyof typeof counters;
const RESYNC_WINDOW_MS = 30_000;
const MAX_RESYNCS_PER_WINDOW = 2;
export const LIVE_FIRST_FRAME_TIMEOUT_MS = 30_000;
// After the first committed frame the deadline becomes an IDLE budget. The
// server heartbeats every 20s, so silence past this is a dead transport, not a
// quiet namespace -- and a blackholed connection (half-open TCP: no FIN, no
// RST) would otherwise sit in `open` forever with frozen rows.
export const LIVE_READ_IDLE_TIMEOUT_MS = 50_000;

const completedSnapshotTxns = new WeakSet<object>();
let resyncTimestamps: number[] = [];
let resumeIntent: { base: string } | null = null;
let requestSubscribed = false;
let reconnectTimerId: number | undefined;
// The rung of the reconnect ladder the next failure will draw from, and the
// epoch ms at which the current connection first committed a snapshot (0 = it
// never did). Together they decide whether a drop restarts the ladder.
let reconnectAttempt = 0;
let snapshotAt = 0;
function addCounter(name: CounterName, amount = 1): void {
    counters[name] += amount;
}
function pruneResyncWindow(now = Date.now()): void {
    resyncTimestamps = resyncTimestamps.filter((timestamp) => now - timestamp < RESYNC_WINDOW_MS);
}

function currentStats(): LiveDebugStats {
    pruneResyncWindow();
    return {
        ...counters,
        state: runtime.status,
        seq: runtime.connection?.cursor?.seq || 0,
        attempt: reconnectAttempt,
        inFlightRequests: listRequestTrackerSnapshot().count,
        resyncsInWindow: resyncTimestamps.length,
    };
}

// liveCanStreamHere is the ONE answer to "would turning Live on do anything on
// this page?". The stored preference is global; whether the stream applies is a
// property of the page, and callers that confuse the two silently do nothing
// (a multi-cluster or watchless list keeps the cookie but has no `_stream`).
export function liveCanStreamHere(): boolean {
    const content = document.getElementById('resource-list-content') as HTMLElement | null;
    if (content?.dataset.liveUrl !== 'location') return false;
    // The server renders the toggle only where `_stream` answers; a page that
    // cannot stream omits it entirely rather than rendering it disabled.
    return document.querySelector('[data-ro-action="toggle-live"]') !== null;
}

function liveStreamBase(): string {
    return liveStreamBaseForURL(new URL(window.location.href));
}

// --- the toggle's transport paint -------------------------------------------
//
// The toggle carries TWO independent readings and they must not be conflated.
// `aria-pressed` is the stored PREFERENCE (the server renders it at SSR from
// the cookie, refresh.ts re-derives it on every click) -- it answers "did the
// user ask for Live?". `data-ro-live-state` is what the TRANSPORT actually
// holds, and it is the only thing the green pulsing dot may hang off: green is
// a live-health signal, so it must be impossible before a committed full
// snapshot. A stream that is retrying or has stopped paints `problem`; every
// other state (connecting, and the deliberate hidden/suspended pauses) stays
// the neutral ghost dot, which is also what an absent attribute renders as --
// so the SSR markup, which carries no transport reading at all, is never green.
type LiveToggleState = 'open' | 'problem' | 'connecting';

function liveToggleState(status: LiveStatus): LiveToggleState {
    switch (status) {
        case 'open':
            return 'open';
        case 'reconnecting':
        case 'offline':
        case 'unavailable':
            return 'problem';
        default:
            return 'connecting';
    }
}

// paintLiveToggleState repaints the current transport reading onto whatever
// toggle is in the DOM now. Transitions paint themselves; this is for the other
// direction -- a boosted body swap installs a fresh, SSR-painted button while
// the transport state machine carries on untouched, so refresh.ts repaints it
// alongside aria-pressed rather than leaving the new button unstyled.
export function paintLiveToggleState(): void {
    paintLiveToggle(runtime.status);
}

function paintLiveToggle(status: LiveStatus): void {
    const toggle = document.querySelector('[data-ro-action="toggle-live"]');
    if (!toggle) return;
    if (status === 'off') {
        toggle.removeAttribute('data-ro-live-state');
        return;
    }
    toggle.setAttribute('data-ro-live-state', liveToggleState(status));
}

// setStatus is the ONE writer of runtime.status, so no transition can reach the
// state machine without also reaching the chrome.
function setStatus(next: LiveStatus): void {
    runtime.status = next;
    paintLiveToggle(next);
}
function isActive(connection: LiveConnection): boolean {
    return runtime.connection === connection;
}

function connectionToken(source: LiveConnection): LiveConnection {
    return Object.freeze({
        ...source,
        cursor: source.cursor ? Object.freeze({ ...source.cursor }) : null,
    });
}

function replaceConnection(
    current: LiveConnection,
    cursor: LiveV2Cursor | Readonly<LiveV2Cursor> | null,
): LiveConnection | null {
    if (!isActive(current)) return null;
    const next = connectionToken({ ...current, cursor });
    runtime.connection = next;
    return next;
}

function abortActiveConnection(): void {
    const connection = runtime.connection;
    runtime.connection = null;
    connection?.ctrl.abort();
}
function clearReconnectTimer(): void {
    window.clearTimeout(reconnectTimerId);
    reconnectTimerId = undefined;
}

// liveSetOff tears the transport down from ANY state: abort the stream, cancel
// the armed retry, drop the resume intent, and clear the Live warning surface.
// It issues no request -- turning Live off is silent.
export function liveSetOff(): void {
    abortActiveConnection();
    clearReconnectTimer();
    resumeIntent = null;
    reconnectAttempt = 0;
    snapshotAt = 0;
    setStatus('off');
    noteStaleRetryAt(0);
    clearLiveStale();
}

export function liveResetPage(): void {
    liveSetOff();
    resetListRequestTracker();
    resyncTimestamps = [];
}

// enterUnavailable is the terminal stop: the server told us this session cannot
// stream (or the client cannot mint an identity for it). No timer is armed and
// no request is made -- the banner's Reload is the only way forward.
function enterUnavailable(): void {
    abortActiveConnection();
    clearReconnectTimer();
    resumeIntent = null;
    setStatus('unavailable');
    noteStaleRetryAt(0);
    markLiveUnavailable();
}

// enterDeferred parks the transport in a state that owns its own wake-up
// (visibility, request settlement, or `online`). The stream is closed but the
// page is not stale-by-failure, so no warning is raised.
function enterDeferred(status: 'hidden' | 'suspended' | 'offline', base: string): void {
    resumeIntent = { base };
    setStatus(status);
    noteStaleRetryAt(0);
}

// noteDisconnected publishes the loss of a stream: the projection is last-known
// from this instant (semantic stale + a dropped ETag validator, so the next
// `_table` request cannot be answered 304 against data we no longer trust). The
// visible dim waits out the grace, EXCEPT after a retry has already failed --
// that proves the drop is not a rollout blip.
function noteDisconnected(): void {
    clearListValidator();
    markLiveStale();
    if (reconnectAttempt >= 1) revealLiveStale();
}

// scheduleReconnect owns every recoverable failure: fetch rejection, a non-200
// reply, a dead reader, the first-frame deadline, and the non-auth terminals.
// `delayMs` carries a server-dictated Retry-After; null falls back to the
// jittered ladder.
function scheduleReconnect(base: string, delayMs: number | null = null): void {
    abortActiveConnection();
    clearReconnectTimer();
    if (!isLiveEnabled()) {
        liveSetOff();
        return;
    }
    // A connection that held a committed snapshot through the healthy window
    // earns a fresh ladder; a stream that never stabilized keeps climbing.
    if (shouldResetBackoff(snapshotAt, Date.now())) reconnectAttempt = 0;
    noteDisconnected();
    if (!window.navigator.onLine) {
        enterDeferred('offline', base);
        return;
    }
    reconnectAttempt += 1;
    const delay = delayMs ?? reconnectDelayMs(reconnectAttempt);
    setStatus('reconnecting');
    addCounter('reconnects');
    noteStaleRetryAt(Date.now() + delay);
    reconnectTimerId = window.setTimeout(() => {
        reconnectTimerId = undefined;
        if (!isLiveEnabled()) {
            liveSetOff();
            return;
        }
        // Re-derive the target: the page may have moved while the retry waited.
        openConnection(liveCanStreamHere() ? liveStreamBase() : '');
    }, delay);
}

function openConnection(base: string): void {
    abortActiveConnection();
    clearReconnectTimer();
    runtime.streamPath = base;
    snapshotAt = 0;
    if (!base) {
        // This page cannot stream (a detail page, a watchless or multi-scope
        // list). The stored preference stays; Live simply does not apply here.
        liveSetOff();
        return;
    }
    if (document.hidden) {
        enterDeferred('hidden', base);
        return;
    }
    if (listRequestTrackerSnapshot().count > 0) {
        enterDeferred('suspended', base);
        return;
    }
    if (!window.navigator.onLine) {
        enterDeferred('offline', base);
        return;
    }
    let generation: string;
    try {
        generation = mintLiveGeneration();
    } catch {
        enterUnavailable();
        return;
    }
    const ctrl = new AbortController();
    const connection = connectionToken({
        ctrl,
        generation,
        base,
        cursor: null,
    });
    runtime.connection = connection;
    setStatus('connecting');
    addCounter('connections');
    void liveConnect(connection);
}

function responseHeader(response: Response, name: string): string | null {
    try {
        return response.headers.get(name);
    } catch {
        return null;
    }
}

function acceptsV2Response(response: Response, connection: LiveConnection): boolean {
    const contentType = responseHeader(response, 'Content-Type');
    if (contentType?.split(';', 1)[0].trim().toLowerCase() !== 'text/event-stream') {
        return false;
    }
    const version = responseHeader(response, 'RO-Live-Version');
    if (version !== null && version !== '2') return false;
    const generation = responseHeader(response, 'RO-Live-Generation');
    return generation === null || generation === connection.generation;
}

async function liveConnect(initial: LiveConnection): Promise<void> {
    let deadlineTimer: number | null = null;
    const clearDeadline = () => {
        if (deadlineTimer !== null) {
            window.clearTimeout(deadlineTimer);
            deadlineTimer = null;
        }
    };
    const armDeadline = (ms: number) => {
        clearDeadline();
        deadlineTimer = window.setTimeout(() => {
            deadlineTimer = null;
            if (runtime.connection?.ctrl === initial.ctrl) {
                scheduleReconnect(initial.base);
            }
        }, ms);
    };
    armDeadline(LIVE_FIRST_FRAME_TIMEOUT_MS);
    try {
        await runLiveConnection(initial, () => armDeadline(LIVE_READ_IDLE_TIMEOUT_MS));
    } finally {
        clearDeadline();
    }
}

// acceptResponse maps the reply's status to this connection's next move and
// returns the readable body only when the stream may proceed. A 429 is an
// admission reject, so the server's own Retry-After outranks the ladder, and a
// 408 is a timeout the next attempt may beat. Every OTHER 4xx is a request this
// browser cannot fix by replaying it byte for byte -- 401/403 need a new
// session, 404 means the server does not offer this stream, 400/406 mean the
// handshake headers never arrived intact -- so they stop with the Reload banner
// instead of retrying every 30s forever. 204 is the watchless-kind answer: a
// successful "there is nothing here to stream".
function acceptResponse(
    response: Response,
    connection: LiveConnection,
): ReadableStream<Uint8Array> | null {
    const status = response.status;
    if (status === 429) {
        scheduleReconnect(connection.base, retryAfterMs(responseHeader(response, 'Retry-After')));
        return null;
    }
    if (status === 408) {
        scheduleReconnect(connection.base);
        return null;
    }
    if (status === 204 || (status >= 400 && status < 500)) {
        enterUnavailable();
        return null;
    }
    if (status !== 200 || !response.body) {
        scheduleReconnect(connection.base);
        return null;
    }
    return response.body;
}

async function runLiveConnection(
    initial: LiveConnection,
    noteLiveProgress: () => void,
): Promise<void> {
    let response: Response;
    try {
        response = await fetch(initial.base, {
            signal: initial.ctrl.signal,
            headers: {
                'RO-Live-Version': '2',
                'RO-Live-Generation': initial.generation,
            },
        });
    } catch {
        if (isActive(initial)) scheduleReconnect(initial.base);
        return;
    }
    if (!isActive(initial)) return;
    const body = acceptResponse(response, initial);
    if (!body) return;
    if (!acceptsV2Response(response, initial)) {
        rejectProtocol(initial);
        return;
    }
    const accepted = replaceConnection(initial, null);
    if (!accepted) return;
    let connection = accepted;
    try {
        const reader = body.getReader();
        const parser = new LiveSSEParser();
        // Before the first frame the first-frame deadline stands. After it,
        // ANY inbound bytes -- a `: heartbeat` comment included -- renew the
        // idle budget, which is what makes a silent transport detectable.
        let sawFrame = false;
        const readNext = async (): Promise<LiveConnection | undefined> => {
            const result = await reader.read();
            if (!isActive(connection) || result.done) return;
            if (sawFrame) noteLiveProgress();
            addCounter('rawBytes', result.value.byteLength);
            let events: LiveSSEEvent[];
            try {
                events = parser.push(result.value);
            } catch {
                addCounter('invalidFrames');
                rejectProtocol(connection, false);
                return;
            }
            for (const event of events) {
                addCounter('payloadBytes', event.dataBytes);
                handleV2Frame(connection, event.name, event.data, event.dataBytes);
                const current = runtime.connection;
                if (!current || current.ctrl !== connection.ctrl) return;
                connection = current;
                sawFrame = true;
                noteLiveProgress();
            }
            return connection;
        };
        while (await readNext()) {}
    } catch {}
    if (isActive(connection)) scheduleReconnect(connection.base);
}

function handleV2Frame(
    connection: LiveConnection,
    name: string | null,
    text: string,
    payloadBytes: number,
): void {
    if (name !== 'ro-live') {
        rejectProtocol(connection);
        return;
    }
    const decoded = decodeLiveV2Envelope(text);
    if (!decoded.ok) {
        rejectProtocol(connection);
        return;
    }
    const envelope = decoded.value;
    const cursor = connection.cursor;
    if (envelope.g !== connection.generation) {
        rejectProtocol(connection);
        return;
    }
    if (!cursor) {
        if (envelope.kind !== 'snapshot' || envelope.seq !== 1) {
            rejectProtocol(connection);
            return;
        }
        commitV2Snapshot(connection, envelope, payloadBytes);
        return;
    }
    if (envelope.kind === 'delta') {
        const applied = applyLiveV2Delta(envelope, cursor);
        if (!applied.ok) {
            rejectProtocol(connection);
            return;
        }
        clearListValidator();
        if (!replaceConnection(connection, applied.cursor)) return;
        addCounter('deltas');
        addCounter('deltaBytes', payloadBytes);
        addCounter('inserted', applied.summary.inserted);
        addCounter('updated', applied.summary.updated);
        addCounter('deleted', applied.summary.deleted);
        addCounter('projected', applied.summary.projected);
        setStatus('open');
        return;
    }
    if (envelope.seq !== cursor.seq + 1) {
        rejectProtocol(connection);
        return;
    }
    if (envelope.kind === 'snapshot') {
        commitV2Snapshot(connection, envelope, payloadBytes);
        return;
    }
    if (envelope.rev !== cursor.rev || envelope.schema !== cursor.schema) {
        rejectProtocol(connection);
        return;
    }
    addCounter('terminals');
    // `auth` is the one terminal a retry cannot survive: the session itself
    // expired. Every other reason (a rolling pod, a failed watch, a recycled
    // 12h session) is exactly what the reconnect ladder exists for.
    if (envelope.reason === 'auth') {
        enterUnavailable();
        return;
    }
    scheduleReconnect(connection.base);
}
function commitV2Snapshot(
    connection: LiveConnection,
    envelope: LiveV2SnapshotEnvelope,
    payloadBytes: number,
): void {
    const txn = Object.freeze({});
    swapSnapshot(envelope.snapshot.html, connection, txn);
    if (!completedSnapshotTxns.has(txn) || !isActive(connection)) {
        rejectProtocol(connection);
        return;
    }
    const cursor: LiveV2Cursor = {
        g: envelope.g,
        seq: envelope.seq,
        rev: envelope.rev,
        schema: envelope.schema,
    };
    if (envelope.rv !== undefined) cursor.rv = envelope.rv;
    replaceConnection(connection, cursor);
    addCounter('v2Snapshots');
    addCounter('snapshotBytes', payloadBytes);
    setStatus('open');
    // The ladder is NOT reset here: a snapshot alone does not prove the stream
    // is healthy, only that it started. shouldResetBackoff reads this timestamp
    // at the next drop and asks for continuity on top of it.
    if (snapshotAt === 0) snapshotAt = Date.now();
    noteStaleRetryAt(0);
    clearLiveStale();
}
function swapSnapshot(html: string, connection: LiveConnection, txn: object): void {
    const content = document.getElementById('resource-list-content');
    const htmx = (window as unknown as { htmx?: Htmx }).htmx;
    if (!content || !htmx || !isActive(connection)) return;
    clearListValidator();
    const eventInfo: LiveSnapshotEventInfo = {
        target: content,
        roLivePush: true,
        roLiveSnapshotTxn: txn,
    };
    // A throwing swap must NOT escape to the read loop: there it would read as
    // a transport drop and start the reconnect ladder. Swallowing it leaves the
    // transaction uncommitted, which the caller turns into the bounded resync a
    // rendering fault actually deserves.
    try {
        htmx.swap(content, html, { swapStyle: 'morph' }, { contextElement: content, eventInfo });
    } catch {}
}
function rejectProtocol(connection: LiveConnection, countInvalid = true): void {
    if (!isActive(connection)) return;
    if (countInvalid) addCounter('invalidFrames');
    const base = connection.base;
    requestResync(base);
}
// A protocol failure is a bug on one side of the wire, not a transport blip, so
// it gets a small bounded budget of immediate reopens and then stops for good.
function requestResync(base: string): void {
    pruneResyncWindow();
    if (resyncTimestamps.length >= MAX_RESYNCS_PER_WINDOW) {
        enterUnavailable();
        return;
    }
    resyncTimestamps.push(Date.now());
    addCounter('resyncs');
    openConnection(base);
}

function requestActivity(activity: ListRequestActivity): void {
    if (activity.phase === 'start') {
        const connection = runtime.connection;
        if (connection) {
            // A request path is only an intent until its list swap lands. Pin the
            // last committed projection so cancellation/failure cannot redirect Live.
            abortActiveConnection();
            enterDeferred(document.hidden ? 'hidden' : 'suspended', connection.base);
        } else if (runtime.status === 'reconnecting' || runtime.status === 'offline') {
            // An armed retry yields to the user's request; the request's own
            // outcome decides which URL the stream reopens against.
            clearReconnectTimer();
            enterDeferred(
                document.hidden ? 'hidden' : 'suspended',
                resumeIntent?.base ?? (runtime.streamPath as string),
            );
        }
        return;
    }
    if (!resumeIntent || activity.inFlight !== 0) return;
    if (!isLiveEnabled()) {
        liveSetOff();
        return;
    }
    const { base } = resumeIntent;
    resumeIntent = null;
    openConnection(base);
}

// Push snapshots commit through their opaque token. A request-driven list swap
// is instead the single point where its new URL becomes committed Live identity.
export function liveOnListSwap(event: Event): void {
    const detail = Object((event as CustomEvent).detail) as Record<string, unknown>;
    if (detail.roLivePush !== true) {
        if (resumeIntent) {
            const base = liveCanStreamHere() ? liveStreamBase() : '';
            resumeIntent = { base };
            runtime.streamPath = base;
        }
        return;
    }
    const snapshotTxn = detail.roLiveSnapshotTxn;
    if (typeof snapshotTxn === 'object' && snapshotTxn !== null) {
        completedSnapshotTxns.add(snapshotTxn as object);
    }
}

export function liveApply(force?: boolean): void {
    if (!requestSubscribed) {
        subscribeListRequests(requestActivity);
        requestSubscribed = true;
    }
    if (!isLiveEnabled()) {
        liveSetOff();
        return;
    }
    const base = liveCanStreamHere() ? liveStreamBase() : '';
    if (force) {
        // An explicit user action (the toggle, the banner's Retry) starts from
        // a clean slate: full resync budget, ladder rung 1, no warning.
        resyncTimestamps = [];
        resumeIntent = null;
        reconnectAttempt = 0;
        clearReconnectTimer();
        clearLiveStale();
    }
    if (!force && base === runtime.streamPath && runtime.status !== 'off') return;
    openConnection(base);
}

// A hidden tab holds no stream: the pod-local WatchHub keeps the shared watch
// alive across the gap, so closing here costs one connection and nothing else.
document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
        const connection = runtime.connection;
        const armed = runtime.status === 'reconnecting' || runtime.status === 'offline';
        const base = connection?.base ?? (armed ? runtime.streamPath : resumeIntent?.base);
        // Off, unavailable, and an already-parked state have nothing to pause.
        if (base === undefined) return;
        abortActiveConnection();
        clearReconnectTimer();
        enterDeferred('hidden', base);
        return;
    }
    if (runtime.status === 'hidden' && resumeIntent) {
        if (!isLiveEnabled()) {
            liveSetOff();
            return;
        }
        const { base } = resumeIntent;
        resumeIntent = null;
        openConnection(base);
    }
});

// An offline browser cannot reach the pod: pause the ladder rather than burn
// its rungs on attempts that are certain to fail, and reconnect ONCE on
// `online` instead of waiting out a delay that no longer means anything.
window.addEventListener('offline', () => {
    const holding =
        runtime.status === 'connecting' ||
        runtime.status === 'open' ||
        runtime.status === 'reconnecting';
    const base = runtime.connection?.base ?? runtime.streamPath;
    // A hidden or suspended tab already owns its own wake-up, and openConnection
    // re-checks connectivity when it fires; only a live attempt is parked here.
    if (!holding || !base) return;
    abortActiveConnection();
    clearReconnectTimer();
    noteDisconnected();
    enterDeferred('offline', base);
});

window.addEventListener('online', () => {
    if (runtime.status !== 'offline' || !resumeIntent) return;
    if (!isLiveEnabled()) {
        liveSetOff();
        return;
    }
    const { base } = resumeIntent;
    resumeIntent = null;
    openConnection(base);
});

window.roLive = { stats: currentStats };
