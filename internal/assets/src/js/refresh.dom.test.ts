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
    effectivePollSeconds,
    fireRefresh,
    handleRefreshAfterRequest,
    handleRefreshBeforeRequest,
    handleRefreshConfigRequest,
    handleRefreshVisibilityChange,
    isPreloadRequest,
    noteRefreshFailure,
    noteRefreshRecovery,
    pruneSettledListRequests,
    refreshBindings,
    refreshInterval,
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

function dispatchHtmx(
    type: 'htmx:afterRequest' | 'htmx:beforeRequest' | 'htmx:configRequest',
    detail: unknown,
): void {
    const event = new CustomEvent(type, { bubbles: true, detail });
    const handler = {
        'htmx:afterRequest': handleRefreshAfterRequest,
        'htmx:beforeRequest': handleRefreshBeforeRequest,
        'htmx:configRequest': handleRefreshConfigRequest,
    }[type];
    handler(event);
}

function refreshBinding(selector: string) {
    const binding = refreshBindings.find((candidate) => candidate.selector === selector);
    expect(binding).toBeDefined();
    return binding as (typeof refreshBindings)[number];
}

function renderRefreshControls(): {
    dropdown: HTMLElement;
    label: HTMLElement;
    live: HTMLElement;
    missing: HTMLElement;
    off: HTMLElement;
    thirty: HTMLElement;
} {
    document.body.innerHTML = `
        <div id="refresh-dropdown">
            <span id="refresh-label"></span>
            <button data-ro-action="set-refresh" data-ro-interval="0">Off</button>
            <button data-ro-action="set-refresh" data-ro-interval="30">30s</button>
            <button data-ro-action="set-refresh" data-ro-interval="Live">Live</button>
            <button data-ro-action="set-refresh" class="is-active">Missing</button>
        </div>
    `;
    return {
        dropdown: document.getElementById('refresh-dropdown') as HTMLElement,
        label: document.getElementById('refresh-label') as HTMLElement,
        live: document.querySelector('[data-ro-interval="Live"]') as HTMLElement,
        missing: document.querySelector(
            '[data-ro-action="set-refresh"]:not([data-ro-interval])',
        ) as HTMLElement,
        off: document.querySelector('[data-ro-interval="0"]') as HTMLElement,
        thirty: document.querySelector('[data-ro-interval="30"]') as HTMLElement,
    };
}

