// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

import type { LiveV2Cursor } from './live-protocol.js';

const dependencies = vi.hoisted(() => ({
    markListStale: vi.fn(),
    refreshMode: vi.fn(() => 'Live'),
    scheduleRefreshTick: vi.fn(),
}));

vi.mock('./refresh.js', () => ({
    refreshMode: dependencies.refreshMode,
    scheduleRefreshTick: dependencies.scheduleRefreshTick,
}));
vi.mock('./stale.js', () => ({
    markListStale: dependencies.markListStale,
}));

afterEach(() => {
    vi.doUnmock('./live-protocol.js');
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    document.body.replaceChildren();
    delete (window as unknown as { htmx?: unknown }).htmx;
});

test('module load publishes clean state and registers the visibility lifecycle', async () => {
    vi.doUnmock('./live-protocol.js');
    vi.resetModules();
    const addEventListener = vi.spyOn(document, 'addEventListener');

    const { liveApply, liveBeforeListRequest, liveFallbackSeconds, liveState, liveTeardown } =
        await import('./live.js');

    expect(liveState).toStrictEqual({
        status: 'idle',
        abort: null,
        gen: '',
        streamPath: '',
    });
    expect(liveFallbackSeconds()).toBe(0);
    expect(window.roLive.discards()).toBe(0);
    expect(window.roLive.stats()).toStrictEqual({
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
        state: 'idle',
        protocol: null,
        seq: 0,
        inFlightRequests: 0,
        resyncsInWindow: 0,
    });
    expect(addEventListener).toHaveBeenCalledWith('visibilitychange', expect.any(Function));

    document.body.innerHTML = `
        <div id="resource-list-content" data-live-url="location"></div>
        <button data-ro-action="set-refresh" data-ro-interval="Live"></button>`;
    window.history.replaceState(null, '', '/clusters/prod/pods');
    const content = document.getElementById('resource-list-content') as HTMLElement;
    const request = new EventTarget() as XMLHttpRequest;
    Object.defineProperty(request, 'status', { value: 500 });
    liveBeforeListRequest(
        new CustomEvent('htmx:beforeRequest', { detail: { target: content, xhr: request } }),
    );
    expect(liveState.status).toBe('idle');
    await Promise.resolve();

    liveState.status = 'hidden';
    const hiddenRequest = new EventTarget() as XMLHttpRequest;
    Object.defineProperty(hiddenRequest, 'status', { value: 500 });
    liveBeforeListRequest(
        new CustomEvent('htmx:beforeRequest', {
            detail: { target: content, xhr: hiddenRequest },
        }),
    );
    expect(liveState.status).toBe('hidden');
    await Promise.resolve();
    liveState.status = 'idle';

    const fetchMock = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => {
        if (fetchMock.mock.calls.length === 1) {
            return Promise.resolve({
                status: 200,
                body: new ReadableStream<Uint8Array>(),
                headers: new Headers({ 'Content-Type': 'application/json' }),
            } as Response);
        }
        return new Promise<Response>(() => {});
    });
    vi.stubGlobal('fetch', fetchMock);
    liveApply();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    const ctrl = liveState.abort as AbortController;
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

    document.dispatchEvent(new Event('visibilitychange'));

    expect(ctrl.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveState.status).toBe('hidden');

    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    expect(fetchMock.mock.calls[2][0]).toMatch(/^\/clusters\/prod\/pods\/_stream\?g=/u);
    expect(window.roLive.stats().resyncs).toBe(1);
    liveTeardown();
});

test.each([
    { hasRV: false, name: 'rv-less', rv: undefined },
    { hasRV: true, name: 'rv-bearing', rv: '10' },
])('a $name snapshot preserves its optional cursor property', async ({ hasRV, rv }) => {
    let cursorHasRV: boolean | null = null;
    let cursorRV: string | null | undefined = null;
    vi.doMock('./live-protocol.js', async (importOriginal) => {
        const actual = await importOriginal<typeof import('./live-protocol.js')>();
        return {
            ...actual,
            applyLiveV2Delta(_input: unknown, cursor: LiveV2Cursor) {
                cursorHasRV = Object.hasOwn(cursor, 'rv');
                cursorRV = cursor.rv;
                return {
                    ok: true as const,
                    cursor: {
                        g: cursor.g,
                        seq: cursor.seq + 1,
                        screen: cursor.screen,
                        rev: 'rev-2',
                        schema: cursor.schema,
                    },
                    summary: {
                        inserted: 0,
                        updated: 0,
                        deleted: 0,
                        projected: 0,
                        reordered: false,
                        regions: ['count'],
                    },
                };
            },
        };
    });
    vi.resetModules();

    let controller!: ReadableStreamDefaultController<Uint8Array>;
    vi.stubGlobal(
        'fetch',
        vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
            const body = new ReadableStream<Uint8Array>({
                start(value) {
                    controller = value;
                },
            });
            const headers = init?.headers as Record<string, string> | undefined;
            const generation = headers?.['RO-Live-Generation'] || '';
            return {
                status: 200,
                body,
                headers: new Headers({
                    'Content-Type': 'text/event-stream',
                    'RO-Live-Version': '2',
                    'RO-Live-Generation': generation,
                }),
            } as Response;
        }),
    );
    document.body.innerHTML = `
        <div id="resource-list-content" data-live-url="location"></div>
        <button data-ro-action="set-refresh" data-ro-interval="Live"></button>`;
    window.history.replaceState(null, '', '/clusters/prod/pods');

    const { liveApply, liveOnListSwap, liveState, liveTeardown } = await import('./live.js');
    (window as unknown as { htmx: { swap: (...args: unknown[]) => void } }).htmx = {
        swap(_target, _html, _spec, options) {
            const eventInfo = (options as { eventInfo: Record<string, unknown> }).eventInfo;
            liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: eventInfo }));
        },
    };
    liveApply();
    await vi.waitFor(() => expect(liveState.status).toBe('syncing-v2'));
    const frame = (value: unknown) =>
        new TextEncoder().encode(`event: ro-live\ndata: ${JSON.stringify(value)}\n\n`);
    const snapshot: Record<string, unknown> = {
        v: 2,
        kind: 'snapshot',
        g: liveState.gen,
        seq: 1,
        screen: '/clusters/prod/pods',
        rev: 'rev-1',
        schema: 'schema-1',
        snapshot: { html: '<p>snapshot</p>' },
    };
    if (rv !== undefined) snapshot.rv = rv;
    controller.enqueue(frame(snapshot));
    await vi.waitFor(() => expect(liveState.status).toBe('open-v2'));
    controller.enqueue(
        frame({
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
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            },
        }),
    );
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));

    expect(cursorHasRV).toBe(hasRV);
    expect(cursorRV).toBe(rv);
    liveTeardown();
});
