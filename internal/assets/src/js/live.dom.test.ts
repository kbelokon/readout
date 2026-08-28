// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    containerRequests: new Set<XMLHttpRequest>(),
    markListStale: vi.fn(),
    pruneSettledListRequests: vi.fn((requests: Set<XMLHttpRequest>) => {
        for (const xhr of requests) {
            if (xhr.readyState === 0 || xhr.readyState === 4) {
                requests.delete(xhr);
            }
        }
    }),
    refreshMode: vi.fn(() => 'Live'),
    scheduleRefreshTick: vi.fn(),
    userRequests: new Set<XMLHttpRequest>(),
}));

vi.mock('./refresh.js', () => ({
    containerListRequestsInFlight: dependencies.containerRequests,
    pruneSettledListRequests: dependencies.pruneSettledListRequests,
    refreshMode: dependencies.refreshMode,
    scheduleRefreshTick: dependencies.scheduleRefreshTick,
    userListRequestsInFlight: dependencies.userRequests,
}));
vi.mock('./stale.js', () => ({
    markListStale: dependencies.markListStale,
}));

import { liveApply, liveFallbackSeconds, liveOnListSwap, liveState, liveTeardown } from './live.js';

const importedLiveState = { ...liveState };
const importedLiveDiscards = window.roLive.discards();
const importedFallbackSeconds = liveFallbackSeconds();

interface HtmxHarness {
    swap: ReturnType<typeof vi.fn>;
}

interface Deferred<T> {
    promise: Promise<T>;
    reject(reason: unknown): void;
    resolve(value: T): void;
}

function deferred<T>(): Deferred<T> {
    let resolve!: (value: T) => void;
    let reject!: (reason: unknown) => void;
    const promise = new Promise<T>((done, fail) => {
        resolve = done;
        reject = fail;
    });
    return { promise, reject, resolve };
}

function xhrAt(readyState: number): XMLHttpRequest {
    return { readyState } as unknown as XMLHttpRequest;
}

function renderLivePage(disabled = false): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.dataset.liveUrl = 'location';
    const option = document.createElement('button');
    option.dataset.roAction = 'set-refresh';
    option.dataset.roInterval = 'Live';
    option.disabled = disabled;
    document.body.append(content, option);
    return content;
}

function installHtmx(): HtmxHarness {
    const htmx = { swap: vi.fn() };
    (window as unknown as { htmx: HtmxHarness }).htmx = htmx;
    return htmx;
}

function streamResponse(parts: Array<string | Uint8Array | Error>, status = 200): Response {
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
        start(controller) {
            for (const part of parts) {
                if (part instanceof Error) {
                    controller.error(part);
                    return;
                }
                controller.enqueue(typeof part === 'string' ? encoder.encode(part) : part);
            }
            controller.close();
        },
    });
    return {
        status,
        body,
    } as unknown as Response;
}

function readerOnlyResponse(parts: string[]): Response {
    const encoder = new TextEncoder();
    const queue = parts.map((part) => encoder.encode(part));
    const body = {
        getReader: () => ({
            read: vi.fn(async () => {
                const value = queue.shift();
                return value === undefined
                    ? { done: true as const, value: undefined }
                    : { done: false as const, value };
            }),
        }),
    };
    return { status: 200, body } as unknown as Response;
}

interface ControlledStream {
    close(): void;
    enqueue(text: string): void;
    response: Response;
}

function controlledStream(): ControlledStream {
    const encoder = new TextEncoder();
    let streamController!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
        start(controller) {
            streamController = controller;
        },
    });
    return {
        close: () => streamController.close(),
        enqueue: (text) => streamController.enqueue(encoder.encode(text)),
        response: { status: 200, body } as unknown as Response,
    };
}

function statusResponse(status: number): Response {
    return { status, body: null } as unknown as Response;
}

function installFetch(
    implementation: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(implementation);
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
}

function installAbortablePendingFetch(): ReturnType<typeof vi.fn> {
    return installFetch((_input, init) => {
        return new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => reject(new Error('aborted')), {
                once: true,
            });
        });
    });
}