function captureTimeouts() {
    const callbacks: Array<() => void> = [];
    const setTimeout = vi.spyOn(window, 'setTimeout').mockImplementation((handler) => {
        expect(handler).toBeTypeOf('function');
        callbacks.push(handler as () => void);
        return callbacks.length as unknown as ReturnType<typeof window.setTimeout>;
    });
    const clearTimeout = vi.spyOn(window, 'clearTimeout').mockImplementation(() => {});
    return { callbacks, clearTimeout, setTimeout };
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
    test('recognizes only the exact preload header and tolerates missing request metadata', () => {
        expect(isPreloadRequest(new Event('plain'))).toBe(false);
        expect(isPreloadRequest(new CustomEvent('request', { detail: null }))).toBe(false);
        expect(isPreloadRequest(new CustomEvent('request', { detail: {} }))).toBe(false);
        expect(
            isPreloadRequest(new CustomEvent('request', { detail: { requestConfig: null } })),
        ).toBe(false);
        expect(
            isPreloadRequest(new CustomEvent('request', { detail: { requestConfig: {} } })),
        ).toBe(false);
        expect(
            isPreloadRequest(
                new CustomEvent('request', { detail: { requestConfig: { headers: {} } } }),
            ),
        ).toBe(false);
        expect(
            isPreloadRequest(
                new CustomEvent('request', {
                    detail: { requestConfig: { headers: { 'HX-Preloaded': 'false' } } },
                }),
            ),
        ).toBe(false);
        expect(
            isPreloadRequest(
                new CustomEvent('request', {
                    detail: { requestConfig: { headers: { 'HX-Preloaded': 'true' } } },
                }),
            ),
        ).toBe(true);
    });

    test('marks only a container config request RO-No-Push', () => {
        const content = renderContent();
        const other = document.createElement('button');
        const untouched: Record<string, string> = {};

        dispatchHtmx('htmx:configRequest', null);
        dispatchHtmx('htmx:configRequest', {});
        dispatchHtmx('htmx:configRequest', { elt: content });
        dispatchHtmx('htmx:configRequest', { elt: other, headers: untouched });
        expect(untouched).toStrictEqual({});

        const headers: Record<string, string> = {};
        dispatchHtmx('htmx:configRequest', { elt: content, headers });
        expect(headers).toStrictEqual({ 'RO-No-Push': 'true' });
    });

    test('tracks each request class, lets a user request win, and settles both sets', () => {
        const content = renderContent();
        const userSource = document.createElement('button');
        document.body.appendChild(userSource);
        const htmx = installHtmx();
        const containerXHR = xhrAt(1);
        const userXHR = xhrAt(1);

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

        dispatchHtmx('htmx:afterRequest', null);
        dispatchHtmx('htmx:afterRequest', {});
        dispatchHtmx('htmx:afterRequest', { unrelated: true });
        expect(containerListRequestsInFlight).toContain(containerXHR);
        expect(userListRequestsInFlight).toContain(userXHR);

        dispatchHtmx('htmx:afterRequest', { xhr: containerXHR });
        dispatchHtmx('htmx:afterRequest', { xhr: userXHR });
        expect(containerListRequestsInFlight).toHaveLength(0);
        expect(userListRequestsInFlight).toHaveLength(0);
    });

    test('treats a container request without an XHR as container-owned and inert', () => {
        const content = renderContent();
        const htmx = installHtmx();

        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content });

        expect(containerListRequestsInFlight).toHaveLength(0);
        expect(userListRequestsInFlight).toHaveLength(0);
        expect(htmx.trigger).not.toHaveBeenCalled();
    });

    test('aborts for a genuine user request even when htmx did not expose an XHR', () => {
        const content = renderContent();
        const userSource = document.createElement('button');
        const htmx = installHtmx();

        dispatchHtmx('htmx:beforeRequest', { elt: userSource, target: content });

        expect(userListRequestsInFlight).toHaveLength(0);
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'htmx:abort');
    });

    test('ignores incomplete, wrong-target, and non-Element user-request shapes', () => {
        const content = renderContent();
        const otherTarget = document.createElement('div');
        const userSource = document.createElement('button');
        const htmx = installHtmx();
        const cases: unknown[] = [
            null,
            {},
            { elt: userSource },
            { target: content },
            { elt: 'not-an-element', target: content },
            { elt: userSource, target: 'not-an-element' },
            { elt: userSource, target: otherTarget },
        ];

        cases.forEach((detail) => {
            dispatchHtmx('htmx:beforeRequest', detail);
        });

        expect(userListRequestsInFlight).toHaveLength(0);
        expect(containerListRequestsInFlight).toHaveLength(0);
        expect(htmx.trigger).not.toHaveBeenCalled();
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

    test('tracks a user XHR without aborting when the live content or htmx seam is absent', () => {
        const detachedTarget = document.createElement('div');
        detachedTarget.id = 'resource-list-content';
        const source = document.createElement('button');
        const firstXHR = xhrAt(1);
        const htmx = installHtmx();

        dispatchHtmx('htmx:beforeRequest', {
            elt: source,
            target: detachedTarget,
            xhr: firstXHR,
        });
        expect(userListRequestsInFlight).toContain(firstXHR);
        expect(htmx.trigger).not.toHaveBeenCalled();

        userListRequestsInFlight.clear();
        delete (window as unknown as { htmx?: HtmxHarness }).htmx;
        renderContent();
        const secondXHR = xhrAt(1);
        dispatchHtmx('htmx:beforeRequest', {
            elt: source,
            target: document.getElementById('resource-list-content'),
            xhr: secondXHR,
        });
        expect(userListRequestsInFlight).toContain(secondXHR);
    });
});

