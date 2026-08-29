// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    markListStale: vi.fn(),
    refreshMode: vi.fn(() => 'Live'),
    scheduleRefreshTick: vi.fn(),
}));

vi.mock('./refresh.js', () => ({
    refreshMode: dependencies.refreshMode,
    scheduleRefreshTick: dependencies.scheduleRefreshTick,
}));
vi.mock('./stale.js', () => ({ markListStale: dependencies.markListStale }));

import {
    adoptListProjection,
    listProjectionRowByKey,
    resetListProjection,
} from './list-projection.js';
import {
    LIVE_FIRST_FRAME_TIMEOUT_MS,
    liveAfterListRequest,
    liveApply,
    liveBeforeListRequest,
    liveBeforeListSwapDecision,
    liveFallbackSeconds,
    liveListRequestSwapFailed,
    liveMarkListRequestSent,
    liveOnListSwap,
    liveResetPage,
    liveState,
    liveTeardown,
} from './live.js';
import { isClientLiveGeneration } from './live-url.js';

interface HtmxHarness {
    swap: ReturnType<typeof vi.fn>;
}

interface ControlledStream {
    close(): void;
    enqueue(value: string | Uint8Array): void;
    headers: Headers;
    response: Response;
}

function renderLivePage(path = '/clusters/prod/pods'): HTMLElement {
    window.history.replaceState(null, '', path);
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.dataset.liveUrl = 'location';
    const option = document.createElement('button');
    option.dataset.roAction = 'set-refresh';
    option.dataset.roInterval = 'Live';
    const bulk = document.createElement('div');
    bulk.id = 'ro-bulkbar';
    bulk.setAttribute('inert', '');
    bulk.innerHTML = '<span id="ro-bulk-count">0 selected</span>';
    document.body.append(content, option, bulk);
    return content;
}

function listHTML(name = 'Alpha', status = 'Ready'): string {
    return `
        <input id="ro-filter-input" value="">
        <div class="ro-table-wrap" tabindex="0">
            <table class="ro-table">
                <thead><tr><th data-hint="string">Name</th><th data-hint="enum">Status</th></tr></thead>
                <tbody>
                    <tr id="row-dev/a" data-key="dev/a" data-name="${name}">
                        <td class="cell-name"><a href="#a">${name}</a></td><td>${status}</td>
                    </tr>
                </tbody>
            </table>
        </div>
        <div class="ro-cardlist"><div class="ro-pcard" data-key="dev/a"><a href="#a-card">${name} card</a></div></div>
        <span class="ro-count" data-ro-live-region="count">1</span>
        <div class="ro-phase-strip" data-ro-live-region="phase" hidden></div>
        <span class="ro-foundline" data-ro-live-region="found">1 found</span>
        <div id="ro-live-status" role="status" aria-live="polite"></div>`;
}

function rowHTML(name: string, status = 'Ready'): string {
    return `<tr id="row-dev/a" data-key="dev/a" data-name="${name}"><td class="cell-name"><a href="#a">${name}</a></td><td>${status}</td></tr>`;
}

function cardHTML(name: string): string {
    return `<div class="ro-pcard" data-key="dev/a"><a href="#a-card">${name} card</a></div>`;
}

function installHtmx(
    options: {
        completeSnapshot?: boolean;
        mapEventInfo?: (eventInfo: Record<string, unknown>) => Record<string, unknown>;
        mutate?: boolean;
    } = {},
): HtmxHarness {
    const completeSnapshot = options.completeSnapshot ?? true;
    const mutate = options.mutate ?? true;
    const swap = vi.fn(
        (
            target: Element,
            html: string,
            _swapSpec: unknown,
            swapOptions: { eventInfo: Record<string, unknown> },
        ) => {
            if (mutate) {
                target.innerHTML = html;
                if (target.querySelector('tbody tr[data-key]')) adoptListProjection(target);
            }
            if (completeSnapshot) {
                const eventInfo = options.mapEventInfo
                    ? options.mapEventInfo(swapOptions.eventInfo)
                    : swapOptions.eventInfo;
                liveOnListSwap(
                    new CustomEvent('htmx:afterSwap', {
                        detail: { ...eventInfo, target },
                    }),
                );
            }
        },
    );
    const htmx = { swap };
    (window as unknown as { htmx: HtmxHarness }).htmx = htmx;
    return htmx;
}

function controlledStream(
    headers: HeadersInit = { 'Content-Type': 'text/event-stream' },
): ControlledStream {
    const encoder = new TextEncoder();
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
        start(value) {
            controller = value;
        },
    });
    const responseHeaders = new Headers(headers);
    return {
        close: () => controller.close(),
        enqueue: (value) =>
            controller.enqueue(typeof value === 'string' ? encoder.encode(value) : value),
        headers: responseHeaders,
        response: { status: 200, body, headers: responseHeaders } as unknown as Response,
    };
}

function response(
    parts: Array<string | Uint8Array>,
    status = 200,
    headers: HeadersInit = { 'Content-Type': 'text/event-stream' },
): Response {
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
        start(controller) {
            for (const part of parts) {
                controller.enqueue(typeof part === 'string' ? encoder.encode(part) : part);
            }
            controller.close();
        },
    });
    return { status, body, headers: new Headers(headers) } as unknown as Response;
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

function requestHeaders(init?: RequestInit): Record<string, string> {
    return init?.headers as Record<string, string>;
}

function sse(name: string, payload: unknown): string {
    return `event: ${name}\ndata: ${JSON.stringify(payload)}\n\n`;
}

function snapshot(g: string, overrides: Record<string, unknown> = {}) {
    return {
        v: 2,
        kind: 'snapshot',
        g,
        seq: 1,
        screen: '/clusters/prod/pods',
        rev: 'rev-1',
        rv: '10',
        schema: 'schema-1',
        snapshot: { html: listHTML() },
        ...overrides,
    };
}

function xhr(status = 200): XMLHttpRequest {
    const target = new EventTarget() as XMLHttpRequest;
    Object.defineProperties(target, {
        readyState: { configurable: true, value: 1, writable: true },
        status: { configurable: true, value: status, writable: true },
    });
    return target;
}

function htmxRequest(type: string, content: Element, request: XMLHttpRequest, extra = {}): Event {
    return new CustomEvent(type, {
        detail: { target: content, xhr: request, ...extra },
    });
}

async function flush(): Promise<void> {
    await Promise.resolve();
    await Promise.resolve();
}

beforeEach(() => {
    liveTeardown();
    liveResetPage();
    liveState.status = 'idle';
    liveState.abort = null;
    liveState.gen = '';
    liveState.streamPath = '';
    dependencies.refreshMode.mockReset().mockReturnValue('Live');
    dependencies.scheduleRefreshTick.mockReset();
    dependencies.markListStale.mockReset();
    document.body.replaceChildren();
    resetListProjection();
    delete (window as unknown as { htmx?: HtmxHarness }).htmx;
});

afterEach(async () => {
    liveTeardown();
    liveResetPage();
    vi.useRealTimers();
    await flush();
});

test('opens with one valid UUID/hex generation, raw query cleanup, and both negotiation headers', () => {
    renderLivePage('/clusters/prod/pods?g=old&f=status:Running,Pending&%67=older&x=%ZZ');
    const fetchMock = installFetch(pendingFetch);
    const before = window.roLive.stats().connections;

    liveApply();

    expect(isClientLiveGeneration(liveState.gen)).toBe(true);
    expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?f=status:Running,Pending&x=%ZZ');
    expect(fetchMock).toHaveBeenCalledExactlyOnceWith(
        `${liveState.streamPath}&g=${liveState.gen}`,
        {
            signal: liveState.abort?.signal,
            headers: {
                'RO-Live-Version': '2',
                'RO-Live-Generation': liveState.gen,
            },
        },
    );
    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().connections).toBe(before + 1);
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();

    liveApply();
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(window.roLive.stats().connections).toBe(before + 1);
    const first = liveState.abort;
    liveApply(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(first?.signal.aborted).toBe(true);
    expect(window.roLive.stats().connections).toBe(before + 2);
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledTimes(2);
});

test('ordinary reconciliation replaces a connection when the route changes', () => {
    renderLivePage('/clusters/prod/pods?sort=Name');
    const fetchMock = installFetch(pendingFetch);
    liveApply();
    const first = liveState.abort;

    window.history.replaceState(null, '', '/clusters/prod/pods?sort=Age');
    liveApply();

    expect(first?.signal.aborted).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toMatch(/^\/clusters\/prod\/pods\/_stream\?sort=Age&g=/u);
    expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?sort=Age');
});

