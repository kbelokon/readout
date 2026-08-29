// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

const steps = vi.hoisted(() => ({
    applyLiveNameFilter: vi.fn(),
    applyRefresh: vi.fn(),
    buildYamlFolds: vi.fn(),
    captureRowModelFromDocument: vi.fn(),
    clearListStale: vi.fn(),
    clearRowState: vi.fn(),
    closeRowMenu: vi.fn(),
    collapseSectionsFromHash: vi.fn(),
    colsPopOpen: vi.fn(() => false),
    highlightYamlLine: vi.fn(),
    initLogsFollow: vi.fn(),
    isListRefreshEvent: vi.fn(() => false),
    liveApply: vi.fn(),
    liveOnListSwap: vi.fn(),
    liveResetPage: vi.fn(),
    liveState: { status: 'idle', streamPath: '' },
    liveTeardown: vi.fn(),
    noteRefreshRecovery: vi.fn(),
    pauseRefresh: vi.fn(),
    rememberListValidator: vi.fn(),
    reapplyRowState: vi.fn(),
    roPrefsSetSort: vi.fn(),
    setColsPopOpen: vi.fn(),
    showToast: vi.fn(),
    syncColsPopState: vi.fn(),
    syncRefreshUI: vi.fn(),
    syncThemeTogglePostTarget: vi.fn(),
    suppressListNotModified: vi.fn((_event: Event) => false),
    updateBulkBar: vi.fn(),
    updateFilterAC: vi.fn(),
    virtualizeAfterSwap: vi.fn(),
    virtualizeInit: vi.fn(),
}));

vi.mock('./columns.js', () => ({
    colsPopOpen: steps.colsPopOpen,
    setColsPopOpen: steps.setColsPopOpen,
    syncColsPopState: steps.syncColsPopState,
}));
vi.mock('./context-menu.js', () => ({ closeRowMenu: steps.closeRowMenu }));
vi.mock('./filters.js', () => ({
    applyLiveNameFilter: steps.applyLiveNameFilter,
    captureRowModelFromDocument: steps.captureRowModelFromDocument,
    updateFilterAC: steps.updateFilterAC,
}));
vi.mock('./list-etag.js', () => ({
    rememberListValidator: steps.rememberListValidator,
    suppressListNotModified: steps.suppressListNotModified,
}));
vi.mock('./live.js', () => ({
    liveApply: steps.liveApply,
    liveOnListSwap: steps.liveOnListSwap,
    liveResetPage: steps.liveResetPage,
    liveState: steps.liveState,
    liveTeardown: steps.liveTeardown,
}));
vi.mock('./logs.js', () => ({ initLogsFollow: steps.initLogsFollow }));
vi.mock('./misc-ui.js', () => ({ collapseSectionsFromHash: steps.collapseSectionsFromHash }));
vi.mock('./prefs.js', () => ({ roPrefsSetSort: steps.roPrefsSetSort }));
vi.mock('./refresh.js', () => ({
    applyRefresh: steps.applyRefresh,
    noteRefreshRecovery: steps.noteRefreshRecovery,
    pauseRefresh: steps.pauseRefresh,
    syncRefreshUI: steps.syncRefreshUI,
}));
vi.mock('./row-selection.js', () => ({
    clearRowState: steps.clearRowState,
    reapplyRowState: steps.reapplyRowState,
    updateBulkBar: steps.updateBulkBar,
}));
vi.mock('./stale.js', () => ({
    clearListStale: steps.clearListStale,
    isListRefreshEvent: steps.isListRefreshEvent,
}));
vi.mock('./theme.js', () => ({
    syncThemeTogglePostTarget: steps.syncThemeTogglePostTarget,
}));
vi.mock('./toasts.js', () => ({ showToast: steps.showToast }));
vi.mock('./virtualizer.js', () => ({
    virtualizeAfterSwap: steps.virtualizeAfterSwap,
    virtualizeInit: steps.virtualizeInit,
}));
vi.mock('./yaml-folds.js', () => ({
    buildYamlFolds: steps.buildYamlFolds,
    highlightYamlLine: steps.highlightYamlLine,
}));
vi.mock('./skeleton.js', () => ({}));

// Import exactly once: init.ts installs resident document/window listeners at module load.
import { handleSortPreferenceRequest, suppressRedundantActiveNavigation } from './init.js';

interface CallTracked {
    mock: { invocationCallOrder: number[] };
}

function expectCalledOnceInOrder(...calls: CallTracked[]): void {
    const order = calls.map((call) => {
        expect(call.mock.invocationCallOrder).toHaveLength(1);
        return call.mock.invocationCallOrder[0] as number;
    });
    expect(order).toStrictEqual([...order].sort((a, b) => a - b));
}

function expectInitOrder(): void {
    expectCalledOnceInOrder(
        steps.syncRefreshUI,
        steps.buildYamlFolds,
        steps.collapseSectionsFromHash,
        steps.highlightYamlLine,
        steps.initLogsFollow,
        steps.syncThemeTogglePostTarget,
        steps.captureRowModelFromDocument,
        steps.applyLiveNameFilter,
        steps.virtualizeInit,
        steps.syncColsPopState,
        steps.reapplyRowState,
        steps.updateBulkBar,
        steps.liveApply,
        steps.applyRefresh,
    );
}

function dispatchBeforeRequest(detail?: object): void {
    handleSortPreferenceRequest(new CustomEvent('htmx:beforeRequest', { detail }));
}

type HistoryEventType =
    | 'htmx:historyCacheHit'
    | 'htmx:historyCacheMiss'
    | 'htmx:historyCacheMissLoad'
    | 'htmx:historyCacheMissLoadError';

function historyXHR(): XMLHttpRequest {
    return new EventTarget() as XMLHttpRequest;
}

function historyEvent(type: HistoryEventType, xhr?: XMLHttpRequest): CustomEvent {
    return new CustomEvent(type, {
        bubbles: true,
        cancelable: true,
        detail: xhr ? { xhr } : {},
    });
}

function dispatchNormalBodyBeforeSwap(shouldSwap = true): CustomEvent {
    const event = new CustomEvent('htmx:beforeSwap', {
        bubbles: true,
        cancelable: true,
        detail: { shouldSwap, target: document.body, xhr: { status: 200 } },
    });
    document.body.dispatchEvent(event);
    return event;
}

beforeEach(() => {
    // Model the fresh document boundary after a hard reload without
    // re-importing this resident-listener module between tests.
    window.dispatchEvent(new Event('pageshow'));
    vi.clearAllMocks();
    steps.colsPopOpen.mockReturnValue(false);
    steps.isListRefreshEvent.mockReturnValue(false);
    steps.suppressListNotModified.mockReset().mockReturnValue(false);
    steps.liveState.status = 'idle';
    steps.liveState.streamPath = '';
    vi.spyOn(window.history, 'go').mockImplementation(() => {});
});