describe('refresh requests', () => {
    test('uses the live location path, trims every trailing slash, and keeps the exact query', () => {
        window.history.replaceState(
            null,
            '',
            '/clusters/prod/namespaces/default/pods///?sort=Age%3Adesc&f=status%3ARunning',
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
        const rejectionHandler = catchRequest.mock.calls[0]?.[0] as (() => void) | undefined;
        expect(rejectionHandler).toBeTypeOf('function');
        expect(() => rejectionHandler?.()).not.toThrow();
        expect(htmx.trigger).not.toHaveBeenCalled();
    });

    test('uses ro:refresh for a legacy multi-type list', () => {
        const content = renderContent('/baked/table');
        const htmx = installHtmx();

        requestListRefresh();

        expect(htmx.ajax).not.toHaveBeenCalled();
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'ro:refresh');
    });

    test('loads safely without the content or htmx and exposes the production window seam', () => {
        const seam = (window as unknown as { requestListRefresh?: unknown }).requestListRefresh;
        expect(seam).toBe(requestListRefresh);

        expect(() => requestListRefresh()).not.toThrow();
        const content = renderContent('location');
        expect(() => requestListRefresh()).not.toThrow();

        content.remove();
        installHtmx();
        expect(() => requestListRefresh()).not.toThrow();
    });

    test.each([undefined, {}, { catch: 'not-a-function' }])(
        'accepts an htmx ajax result without a callable catch: %j',
        (request) => {
            renderContent('location');
            const htmx = installHtmx();
            htmx.ajax.mockReturnValue(request);

            expect(() => requestListRefresh()).not.toThrow();
            expect(htmx.ajax).toHaveBeenCalledOnce();
        },
    );

    test('retry aborts the old container request before issuing the replacement', () => {
        const content = renderContent('location');
        const htmx = installHtmx();
        const retry = refreshBinding('[data-ro-action="retry"]');
        const event = new Event('click', { cancelable: true });

        expect(retry.handler(event, null)).toBe(true);

        expect(event.defaultPrevented).toBe(true);
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'htmx:abort');
        expect(htmx.ajax).toHaveBeenCalledOnce();
        expect(htmx.trigger.mock.invocationCallOrder[0]).toBeLessThan(
            htmx.ajax.mock.invocationCallOrder[0] as number,
        );
    });

    test('retry remains a handled, prevented no-op when no refresh surface exists', () => {
        const retry = refreshBinding('[data-ro-action="retry"]');
        const htmx = installHtmx();
        const withoutContent = new Event('click', { cancelable: true });

        expect(retry.handler(withoutContent, null)).toBe(true);
        expect(withoutContent.defaultPrevented).toBe(true);
        expect(htmx.trigger).not.toHaveBeenCalled();
        expect(htmx.ajax).not.toHaveBeenCalled();

        delete (window as unknown as { htmx?: HtmxHarness }).htmx;
        renderContent('location');
        const withoutHtmx = new Event('click', { cancelable: true });
        expect(retry.handler(withoutHtmx, null)).toBe(true);
        expect(withoutHtmx.defaultPrevented).toBe(true);
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

describe('persisted refresh mode', () => {
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

    test('does not consult legacy storage when any canonical mode is present', () => {
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Live', ns: {} });
        const getItem = vi.spyOn(Storage.prototype, 'getItem');

        expect(refreshMode()).toBe('Live');
        expect(getItem).not.toHaveBeenCalled();
        expect(dependencies.roPrefsSetRefresh).not.toHaveBeenCalled();
    });

    test.each([null, ''])('treats an absent legacy value %j as no preference', (legacy) => {
        if (legacy !== null) {
            window.localStorage.setItem('roRefresh', legacy);
        }

        expect(refreshMode()).toBe('');
        expect(dependencies.roPrefsSetRefresh).not.toHaveBeenCalled();
    });

    test('degrades safely when localStorage cannot be read', () => {
        vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
            throw new DOMException('blocked', 'SecurityError');
        });

        expect(refreshMode()).toBe('');
        expect(dependencies.roPrefsSetRefresh).not.toHaveBeenCalled();
    });

    test.each([
        { legacy: '0', mode: 'Off' },
        { legacy: '-5', mode: 'Off' },
        { legacy: 'junk', mode: 'Off' },
        { legacy: '5junk', mode: 'Off' },
        { legacy: 'junk5', mode: 'Off' },
        { legacy: '+005', mode: '5' },
    ])('normalizes legacy $legacy to $mode before writing the cookie', ({ legacy, mode }) => {
        window.localStorage.setItem('roRefresh', legacy);

        expect(refreshMode()).toBe(mode);
        expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith(mode);
    });

    test.each([
        { mode: '', seconds: 0 },
        { mode: 'Off', seconds: 0 },
        { mode: 'Live', seconds: 0 },
        { mode: '0', seconds: 0 },
        { mode: '-5', seconds: 0 },
        { mode: ' 5', seconds: 0 },
        { mode: '5 ', seconds: 0 },
        { mode: '5.5', seconds: 0 },
        { mode: '5junk', seconds: 0 },
        { mode: 'junk5', seconds: 0 },
        { mode: '9007199254740992', seconds: 0 },
        { mode: '5', seconds: 5 },
        { mode: '9', seconds: 9 },
        { mode: '10', seconds: 10 },
        { mode: '005', seconds: 5 },
        { mode: '+5', seconds: 5 },
    ])('parses canonical mode $mode as $seconds polling seconds', ({ mode, seconds }) => {
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: mode, ns: {} });
        expect(refreshInterval()).toBe(seconds);
    });

    test('folds the parsed mode and Live fallback from one preference snapshot', () => {
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '30', ns: {} });
        dependencies.liveFallbackSeconds.mockReturnValue(5);
        expect(effectivePollSeconds()).toBe(30);
        expect(dependencies.readPrefs).toHaveBeenCalledOnce();

        dependencies.readPrefs.mockClear();
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Live', ns: {} });
        expect(effectivePollSeconds()).toBe(5);
        expect(dependencies.readPrefs).toHaveBeenCalledOnce();

        dependencies.readPrefs.mockClear();
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Off', ns: {} });
        expect(effectivePollSeconds()).toBe(0);
        expect(dependencies.readPrefs).toHaveBeenCalledOnce();
    });
});