test('turning Live Off aborts a pending fetch and makes its late response inert', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const pending = deferred<Response>();
    const fetchMock = installFetch(() => pending.promise);
    const fallbacks = window.roLive.stats().fallbacks;

    liveApply();
    const generation = liveState.gen;
    const ctrl = liveState.abort;
    expect(fetchMock).toHaveBeenCalledOnce();

    dependencies.refreshMode.mockReturnValue('Off');
    liveApply();

    expect(ctrl?.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveState.status).toBe('idle');
    expect(liveState.streamPath).toBe('');
    expect(window.roLive.stats()).toMatchObject({ protocol: null, inFlightRequests: 0 });

    pending.resolve(response([sse('ro-table', { g: generation, html: '<p>late</p>' })]));
    await flush();

    expect(htmx.swap).not.toHaveBeenCalled();
    expect(liveState.status).toBe('idle');
    expect(liveState.abort).toBeNull();
    expect(window.roLive.stats().fallbacks).toBe(fallbacks);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('an unsupported page and a non-Live preference use sticky polling/idle states', () => {
    const content = renderLivePage();
    const fetchMock = installFetch(pendingFetch);
    const before = window.roLive.stats().fallbacks;
    content.dataset.liveUrl = 'baked';

    liveApply();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(liveState.status).toBe('fallback');
    expect(liveFallbackSeconds()).toBe(5);
    expect(window.roLive.stats().fallbacks).toBe(before + 1);
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();
    expect(dependencies.markListStale).not.toHaveBeenCalled();

    dependencies.refreshMode.mockReturnValue('Off');
    liveApply();
    expect(liveState.status).toBe('idle');
    expect(liveState.streamPath).toBe('');
    expect(liveFallbackSeconds()).toBe(0);
});

test.each(['missing content', 'wrong marker', 'missing option', 'disabled option'] as const)(
    '%s is an unsupported Live surface',
    (variant) => {
        const content = variant === 'missing content' ? null : renderLivePage();
        if (variant === 'wrong marker' && content) content.dataset.liveUrl = 'baked';
        const option = document.querySelector(
            '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
        ) as HTMLButtonElement | null;
        if (variant === 'missing option') option?.remove();
        if (variant === 'disabled option' && option) option.disabled = true;
        const fetchMock = installFetch(pendingFetch);

        liveApply();

        expect(fetchMock).not.toHaveBeenCalled();
        expect(liveState.status).toBe('fallback');
        expect(liveFallbackSeconds()).toBe(content ? 5 : 0);
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    },
);

test('generation failure enters silent fallback before any request is issued', () => {
    renderLivePage();
    const fetchMock = installFetch(pendingFetch);
    vi.spyOn(window.crypto, 'randomUUID').mockImplementation(() => {
        throw new Error('UUID unavailable');
    });
    vi.spyOn(window.crypto, 'getRandomValues').mockImplementation(() => {
        throw new Error('entropy unavailable');
    });

    liveApply();

    expect(fetchMock).not.toHaveBeenCalled();
    expect(liveState.status).toBe('fallback');
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('an active fetch failure falls back, while a superseded failure is inert', async () => {
    renderLivePage();
    const first = deferred<Response>();
    const second = deferred<Response>();
    const fetchMock = installFetch(() =>
        fetchMock.mock.calls.length === 1 ? first.promise : second.promise,
    );

    liveApply();
    const firstController = liveState.abort;
    liveApply(true);
    const secondController = liveState.abort;
    first.reject(new Error('superseded'));
    await flush();

    expect(firstController?.signal.aborted).toBe(true);
    expect(secondController?.signal.aborted).toBe(false);
    expect(liveState.status).toBe('connecting');

    second.reject(new Error('active'));
    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('a superseded successful response cannot overwrite its replacement connection', async () => {
    renderLivePage();
    const first = deferred<Response>();
    const fetchMock = installFetch(() =>
        fetchMock.mock.calls.length === 1 ? first.promise : pendingFetch(),
    );

    liveApply();
    const firstController = liveState.abort;
    liveApply(true);
    const replacement = liveState.abort;
    first.resolve(response([], 429));
    await flush();

    expect(firstController?.signal.aborted).toBe(true);
    expect(replacement?.signal.aborted).toBe(false);
    expect(liveState.abort).toBe(replacement);
    expect(liveState.status).toBe('connecting');
    expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('normalizes the event-stream media type before negotiating legacy v1', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream({
        'Content-Type': ' Text/Event-Stream ; charset=utf-8',
    });
    installFetch(async () => stream.response);

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
    stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>normalized</p>' }));

    await vi.waitFor(() => expect(liveState.status).toBe('open-v1'));
    expect(document.getElementById('resource-list-content')?.innerHTML).toBe('<p>normalized</p>');
});

test('a throwing response-header surface is a counted protocol failure', async () => {
    renderLivePage();
    const before = window.roLive.stats().invalidFrames;
    const body = new ReadableStream<Uint8Array>();
    const fetchMock = installFetch(async () => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        return {
            status: 200,
            body,
            headers: {
                get() {
                    throw new Error('blocked headers');
                },
            },
        } as unknown as Response;
    });

    liveApply();

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats().invalidFrames).toBe(before + 1);
    expect(liveState.status).toBe('connecting');
});

test.each([
    [
        'reader acquisition',
        {
            getReader() {
                throw new Error('no reader');
            },
        },
    ],
    [
        'reader read',
        {
            getReader() {
                return { read: () => Promise.reject(new Error('read failed')) };
            },
        },
    ],
] as const)('%s failure enters banner fallback', async (_name, body) => {
    renderLivePage();
    installFetch(
        async () =>
            ({
                status: 200,
                body,
                headers: new Headers({ 'Content-Type': 'text/event-stream' }),
            }) as unknown as Response,
    );

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('a reentrant reader-acquisition failure cannot poison its replacement connection', async () => {
    renderLivePage();
    const before = window.roLive.stats().fallbacks;
    const fetchMock = installFetch(async () => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        return {
            status: 200,
            body: {
                getReader() {
                    liveApply(true);
                    throw new Error('old reader unavailable');
                },
            },
            headers: new Headers({ 'Content-Type': 'text/event-stream' }),
        } as unknown as Response;
    });

    liveApply();

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    const replacement = liveState.abort;
    expect(replacement).not.toBeNull();
    expect(replacement?.signal.aborted).toBe(false);
    expect(liveState.abort).toBe(replacement);
    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().fallbacks).toBe(before);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('a clean event-stream EOF without a frame enters banner fallback', async () => {
    renderLivePage();
    installFetch(async () => response([]));
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('a superseded pending reader result is inert', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const read = deferred<ReadableStreamReadResult<Uint8Array>>();
    const fetchMock = installFetch(async () => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        return {
            status: 200,
            body: { getReader: () => ({ read: () => read.promise }) },
            headers: new Headers({ 'Content-Type': 'text/event-stream' }),
        } as unknown as Response;
    });
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
    const oldGeneration = liveState.gen;

    liveApply(true);
    const replacement = liveState.abort;
    read.resolve({
        done: false,
        value: new TextEncoder().encode(
            sse('ro-table', { g: oldGeneration, html: '<p>old reader</p>' }),
        ),
    });
    await flush();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(liveState.abort).toBe(replacement);
    expect(replacement?.signal.aborted).toBe(false);
    expect(liveState.status).toBe('connecting');
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('a superseded reader rejection is inert', async () => {
    renderLivePage();
    const read = deferred<ReadableStreamReadResult<Uint8Array>>();
    const fetchMock = installFetch(async () => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        return {
            status: 200,
            body: { getReader: () => ({ read: () => read.promise }) },
            headers: new Headers({ 'Content-Type': 'text/event-stream' }),
        } as unknown as Response;
    });
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));

    liveApply(true);
    const replacement = liveState.abort;
    read.reject(new Error('old reader'));
    await flush();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(liveState.abort).toBe(replacement);
    expect(replacement?.signal.aborted).toBe(false);
    expect(liveState.status).toBe('connecting');
});

test('an unversioned text/event-stream stays on the legacy v1 lane', async () => {
    const content = renderLivePage();
    content.dataset.roEtag = '"old"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);
    const before = window.roLive.stats();

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
    const payload = { g: liveState.gen, html: '<p>legacy</p>' };
    const wire = sse('ro-table', payload);
    stream.enqueue(wire);

    await vi.waitFor(() => expect(liveState.status).toBe('open-v1'));
    expect(htmx.swap).toHaveBeenCalledExactlyOnceWith(
        content,
        '<p>legacy</p>',
        { swapStyle: 'morph' },
        {
            contextElement: content,
            eventInfo: { target: content, roLivePush: true },
        },
    );
    expect(content.dataset.roEtag).toBeUndefined();
    const after = window.roLive.stats();
    expect(after.v1Snapshots).toBe(before.v1Snapshots + 1);
    expect(after.rawBytes).toBe(before.rawBytes + new TextEncoder().encode(wire).byteLength);
    expect(after.payloadBytes).toBe(
        before.payloadBytes + new TextEncoder().encode(JSON.stringify(payload)).byteLength,
    );
    expect(after.snapshotBytes).toBe(
        before.snapshotBytes + new TextEncoder().encode(JSON.stringify(payload)).byteLength,
    );
});

test('exact v2 response headers negotiate and commit the first seq=1 snapshot after its marker', async () => {
    const content = renderLivePage();
    content.dataset.roEtag = '"old"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    let seqInsideSwap = -1;
    const htmx = installHtmx();
    htmx.swap.mockImplementationOnce((target, html, _spec, options) => {
        expect(content.dataset.roEtag).toBeUndefined();
        seqInsideSwap = window.roLive.stats().seq;
        target.innerHTML = html;
        adoptListProjection(target);
        liveOnListSwap(
            new CustomEvent('htmx:afterSwap', {
                detail: { ...options.eventInfo, target },
            }),
        );
    });
    const stream = controlledStream();
    const before = window.roLive.stats();
    installFetch(async (_url, init) => {
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    const payload = snapshot(liveState.gen);
    const wire = sse('ro-live', payload);
    stream.enqueue(wire);

    await vi.waitFor(() => expect(liveState.status).toBe('open-v2'));
    expect(seqInsideSwap).toBe(0);
    const after = window.roLive.stats();
    expect(after.seq).toBe(1);
    expect(after.v2Snapshots).toBe(before.v2Snapshots + 1);
    expect(after.rawBytes).toBe(before.rawBytes + new TextEncoder().encode(wire).byteLength);
    expect(after.payloadBytes).toBe(
        before.payloadBytes + new TextEncoder().encode(JSON.stringify(payload)).byteLength,
    );
    expect(after.snapshotBytes).toBe(
        before.snapshotBytes + new TextEncoder().encode(JSON.stringify(payload)).byteLength,
    );
    expect(htmx.swap.mock.calls[0][2]).toStrictEqual({ swapStyle: 'morph' });
});

test.each([
    ['version without generation', { 'RO-Live-Version': '2' }],
    ['generation without version', { 'RO-Live-Generation': 'wrong' }],
    ['unsupported version', { 'RO-Live-Version': '3', 'RO-Live-Generation': 'echo' }],
    ['mismatched generation', { 'RO-Live-Version': '2', 'RO-Live-Generation': 'wrong' }],
])('%s is a protocol failure and starts one resync', async (_name, responseHeaders) => {
    renderLivePage();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const headers = new Headers({ 'Content-Type': 'text/event-stream', ...responseHeaders });
        if (headers.get('RO-Live-Generation') === 'echo') {
            headers.set('RO-Live-Generation', requestHeaders(init)['RO-Live-Generation']);
        }
        return response([], 200, headers);
    });

    liveApply();

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats().resyncsInWindow).toBe(1);
    expect(liveState.status).toBe('connecting');
});

test.each([
    ['bad status', response([], 429)],
    ['missing body', { status: 200, body: null, headers: new Headers() } as unknown as Response],
])('%s enters silent fallback', async (_name, result) => {
    renderLivePage();
    installFetch(async () => result);
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(dependencies.markListStale).not.toHaveBeenCalled();
    expect(liveFallbackSeconds()).toBe(5);
});

test('legacy parsing survives comments, CR variants, split emoji, malformed JSON and stale frames', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);
    const beforeDiscards = window.roLive.discards();
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));

    const rejectedWire = new TextEncoder().encode(
        `: heartbeat\revent: ro-table\rdata: {broken}\r\r${sse('ro-table', {
            g: 'stale',
            html: '<p>stale</p>',
        })}`,
    );
    stream.enqueue(rejectedWire);

    await vi.waitFor(() => expect(window.roLive.discards()).toBe(beforeDiscards + 1));
    expect(window.roLive.stats().discards).toBe(beforeDiscards + 1);
    expect(htmx.swap).not.toHaveBeenCalled();

    const acceptedWire = new TextEncoder().encode(
        sse('ro-table', { g: liveState.gen, html: '<p>🫠</p>' }),
    );
    const emoji = Array.from(acceptedWire).findIndex((byte) => byte >= 0xf0);
    stream.enqueue(acceptedWire.slice(0, emoji + 2));
    stream.enqueue(acceptedWire.slice(emoji + 2));

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(htmx.swap.mock.calls[0][1]).toBe('<p>🫠</p>');
    expect(window.roLive.stats().invalidFrames).toBeGreaterThan(0);
});

