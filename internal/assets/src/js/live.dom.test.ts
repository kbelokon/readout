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
        clearLiveUnavailable: vi.fn(),
        markLiveUnavailable: vi.fn(),
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
    clearLiveUnavailable: dependencies.clearLiveUnavailable,
    markLiveUnavailable: dependencies.markLiveUnavailable,
}));

import {
    adoptListProjection,
    listProjectionRowByKey,
    resetListProjection,
} from './list-projection.js';
import {
    LIVE_FIRST_FRAME_TIMEOUT_MS,
    liveApply,
    liveFallbackSeconds,
    liveOnListSwap,
    liveResetPage,
} from './live.js';
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
    dependencies.clearLiveUnavailable.mockReset();
    dependencies.markLiveUnavailable.mockReset();
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
    expect(dependencies.clearLiveUnavailable).toHaveBeenCalledOnce();
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
    const before = window.roLive.stats().fallbacks;

    liveApply();
    await flush();
    liveApply(true);
    oldRead.reject(new Error('late read failure'));
    await flush();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(window.roLive.stats()).toMatchObject({ state: 'connecting', fallbacks: before });
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
            reason: 'idle',
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
            reason: 'idle',
            ...cursorFields,
        }),
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats()).toMatchObject({
        terminals: before.terminals,
        invalidFrames: before.invalidFrames + 1,
    });
});

test('a cursor-matching terminal enters polling fallback without a resync', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async () => stream.response);
    const before = window.roLive.stats();

    liveApply();
    const generation = requestGeneration(fetchMock);
    stream.enqueue(sse('ro-live', snapshot(generation)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    stream.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'terminal',
            g: generation,
            seq: 2,
            rev: 'rev-1',
            schema: 'schema-1',
            reason: 'idle',
        }),
    );

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));
    expect(window.roLive.stats()).toMatchObject({
        terminals: before.terminals + 1,
        resyncs: before.resyncs,
    });
    expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
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

test('the third protocol failure in one window enters visible polling fallback', async () => {
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

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));
    expect(window.roLive.stats()).toMatchObject({
        resyncs: before.resyncs + 2,
        fallbacks: before.fallbacks + 1,
    });
    expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
    expect(liveFallbackSeconds()).toBe(5);
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

test('a protocol failure prunes an expired resync budget before deciding fallback', async () => {
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

    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));
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