test('publishes showToast through the resident window seam', () => {
    expect(window.roToast).toBe(steps.showToast);

    window.roToast?.('Detached result');

    expect(steps.showToast).toHaveBeenCalledExactlyOnceWith('Detached result');
});

describe('runInit orchestration', () => {
    test('runs the complete repair chain once and in order for the initial document', () => {
        document.dispatchEvent(new Event('DOMContentLoaded'));
        expectInitOrder();
    });

    test('runs the complete repair chain once during a successful body afterSwap', () => {
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expectInitOrder();
    });

    test('never reruns whole-document init for descendant htmx:load events', () => {
        document.body.innerHTML = '<main><section><div id="loaded-child"></div></section></main>';
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        expectInitOrder();

        for (const target of [
            document.querySelector('main'),
            document.querySelector('section'),
            document.getElementById('loaded-child'),
        ]) {
            target?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
        }

        // A body swap already ran the chain before paint. HTMX subsequently
        // emits load once per inserted top-level element; none may repeat it.
        expectInitOrder();
    });

    test('isolates a failing step and continues the remaining chain', () => {
        const failure = new Error('live setup failed');
        steps.liveApply.mockImplementationOnce(() => {
            throw failure;
        });
        const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expectCalledOnceInOrder(
            steps.syncRefreshUI,
            steps.captureRowModelFromDocument,
            steps.applyLiveNameFilter,
            steps.virtualizeInit,
            steps.updateBulkBar,
            steps.liveApply,
            steps.applyRefresh,
        );
        expect(warn).toHaveBeenCalledExactlyOnceWith('readout init step failed', failure);
    });

    test('measures the sticky namespace between theme sync and full-model capture', () => {
        document.body.innerHTML = `
            <div class="ro-table-wrap">
                <table class="ro-table"><tbody><tr><td class="cell-ns"></td></tr></tbody></table>
            </div>
        `;
        const cell = document.querySelector('.cell-ns') as HTMLTableCellElement;
        const measure = vi
            .spyOn(cell, 'getBoundingClientRect')
            .mockReturnValue({ width: 72 } as DOMRect);

        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expectCalledOnceInOrder(
            steps.syncThemeTogglePostTarget,
            measure,
            steps.captureRowModelFromDocument,
        );
    });
});