test('maintains one timer chain across scheduling, backoff, recovery, and one fired tick', () => {
    const now = new Date('2026-08-25T10:00:00Z').getTime();
    const dateNow = vi.spyOn(Date, 'now').mockReturnValue(now);
    const timers = captureTimeouts();
    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '5', ns: {} });
    const content = renderContent('location');
    const htmx = installHtmx();
    htmx.ajax.mockReturnValue({ catch: vi.fn() });
    const toast = vi.fn();
    window.roToast = toast;

    applyRefresh();
    expect(refreshNextAtMs()).toBe(now + 5_000);
    expect(timers.setTimeout).toHaveBeenLastCalledWith(expect.any(Function), 5_000);

    scheduleRefreshTick();
    expect(timers.clearTimeout).toHaveBeenLastCalledWith(1);
    expect(timers.setTimeout).toHaveBeenCalledTimes(2);

    noteRefreshFailure();
    expect(refreshNextAtMs()).toBe(now + 5_000);
    expect(timers.clearTimeout).toHaveBeenLastCalledWith(2);
    expect(timers.setTimeout).toHaveBeenLastCalledWith(expect.any(Function), 5_000);

    noteRefreshFailure();
    expect(refreshNextAtMs()).toBe(now + 10_000);
    expect(timers.clearTimeout).toHaveBeenLastCalledWith(3);
    expect(timers.setTimeout).toHaveBeenLastCalledWith(expect.any(Function), 10_000);

    noteRefreshRecovery();
    expect(refreshNextAtMs()).toBe(now + 5_000);
    expect(timers.clearTimeout).toHaveBeenLastCalledWith(4);
    expect(timers.setTimeout).toHaveBeenLastCalledWith(expect.any(Function), 5_000);
    expect(toast).toHaveBeenCalledExactlyOnceWith('Refresh resumed');

    const firedTick = timers.callbacks.at(-1);
    expect(firedTick).toBeTypeOf('function');
    dateNow.mockReturnValue(now + 5_000);
    firedTick?.();

    expect(htmx.ajax).toHaveBeenCalledExactlyOnceWith('GET', '/_table', { source: content });
    expect(refreshNextAtMs()).toBe(now + 10_000);
    expect(timers.setTimeout).toHaveBeenCalledTimes(6);
    expect(timers.setTimeout).toHaveBeenLastCalledWith(expect.any(Function), 5_000);
    expect(timers.setTimeout.mock.invocationCallOrder.at(-1)).toBeLessThan(
        htmx.ajax.mock.invocationCallOrder[0] as number,
    );
    expect(dependencies.updateStaleCountdown).toHaveBeenCalled();
});

test('disarms a pending timer at zero cadence and repaints the stale countdown', () => {
    const timers = captureTimeouts();
    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '5', ns: {} });
    applyRefresh();
    expect(timers.setTimeout).toHaveBeenCalledOnce();

    timers.setTimeout.mockClear();
    timers.clearTimeout.mockClear();
    dependencies.updateStaleCountdown.mockClear();
    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Off', ns: {} });

    scheduleRefreshTick();

    expect(timers.clearTimeout).toHaveBeenCalledExactlyOnceWith(1);
    expect(timers.setTimeout).not.toHaveBeenCalled();
    expect(refreshNextAtMs()).toBe(0);
    expect(dependencies.updateStaleCountdown).toHaveBeenCalledOnce();
});

test('keeps an ordinary stage-zero recovery silent and does not re-arm', () => {
    const timers = captureTimeouts();
    const toast = vi.fn();
    window.roToast = toast;

    noteRefreshRecovery();

    expect(toast).not.toHaveBeenCalled();
    expect(timers.setTimeout).not.toHaveBeenCalled();
    expect(dependencies.updateStaleCountdown).not.toHaveBeenCalled();
});

