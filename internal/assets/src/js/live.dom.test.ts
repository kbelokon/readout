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

function installHtmx(options: { completeSnapshot?: boolean; mutate?: boolean } = {}): HtmxHarness {
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
                liveOnListSwap(
                    new CustomEvent('htmx:afterSwap', {
                        detail: { ...swapOptions.eventInfo, target },
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

    liveApply();
    expect(fetchMock).toHaveBeenCalledOnce();
    const first = liveState.abort;
    liveApply(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(first?.signal.aborted).toBe(true);
});

test('an unsupported page and a non-Live preference use sticky polling/idle states', () => {
    const content = renderLivePage();
    const fetchMock = installFetch(pendingFetch);
    content.dataset.liveUrl = 'baked';

    liveApply();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(liveState.status).toBe('fallback');
    expect(liveFallbackSeconds()).toBe(5);

    dependencies.refreshMode.mockReturnValue('Off');
    liveApply();
    expect(liveState.status).toBe('idle');
    expect(liveState.streamPath).toBe('');
    expect(liveFallbackSeconds()).toBe(0);
});

test('an unversioned text/event-stream stays on the legacy v1 lane', async () => {
    const content = renderLivePage();
    content.dataset.roEtag = '"old"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    const htmx = installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
    stream.enqueue(sse('ro-table', { g: liveState.gen, html: '<p>legacy</p>' }));

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
    expect(window.roLive.stats().v1Snapshots).toBeGreaterThan(0);
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
    installFetch(async (_url, init) => {
        const generation = requestHeaders(init)['RO-Live-Generation'];
        stream.headers.set('RO-Live-Version', '2');
        stream.headers.set('RO-Live-Generation', generation);
        return stream.response;
    });

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));

    await vi.waitFor(() => expect(liveState.status).toBe('open-v2'));
    expect(seqInsideSwap).toBe(0);
    expect(window.roLive.stats().seq).toBe(1);
    expect(htmx.swap.mock.calls[0][2]).toStrictEqual({ swapStyle: 'morph' });
});

test.each([
    ['version without generation', { 'RO-Live-Version': '2' }],
    ['generation without version', { 'RO-Live-Generation': 'wrong' }],
    ['unsupported version', { 'RO-Live-Version': '3', 'RO-Live-Generation': 'wrong' }],
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

    const wire = new TextEncoder().encode(
        `: heartbeat\revent: ro-table\rdata: {broken}\r\r${sse('ro-table', {
            g: 'stale',
            html: '<p>stale</p>',
        })}${sse('ro-table', { g: liveState.gen, html: '<p>🫠</p>' })}`,
    );
    const emoji = Array.from(wire).findIndex((byte) => byte >= 0xf0);
    stream.enqueue(wire.slice(0, emoji + 2));
    stream.enqueue(wire.slice(emoji + 2));

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(htmx.swap.mock.calls[0][1]).toBe('<p>🫠</p>');
    expect(window.roLive.discards()).toBe(beforeDiscards + 1);
    expect(window.roLive.stats().invalidFrames).toBeGreaterThan(0);
});

test('a valid legacy terminal stops the stream and shows stale fallback', async () => {
    renderLivePage();
    installHtmx();
    const stream = controlledStream();
    installFetch(async () => stream.response);
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v1'));
    stream.enqueue(sse('ro-terminal', { g: liveState.gen, reason: 'auth' }));
    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

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
});

test('a committed delta updates directly without another htmx swap/afterSwap/transition', async () => {
    renderLivePage();
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
    const beforeDeltaCount = window.roLive.stats().deltas;

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    stream.enqueue(sse('ro-live', snapshot(liveState.gen)));
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(1));
    const row = listProjectionRowByKey('dev/a');
    stream.enqueue(
        sse('ro-live', {
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
        }),
    );
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));
    expect(listProjectionRowByKey('dev/a')).toBe(row);
    expect(row?.textContent).toContain('Beta');
    expect(htmx.swap).toHaveBeenCalledOnce();
    expect(afterSwap).not.toHaveBeenCalled();
    expect(window.roLive.stats().deltas).toBe(beforeDeltaCount + 1);
    document.removeEventListener('htmx:afterSwap', afterSwap);
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
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('two protocol resyncs are allowed in 30s; the third failure falls back with a banner', async () => {
    renderLivePage();
    installHtmx();
    const fetchMock = installFetch(async (_url, init) => {
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

    test('a stale page epoch makes an old detached loadend inert', async () => {
        const content = renderLivePage();
        installHtmx();
        const { fetchMock } = await ridingStream();
        const request = xhr();
        liveBeforeListRequest(htmxRequest('htmx:beforeRequest', content, request));
        liveMarkListRequestSent(htmxRequest('htmx:beforeSend', content, request));
        liveResetPage();
        liveTeardown();
        liveState.status = 'idle';
        request.dispatchEvent(new Event('loadend'));
        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('idle');
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
    test('initialization in a hidden tab defers fetch until the first visible event', () => {
        renderLivePage();
        const fetchMock = installFetch(pendingFetch);
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

        liveApply();
        expect(liveState.status).toBe('hidden');
        expect(fetchMock).not.toHaveBeenCalled();

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
});

test('debug stats are copied rather than exposing mutable transport state', () => {
    const first = window.roLive.stats();
    first.connections = -1;
    expect(window.roLive.stats().connections).toBeGreaterThanOrEqual(0);
    expect(window.roLive.discards()).toBe(window.roLive.stats().discards);
});