test('a hostile legacy terminal reason is invalid and does not stop a following snapshot', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);
    const before = window.roLive.stats();
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));

    stream.enqueue(
        `${sse('ro-terminal', {
            g: liveState.gen,
            reason: { toString: null, valueOf: null },
        })}${sse('ro-table', { g: liveState.gen, html: '<p>still live</p>' })}`,
    );

    await vi.waitFor(() => expect(liveState.status).toBe('open-v1'));
    expect(window.roLive.stats().invalidFrames).toBe(before.invalidFrames + 1);
    expect(window.roLive.stats().v1Snapshots).toBe(before.v1Snapshots + 1);
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(htmx.swap.mock.calls[0][1]).toBe('<p>still live</p>');
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('a current-generation legacy frame is discarded after the URL base changes', async () => {
    renderLivePage('/clusters/prod/pods?sort=Name');
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);
    const before = window.roLive.discards();
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));

    window.history.replaceState(null, '', '/clusters/prod/pods?sort=Age');
    stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>wrong page</p>' }));

    await vi.waitFor(() => expect(window.roLive.discards()).toBe(before + 1));
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(liveState.status).toBe('syncing-v1');

    window.history.replaceState(null, '', '/clusters/prod/pods?sort=Name');
    stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>current page</p>' }));
    await vi.waitFor(() => expect(liveState.status).toBe('open-v1'));
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(htmx.swap.mock.calls[0][1]).toBe('<p>current page</p>');
});

test.each(['idle', 'auth', 'watch-failed', 'shutdown'])(
    'valid legacy terminal reason %s stops the stream and shows stale fallback',
    async (reason) => {
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        installFetch(async () => stream.response);
        const before = window.roLive.stats().terminals;
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
        stream.enqueue(sse('ro-terminal', { g: liveState.gen, reason }));
        await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
        expect(window.roLive.stats().terminals).toBe(before + 1);
        expect(dependencies.markListStale).toHaveBeenCalledOnce();
    },
);

test('invalid legacy records are counted and ignored until a valid table arrives', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);
    const before = window.roLive.stats().invalidFrames;
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
    stream.enqueue(
        [
            sse('ro-table', null),
            sse('ro-table', []),
            sse('ro-table', 7),
            sse('ro-table', { g: liveState.gen }),
            sse('ro-table', { g: 7, html: '<p>bad</p>' }),
            sse('ro-terminal', { g: 'stale', reason: 'auth' }),
            sse('ro-terminal', { g: liveState.gen, reason: 'unknown' }),
            sse('extension', { ignored: true }),
            sse('ro-table', { g: liveState.gen, html: '<p>valid</p>' }),
        ].join(''),
    );

    await vi.waitFor(() => expect(liveState.status).toBe('open-v1'));
    expect(window.roLive.stats().invalidFrames).toBe(before + 7);
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(htmx.swap.mock.calls[0][1]).toBe('<p>valid</p>');
});

test.each(['v1', 'v2'] as const)(
    'invalid UTF-8 on %s fails closed and is counted',
    async (lane) => {
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            if (lane === 'v2') {
                stream.headers.set('RO-Live-Version', '2');
                stream.headers.set(
                    'RO-Live-Generation',
                    requestHeaders(init)['RO-Live-Generation'],
                );
            }
            return stream.response;
        });
        const before = window.roLive.stats().invalidFrames;
        liveApply();
        await vi.waitFor(() =>
            expect(liveState.status).toBe(lane === 'v2' ? 'syncing-v2' : 'syncing-v1'),
        );
        stream.enqueue(Uint8Array.of(0x64, 0x61, 0x74, 0x61, 0x3a, 0xc3, 0x0a));

        if (lane === 'v2') {
            await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
            expect(liveState.status).toBe('connecting');
        } else {
            await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
            expect(dependencies.markListStale).toHaveBeenCalledOnce();
        }
        expect(window.roLive.stats().invalidFrames).toBe(before + 1);
    },
);

test.each(['missing htmx', 'non-callable swap', 'throwing swap', 'missing content'] as const)(
    '%s makes a legacy snapshot fail closed',
    async (failure) => {
        const content = renderLivePage();
        if (failure === 'non-callable swap') {
            (window as unknown as { htmx: { swap: unknown } }).htmx = { swap: null };
        } else if (failure === 'throwing swap') {
            installHtmx().swap.mockImplementation(() => {
                throw new Error('swap failed');
            });
        } else if (failure !== 'missing htmx') {
            installHtmx();
        }
        const stream = controlledStream();
        installFetch(async () => stream.response);
        const before = window.roLive.stats().invalidFrames;
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
        if (failure === 'missing content') content.remove();
        stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>snapshot</p>' }));

        await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
        expect(window.roLive.stats().invalidFrames).toBe(before + 1);
        expect(dependencies.markListStale).toHaveBeenCalledOnce();
    },
);

test.each(['delta', 'terminal'] as const)(
    'a first v2 %s frame is rejected before DOM/cursor',
    async (kind) => {
        renderLivePage();
        const htmx = installHtmx();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            const envelope =
                kind === 'delta'
                    ? {
                          v: 2,
                          kind,
                          g: generation,
                          seq: 1,
                          screen: '/clusters/prod/pods',
                          rev: 'rev-2',
                          schema: 'schema-1',
                          delta: {
                              base: 'rev-1',
                              rev: 'rev-2',
                              regions: [
                                  {
                                      region: 'count',
                                      html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                                  },
                              ],
                          },
                      }
                    : {
                          v: 2,
                          kind,
                          g: generation,
                          seq: 1,
                          screen: '/clusters/prod/pods',
                          reason: 'idle',
                      };
            return response([sse('ro-live', envelope)], 200, {
                'Content-Type': 'text/event-stream',
                'RO-Live-Version': '2',
                'RO-Live-Generation': generation,
            });
        });

        liveApply();
        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
        expect(htmx.swap).not.toHaveBeenCalled();
        expect(window.roLive.stats().seq).toBe(0);
    },
);

