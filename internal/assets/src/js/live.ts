// live.ts -- Live v2 browser transport and lifecycle.
import { clearListValidator } from './list-etag.js';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    type LiveV2Cursor,
    type LiveV2SnapshotEnvelope,
} from './live-protocol.js';
import { type LiveSSEEvent, LiveSSEParser } from './live-sse.js';
import { liveStreamBaseForURL, mintLiveGeneration } from './live-url.js';
import {
    type ListRequestActivity,
    listRequestTrackerSnapshot,
    refreshMode,
    resetListRequestTracker,
    scheduleRefreshTick,
    subscribeListRequests,
} from './refresh.js';
import { clearLiveUnavailable, markLiveUnavailable } from './stale.js';

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
export type LiveStatus = 'off' | 'connecting' | 'open' | 'suspended' | 'hidden' | 'fallback';
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
    fallbacks: 0,
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
    inFlightRequests: number;
    resyncsInWindow: number;
};
type CounterName = keyof typeof counters;
const RESYNC_WINDOW_MS = 30_000;
const MAX_RESYNCS_PER_WINDOW = 2;
const FALLBACK_RETRY_INITIAL_MS = 60_000;
const FALLBACK_RETRY_MAX_MS = 300_000;
export const LIVE_FIRST_FRAME_TIMEOUT_MS = 30_000;

const completedSnapshotTxns = new WeakSet<object>();
let liveFallbackSecs = 0;
let resyncTimestamps: number[] = [];
let resumeIntent: { base: string; waitForChangedBase?: true } | null = null;
let requestSubscribed = false;
let fallbackRetryTimerId: number | undefined;
let fallbackRetryDelayMs = FALLBACK_RETRY_INITIAL_MS;
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
        inFlightRequests: listRequestTrackerSnapshot().count,
        resyncsInWindow: resyncTimestamps.length,
    };
}

export function liveFallbackSeconds(): number {
    return liveFallbackSecs;
}
function liveSupported(): boolean {
    const content = document.getElementById('resource-list-content') as HTMLElement | null;
    if (content?.dataset.liveUrl !== 'location') return false;
    const option = document.querySelector(
        '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
    ) as HTMLButtonElement | null;
    return !!option && !option.disabled;
}

