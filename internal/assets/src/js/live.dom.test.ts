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

interface HtmxHarness {
    swap: ReturnType<typeof vi.fn>;
}

interface Deferred<T> {
    promise: Promise<T>;
    resolve(value: T): void;
}

function deferred<T>(): Deferred<T> {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((done) => {
        resolve = done;
    });
    return { promise, resolve };
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

function streamResponse(parts: Array<string | Error>): Response {
    const queue = [...parts];
    const reader = {
        read: vi.fn(async () => {
            const next = queue.shift();
            if (next instanceof Error) {
                throw next;
            }
            if (next === undefined) {
                return { done: true, value: undefined };
            }
            return { done: false, value: new TextEncoder().encode(next) };
        }),
    };
    return {
        status: 200,
        body: { getReader: () => reader },
    } as unknown as Response;
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

test('liveApply derives the raw stream URL, is idempotent, force-reopens, and tears down', () => {
    window.history.replaceState(
        null,
        '',
        '/clusters/prod/namespaces/default/pods/?f=status:Running,Pending&sort=Age%3Adesc',
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

test('an unsupported Live page degrades to polling without opening a stream', () => {
    renderLivePage(true);
    const fetchMock = installAbortablePendingFetch();

    liveApply();

    expect(fetchMock).not.toHaveBeenCalled();
    expect(liveState.status).toBe('fallback');
    expect(liveState.streamPath).toBe('');
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledOnce();
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test('liveOnListSwap ignores pushes and reopens from the exact request path', () => {
    window.history.replaceState(null, '', '/clusters/prod/pods?sort=Name');
    renderLivePage();
    const fetchMock = installAbortablePendingFetch();
    liveApply();
    const first = liveState.abort;

    liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: { roLivePush: true } }));
    expect(fetchMock).toHaveBeenCalledOnce();

    liveOnListSwap(
        new CustomEvent('htmx:afterSwap', {
            detail: {
                pathInfo: {
                    finalRequestPath: '/clusters/prod/pods/_table?f=status:Running,Pending',
                },
            },
        }),
    );

    const base = '/clusters/prod/pods/_stream?f=status:Running,Pending';
    expect(first?.signal.aborted).toBe(true);
    expect(liveState.streamPath).toBe(base);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toBe(`${base}&g=${liveState.gen}`);

    liveTeardown();
    liveState.status = 'fallback';
    liveOnListSwap(new CustomEvent('htmx:afterSwap'));
    expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('a fetch connection failure enters silent polling fallback', async () => {
    renderLivePage();
    installFetch(async () => {
        throw new Error('offline');
    });

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
    expect(dependencies.scheduleRefreshTick).toHaveBeenCalledTimes(2);
});

test.each([204, 429])('HTTP %s enters silent polling fallback', async (status) => {
    renderLivePage();
    installFetch(async () => statusResponse(status));

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).not.toHaveBeenCalled();
});

test.each([
    ['a reader failure', [new Error('stream dropped')]],
    ['terminal-less EOF', []],
] as const)('%s enters banner polling fallback', async (_name, parts) => {
    renderLivePage();
    installFetch(async () => streamResponse([...parts]));

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('an ro-terminal frame stops reading and enters banner polling fallback', async () => {
    renderLivePage();
    installFetch(async () =>
        streamResponse(['event: ro-terminal\r\ndata: {"g":"server","reason":"auth"}\r\n\r\n']),
    );

    liveApply();

    await vi.waitFor(() => expect(liveState.status).toBe('fallback'));
    expect(liveFallbackSeconds()).toBe(5);
    expect(dependencies.markListStale).toHaveBeenCalledOnce();
});

test('parses split CRLF and multi-data frames, skips malformed payloads, and swaps exactly', async () => {
    const content = renderLivePage();
    const htmx = installHtmx();
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

describe.each(['stale-generation', 'wrong-page', 'request-in-flight'] as const)(
    '%s discard',
    (reason) => {
        test('drops the full frame before htmx.swap and increments observability', async () => {
            const response = deferred<Response>();
            renderLivePage();
            const htmx = installHtmx();
            installFetch(() => response.promise);
            window.history.replaceState(null, '', '/clusters/prod/pods');
            liveApply();
            const before = window.roLive.discards();
            const generation =
                reason === 'stale-generation' ? `${liveState.gen}.old` : liveState.gen;

            if (reason === 'wrong-page') {
                window.history.replaceState(null, '', '/clusters/prod/services');
            }
            if (reason === 'request-in-flight') {
                dependencies.userRequests.add(xhrAt(1));
            }
            response.resolve(
                streamResponse([
                    `event: ro-table\ndata: ${JSON.stringify({ g: generation, html: '<p>wrong</p>' })}\n\n`,
                ]),
            );

            await vi.waitFor(() => expect(window.roLive.discards()).toBe(before + 1));
            expect(htmx.swap).not.toHaveBeenCalled();
            expect(dependencies.pruneSettledListRequests).toHaveBeenCalledTimes(2);
        });
    },
);