test.each([
    ['wrong SSE event name', 'other', snapshot('placeholder')],
    ['malformed JSON', 'ro-live', '{broken'],
    ['invalid envelope', 'ro-live', { v: 2, kind: 'snapshot' }],
    ['initial snapshot sequence gap', 'ro-live', snapshot('placeholder', { seq: 2 })],
] as const)('%s rejects v2 before publishing a cursor', async (_case, eventName, payload) => {
    renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    const before = window.roLive.stats().invalidFrames;
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    const value =
        typeof payload === 'string'
            ? payload
            : JSON.stringify({
                  ...payload,
                  g:
                      'g' in payload && payload.g === 'placeholder'
                          ? liveState.gen
                          : 'g' in payload
                            ? payload.g
                            : undefined,
              });
    stream.enqueue(`event: ${eventName}\ndata: ${value}\n\n`);

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(window.roLive.stats().seq).toBe(0);
    expect(window.roLive.stats().invalidFrames).toBe(before + 1);
    expect(htmx.swap).not.toHaveBeenCalled();
});

test('a rejected v2 event stops every remaining event from its old reader', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    const before = window.roLive.stats();
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));

    stream.enqueue(`${sse('wrong', {})}${sse('ro-live', snapshot(liveState.gen, { seq: 1 }))}`);

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().invalidFrames).toBe(before.invalidFrames + 1);
    expect(window.roLive.stats().resyncs).toBe(before.resyncs + 1);
    expect(window.roLive.stats().payloadBytes).toBe(before.payloadBytes + 2);
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test.each([
    [
        'generation',
        (generation: string) => ({
            ...snapshot(generation),
            kind: 'terminal',
            seq: 2,
            g: '00112233445566778899aabbccddeeff',
            reason: 'idle',
            snapshot: undefined,
        }),
    ],
    [
        'screen',
        (generation: string) => ({
            ...snapshot(generation),
            kind: 'terminal',
            seq: 2,
            screen: '/clusters/prod/services',
            reason: 'idle',
            snapshot: undefined,
        }),
    ],
    [
        'sequence',
        (generation: string) => ({
            ...snapshot(generation),
            kind: 'terminal',
            seq: 3,
            reason: 'idle',
            snapshot: undefined,
        }),
    ],
    [
        'terminal revision',
        (generation: string) => ({
            ...snapshot(generation),
            kind: 'terminal',
            seq: 2,
            rev: 'wrong',
            reason: 'idle',
            snapshot: undefined,
        }),
    ],
    [
        'terminal schema',
        (generation: string) => ({
            ...snapshot(generation),
            kind: 'terminal',
            seq: 2,
            schema: 'wrong',
            reason: 'idle',
            snapshot: undefined,
        }),
    ],
    [
        'delta base',
        (generation: string) => ({
            v: 2,
            kind: 'delta',
            g: generation,
            seq: 2,
            screen: '/clusters/prod/pods',
            rev: 'rev-2',
            schema: 'schema-1',
            delta: {
                base: 'wrong',
                rev: 'rev-2',
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            },
        }),
    ],
    [
        'delta schema',
        (generation: string) => ({
            v: 2,
            kind: 'delta',
            g: generation,
            seq: 2,
            screen: '/clusters/prod/pods',
            rev: 'rev-2',
            schema: 'wrong',
            delta: {
                base: 'rev-1',
                rev: 'rev-2',
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            },
        }),
    ],
] as const)(
    'rejects a successor with mismatched %s without touching the snapshot',
    async (_name, next) => {
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
        await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
        const before = document.getElementById('resource-list-content')?.innerHTML;
        stream.enqueue(sse('ro-live', next(liveState.gen)));
        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
        expect(document.getElementById('resource-list-content')?.innerHTML).toBe(before);
    },
);

test('successor checkpoint snapshots may retain rev and replace schema', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream();
    installFetch(async (_url, init) => {
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    vi.stubGlobal('Idiomorph', {
        morph: (current: HTMLElement, incoming: HTMLElement) => {
            for (const attribute of Array.from(current.attributes))
                current.removeAttribute(attribute.name);
            for (const attribute of Array.from(incoming.attributes)) {
                current.setAttribute(attribute.name, attribute.value);
            }
            current.replaceChildren(
                ...Array.from(incoming.childNodes, (node) => node.cloneNode(true)),
            );
        },
    });
    const before = window.roLive.stats().v2Snapshots;
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    stream.enqueue(
        sse(
            'ro-live',
            snapshot(liveState.gen, {
                seq: 2,
                schema: 'schema-2',
                snapshot: { html: listHTML('Checkpoint') },
            }),
        ),
    );

    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));
    expect(window.roLive.stats().v2Snapshots).toBe(before + 2);
    expect(document.querySelector('[data-key="dev/a"]')?.textContent).toContain('Checkpoint');

    const beforeDelta = window.roLive.stats().deltas;
    stream.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'delta',
            g: liveState.gen,
            seq: 3,
            screen: '/clusters/prod/pods',
            rev: 'rev-2',
            rv: '11',
            schema: 'schema-2',
            delta: {
                base: 'rev-1',
                rev: 'rev-2',
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('After checkpoint', 'Running'),
                        card: cardHTML('After checkpoint'),
                    },
                ],
            },
        }),
    );

    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(3));
    expect(window.roLive.stats().deltas).toBe(beforeDelta + 1);
    expect(listProjectionRowByKey('dev/a')?.textContent).toContain('After checkpoint');
});

test('successive committed deltas clear validators and keep reading without HTMX swaps', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async (_url, init) => {
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    vi.stubGlobal('Idiomorph', {
        morph: (current: HTMLElement, incoming: HTMLElement) => {
            for (const attribute of Array.from(current.attributes))
                current.removeAttribute(attribute.name);
            for (const attribute of Array.from(incoming.attributes)) {
                current.setAttribute(attribute.name, attribute.value);
            }
            current.replaceChildren(
                ...Array.from(incoming.childNodes, (node) => node.cloneNode(true)),
            );
        },
    });
    const afterSwap = vi.fn();
    document.addEventListener('htmx:afterSwap', afterSwap);
    const beforeDelta = window.roLive.stats();

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    content.dataset.roEtag = '"snapshot-validator"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    const row = listProjectionRowByKey('dev/a');
    const payload = {
        v: 2,
        kind: 'delta',
        g: liveState.gen,
        seq: 2,
        screen: '/clusters/prod/pods',
        rev: 'rev-2',
        rv: '11',
        schema: 'schema-1',
        delta: {
            base: 'rev-1',
            rev: 'rev-2',
            upsert: [
                {
                    key: 'dev/a',
                    row: rowHTML('Beta', 'Running'),
                    card: cardHTML('Beta'),
                },
            ],
        },
    };
    stream.enqueue(sse('ro-live', payload));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));
    expect(content.dataset.roEtag).toBeUndefined();
    expect(content.dataset.roEtagPath).toBeUndefined();
    expect(listProjectionRowByKey('dev/a')).toBe(row);
    expect(row?.textContent).toContain('Beta');

    const nextPayload = {
        ...payload,
        seq: 3,
        rev: 'rev-3',
        rv: '12',
        delta: {
            base: 'rev-2',
            rev: 'rev-3',
            upsert: [
                {
                    key: 'dev/a',
                    row: rowHTML('Gamma', 'Ready'),
                    card: cardHTML('Gamma'),
                },
            ],
        },
    };
    stream.enqueue(sse('ro-live', nextPayload));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(3));
    expect(listProjectionRowByKey('dev/a')).toBe(row);
    expect(row?.textContent).toContain('Gamma');
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(afterSwap).not.toHaveBeenCalled();
    const afterDelta = window.roLive.stats();
    expect(afterDelta.deltas).toBe(beforeDelta.deltas + 2);
    expect(afterDelta.deltaBytes).toBe(
        beforeDelta.deltaBytes +
            new TextEncoder().encode(JSON.stringify(payload)).byteLength +
            new TextEncoder().encode(JSON.stringify(nextPayload)).byteLength,
    );
    expect(afterDelta.updated).toBe(beforeDelta.updated + 2);
    expect(afterDelta.inserted).toBe(beforeDelta.inserted);
    expect(afterDelta.deleted).toBe(beforeDelta.deleted);
    expect(afterDelta.projected).toBe(beforeDelta.projected);
    document.removeEventListener('htmx:afterSwap', afterSwap);
});

test('a reentrant delta morph cannot publish an old cursor into its replacement stream', async () => {
    const content = renderLivePage();
    installHtmx();
    const stream = controlledStream();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    content.dataset.roEtag = '"replacement-validator"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    const before = window.roLive.stats();
    let replaced = false;
    vi.stubGlobal('Idiomorph', {
        morph: (current: HTMLElement, incoming: HTMLElement) => {
            if (!replaced) {
                replaced = true;
                liveApply(true);
            }
            for (const attribute of Array.from(current.attributes)) {
                current.removeAttribute(attribute.name);
            }
            for (const attribute of Array.from(incoming.attributes)) {
                current.setAttribute(attribute.name, attribute.value);
            }
            current.replaceChildren(
                ...Array.from(incoming.childNodes, (node) => node.cloneNode(true)),
            );
        },
    });
    stream.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'delta',
            g: liveState.gen,
            seq: 2,
            screen: '/clusters/prod/pods',
            rev: 'rev-2',
            schema: 'schema-1',
            delta: {
                base: 'rev-1',
                rev: 'rev-2',
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('Superseded delta'),
                        card: cardHTML('Superseded delta'),
                    },
                ],
            },
        }),
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    await flush();
    const replacement = liveState.abort;
    expect(replacement?.signal.aborted).toBe(false);
    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().seq).toBe(0);
    expect(window.roLive.stats().deltas).toBe(before.deltas);
    expect(content.dataset.roEtag).toBe('"replacement-validator"');
});

