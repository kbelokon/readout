// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => {
    type Activity = {
        phase: 'start' | 'settle';
        inFlight: number;
    };
    const subscribers = new Set<(activity: Activity) => void>();
    const snapshot = { count: 0 };
    return {
        clearLiveStale: vi.fn(),
        markLiveStale: vi.fn(),
        markLiveUnavailable: vi.fn(),
        noteStaleRetryAt: vi.fn(),
        revealLiveStale: vi.fn(),
        isLiveEnabled: vi.fn(() => true),
        resetListRequestTracker: vi.fn(() => {
            if (snapshot.count === 0) return;
            snapshot.count = 0;
            subscribers.forEach((subscriber) => {
                subscriber({ phase: 'settle', inFlight: 0 });
            });
        }),
        snapshot,
        subscribers,
        subscribeListRequests: vi.fn((subscriber: (activity: Activity) => void) => {
            subscribers.add(subscriber);
            return () => subscribers.delete(subscriber);
        }),
    };
});

vi.mock('./refresh.js', () => ({
    isLiveEnabled: dependencies.isLiveEnabled,
    listRequestTrackerSnapshot: () => ({ ...dependencies.snapshot }),
    resetListRequestTracker: dependencies.resetListRequestTracker,
    subscribeListRequests: dependencies.subscribeListRequests,
}));
vi.mock('./stale.js', () => ({
    clearLiveStale: dependencies.clearLiveStale,
    markLiveStale: dependencies.markLiveStale,
    markLiveUnavailable: dependencies.markLiveUnavailable,
    noteStaleRetryAt: dependencies.noteStaleRetryAt,
    revealLiveStale: dependencies.revealLiveStale,
}));

import {
    adoptListProjection,
    listProjectionRowByKey,
    resetListProjection,
} from './list-projection.js';
import {
    LIVE_FIRST_FRAME_TIMEOUT_MS,
    LIVE_READ_IDLE_TIMEOUT_MS,
    liveApply,
    liveOnListSwap,
    liveResetPage,
} from './live.js';
import { RECONNECT_DELAY_LADDER_MS } from './live-policy.js';
import { LIST_DELTA_APPLIED_EVENT } from './live-protocol.js';
import { isClientLiveGeneration } from './live-url.js';

interface HtmxHarness {
    swap: ReturnType<typeof vi.fn>;
}

interface ControlledStream {
    close(): void;
    enqueue(value: string | Uint8Array): void;
    response: Response;
}

function renderLivePage(path = '/clusters/prod/pods'): HTMLElement {
    window.history.replaceState(null, '', path);
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.dataset.liveUrl = 'location';
    const toggle = document.createElement('button');
    toggle.dataset.roAction = 'toggle-live';
    toggle.setAttribute('aria-pressed', 'true');
    document.body.append(content, toggle);
    return content;
}

function listHTML(name = 'Alpha'): string {
    return `
        <input id="ro-filter-input" value="">
        <div class="ro-table-wrap" tabindex="0">
            <table class="ro-table">
                <thead><tr><th data-hint="string">Name</th></tr></thead>
                <tbody><tr id="row-dev/a" data-key="dev/a" data-name="${name}">
                    <td class="cell-name"><a href="#a">${name}</a></td>
                </tr></tbody>
            </table>
        </div>
        <div class="ro-cardlist"><div class="ro-pcard" data-key="dev/a">${name}</div></div>
        <span class="ro-count" data-ro-live-region="count">1</span>
        <div class="ro-phase-strip" data-ro-live-region="phase" hidden></div>
        <span class="ro-foundline" data-ro-live-region="found">1 found</span>
        <div id="ro-live-status" role="status" aria-live="polite"></div>`;
}

function installHtmx(
    mapEventInfo: (eventInfo: Record<string, unknown>) => Record<string, unknown> = (value) =>
        value,
): HtmxHarness {
    const swap = vi.fn(
        (
            target: Element,
            html: string,
            _swapSpec: unknown,
            options: { eventInfo: Record<string, unknown> },
        ) => {
            target.innerHTML = html;
            if (target.querySelector('tbody tr[data-key]')) adoptListProjection(target);
            liveOnListSwap(
                new CustomEvent('htmx:afterSwap', {
                    detail: { ...mapEventInfo(options.eventInfo), target },
                }),
            );
        },
    );
    const htmx = { swap };
    (window as unknown as { htmx: HtmxHarness }).htmx = htmx;
    return htmx;
}

function controlledStream(headers: HeadersInit = {}): ControlledStream {
    const encoder = new TextEncoder();
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
        start(value) {
            controller = value;
        },
    });
    return {
        close: () => controller.close(),
        enqueue: (value) =>
            controller.enqueue(typeof value === 'string' ? encoder.encode(value) : value),
        response: {
            status: 200,
            body,
            headers: new Headers({ 'Content-Type': 'text/event-stream', ...headers }),
        } as unknown as Response,
    };
}

function response(
    status: number,
    body: ReadableStream<Uint8Array> | null,
    headers: HeadersInit = {},
): Response {
    return {
        status,
        body,
        headers: new Headers(headers),
    } as unknown as Response;
}

function readerResponse(read: () => Promise<ReadableStreamReadResult<Uint8Array>>): Response {
    return response(200, { getReader: () => ({ read }) } as unknown as ReadableStream<Uint8Array>, {
        'Content-Type': 'text/event-stream',
    });
}

function pendingFetch(): Promise<Response> {
    return new Promise(() => {});
}

function deferred<T>() {
    let resolve!: (value: T) => void;
    let reject!: (reason?: unknown) => void;
    const promise = new Promise<T>((accept, decline) => {
        resolve = accept;
        reject = decline;
    });
    return { promise, reject, resolve };
}

function installFetch(
    implementation: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
) {
    const mock = vi.fn(implementation);
    vi.stubGlobal('fetch', mock);
    return mock;
}

function requestHeaders(fetchMock: ReturnType<typeof vi.fn>, call = 0): Record<string, string> {
    return fetchMock.mock.calls[call][1]?.headers as Record<string, string>;
}

function requestGeneration(fetchMock: ReturnType<typeof vi.fn>, call = 0): string {
    return requestHeaders(fetchMock, call)['RO-Live-Generation'];
}

