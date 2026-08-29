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

    const { liveApply, liveFallbackSeconds, liveState, liveTeardown } = await import('./live.js');

    expect(liveState).toStrictEqual({
        status: 'idle',
        abort: null,
        gen: '',
        streamPath: '',
    });
    expect(liveFallbackSeconds()).toBe(0);
    expect(window.roLive.discards()).toBe(0);
    expect(window.roLive.stats().state).toBe('idle');
    expect(addEventListener).toHaveBeenCalledWith('visibilitychange', expect.any(Function));

    document.body.innerHTML = `
        <div id="resource-list-content" data-live-url="location"></div>
        <button data-ro-action="set-refresh" data-ro-interval="Live"></button>`;
    window.history.replaceState(null, '', '/clusters/prod/pods');
    vi.stubGlobal(
        'fetch',
        vi.fn(() => new Promise<Response>(() => {})),
    );
    liveApply();
    const ctrl = liveState.abort as AbortController;
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

    document.dispatchEvent(new Event('visibilitychange'));

    expect(ctrl.signal.aborted).toBe(true);
    expect(liveState.abort).toBeNull();
    expect(liveState.status).toBe('hidden');
    liveTeardown();
});