describe('htmx swap lifecycle', () => {
    test.each([400, 403, 404, 500, 503, 599])(
        'allows a designed %s error screen to replace the body',
        (status) => {
            const detail = {
                target: document.body,
                xhr: { status },
                shouldSwap: false,
            };

            document.dispatchEvent(new CustomEvent('htmx:beforeSwap', { detail }));

            expect(detail.shouldSwap).toBe(true);
            expect(steps.liveTeardown).toHaveBeenCalledOnce();
        },
    );

    test.each([0, 200, 304, 399, 600, '500', undefined])(
        'does not override the body swap decision for status %s',
        (status) => {
            const detail = {
                target: document.body,
                xhr: { status },
                shouldSwap: false,
            };

            document.dispatchEvent(new CustomEvent('htmx:beforeSwap', { detail }));

            expect(detail.shouldSwap).toBe(false);
        },
    );

    test('keeps a failed list refresh non-swapping', () => {
        const content = document.createElement('div');
        const detail = {
            target: content,
            xhr: { status: 500 },
            shouldSwap: false,
        };

        document.dispatchEvent(new CustomEvent('htmx:beforeSwap', { detail }));

        expect(detail.shouldSwap).toBe(false);
        expect(steps.closeRowMenu).not.toHaveBeenCalled();
        expect(steps.liveTeardown).not.toHaveBeenCalled();
    });

    test('an exact list 304 recovers without entering the afterSwap repair or Live pipeline', () => {
        const content = document.createElement('div');
        content.id = 'resource-list-content';
        document.body.appendChild(content);
        const detail = {
            elt: content,
            target: content,
            xhr: { status: 304 },
            requestConfig: { elt: content },
            shouldSwap: true,
            isError: true,
        };
        steps.suppressListNotModified.mockImplementationOnce((event: Event) => {
            const matched = (event as CustomEvent).detail;
            matched.shouldSwap = false;
            matched.isError = false;
            return true;
        });

        document.dispatchEvent(new CustomEvent('htmx:beforeSwap', { detail }));

        expect(detail.shouldSwap).toBe(false);
        expect(detail.isError).toBe(false);
        expectCalledOnceInOrder(
            steps.suppressListNotModified,
            steps.noteRefreshRecovery,
            steps.clearListStale,
        );
        expect(steps.rememberListValidator).not.toHaveBeenCalled();
        expect(steps.reapplyRowState).not.toHaveBeenCalled();
        expect(steps.applyLiveNameFilter).not.toHaveBeenCalled();
        expect(steps.virtualizeAfterSwap).not.toHaveBeenCalled();
        expect(steps.liveOnListSwap).not.toHaveBeenCalled();
        expect(steps.liveTeardown).not.toHaveBeenCalled();
    });

    test('an unmatched current-list 304 stays non-swapping but takes the responseError path', () => {
        const content = document.createElement('div');
        content.id = 'resource-list-content';
        document.body.appendChild(content);
        const detail = {
            elt: content,
            target: content,
            xhr: { status: 304 },
            requestConfig: { elt: document.createElement('a') },
            shouldSwap: true,
            isError: false,
        };
        steps.suppressListNotModified.mockImplementationOnce((event: Event) => {
            const unmatched = (event as CustomEvent).detail;
            unmatched.shouldSwap = false;
            unmatched.isError = true;
            return false;
        });

        document.dispatchEvent(new CustomEvent('htmx:beforeSwap', { detail }));

        expect(detail.shouldSwap).toBe(false);
        expect(detail.isError).toBe(true);
        expect(steps.suppressListNotModified).toHaveBeenCalledExactlyOnceWith(expect.any(Event));
        expect(steps.noteRefreshRecovery).not.toHaveBeenCalled();
        expect(steps.clearListStale).not.toHaveBeenCalled();
        expect(steps.rememberListValidator).not.toHaveBeenCalled();
        expect(steps.reapplyRowState).not.toHaveBeenCalled();
        expect(steps.virtualizeAfterSwap).not.toHaveBeenCalled();
        expect(steps.liveOnListSwap).not.toHaveBeenCalled();
    });

    test('tears down the old screen only for a body swap and resets live identity', () => {
        const content = document.createElement('div');
        document.body.appendChild(content);
        steps.liveState.status = 'riding';
        steps.liveState.streamPath = '/pods/_stream';

        document.dispatchEvent(new CustomEvent('htmx:beforeSwap', { detail: { target: content } }));
        expect(steps.closeRowMenu).not.toHaveBeenCalled();
        expect(steps.liveState).toStrictEqual({
            status: 'riding',
            streamPath: '/pods/_stream',
        });

        document.dispatchEvent(
            new CustomEvent('htmx:beforeSwap', { detail: { target: document.body } }),
        );

        expectCalledOnceInOrder(
            steps.closeRowMenu,
            steps.clearRowState,
            steps.clearListStale,
            steps.liveTeardown,
            steps.pauseRefresh,
            steps.liveResetPage,
        );
        expect(steps.liveState).toStrictEqual({ status: 'idle', streamPath: '' });
    });

    test('initializes the accepted body once after teardown and before descendant loads', () => {
        document.body.innerHTML = '<main id="old-body"></main>';

        dispatchNormalBodyBeforeSwap();
        expect(steps.liveApply).not.toHaveBeenCalled();

        document.body.innerHTML = '<main id="fresh-body"><div id="fresh-child"></div></main>';
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expectCalledOnceInOrder(
            steps.closeRowMenu,
            steps.liveTeardown,
            steps.syncRefreshUI,
            steps.buildYamlFolds,
            steps.captureRowModelFromDocument,
            steps.virtualizeInit,
            steps.liveApply,
            steps.applyRefresh,
        );

        document
            .getElementById('fresh-child')
            ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
        expect(steps.buildYamlFolds).toHaveBeenCalledOnce();
        expect(steps.liveApply).toHaveBeenCalledOnce();
        expect(steps.applyRefresh).toHaveBeenCalledOnce();
    });

    test('repairs a list afterSwap in fixed order before handing it back to Live', () => {
        document.body.innerHTML = `
            <div id="resource-list-content">
                <input id="ro-filter-input" value="status:">
            </div>
        `;
        const input = document.getElementById('ro-filter-input') as HTMLInputElement;
        input.focus();
        steps.isListRefreshEvent.mockReturnValue(true);
        steps.colsPopOpen.mockReturnValue(true);
        const event = new CustomEvent('htmx:afterSwap', {
            bubbles: true,
            detail: { target: document.getElementById('resource-list-content') },
        });

        document.getElementById('resource-list-content')?.dispatchEvent(event);

        expectCalledOnceInOrder(
            steps.rememberListValidator,
            steps.noteRefreshRecovery,
            steps.clearListStale,
            steps.reapplyRowState,
            steps.applyLiveNameFilter,
            steps.updateFilterAC,
            steps.colsPopOpen,
            steps.setColsPopOpen,
            steps.virtualizeAfterSwap,
            steps.liveOnListSwap,
        );
        expect(steps.setColsPopOpen).toHaveBeenCalledExactlyOnceWith(true);
        expect(steps.rememberListValidator).toHaveBeenCalledExactlyOnceWith(event);
        expect(steps.liveOnListSwap).toHaveBeenCalledExactlyOnceWith(event);
        expect(steps.buildYamlFolds).not.toHaveBeenCalled();
        expect(steps.virtualizeInit).not.toHaveBeenCalled();
        expect(steps.liveApply).not.toHaveBeenCalled();
    });

    test('ignores an afterSwap that is not a list refresh', () => {
        const event = new CustomEvent('htmx:afterSwap', { detail: { target: document.body } });

        document.dispatchEvent(event);

        expect(steps.isListRefreshEvent).toHaveBeenCalledExactlyOnceWith(event);
        expect(steps.noteRefreshRecovery).not.toHaveBeenCalled();
        expect(steps.rememberListValidator).not.toHaveBeenCalled();
        expect(steps.clearListStale).not.toHaveBeenCalled();
        expect(steps.reapplyRowState).not.toHaveBeenCalled();
        expect(steps.applyLiveNameFilter).not.toHaveBeenCalled();
        expect(steps.updateFilterAC).not.toHaveBeenCalled();
        expect(steps.colsPopOpen).not.toHaveBeenCalled();
        expect(steps.virtualizeAfterSwap).not.toHaveBeenCalled();
        expect(steps.liveOnListSwap).not.toHaveBeenCalled();
    });

    test.each([
        { name: 'missing input', input: '' },
        {
            name: 'focused empty input',
            input: '<input id="ro-filter-input" value="" data-focus>',
        },
        {
            name: 'unfocused non-empty input',
            input: '<input id="ro-filter-input" value="status:">',
        },
    ])('runs the list pipeline but skips optional repairs for $name', ({ input }) => {
        document.body.innerHTML = `<div id="resource-list-content">${input}</div>`;
        const filterInput = document.getElementById('ro-filter-input') as HTMLInputElement | null;
        if (filterInput?.hasAttribute('data-focus')) {
            filterInput.focus();
        }
        steps.isListRefreshEvent.mockReturnValue(true);
        steps.colsPopOpen.mockReturnValue(false);
        const event = new CustomEvent('htmx:afterSwap', {
            detail: { target: document.getElementById('resource-list-content') },
        });

        document.dispatchEvent(event);

        expectCalledOnceInOrder(
            steps.rememberListValidator,
            steps.noteRefreshRecovery,
            steps.clearListStale,
            steps.reapplyRowState,
            steps.applyLiveNameFilter,
            steps.colsPopOpen,
            steps.virtualizeAfterSwap,
            steps.liveOnListSwap,
        );
        expect(steps.updateFilterAC).not.toHaveBeenCalled();
        expect(steps.setColsPopOpen).not.toHaveBeenCalled();
        expect(steps.liveOnListSwap).toHaveBeenCalledExactlyOnceWith(event);
    });

    test.each(['htmx:historyCacheHit', 'htmx:historyCacheMiss'] as const)(
        '%s retires old Live ownership before the history body swap',
        (type) => {
            steps.liveState.status = 'open-v2';
            steps.liveState.streamPath = '/old/_stream';
            const xhr = type === 'htmx:historyCacheMiss' ? historyXHR() : undefined;

            document.body.dispatchEvent(historyEvent(type, xhr));

            expectCalledOnceInOrder(steps.liveTeardown, steps.pauseRefresh, steps.liveResetPage);
            expect(steps.liveState).toStrictEqual({ status: 'idle', streamPath: '' });
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.reapplyRowState).not.toHaveBeenCalled();
            expect(steps.updateBulkBar).not.toHaveBeenCalled();

            if (xhr) document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));
            document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        },
    );

    test('historyRestore itself never initializes or repairs the outgoing body', () => {
        document.dispatchEvent(new Event('htmx:historyRestore'));

        expect(steps.liveTeardown).not.toHaveBeenCalled();
        expect(steps.liveResetPage).not.toHaveBeenCalled();
        expect(steps.reapplyRowState).not.toHaveBeenCalled();
        expect(steps.updateBulkBar).not.toHaveBeenCalled();
        expect(steps.liveApply).not.toHaveBeenCalled();
    });

    test.each(['htmx:historyCacheHit', 'htmx:historyCacheMiss'] as const)(
        'a later %s listener cancellation reloads once and keeps the mismatched body inert',
        async (type) => {
            document.body.innerHTML = '<main id="outgoing-list">old body</main>';
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            const xhr = type === 'htmx:historyCacheMiss' ? historyXHR() : undefined;
            document.addEventListener(
                type,
                (event) => {
                    event.preventDefault();
                },
                { once: true },
            );
            const start = historyEvent(type, xhr);

            document.body.dispatchEvent(start);

            expect(start.defaultPrevented).toBe(true);
            expect(reload).not.toHaveBeenCalled();
            await Promise.resolve();
            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            expect(steps.pauseRefresh).toHaveBeenCalledOnce();

            document.body.dispatchEvent(new Event('htmx:swapError', { bubbles: true }));
            document
                .getElementById('outgoing-list')
                ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
            xhr?.dispatchEvent(new Event('loadend'));
            await Promise.resolve();
            expect(reload).toHaveBeenCalledOnce();
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.applyRefresh).not.toHaveBeenCalled();
        },
    );

    test.each(['htmx:historyCacheHit', 'htmx:historyCacheMiss'] as const)(
        'a pending %s body swapError reloads exactly once without afterSwap',
        async (type) => {
            document.body.innerHTML = '<main id="outgoing-list">old body</main>';
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            const xhr = type === 'htmx:historyCacheMiss' ? historyXHR() : undefined;
            document.body.dispatchEvent(historyEvent(type, xhr));
            if (xhr) document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));
            await Promise.resolve();
            expect(reload).not.toHaveBeenCalled();

            document.body.dispatchEvent(new Event('htmx:swapError', { bubbles: true }));
            document.body.dispatchEvent(new Event('htmx:swapError', { bubbles: true }));
            document
                .getElementById('outgoing-list')
                ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));

            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            expect(steps.pauseRefresh).toHaveBeenCalledOnce();
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.applyRefresh).not.toHaveBeenCalled();
        },
    );

    test.each(['domain error', 'native loadend'] as const)(
        'a cache-miss %s reloads once before any body swap',
        (failure) => {
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            const xhr = historyXHR();
            document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));

            if (failure === 'domain error') {
                document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoadError', xhr));
            } else {
                xhr.dispatchEvent(new Event('loadend'));
            }

            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoadError', xhr));
            xhr.dispatchEvent(new Event('loadend'));
            expect(reload).toHaveBeenCalledOnce();
        },
    );

    test('a malformed cache-miss start without an XHR is cancelled and reloaded', async () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const start = historyEvent('htmx:historyCacheMiss');

        document.body.dispatchEvent(start);
        await Promise.resolve();

        expect(start.defaultPrevented).toBe(true);
        expect(reload).toHaveBeenCalledExactlyOnceWith(0);
    });

    test('a stray cache-miss completion retires the current screen and reloads only once', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        steps.liveState.status = 'open-v2';
        steps.liveState.streamPath = '/old/_stream';
        const staleLoad = historyEvent('htmx:historyCacheMissLoad', historyXHR());

        document.body.dispatchEvent(staleLoad);
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoadError', historyXHR()));

        expect(staleLoad.defaultPrevented).toBe(true);
        expect(reload).toHaveBeenCalledExactlyOnceWith(0);
        expectCalledOnceInOrder(steps.liveTeardown, steps.pauseRefresh, steps.liveResetPage);
        expect(steps.liveState).toStrictEqual({ status: 'idle', streamPath: '' });
    });

    test('an unrelated swapError cannot reload an idle body', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const source = document.createElement('a');
        document.body.appendChild(source);

        source.dispatchEvent(
            new CustomEvent('htmx:swapError', {
                bubbles: true,
                detail: { target: document.createElement('main') },
            }),
        );

        expect(reload).not.toHaveBeenCalled();
        expect(steps.liveTeardown).not.toHaveBeenCalled();
    });

    test('a body-targeted swapError is inert without a body ownership ticket', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});

        document.body.dispatchEvent(
            new CustomEvent('htmx:swapError', {
                bubbles: true,
                detail: { target: document.body },
            }),
        );

        expect(reload).not.toHaveBeenCalled();
        expect(steps.liveTeardown).not.toHaveBeenCalled();
    });

    test('cache-miss loadend cannot fail an accepted swap-phase response', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const xhr = historyXHR();
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));

        xhr.dispatchEvent(new Event('loadend'));

        expect(reload).not.toHaveBeenCalled();
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
    });

    test('a stale cache-miss loadend cannot retire a newer body owner', async () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const staleXHR = historyXHR();
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', staleXHR));
        window.dispatchEvent(new Event('pageshow'));
        dispatchNormalBodyBeforeSwap();

        staleXHR.dispatchEvent(new Event('loadend'));

        expect(reload).not.toHaveBeenCalled();
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        await Promise.resolve();
        expect(reload).not.toHaveBeenCalled();
    });

    test('a duplicate cache-miss load cannot be accepted after request phase', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const xhr = historyXHR();
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));
        const duplicate = historyEvent('htmx:historyCacheMissLoad', xhr);

        document.body.dispatchEvent(duplicate);

        expect(duplicate.defaultPrevented).toBe(true);
        expect(reload).toHaveBeenCalledExactlyOnceWith(0);
    });

    test('an unrelated swapError cannot steal a pending normal body ticket', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const source = document.createElement('a');
        document.body.appendChild(source);
        dispatchNormalBodyBeforeSwap();

        source.dispatchEvent(
            new CustomEvent('htmx:swapError', {
                bubbles: true,
                detail: { target: document.createElement('main') },
            }),
        );

        expect(reload).not.toHaveBeenCalled();
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
    });

    test('an anonymous body afterSwap during the cache-miss network phase fails closed', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const xhr = historyXHR();
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));

        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expect(reload).toHaveBeenCalledExactlyOnceWith(0);
        document.dispatchEvent(new Event('htmx:load'));
        expect(steps.liveApply).not.toHaveBeenCalled();
        expect(steps.applyRefresh).not.toHaveBeenCalled();
    });

    test.each(['normal', 'hit', 'miss'] as const)(
        'owns a %s body swap without scheduling a fallback timer',
        (kind) => {
            const schedule = vi.spyOn(window, 'setTimeout');
            const xhr = kind === 'miss' ? historyXHR() : undefined;
            if (kind === 'normal') {
                dispatchNormalBodyBeforeSwap();
            } else {
                document.body.dispatchEvent(
                    historyEvent(
                        kind === 'miss' ? 'htmx:historyCacheMiss' : 'htmx:historyCacheHit',
                        xhr,
                    ),
                );
            }

            expect(schedule).not.toHaveBeenCalled();
            if (xhr) document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));
            document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
            expect(steps.liveApply).toHaveBeenCalledOnce();
        },
    );

    test('successful body and synchronous history swaps never reload', async () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});

        document.body.dispatchEvent(historyEvent('htmx:historyCacheHit'));
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        await Promise.resolve();
        const xhr = historyXHR();
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        xhr.dispatchEvent(new Event('loadend'));

        expect(reload).not.toHaveBeenCalled();
    });

    test('preventing cache-miss load cannot cancel its already accepted body swap', async () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const xhr = historyXHR();
        document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));
        document.addEventListener(
            'htmx:historyCacheMissLoad',
            (event) => {
                event.preventDefault();
            },
            { once: true },
        );
        const cacheMissLoad = historyEvent('htmx:historyCacheMissLoad', xhr);

        document.body.dispatchEvent(cacheMissLoad);
        await Promise.resolve();

        expect(cacheMissLoad.defaultPrevented).toBe(true);
        expect(reload).not.toHaveBeenCalled();
        expect(steps.liveApply).not.toHaveBeenCalled();
        expect(steps.applyRefresh).not.toHaveBeenCalled();

        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        document.dispatchEvent(new Event('htmx:load'));
        expect(reload).not.toHaveBeenCalled();
        expect(steps.liveApply).toHaveBeenCalledOnce();
        expect(steps.applyRefresh).toHaveBeenCalledOnce();
    });

    test.each([
        { completionOrder: ['first', 'second'], firstType: 'hit', secondType: 'hit' },
        { completionOrder: ['second', 'first'], firstType: 'hit', secondType: 'hit' },
        { completionOrder: ['first', 'second'], firstType: 'hit', secondType: 'miss' },
        { completionOrder: ['second', 'first'], firstType: 'hit', secondType: 'miss' },
        { completionOrder: ['first', 'second'], firstType: 'miss', secondType: 'hit' },
        { completionOrder: ['second', 'first'], firstType: 'miss', secondType: 'hit' },
        { completionOrder: ['first', 'second'], firstType: 'miss', secondType: 'miss' },
        { completionOrder: ['second', 'first'], firstType: 'miss', secondType: 'miss' },
    ])(
        'serializes $firstType->$secondType history intents when bodies finish $completionOrder',
        async ({ completionOrder, firstType, secondType }) => {
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            const firstXHR = firstType === 'miss' ? historyXHR() : undefined;
            const secondXHR = secondType === 'miss' ? historyXHR() : undefined;
            const first = historyEvent(
                firstType === 'miss' ? 'htmx:historyCacheMiss' : 'htmx:historyCacheHit',
                firstXHR,
            );
            const second = historyEvent(
                secondType === 'miss' ? 'htmx:historyCacheMiss' : 'htmx:historyCacheHit',
                secondXHR,
            );

            document.body.dispatchEvent(first);
            document.body.dispatchEvent(second);

            expect(first.defaultPrevented).toBe(false);
            expect(second.defaultPrevented).toBe(true);
            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            expect(steps.liveTeardown).toHaveBeenCalledOnce();
            expect(steps.pauseRefresh).toHaveBeenCalledOnce();
            expect(steps.liveResetPage).toHaveBeenCalledOnce();

            for (const completion of completionOrder) {
                const xhr = completion === 'first' ? firstXHR : secondXHR;
                if (xhr) {
                    document.body.dispatchEvent(historyEvent('htmx:historyCacheMissLoad', xhr));
                }
                document.body.innerHTML = `<main id="${completion}-history-body"></main>`;
                document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
                document
                    .getElementById(`${completion}-history-body`)
                    ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
            }
            await Promise.resolve();

            expect(reload).toHaveBeenCalledOnce();
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.applyRefresh).not.toHaveBeenCalled();
        },
    );

    test.each(['wrong xhr', 'wrong phase'] as const)(
        'fails closed when cache-miss load has %s identity',
        (failure) => {
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            const xhr = historyXHR();
            if (failure === 'wrong xhr') {
                document.body.dispatchEvent(historyEvent('htmx:historyCacheMiss', xhr));
            } else {
                document.body.dispatchEvent(historyEvent('htmx:historyCacheHit'));
            }

            const staleLoad = historyEvent('htmx:historyCacheMissLoad', historyXHR());
            document.body.dispatchEvent(staleLoad);

            expect(staleLoad.defaultPrevented).toBe(true);
            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
        },
    );

    test.each(['pending', 'reloading'] as const)(
        'a normal body swap cannot clear a %s history gate',
        (state) => {
            document.body.innerHTML = '<main id="outgoing-list">old body</main>';
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            document.body.dispatchEvent(historyEvent('htmx:historyCacheHit'));
            if (state === 'reloading') {
                document.body.dispatchEvent(historyEvent('htmx:historyCacheHit'));
            }
            const beforeSwap = new CustomEvent('htmx:beforeSwap', {
                bubbles: true,
                cancelable: true,
                detail: { target: document.body, xhr: { status: 200 } },
            });

            document.body.dispatchEvent(beforeSwap);

            expect(beforeSwap.defaultPrevented).toBe(true);
            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            document.body.innerHTML = '<main id="late-normal-body"></main>';
            document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
            document
                .getElementById('late-normal-body')
                ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.applyRefresh).not.toHaveBeenCalled();
        },
    );

    test.each(['hit', 'miss'] as const)(
        'an older normal body completion cannot clear a newer history %s gate',
        (historyKind) => {
            document.body.innerHTML = '<main id="outgoing-list">old body</main>';
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            const normal = dispatchNormalBodyBeforeSwap();
            const xhr = historyKind === 'miss' ? historyXHR() : undefined;
            const history = historyEvent(
                historyKind === 'miss' ? 'htmx:historyCacheMiss' : 'htmx:historyCacheHit',
                xhr,
            );

            document.body.dispatchEvent(history);

            expect(normal.defaultPrevented).toBe(false);
            expect(history.defaultPrevented).toBe(true);
            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            document.body.innerHTML = '<main id="late-normal-body"></main>';
            document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
            document
                .getElementById('late-normal-body')
                ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
            expect(reload).toHaveBeenCalledOnce();
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.applyRefresh).not.toHaveBeenCalled();
        },
    );

    test.each(['prevented', 'shouldSwap=false'] as const)(
        'a later listener making a normal body swap %s triggers one reload',
        async (failure) => {
            document.body.innerHTML = '<main id="outgoing-list">old body</main>';
            const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
            document.addEventListener(
                'htmx:beforeSwap',
                (event) => {
                    const detail = (event as CustomEvent).detail;
                    if (detail?.target !== document.body) return;
                    if (failure === 'prevented') event.preventDefault();
                    else detail.shouldSwap = false;
                },
                { once: true },
            );

            const beforeSwap = dispatchNormalBodyBeforeSwap();
            await Promise.resolve();

            expect(beforeSwap.defaultPrevented).toBe(failure === 'prevented');
            expect(reload).toHaveBeenCalledExactlyOnceWith(0);
            document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
            document.dispatchEvent(new Event('htmx:load'));
            expect(steps.liveApply).not.toHaveBeenCalled();
            expect(steps.applyRefresh).not.toHaveBeenCalled();
        },
    );

    test('an accepted normal body swap may complete after the cancellation microtask', async () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});

        dispatchNormalBodyBeforeSwap();
        await Promise.resolve();

        expect(reload).not.toHaveBeenCalled();
        expect(steps.liveApply).not.toHaveBeenCalled();

        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expect(reload).not.toHaveBeenCalled();
        expect(steps.liveApply).toHaveBeenCalledOnce();
        expect(steps.applyRefresh).toHaveBeenCalledOnce();
    });

    test('a normal body swapError dispatched on its request source reloads immediately', () => {
        const reload = vi.spyOn(window.history, 'go').mockImplementation(() => {});
        const source = document.createElement('a');
        document.body.appendChild(source);
        dispatchNormalBodyBeforeSwap();

        source.dispatchEvent(
            new CustomEvent('htmx:swapError', {
                bubbles: true,
                detail: { target: document.body },
            }),
        );

        expect(reload).toHaveBeenCalledExactlyOnceWith(0);
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        source.dispatchEvent(new Event('htmx:load', { bubbles: true }));
        expect(steps.liveApply).not.toHaveBeenCalled();
        expect(steps.applyRefresh).not.toHaveBeenCalled();
    });

    test.each([
        {
            name: 'detail to list',
            outgoing: '<main id="outgoing-detail">detail</main>',
        },
        {
            name: 'list to list',
            outgoing:
                '<main id="outgoing-list"><div id="resource-list-content" data-screen="old-list"></div></main>',
        },
    ])('delays $name Live initialization until the cached body actually lands', ({ outgoing }) => {
        document.body.innerHTML = outgoing;
        const observedBodies: string[] = [];
        steps.liveApply.mockImplementation(() => {
            observedBodies.push(
                document.getElementById('restored-list')?.dataset.screen || 'outgoing',
            );
        });

        document.body.dispatchEvent(new Event('htmx:historyCacheHit', { bubbles: true }));
        document.querySelector('main')?.dispatchEvent(new Event('htmx:load', { bubbles: true }));
        document.body.dispatchEvent(new Event('htmx:historyRestore', { bubbles: true }));

        expect(steps.liveApply).not.toHaveBeenCalled();
        expect(observedBodies).toStrictEqual([]);

        document.body.innerHTML =
            '<main id="restored-list" data-screen="cached-list"><div id="resource-list-content"></div></main>';
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));
        document
            .getElementById('restored-list')
            ?.dispatchEvent(new Event('htmx:load', { bubbles: true }));

        expect(steps.liveApply).toHaveBeenCalledOnce();
        expect(observedBodies).toStrictEqual(['cached-list']);
        expect(steps.liveTeardown).toHaveBeenCalledOnce();
        expect(steps.liveResetPage).toHaveBeenCalledOnce();
    });

    test('a synchronous cache restore keeps the stream opened by body afterSwap', () => {
        document.body.innerHTML = '<main id="outgoing-detail">detail</main>';
        document.body.dispatchEvent(new Event('htmx:historyCacheHit', { bubbles: true }));
        document.body.innerHTML = '<main id="restored-list" data-screen="cached-list"></main>';
        document.body.dispatchEvent(new Event('htmx:afterSwap', { bubbles: true }));

        expect(steps.liveApply).toHaveBeenCalledOnce();
        document.body.dispatchEvent(new Event('htmx:historyRestore', { bubbles: true }));

        expect(steps.liveApply).toHaveBeenCalledOnce();
        expect(steps.liveTeardown).toHaveBeenCalledOnce();
        expect(steps.liveResetPage).toHaveBeenCalledOnce();
    });
});

