// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    liveApply: vi.fn(),
    liveFallbackSeconds: vi.fn(() => 0),
    readPrefs: vi.fn(() => ({ kinds: [], refresh: '', ns: {} })),
    roPrefsSetRefresh: vi.fn(),
    updateStaleCountdown: vi.fn(),
}));

vi.mock('./live.js', () => ({
    liveApply: dependencies.liveApply,
    liveFallbackSeconds: dependencies.liveFallbackSeconds,
}));
vi.mock('./prefs.js', () => ({
    REFRESH_KEY: 'roRefresh',
    readPrefs: dependencies.readPrefs,
    roPrefsSetRefresh: dependencies.roPrefsSetRefresh,
}));
vi.mock('./stale.js', () => ({
    updateStaleCountdown: dependencies.updateStaleCountdown,
}));

import {
    applyRefresh,
    containerListRequestsInFlight,
    fireRefresh,
    noteRefreshFailure,
    noteRefreshRecovery,
    pruneSettledListRequests,
    refreshBindings,
    refreshMode,
    refreshNextAtMs,
    requestListRefresh,
    scheduleRefreshTick,
    syncRefreshUI,
    userListRequestsInFlight,
} from './refresh.js';

interface HtmxHarness {
    ajax: ReturnType<typeof vi.fn>;
    trigger: ReturnType<typeof vi.fn>;
}

function installHtmx(): HtmxHarness {
    const htmx = { ajax: vi.fn(), trigger: vi.fn() };
    (window as unknown as { htmx: HtmxHarness }).htmx = htmx;
    return htmx;
}

function renderContent(liveURL?: string): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    if (liveURL !== undefined) {
        content.dataset.liveUrl = liveURL;
    }
    document.body.appendChild(content);
    return content;
}

function xhrAt(readyState: number): XMLHttpRequest {
    return { readyState } as unknown as XMLHttpRequest;
}

function dispatchHtmx(type: string, detail: Record<string, unknown>): void {
    document.dispatchEvent(new CustomEvent(type, { bubbles: true, detail }));
}

beforeEach(() => {
    dependencies.liveApply.mockReset();
    dependencies.liveFallbackSeconds.mockReset().mockReturnValue(0);
    dependencies.readPrefs.mockReset().mockReturnValue({ kinds: [], refresh: '', ns: {} });
    dependencies.roPrefsSetRefresh.mockReset();
    dependencies.updateStaleCountdown.mockReset();
    userListRequestsInFlight.clear();
    containerListRequestsInFlight.clear();
    delete (window as unknown as { htmx?: HtmxHarness }).htmx;
    delete window.roToast;

    // Reset the module-owned failure stage and retire any timer left by the
    // preceding test without re-importing the listener-owning module.
    applyRefresh();
    vi.clearAllMocks();
});

test('prunes only DONE and aborted XHRs from an in-flight set', () => {
    const aborted = xhrAt(0);
    const opened = xhrAt(1);
    const headersReceived = xhrAt(2);
    const loading = xhrAt(3);
    const done = xhrAt(4);
    const requests = new Set([aborted, opened, headersReceived, loading, done]);

    pruneSettledListRequests(requests);

    expect([...requests]).toStrictEqual([opened, headersReceived, loading]);
});

describe('htmx request lifecycle', () => {
    test('marks container requests RO-No-Push and tracks each request class', () => {
        const content = renderContent();
        const userSource = document.createElement('button');
        document.body.appendChild(userSource);
        const htmx = installHtmx();
        const headers: Record<string, string> = {};
        const containerXHR = xhrAt(1);
        const userXHR = xhrAt(1);

        dispatchHtmx('htmx:configRequest', { elt: content, headers });
        expect(headers).toStrictEqual({ 'RO-No-Push': 'true' });

        dispatchHtmx('htmx:beforeRequest', {
            elt: content,
            target: content,
            xhr: containerXHR,
        });
        expect(containerListRequestsInFlight).toContain(containerXHR);
        expect(htmx.trigger).not.toHaveBeenCalled();

        htmx.trigger.mockImplementationOnce(() => {
            expect(userListRequestsInFlight).toContain(userXHR);
        });
        dispatchHtmx('htmx:beforeRequest', {
            elt: userSource,
            target: content,
            xhr: userXHR,
        });
        expect(userListRequestsInFlight).toContain(userXHR);
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'htmx:abort');

        dispatchHtmx('htmx:afterRequest', { xhr: containerXHR });
        dispatchHtmx('htmx:afterRequest', { xhr: userXHR });
        expect(containerListRequestsInFlight).toHaveLength(0);
        expect(userListRequestsInFlight).toHaveLength(0);
    });

    test('does not track or abort a preloaded user request', () => {
        const content = renderContent();
        const userSource = document.createElement('a');
        const htmx = installHtmx();

        dispatchHtmx('htmx:beforeRequest', {
            elt: userSource,
            target: content,
            xhr: xhrAt(1),
            requestConfig: { headers: { 'HX-Preloaded': 'true' } },
        });

        expect(userListRequestsInFlight).toHaveLength(0);
        expect(htmx.trigger).not.toHaveBeenCalled();
    });
});