function sse(name: string, payload: unknown): string {
    return `event: ${name}\ndata: ${JSON.stringify(payload)}\n\n`;
}

function snapshot(generation: string, overrides: Record<string, unknown> = {}) {
    return {
        v: 2,
        kind: 'snapshot',
        g: generation,
        seq: 1,
        rev: 'rev-1',
        rv: '10',
        schema: 'schema-1',
        snapshot: { html: listHTML() },
        ...overrides,
    };
}

function delta(generation: string, overrides: Record<string, unknown> = {}) {
    return {
        v: 2,
        kind: 'delta',
        g: generation,
        seq: 2,
        rev: 'rev-2',
        schema: 'schema-1',
        delta: {
            base: 'rev-1',
            rev: 'rev-2',
            remove: [{ key: 'dev/a', cause: 'delete' }],
        },
        ...overrides,
    };
}

function terminalFrame(
    generation: string,
    reason: 'auth' | 'lifetime' | 'shutdown' | 'watch-failed',
) {
    return {
        v: 2,
        kind: 'terminal',
        g: generation,
        seq: 2,
        rev: 'rev-1',
        schema: 'schema-1',
        reason,
    };
}

function startListRequest(): void {
    dependencies.snapshot.count += 1;
    const activity = { phase: 'start' as const, inFlight: dependencies.snapshot.count };
    dependencies.subscribers.forEach((subscriber) => {
        subscriber(activity);
    });
}

function settleListRequest(): void {
    dependencies.snapshot.count = Math.max(0, dependencies.snapshot.count - 1);
    const activity = { phase: 'settle' as const, inFlight: dependencies.snapshot.count };
    dependencies.subscribers.forEach((subscriber) => {
        subscriber(activity);
    });
}

function commitListSwap(path: string): void {
    window.history.replaceState(null, '', path);
    liveOnListSwap(
        new CustomEvent('htmx:afterSwap', {
            detail: { target: document.getElementById('resource-list-content') },
        }),
    );
}

async function flush(): Promise<void> {
    await Promise.resolve();
    await Promise.resolve();
}

beforeEach(() => {
    liveResetPage();
    dependencies.isLiveEnabled.mockReset().mockReturnValue(true);
    dependencies.resetListRequestTracker.mockClear();
    dependencies.clearLiveStale.mockReset();
    dependencies.markLiveStale.mockReset();
    dependencies.markLiveUnavailable.mockReset();
    dependencies.noteStaleRetryAt.mockReset();
    dependencies.revealLiveStale.mockReset();
    dependencies.snapshot.count = 0;
    document.body.replaceChildren();
    resetListProjection();
    delete (window as unknown as { htmx?: HtmxHarness }).htmx;
});

afterEach(async () => {
    liveResetPage();
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    await flush();
});

test('opens v2 with one header generation and preserves the raw list query', () => {
    renderLivePage('/clusters/prod/pods?g=old&f=status:Running,Pending&%67=older&x=%ZZ');
    const fetchMock = installFetch(pendingFetch);
    const before = window.roLive.stats().connections;

    liveApply();

    const generation = requestGeneration(fetchMock);
    const firstSignal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;
    expect(isClientLiveGeneration(generation)).toBe(true);
    expect(fetchMock).toHaveBeenCalledExactlyOnceWith(
        '/clusters/prod/pods/_stream?g=old&f=status:Running,Pending&%67=older&x=%ZZ',
        {
            signal: expect.any(AbortSignal),
            headers: {
                'RO-Live-Version': '2',
                'RO-Live-Generation': generation,
            },
        },
    );
    expect(window.roLive.stats()).toMatchObject({
        state: 'connecting',
        connections: before + 1,
    });
    expect(dependencies.subscribeListRequests).toHaveBeenCalledOnce();

    liveApply();
    expect(fetchMock).toHaveBeenCalledOnce();
    liveApply(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(firstSignal.aborted).toBe(true);
    expect(requestGeneration(fetchMock, 1)).not.toBe(generation);
    expect(dependencies.subscribeListRequests).toHaveBeenCalledOnce();
});

test('a response with stripped Live headers still commits a ro-live snapshot', async () => {
    const content = renderLivePage();
    installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);

    liveApply();
    stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('open'));
    expect(window.roLive.stats().seq).toBe(1);
    expect(content.querySelector('[data-key="dev/a"]')).toHaveTextContent('Alpha');
    expect(dependencies.clearLiveStale).toHaveBeenCalledOnce();
    expect(fetchMock.mock.calls[0][1]?.signal).toBeInstanceOf(AbortSignal);
});

test('exact v2 response headers and a normalized event-stream media type are accepted', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream({ 'Content-Type': ' Text/Event-Stream ; charset=utf-8' });
    const fetchMock = installFetch(async (_input, init) => {
        const headers = init?.headers as Record<string, string> | undefined;
        const generation = headers?.['RO-Live-Generation'] || '';
        (stream.response.headers as Headers).set('RO-Live-Version', '2');
        (stream.response.headers as Headers).set('RO-Live-Generation', generation);
        return stream.response;
    });

    liveApply();
    stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('open'));
});

test.each([
    ['wrong response version', { 'RO-Live-Version': '3' }],
    ['wrong echoed generation', { 'RO-Live-Generation': 'wrong' }],
    ['wrong content type', { 'Content-Type': 'application/json' }],
] as const)('%s resynchronizes instead of opening', async (_name, headers) => {
    renderLivePage();
    const stream = controlledStream(headers);
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? stream.response : pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats()).toMatchObject({
        state: 'connecting',
        resyncs: before.resyncs + 1,
        invalidFrames: before.invalidFrames + 1,
    });
});

test('a non-ro-live event is rejected and cannot mutate the DOM', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );

    liveApply();
    first.enqueue(sse('other', snapshot(requestGeneration(fetchMock))));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(content).toBeEmptyDOMElement();
});

