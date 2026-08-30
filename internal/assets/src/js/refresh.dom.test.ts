// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    liveApply: vi.fn(),
    liveSetOff: vi.fn(),
    readPrefs: vi.fn(() => ({ kinds: [], refresh: '', ns: {} })),
    roPrefsSetRefresh: vi.fn(),
}));

vi.mock('./live.js', () => ({
    liveApply: dependencies.liveApply,
    liveSetOff: dependencies.liveSetOff,
}));
vi.mock('./prefs.js', () => ({
    readPrefs: dependencies.readPrefs,
    roPrefsSetRefresh: dependencies.roPrefsSetRefresh,
}));

import {
    handleRefreshAfterRequest,
    handleRefreshBeforeRequest,
    handleRefreshConfigRequest,
    isLiveEnabled,
    listRequestTrackerSnapshot,
    pruneSettledListRequests,
    refreshBindings,
    requestListRefresh,
    resetListRequestTracker,
    subscribeListRequests,
    syncLiveToggle,
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
    const xhr = new XMLHttpRequest();
    Object.defineProperty(xhr, 'readyState', {
        configurable: true,
        value: readyState,
        writable: true,
    });
    return xhr;
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

// The two navbar update controls the server renders on a Live-capable list page.
function renderUpdateControls(): { refreshNow: HTMLButtonElement; toggle: HTMLButtonElement } {
    document.body.innerHTML = `
        <button type="button" data-ro-action="toggle-live" aria-pressed="false">
            <span class="ro-livedot"></span><span class="live-label">Live</span>
        </button>
        <button type="button" data-ro-action="refresh-now" aria-label="Refresh now"></button>
    `;
    return {
        refreshNow: document.querySelector('[data-ro-action="refresh-now"]') as HTMLButtonElement,
        toggle: document.querySelector('[data-ro-action="toggle-live"]') as HTMLButtonElement,
    };
}

// A preference cell the module reads through the mocked cookie codec.
function persistPreference(initial: string): { value: () => string } {
    let persisted = initial;
    dependencies.readPrefs.mockImplementation(() => ({ kinds: [], refresh: persisted, ns: {} }));
    dependencies.roPrefsSetRefresh.mockImplementation((mode: string) => {
        persisted = mode;
    });
    return { value: () => persisted };
}

function click(binding: (typeof refreshBindings)[number], matched: Element | null): boolean {
    const event = new Event('click', { cancelable: true });
    const handled = binding.handler(event, matched);
    expect(event.defaultPrevented).toBe(true);
    return handled === true;
}

beforeEach(() => {
    document.body.replaceChildren();
    dependencies.liveApply.mockReset();
    dependencies.liveSetOff.mockReset();
    dependencies.readPrefs.mockReset().mockReturnValue({ kinds: [], refresh: '', ns: {} });
    dependencies.roPrefsSetRefresh.mockReset();
    delete (window as unknown as { htmx?: HtmxHarness }).htmx;
    delete window.roToast;

    // Reset the module-owned request tracker without re-importing its resident
    // DOM listeners.
    resetListRequestTracker();
    vi.clearAllMocks();
});

test('the tracker prunes only DONE and aborted list XHRs', () => {
    const content = renderContent();
    const aborted = xhrAt(0);
    const opened = xhrAt(1);
    const headersReceived = xhrAt(2);
    const loading = xhrAt(3);
    const done = xhrAt(4);
    const requests = [aborted, opened, headersReceived, loading, done];
    requests.forEach((xhr) => {
        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr });
    });

    pruneSettledListRequests();

    expect(listRequestTrackerSnapshot().count).toBe(3);
    [opened, headersReceived, loading].forEach((xhr) => {
        dispatchHtmx('htmx:afterRequest', { xhr });
    });
});

