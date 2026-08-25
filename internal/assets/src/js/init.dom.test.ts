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
import './init.js';

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

function dispatchBeforeRequest(detail: object): void {
    document.dispatchEvent(new CustomEvent('htmx:beforeRequest', { detail }));
}

beforeEach(() => {
    vi.clearAllMocks();
    steps.colsPopOpen.mockReturnValue(false);
    steps.isListRefreshEvent.mockReturnValue(false);
    steps.liveState.status = 'idle';
    steps.liveState.streamPath = '';
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
});

describe('htmx swap lifecycle', () => {
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
});