test('recovers and re-arms safely when the optional toast seam is absent', () => {
    const timers = captureTimeouts();
    dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '5', ns: {} });
    noteRefreshFailure();
    timers.setTimeout.mockClear();
    timers.clearTimeout.mockClear();

    expect(() => noteRefreshRecovery()).not.toThrow();

    expect(timers.clearTimeout).toHaveBeenCalledOnce();
    expect(timers.setTimeout).toHaveBeenCalledExactlyOnceWith(expect.any(Function), 5_000);
});

describe('refresh UI', () => {
    test.each([
        {
            mode: 'Live',
            label: 'Live',
            active: 'live',
            refreshOn: true,
        },
        {
            mode: '30',
            label: '30s',
            active: 'thirty',
            refreshOn: true,
        },
        {
            mode: 'Off',
            label: 'Off',
            active: 'off',
            refreshOn: false,
        },
        {
            mode: '30junk',
            label: 'Off',
            active: 'off',
            refreshOn: false,
        },
    ] as const)(
        'renders $mode without cross-activating another option',
        ({ mode, label, active, refreshOn }) => {
            const controls = renderRefreshControls();
            dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: mode, ns: {} });

            syncRefreshUI();

            expect(controls.label).toHaveTextContent(label);
            expect(controls.live.classList.contains('is-active')).toBe(active === 'live');
            expect(controls.thirty.classList.contains('is-active')).toBe(active === 'thirty');
            expect(controls.off.classList.contains('is-active')).toBe(active === 'off');
            expect(controls.missing).not.toHaveClass('is-active');
            expect(controls.dropdown.classList.contains('refresh-on')).toBe(refreshOn);
        },
    );

    test('updates valid options without requiring a label or dropdown', () => {
        document.body.innerHTML = `
            <button data-ro-action="set-refresh" data-ro-interval="0"></button>
            <button data-ro-action="set-refresh" data-ro-interval="Live" class="is-active"></button>
        `;
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Off', ns: {} });

        expect(() => syncRefreshUI()).not.toThrow();

        expect(document.querySelector('[data-ro-interval="0"]')).toHaveClass('is-active');
        expect(document.querySelector('[data-ro-interval="Live"]')).not.toHaveClass('is-active');
    });
});

describe('visibility catch-up', () => {
    test('fires only when the document becomes visible with an effective cadence', () => {
        renderContent('location');
        const htmx = installHtmx();
        const hidden = vi.spyOn(document, 'hidden', 'get');

        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '30', ns: {} });
        hidden.mockReturnValue(true);
        handleRefreshVisibilityChange();
        expect(htmx.ajax).not.toHaveBeenCalled();

        hidden.mockReturnValue(false);
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Off', ns: {} });
        handleRefreshVisibilityChange();
        expect(htmx.ajax).not.toHaveBeenCalled();

        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Live', ns: {} });
        dependencies.liveFallbackSeconds.mockReturnValue(0);
        handleRefreshVisibilityChange();
        expect(htmx.ajax).not.toHaveBeenCalled();

        dependencies.liveFallbackSeconds.mockReturnValue(5);
        handleRefreshVisibilityChange();
        expect(htmx.ajax).toHaveBeenCalledOnce();

        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: '30', ns: {} });
        dependencies.liveFallbackSeconds.mockReturnValue(0);
        handleRefreshVisibilityChange();
        expect(htmx.ajax).toHaveBeenCalledTimes(2);
    });
});