describe('visible fallback and recovery', () => {
    test.each([
        ['fetch rejection', async () => Promise.reject(new Error('offline'))],
        [
            'HTTP rejection',
            async () =>
                response(429, controlledStream().response.body, {
                    'Content-Type': 'text/event-stream',
                }),
        ],
        ['missing response body', async () => response(200, null)],
    ] as const)('%s always reveals the Live-unavailable banner', async (_name, result) => {
        renderLivePage();
        const fetchMock = installFetch(result);
        const before = window.roLive.stats();

        liveApply();

        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats()).toMatchObject({
            resyncs: before.resyncs,
            invalidFrames: before.invalidFrames,
        });
        expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
        expect(liveFallbackSeconds()).toBe(5);
    });

    test('reader acquisition, reader failure, and clean EOF all reveal fallback', async () => {
        renderLivePage();
        const bodies = [
            {
                getReader() {
                    throw new Error('reader unavailable');
                },
            },
            { getReader: () => ({ read: () => Promise.reject(new Error('read failed')) }) },
            {
                getReader: () => ({
                    read: () => Promise.resolve({ done: true, value: undefined }),
                }),
            },
        ];
        const fetchMock = installFetch(async () =>
            response(
                200,
                bodies[fetchMock.mock.calls.length - 1] as unknown as ReadableStream<Uint8Array>,
                { 'Content-Type': 'text/event-stream' },
            ),
        );

        for (let index = 0; index < bodies.length; index += 1) {
            liveApply(true);
            await vi.waitFor(() =>
                expect(dependencies.markLiveUnavailable).toHaveBeenCalledTimes(index + 1),
            );
        }
    });

    test('the client waits thirty seconds for the first committed frame', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installFetch(pendingFetch);

        liveApply();
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS - 1);
        expect(window.roLive.stats().state).toBe('connecting');
        await vi.advanceTimersByTimeAsync(1);

        expect(window.roLive.stats().state).toBe('fallback');
        expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
    });

    test('an accepted response without a frame still expires at the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const stream = controlledStream();
        installFetch(async () => stream.response);

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);

        expect(window.roLive.stats().state).toBe('fallback');
        expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
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
        expect(dependencies.markLiveUnavailable).not.toHaveBeenCalled();

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS / 2);
        expect(window.roLive.stats().state).toBe('fallback');
    });

    test('the first accepted frame permanently clears its connection deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async () => stream.response);

        liveApply();
        stream.enqueue(sse('ro-live', snapshot(requestGeneration(fetchMock))));
        await vi.advanceTimersByTimeAsync(0);
        await flush();
        expect(window.roLive.stats().state).toBe('open');

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);
        expect(window.roLive.stats().state).toBe('open');
        expect(dependencies.markLiveUnavailable).not.toHaveBeenCalled();
    });

    test('an early fetch failure clears its obsolete first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installFetch(async () => Promise.reject(new Error('offline')));
        const before = window.roLive.stats().fallbacks;

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(0);
        expect(window.roLive.stats().fallbacks).toBe(before + 1);
        expect(vi.getTimerCount()).toBe(1);

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);
        expect(window.roLive.stats().fallbacks).toBe(before + 1);
    });

    test('fallback retries after 60s and backs off to 120s after another failure', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));

        liveApply();
        await flush();
        expect(fetchMock).toHaveBeenCalledOnce();
        await vi.advanceTimersByTimeAsync(59_999);
        expect(fetchMock).toHaveBeenCalledOnce();
        await vi.advanceTimersByTimeAsync(1);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        await flush();
        await vi.advanceTimersByTimeAsync(29_999);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        await vi.advanceTimersByTimeAsync(1);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        await vi.advanceTimersByTimeAsync(90_000);
        expect(fetchMock).toHaveBeenCalledTimes(3);
    });

    test('a persisted mode change prevents an already-armed fallback retry', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));

        liveApply();
        await flush();
        dependencies.isLiveEnabled.mockReturnValue(false);
        await vi.advanceTimersByTimeAsync(60_000);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('fallback');
    });

    test('turning Live Off cancels the armed fallback retry', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(0);
        expect(vi.getTimerCount()).toBe(1);
        dependencies.isLiveEnabled.mockReturnValue(false);
        liveApply();
        expect(vi.getTimerCount()).toBe(0);
        await vi.advanceTimersByTimeAsync(60_000);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('off');
    });

    test('a forced fallback retry replaces the old deadline and resets its backoff', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));

        liveApply();
        await flush();
        await vi.advanceTimersByTimeAsync(60_000);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        await flush();
        await vi.advanceTimersByTimeAsync(30_000);

        liveApply(true);
        await flush();
        expect(fetchMock).toHaveBeenCalledTimes(3);

        await vi.advanceTimersByTimeAsync(59_999);
        expect(fetchMock).toHaveBeenCalledTimes(3);
        await vi.advanceTimersByTimeAsync(1);
        expect(fetchMock).toHaveBeenCalledTimes(4);
    });

    test('a retry rechecks that the current page still supports Live', async () => {
        vi.useFakeTimers();
        const content = renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));

        liveApply();
        await flush();
        content.dataset.liveUrl = 'baked';
        await vi.advanceTimersByTimeAsync(60_000);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('fallback');
    });
});

test('turning Live off aborts the connection and makes a late response inert', async () => {
    renderLivePage();
    const pending = deferred<Response>();
    const fetchMock = installFetch(() => pending.promise);
    const before = window.roLive.stats().fallbacks;

    liveApply();
    const signal = fetchMock.mock.calls[0][1]?.signal as AbortSignal;
    dependencies.isLiveEnabled.mockReturnValue(false);
    liveApply();

    expect(signal.aborted).toBe(true);
    expect(window.roLive.stats()).toMatchObject({ state: 'off', seq: 0 });
    expect(dependencies.clearLiveUnavailable).toHaveBeenCalledOnce();
    pending.reject(new Error('late rejection'));
    await flush();
    expect(window.roLive.stats().fallbacks).toBe(before);
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
        fallbacks: before.fallbacks,
    });
});

