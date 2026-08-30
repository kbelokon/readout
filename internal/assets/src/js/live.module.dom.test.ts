// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

import type { LiveV2Cursor } from './live-protocol.js';

const dependencies = vi.hoisted(() => ({
    clearLiveStale: vi.fn(),
    markLiveStale: vi.fn(),
    markLiveUnavailable: vi.fn(),
    noteStaleRetryAt: vi.fn(),
    revealLiveStale: vi.fn(),
    isLiveEnabled: vi.fn(() => true),
    resetListRequestTracker: vi.fn(),
    subscribeListRequests: vi.fn(() => () => {}),
}));

vi.mock('./refresh.js', () => ({
    isLiveEnabled: dependencies.isLiveEnabled,
    listRequestTrackerSnapshot: () => ({ count: 0 }),
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

afterEach(() => {
    vi.doUnmock('./live-protocol.js');
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    document.body.replaceChildren();
    delete (window as unknown as { htmx?: unknown }).htmx;
});

test('module load publishes immutable diagnostics and registers the lifecycle listeners', async () => {
    vi.doUnmock('./live-protocol.js');
    vi.resetModules();
    const documentListener = vi.spyOn(document, 'addEventListener');
    const windowListener = vi.spyOn(window, 'addEventListener');

    await import('./live.js');

    expect(window.roLive.stats()).toStrictEqual({
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
        state: 'off',
        seq: 0,
        attempt: 0,
        inFlightRequests: 0,
        resyncsInWindow: 0,
    });
    // The three lifecycle signals the transport reacts to, and nothing else:
    // the request tracker is only subscribed once liveApply actually runs.
    expect(documentListener).toHaveBeenCalledWith('visibilitychange', expect.any(Function));
    expect(windowListener).toHaveBeenCalledWith('offline', expect.any(Function));
    expect(windowListener).toHaveBeenCalledWith('online', expect.any(Function));
    expect(dependencies.subscribeListRequests).not.toHaveBeenCalled();
});

test.each([
    { hasRV: false, name: 'rv-less', rv: undefined },
    { hasRV: true, name: 'rv-bearing', rv: '10' },
])('a $name snapshot preserves the optional cursor property', async ({ hasRV, rv }) => {
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
    let generation = '';
    vi.stubGlobal(
        'fetch',
        vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
            const body = new ReadableStream<Uint8Array>({
                start(value) {
                    controller = value;
                },
            });
            const headers = init?.headers as Record<string, string> | undefined;
            generation = headers?.['RO-Live-Generation'] || '';
            return {
                status: 200,
                body,
                headers: new Headers({ 'Content-Type': 'text/event-stream' }),
            } as Response;
        }),
    );
    document.body.innerHTML = `
        <div id="resource-list-content" data-live-url="location"></div>
        <button data-ro-action="toggle-live" aria-pressed="true"></button>`;
    window.history.replaceState(null, '', '/clusters/prod/pods');

    const { liveApply, liveOnListSwap, liveResetPage } = await import('./live.js');
    (window as unknown as { htmx: { swap: (...args: unknown[]) => void } }).htmx = {
        swap(_target, _html, _spec, options) {
            const eventInfo = (options as { eventInfo: Record<string, unknown> }).eventInfo;
            liveOnListSwap(new CustomEvent('htmx:afterSwap', { detail: eventInfo }));
        },
    };
    const frame = (value: unknown) =>
        new TextEncoder().encode(`event: ro-live\ndata: ${JSON.stringify(value)}\n\n`);

    liveApply();
    await vi.waitFor(() => expect(generation).not.toBe(''));
    const snapshot: Record<string, unknown> = {
        v: 2,
        kind: 'snapshot',
        g: generation,
        seq: 1,
        rev: 'rev-1',
        schema: 'schema-1',
        snapshot: { html: '<p>snapshot</p>' },
    };
    if (rv !== undefined) snapshot.rv = rv;
    controller.enqueue(frame(snapshot));
    await vi.waitFor(() => expect(window.roLive.stats().state).toBe('open'));
    controller.enqueue(
        frame({
            v: 2,
            kind: 'delta',
            g: generation,
            seq: 2,
            rev: 'rev-2',
            schema: 'schema-1',
            delta: {
                base: 'rev-1',
                rev: 'rev-2',
                regions: [
                    {
                        region: 'count',
                        html: '<span data-ro-live-region="count">2</span>',
                    },
                ],
            },
        }),
    );
    await vi.waitFor(() => expect(window.roLive.stats().seq).toBe(2));

    expect(cursorHasRV).toBe(hasRV);
    expect(cursorRV).toBe(rv);
    liveResetPage();
});