describe('refresh dispatcher bindings', () => {
    test('publish the exact handled click contracts in retry-then-picker order', () => {
        expect(
            refreshBindings.map(({ event, selector, stop }) => ({ event, selector, stop })),
        ).toStrictEqual([
            { event: 'click', selector: '[data-ro-action="retry"]', stop: true },
            { event: 'click', selector: '[data-ro-action="set-refresh"]', stop: true },
        ]);
    });

    test('a Live pick persists, reconciles Live, paints, disarms polling, blurs, and prevents', () => {
        const controls = renderRefreshControls();
        const option = controls.live;
        let persisted = 'Off';
        dependencies.readPrefs.mockImplementation(() => ({
            kinds: [],
            refresh: persisted,
            ns: {},
        }));
        dependencies.roPrefsSetRefresh.mockImplementation((mode: string) => {
            persisted = mode;
        });
        option.focus();
        const event = new Event('click', { cancelable: true });
        const picker = refreshBinding('[data-ro-action="set-refresh"]');

        expect(picker.handler(event, option)).toBe(true);

        expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith('Live');
        expect(dependencies.liveApply).toHaveBeenCalledExactlyOnceWith(true);
        expect(controls.label).toHaveTextContent('Live');
        expect(controls.live).toHaveClass('is-active');
        expect(controls.off).not.toHaveClass('is-active');
        expect(controls.dropdown).toHaveClass('refresh-on');
        expect(dependencies.updateStaleCountdown).toHaveBeenCalledOnce();
        expect(document.activeElement).not.toBe(option);
        expect(event.defaultPrevented).toBe(true);
        expect(dependencies.roPrefsSetRefresh.mock.invocationCallOrder[0]).toBeLessThan(
            dependencies.liveApply.mock.invocationCallOrder[0] as number,
        );
    });

    test('a numeric pick canonicalizes the interval, paints it, and arms that exact delay', () => {
        const controls = renderRefreshControls();
        const timers = captureTimeouts();
        let persisted = 'Off';
        dependencies.readPrefs.mockImplementation(() => ({
            kinds: [],
            refresh: persisted,
            ns: {},
        }));
        dependencies.roPrefsSetRefresh.mockImplementation((mode: string) => {
            persisted = mode;
        });
        controls.thirty.dataset.roInterval = '030';
        controls.thirty.focus();
        const event = new Event('click', { cancelable: true });
        const picker = refreshBinding('[data-ro-action="set-refresh"]');

        expect(picker.handler(event, controls.thirty)).toBe(true);

        expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith('30');
        expect(dependencies.liveApply).toHaveBeenCalledExactlyOnceWith(true);
        expect(controls.label).toHaveTextContent('30s');
        expect(controls.thirty).toHaveClass('is-active');
        expect(timers.setTimeout.mock.calls.filter(([, delay]) => delay === 30_000)).toHaveLength(
            1,
        );
        expect(document.activeElement).not.toBe(controls.thirty);
        expect(event.defaultPrevented).toBe(true);
    });

    test.each([undefined, '0', '-5', '30junk'])(
        'an invalid pick %j canonicalizes to Off',
        (value) => {
            const controls = renderRefreshControls();
            let persisted = '30';
            dependencies.readPrefs.mockImplementation(() => ({
                kinds: [],
                refresh: persisted,
                ns: {},
            }));
            dependencies.roPrefsSetRefresh.mockImplementation((mode: string) => {
                persisted = mode;
            });
            if (value === undefined) {
                delete controls.thirty.dataset.roInterval;
            } else {
                controls.thirty.dataset.roInterval = value;
            }
            const event = new Event('click', { cancelable: true });
            const picker = refreshBinding('[data-ro-action="set-refresh"]');

            expect(picker.handler(event, controls.thirty)).toBe(true);

            expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith('Off');
            expect(controls.label).toHaveTextContent('Off');
            expect(controls.off).toHaveClass('is-active');
            expect(controls.dropdown).not.toHaveClass('refresh-on');
            expect(event.defaultPrevented).toBe(true);
        },
    );
});

test('registers every required resident listener and binding at module load', async () => {
    // The first import happens while Vitest collects this file, outside an
    // individual test's coverage window. Re-evaluate the side-effect module so
    // listener registrations and descriptor literals are mutation-covered.
    vi.resetModules();
    const documentAdd = vi.spyOn(document, 'addEventListener');
    const registrationStart = documentAdd.mock.calls.length;

    const fresh = await import('./refresh.js');

    expect(documentAdd.mock.calls.slice(registrationStart)).toEqual(
        expect.arrayContaining([
            ['htmx:configRequest', fresh.handleRefreshConfigRequest],
            ['htmx:beforeRequest', fresh.handleRefreshBeforeRequest],
            ['htmx:afterRequest', fresh.handleRefreshAfterRequest],
            ['visibilitychange', fresh.handleRefreshVisibilityChange],
        ]),
    );
    expect(
        fresh.refreshBindings.map(({ event, handler, selector, stop }) => ({
            event,
            handler: typeof handler,
            selector,
            stop,
        })),
    ).toEqual(
        expect.arrayContaining([
            {
                event: 'click',
                handler: 'function',
                selector: '[data-ro-action="retry"]',
                stop: true,
            },
            {
                event: 'click',
                handler: 'function',
                selector: '[data-ro-action="set-refresh"]',
                stop: true,
            },
        ]),
    );
});