function liveStreamBase(): string {
    return liveStreamBaseForURL(new URL(window.location.href));
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
function clearFallbackRetry(): void {
    window.clearTimeout(fallbackRetryTimerId);
    fallbackRetryTimerId = undefined;
}
function resetFallbackRetry(): void {
    clearFallbackRetry();
    fallbackRetryDelayMs = FALLBACK_RETRY_INITIAL_MS;
}

function setOff(): void {
    abortActiveConnection();
    resetFallbackRetry();
    resumeIntent = null;
    liveFallbackSecs = 0;
    runtime.status = 'off';
    clearLiveUnavailable();
}

export function liveResetPage(): void {
    setOff();
    resetListRequestTracker();
    resyncTimestamps = [];
}
function scheduleFallbackRetry(): void {
    fallbackRetryTimerId = window.setTimeout(() => {
        fallbackRetryTimerId = undefined;
        if (refreshMode() !== 'Live') return;
        const base = liveSupported() ? liveStreamBase() : '';
        fallbackRetryDelayMs = Math.min(fallbackRetryDelayMs * 2, FALLBACK_RETRY_MAX_MS);
        openConnection(base);
    }, fallbackRetryDelayMs);
}

function liveEngageFallback(): void {
    abortActiveConnection();
    resumeIntent = null;
    runtime.status = 'fallback';
    liveFallbackSecs = document.getElementById('resource-list-content') ? 5 : 0;
    addCounter('fallbacks');
    scheduleRefreshTick();
    scheduleFallbackRetry();
    markLiveUnavailable();
}

function openConnection(base: string): void {
    abortActiveConnection();
    clearFallbackRetry();
    liveFallbackSecs = 0;
    runtime.streamPath = base;
    if (!base) {
        liveEngageFallback();
        return;
    }
    const deferredStatus = document.hidden
        ? 'hidden'
        : listRequestTrackerSnapshot().count > 0
          ? 'suspended'
          : null;
    if (deferredStatus) {
        resumeIntent = { base };
        runtime.status = deferredStatus;
        scheduleRefreshTick();
        return;
    }
    let generation: string;
    try {
        generation = mintLiveGeneration();
    } catch {
        liveEngageFallback();
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
    runtime.status = 'connecting';
    addCounter('connections');
    scheduleRefreshTick();
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
    let firstFrameTimer: number | null = window.setTimeout(() => {
        firstFrameTimer = null;
        if (runtime.connection?.ctrl === initial.ctrl) {
            liveEngageFallback();
        }
    }, LIVE_FIRST_FRAME_TIMEOUT_MS);
    const clearFirstFrameTimer = () => {
        if (firstFrameTimer !== null) {
            window.clearTimeout(firstFrameTimer);
            firstFrameTimer = null;
        }
    };
    try {
        await runLiveConnection(initial, clearFirstFrameTimer);
    } finally {
        clearFirstFrameTimer();
    }
}

async function runLiveConnection(
    initial: LiveConnection,
    clearFirstFrameTimer: () => void,
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
        if (isActive(initial)) liveEngageFallback();
        return;
    }
    if (!isActive(initial)) return;
    if (response.status !== 200 || !response.body) {
        liveEngageFallback();
        return;
    }
    if (!acceptsV2Response(response, initial)) {
        rejectProtocol(initial);
        return;
    }
    const accepted = replaceConnection(initial, null);
    if (!accepted) return;
    let connection = accepted;
    try {
        const reader = response.body.getReader();
        const parser = new LiveSSEParser();
        const readNext = async (): Promise<LiveConnection | undefined> => {
            const result = await reader.read();
            if (!isActive(connection) || result.done) return;
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
                clearFirstFrameTimer();
            }
            return connection;
        };
        while (await readNext()) {}
    } catch {}
    if (isActive(connection)) liveEngageFallback();
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
        if (!replaceConnection(connection, applied.cursor)) return;
        clearListValidator();
        addCounter('deltas');
        addCounter('deltaBytes', payloadBytes);
        addCounter('inserted', applied.summary.inserted);
        addCounter('updated', applied.summary.updated);
        addCounter('deleted', applied.summary.deleted);
        addCounter('projected', applied.summary.projected);
        runtime.status = 'open';
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
    liveEngageFallback();
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
    runtime.status = 'open';
    fallbackRetryDelayMs = FALLBACK_RETRY_INITIAL_MS;
    clearLiveUnavailable();
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
function requestResync(base: string): void {
    pruneResyncWindow();
    if (resyncTimestamps.length >= MAX_RESYNCS_PER_WINDOW) {
        liveEngageFallback();
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
            resumeIntent = { base: connection.base };
            abortActiveConnection();
            runtime.status = document.hidden ? 'hidden' : 'suspended';
        } else if (runtime.status === 'fallback' && !resumeIntent) {
            // A fallback poll does not itself justify another stream attempt.
            // Its successful swap may commit a different base below.
            resumeIntent = { base: runtime.streamPath as string, waitForChangedBase: true };
        }
        return;
    }
    if (!resumeIntent || activity.inFlight !== 0) return;
    if (refreshMode() !== 'Live') {
        setOff();
        return;
    }
    const { base, waitForChangedBase } = resumeIntent;
    resumeIntent = null;
    if (waitForChangedBase) {
        runtime.status = 'fallback';
        return;
    }
    openConnection(base);
}

// Push snapshots commit through their opaque token. A request-driven list swap
// is instead the single point where its new URL becomes committed Live identity.
export function liveOnListSwap(event: Event): void {
    const detail = Object((event as CustomEvent).detail) as Record<string, unknown>;
    if (detail.roLivePush !== true) {
        if (resumeIntent) {
            const base = liveSupported() ? liveStreamBase() : '';
            if (!resumeIntent.waitForChangedBase || base !== resumeIntent.base) {
                resumeIntent = { base };
            }
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
    if (refreshMode() !== 'Live') {
        setOff();
        return;
    }
    const base = liveSupported() ? liveStreamBase() : '';
    if (force) {
        resyncTimestamps = [];
        resumeIntent = null;
        resetFallbackRetry();
    }
    if (!force && base === runtime.streamPath && runtime.status !== 'off') return;
    openConnection(base);
}

document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
        const connection = runtime.connection;
        if (connection) resumeIntent = { base: connection.base };
        const intent = resumeIntent;
        if (!intent || intent.waitForChangedBase) return;
        abortActiveConnection();
        runtime.status = 'hidden';
        return;
    }
    if (runtime.status === 'hidden' && resumeIntent) {
        if (refreshMode() !== 'Live') {
            setOff();
            return;
        }
        const { base } = resumeIntent;
        resumeIntent = null;
        openConnection(base);
    }
});

window.roLive = { stats: currentStats };