describe('sticky namespace measurement', () => {
    test('measures a real namespace row and clears stale state from other table shapes', () => {
        document.body.innerHTML = `
            <div class="ro-table-wrap">
                <table id="all" class="ro-table">
                    <tbody>
                        <tr class="ro-vspacer"><td id="spacer" class="cell-ns"></td></tr>
                        <tr><td id="namespace" class="cell-ns"></td></tr>
                    </tbody>
                </table>
                <table id="single" class="ro-table ro-sticky2" style="--ns-col-w: 123px">
                    <tbody><tr><td class="cell-name"></td></tr></tbody>
                </table>
                <table id="empty" class="ro-table ro-sticky2" style="--ns-col-w: 456px">
                    <tbody></tbody>
                </table>
            </div>
        `;
        const all = document.getElementById('all') as HTMLTableElement;
        const single = document.getElementById('single') as HTMLTableElement;
        const empty = document.getElementById('empty') as HTMLTableElement;
        const spacer = document.getElementById('spacer') as HTMLTableCellElement;
        const namespace = document.getElementById('namespace') as HTMLTableCellElement;
        const spacerMeasure = vi
            .spyOn(spacer, 'getBoundingClientRect')
            .mockReturnValue({ width: 999 } as DOMRect);
        const namespaceMeasure = vi
            .spyOn(namespace, 'getBoundingClientRect')
            .mockReturnValue({ width: 72 } as DOMRect);

        document.dispatchEvent(new Event('htmx:afterSettle'));

        expect(spacerMeasure).not.toHaveBeenCalled();
        expect(namespaceMeasure).toHaveBeenCalledOnce();
        expect(all).toHaveClass('ro-sticky2');
        expect(all.style.getPropertyValue('--ns-col-w')).toBe('72px');
        for (const stale of [single, empty]) {
            expect(stale).not.toHaveClass('ro-sticky2');
            expect(stale.style.getPropertyValue('--ns-col-w')).toBe('');
        }

        namespaceMeasure.mockReturnValue({ width: 96 } as DOMRect);
        window.dispatchEvent(new Event('resize'));

        expect(namespaceMeasure).toHaveBeenCalledTimes(2);
        expect(all.style.getPropertyValue('--ns-col-w')).toBe('96px');
    });
});