test.each([
    ['malformed JSON', () => 'event: ro-live\ndata: {\n\n'],
    ['wrong snapshot generation', () => sse('ro-live', snapshot('stale'))],
    [
        'delta before the initial snapshot',
        (generation: string) => sse('ro-live', delta(generation, { seq: 1 })),
    ],
    [
        'non-initial snapshot sequence',
        (generation: string) => sse('ro-live', snapshot(generation, { seq: 2 })),
    ],
] as const)('%s is rejected before a cursor can be committed', async (_name, frame) => {
    renderLivePage();
    const htmx = installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();
    const signal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;
    first.enqueue(frame(requestGeneration(fetchMock)));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(signal.aborted).toBe(true);
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(window.roLive.stats()).toMatchObject({
        seq: 0,
        resyncs: before.resyncs + 1,
        invalidFrames: before.invalidFrames + 1,
    });
});

test('fatal SSE framing counts one invalid frame and starts one resync', async () => {
    renderLivePage();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();
    first.enqueue(new Uint8Array([0xff, 0x0a]));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats()).toMatchObject({
        invalidFrames: before.invalidFrames + 1,
        resyncs: before.resyncs + 1,
    });
});

test('snapshot cursor is published only after the exact synchronous swap token completes', async () => {
    renderLivePage();
    let seqInsideSwap = -1;
    const htmx = installHtmx();
    htmx.swap.mockImplementationOnce((target, html, _spec, options) => {
        seqInsideSwap = window.roLive.stats().seq;
        target.innerHTML = html;
        adoptListProjection(target);
        liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: options.eventInfo }));
    });
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);

    liveApply();
    stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));

    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    expect(seqInsideSwap).toBe(0);
});

const invalidSnapshotMarkers: Array<
    [string, (info: Record<string, unknown>) => Record<string, unknown>]
> = [
    ['missing marker', () => ({})],
    ['wrong truthy marker', (info) => ({ ...info, roLivePush: 'true' })],
    ['wrong transaction', (info) => ({ ...info, roLiveSnapshotTxn: {} })],
];

test.each(invalidSnapshotMarkers)('%s cannot commit a pending snapshot', async (_name, map) => {
    renderLivePage();
    installHtmx(map);
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );

    liveApply();
    first.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats().seq).toBe(0);
});

test.each([undefined, null, false, 0, 'token'])(
    'a public push marker with non-object transaction %j is total',
    (transaction) => {
        expect(() =>
            liveOnListSwap(
                new CustomEvent('htmx:afterSwap', {
                    detail: { roLivePush: true, roLiveSnapshotTxn: transaction },
                }),
            ),
        ).not.toThrow();
    },
);

test('a delta cannot overwrite a connection replaced by synchronous DOM observers', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? stream.response : pendingFetch(),
    );

    liveApply();
    const generation = requestGeneration(fetchMock);
    stream.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    document.addEventListener(LIST_DELTA_APPLIED_EVENT, () => liveApply(true), { once: true });
    stream.enqueue(sse('ro-live', delta(generation)));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats()).toMatchObject({ state: 'connecting', seq: 0 });
});

test('a reentrant snapshot replacement retires the old reader and its remaining frames', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );
    htmx.swap.mockImplementationOnce((_target, _html, _spec, options) => {
        liveApply(true);
        const eventInfo = (options as { eventInfo: Record<string, unknown> }).eventInfo;
        liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: eventInfo }));
    });

    liveApply();
    const generation = requestGeneration(fetchMock);
    first.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    first.enqueue(sse('ro-live', snapshot(generation)));
    await flush();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(window.roLive.stats()).toMatchObject({ state: 'connecting', seq: 0 });
});

test('a late valid chunk from a replaced reader is inert', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const oldRead = deferred<ReadableStreamReadResult<Uint8Array>>();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? readerResponse(() => oldRead.promise) : pendingFetch(),
    );

    liveApply();
    await flush();
    const generation = requestGeneration(fetchMock);
    liveApply(true);
    const beforeLateChunk = window.roLive.stats();
    oldRead.resolve({
        done: false,
        value: new TextEncoder().encode(sse('ro-live', snapshot(generation))),
    });
    await flush();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(window.roLive.stats()).toMatchObject({
        state: 'connecting',
        seq: 0,
        rawBytes: beforeLateChunk.rawBytes,
        payloadBytes: beforeLateChunk.payloadBytes,
    });
});

test('a late read failure from a replaced reader is inert', async () => {
    renderLivePage();
    const oldRead = deferred<ReadableStreamReadResult<Uint8Array>>();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? readerResponse(() => oldRead.promise) : pendingFetch(),
    );
    const before = window.roLive.stats().reconnects;

    liveApply();
    await flush();
    liveApply(true);
    oldRead.reject(new Error('late read failure'));
    await flush();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(window.roLive.stats()).toMatchObject({ state: 'connecting', reconnects: before });
});

test('a committed snapshot accepts the next delta and advances the cursor after DOM work', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);
    const before = window.roLive.stats();
    content.dataset.roEtag = '"before-snapshot"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';

    liveApply();
    const generation = requestGeneration(fetchMock);
    stream.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    expect(htmx.swap.mock.calls[0][2]).toStrictEqual({ swapStyle: 'morph' });
    expect(content).not.toHaveAttribute('data-ro-etag');
    expect(content).not.toHaveAttribute('data-ro-etag-path');
    content.dataset.roEtag = '"snapshot"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    stream.enqueue(sse('ro-live', delta(generation)));

    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));
    expect(listProjectionRowByKey('dev/a')).toBeNull();
    expect(content).not.toHaveAttribute('data-ro-etag');
    expect(content).not.toHaveAttribute('data-ro-etag-path');
    expect(window.roLive.stats()).toMatchObject({
        state: 'open',
        v2Snapshots: before.v2Snapshots + 1,
        deltas: before.deltas + 1,
        deleted: before.deleted + 1,
    });
});

test('a sequence mismatch resynchronizes without advancing the cursor', async () => {
    renderLivePage();
    installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );

    liveApply();
    const generation = requestGeneration(fetchMock);
    first.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    first.enqueue(sse('ro-live', delta(generation, { seq: 3 })));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats().seq).toBe(0);
});

test('a cursor-matching terminal with a sequence gap resynchronizes', async () => {
    renderLivePage();
    installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();
    const generation = requestGeneration(fetchMock);
    first.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    first.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'terminal',
            g: generation,
            seq: 3,
            rev: 'rev-1',
            schema: 'schema-1',
            reason: 'shutdown',
        }),
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats()).toMatchObject({
        state: 'connecting',
        seq: 0,
        terminals: before.terminals,
        invalidFrames: before.invalidFrames + 1,
    });
});