describe('htmx request lifecycle', () => {
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

    test('does not invent container ownership when the current container is absent', () => {
        const headers: Record<string, string> = { 'RO-No-Push': 'external' };

        dispatchHtmx('htmx:configRequest', { elt: null, target: null, headers });

        expect(headers).toStrictEqual({ 'RO-No-Push': 'external' });
    });

    test('owns every RO-No-Push casing without suppressing user history', () => {
        const content = renderContent();
        const userSource = document.createElement('a');
        const containerHeaders: Record<string, string> = {
            'ro-no-push': 'false',
            'Ro-No-Push': 'spoof-one',
            'RO-NO-PUSH': 'spoof-two',
            'HX-Push-Url': 'false',
        };

        dispatchHtmx('htmx:configRequest', {
            elt: content,
            target: content,
            headers: containerHeaders,
        });

        expect(containerHeaders).toStrictEqual({
            'HX-Push-Url': 'false',
            'RO-No-Push': 'true',
        });

        const userHeaders: Record<string, string> = {
            'ro-no-push': 'true',
            'RO-No-Push': 'spoof',
            'HX-Push-Url': 'true',
        };
        dispatchHtmx('htmx:configRequest', {
            elt: userSource,
            target: content,
            headers: userHeaders,
        });

        expect(userHeaders).toStrictEqual({ 'HX-Push-Url': 'true' });
    });

    test('strips RO-No-Push when the current container explicitly targets elsewhere', () => {
        const content = renderContent();
        const headers = { 'rO-nO-pUsH': 'true', 'HX-Push-Url': 'true' };

        dispatchHtmx('htmx:configRequest', {
            elt: content,
            target: document.createElement('main'),
            headers,
        });

        expect(headers).toStrictEqual({ 'HX-Push-Url': 'true' });
    });

    test('integrates the exact stored validator only for current-container traffic', () => {
        window.history.replaceState(null, '', '/clusters/prod/pods');
        const content = renderContent();
        content.append(document.createElement('table'));
        content.dataset.roEtag = 'W/"stored"';
        content.dataset.roEtagPath = '/clusters/prod/pods/_table?sort=Name';
        const containerHeaders = {
            'if-none-match': '"spoof"',
            'ro-no-push': 'false',
            'RO-NO-PUSH': 'spoof',
        };

        dispatchHtmx('htmx:configRequest', {
            elt: content,
            target: content,
            path: '/clusters/prod/pods/_table?sort=Name',
            headers: containerHeaders,
        });

        expect(containerHeaders).toStrictEqual({
            'RO-No-Push': 'true',
            'If-None-Match': 'W/"stored"',
        });

        const userSource = document.createElement('a');
        const userHeaders = { 'If-None-Match': '"spoof"' };
        dispatchHtmx('htmx:configRequest', {
            elt: userSource,
            target: content,
            path: '/clusters/prod/pods/_table?sort=Name',
            headers: userHeaders,
        });

        expect(userHeaders).toStrictEqual({});
    });

    test('a detached same-id source cannot earn refresh or conditional headers', () => {
        const content = renderContent();
        content.dataset.roEtag = 'W/"stored"';
        content.dataset.roEtagPath = '/pods/_table';
        const detached = document.createElement('div');
        detached.id = 'resource-list-content';
        const headers = { 'If-None-Match': '"spoof"' };

        dispatchHtmx('htmx:configRequest', {
            elt: detached,
            target: content,
            path: '/pods/_table',
            headers,
        });

        expect(headers).toStrictEqual({});
    });

    test('one tracker publishes container and user request lifecycles and user traffic wins', () => {
        const content = renderContent();
        const userSource = document.createElement('button');
        document.body.appendChild(userSource);
        const htmx = installHtmx();
        const containerXHR = xhrAt(1);
        const userXHR = xhrAt(1);
        const activities: Array<{ phase: string; inFlight: number }> = [];
        const unsubscribe = subscribeListRequests((activity) => activities.push(activity));

        dispatchHtmx('htmx:beforeRequest', {
            elt: content,
            target: content,
            xhr: containerXHR,
            pathInfo: { finalRequestPath: '/pods/_table?sort=Name' },
        });
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 1 });
        expect(htmx.trigger).not.toHaveBeenCalled();

        dispatchHtmx('htmx:beforeRequest', {
            elt: userSource,
            target: content,
            xhr: userXHR,
            pathInfo: { requestPath: '/pods/_table?sort=Age' },
        });
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 2 });
        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'htmx:abort');

        dispatchHtmx('htmx:afterRequest', null);
        dispatchHtmx('htmx:afterRequest', {});
        dispatchHtmx('htmx:afterRequest', { unrelated: true });
        expect(listRequestTrackerSnapshot().count).toBe(2);

        dispatchHtmx('htmx:afterRequest', { xhr: containerXHR });
        dispatchHtmx('htmx:afterRequest', { xhr: userXHR });
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 0 });
        expect(activities).toStrictEqual([
            { phase: 'start', inFlight: 1 },
            { phase: 'start', inFlight: 2 },
            { phase: 'settle', inFlight: 1 },
            { phase: 'settle', inFlight: 0 },
        ]);
        unsubscribe();

        const afterUnsubscribe = xhrAt(1);
        dispatchHtmx('htmx:beforeRequest', {
            elt: content,
            target: content,
            xhr: afterUnsubscribe,
        });
        dispatchHtmx('htmx:afterRequest', { xhr: afterUnsubscribe });
        expect(activities).toHaveLength(4);
    });

    test('unowned settlement and an empty page reset publish no request activity', () => {
        const activities: Array<{ phase: string; inFlight: number }> = [];
        const unsubscribe = subscribeListRequests((activity) => activities.push(activity));

        dispatchHtmx('htmx:afterRequest', { xhr: xhrAt(4) });
        resetListRequestTracker();

        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 0 });
        expect(activities).toStrictEqual([]);
        unsubscribe();
    });

    test('the same XHR identity is tracked only once until it settles', () => {
        const content = renderContent();
        const request = xhrAt(1);
        const activities: Array<{ phase: string; inFlight: number }> = [];
        const unsubscribe = subscribeListRequests((activity) => activities.push(activity));

        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr: request });
        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr: request });
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 1 });
        expect(activities).toStrictEqual([{ phase: 'start', inFlight: 1 }]);

        request.dispatchEvent(new Event('loadend'));
        expect(activities).toStrictEqual([
            { phase: 'start', inFlight: 1 },
            { phase: 'settle', inFlight: 0 },
        ]);
        unsubscribe();
    });

    test('a page reset retires the old owner without letting its late loadend settle a reuse', () => {
        const content = renderContent();
        const request = xhrAt(1);
        const loadends: EventListener[] = [];
        vi.spyOn(request, 'addEventListener').mockImplementation((type, listener) => {
            if (type === 'loadend' && typeof listener === 'function') loadends.push(listener);
        });

        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr: request });
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 1 });
        resetListRequestTracker();
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 0 });

        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr: request });
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 1 });
        loadends[0]?.call(request, new Event('loadend'));
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 1 });

        loadends[1]?.call(request, new Event('loadend'));
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 0 });
    });

    test('native loadend settles a request even when its issuing element detaches', () => {
        const content = renderContent();
        const request = xhrAt(1);
        dispatchHtmx('htmx:beforeRequest', {
            elt: content,
            target: content,
            xhr: request,
        });
        content.remove();

        request.dispatchEvent(new Event('loadend'));

        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 0 });
    });

    test('a cancelled beforeRequest with an UNSENT xhr retires after dispatch', async () => {
        const content = renderContent();
        const request = xhrAt(0);
        dispatchHtmx('htmx:beforeRequest', {
            elt: content,
            target: content,
            xhr: request,
        });

        expect(listRequestTrackerSnapshot().count).toBe(1);
        await Promise.resolve();

        expect(listRequestTrackerSnapshot().count).toBe(0);
    });

    test('a later beforeRequest canceler retires an OPENED xhr after dispatch', async () => {
        const content = renderContent();
        const request = xhrAt(1);
        const event = new CustomEvent('htmx:beforeRequest', {
            cancelable: true,
            detail: { elt: content, target: content, xhr: request },
        });

        handleRefreshBeforeRequest(event);
        expect(listRequestTrackerSnapshot().count).toBe(1);

        event.preventDefault();
        await Promise.resolve();

        expect(event.defaultPrevented).toBe(true);
        expect(listRequestTrackerSnapshot().count).toBe(0);
    });

    test('an ordinary OPENED request remains tracked after the cancellation checkpoint', async () => {
        const content = renderContent();
        const request = xhrAt(1);

        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr: request });
        await Promise.resolve();

        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 1 });
        request.dispatchEvent(new Event('loadend'));
        expect(listRequestTrackerSnapshot()).toStrictEqual({ count: 0 });
    });

    test('missing XHR and incomplete or wrong-target request shapes are inert', () => {
        const content = renderContent();
        const otherTarget = document.createElement('div');
        const userSource = document.createElement('button');
        const htmx = installHtmx();
        const cases: unknown[] = [
            null,
            {},
            { elt: content, target: content },
            { elt: userSource, target: content },
            { elt: userSource, target: otherTarget, xhr: xhrAt(1) },
            { elt: content, target: otherTarget, xhr: xhrAt(1) },
            { elt: 'not-an-element', target: content, xhr: xhrAt(1) },
            { elt: content, target: content, xhr: {} },
            {
                elt: content,
                target: content,
                xhr: { addEventListener: vi.fn(), readyState: 1 },
            },
        ];

        cases.forEach((detail) => {
            dispatchHtmx('htmx:beforeRequest', detail);
        });

        expect(listRequestTrackerSnapshot().count).toBe(0);
        expect(htmx.trigger).not.toHaveBeenCalled();
    });

    test('a detached same-id target is ignored while a current target is tracked without htmx', () => {
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
        expect(listRequestTrackerSnapshot().count).toBe(0);
        expect(htmx.trigger).not.toHaveBeenCalled();

        delete (window as unknown as { htmx?: HtmxHarness }).htmx;
        const content = renderContent();
        const secondXHR = xhrAt(1);
        dispatchHtmx('htmx:beforeRequest', {
            elt: source,
            target: content,
            xhr: secondXHR,
        });
        expect(listRequestTrackerSnapshot().count).toBe(1);
        dispatchHtmx('htmx:afterRequest', { xhr: secondXHR });
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

    test('retry reopens Live instead of issuing a competing poll request', () => {
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Live', ns: {} });
        const content = renderContent('location');
        const htmx = installHtmx();
        const retry = refreshBinding('[data-ro-action="retry"]');
        const event = new Event('click', { cancelable: true });

        expect(retry.handler(event, null)).toBe(true);

        expect(htmx.trigger).toHaveBeenCalledExactlyOnceWith(content, 'htmx:abort');
        expect(dependencies.liveApply).toHaveBeenCalledExactlyOnceWith(true);
        expect(htmx.ajax).not.toHaveBeenCalled();
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

describe('the stored Live preference', () => {
    test.each(['Live'])('exactly %j turns Live on', (refresh) => {
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh, ns: {} });
        expect(isLiveEnabled()).toBe(true);
    });

    // Numeric cadences are what an older profile persisted. They must read as
    // OFF -- never as "some polling mode" -- so an upgraded tab is quiet.
    test.each(['', 'Off', '0', '5', '10', '30', '60', 'live', 'LIVE', ' Live', 'Live '])(
        'any other stored value %j reads as off',
        (refresh) => {
            dependencies.readPrefs.mockReturnValue({ kinds: [], refresh, ns: {} });
            expect(isLiveEnabled()).toBe(false);
        },
    );

    test('a legacy localStorage cadence is never consulted or migrated', () => {
        window.localStorage.setItem('roRefresh', '30');
        const getItem = vi.spyOn(Storage.prototype, 'getItem');

        expect(isLiveEnabled()).toBe(false);

        expect(getItem).not.toHaveBeenCalled();
        expect(dependencies.roPrefsSetRefresh).not.toHaveBeenCalled();
        window.localStorage.removeItem('roRefresh');
    });
});

describe('the navbar update controls', () => {
    test.each([
        { pressed: 'true', refresh: 'Live' },
        { pressed: 'false', refresh: 'Off' },
        { pressed: 'false', refresh: '30' },
        { pressed: 'false', refresh: '' },
    ])('syncLiveToggle paints aria-pressed $pressed for $refresh', ({ pressed, refresh }) => {
        const controls = renderUpdateControls();
        controls.toggle.setAttribute('aria-pressed', 'unset');
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh, ns: {} });

        syncLiveToggle();

        expect(controls.toggle).toHaveAttribute('aria-pressed', pressed);
    });

    test('syncLiveToggle is inert on a page that renders no Live toggle', () => {
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh: 'Live', ns: {} });
        expect(() => syncLiveToggle()).not.toThrow();
        expect(document.querySelector('[data-ro-action="toggle-live"]')).toBeNull();
    });

    test('the Refresh button tracks the one in-flight list request', () => {
        const controls = renderUpdateControls();
        const content = renderContent('location');
        const xhr = xhrAt(1);

        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr });
        expect(controls.refreshNow.disabled).toBe(true);

        dispatchHtmx('htmx:afterRequest', { xhr });
        expect(controls.refreshNow.disabled).toBe(false);
    });
});