describe('exact active navigation no-op', () => {
    function navigationEvent(
        anchor: HTMLAnchorElement,
        overrides: Record<string, unknown> = {},
    ): CustomEvent {
        return new CustomEvent('htmx:configRequest', {
            bubbles: true,
            cancelable: true,
            detail: {
                boosted: true,
                elt: anchor,
                path: anchor.getAttribute('href'),
                target: document.body,
                triggeringEvent: new MouseEvent('click', { button: 0 }),
                verb: 'get',
                ...overrides,
            },
        });
    }

    test('cancels an exact current active sidebar or tab click before request', () => {
        const original = window.location.href;
        window.history.replaceState(null, '', '/clusters/prod/namespaces');
        document.body.innerHTML = `
            <aside><a id="sidebar" class="menu-item is-active"
                href="/clusters/prod/namespaces">Namespaces</a></aside>
            <nav><span class="is-active"><a id="tab"
                href="/clusters/prod/namespaces">Namespaces tab</a></span></nav>
        `;

        for (const id of ['sidebar', 'tab']) {
            const anchor = document.getElementById(id) as HTMLAnchorElement;
            const event = navigationEvent(anchor);
            anchor.dispatchEvent(event);
            expect(event.defaultPrevented).toBe(true);
        }

        window.history.replaceState(null, '', original);
    });

    test('treats the Default tab bare query delimiter as the current screen', () => {
        const original = window.location.href;
        window.history.replaceState(
            null,
            '',
            '/clusters/prod/namespaces/checkout/pods/checkout-api-9f2a-h8k7p',
        );
        document.body.innerHTML = '<nav><a class="is-active" href="?">Default</a></nav>';
        const anchor = document.querySelector('a') as HTMLAnchorElement;
        const event = navigationEvent(anchor);

        anchor.dispatchEvent(event);

        expect(event.defaultPrevented).toBe(true);
        window.history.replaceState(null, '', original);
    });

    test('keeps Default useful when it clears a non-empty detail query', () => {
        const original = window.location.href;
        window.history.replaceState(
            null,
            '',
            '/clusters/prod/namespaces/checkout/pods/checkout-api-9f2a-h8k7p?view=yaml',
        );
        document.body.innerHTML = '<nav><a class="is-active" href="?">Default</a></nav>';
        const anchor = document.querySelector('a') as HTMLAnchorElement;
        const event = navigationEvent(anchor);

        anchor.dispatchEvent(event);

        expect(event.defaultPrevented).toBe(false);
        window.history.replaceState(null, '', original);
    });

    test('keeps useful active-path, retry, fragment, modifier, and non-body requests', () => {
        const original = window.location.href;
        window.history.replaceState(null, '', '/clusters/prod/namespaces?sort=Name');
        document.body.innerHTML = `
            <a id="active" class="is-active" href="/clusters/prod/namespaces">Namespaces</a>
            <a id="exact" class="is-active"
                href="/clusters/prod/namespaces?sort=Name">Current tab</a>
            <a id="retry" href="/clusters/prod/namespaces?sort=Name">Retry</a>
            <a id="fragment" class="is-active" href="#section">Section</a>
        `;
        const active = document.getElementById('active') as HTMLAnchorElement;
        const exact = document.getElementById('exact') as HTMLAnchorElement;
        const retry = document.getElementById('retry') as HTMLAnchorElement;
        const fragment = document.getElementById('fragment') as HTMLAnchorElement;
        const cases = [
            navigationEvent(active), // clears the current query
            navigationEvent(retry), // same URL but not active navigation
            navigationEvent(fragment, { path: window.location.pathname + window.location.search }),
            navigationEvent(exact, {
                path: window.location.pathname + window.location.search,
                triggeringEvent: new MouseEvent('click', { metaKey: true }),
            }),
            navigationEvent(exact, {
                path: window.location.pathname + window.location.search,
                target: document.createElement('div'),
            }),
            navigationEvent(exact, {
                path: window.location.pathname + window.location.search,
                verb: 'post',
            }),
        ];

        for (const event of cases) {
            expect(() => suppressRedundantActiveNavigation(event)).not.toThrow();
            expect(event.defaultPrevented).toBe(false);
        }

        for (const detail of [undefined, null, 7, {}, { boosted: true }]) {
            const event = new CustomEvent('htmx:configRequest', { cancelable: true, detail });
            expect(() => suppressRedundantActiveNavigation(event)).not.toThrow();
            expect(event.defaultPrevented).toBe(false);
        }

        window.history.replaceState(null, '', original);
    });

    test('requires every exact-navigation proof before suppressing the request', () => {
        const original = window.location.href;
        window.history.replaceState(null, '', '/clusters/prod/namespaces?sort=Name');
        document.body.innerHTML = `
            <a id="active" class="is-active"
                href="/clusters/prod/namespaces?sort=Name">Namespaces</a>
            <button id="button" class="is-active">Not an anchor</button>
        `;
        const active = document.getElementById('active') as HTMLAnchorElement;
        const button = document.getElementById('button') as HTMLButtonElement;
        const currentPath = window.location.pathname + window.location.search;
        const otherOrigin = new URL(currentPath, 'https://example.invalid').href;
        const cases: ReadonlyArray<readonly [string, Record<string, unknown>]> = [
            ['not boosted', { boosted: false }],
            ['not a body target', { target: document.createElement('main') }],
            ['not a GET', { verb: 'post' }],
            ['not an anchor', { elt: button }],
            ['not a mouse event', { triggeringEvent: new Event('click') }],
            ['not a click', { triggeringEvent: new MouseEvent('mousedown', { button: 0 }) }],
            ['not the primary button', { triggeringEvent: new MouseEvent('click', { button: 1 }) }],
            ['Alt click', { triggeringEvent: new MouseEvent('click', { altKey: true }) }],
            ['Control click', { triggeringEvent: new MouseEvent('click', { ctrlKey: true }) }],
            ['Meta click', { triggeringEvent: new MouseEvent('click', { metaKey: true }) }],
            ['Shift click', { triggeringEvent: new MouseEvent('click', { shiftKey: true }) }],
            ['configured origin differs', { path: otherOrigin }],
            ['configured pathname differs', { path: '/clusters/prod/services?sort=Name' }],
            ['configured search differs', { path: '/clusters/prod/namespaces?sort=Age' }],
            ['configured hash differs', { path: `${currentPath}#other` }],
        ];

        for (const [label, overrides] of cases) {
            const event = navigationEvent(active, overrides);
            suppressRedundantActiveNavigation(event);
            expect(event.defaultPrevented, label).toBe(false);
        }

        const rawMismatches = [
            otherOrigin,
            '/clusters/prod/services?sort=Name',
            '/clusters/prod/namespaces?sort=Age',
        ];
        for (const href of rawMismatches) {
            active.setAttribute('href', href);
            const event = navigationEvent(active, { path: currentPath });
            suppressRedundantActiveNavigation(event);
            expect(event.defaultPrevented, href).toBe(false);
        }

        window.history.replaceState(null, '', original);
    });
});