test('the next-sequence snapshot atomically replaces the current snapshot', async () => {
    const content = renderLivePage();
    installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);
    const before = window.roLive.stats().v2Snapshots;

    liveApply();
    const generation = requestGeneration(fetchMock);
    stream.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    stream.enqueue(
        sse(
            'ro-live',
            snapshot(generation, {
                seq: 2,
                rev: 'rev-2',
                rv: '11',
                snapshot: { html: listHTML('Beta') },
            }),
        ),
    );

    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));
    expect(content.querySelector('[data-key="dev/a"]')).toHaveTextContent('Beta');
    expect(window.roLive.stats().v2Snapshots).toBe(before + 2);
});

test.each([
    ['revision', { rev: 'wrong', schema: 'schema-1' }],
    ['schema', { rev: 'rev-1', schema: 'wrong' }],
] as const)('a terminal with the wrong %s resynchronizes', async (_name, cursorFields) => {
    renderLivePage();
    installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();
    const generation = requestGeneration(fetchMock);
    first.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    first.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'terminal',
            g: generation,
            seq: 2,
            reason: 'shutdown',
            ...cursorFields,
        }),
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats()).toMatchObject({
        terminals: before.terminals,
        invalidFrames: before.invalidFrames + 1,
    });
});

test.each(['shutdown', 'watch-failed', 'lifetime'] as const)(
    'the %s terminal reconnects without a resync',
    async (reason) => {
        vi.spyOn(Math, 'random').mockReturnValue(1);
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);
        const before = window.roLive.stats();

        liveApply();
        const generation = requestGeneration(fetchMock);
        stream.enqueue(sse('ro-live', snapshot(generation)));
        await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
        stream.enqueue(sse('ro-live', terminalFrame(generation, reason)));

        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('reconnecting'));
        expect(window.roLive.stats()).toMatchObject({
            attempt: 1,
            terminals: before.terminals + 1,
            resyncs: before.resyncs,
        });
        expect(dependencies.markLiveUnavailable).not.toHaveBeenCalled();
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
    },
);

test('the auth terminal stops for good and asks for a reload', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);

    liveApply();
    const generation = requestGeneration(fetchMock);
    stream.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    stream.enqueue(sse('ro-live', terminalFrame(generation, 'auth')));

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('unavailable'));
    expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
    expect(fetchMock).toHaveBeenCalledOnce();
});

// A rendering fault is a bug on this side of the wire, not a transport blip:
// it must take the bounded resync path, never the reconnect ladder.
test('a snapshot whose swap throws resynchronizes instead of reconnecting', async () => {
    renderLivePage();
    const harness = installHtmx();
    harness.swap.mockImplementation(() => {
        throw new Error('morph exploded');
    });
    const streams = [controlledStream(), controlledStream()];
    const fetchMock = installFetch(
        async () => streams[fetchMock.mock.calls.length - 1]?.response ?? pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    streams[0].enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));

    // The reopen is immediate and off the ladder: no backoff rung was burned
    // and no stale banner was raised for what is a rendering fault.
    await vi.waitFor(() => expect(window.roLive.stats().resyncs).toBe(before.resyncs + 1));
    expect(window.roLive.stats()).toMatchObject({
        attempt: 0,
        reconnects: before.reconnects,
        seq: before.seq,
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(dependencies.markLiveStale).not.toHaveBeenCalled();
});

test('a decoded delta that cannot cover the final projection resynchronizes', async () => {
    renderLivePage();
    installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async () =>
        fetchMock.mock.calls.length === 1 ? first.response : pendingFetch(),
    );

    liveApply();
    const generation = requestGeneration(fetchMock);
    first.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    first.enqueue(
        sse(
            'ro-live',
            delta(generation, {
                delta: { base: 'rev-1', rev: 'rev-2', order: [] },
            }),
        ),
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats().seq).toBe(0);
});

test('the third protocol failure in one window stops without a retry', async () => {
    renderLivePage();
    const streams = [controlledStream(), controlledStream(), controlledStream()];
    const fetchMock = installFetch(
        async () => streams[fetchMock.mock.calls.length - 1]?.response ?? pendingFetch(),
    );
    const before = window.roLive.stats();

    liveApply();
    for (let index = 0; index < streams.length; index += 1) {
        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(index + 1));
        streams[index].enqueue(sse('other', {}));
    }

    // A protocol fault is a bug on one side of the wire, not a transport blip:
    // the bounded resync budget is the whole recovery, and then it stops.
    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('unavailable'));
    expect(window.roLive.stats()).toMatchObject({
        resyncs: before.resyncs + 2,
        reconnects: before.reconnects,
    });
    expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
    expect(fetchMock).toHaveBeenCalledTimes(3);
});

test('diagnostics expire resyncs at the exact thirty-second boundary', async () => {
    renderLivePage();
    const now = vi.spyOn(Date, 'now').mockReturnValue(100_000);
    const streams = [controlledStream(), controlledStream()];
    const fetchMock = installFetch(
        async () => streams[fetchMock.mock.calls.length - 1]?.response ?? pendingFetch(),
    );

    liveApply();
    streams[0].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    streams[1].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    expect(window.roLive.stats().resyncsInWindow).toBe(2);

    now.mockReturnValue(130_000);
    expect(window.roLive.stats().resyncsInWindow).toBe(0);
});

test('a protocol failure prunes an expired resync budget before deciding to stop', async () => {
    renderLivePage();
    const now = vi.spyOn(Date, 'now').mockReturnValue(100_000);
    const streams = [controlledStream(), controlledStream(), controlledStream()];
    const fetchMock = installFetch(
        async () => streams[fetchMock.mock.calls.length - 1]?.response ?? pendingFetch(),
    );

    liveApply();
    streams[0].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    streams[1].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    now.mockReturnValue(130_000);
    streams[2].enqueue(sse('other', {}));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
    expect(window.roLive.stats()).toMatchObject({ state: 'connecting', resyncsInWindow: 1 });
});