describe('refresh requests', () => {
    test('uses the live location path and exact current query for v2 lists', () => {
        window.history.replaceState(
            null,
            '',
            '/clusters/prod/namespaces/default/pods/?sort=Age%3Adesc&f=status%3ARunning',
        );
        const content = renderContent('location');
        const htmx = installHtmx();
        const catchRequest = vi.fn();
        htmx.ajax.mockReturnValue({ catch: catchRequest });

        requestListRefresh();

        expect(htmx.ajax).toHaveBeenCalledExactlyOnceWith(
            'GET',
            '/clusters/prod/namespaces/default/pods/_table?sort=Age%3Adesc&f=status%3ARunning',
            { source: content },
        );
        expect(catchRequest).toHaveBeenCalledWith(expect.any(Function));
        expect(htmx.trigger).not.toHaveBeenCalled();
    });

    test('uses ro:refresh for a legacy multi-type list', () => {
        const content = renderContent('/baked/table');
        const htmx = installHtmx();

        requestListRefresh();

        expect(htmx.ajax).not.toHaveBeenCalled();
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'ro:refresh');
    });

    test('retry aborts the old container request before issuing the replacement', () => {
        const content = renderContent('location');
        const htmx = installHtmx();
        const retry = refreshBindings.find(
            (binding) => binding.selector === '[data-ro-action="retry"]',
        );
        expect(retry).toBeDefined();
        const event = new Event('click', { cancelable: true });

        expect(retry?.handler(event, null)).toBe(true);

        expect(event.defaultPrevented).toBe(true);
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'htmx:abort');
        expect(htmx.ajax).toHaveBeenCalledOnce();
        expect(htmx.trigger.mock.invocationCallOrder[0]).toBeLessThan(
            htmx.ajax.mock.invocationCallOrder[0] as number,
        );
    });
});

test('fireRefresh is suppressed while hidden or while either request class is in flight', () => {
    renderContent('location');
    const htmx = installHtmx();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);

    fireRefresh();
    expect(htmx.ajax).not.toHaveBeenCalled();

    hidden.mockReturnValue(false);
    userListRequestsInFlight.add(xhrAt(1));
    fireRefresh();
    expect(htmx.ajax).not.toHaveBeenCalled();

    userListRequestsInFlight.clear();
    containerListRequestsInFlight.add(xhrAt(1));
    fireRefresh();
    expect(htmx.ajax).not.toHaveBeenCalled();

    containerListRequestsInFlight.clear();
    userListRequestsInFlight.add(xhrAt(4));
    containerListRequestsInFlight.add(xhrAt(0));
    fireRefresh();
    expect(userListRequestsInFlight).toHaveLength(0);
    expect(containerListRequestsInFlight).toHaveLength(0);
    expect(htmx.ajax).toHaveBeenCalledOnce();
});

test('migrates legacy localStorage once and then prefers the canonical preference', () => {
    let persisted = '';
    dependencies.readPrefs.mockImplementation(() => ({
        kinds: [],
        refresh: persisted,
        ns: {},
    }));
    dependencies.roPrefsSetRefresh.mockImplementation((mode: string) => {
        persisted = mode;
    });
    window.localStorage.setItem('roRefresh', '30');

    expect(refreshMode()).toBe('30');
    expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith('30');

    window.localStorage.setItem('roRefresh', '5');
    expect(refreshMode()).toBe('30');
    expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledOnce();
});

test('maintains one timer chain across scheduling, backoff, recovery, and a fired tick', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-08-25T10:00:00Z'));
    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '5', ns: {} });
    const content = renderContent('location');
    const htmx = installHtmx();
    htmx.ajax.mockReturnValue({ catch: vi.fn() });
    const toast = vi.fn();
    window.roToast = toast;
    const now = Date.now();

    applyRefresh();
    expect(refreshNextAtMs()).toBe(now + 5_000);
    expect(vi.getTimerCount()).toBe(1);

    scheduleRefreshTick();
    expect(vi.getTimerCount()).toBe(1);

    noteRefreshFailure();
    expect(refreshNextAtMs()).toBe(now + 5_000);
    expect(vi.getTimerCount()).toBe(1);

    noteRefreshFailure();
    expect(refreshNextAtMs()).toBe(now + 10_000);
    expect(vi.getTimerCount()).toBe(1);

    noteRefreshRecovery();
    expect(refreshNextAtMs()).toBe(now + 5_000);
    expect(vi.getTimerCount()).toBe(1);
    expect(toast).toHaveBeenCalledExactlyOnceWith('Refresh resumed');

    vi.advanceTimersByTime(5_000);
    expect(htmx.ajax).toHaveBeenCalledExactlyOnceWith('GET', '/_table', { source: content });
    expect(refreshNextAtMs()).toBe(now + 10_000);
    expect(vi.getTimerCount()).toBe(1);
    expect(dependencies.updateStaleCountdown).toHaveBeenCalled();
});

test('syncRefreshUI renders Live and numeric modes without cross-activating options', () => {
    document.body.innerHTML = `
        <div id="refresh-dropdown">
            <span id="refresh-label"></span>
            <button data-ro-action="set-refresh" data-ro-interval="0"></button>
            <button data-ro-action="set-refresh" data-ro-interval="30"></button>
            <button data-ro-action="set-refresh" data-ro-interval="Live"></button>
        </div>
    `;
    const dropdown = document.getElementById('refresh-dropdown');
    const off = document.querySelector('[data-ro-interval="0"]');
    const thirty = document.querySelector('[data-ro-interval="30"]');
    const live = document.querySelector('[data-ro-interval="Live"]');

    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Live', ns: {} });
    syncRefreshUI();
    expect(document.getElementById('refresh-label')).toHaveTextContent('Live');
    expect(live).toHaveClass('is-active');
    expect(thirty).not.toHaveClass('is-active');
    expect(off).not.toHaveClass('is-active');
    expect(dropdown).toHaveClass('refresh-on');

    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '30', ns: {} });
    syncRefreshUI();
    expect(document.getElementById('refresh-label')).toHaveTextContent('30s');
    expect(thirty).toHaveClass('is-active');
    expect(live).not.toHaveClass('is-active');
    expect(off).not.toHaveClass('is-active');
    expect(dropdown).toHaveClass('refresh-on');
});
