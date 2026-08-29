// live.ts -- negotiated Live v1/v2 browser transport and lifecycle.
//
// v1 remains a rolling-deploy compatibility lane: the client still sends the
// legacy query generation and accepts an unversioned response. Negotiated v2
// carries strict snapshot/delta envelopes, publishes its cursor only after the
// complete DOM repair transaction, and resynchronizes on any protocol drift.

import { clearListValidator } from './list-etag.js';
import { shouldDiscardPush } from './live-policy.js';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    type LiveV2Cursor,
    type LiveV2Envelope,
    type LiveV2SnapshotEnvelope,
} from './live-protocol.js';
import { LiveSSEError, type LiveSSEEvent, LiveSSEParser } from './live-sse.js';
import {
    liveRequestURL,
    liveScreenForBase,
    liveStreamBaseForURL,
    liveStreamBaseFromTableRequest,
    mintLiveGeneration,
} from './live-url.js';
import { refreshMode, scheduleRefreshTick } from './refresh.js';
import { markListStale } from './stale.js';

interface LiveSnapshotEventInfo {
    target: Element;
    roLivePush: true;
    roLiveSnapshotTxn?: object;
}

interface Htmx {
    swap(
        target: Element,
        content: string,
        swapSpec: { swapStyle: string },
        swapOptions: { contextElement: Element; eventInfo: LiveSnapshotEventInfo },
    ): void;
}

function getHtmx(): Htmx | undefined {
    return (window as unknown as { htmx?: Htmx }).htmx;
}

export type LiveStatus =
    | 'idle'
    | 'connecting'
    | 'syncing-v1'
    | 'syncing-v2'
    | 'open-v1'
    | 'open-v2'
    | 'resyncing'
    | 'suspended'
    | 'fallback'
    | 'hidden';

export const liveState: {
    status: LiveStatus;
    abort: AbortController | null;
    gen: string;
    streamPath: string;
} = {
    status: 'idle',
    abort: null,
    gen: '',
    streamPath: '',
};

type LiveProtocol = 'pending' | 'v1' | 'v2';

interface LiveConnection {
    readonly ctrl: AbortController;
    readonly generation: string;
    readonly base: string;
    readonly screen: string;
    readonly pageEpoch: number;
    readonly protocol: LiveProtocol;
    readonly cursor: Readonly<LiveV2Cursor> | null;
}

interface LiveCounters {
    connections: number;
    resyncs: number;
    fallbacks: number;
    v1Snapshots: number;
    v2Snapshots: number;
    deltas: number;
    terminals: number;
    invalidFrames: number;
    discards: number;
    rawBytes: number;
    payloadBytes: number;
    snapshotBytes: number;
    deltaBytes: number;
    inserted: number;
    updated: number;
    deleted: number;
    projected: number;
}

export interface LiveDebugStats extends LiveCounters {
    state: LiveStatus;
    protocol: LiveProtocol | null;
    seq: number;
    inFlightRequests: number;
    resyncsInWindow: number;
}

const counters: LiveCounters = {
    connections: 0,
    resyncs: 0,
    fallbacks: 0,
    v1Snapshots: 0,
    v2Snapshots: 0,
    deltas: 0,
    terminals: 0,
    invalidFrames: 0,
    discards: 0,
    rawBytes: 0,
    payloadBytes: 0,
    snapshotBytes: 0,
    deltaBytes: 0,
    inserted: 0,
    updated: 0,
    deleted: 0,
    projected: 0,
};

type CounterName = keyof LiveCounters;
const MAX_COUNTER = Number.MAX_SAFE_INTEGER;
const RESYNC_WINDOW_MS = 30_000;
const MAX_RESYNCS_PER_WINDOW = 2;
export const LIVE_FIRST_FRAME_TIMEOUT_MS = 10_000;

let activeConnection: LiveConnection | null = null;
let liveFallbackSecs = 0;
let pageEpoch = 0;
let pendingSnapshotTxn: object | null = null;
const completedSnapshotTxns = new WeakSet<object>();
let resyncTimestamps: number[] = [];
let resyncScheduled = false;
let pendingResync = false;
let resumeAfterRequests = false;
let resumeAfterHidden = false;
let resumeBase = '';
// Retain the historical bundle needle and expose the same saturating value
// through the debug seam. Expected connection aborts/supersessions never feed
// this counter; only a parsed frame rejected at its identity gate does.
let liveDiscards = 0;