test('an ordinary same-page apply cannot replenish the protocol resync budget', async () => {
    renderLivePage();
    const streams = [controlledStream(), controlledStream(), controlledStream()];
    const fetchMock = installFetch(
        async () => streams[fetchMock.mock.calls.length - 1]?.response ?? pendingFetch(),
    );

    liveApply();
    streams[0].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    streams[1].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    liveApply();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    streams[2].enqueue(sse('other', {}));

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('unavailable'));
    expect(fetchMock).toHaveBeenCalledTimes(3);
});

test('a forced retry explicitly replenishes the protocol resync budget', async () => {
    renderLivePage();
    const streams = [
        controlledStream(),
        controlledStream(),
        controlledStream(),
        controlledStream(),
    ];
    const fetchMock = installFetch(
        async () => streams[fetchMock.mock.calls.length - 1]?.response ?? pendingFetch(),
    );

    liveApply();
    streams[0].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    streams[1].enqueue(sse('other', {}));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    liveApply(true);
    expect(fetchMock).toHaveBeenCalledTimes(4);
    streams[3].enqueue(sse('other', {}));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(5));
    expect(window.roLive.stats().state).toBe('connecting');
});

describe('reconnect schedule and recovery', () => {
    // Full jitter draws the WHOLE window, so pin the roll at 1 and every delay
    // below is exactly its ladder rung -- the schedule is then assertable to
    // the millisecond without re-testing live-policy's own math.
    function pinJitter(): void {
        vi.spyOn(Math, 'random').mockReturnValue(1);
    }

    test.each([
        ['fetch rejection', async () => Promise.reject(new Error('down'))],
        ['a server error', async () => response(500, null)],
        ['a missing response body', async () => response(200, null)],
    ] as const)('%s arms a retry and marks the projection stale', async (_name, result) => {
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(result);
        const before = window.roLive.stats();

        liveApply();

        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('reconnecting'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats()).toMatchObject({
            attempt: 1,
            reconnects: before.reconnects + 1,
            resyncs: before.resyncs,
            invalidFrames: before.invalidFrames,
        });
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
        // The first drop keeps its visual grace: no dim, no banner yet.
        expect(dependencies.revealLiveStale).not.toHaveBeenCalled();
        expect(dependencies.markLiveUnavailable).not.toHaveBeenCalled();
    });

    // Every 4xx except 429 (admission, retryable on its own schedule) and 408
    // (a timeout the next attempt may beat) describes a request this browser
    // cannot fix by replaying it byte for byte: 400/406 mean the handshake
    // headers never arrived intact, 401/403 need a new session, 404 means the
    // server does not offer this stream. 204 is the watchless-kind answer.
    test.each([400, 401, 403, 404, 406, 410, 204] as const)(
        'a %d reply stops for good with the Reload banner',
        async (status) => {
            renderLivePage();
            const fetchMock = installFetch(async () => response(status, null));

            liveApply();

            await vi.waitFor(() => expect(window.roLive.stats().state).toBe('unavailable'));
            expect(fetchMock).toHaveBeenCalledOnce();
            expect(window.roLive.stats().attempt).toBe(0);
            expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
            expect(dependencies.markLiveStale).not.toHaveBeenCalled();
            expect(dependencies.noteStaleRetryAt).toHaveBeenLastCalledWith(0);
        },
    );

    test('a 408 keeps climbing the ladder instead of giving up', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => response(408, null));

        liveApply();
        await flush();

        expect(window.roLive.stats().state).toBe('reconnecting');
        expect(window.roLive.stats().attempt).toBe(1);
        expect(dependencies.markLiveUnavailable).not.toHaveBeenCalled();
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0] as number);
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('a 429 waits exactly as long as its Retry-After', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => response(429, null, { 'Retry-After': '7' }));

        liveApply();
        await flush();
        expect(window.roLive.stats().state).toBe('reconnecting');

        await vi.advanceTimersByTimeAsync(6_999);
        expect(fetchMock).toHaveBeenCalledOnce();
        await vi.advanceTimersByTimeAsync(1);
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('a 429 without a usable Retry-After falls back to the ladder', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () =>
            response(429, null, { 'Retry-After': 'soonish' }),
        );

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0] - 1);
        expect(fetchMock).toHaveBeenCalledOnce();
        await vi.advanceTimersByTimeAsync(1);
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('consecutive failures climb the ladder and flatten at its last rung', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();

        const rungs = [...RECONNECT_DELAY_LADDER_MS, 30_000];
        for (let index = 0; index < rungs.length; index += 1) {
            expect(window.roLive.stats().attempt).toBe(index + 1);
            await vi.advanceTimersByTimeAsync((rungs[index] as number) - 1);
            expect(fetchMock).toHaveBeenCalledTimes(index + 1);
            await vi.advanceTimersByTimeAsync(1);
            await flush();
            expect(fetchMock).toHaveBeenCalledTimes(index + 2);
        }
    });

    test('the first failed retry ends the visual grace early', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();
        expect(dependencies.revealLiveStale).not.toHaveBeenCalled();

        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);
        await flush();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(dependencies.revealLiveStale).toHaveBeenCalledOnce();
    });

    test('a healthy connection restarts the ladder, an unstable one does not', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const recovered = controlledStream();
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length < 3
                ? Promise.reject(new Error('down'))
                : recovered.response,
        );

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);
        await flush();
        expect(window.roLive.stats().attempt).toBe(2);

        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[1]);
        await flush();
        recovered.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock, 2))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');
        // A snapshot alone is not health: the two rungs already climbed stand
        // until the connection has held that snapshot through the healthy window.
        expect(window.roLive.stats().attempt).toBe(2);

        await vi.advanceTimersByTimeAsync(30_000);
        recovered.close();
        await flush();

        expect(window.roLive.stats().attempt).toBe(1);
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);
        expect(fetchMock).toHaveBeenCalledTimes(4);
    });

    test.each([
        [
            'a reader that cannot be acquired',
            () => ({
                getReader() {
                    throw new Error('reader unavailable');
                },
            }),
        ],
        [
            'a failing read',
            () => ({ getReader: () => ({ read: () => Promise.reject(new Error('read failed')) }) }),
        ],
        [
            'a clean EOF',
            () => ({
                getReader: () => ({
                    read: () => Promise.resolve({ done: true, value: undefined }),
                }),
            }),
        ],
    ] as const)('%s reconnects on the ladder', async (_name, makeBody) => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () =>
            response(200, makeBody() as unknown as ReadableStream<Uint8Array>, {
                'Content-Type': 'text/event-stream',
            }),
        );

        liveApply();
        await vi.advanceTimersByTimeAsync(0);
        await flush();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats()).toMatchObject({ state: 'reconnecting', attempt: 1 });
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();

        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('the client waits thirty seconds for the first committed frame', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installFetch(pendingFetch);

        liveApply();
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS - 1);
        expect(window.roLive.stats().state).toBe('connecting');
        await vi.advanceTimersByTimeAsync(1);

        expect(window.roLive.stats().state).toBe('reconnecting');
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
    });

    test('an accepted response without a frame still expires at the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const stream = controlledStream();
        installFetch(async () => stream.response);

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);

        expect(window.roLive.stats().state).toBe('reconnecting');
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
    });

    test('a replaced connection cannot inherit the old first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);

        liveApply();
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS / 2);
        liveApply(true);
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS / 2);

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().state).toBe('connecting');
        expect(dependencies.markLiveStale).not.toHaveBeenCalled();

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS / 2);
        expect(window.roLive.stats().state).toBe('reconnecting');
    });

    test('the first accepted frame trades the first-frame deadline for the idle budget', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');
        expect(dependencies.clearLiveStale).toHaveBeenCalledOnce();

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);
        expect(window.roLive.stats().state).toBe('open');
        expect(dependencies.markLiveStale).not.toHaveBeenCalled();
    });

    // Live is the only update path, so a transport that goes silent WITHOUT
    // closing (half-open TCP: no FIN, no RST) must not leave the page green and
    // frozen. The server heartbeats every 20s; silence past the idle budget is
    // a dead connection.
    test('a silent open stream is reconnected once the idle budget expires', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');

        await vi.advanceTimersByTimeAsync(LIVE_READ_IDLE_TIMEOUT_MS - 1);
        expect(window.roLive.stats().state).toBe('open');
        expect(fetchMock).toHaveBeenCalledOnce();

        await vi.advanceTimersByTimeAsync(1);
        expect(window.roLive.stats().state).toBe('reconnecting');
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
    });

    // A heartbeat carries no event, so only the raw read can renew the budget.
    test('a server heartbeat renews the idle budget without a frame', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');

        for (let beat = 0; beat < 4; beat += 1) {
            await vi.advanceTimersByTimeAsync(LIVE_READ_IDLE_TIMEOUT_MS - 1000);
            stream.enqueue(': heartbeat\n\n');
            await vi.advanceTimersByTimeAsync(0);
            await flush();
        }

        expect(window.roLive.stats().state).toBe('open');
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(dependencies.markLiveStale).not.toHaveBeenCalled();
    });

    test('an early fetch failure clears its obsolete first-frame deadline', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(0);
        // Exactly one timer survives the failure: the armed reconnect.
        expect(vi.getTimerCount()).toBe(1);
    });

    test('a persisted mode change prevents an already-armed retry', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();
        dependencies.isLiveEnabled.mockReturnValue(false);
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('off');
    });

    test('turning Live Off cancels the armed retry', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(0);
        expect(vi.getTimerCount()).toBe(1);
        dependencies.isLiveEnabled.mockReturnValue(false);
        liveApply();
        expect(vi.getTimerCount()).toBe(0);
        await vi.advanceTimersByTimeAsync(60_000);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats()).toMatchObject({ state: 'off', attempt: 0 });
        expect(dependencies.clearLiveStale).toHaveBeenCalled();
    });

    test('a forced retry replaces the armed schedule and restarts the ladder', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);
        await flush();
        expect(window.roLive.stats().attempt).toBe(2);

        liveApply(true);
        await flush();
        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(window.roLive.stats().attempt).toBe(1);
    });

    test('a retry rechecks that the current page still supports Live', async () => {
        vi.useFakeTimers();
        pinJitter();
        const content = renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('down')));

        liveApply();
        await flush();
        content.dataset.liveUrl = 'baked';
        await vi.advanceTimersByTimeAsync(RECONNECT_DELAY_LADDER_MS[0]);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('off');
    });

    test('going offline parks the transport and coming back reconnects once', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);
        const onLine = vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(true);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');

        onLine.mockReturnValue(false);
        window.dispatchEvent(new Event('offline'));
        expect(window.roLive.stats().state).toBe('offline');
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
        // A paused ladder burns no rungs on attempts that cannot succeed.
        await vi.advanceTimersByTimeAsync(300_000);
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().attempt).toBe(0);

        onLine.mockReturnValue(true);
        window.dispatchEvent(new Event('online'));
        window.dispatchEvent(new Event('online'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().state).toBe('connecting');
    });

    test('a failure while offline parks instead of arming a doomed retry', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const onLine = vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(true);
        const fetchMock = installFetch(async () => {
            onLine.mockReturnValue(false);
            return Promise.reject(new Error('down'));
        });

        liveApply();
        await flush();

        expect(window.roLive.stats()).toMatchObject({ state: 'offline', attempt: 0 });
        expect(dependencies.markLiveStale).toHaveBeenCalledOnce();
        await vi.advanceTimersByTimeAsync(300_000);
        expect(fetchMock).toHaveBeenCalledOnce();
    });

    // Turning Live on while already offline must park, not burn a ladder rung
    // on a fetch that cannot succeed.
    test('turning Live on while offline parks without issuing a request', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(false);

        liveApply();

        expect(fetchMock).not.toHaveBeenCalled();
        expect(window.roLive.stats()).toMatchObject({ state: 'offline', attempt: 0 });
        await vi.advanceTimersByTimeAsync(300_000);
        expect(fetchMock).not.toHaveBeenCalled();
    });

    test('an online event does not reopen a stream the user turned off while parked', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);
        const onLine = vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(true);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        onLine.mockReturnValue(false);
        window.dispatchEvent(new Event('offline'));
        expect(window.roLive.stats().state).toBe('offline');

        dependencies.isLiveEnabled.mockReturnValue(false);
        onLine.mockReturnValue(true);
        window.dispatchEvent(new Event('online'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('off');
    });

    // The drop and the toggle race: the reader dies, and the user clicks Live
    // off before the failure path reaches scheduleReconnect. No retry, no
    // stale banner for a feature the user just switched off.
    test('a drop that lands after Live was turned off arms nothing', async () => {
        vi.useFakeTimers();
        pinJitter();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');

        dependencies.isLiveEnabled.mockReturnValue(false);
        stream.close();
        await vi.advanceTimersByTimeAsync(0);
        await flush();

        expect(window.roLive.stats().state).toBe('off');
        expect(window.roLive.stats().attempt).toBe(0);
        expect(vi.getTimerCount()).toBe(0);
        await vi.advanceTimersByTimeAsync(300_000);
        expect(fetchMock).toHaveBeenCalledOnce();
    });

    test('an offline event is inert once the transport has stopped for good', async () => {
        renderLivePage();
        installFetch(async () => response(401, null));

        liveApply();
        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('unavailable'));
        window.dispatchEvent(new Event('offline'));
        window.dispatchEvent(new Event('online'));

        expect(window.roLive.stats().state).toBe('unavailable');
    });
});