describe('sort preference gate', () => {
    test('persists only a direct, pushable sort-header request', () => {
        document.body.innerHTML = `
            <table><thead><tr><th><a id="sort">Sort</a></th></tr></thead></table>
            <button id="outside">Outside</button>
            <div id="resource-list-content"></div>
            <div id="other-target"></div>
        `;
        const sort = document.getElementById('sort') as HTMLAnchorElement;
        const outside = document.getElementById('outside') as HTMLButtonElement;
        const target = document.getElementById('resource-list-content') as HTMLElement;
        const path = '/clusters/prod/namespaces/default/pods/_table?sort=Age%3Adesc';

        dispatchBeforeRequest({
            elt: sort,
            target,
            requestConfig: { headers: {}, path },
        });
        expect(steps.roPrefsSetSort).toHaveBeenCalledExactlyOnceWith('pods', 'Age:desc');

        steps.roPrefsSetSort.mockClear();
        dispatchBeforeRequest({
            elt: sort,
            target,
            requestConfig: { headers: { 'RO-No-Push': '1' }, path },
        });
        dispatchBeforeRequest({
            elt: sort,
            target,
            requestConfig: { headers: { 'HX-Preloaded': 'true' }, path },
        });
        dispatchBeforeRequest({
            elt: outside,
            target,
            requestConfig: { headers: {}, path },
        });
        dispatchBeforeRequest({
            elt: sort,
            target: document.getElementById('other-target'),
            requestConfig: { headers: {}, path },
        });

        expect(steps.roPrefsSetSort).not.toHaveBeenCalled();
    });

    test('accepts an encoded plural and a request with no headers', () => {
        document.body.innerHTML = `
            <table><thead><tr><th><a id="sort">Sort</a></th></tr></thead></table>
            <div id="resource-list-content"></div>
        `;
        const sort = document.getElementById('sort') as HTMLAnchorElement;
        const target = document.getElementById('resource-list-content') as HTMLElement;

        dispatchBeforeRequest({
            elt: sort,
            target,
            requestConfig: { path: '/clusters/prod/namespaces/default/%70ods/_table?sort=Name' },
        });

        expect(steps.roPrefsSetSort).toHaveBeenCalledExactlyOnceWith('pods', 'Name');
    });

    test.each([
        { name: 'no event detail', detail: undefined },
        { name: 'no request config', detail: {} },
        {
            name: 'no source element',
            detail: {
                target: document.createElement('div'),
                requestConfig: { path: '/pods/_table?sort=Name' },
            },
        },
        {
            name: 'no target',
            detail: {
                elt: document.createElement('a'),
                requestConfig: { path: '/pods/_table?sort=Name' },
            },
        },
    ])('ignores incomplete request metadata: $name', ({ detail }) => {
        expect(() => dispatchBeforeRequest(detail)).not.toThrow();
        expect(steps.roPrefsSetSort).not.toHaveBeenCalled();
    });

    test.each([
        { name: 'missing path', path: undefined },
        { name: 'empty path', path: '' },
        { name: 'non-table path', path: '/pods?sort=Name' },
        { name: 'empty plural segment', path: '//_table?sort=Name' },
        { name: 'table-like suffix', path: '/pods/_table-extra?sort=Name' },
        { name: 'table path without sort', path: '/pods/_table' },
        { name: 'table path with empty sort', path: '/pods/_table?sort=' },
        { name: 'unparseable URL', path: 'http://[/pods/_table?sort=Name' },
        { name: 'malformed plural escape', path: '/%ZZ/_table?sort=Name' },
    ])('does not persist an untrustworthy sort request: $name', ({ path }) => {
        document.body.innerHTML = `
            <table><thead><tr><th><a id="sort">Sort</a></th></tr></thead></table>
            <div id="resource-list-content"></div>
        `;
        const sort = document.getElementById('sort') as HTMLAnchorElement;
        const target = document.getElementById('resource-list-content') as HTMLElement;

        expect(() =>
            dispatchBeforeRequest({
                elt: sort,
                target,
                requestConfig: { headers: {}, path },
            }),
        ).not.toThrow();
        expect(steps.roPrefsSetSort).not.toHaveBeenCalled();
    });

    test('ignores a source that cannot prove it came from a sort header', () => {
        const target = document.createElement('div');
        target.id = 'resource-list-content';

        dispatchBeforeRequest({
            elt: { closest: null },
            target,
            requestConfig: { headers: {}, path: '/pods/_table?sort=Name' },
        });

        expect(steps.roPrefsSetSort).not.toHaveBeenCalled();
    });
});