describe('nothing in this module fires on its own', () => {
    test.each([
        { label: 'Live held open', refresh: 'Live' },
        { label: 'a legacy 5s cadence', refresh: '5' },
        { label: 'a legacy 30s cadence', refresh: '30' },
        { label: 'no preference at all', refresh: '' },
    ])('hours pass with $label and no list request is issued', ({ refresh }) => {
        vi.useFakeTimers();
        renderUpdateControls();
        renderContent('location');
        const htmx = installHtmx();
        dependencies.readPrefs.mockReturnValue({ kinds: [], refresh, ns: {} });

        syncLiveToggle();
        vi.advanceTimersByTime(6 * 60 * 60 * 1000);

        expect(htmx.ajax).not.toHaveBeenCalled();
        expect(htmx.trigger).not.toHaveBeenCalled();
        expect(vi.getTimerCount()).toBe(0);
        vi.useRealTimers();
    });

    test('a toggled-on Live session still arms no list timer', () => {
        vi.useFakeTimers();
        const controls = renderUpdateControls();
        renderContent('location');
        const htmx = installHtmx();
        persistPreference('Off');

        click(refreshBinding('[data-ro-action="toggle-live"]'), controls.toggle);
        vi.advanceTimersByTime(6 * 60 * 60 * 1000);

        expect(dependencies.liveApply).toHaveBeenCalledOnce();
        expect(htmx.ajax).not.toHaveBeenCalled();
        expect(vi.getTimerCount()).toBe(0);
        vi.useRealTimers();
    });
});