test('turning Live off aborts the connection and makes a late response inert', async () => {
    renderLivePage();
    const pending = deferred<Response>();
    const fetchMock = installFetch(() => pending.promise);
    const before = window.roLive.stats().reconnects;

    liveApply();
    const signal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;
    dependencies.isLiveEnabled.mockReturnValue(false);
    liveApply();

    expect(signal.aborted).toBe(true);
    expect(window.roLive.stats()).toMatchObject({ state: 'off', seq: 0 });
    expect(dependencies.clearLiveStale).toHaveBeenCalledOnce();
    pending.reject(new Error('late rejection'));
    await flush();
    expect(window.roLive.stats().reconnects).toBe(before);
    expect(dependencies.markLiveStale).not.toHaveBeenCalled();
});

test('turning Live off makes a late successful response opaque to the retired connection', async () => {
    renderLivePage();
    const pending = deferred<Response>();
    installFetch(() => pending.promise);
    const before = window.roLive.stats();
    let inspected = 0;
    const lateResponse = Object.defineProperties({} as Response, {
        status: {
            get() {
                inspected += 1;
                return 200;
            },
        },
        body: {
            get() {
                inspected += 1;
                return null;
            },
        },
    });

    liveApply();
    dependencies.isLiveEnabled.mockReturnValue(false);
    liveApply();
    pending.resolve(lateResponse);
    await flush();

    expect(inspected).toBe(0);
    expect(window.roLive.stats()).toMatchObject({
        state: 'off',
        reconnects: before.reconnects,
    });
});