test('a reducer failure leaves the snapshot DOM intact and does not publish a delta cursor', async () => {
    renderLivePage();
    installHtmx();
    const first = controlledStream();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        first.headers.set('RO-Live-Version', '2');
        first.headers.set('RO-Live-Generation', generation);
        return first.response;
    });
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    first.enqueue(sse('ro-live', snapshot(liveState.gen)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    const before = document.getElementById('resource-list-content')?.innerHTML;
    const deltas = window.roLive.stats().deltas;
    first.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'delta',
            g: liveState.gen,
            seq: 2,
            screen: '/clusters/prod/pods',
            rev: 'rev-2',
            schema: 'schema-1',
            delta: {
                base: 'rev-1',
                rev: 'rev-2',
                upsert: [{ key: 'dev/a', row: '<tr data-key="wrong"><td>bad</td></tr>' }],
            },
        }),
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(document.getElementById('resource-list-content')?.innerHTML).toBe(before);
    expect(window.roLive.stats().deltas).toBe(deltas);
});

test('a valid successor terminal requires current rev/schema and enters banner fallback', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream();
    installFetch(async (_url, init) => {
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    const before = window.roLive.stats().terminals;
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    stream.enqueue(
        sse('ro-live', {
            v: 2,
            kind: 'terminal',
            g: liveState.gen,
            seq: 2,
            screen: '/clusters/prod/pods',
            rev: 'rev-1',
            schema: 'schema-1',
            reason: 'shutdown',
        }),
    );
    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(window.roLive.stats().terminals).toBe(before + 1);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('two protocol resyncs are allowed in 30s; the third failure falls back with a banner', async () => {
    renderLivePage();
    installHtmx();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 3) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        return response(
            [
                sse('ro-live', {
                    v: 2,
                    kind: 'terminal',
                    g: generation,
                    seq: 1,
                    screen: '/clusters/prod/pods',
                    reason: 'idle',
                }),
            ],
            200,
            {
                'Content-Type': 'text/event-stream',
                'RO-Live-Version': '2',
                'RO-Live-Generation': generation,
            },
        );
    });

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(window.roLive.stats().resyncsInWindow).toBe(2);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();

    liveApply();

    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(window.roLive.stats().resyncsInWindow).toBe(2);

    liveApply(true);

    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().resyncsInWindow).toBe(0);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('the resync budget releases an attempt at the exact 30 second boundary', async () => {
    renderLivePage();
    installHtmx();
    let now = 1_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);
    const streams = [controlledStream(), controlledStream(), controlledStream()];
    const fetchMock = installFetch(async (_url, init) => {
        const index = fetchMock.mock.calls.length - 1;
        if (index >= streams.length) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        streams[index].headers.set('RO-Live-Version', '2');
        streams[index].headers.set('RO-Live-Generation', generation);
        return streams[index].response;
    });

    liveApply();
    for (let index = 0; index < streams.length; index += 1) {
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        streams[index].enqueue(
            sse('ro-live', {
                v: 2,
                kind: 'terminal',
                g: liveState.gen,
                seq: 1,
                screen: '/clusters/prod/pods',
                reason: 'idle',
            }),
        );
        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(index + 2));
        if (index < streams.length - 1) now += 30_000;
    }

    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().resyncsInWindow).toBe(1);
    expect(dependencies.markListStale).not.toHaveBeenCalled();

    now += 30_000;
    expect(window.roLive.stats().resyncsInWindow).toBe(0);
});

test('a forced start consumes a hidden resync ticket before its next rejection', async () => {
    renderLivePage();
    installHtmx();
    const streams = [controlledStream(), controlledStream()];
    const fetchMock = installFetch(async (_url, init) => {
        const index = fetchMock.mock.calls.length - 1;
        if (index >= streams.length) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        streams[index].headers.set('RO-Live-Version', '2');
        streams[index].headers.set('RO-Live-Generation', generation);
        return streams[index].response;
    });
    const before = window.roLive.stats().resyncs;

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    streams[0].enqueue('event: wrong\ndata: {}\n\n');
    await vi.waitFor(() => expect(liveState.status).toBe('hidden'));
    expect(window.roLive.stats().resyncs).toBe(before);

    liveApply(true);
    expect(fetchMock).toHaveBeenCalledOnce();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(window.roLive.stats().resyncs).toBe(before);

    streams[1].enqueue('event: wrong\ndata: {}\n\n');

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    expect(window.roLive.stats().resyncs).toBe(before + 1);
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(3);
});

test('a snapshot without the exact final repair marker is not committed', async () => {
    renderLivePage();
    const htmx = installHtmx({ completeSnapshot: false });
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        return response([sse('ro-live', snapshot(generation))], 200, {
            'Content-Type': 'text/event-stream',
            'RO-Live-Version': '2',
            'RO-Live-Generation': generation,
        });
    });
    const snapshots = window.roLive.stats().v2Snapshots;
    liveApply();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(window.roLive.stats().v2Snapshots).toBe(snapshots);
});