test('registers every required resident document and window event at module load', async () => {
    // The first import happens while Vitest is collecting this file, outside an
    // individual test's coverage window. Re-evaluate the side-effect module here
    // so registration itself is observable and mutation-covered.
    vi.resetModules();
    const documentAdd = vi.spyOn(document, 'addEventListener');
    const windowAdd = vi.spyOn(window, 'addEventListener');
    const documentStart = documentAdd.mock.calls.length;
    const windowStart = windowAdd.mock.calls.length;

    await import('./init.js');

    const documentRegistrations = documentAdd.mock.calls
        .slice(documentStart)
        .map(([type, listener]) => ({ listener: typeof listener, type }));
    const windowRegistrations = windowAdd.mock.calls
        .slice(windowStart)
        .map(([type, listener]) => ({ listener: typeof listener, type }));
    expect(documentRegistrations).toEqual(
        expect.arrayContaining([
            { type: 'htmx:configRequest', listener: 'function' },
            { type: 'htmx:beforeRequest', listener: 'function' },
            { type: 'htmx:afterSwap', listener: 'function' },
            { type: 'htmx:beforeSwap', listener: 'function' },
            { type: 'htmx:historyCacheHit', listener: 'function' },
            { type: 'htmx:historyCacheMiss', listener: 'function' },
            { type: 'htmx:historyCacheMissLoad', listener: 'function' },
            { type: 'htmx:historyCacheMissLoadError', listener: 'function' },
            { type: 'htmx:historyRestore', listener: 'function' },
            { type: 'htmx:swapError', listener: 'function' },
            { type: 'DOMContentLoaded', listener: 'function' },
            { type: 'htmx:afterSettle', listener: 'function' },
        ]),
    );
    expect(documentRegistrations).not.toContainEqual({
        type: 'htmx:load',
        listener: 'function',
    });
    expect(windowRegistrations).toEqual(
        expect.arrayContaining([
            { type: 'pageshow', listener: 'function' },
            { type: 'resize', listener: 'function' },
        ]),
    );
});