beforeEach(() => {
    liveTeardown();
    liveState.status = 'idle';
    liveState.abort = null;
    liveState.gen = '';
    liveState.streamPath = '';
    dependencies.containerRequests.clear();
    dependencies.userRequests.clear();
    dependencies.markListStale.mockReset();
    dependencies.pruneSettledListRequests.mockClear();
    dependencies.refreshMode.mockReset().mockReturnValue('Live');
    dependencies.scheduleRefreshTick.mockReset();
    delete (window as unknown as { htmx?: HtmxHarness }).htmx;
});

afterEach(async () => {
    liveTeardown();
    liveState.status = 'idle';
    liveState.streamPath = '';
    await Promise.resolve();
});

test('starts from a clean, idle stream state', () => {
    expect(importedLiveState).toStrictEqual({
        status: 'idle',
        abort: null,
        gen: '',
        streamPath: '',
    });
    expect(importedLiveDiscards).toBe(0);
    expect(importedFallbackSeconds).toBe(0);
});

test('liveApply derives the raw stream URL, is idempotent, force-reopens, and tears down', () => {
    window.history.replaceState(
        null,
        '',
        '/clusters/prod/namespaces/default/pods///?f=status:Running,Pending&sort=Age%3Adesc',
    );
    renderLivePage();
    const fetchMock = installAbortablePendingFetch();

    liveApply();

    const base =
        '/clusters/prod/namespaces/default/pods/_stream?f=status:Running,Pending&sort=Age%3Adesc';
    const first = liveState.abort;
    expect(liveState.status).toBe('connecting');
    expect(liveState.streamPath).toBe(base);
    expect(first).not.toBeNull();
    const firstGeneration = liveState.gen;
    expect(firstGeneration).not.toBe('');
    expect(fetchMock).toHaveBeenCalledExactlyOnceWith(`${base}&g=${liveState.gen}`, {
        signal: first?.signal,
    });
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();

    liveApply();
    expect(fetchMock).toHaveBeenCalledOnce();

    liveApply(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(first?.signal.aborted).toBe(true);
    expect(liveState.abort).not.toBe(first);
    expect(liveState.gen).not.toBe(firstGeneration);

    const second = liveState.abort;
    liveTeardown();
    expect(second?.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveFallbackSeconds()).toBe(0);

    dependencies.refreshMode.mockReturnValue('Off');
    liveApply();
    expect(liveState.status).toBe('idle');
    expect(liveState.streamPath).toBe('');
});

test('a queryless stream URL uses the first query delimiter for its generation', () => {
    window.history.replaceState(null, '', '/clusters/prod/pods');
    renderLivePage();
    const fetchMock = installAbortablePendingFetch();

    liveApply();

    expect(fetchMock).toHaveBeenCalledExactlyOnceWith(
        `/clusters/prod/pods/_stream?g=${liveState.gen}`,
        { signal: liveState.abort?.signal },
    );
});

describe('unsupported Live pages', () => {
    test.each([
        ['disabled option', () => renderLivePage(true), 5],
        [
            'missing option',
            () => {
                const content = document.createElement('div');
                content.id = 'resource-list-content';
                content.dataset.liveUrl = 'location';
                document.body.append(content);
            },
            5,
        ],
        [
            'wrong live contract',
            () => {
                const content = renderLivePage();
                content.dataset.liveUrl = 'baked';
            },
            5,
        ],
        [
            'missing list container',
            () => {
                const option = document.createElement('button');
                option.dataset.roAction = 'set-refresh';
                option.dataset.roInterval = 'Live';
                document.body.append(option);
            },
            0,
        ],
    ] as const)('%s degrades without opening a stream', (_name, render, fallbackSeconds) => {
        render();
        const fetchMock = installAbortablePendingFetch();

        liveApply();

        expect(fetchMock).not.toHaveBeenCalled();
        expect(liveState.status).toBe('fallback');
        expect(liveState.streamPath).toBe('');
        expect(liveFallbackSeconds()).toBe(fallbackSeconds);
        expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();
        expect(dependencies.markListStale).not.toHaveBeenCalled();
    });
});

test('an idle state with the same page identity still opens a stream', () => {
    window.history.replaceState(null, '', '/clusters/prod/pods');
    renderLivePage();
    const fetchMock = installAbortablePendingFetch();
    liveState.status = 'idle';
    liveState.streamPath = '/clusters/prod/pods/_stream';

    liveApply();

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(liveState.status).toBe('connecting');
    expect(liveState.abort).not.toBeNull();
});

test('a changed page identity reopens without an explicit force', () => {
    window.history.replaceState(null, '', '/clusters/prod/pods');
    renderLivePage();
    const fetchMock = installAbortablePendingFetch();
    liveApply();
    const first = liveState.abort;
    window.history.replaceState(null, '', '/clusters/prod/services?sort=Name');

    liveApply();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(first?.signal.aborted).toBe(true);
    expect(liveState.streamPath).toBe('/clusters/prod/services/_stream?sort=Name');
});

test('a standing fallback is idempotent until an explicit retry', () => {
    renderLivePage(true);
    const fetchMock = installAbortablePendingFetch();
    liveApply();

    liveApply();

    expect(fetchMock).not.toHaveBeenCalled();
    expect(liveState.status).toBe('fallback');
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();
});

test('switching Off cleans even an inconsistent idle fallback state', () => {
    renderLivePage(true);
    installAbortablePendingFetch();
    liveApply();
    expect(liveFallbackSeconds()).toBe(5);
    liveState.status = 'idle';
    dependencies.refreshMode.mockReturnValue('Off');

    liveApply();

    expect(liveState.status).toBe('idle');
    expect(liveState.streamPath).toBe('');
    expect(liveState.abort).toBeNull();
    expect(liveFallbackSeconds()).toBe(0);
});

test('liveOnListSwap ignores pushes and every non-riding state', () => {
    window.history.replaceState(null, '', '/clusters/prod/pods?sort=Name');
    renderLivePage();
    const fetchMock = installAbortablePendingFetch();
    liveApply();

    liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: { roLivePush: true } }));
    expect(fetchMock).toHaveBeenCalledOnce();

    for (const status of ['idle', 'fallback', 'hidden'] as const) {
        liveState.status = status;
        liveOnListSwap(new CustomEvent('htmx:afterSwap'));
    }
    expect(fetchMock).toHaveBeenCalledOnce();
});