// A page that cannot stream is not a failure: the stored Live preference is
// kept and simply does not apply here (a detail page, a watchless kind, a
// multi-cluster list). Nothing is requested and no warning is raised.
test.each(['wrong marker', 'missing option', 'missing content'] as const)(
    '%s is an unsupported Live surface and stays silently off',
    (variant) => {
        const content = renderLivePage();
        const option = document.querySelector(
            '[data-ro-action="toggle-live"]',
        ) as HTMLButtonElement;
        if (variant === 'wrong marker') content.dataset.liveUrl = 'baked';
        if (variant === 'missing option') option.remove();
        if (variant === 'missing content') content.remove();
        const fetchMock = installFetch(pendingFetch);

        liveApply();

        expect(fetchMock).not.toHaveBeenCalled();
        expect(window.roLive.stats().state).toBe('off');
        expect(dependencies.markLiveUnavailable).not.toHaveBeenCalled();
        expect(dependencies.markLiveStale).not.toHaveBeenCalled();
    },
);

test('generation failure stops without issuing a request', () => {
    renderLivePage();
    vi.stubGlobal('crypto', {
        randomUUID: () => {
            throw new Error('UUID unavailable');
        },
        getRandomValues: () => {
            throw new Error('entropy unavailable');
        },
    });
    const fetchMock = installFetch(pendingFetch);

    liveApply();

    expect(fetchMock).not.toHaveBeenCalled();
    expect(window.roLive.stats().state).toBe('unavailable');
    expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
});

test('Live selected while hidden defers its first request until visibility returns', () => {
    renderLivePage();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const fetchMock = installFetch(pendingFetch);

    liveApply();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(window.roLive.stats().state).toBe('hidden');

    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().state).toBe('connecting');
});