test('a stale snapshot and the rest of its chunk cannot abort a force-replacement stream', async () => {
    renderLivePage();
    const htmx = installHtmx({ completeSnapshot: false, mutate: false });
    const stream = controlledStream();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    const before = window.roLive.stats();
    htmx.swap.mockImplementationOnce(() => liveApply(true));

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    const oldController = liveState.abort;
    const oldGeneration = liveState.gen;
    stream.enqueue(
        `${sse('ro-live', snapshot(oldGeneration))}${sse('ro-live', snapshot(oldGeneration))}`,
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    await flush();
    const replacement = liveState.abort;
    expect(oldController?.signal.aborted).toBe(true);
    expect(replacement).not.toBe(oldController);
    expect(replacement?.signal.aborted).toBe(false);
    expect(liveState.status).toBe('connecting');
    expect(window.roLive.stats().seq).toBe(0);
    expect(window.roLive.stats().v2Snapshots).toBe(before.v2Snapshots);
    expect(window.roLive.stats().resyncs).toBe(before.resyncs);
});

test.each([
    {
        mapEventInfo: (eventInfo: Record<string, unknown>) => ({
            ...eventInfo,
            roLiveSnapshotTxn: {},
        }),
        name: 'a wrong truthy transaction',
    },
    {
        mapEventInfo: (eventInfo: Record<string, unknown>) => {
            const copy = { ...eventInfo };
            delete copy.roLivePush;
            return copy;
        },
        name: 'the exact transaction without the push marker',
    },
])('$name cannot complete a snapshot transaction', async ({ mapEventInfo }) => {
    renderLivePage();
    const htmx = installHtmx({ mapEventInfo });
    const stream = controlledStream();
    const fetchMock = installFetch(async (_url, init) => {
        if (fetchMock.mock.calls.length > 1) return pendingFetch();
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });
    const snapshots = window.roLive.stats().v2Snapshots;

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(window.roLive.stats().v2Snapshots).toBe(snapshots);
    expect(window.roLive.stats().seq).toBe(0);
});

describe('current-list request suspension', () => {
    async function ridingStream() {
        const stream = controlledStream();
        const fetchMock = installFetch(async () =>
            fetchMock.mock.calls.length === 1 ? stream.response : pendingFetch(),
        );
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
        stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>live</p>' }));
        await vi.waitFor(() => expect(liveState.status).toBe('open-v1'));
        return { fetchMock, stream };
    }

    test('foreign and detached list targets never acquire request ownership', async () => {
        const content = renderLivePage();
        const other = document.createElement('div');
        const foreign = xhr(500);
        const detached = xhr(500);
        const before = window.roLive.stats().inFlightRequests;

        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', other, foreign));
        expect(window.roLive.stats().inFlightRequests).toBe(before);

        content.remove();
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, detached));
        expect(window.roLive.stats().inFlightRequests).toBe(before);

        foreign.dispatchEvent(new Event('loadend'));
        detached.dispatchEvent(new Event('loadend'));
        await flush();
        expect(window.roLive.stats().inFlightRequests).toBe(before);
        expect(liveState.status).toBe('idle');
    });

    test('an owned request resumes the pinned stream even if history changes while suspended', async () => {
        const content = renderLivePage('/clusters/prod/pods?sort=Name');
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        window.history.replaceState(null, '', '/clusters/prod/pods?sort=Age');
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toMatch(
            /^\/clusters\/prod\/pods\/_stream\?sort=Name&g=/u,
        );
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?sort=Name');
    });

    test('a request begun while hidden retains the one visible resume owner', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('hidden');

        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('hidden');

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.status).toBe('connecting');
    });

    test('a request that observes hidden before visibility dispatch owns the hidden state', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const first = liveState.abort;
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        const request = xhr(500);

        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        expect(first?.signal.aborted).toBe(true);
        expect(liveState.status).toBe('hidden');
        expect(fetchMock).toHaveBeenCalledOnce();

        request.dispatchEvent(new Event('loadend'));
        expect(liveState.status).toBe('hidden');
        expect(fetchMock).toHaveBeenCalledOnce();

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.status).toBe('connecting');
    });

    test('a push marker cannot complete or redirect an owned request', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        liveOnListSwap(
            htmxRequest('htmx:afterSwap', content, request, {
                pathInfo: { finalRequestPath: '/clusters/prod/namespaces/_table' },
                roLivePush: true,
                roLiveSnapshotTxn: {},
            }),
        );
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
    });

    test('a null push transaction marker is inert', () => {
        renderLivePage();
        const before = window.roLive.stats();

        expect(() =>
            liveOnListSwap(
                new CustomEvent('htmx:afterSwap', {
                    detail: { roLivePush: true, roLiveSnapshotTxn: null },
                }),
            ),
        ).not.toThrow();

        expect(window.roLive.stats()).toEqual(before);
        expect(liveState.status).toBe('idle');
    });

    test('a hostile push marker cannot satisfy a successful owned request DOM barrier', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        liveOnListSwap(
            htmxRequest('htmx:afterSwap', content, request, {
                pathInfo: { finalRequestPath: '/clusters/prod/namespaces/_table' },
                roLivePush: true,
                roLiveSnapshotTxn: {},
            }),
        );
        request.dispatchEvent(new Event('loadend'));
        await flush();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().inFlightRequests).toBe(0);
        expect(liveState.status).toBe('fallback');
        expect(dependencies.markListStale).toHaveBeenCalledOnce();
    });

    test('a list request cancels a queued resync opener without consuming another budget slot', async () => {
        const content = renderLivePage();
        installHtmx();
        const first = controlledStream();
        const second = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            const call = fetchMock.mock.calls.length;
            if (call > 2) return pendingFetch();
            const stream = call === 1 ? first : second;
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        const queued: Array<() => void> = [];
        const queue = vi
            .spyOn(globalThis, 'queueMicrotask')
            .mockImplementation((callback) => queued.push(callback));
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        first.enqueue(
            sse('ro-live', {
                v: 2,
                kind: 'terminal',
                g: liveState.gen,
                seq: 1,
                screen: '/clusters/prod/pods',
                reason: 'idle',
            }),
        );
        await vi.waitFor(() => expect(liveState.status).toBe('resyncing'));
        expect(window.roLive.stats().resyncsInWindow).toBe(1);

        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        expect(liveState.status).toBe('suspended');
        for (const callback of queued.splice(0)) callback();
        expect(fetchMock).toHaveBeenCalledOnce();
        queue.mockRestore();

        request.dispatchEvent(new Event('loadend'));
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().resyncsInWindow).toBe(1);

        second.enqueue(
            sse('ro-live', {
                v: 2,
                kind: 'terminal',
                g: liveState.gen,
                seq: 1,
                screen: '/clusters/prod/pods',
                reason: 'idle',
            }),
        );
        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
        expect(window.roLive.stats().resyncsInWindow).toBe(2);
    });

    test('overlapping XHRs abort before send and reopen exactly once after the last settles', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const firstController = liveState.abort;
        const a = xhr(200);
        const b = xhr(500);

        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, a));
        expect(firstController?.signal.aborted).toBe(true);
        expect(liveState.status).toBe('suspended');
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, a));
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, b));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, b));
        liveOnListSwap(
            htmxRequest('htmx:afterSwap', content, a, {
                pathInfo: { finalRequestPath: '/clusters/prod/pods/_table?sort=Name' },
            }),
        );

        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, a));
        expect(fetchMock).toHaveBeenCalledOnce();
        b.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?sort=Name');
        a.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test.each([304, 500, 0])('native loadend resumes after status %i', async (status) => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(status);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe(`/clusters/prod/pods/_stream?g=${liveState.gen}`);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
    });

    test('a page reset clears stale resume ownership before native loadend reopens Live', () => {
        const content = renderLivePage('/clusters/prod/services');
        const fetchMock = installFetch(pendingFetch);
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        liveApply();
        expect(fetchMock).not.toHaveBeenCalled();
        expect(liveState.status).toBe('hidden');

        const stale = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, stale));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, stale));
        window.history.replaceState(null, '', '/clusters/prod/pods');

        liveTeardown();
        liveResetPage();
        liveState.status = 'idle';
        liveState.streamPath = '';

        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('idle');
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).not.toHaveBeenCalled();

        liveApply();
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(fetchMock.mock.calls[0][0]).toBe(`/clusters/prod/pods/_stream?g=${liveState.gen}`);

        const current = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, current));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, current));
        current.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toBe(`/clusters/prod/pods/_stream?g=${liveState.gen}`);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
        stale.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('a later beforeSwap cancellation satisfies the 200 DOM barrier without reopening early', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        const event = htmxRequest('htmx:beforeSwap', content, request, { shouldSwap: true });
        liveBeforeListSwapDecision(event);
        (event as CustomEvent).detail.shouldSwap = false;

        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));
        expect(fetchMock).toHaveBeenCalledOnce();
        await flush();
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('a prevented beforeSwap satisfies the same 200 DOM barrier', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        const event = new CustomEvent('htmx:beforeSwap', {
            cancelable: true,
            detail: { target: content, xhr: request, shouldSwap: true },
        });
        liveBeforeListSwapDecision(event);
        event.preventDefault();
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        await flush();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('swapError releases a network-settled 200 request exactly once', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        liveListRequestSwapFailed(htmxRequest('htmx:swapError', content, request));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('swapError before network settlement waits for the native barrier', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        liveListRequestSwapFailed(htmxRequest('htmx:swapError', content, request));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().inFlightRequests).toBe(1);

        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('an allowed beforeSwap is not mistaken for the final DOM barrier', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveBeforeListSwapDecision(
            htmxRequest('htmx:beforeSwap', content, request, { shouldSwap: true }),
        );
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        await flush();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('fallback');
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('a network-settled 200 with no terminal swap event fails closed next microtask', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        await flush();
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().inFlightRequests).toBe(0);
        expect(liveState.status).toBe('fallback');
        expect(dependencies.markListStale).toHaveBeenCalledOnce();

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('fallback');

        window.history.replaceState(null, '', '/clusters/prod/pods?sort=Name');
        liveOnListSwap(
            htmxRequest('htmx:afterSwap', content, request, {
                pathInfo: { finalRequestPath: '/clusters/prod/pods/_table?sort=Name' },
            }),
        );
        liveApply();
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('fallback');
    });

    test('a missing 200 swap retires cleanly when Live was turned Off meanwhile', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));
        dependencies.refreshMode.mockReturnValue('Off');

        await flush();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().inFlightRequests).toBe(0);
        expect(liveState.status).toBe('idle');
        expect(liveState.streamPath).toBe('');
        expect(dependencies.markListStale).not.toHaveBeenCalled();

        dependencies.refreshMode.mockReturnValue('Live');
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
    });

    test('a nonresumable 200 without a swap retires without inventing fallback ownership', async () => {
        const content = renderLivePage();
        const request = xhr(200);
        const fallbacks = window.roLive.stats().fallbacks;
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        await flush();

        expect(window.roLive.stats().inFlightRequests).toBe(0);
        expect(window.roLive.stats().fallbacks).toBe(fallbacks);
        expect(liveState.status).toBe('idle');
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a request settled after preference Off cannot reopen later polling traffic', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        dependencies.refreshMode.mockReturnValue('Off');
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
        expect(liveState.streamPath).toBe('');
        expect(window.roLive.stats().inFlightRequests).toBe(0);

        const poll = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, poll));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, poll));
        expect(liveState.status).toBe('idle');
        poll.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledOnce();
    });

    test('a page reset makes a queued missing-swap check inert', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));
        liveResetPage();
        liveTeardown();
        liveState.status = 'idle';

        expect(window.roLive.stats().inFlightRequests).toBe(0);
        await flush();
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
    });

    test('an old missing-swap callback cannot poison a new suspended request', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const old = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, old));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, old));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, old));

        liveResetPage();
        liveTeardown();
        liveState.status = 'idle';
        liveApply();
        const current = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, current));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, current));

        await flush();

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.status).toBe('suspended');
        expect(window.roLive.stats().inFlightRequests).toBe(1);
        expect(dependencies.markListStale).not.toHaveBeenCalled();

        current.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(3);
    });

    test('reentrant listener registration cannot suspend replacement ownership of the same XHR', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        const addEventListener = EventTarget.prototype.addEventListener.bind(request);
        let reentered = false;
        Object.defineProperty(request, 'addEventListener', {
            configurable: true,
            value(
                type: string,
                callback: EventListenerOrEventListenerObject | null,
                options?: AddEventListenerOptions | boolean,
            ) {
                if (!reentered) {
                    reentered = true;
                    liveResetPage();
                    liveTeardown();
                    liveState.status = 'idle';
                    liveState.streamPath = '';
                    liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
                    liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
                    return;
                }
                addEventListener(type, callback, options);
            },
        });

        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().inFlightRequests).toBe(1);
        expect(liveState.status).toBe('idle');
        expect(liveState.streamPath).toBe('');

        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().inFlightRequests).toBe(0);
        expect(liveState.status).toBe('idle');
    });

    test('reentrant listener registration cannot abort an unowned replacement connection', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        const replacement: { ctrl: AbortController | null } = { ctrl: null };
        Object.defineProperty(request, 'addEventListener', {
            configurable: true,
            value() {
                liveResetPage();
                liveApply(true);
                replacement.ctrl = liveState.abort;
            },
        });

        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(replacement.ctrl).not.toBeNull();
        expect(replacement.ctrl?.signal.aborted).toBe(false);
        expect(liveState.abort).toBe(replacement.ctrl);
        expect(liveState.status).toBe('connecting');
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('a reentrant status getter cannot retire replacement ownership of the same XHR', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        let reentered = false;
        Object.defineProperty(request, 'status', {
            configurable: true,
            get() {
                if (!reentered) {
                    reentered = true;
                    liveResetPage();
                    liveApply(true);
                    liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
                    liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
                }
                return 500;
            },
        });

        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().inFlightRequests).toBe(1);
        expect(liveState.status).toBe('suspended');

        Object.defineProperty(request, 'status', {
            configurable: true,
            value: 500,
            writable: true,
        });
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(fetchMock.mock.calls[2][0]).toMatch(/^\/clusters\/prod\/pods\/_stream\?g=/u);
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('a reentrant path getter cannot retire or redirect replacement ownership', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(200);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        let reentered = false;
        const pathInfo: Record<string, unknown> = {};
        Object.defineProperty(pathInfo, 'finalRequestPath', {
            get() {
                if (!reentered) {
                    reentered = true;
                    liveResetPage();
                    liveApply(true);
                    liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
                    liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
                }
                return '/clusters/prod/namespaces/_table';
            },
        });

        liveOnListSwap(htmxRequest('htmx:afterSwap', content, request, { pathInfo }));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().inFlightRequests).toBe(1);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');

        Object.defineProperty(request, 'status', {
            configurable: true,
            value: 500,
            writable: true,
        });
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(fetchMock.mock.calls[2][0]).toMatch(/^\/clusters\/prod\/pods\/_stream\?g=/u);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('a canceled beforeRequest self-settles in a microtask', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr();
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        expect(liveState.status).toBe('suspended');
        await flush();
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('a page reset makes an old detached loadend inert', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr();
        let statusReads = 0;
        Object.defineProperty(request, 'status', {
            configurable: true,
            get() {
                statusReads += 1;
                return 500;
            },
        });
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveResetPage();
        liveTeardown();
        liveState.status = 'idle';
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
        expect(statusReads).toBe(0);
    });

    test('an Off/poll XHR is tracked so an explicit Live pick waits for loadend', async () => {
        const content = renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        dependencies.refreshMode.mockReturnValue('Off');
        const request = xhr();
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        dependencies.refreshMode.mockReturnValue('Live');
        liveApply(true);
        expect(fetchMock).not.toHaveBeenCalled();
        expect(liveState.status).toBe('suspended');
        liveOnListSwap(
            htmxRequest('htmx:afterSwap', content, request, {
                pathInfo: { finalRequestPath: '/clusters/prod/pods/_table' },
            }),
        );
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledOnce();
    });

    test('a deferred Live pick derives its base when the surface becomes supported', () => {
        const content = renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        const option = document.querySelector<HTMLButtonElement>(
            '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
        );
        if (!option) throw new Error('Live option is missing');
        dependencies.refreshMode.mockReturnValue('Off');
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

        option.disabled = true;
        dependencies.refreshMode.mockReturnValue('Live');
        liveApply(true);
        expect(liveState.status).toBe('suspended');
        expect(liveState.streamPath).toBe('');
        expect(fetchMock).not.toHaveBeenCalled();

        option.disabled = false;
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(fetchMock.mock.calls[0][0]).toBe(`/clusters/prod/pods/_stream?g=${liveState.gen}`);
        expect(liveState.status).toBe('connecting');
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
    });

    test('a hidden explicit Live pick waits for its owned poll and then visibility', async () => {
        const content = renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        dependencies.refreshMode.mockReturnValue('Off');
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

        dependencies.refreshMode.mockReturnValue('Live');
        liveApply(true);
        expect(liveState.status).toBe('hidden');
        expect(fetchMock).not.toHaveBeenCalled();

        request.dispatchEvent(new Event('loadend'));
        expect(liveState.status).toBe('hidden');
        expect(fetchMock).not.toHaveBeenCalled();

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('connecting');
    });

    test('a hidden deferred Live pick derives its base after support returns', () => {
        const content = renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        const option = document.querySelector<HTMLButtonElement>(
            '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
        );
        if (!option) throw new Error('Live option is missing');
        dependencies.refreshMode.mockReturnValue('Off');
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

        option.disabled = true;
        dependencies.refreshMode.mockReturnValue('Live');
        liveApply(true);
        request.dispatchEvent(new Event('loadend'));
        expect(liveState.status).toBe('hidden');
        expect(liveState.streamPath).toBe('');
        expect(fetchMock).not.toHaveBeenCalled();

        option.disabled = false;
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(fetchMock.mock.calls[0][0]).toBe(`/clusters/prod/pods/_stream?g=${liveState.gen}`);
        expect(liveState.status).toBe('connecting');
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
    });

    test.each([
        { hidden: false, name: 'visible' },
        { hidden: true, name: 'hidden until visibility returns' },
    ])(
        'an unsupported explicit Live pick becomes fallback when its owned poll settles $name',
        async ({ hidden }) => {
            const content = renderLivePage();
            document.querySelector('[data-ro-interval="Live"]')?.remove();
            const fetchMock = installFetch(pendingFetch);
            const request = xhr(500);
            liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
            liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
            vi.spyOn(document, 'hidden', 'get').mockReturnValue(hidden);

            liveApply(true);
            request.dispatchEvent(new Event('loadend'));

            if (hidden) {
                expect(liveState.status).toBe('hidden');
                expect(liveFallbackSeconds()).toBe(0);
                vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
                document.dispatchEvent(new Event('visibilitychange'));
            }
            expect(fetchMock).not.toHaveBeenCalled();
            expect(liveState.status).toBe('fallback');
            expect(liveState.streamPath).toBe('');
            expect(liveFallbackSeconds()).toBe(5);
            expect(window.roLive.stats().inFlightRequests).toBe(0);
            expect(dependencies.markListStale).not.toHaveBeenCalled();
        },
    );

    test('requestPath is used when finalRequestPath is absent', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveOnListSwap(
            htmxRequest('htmx:afterSwap', content, request, {
                pathInfo: { requestPath: '/clusters/prod/pods/_table?sort=Age' },
            }),
        );
        request.dispatchEvent(new Event('loadend'));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?sort=Age');
    });

    test.each([
        {
            detail: () =>
                Object.defineProperty({}, 'pathInfo', {
                    get() {
                        throw new Error('pathInfo unavailable');
                    },
                }),
            name: 'pathInfo',
        },
        {
            detail: () => ({
                pathInfo: Object.defineProperty({}, 'finalRequestPath', {
                    get() {
                        throw new Error('finalRequestPath unavailable');
                    },
                }),
            }),
            name: 'finalRequestPath',
        },
    ])(
        'a throwing $name getter completes the DOM barrier without redirecting it',
        async ({ detail }) => {
            const content = renderLivePage();
            installHtmx();
            const { fetchMock } = await ridingStream();
            const request = xhr(500);
            liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
            liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));

            expect(() =>
                liveOnListSwap(htmxRequest('htmx:afterSwap', content, request, detail())),
            ).not.toThrow();
            request.dispatchEvent(new Event('loadend'));

            expect(fetchMock).toHaveBeenCalledTimes(2);
            expect(fetchMock.mock.calls[1][0]).toMatch(/^\/clusters\/prod\/pods\/_stream\?g=/u);
            expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream');
            expect(window.roLive.stats().inFlightRequests).toBe(0);
        },
    );

    test('throwing synthetic XHR surfaces still settle through afterRequest', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = {
            addEventListener() {
                throw new Error('synthetic XHR');
            },
            get status() {
                throw new Error('status unavailable');
            },
        } as unknown as XMLHttpRequest;
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveAfterListRequest(htmxRequest('htmx:afterRequest', content, request));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(window.roLive.stats().inFlightRequests).toBe(0);
    });

    test('malformed and duplicate public request events remain inert', async () => {
        const content = renderLivePage();
        const request = xhr(500);
        const other = document.createElement('div');
        const before = window.roLive.stats().inFlightRequests;

        expect(() => liveBeforeListRequest(new Event('htmx:beforeRequest'))).not.toThrow();
        expect(() =>
            liveBeforeListRequest(htmxRequest('htmx:beforeRequest', other, request)),
        ).not.toThrow();
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        expect(window.roLive.stats().inFlightRequests).toBe(before + 1);
        expect(() => liveMarkListRequestSent(new Event('htmx:beforeSend'))).not.toThrow();
        expect(() => liveAfterListRequest(new Event('htmx:afterRequest'))).not.toThrow();
        expect(() => liveBeforeListSwapDecision(new Event('htmx:beforeSwap'))).not.toThrow();
        expect(() => liveListRequestSwapFailed(new Event('htmx:swapError'))).not.toThrow();
        expect(() => liveOnListSwap(new Event('htmx:afterSwap'))).not.toThrow();

        await flush();
        expect(window.roLive.stats().inFlightRequests).toBe(before);
    });

    test('an explicit Live repick clears fallback countdown while its poll is still owned', async () => {
        const content = renderLivePage();
        const fetchMock = installFetch(async () => response([], 429));
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
        expect(liveFallbackSeconds()).toBe(5);

        const request = xhr(304);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveApply(true);

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('suspended');
        expect(liveFallbackSeconds()).toBe(0);
        expect(window.roLive.stats().inFlightRequests).toBe(1);
    });

    test('fallback polling never auto-resumes a stream', async () => {
        const content = renderLivePage();
        installFetch(async () => response([], 429));
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
        const request = xhr(304);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        request.dispatchEvent(new Event('loadend'));
        expect(liveState.status).toBe('fallback');
    });
});