describe('refresh dispatcher bindings', () => {
    test('publish the exact handled click contracts in their registration order', () => {
        expect(
            refreshBindings.map(({ event, selector, stop }) => ({ event, selector, stop })),
        ).toStrictEqual([
            { event: 'click', selector: '[data-ro-action="retry"]', stop: true },
            { event: 'click', selector: '[data-ro-action="toggle-live"]', stop: true },
            { event: 'click', selector: '[data-ro-action="refresh-now"]', stop: true },
            { event: 'click', selector: '[data-ro-action="reload"]', stop: true },
        ]);
    });

    test('turning Live on persists exactly Live, presses the toggle, and opens the stream', () => {
        const controls = renderUpdateControls();
        const preference = persistPreference('Off');
        const timer = vi.spyOn(window, 'setTimeout');

        expect(click(refreshBinding('[data-ro-action="toggle-live"]'), controls.toggle)).toBe(true);

        expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith('Live');
        expect(preference.value()).toBe('Live');
        expect(controls.toggle).toHaveAttribute('aria-pressed', 'true');
        expect(dependencies.liveApply).toHaveBeenCalledExactlyOnceWith(true);
        expect(dependencies.liveSetOff).not.toHaveBeenCalled();
        expect(timer).not.toHaveBeenCalled();
        expect(dependencies.roPrefsSetRefresh.mock.invocationCallOrder[0]).toBeLessThan(
            dependencies.liveApply.mock.invocationCallOrder[0] as number,
        );
    });

    test('turning Live off persists Off, releases the toggle, and issues no request', () => {
        const controls = renderUpdateControls();
        const preference = persistPreference('Live');
        const htmx = installHtmx();
        renderContent('location');
        syncLiveToggle();
        expect(controls.toggle).toHaveAttribute('aria-pressed', 'true');

        expect(click(refreshBinding('[data-ro-action="toggle-live"]'), controls.toggle)).toBe(true);

        expect(dependencies.roPrefsSetRefresh).toHaveBeenCalledExactlyOnceWith('Off');
        expect(preference.value()).toBe('Off');
        expect(controls.toggle).toHaveAttribute('aria-pressed', 'false');
        expect(dependencies.liveSetOff).toHaveBeenCalledOnce();
        expect(dependencies.liveApply).not.toHaveBeenCalled();
        expect(htmx.ajax).not.toHaveBeenCalled();
        expect(htmx.trigger).not.toHaveBeenCalled();
    });

    test('Refresh now makes exactly one request, arms no timer, and writes no preference', () => {
        const controls = renderUpdateControls();
        const content = renderContent('location');
        const htmx = installHtmx();
        const timer = vi.spyOn(window, 'setTimeout');

        expect(click(refreshBinding('[data-ro-action="refresh-now"]'), controls.refreshNow)).toBe(
            true,
        );

        expect(htmx.ajax).toHaveBeenCalledExactlyOnceWith(
            'GET',
            '/_table',
            expect.objectContaining({ source: content }),
        );
        expect(timer).not.toHaveBeenCalled();
        expect(dependencies.roPrefsSetRefresh).not.toHaveBeenCalled();
        expect(dependencies.liveApply).not.toHaveBeenCalled();
    });

    test('Refresh now reclaims a wedged tracker entry whose terminal event never bubbled', () => {
        const controls = renderUpdateControls();
        const content = renderContent('location');
        const htmx = installHtmx();
        // A request whose issuing element detached mid-flight: htmx:afterRequest
        // never reaches document, so only the readyState prune can retire it.
        const wedged = xhrAt(1);
        dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr: wedged });
        expect(controls.refreshNow.disabled).toBe(true);
        Object.defineProperty(wedged, 'readyState', { value: 4 });

        click(refreshBinding('[data-ro-action="refresh-now"]'), controls.refreshNow);

        expect(listRequestTrackerSnapshot().count).toBe(0);
        expect(htmx.ajax).toHaveBeenCalledOnce();
        expect(controls.refreshNow.disabled).toBe(false);
    });

    test('Refresh now is disabled in flight and cannot stack a second request', () => {
        const controls = renderUpdateControls();
        const content = renderContent('location');
        const htmx = installHtmx();
        // htmx issues the XHR the tracker observes; model it on the ajax call.
        const xhr = xhrAt(1);
        htmx.ajax.mockImplementation(() => {
            dispatchHtmx('htmx:beforeRequest', { elt: content, target: content, xhr });
            return undefined;
        });
        const refreshNow = refreshBinding('[data-ro-action="refresh-now"]');

        click(refreshNow, controls.refreshNow);
        expect(htmx.ajax).toHaveBeenCalledOnce();
        expect(controls.refreshNow.disabled).toBe(true);

        // A second (synthetic) click while the first is unsettled is inert.
        click(refreshNow, controls.refreshNow);
        expect(htmx.ajax).toHaveBeenCalledOnce();

        dispatchHtmx('htmx:afterRequest', { xhr });
        expect(controls.refreshNow.disabled).toBe(false);
        click(refreshNow, controls.refreshNow);
        expect(htmx.ajax).toHaveBeenCalledTimes(2);
    });

    test('Reload reloads the document and nothing else', () => {
        const controls = renderUpdateControls();
        const htmx = installHtmx();
        const reload = vi.fn();
        vi.spyOn(window, 'location', 'get').mockReturnValue({
            ...window.location,
            reload,
        } as unknown as Location);

        expect(click(refreshBinding('[data-ro-action="reload"]'), controls.refreshNow)).toBe(true);

        expect(reload).toHaveBeenCalledOnce();
        expect(htmx.ajax).not.toHaveBeenCalled();
        expect(dependencies.liveApply).not.toHaveBeenCalled();
    });
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
        ]),
    );
    // No timer and no visibility listener: nothing in this module ever fires on
    // its own.
    expect(documentAdd.mock.calls.slice(registrationStart).map(([type]) => type)).not.toContain(
        'visibilitychange',
    );
    expect(
        fresh.refreshBindings.map(({ event, handler, selector, stop }) => ({
            event,
            handler: typeof handler,
            selector,
            stop,
        })),
    ).toStrictEqual([
        { event: 'click', handler: 'function', selector: '[data-ro-action="retry"]', stop: true },
        {
            event: 'click',
            handler: 'function',
            selector: '[data-ro-action="toggle-live"]',
            stop: true,
        },
        {
            event: 'click',
            handler: 'function',
            selector: '[data-ro-action="refresh-now"]',
            stop: true,
        },
        { event: 'click', handler: 'function', selector: '[data-ro-action="reload"]', stop: true },
    ]);

    // The freshly imported module subscribes its own Refresh-button paint to the
    // one request tracker.
    const controls = renderUpdateControls();
    const content = renderContent('location');
    const xhr = xhrAt(1);
    fresh.handleRefreshBeforeRequest(
        new CustomEvent('htmx:beforeRequest', { detail: { elt: content, target: content, xhr } }),
    );
    expect(controls.refreshNow.disabled).toBe(true);
});