test.each(['wrong marker', 'missing option', 'disabled option', 'missing content'] as const)(
    '%s is an unsupported Live surface and enters visible fallback',
    (variant) => {
        const content = renderLivePage();
        const option = document.querySelector(
            '[data-ro-action="toggle-live"]',
        ) as HTMLButtonElement;
        if (variant === 'wrong marker') content.dataset.liveUrl = 'baked';
        if (variant === 'missing option') option.remove();
        if (variant === 'disabled option') option.disabled = true;
        if (variant === 'missing content') content.remove();
        const fetchMock = installFetch(pendingFetch);

        liveApply();

        expect(fetchMock).not.toHaveBeenCalled();
        expect(window.roLive.stats().state).toBe('fallback');
        expect(dependencies.markLiveUnavailable).toHaveBeenCalledOnce();
        expect(liveFallbackSeconds()).toBe(variant === 'missing content' ? 0 : 5);
    },
);

test('generation failure enters visible fallback without issuing a request', () => {
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
    expect(window.roLive.stats().state).toBe('fallback');
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
        expect(dependencies.clearLiveUnavailable).toHaveBeenCalledOnce();
    });

    test('a fallback retry waits for a request and resumes the committed URL after cancellation', async () => {
        vi.useFakeTimers();
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1
                ? Promise.reject(new Error('offline'))
                : pendingFetch(),
        );
        liveApply();
        await flush();
        startListRequest();

        await vi.advanceTimersByTimeAsync(60_000);
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('suspended');
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Name');
    });

    test('ordinary fallback polling never creates a resume intent', async () => {
        renderLivePage();
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));
        liveApply();
        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));

        startListRequest();
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('fallback');
    });

    test('hide and show cannot promote a fallback poll into a Live reopen', async () => {
        renderLivePage();
        const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));
        liveApply();
        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));

        startListRequest();
        hidden.mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        hidden.mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));

        expect(window.roLive.stats().state).toBe('fallback');
        settleListRequest();
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('fallback');
    });

    test('a successful same-base fallback poll stays on its scheduled retry', async () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(async () => Promise.reject(new Error('offline')));
        liveApply();
        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));

        startListRequest();
        commitListSwap('/clusters/prod/pods?sort=Name');
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().state).toBe('fallback');
    });

    test('a changed fallback swap survives an earlier overlapping settlement', async () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1
                ? Promise.reject(new Error('offline'))
                : pendingFetch(),
        );
        liveApply();
        await vi.waitFor(() => expect(window.roLive.stats().state).toBe('fallback'));

        startListRequest();
        startListRequest();
        settleListRequest();
        commitListSwap('/clusters/prod/pods?sort=Age');
        expect(fetchMock).toHaveBeenCalledOnce();
        settleListRequest();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe('/clusters/prod/pods/_stream?sort=Age');
        expect(window.roLive.stats().state).toBe('connecting');
    });

    test('a successful changed-base fallback poll reopens and retires the old retry', async () => {
        vi.useFakeTimers();
        renderLivePage('/clusters/prod/pods?sort=Name');
        installHtmx();
        const recovered = controlledStream();
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1
                ? Promise.reject(new Error('offline'))
                : recovered.response,
        );
        liveApply();
        await flush();
        expect(window.roLive.stats().state).toBe('fallback');

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

        await vi.advanceTimersByTimeAsync(60_000);
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

test('an unsupported committed list swap falls back instead of opening an invented base', () => {
    const content = renderLivePage('/clusters/prod/pods?sort=Name');
    const fetchMock = installFetch(pendingFetch);
    liveApply();
    startListRequest();
    content.dataset.liveUrl = 'baked';

    liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: {} }));
    settleListRequest();

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().state).toBe('fallback');
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