interface OwnedRequest {
    readonly epoch: number;
    readonly xhr: XMLHttpRequest;
    networkSettled: boolean;
    sent: boolean;
    swapCompleted: boolean;
}

const ownedRequests = new Map<XMLHttpRequest, OwnedRequest>();

function addCounter(name: CounterName, amount = 1): void {
    if (!Number.isFinite(amount) || amount <= 0) return;
    counters[name] = Math.min(MAX_COUNTER, counters[name] + Math.floor(amount));
    if (name === 'discards') liveDiscards = counters.discards;
}

function pruneResyncWindow(now = Date.now()): void {
    resyncTimestamps = resyncTimestamps.filter((timestamp) => now - timestamp < RESYNC_WINDOW_MS);
}

function currentStats(): LiveDebugStats {
    pruneResyncWindow();
    return {
        ...counters,
        state: liveState.status,
        protocol: activeConnection?.protocol || null,
        seq: activeConnection?.cursor?.seq || 0,
        inFlightRequests: ownedRequests.size,
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

// The exact literal comparison `liveStreamBase() !== liveState.streamPath` is
// retained in the v1 morph gate below for the server-side bundle contract.
function liveStreamBase(): string {
    return liveStreamBaseForURL(new URL(window.location.href));
}

function isActive(connection: LiveConnection): boolean {
    return (
        activeConnection === connection &&
        liveState.abort === connection.ctrl &&
        connection.pageEpoch === pageEpoch
    );
}

function connectionToken(
    source: Omit<LiveConnection, 'protocol' | 'cursor'> &
        Partial<Pick<LiveConnection, 'protocol' | 'cursor'>>,
): LiveConnection {
    return Object.freeze({
        ...source,
        protocol: source.protocol || 'pending',
        cursor: source.cursor ? Object.freeze({ ...source.cursor }) : null,
    });
}

function replaceConnection(
    current: LiveConnection,
    protocol: LiveProtocol,
    cursor: LiveV2Cursor | Readonly<LiveV2Cursor> | null,
): LiveConnection | null {
    if (!isActive(current)) return null;
    const next = connectionToken({ ...current, protocol, cursor });
    activeConnection = next;
    return next;
}

function abortActiveConnection(): void {
    const connection = activeConnection;
    activeConnection = null;
    liveState.abort = null;
    pendingSnapshotTxn = null;
    if (connection && !connection.ctrl.signal.aborted) connection.ctrl.abort();
}

export function liveTeardown(): void {
    abortActiveConnection();
    liveFallbackSecs = 0;
}

// Called at body/history ownership boundaries. The existing body hook keeps
// the pinned liveTeardown + state reset literals alongside this epoch bump.
export function liveResetPage(): void {
    pageEpoch += 1;
    ownedRequests.clear();
    pendingSnapshotTxn = null;
    resumeAfterRequests = false;
    resumeAfterHidden = false;
    resumeBase = '';
    pendingResync = false;
    resyncScheduled = false;
    resyncTimestamps = [];
}

function liveEngageFallback(banner: boolean): void {
    abortActiveConnection();
    pendingResync = false;
    resyncScheduled = false;
    resumeAfterRequests = false;
    resumeAfterHidden = false;
    liveState.status = 'fallback';
    liveFallbackSecs = document.getElementById('resource-list-content') ? 5 : 0;
    addCounter('fallbacks');
    scheduleRefreshTick();
    if (banner) markListStale();
}

function openConnection(base: string): void {
    abortActiveConnection();
    liveFallbackSecs = 0;
    liveState.streamPath = base;
    if (!base) {
        liveEngageFallback(false);
        return;
    }
    if (document.hidden) {
        resumeBase = base;
        resumeAfterHidden = true;
        liveState.status = 'hidden';
        scheduleRefreshTick();
        return;
    }
    let generation: string;
    try {
        generation = mintLiveGeneration();
    } catch {
        liveEngageFallback(false);
        return;
    }
    const ctrl = new AbortController();
    const connection = connectionToken({
        ctrl,
        generation,
        base,
        screen: liveScreenForBase(base),
        pageEpoch,
        protocol: 'pending',
        cursor: null,
    });
    activeConnection = connection;
    liveState.abort = ctrl;
    liveState.gen = generation;
    liveState.status = 'connecting';
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

function negotiatedProtocol(response: Response, connection: LiveConnection): 'v1' | 'v2' | null {
    const contentType = responseHeader(response, 'Content-Type');
    if (contentType?.split(';', 1)[0].trim().toLowerCase() !== 'text/event-stream') {
        return null;
    }
    const version = responseHeader(response, 'RO-Live-Version');
    const generation = responseHeader(response, 'RO-Live-Generation');
    if (version === null && generation === null) return 'v1';
    if (version === '2' && generation === connection.generation) return 'v2';
    return null;
}

function firstFrameAccepted(): boolean {
    return liveState.status === 'open-v1' || liveState.status === 'open-v2';
}

async function liveConnect(initial: LiveConnection): Promise<void> {
    let firstFrameTimer: number | null = window.setTimeout(() => {
        firstFrameTimer = null;
        const current = activeConnection;
        if (
            current?.ctrl === initial.ctrl &&
            (liveState.status === 'connecting' ||
                liveState.status === 'syncing-v1' ||
                liveState.status === 'syncing-v2')
        ) {
            liveEngageFallback(false);
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
        response = await fetch(liveRequestURL(initial.base, initial.generation), {
            signal: initial.ctrl.signal,
            headers: {
                'RO-Live-Version': '2',
                'RO-Live-Generation': initial.generation,
            },
        });
    } catch {
        if (!isActive(initial)) return;
        liveEngageFallback(false);
        return;
    }
    if (!isActive(initial)) return;
    if (response.status !== 200 || !response.body) {
        liveEngageFallback(false);
        return;
    }
    const protocol = negotiatedProtocol(response, initial);
    if (!protocol) {
        rejectProtocol(initial);
        return;
    }
    let connection = replaceConnection(initial, protocol, null);
    if (!connection) return;
    liveState.status = protocol === 'v2' ? 'syncing-v2' : 'syncing-v1';
    const parser = new LiveSSEParser();
    let reader: ReadableStreamDefaultReader<Uint8Array>;
    try {
        reader = response.body.getReader();
    } catch {
        if (isActive(connection)) liveEngageFallback(true);
        return;
    }
    try {
        for (;;) {
            const result = await reader.read();
            if (!isActive(connection)) return;
            if (result.done) {
                parser.finish();
                break;
            }
            const value = result.value;
            addCounter('rawBytes', value.byteLength);
            let events: LiveSSEEvent[];
            try {
                events = parser.push(value);
            } catch (error) {
                if (error instanceof LiveSSEError) addCounter('invalidFrames');
                if (connection.protocol === 'v2') rejectProtocol(connection, false);
                else liveEngageFallback(true);
                return;
            }
            for (const event of events) {
                if (!isActive(connection)) {
                    addCounter('discards');
                    return;
                }
                addCounter('payloadBytes', event.dataBytes);
                if (connection.protocol === 'v2') {
                    if (!handleV2Frame(connection, event.name, event.data, event.dataBytes)) return;
                } else if (!handleV1Frame(connection, event.name, event.data, event.dataBytes)) {
                    return;
                }
                if (firstFrameAccepted()) clearFirstFrameTimer();
                const current = activeConnection;
                if (!current || current.ctrl !== connection.ctrl) return;
                connection = current;
            }
        }
    } catch {
        if (!isActive(connection)) return;
    }
    if (isActive(connection)) liveEngageFallback(true);
}

function parseJSONRecord(text: string): Record<string, unknown> | null {
    try {
        const value: unknown = JSON.parse(text);
        return typeof value === 'object' && value !== null && !Array.isArray(value)
            ? (value as Record<string, unknown>)
            : null;
    } catch {
        return null;
    }
}

const TERMINAL_REASONS = new Set(['idle', 'auth', 'watch-failed', 'shutdown']);

function handleV1Frame(
    connection: LiveConnection,
    name: string | null,
    text: string,
    payloadBytes: number,
): boolean {
    if (name !== 'ro-table' && name !== 'ro-terminal') return true;
    const payload = parseJSONRecord(text);
    if (!payload) {
        addCounter('invalidFrames');
        return true;
    }
    if (name === 'ro-terminal') {
        if (payload.g !== connection.generation || !TERMINAL_REASONS.has(String(payload.reason))) {
            addCounter('invalidFrames');
            return true;
        }
        addCounter('terminals');
        liveEngageFallback(true);
        return false;
    }
    if (typeof payload.g !== 'string' || typeof payload.html !== 'string') {
        addCounter('invalidFrames');
        return true;
    }
    const reason = shouldDiscardPush({
        frameGeneration: payload.g,
        currentGeneration: connection.generation,
        liveStreamBase: liveState.streamPath,
        openedStreamBase: liveState.streamPath,
        requestInFlight: ownedRequests.size > 0,
    });
    if (
        reason === 'stale-generation' ||
        liveStreamBase() !== liveState.streamPath ||
        reason === 'request-in-flight'
    ) {
        addCounter('discards');
        return true;
    }
    if (!swapSnapshot(payload.html, connection, null)) {
        addCounter('invalidFrames');
        liveEngageFallback(true);
        return false;
    }
    addCounter('v1Snapshots');
    addCounter('snapshotBytes', payloadBytes);
    liveState.status = 'open-v1';
    return true;
}

function validEnvelopeIdentity(envelope: LiveV2Envelope, connection: LiveConnection): boolean {
    return envelope.g === connection.generation && envelope.screen === connection.screen;
}

function snapshotCursor(envelope: LiveV2SnapshotEnvelope): LiveV2Cursor {
    const cursor: LiveV2Cursor = {
        g: envelope.g,
        seq: envelope.seq,
        screen: envelope.screen,
        rev: envelope.rev,
        schema: envelope.schema,
    };
    if (envelope.rv !== undefined) cursor.rv = envelope.rv;
    return cursor;
}

function handleV2Frame(
    connection: LiveConnection,
    name: string | null,
    text: string,
    payloadBytes: number,
): boolean {
    if (name !== 'ro-live') {
        rejectProtocol(connection);
        return false;
    }
    const decoded = decodeLiveV2Envelope(text);
    if (!decoded.ok) {
        rejectProtocol(connection);
        return false;
    }
    const envelope = decoded.value;
    const cursor = connection.cursor;
    if (!validEnvelopeIdentity(envelope, connection)) {
        rejectProtocol(connection);
        return false;
    }
    if (!cursor) {
        if (envelope.kind !== 'snapshot' || envelope.seq !== 1) {
            rejectProtocol(connection);
            return false;
        }
        return commitV2Snapshot(connection, envelope, payloadBytes);
    }
    if (envelope.seq !== cursor.seq + 1) {
        rejectProtocol(connection);
        return false;
    }
    if (envelope.kind === 'snapshot') {
        return commitV2Snapshot(connection, envelope, payloadBytes);
    }
    if (envelope.kind === 'terminal') {
        if (envelope.rev !== cursor.rev || envelope.schema !== cursor.schema) {
            rejectProtocol(connection);
            return false;
        }
        addCounter('terminals');
        liveEngageFallback(true);
        return false;
    }
    const applied = applyLiveV2Delta(decoded.value, cursor);
    if (!applied.ok) {
        rejectProtocol(connection);
        return false;
    }
    if (!replaceConnection(connection, 'v2', applied.cursor)) {
        return false;
    }
    // A direct delta has no response validator and emits no fake HTMX event.
    // Clear only after the atomic reducer has committed.
    clearListValidator();
    addCounter('deltas');
    addCounter('deltaBytes', payloadBytes);
    addCounter('inserted', applied.summary.inserted);
    addCounter('updated', applied.summary.updated);
    addCounter('deleted', applied.summary.deleted);
    addCounter('projected', applied.summary.projected);
    liveState.status = 'open-v2';
    return true;
}

function commitV2Snapshot(
    connection: LiveConnection,
    envelope: LiveV2SnapshotEnvelope,
    payloadBytes: number,
): boolean {
    const txn = Object.freeze({
        generation: connection.generation,
        pageEpoch: connection.pageEpoch,
        sequence: envelope.seq,
    });
    pendingSnapshotTxn = txn;
    if (!swapSnapshot(envelope.snapshot.html, connection, txn)) {
        pendingSnapshotTxn = null;
        rejectProtocol(connection);
        return false;
    }
    const completed = completedSnapshotTxns.has(txn);
    pendingSnapshotTxn = null;
    if (!completed || !isActive(connection)) {
        rejectProtocol(connection);
        return false;
    }
    const next = replaceConnection(connection, 'v2', snapshotCursor(envelope));
    if (!next) {
        return false;
    }
    addCounter('v2Snapshots');
    addCounter('snapshotBytes', payloadBytes);
    liveState.status = 'open-v2';
    return true;
}

function swapSnapshot(html: string, connection: LiveConnection, txn: object | null): boolean {
    const content = document.getElementById('resource-list-content');
    const htmx = getHtmx();
    if (!content || !htmx || typeof htmx.swap !== 'function' || !isActive(connection)) return false;
    clearListValidator();
    const eventInfo: LiveSnapshotEventInfo = { target: content, roLivePush: true };
    if (txn) eventInfo.roLiveSnapshotTxn = txn;
    try {
        htmx.swap(content, html, { swapStyle: 'morph' }, { contextElement: content, eventInfo });
        return isActive(connection);
    } catch {
        return false;
    }
}

function rejectProtocol(connection: LiveConnection, countInvalid = true): void {
    if (!isActive(connection)) return;
    if (countInvalid) addCounter('invalidFrames');
    const base = connection.base;
    abortActiveConnection();
    requestResync(base);
}

function requestResync(base: string): void {
    if (resyncScheduled) return;
    if (document.hidden || ownedRequests.size > 0) {
        pendingResync = true;
        resumeBase = base;
        if (document.hidden) {
            liveState.status = 'hidden';
            resumeAfterHidden = true;
        } else {
            liveState.status = 'suspended';
            resumeAfterRequests = true;
        }
        return;
    }
    const now = Date.now();
    pruneResyncWindow(now);
    if (resyncTimestamps.length >= MAX_RESYNCS_PER_WINDOW) {
        liveEngageFallback(true);
        return;
    }
    resyncTimestamps.push(now);
    addCounter('resyncs');
    liveState.status = 'resyncing';
    resyncScheduled = true;
    const epoch = pageEpoch;
    queueMicrotask(() => {
        if (!resyncScheduled || liveState.status !== 'resyncing' || pageEpoch !== epoch) return;
        resyncScheduled = false;
        openConnection(base);
    });
}

function requestDetail(event: Event): Record<string, unknown> {
    return Object((event as CustomEvent).detail) as Record<string, unknown>;
}

function requestPathBase(detail: Record<string, unknown>): string | null {
    const pathInfo = Object(detail.pathInfo) as Record<string, unknown>;
    return (
        liveStreamBaseFromTableRequest(pathInfo.finalRequestPath) ||
        liveStreamBaseFromTableRequest(pathInfo.requestPath)
    );
}

// Called as the very first statement of refresh.ts's beforeRequest listener.
// Ownership is tied to the exact current content node and native XHR loadend,
// so a detached issuer cannot strand the stream in a suspended state.
export function liveBeforeListRequest(event: Event): void {
    const detail = requestDetail(event);
    const content = document.getElementById('resource-list-content');
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    if (!content || detail.target !== content || !xhr || ownedRequests.has(xhr)) return;
    const scheduledResync = resyncScheduled || liveState.status === 'resyncing';
    const resumable =
        activeConnection !== null ||
        scheduledResync ||
        resumeAfterRequests ||
        (liveState.status === 'hidden' && resumeAfterHidden);
    const entry: OwnedRequest = {
        epoch: pageEpoch,
        xhr,
        networkSettled: false,
        sent: false,
        swapCompleted: false,
    };
    ownedRequests.set(xhr, entry);
    try {
        xhr.addEventListener('loadend', () => noteRequestNetworkSettled(xhr, entry), {
            once: true,
        });
    } catch {
        // htmx:afterRequest remains the ordinary settlement path. Native
        // XMLHttpRequest always supports addEventListener; this keeps the
        // public document handler total under synthetic tests/tooling events.
    }
    // htmx:beforeRequest is cancelable. beforeSend follows synchronously only
    // when HTMX will actually send this exact XHR; otherwise no native loadend
    // exists, so retire the speculative ownership in the next microtask.
    queueMicrotask(() => {
        if (!entry.sent) finalizeOwnedRequest(xhr, entry);
    });
    // Track even polling/Off traffic: an explicit Live pick during this XHR
    // must see it and defer opening. Sticky fallback itself still never sets a
    // resume intent and therefore cannot auto-reopen from ordinary polling.
    if (!resumable || liveState.status === 'fallback') return;
    // The resync budget was charged when this ticket was scheduled. Cancel its
    // queued opener and let the ordinary final-request barrier perform that
    // already-paid reopen once; leaving the flag set would poison all later
    // resync attempts.
    if (scheduledResync) resyncScheduled = false;
    resumeAfterRequests = true;
    resumeBase ||= activeConnection?.base || liveState.streamPath;
    abortActiveConnection();
    if (document.hidden) {
        liveState.status = 'hidden';
        resumeAfterHidden = true;
    } else {
        liveState.status = 'suspended';
    }
}

export function liveMarkListRequestSent(event: Event): void {
    const xhr = requestDetail(event).xhr as XMLHttpRequest | undefined;
    const entry = xhr ? ownedRequests.get(xhr) : undefined;
    if (entry && entry.epoch === pageEpoch) entry.sent = true;
}

export function liveAfterListRequest(event: Event): void {
    const detail = requestDetail(event);
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    if (!xhr) return;
    const entry = ownedRequests.get(xhr);
    if (entry) noteRequestNetworkSettled(xhr, entry);
}

function requestStatus(xhr: XMLHttpRequest): number {
    try {
        return xhr.status;
    } catch {
        return 0;
    }
}

function noteRequestNetworkSettled(xhr: XMLHttpRequest, entry: OwnedRequest): void {
    if (entry.epoch !== pageEpoch || ownedRequests.get(xhr) !== entry) return;
    entry.networkSettled = true;
    // A successful HTMX request is not complete from Live's point of view until
    // its final afterSwap repair marker lands. Readout disables delayed native
    // transitions, so HTMX normally supplies that marker before afterRequest.
    // A beforeSwap cancellation resolves in an earlier queued microtask. Give
    // that synchronous lifecycle one microtask to finish; if no terminal swap
    // signal exists, fail closed instead of keeping a timer-owned request.
    if (requestStatus(xhr) === 200 && !entry.swapCompleted) {
        queueMicrotask(() => failOwnedRequestWithoutSwap(xhr, entry));
        return;
    }
    finalizeOwnedRequest(xhr, entry);
}

function completeOwnedRequestSwap(
    xhr: XMLHttpRequest,
    entry: OwnedRequest,
    successfulBase: string | null,
): void {
    if (entry.epoch !== pageEpoch || ownedRequests.get(xhr) !== entry) return;
    entry.swapCompleted = true;
    if (successfulBase) {
        resumeBase = successfulBase;
    }
    if (entry.networkSettled) finalizeOwnedRequest(xhr, entry);
}

function failOwnedRequestWithoutSwap(xhr: XMLHttpRequest, entry: OwnedRequest): void {
    if (
        entry.epoch !== pageEpoch ||
        ownedRequests.get(xhr) !== entry ||
        entry.swapCompleted ||
        !entry.networkSettled
    ) {
        return;
    }
    ownedRequests.delete(xhr);
    if (!resumeAfterRequests) return;
    if (refreshMode() !== 'Live') {
        resumeAfterRequests = false;
        resumeAfterHidden = false;
        pendingResync = false;
        liveState.status = 'idle';
        liveState.streamPath = '';
        return;
    }
    // A 200 without afterSwap, an explicit cancellation, or swapError violates
    // the owned HTMX lifecycle. Sticky polling is the safe fail-closed state;
    // only an explicit Live repick may leave it.
    liveEngageFallback(true);
}

function finalizeOwnedRequest(xhr: XMLHttpRequest, entry: OwnedRequest): void {
    if (entry.epoch !== pageEpoch || ownedRequests.get(xhr) !== entry) return;
    ownedRequests.delete(xhr);
    if (ownedRequests.size > 0 || !resumeAfterRequests) return;
    if (document.hidden) {
        liveState.status = 'hidden';
        resumeAfterHidden = true;
        return;
    }
    const base = resumeBase || (liveSupported() ? liveStreamBase() : '');
    resumeAfterRequests = false;
    resumeAfterHidden = false;
    const shouldResync = pendingResync;
    pendingResync = false;
    if (refreshMode() !== 'Live') {
        liveState.status = 'idle';
        liveState.streamPath = '';
        return;
    }
    if (shouldResync) requestResync(base);
    else openConnection(liveSupported() ? base : '');
}

// Registered before init.ts's beforeSwap policy listener. Deferring the check
// to a microtask observes the final shouldSwap/defaultPrevented decision after
// every synchronous listener has run, including the app-managed 304 gate.
export function liveBeforeListSwapDecision(event: Event): void {
    const detail = requestDetail(event);
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    const entry = xhr ? ownedRequests.get(xhr) : undefined;
    if (!xhr || !entry || entry.epoch !== pageEpoch) return;
    queueMicrotask(() => {
        if ((event as CustomEvent).defaultPrevented || detail.shouldSwap === false) {
            completeOwnedRequestSwap(xhr, entry, null);
        }
    });
}

// HTMX reports a thrown/failed swap separately from afterSwap. Treat it as a
// completed (unsuccessful) DOM barrier so a 200 cannot strand Live forever.
export function liveListRequestSwapFailed(event: Event): void {
    const detail = requestDetail(event);
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    const entry = xhr ? ownedRequests.get(xhr) : undefined;
    if (xhr && entry) completeOwnedRequestSwap(xhr, entry, null);
}

// Called at the end of init.ts's fixed afterSwap pipeline. A Live snapshot
// marks only its exact transaction complete; a real request swap records its
// final byte-preserved table path for the one post-loadend resume.
export function liveOnListSwap(event: Event): void {
    const detail = requestDetail(event);
    const snapshotTxn = detail.roLiveSnapshotTxn;
    if (snapshotTxn && snapshotTxn === pendingSnapshotTxn) {
        completedSnapshotTxns.add(snapshotTxn as object);
        return;
    }
    if (detail.roLivePush) return;
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    const entry = xhr ? ownedRequests.get(xhr) : undefined;
    if (!entry || entry.epoch !== pageEpoch) return;
    const base = requestPathBase(detail);
    completeOwnedRequestSwap(xhr as XMLHttpRequest, entry, base);
}

export function liveApply(force?: boolean): void {
    if (refreshMode() !== 'Live') {
        liveTeardown();
        liveState.status = 'idle';
        liveState.streamPath = '';
        return;
    }
    const base = liveSupported() ? liveStreamBase() : '';
    if (force) {
        resyncTimestamps = [];
        pendingResync = false;
        resyncScheduled = false;
    }
    if (!force && liveState.status === 'fallback') return;
    if (!force && base === liveState.streamPath && liveState.status !== 'idle') return;
    if (ownedRequests.size > 0) {
        liveTeardown();
        resumeAfterRequests = true;
        resumeBase = base;
        liveState.streamPath = base;
        liveState.status = document.hidden ? 'hidden' : 'suspended';
        resumeAfterHidden = document.hidden;
        return;
    }
    openConnection(base);
}

document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
        if (
            activeConnection ||
            liveState.status === 'suspended' ||
            liveState.status === 'resyncing'
        ) {
            resumeBase ||= activeConnection?.base || liveState.streamPath;
            resumeAfterHidden = true;
            abortActiveConnection();
            resyncScheduled = false;
            liveState.status = 'hidden';
        }
        return;
    }
    if (liveState.status !== 'hidden' || !resumeAfterHidden || refreshMode() !== 'Live') return;
    if (ownedRequests.size > 0) {
        liveState.status = 'suspended';
        return;
    }
    resumeAfterHidden = false;
    const base = resumeBase || (liveSupported() ? liveStreamBase() : '');
    if (pendingResync) {
        pendingResync = false;
        requestResync(base);
    } else {
        openConnection(liveSupported() ? base : '');
    }
});

window.roLive = {
    discards() {
        return liveDiscards;
    },
    stats() {
        return currentStats();
    },
};