describe.each(['connecting', 'open'] as const)('%s list swaps', (status) => {
    test.each([
        [
            'prefers the final request path and preserves its raw query',
            {
                finalRequestPath: '/clusters/prod/pods/_table?f=status:Running,Pending',
                requestPath: '/clusters/prod/pods/_table?f=wrong',
            },
            '/clusters/prod/pods/_stream?f=status:Running,Pending',
        ],
        [
            'falls back to a queryless request path',
            { requestPath: '/clusters/prod/services/_table' },
            '/clusters/prod/services/_stream',
        ],
        [
            'does not rewrite a table-looking query value',
            { finalRequestPath: '/clusters/prod/pods?next=/_table' },
            '/clusters/prod/pods/_stream?sort=Name',
        ],
        [
            'uses the live location when pathInfo is absent',
            undefined,
            '/clusters/prod/pods/_stream?sort=Name',
        ],
        [
            'uses the live location when the request path is not a string',
            { finalRequestPath: 42 },
            '/clusters/prod/pods/_stream?sort=Name',
        ],
        [
            'uses the live location when a present detail has no path info',
            null,
            '/clusters/prod/pods/_stream?sort=Name',
        ],
    ] as const)('%s', (_name, pathInfo, expectedBase) => {
        window.history.replaceState(null, '', '/clusters/prod/pods?sort=Name');
        renderLivePage();
        const fetchMock = installAbortablePendingFetch();
        liveApply();
        liveState.status = status;
        const first = liveState.abort;
        const detail = pathInfo === undefined ? undefined : { pathInfo: pathInfo ?? undefined };

        liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail }));

        expect(first?.signal.aborted).toBe(true);
        expect(liveState.streamPath).toBe(expectedBase);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        const separator = expectedBase.includes('?') ? '&' : '?';
        expect(fetchMock.mock.calls[1][0]).toBe(`${expectedBase}${separator}g=${liveState.gen}`);
    });
});