describe('visibility and first-frame deadline', () => {
    test('visibility changes are inert without resumable Live ownership', () => {
        renderLivePage();
        installFetch(pendingFetch);
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('idle');

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('idle');
    });

    test('initialization in a hidden tab defers fetch until the first visible event', () => {
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

        liveApply();
        expect(liveState.status).toBe('hidden');
        expect(fetchMock).not.toHaveBeenCalled();
        expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('connecting');
    });

    test('hidden aborts; visible waits for owned XHR then opens once', async () => {
        const content = renderLivePage();
        installHtmx();
        const fetchMock = installFetch(pendingFetch);
        liveApply();
        const first = liveState.abort;
        const request = xhr(304);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(first?.signal.aborted).toBe(true);
        expect(liveState.status).toBe('hidden');

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('suspended');
        expect(fetchMock).toHaveBeenCalledOnce();
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('visibility resumes the pinned stream once even if history changes while hidden', () => {
        renderLivePage('/clusters/prod/pods?sort=Name');
        const fetchMock = installFetch(pendingFetch);
        liveApply();
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));

        window.history.replaceState(null, '', '/clusters/prod/pods?sort=Age');
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock.mock.calls[1][0]).toMatch(
            /^\/clusters\/prod\/pods\/_stream\?sort=Name&g=/u,
        );
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?sort=Name');

        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    test('a hidden pending resync survives one owned request and is then consumed exactly once', async () => {
        const content = renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        const before = window.roLive.stats().resyncs;
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        stream.enqueue('event: wrong\ndata: {}\n\n');
        await vi.waitFor(() => expect(liveState.status).toBe('hidden'));

        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('suspended');
        expect(fetchMock).toHaveBeenCalledOnce();

        request.dispatchEvent(new Event('loadend'));
        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
        expect(window.roLive.stats().resyncs).toBe(before + 1);

        const resumed = liveState.abort;
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(resumed?.signal.aborted).toBe(true);
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(window.roLive.stats().resyncs).toBe(before + 1);

        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledTimes(3);
    });

    test('a disabled surface consumes an owned hidden resync without charging it', async () => {
        const content = renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        const before = window.roLive.stats();
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        stream.enqueue('event: wrong\ndata: {}\n\n');
        await vi.waitFor(() => expect(liveState.status).toBe('hidden'));

        const request = xhr(500);
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(liveState.status).toBe('suspended');

        const option = document.querySelector<HTMLButtonElement>(
            '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
        );
        if (!option) throw new Error('Live option is missing');
        option.disabled = true;
        request.dispatchEvent(new Event('loadend'));
        await flush();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().resyncs).toBe(before.resyncs);
        expect(window.roLive.stats().resyncsInWindow).toBe(before.resyncsInWindow);
        expect(liveState.status).toBe('fallback');
        expect(liveState.abort).toBeNull();
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a hidden pending resync is cleared when the preference turns Off', async () => {
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        const resyncs = window.roLive.stats().resyncs;
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        const first = liveState.abort;
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        stream.enqueue('event: wrong\ndata: {}\n\n');
        await vi.waitFor(() => expect(liveState.status).toBe('hidden'));
        expect(first?.signal.aborted).toBe(true);
        expect(liveState.status).toBe('hidden');

        dependencies.refreshMode.mockReturnValue('Off');
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
        expect(liveState.streamPath).toBe('');
        expect(liveState.abort).toBeNull();
        expect(window.roLive.stats().resyncs).toBe(resyncs);

        dependencies.refreshMode.mockReturnValue('Live');
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        document.dispatchEvent(new Event('visibilitychange'));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
        expect(window.roLive.stats().resyncs).toBe(resyncs);
    });

    test('a protocol rejection observed while hidden resumes through the paid resync path', async () => {
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        const beforeWindow = window.roLive.stats().resyncsInWindow;
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        const rejected = liveState.abort;
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        stream.enqueue('event: wrong\ndata: {}\n\n');
        await vi.waitFor(() => expect(liveState.status).toBe('hidden'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(rejected?.signal.aborted).toBe(true);
        expect(liveState.abort).toBeNull();
        expect(window.roLive.stats().resyncsInWindow).toBe(beforeWindow);

        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));

        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
        expect(window.roLive.stats().resyncsInWindow).toBe(beforeWindow + 1);
        expect(liveState.status).toBe('connecting');
    });

    test('a disabled Live surface consumes a hidden rejection without reopening it', async () => {
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        const fetchMock = installFetch(async (_url, init) => {
            if (fetchMock.mock.calls.length > 1) return pendingFetch();
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        const before = window.roLive.stats();
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        stream.enqueue('event: wrong\ndata: {}\n\n');
        await vi.waitFor(() => expect(liveState.status).toBe('hidden'));

        const option = document.querySelector<HTMLButtonElement>(
            '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
        );
        if (!option) throw new Error('Live option is missing');
        option.disabled = true;
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        document.dispatchEvent(new Event('visibilitychange'));
        await flush();

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(window.roLive.stats().resyncs).toBe(before.resyncs);
        expect(liveState.status).toBe('fallback');
        expect(liveState.abort).toBeNull();
        expect(liveFallbackSeconds()).toBe(5);
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a heartbeat-only hung stream silently falls back at the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        installFetch(async () => stream.response);
        liveApply();
        await flush();
        expect(liveState.status).toBe('syncing-v1');
        stream.enqueue(': heartbeat\n\n');
        await flush();
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);
        expect(liveState.status).toBe('fallback');
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a rejected table frame does not clear the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        installFetch(async () => stream.response);
        const discards = window.roLive.discards();
        liveApply();
        await flush();

        stream.enqueue(sse('ro-table', { g: 'stale', html: '<p>stale</p>' }));
        await flush();
        expect(window.roLive.discards()).toBe(discards + 1);
        expect(liveState.status).toBe('syncing-v1');

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);

        expect(liveState.status).toBe('fallback');
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a negotiated v2 stream is also bounded by the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        installFetch(async (_url, init) => {
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        liveApply();
        await flush();
        expect(liveState.status).toBe('syncing-v2');

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);

        expect(liveState.status).toBe('fallback');
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a fetch that never returns headers is covered by the same silent deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installFetch(pendingFetch);
        liveApply();
        expect(liveState.status).toBe('connecting');
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS);
        expect(liveState.status).toBe('fallback');
        expect(liveState.abort).toBeNull();
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('a forced replacement gets its own full first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        const failed = deferred<Response>();
        const fetchMock = installFetch(() =>
            fetchMock.mock.calls.length === 1 ? failed.promise : pendingFetch(),
        );
        liveApply();
        const first = liveState.abort;
        expect(vi.getTimerCount()).toBe(1);

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS / 2);
        liveApply(true);
        const replacement = liveState.abort;
        expect(first?.signal.aborted).toBe(true);
        expect(replacement).not.toBe(first);
        failed.reject(new DOMException('superseded', 'AbortError'));
        await flush();
        expect(vi.getTimerCount()).toBe(1);

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS / 2);

        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(liveState.abort).toBe(replacement);
        expect(replacement?.signal.aborted).toBe(false);
        expect(liveState.status).toBe('connecting');
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });

    test('an accepted table clears the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        installFetch(async () => stream.response);
        liveApply();
        await flush();
        stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>ready</p>' }));
        await flush();
        expect(liveState.status).toBe('open-v1');
        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS + 1);
        expect(liveState.status).toBe('open-v1');
    });

    test('an accepted v2 snapshot also clears the first-frame deadline', async () => {
        vi.useFakeTimers();
        renderLivePage();
        installHtmx();
        const stream = controlledStream();
        installFetch(async (_url, init) => {
            const generation = requestHeaders(init)['RO-Live-Generation'];
            stream.headers.set('RO-Live-Version', '2');
            stream.headers.set('RO-Live-Generation', generation);
            return stream.response;
        });
        liveApply();
        await flush();
        stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
        await flush();
        expect(liveState.status).toBe('open-v2');

        await vi.advanceTimersByTimeAsync(LIVE_FIRST_FRAME_TIMEOUT_MS + 1);

        expect(liveState.status).toBe('open-v2');
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });
});

test('debug stats are copied rather than exposing mutable transport state', () => {
    const first = window.roLive.stats();
    first.connections = -1;
    expect(window.roLive.stats().connections).toBeGreaterThanOrEqual(0);
    expect(window.roLive.discards()).toBe(window.roLive.stats().discards);
});
