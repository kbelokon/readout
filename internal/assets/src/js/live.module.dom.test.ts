// @vitest-environment jsdom

import { expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    containerRequests: new Set<XMLHttpRequest>(),
    markListStale: vi.fn(),
    pruneSettledListRequests: vi.fn(),
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

test('module load publishes clean state and registers the visibility lifecycle', async () => {
    vi.resetModules();
    const addEventListener = vi.spyOn(document, 'addEventListener');

    const { liveFallbackSeconds, liveState } = await import('./live.js');

    expect(liveState).toStrictEqual({
        status: 'idle',
        abort: null,
        gen: '',
        streamPath: '',
    });
    expect(liveFallbackSeconds()).toBe(0);
    expect(window.roLive.discards()).toBe(0);
    expect(addEventListener).toHaveBeenCalledWith('visibilitychange', expect.any(Function));

    const ctrl = new AbortController();
    liveState.status = 'connecting';
    liveState.abort = ctrl;
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

    document.dispatchEvent(new Event('visibilitychange'));

    expect(ctrl.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveState.status).toBe('hidden');
});
