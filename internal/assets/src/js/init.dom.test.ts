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
    liveState: { status: 'idle', streamPath: '' },
    liveTeardown: vi.fn(),
    noteRefreshRecovery: vi.fn(),
    reapplyRowState: vi.fn(),
    roPrefsSetSort: vi.fn(),
    setColsPopOpen: vi.fn(),
    showToast: vi.fn(),
    syncColsPopState: vi.fn(),
    syncRefreshUI: vi.fn(),
    syncThemeTogglePostTarget: vi.fn(),
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
vi.mock('./live.js', () => ({
    liveApply: steps.liveApply,
    liveOnListSwap: steps.liveOnListSwap,
    liveState: steps.liveState,
    liveTeardown: steps.liveTeardown,
}));
vi.mock('./logs.js', () => ({ initLogsFollow: steps.initLogsFollow }));
vi.mock('./misc-ui.js', () => ({ collapseSectionsFromHash: steps.collapseSectionsFromHash }));
vi.mock('./prefs.js', () => ({ roPrefsSetSort: steps.roPrefsSetSort }));
vi.mock('./refresh.js', () => ({
    applyRefresh: steps.applyRefresh,
    noteRefreshRecovery: steps.noteRefreshRecovery,
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
import { handleSortPreferenceRequest } from './init.js';

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
        steps.liveApply,
        steps.applyRefresh,
        steps.buildYamlFolds,
        steps.collapseSectionsFromHash,
        steps.highlightYamlLine,
        steps.initLogsFollow,
        steps.syncThemeTogglePostTarget,
        steps.captureRowModelFromDocument,
        steps.virtualizeInit,
        steps.syncColsPopState,
        steps.reapplyRowState,
        steps.updateBulkBar,
    );
}

function dispatchBeforeRequest(detail?: object): void {
    handleSortPreferenceRequest(new CustomEvent('htmx:beforeRequest', { detail }));
}

beforeEach(() => {
    vi.clearAllMocks();
    steps.colsPopOpen.mockReturnValue(false);
    steps.isListRefreshEvent.mockReturnValue(false);
    steps.liveState.status = 'idle';
    steps.liveState.streamPath = '';
});

test('publishes showToast through the resident window seam', () => {
    expect(window.roToast).toBe(steps.showToast);

    window.roToast?.('Detached result');

    expect(steps.showToast).toHaveBeenCalledExactlyOnceWith('Detached result');
});

describe('runInit orchestration', () => {
    test('runs the complete repair chain once and in order for both load events', () => {
        document.dispatchEvent(new Event('DOMContentLoaded'));
        expectInitOrder();

        vi.clearAllMocks();
        document.dispatchEvent(new Event('htmx:load'));
        expectInitOrder();
    });

    test('isolates a failing step and continues the remaining chain', () => {
        const failure = new Error('live setup failed');
        steps.liveApply.mockImplementationOnce(() => {
            throw failure;
        });
        const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

        document.dispatchEvent(new Event('htmx:load'));

        expectCalledOnceInOrder(
            steps.syncRefreshUI,
            steps.liveApply,
            steps.applyRefresh,
            steps.captureRowModelFromDocument,
            steps.virtualizeInit,
            steps.updateBulkBar,
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

        document.dispatchEvent(new Event('htmx:load'));

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
        );
        expect(steps.liveState).toStrictEqual({ status: 'idle', streamPath: '' });
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
            detail: { target: document.getElementById('resource-list-content') },
        });

        document.dispatchEvent(event);

        expectCalledOnceInOrder(
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
        expect(steps.liveOnListSwap).toHaveBeenCalledExactlyOnceWith(event);
    });

    test('ignores an afterSwap that is not a list refresh', () => {
        const event = new CustomEvent('htmx:afterSwap', { detail: { target: document.body } });

        document.dispatchEvent(event);

        expect(steps.isListRefreshEvent).toHaveBeenCalledExactlyOnceWith(event);
        expect(steps.noteRefreshRecovery).not.toHaveBeenCalled();
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

    test('repaints row and bulk state in order on history restore', () => {
        document.dispatchEvent(new Event('htmx:historyRestore'));

        expectCalledOnceInOrder(steps.reapplyRowState, steps.updateBulkBar);
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
            { type: 'htmx:beforeRequest', listener: 'function' },
            { type: 'htmx:afterSwap', listener: 'function' },
            { type: 'htmx:beforeSwap', listener: 'function' },
            { type: 'htmx:historyRestore', listener: 'function' },
            { type: 'DOMContentLoaded', listener: 'function' },
            { type: 'htmx:load', listener: 'function' },
            { type: 'htmx:afterSettle', listener: 'function' },
        ]),
    );
    expect(windowRegistrations).toEqual(
        expect.arrayContaining([{ type: 'resize', listener: 'function' }]),
    );
});