describe('one refresh request tracker subscription', () => {
    test('page reset retires the outgoing request tracker with the old Live screen', () => {
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        liveApply();
        startListRequest();

        expect(window.roLive.stats()).toMatchObject({ state: 'suspended', inFlightRequests: 1 });
        liveResetPage();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(dependencies.resetListRequestTracker).toHaveBeenCalledOnce();
        expect(window.roLive.stats()).toMatchObject({ state: 'off', inFlightRequests: 0 });

        startListRequest();
        settleListRequest();
        expect(window.roLive.stats()).toMatchObject({ state: 'off', inFlightRequests: 0 });
    });

    test('a late-canceled user sort resumes the committed base, not its requested base', () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(pendingFetch);
        liveApply();
        const firstSignal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;

        startListRequest();

        expect(firstSignal.aborted).toBe(true);
        expect(window.roLive.stats()).toMatchObject({ state: 'suspended', inFlightRequests: 1 });
        // No non-push afterSwap: a later beforeRequest listener canceled the sort.
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Name');
        expect(window.roLive.stats()).toMatchObject({ state: 'connecting', inFlightRequests: 0 });
    });

    test('a successful non-push list swap commits its URL before settlement', () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(pendingFetch);
        liveApply();

        startListRequest();
        commitListSwap('/clusters/prod/pods?sort=Age');
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Age');
    });

    test('Live picked during an existing canceled request waits on the committed URL', () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        dependencies.snapshot.count = 1;
        const fetchMock = installFetch(pendingFetch);

        liveApply(true);

        expect(fetchMock).not.toHaveBeenCalled();
        expect(window.roLive.stats().state).toBe('suspended');
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(fetchMock.mock.calls[0][0]).toBe('/clusters/prod/pods/_stream?sort=Name');
    });

    test('overlapping requests reopen only after the last settlement', () => {
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        liveApply();

        startListRequest();
        startListRequest();
        commitListSwap('/clusters/prod/pods?sort=Status');
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledOnce();
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Status');
    });

    test('overlapping canceled requests cannot replace the committed base', () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(pendingFetch);
        liveApply();

        startListRequest();
        startListRequest();
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledOnce();
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Name');
    });

    test('a mode change while suspended settles directly to Off', () => {
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        liveApply();
        startListRequest();
        dependencies.isLiveEnabled.mockReturnValue(false);

        settleListRequest();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('off');
        expect(dependencies.clearLiveStale).toHaveBeenCalledOnce();
    });

    test('an armed retry yields to a user request and resumes the committed URL', async () => {
        vi.useFakeTimers();
        vi.spyOn(Math, 'random').mockReturnValue(1);
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1 ? Promise.reject(new Error('down')) : pendingFetch(),
        );
        liveApply();
        await flush();
        expect(window.roLive.stats().state).toBe('reconnecting');

        startListRequest();
        expect(window.roLive.stats().state).toBe('suspended');
        await vi.advanceTimersByTimeAsync(300_000);
        expect(fetchMock).toHaveBeenCalledOnce();

        // The request was cancelled: no swap committed a new URL, so the stream
        // reopens against the one it was already pinned to.
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Name');
    });

    test('a successful swap during an armed retry reopens on the new URL', async () => {
        vi.useFakeTimers();
        vi.spyOn(Math, 'random').mockReturnValue(1);
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1 ? Promise.reject(new Error('down')) : pendingFetch(),
        );
        liveApply();
        await flush();

        startListRequest();
        commitListSwap('/clusters/prod/pods?sort=Age');
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Age');
        expect(window.roLive.stats().state).toBe('connecting');
    });

    test('hide and show during a suspended retry still opens exactly one stream', async () => {
        vi.useFakeTimers();
        vi.spyOn(Math, 'random').mockReturnValue(1);
        renderLivePage();
        const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1 ? Promise.reject(new Error('down')) : pendingFetch(),
        );
        liveApply();
        await flush();

        startListRequest();
        hidden.mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        hidden.mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(window.roLive.stats().state).toBe('suspended');
        expect(fetchMock).toHaveBeenCalledOnce();

        settleListRequest();
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('a recovered reconnect after a swap retires the old schedule', async () => {
        vi.useFakeTimers();
        vi.spyOn(Math, 'random').mockReturnValue(1);
        renderLivePage('/clusters/prod/pods?sort=Name');
        installHtmx();
        const recovered = controlledStream();
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1
                ? Promise.reject(new Error('down'))
                : recovered.response,
        );
        liveApply();
        await flush();
        expect(window.roLive.stats().state).toBe('reconnecting');

        startListRequest();
        commitListSwap('/clusters/prod/pods?sort=Age');
        expect(fetchMock).toHaveBeenCalledOnce();
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Age');
        recovered.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock, 1))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');

        await vi.advanceTimersByTimeAsync(LIVE_READ_IDLE_TIMEOUT_MS - 1);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().state).toBe('open');
    });
});

test('an unsolicited non-push swap cannot hide a route change from Live reconciliation', () => {
    renderLivePage('/clusters/prod/pods?sort=Name');
    const fetchMock = installFetch(pendingFetch);
    liveApply();
    window.history.replaceState(null, '', '/clusters/prod/pods?sort=Age');

    liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: {} }));
    liveApply();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Age');
});

test('a protocol resync observed after the tab hides waits for visibility', async () => {
    renderLivePage();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);

    liveApply();
    const signal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;
    hidden.mockReturnValue(true);
    stream.enqueue(sse('other', {}));
    await flush();

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(signal.aborted).toBe(true);
    expect(window.roLive.stats().state).toBe('hidden');
    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('an unsupported committed list swap stops instead of opening an invented base', () => {
    const content = renderLivePage('/clusters/prod/pods?sort=Name');
    const fetchMock = installFetch(pendingFetch);
    liveApply();
    startListRequest();
    content.dataset.liveUrl = 'baked';

    liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: {} }));
    settleListRequest();

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().state).toBe('off');
});

test('visibility owns one pinned resume intent across a hidden request', () => {
    renderLivePage('/clusters/prod/pods?sort=Name');
    const fetchMock = installFetch(pendingFetch);
    const hidden = vi.spyOn(document, 'hidden', 'get');
    hidden.mockReturnValue(false);
    liveApply();
    const firstSignal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;
    hidden.mockReturnValue(true);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(window.roLive.stats().state).toBe('hidden');
    expect(firstSignal.aborted).toBe(true);

    startListRequest();
    commitListSwap('/clusters/prod/pods?sort=Age');
    settleListRequest();
    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Age');
});

test('visibility events are inert while Off and while an active connection is already visible', () => {
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const fetchMock = installFetch(pendingFetch);

    document.dispatchEvent(new Event('visibilitychange'));
    expect(window.roLive.stats().state).toBe('off');

    renderLivePage();
    hidden.mockReturnValue(false);
    liveApply();
    document.dispatchEvent(new Event('visibilitychange'));
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().state).toBe('connecting');
});

test('becoming visible during an active request remains suspended until settlement', () => {
    renderLivePage();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const fetchMock = installFetch(pendingFetch);
    liveApply();
    hidden.mockReturnValue(true);
    document.dispatchEvent(new Event('visibilitychange'));
    startListRequest();

    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().state).toBe('suspended');

    settleListRequest();
    expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('a visible request carries its committed resume intent through hide and show', () => {
    renderLivePage();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const fetchMock = installFetch(pendingFetch);
    liveApply();

    startListRequest();
    expect(window.roLive.stats().state).toBe('suspended');
    hidden.mockReturnValue(true);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(window.roLive.stats().state).toBe('hidden');

    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().state).toBe('suspended');

    settleListRequest();
    expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('becoming visible after Live is turned Off clears the hidden intent', () => {
    renderLivePage();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const fetchMock = installFetch(pendingFetch);
    liveApply();
    dependencies.isLiveEnabled.mockReturnValue(false);

    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));

    expect(fetchMock).not.toHaveBeenCalled();
    expect(window.roLive.stats().state).toBe('off');
});