test('a fetch connection failure enters silent polling fallback', async () => {
    renderLivePage();
    installFetch(async () => {
        throw new Error('offline');
    });

    liveApply();
    const ctrl = liveState.abort;

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(ctrl?.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledTimes(2);
});

test.each([
    ['HTTP 204 with no body', () => statusResponse(204)],
    ['HTTP 429 with no body', () => statusResponse(429)],
    ['HTTP 200 with no body', () => statusResponse(200)],
    ['HTTP 503 even with a body', () => streamResponse([], 503)],
] as const)('%s enters silent polling fallback', async (_name, response) => {
    renderLivePage();
    installFetch(async () => response());

    liveApply();
    const ctrl = liveState.abort;

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(ctrl?.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test.each(['rejects', 'resolves'] as const)(
    'a superseded connection that later %s cannot alter the replacement stream',
    async (outcome) => {
        const firstResponse = deferred<Response>();
        const replacement = controlledStream();
        renderLivePage();
        let fetchCount = 0;
        installFetch(() => {
            fetchCount += 1;
            return fetchCount === 1 ? firstResponse.promise : Promise.resolve(replacement.response);
        });

        liveApply();
        const first = liveState.abort;
        liveApply(true);
        const second = liveState.abort;
        await vi.waitFor(() => expect(liveState.status).toBe('open'));

        if (outcome === 'rejects') {
            firstResponse.reject(new Error('old connection failed'));
        } else {
            firstResponse.resolve(statusResponse(429));
        }
        await Promise.resolve();
        await Promise.resolve();

        expect(first?.signal.aborted).toBe(true);
        expect(second).not.toBeNull();
        expect(liveState.abort).toBe(second);
        expect(liveState.status).toBe('open');
        expect(liveFallbackSeconds()).toBe(0);
        expect(dependencies.scheduleRefreshTick).toHaveBeenCalledTimes(2);
        expect(dependencies.markListStale).not.toHaveBeenCalled();
        liveTeardown();
        replacement.close();
    },
);

test('a superseded reader goes inert before dispatching its next chunk', async () => {
    const oldStream = controlledStream();
    const replacement = controlledStream();
    const content = renderLivePage();
    const htmx = installHtmx();
    let fetchCount = 0;
    installFetch(async () => {
        fetchCount += 1;
        return fetchCount === 1 ? oldStream.response : replacement.response;
    });

    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('open'));
    liveApply(true);
    const replacementController = liveState.abort;
    await vi.waitFor(() => expect(liveState.status).toBe('open'));

    oldStream.enqueue(
        `event: ro-table\ndata: ${JSON.stringify({ g: liveState.gen, html: '<p>old</p>' })}\n\n`,
    );
    await Promise.resolve();
    await Promise.resolve();

    expect(document.getElementById('resource-list-content')).toBe(content);
    expect(htmx.swap).not.toHaveBeenCalled();
    expect(liveState.abort).toBe(replacementController);
    expect(liveState.status).toBe('open');
    expect(liveFallbackSeconds()).toBe(0);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
    liveTeardown();
    replacement.close();
});

test('a stream closed after teardown cannot engage EOF fallback', async () => {
    const stream = controlledStream();
    renderLivePage();
    installFetch(async () => stream.response);
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('open'));

    liveTeardown();
    liveState.status = 'hidden';
    stream.close();
    // Let the reader's EOF continuation finish, not merely the close promise's
    // first microtask. The post-read supersession guard must own the outcome.
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(liveState.status).toBe('hidden');
    expect(liveFallbackSeconds()).toBe(0);
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test.each([
    ['a reader failure', [new Error('stream dropped')]],
    ['terminal-less EOF', []],
] as const)('%s enters banner polling fallback', async (_name, parts) => {
    renderLivePage();
    installFetch(async () => streamResponse([...parts]));

    liveApply();
    const ctrl = liveState.abort;

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(ctrl?.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('an ro-terminal frame stops reading and enters banner polling fallback', async () => {
    renderLivePage();
    const htmx = installHtmx();
    installFetch(async () => {
        const laterTable = JSON.stringify({ g: liveState.gen, html: '<p>must-not-land</p>' });
        return streamResponse([
            `event: ro-terminal\r\ndata: {"g":"server","reason":"auth"}\r\n\r\nevent: ro-table\r\ndata: ${laterTable}\r\n\r\n`,
        ]);
    });

    liveApply();
    const ctrl = liveState.abort;

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(ctrl?.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
    expect(htmx.swap).not.toHaveBeenCalled();
});

test('parses split CRLF and multi-data frames, skips malformed payloads, and swaps exactly', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
    content.dataset.roEtag = 'W/"last-good"';
    content.dataset.roEtagPath = '/clusters/prod/pods/_table';
    htmx.swap.mockImplementationOnce(() => {
        // Invalidation is synchronous, before a View-Transition-delayed swap
        // could let a terminal frame engage fallback polling.
        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
    });
    installFetch(async () => {
        const payload = JSON.stringify({
            g: liveState.gen,
            html: '<tbody data-state="fresh"><tr><td>pod-a</td></tr></tbody>',
        });
        const split = payload.indexOf('"html"');
        return streamResponse([
            'event: ignored\r\ndata: {}\r\n\r\nevent: ro-table\r\ndata: {broken}\r\n\r\n',
            `event: ro-table\r\ndata: ${payload.slice(0, split)}\r`,
            `\ndata: ${payload.slice(split)}\r\n\r\n`,
        ]);
    });

    liveApply();

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(htmx.swap).toHaveBeenCalledExactlyOnceWith(
        content,
        '<tbody data-state="fresh"><tr><td>pod-a</td></tr></tbody>',
        { swapStyle: 'morph' },
        {
            contextElement: content,
            eventInfo: { target: content, roLivePush: true },
        },
    );
});

test('preserves split UTF-8 and data lines without the optional leading space', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
    installFetch(async () => {
        const frame = `event: ro-table\ndata:${JSON.stringify({
            g: liveState.gen,
            html: '<p>pod-🐝 keeps this space</p>',
        })}\n\n`;
        const encoded = new TextEncoder().encode(frame);
        const beeStart = frame.indexOf('🐝');
        const byteSplit = new TextEncoder().encode(frame.slice(0, beeStart)).length + 2;
        return streamResponse([encoded.slice(0, byteSplit), encoded.slice(byteSplit)]);
    });

    liveApply();

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(htmx.swap).toHaveBeenCalledWith(
        content,
        '<p>pod-🐝 keeps this space</p>',
        expect.anything(),
        expect.anything(),
    );
});

test('reads a Fetch body that exposes getReader without an async iterator', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
    installFetch(async () => {
        const payload = JSON.stringify({ g: liveState.gen, html: '<p>reader-only</p>' });
        const response = readerOnlyResponse([`event: ro-table\ndata: ${payload}\n\n`]);
        expect(Symbol.asyncIterator in (response.body as unknown as object)).toBe(false);
        return response;
    });

    liveApply();

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(htmx.swap).toHaveBeenCalledWith(
        content,
        '<p>reader-only</p>',
        expect.anything(),
        expect.anything(),
    );
});

test('rejects JSON made valid only by illegally concatenating SSE data lines', async () => {
    renderLivePage();
    const htmx = installHtmx();
    installFetch(async () => {
        const invalid = JSON.stringify({
            g: liveState.gen,
            html: '<p>must-not-land</p>',
        });
        const split = invalid.indexOf('not-land');
        const valid = JSON.stringify({ g: liveState.gen, html: '<p>fresh</p>' });
        return streamResponse([
            `event: ro-table\ndata:${invalid.slice(0, split)}\ndata:${invalid.slice(split)}\n\n` +
                `event: ro-table\ndata:${valid}\n\n`,
        ]);
    });

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(htmx.swap).toHaveBeenCalledExactlyOnceWith(
        expect.any(Element),
        '<p>fresh</p>',
        expect.anything(),
        expect.anything(),
    );
});

test('skips empty, malformed, wrong-event, and schema-invalid frames, then keeps reading', async () => {
    renderLivePage();
    const htmx = installHtmx();
    installFetch(async () => {
        const invalidFrames = [
            'event: ro-terminal\ndata: []\n\n',
            'event: ro-terminal\ndata: "scalar"\n\n',
            'event: ro-table\ndata: null\n\n',
            'event: ro-table\ndata: "scalar"\n\n',
            'event: ro-table\ndata: {}\n\n',
            `event: ro-table\ndata: ${JSON.stringify({ g: 7, html: '<p>number-g</p>' })}\n\n`,
            `event: ro-table\ndata: ${JSON.stringify({ g: liveState.gen })}\n\n`,
            `event: ro-table\ndata: ${JSON.stringify({ g: liveState.gen, html: null })}\n\n`,
            `event: ignored\ndata: ${JSON.stringify({ g: liveState.gen, html: '<p>wrong-event</p>' })}\n\n`,
            `event: ro-table\njunk:${JSON.stringify({ g: liveState.gen, html: '<p>wrong-field</p>' })}\n\n`,
            'event: ro-table\n\n',
            'event: ro-table\ndata: {broken}\n\n',
        ];
        const first = JSON.stringify({ g: liveState.gen, html: '<p>first-valid</p>' });
        const second = JSON.stringify({ g: liveState.gen, html: '<p>second-valid</p>' });
        return streamResponse([
            `${invalidFrames.join('')}event: ro-table\ndata: ${first}\n\nevent: ro-table\ndata: ${second}\n\n`,
        ]);
    });

    liveApply();

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledTimes(2));
    expect(htmx.swap.mock.calls.map((call) => call[1])).toStrictEqual([
        '<p>first-valid</p>',
        '<p>second-valid</p>',
    ]);
});

test('a discarded generation does not stop the following fresh snapshot', async () => {
    renderLivePage();
    const htmx = installHtmx();
    const before = window.roLive.discards();
    installFetch(async () => {
        const stale = JSON.stringify({ g: `${liveState.gen}.old`, html: '<p>old</p>' });
        const fresh = JSON.stringify({ g: liveState.gen, html: '<p>fresh</p>' });
        return streamResponse([
            `event: ro-table\ndata: ${stale}\n\nevent: ro-table\ndata: ${fresh}\n\n`,
        ]);
    });

    liveApply();

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(window.roLive.discards()).toBe(before + 1);
    expect(htmx.swap.mock.calls[0][1]).toBe('<p>fresh</p>');
});

test.each(['missing-content', 'missing-htmx', 'non-callable-swap'] as const)(
    '%s safely ignores an otherwise valid pushed fragment',
    async (missing) => {
        const content = renderLivePage();
        const stream = controlledStream();
        const swap = vi.fn();
        if (missing === 'missing-htmx') {
            delete (window as unknown as { htmx?: HtmxHarness }).htmx;
        } else if (missing === 'non-callable-swap') {
            (window as unknown as { htmx: { swap: null } }).htmx = { swap: null };
        } else {
            (window as unknown as { htmx: HtmxHarness }).htmx = { swap };
        }
        installFetch(async () => stream.response);
        liveApply();
        await vi.waitFor(() => expect(liveState.status).toBe('open'));
        if (missing === 'missing-content') {
            content.remove();
        }

        stream.enqueue(
            `event: ro-table\ndata: ${JSON.stringify({ g: liveState.gen, html: '<p>fresh</p>' })}\n\n`,
        );

        await vi.waitFor(() =>
            expect(dependencies.pruneSettledListRequests).toHaveBeenCalledTimes(2),
        );
        await Promise.resolve();
        expect(swap).not.toHaveBeenCalled();
        expect(liveState.status).toBe('open');
        expect(liveFallbackSeconds()).toBe(0);
        liveTeardown();
        stream.close();
    },
);

describe.each([
    'stale-generation',
    'wrong-page',
    'user-request-in-flight',
    'container-request-in-flight',
] as const)('%s discard', (reason) => {
    test('drops the full frame before htmx.swap and increments observability', async () => {
        const response = deferred<Response>();
        const content = renderLivePage();
        content.dataset.roEtag = 'W/"last-good"';
        content.dataset.roEtagPath = '/clusters/prod/pods/_table';
        const htmx = installHtmx();
        installFetch(() => response.promise);
        window.history.replaceState(null, '', '/clusters/prod/pods');
        liveApply();
        const before = window.roLive.discards();
        const generation = reason === 'stale-generation' ? `${liveState.gen}.old` : liveState.gen;

        if (reason === 'wrong-page') {
            window.history.replaceState(null, '', '/clusters/prod/services');
        }
        if (reason === 'user-request-in-flight') {
            dependencies.userRequests.add(xhrAt(1));
        }
        if (reason === 'container-request-in-flight') {
            dependencies.containerRequests.add(xhrAt(1));
        }
        response.resolve(
            streamResponse([
                `event: ro-table\ndata: ${JSON.stringify({ g: generation, html: '<p>wrong</p>' })}\n\n`,
            ]),
        );

        await vi.waitFor(() => expect(window.roLive.discards()).toBe(before + 1));
        expect(htmx.swap).not.toHaveBeenCalled();
        expect(dependencies.pruneSettledListRequests).toHaveBeenCalledTimes(2);
        expect(content.dataset.roEtag).toBe('W/"last-good"');
        expect(content.dataset.roEtagPath).toBe('/clusters/prod/pods/_table');
    });
});

test('settled requests are pruned and do not block a fresh frame', async () => {
    renderLivePage();
    const htmx = installHtmx();
    dependencies.userRequests.add(xhrAt(4));
    dependencies.containerRequests.add(xhrAt(0));
    installFetch(async () => {
        const payload = JSON.stringify({ g: liveState.gen, html: '<p>fresh</p>' });
        return streamResponse([`event: ro-table\ndata: ${payload}\n\n`]);
    });

    liveApply();

    await vi.waitFor(() => expect(htmx.swap).toHaveBeenCalledOnce());
    expect(dependencies.userRequests).toHaveLength(0);
    expect(dependencies.containerRequests).toHaveLength(0);
    expect(dependencies.pruneSettledListRequests).toHaveBeenCalledTimes(2);
});

describe('visibility lifecycle', () => {
    test.each(['open', 'connecting'] as const)('hiding a %s stream closes it', (status) => {
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
        const ctrl = new AbortController();
        liveState.status = status;
        liveState.abort = ctrl;
        liveState.streamPath = '/clusters/prod/pods/_stream';

        document.dispatchEvent(new Event('visibilitychange'));

        expect(ctrl.signal.aborted).toBe(true);
        expect(liveState.abort).toBeNull();
        expect(liveState.status).toBe('hidden');
        expect(liveFallbackSeconds()).toBe(0);
    });

    test.each(['idle', 'fallback', 'hidden'] as const)(
        'hiding leaves the %s state untouched',
        (status) => {
            vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
            const ctrl = new AbortController();
            liveState.status = status;
            liveState.abort = ctrl;

            document.dispatchEvent(new Event('visibilitychange'));

            expect(ctrl.signal.aborted).toBe(false);
            expect(liveState.abort).toBe(ctrl);
            expect(liveState.status).toBe(status);
        },
    );

    test('showing a hidden Live stream reopens it', () => {
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        window.history.replaceState(null, '', '/clusters/prod/pods?sort=Name');
        renderLivePage();
        const fetchMock = installAbortablePendingFetch();
        liveState.status = 'hidden';
        liveState.streamPath = '/clusters/prod/pods/_stream?sort=Name';

        document.dispatchEvent(new Event('visibilitychange'));

        expect(fetchMock).toHaveBeenCalledOnce();
        expect(liveState.status).toBe('connecting');
        expect(liveState.streamPath).toBe('/clusters/prod/pods/_stream?sort=Name');
    });

    test.each([
        ['hidden but Off', 'hidden', 'Off'],
        ['fallback while Live', 'fallback', 'Live'],
        ['idle while Live', 'idle', 'Live'],
    ] as const)('showing %s does not open a stream', (_name, status, mode) => {
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        renderLivePage();
        const fetchMock = installAbortablePendingFetch();
        dependencies.refreshMode.mockReturnValue(mode);
        liveState.status = status;

        document.dispatchEvent(new Event('visibilitychange'));

        expect(fetchMock).not.toHaveBeenCalled();
        expect(liveState.status).toBe(status);
    });

    test('showing a hidden stream on a now-unsupported page enters fallback', () => {
        vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
        renderLivePage(true);
        const fetchMock = installAbortablePendingFetch();
        liveState.status = 'hidden';
        liveState.streamPath = '/clusters/prod/pods/_stream';

        document.dispatchEvent(new Event('visibilitychange'));

        expect(fetchMock).not.toHaveBeenCalled();
        expect(liveState.status).toBe('fallback');
        expect(liveState.streamPath).toBe('');
        expect(liveFallbackSeconds()).toBe(5);
        expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();
    });
});
