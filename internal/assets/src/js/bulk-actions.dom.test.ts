// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const rowSelection = vi.hoisted(() => ({
    clearRowState: vi.fn<() => void>(),
    roCopyText: vi.fn<(text: string, done: (ok: boolean) => void) => void>(),
}));

vi.mock('./row-selection.js', () => ({
    BULK_NAMES_MAX: 100,
    clearRowState: rowSelection.clearRowState,
    roCopyText: rowSelection.roCopyText,
}));

import { bulkBindings } from './bulk-actions.js';

interface SelectedEntry {
    key: string;
    name: string;
}

function binding(selector: string): Binding {
    const found = bulkBindings.find((candidate) => candidate.selector === selector);
    expect(found).toBeDefined();
    return found as Binding;
}

function setSelectedEntries(entries: SelectedEntry[]): void {
    (window as unknown as { roRowState: { selectedEntries(): SelectedEntry[] } }).roRowState = {
        selectedEntries: () => entries,
    };
}

function renderBulkBar(): HTMLElement {
    document.body.innerHTML = `
        <div id="ro-bulkbar" data-bulk-href="#/bulk?format=yaml"
            data-bulk-cluster="prod" data-bulk-allns="false">
            <button id="ro-bulk-copy"><span aria-hidden="true">icon</span><span>Copy names</span></button>
            <button id="ro-bulk-download">Download YAML</button>
            <button id="ro-bulk-clear">Clear</button>
        </div>
    `;
    return document.getElementById('ro-bulkbar') as HTMLElement;
}

beforeEach(() => {
    renderBulkBar();
    setSelectedEntries([]);
    window.history.replaceState(null, '', '/');
});

test('declares the exact delegated-event contract', () => {
    expect(bulkBindings.map(({ event, selector, stop }) => ({ event, selector, stop }))).toEqual([
        { event: 'click', selector: '#ro-bulk-download', stop: true },
        { event: 'click', selector: '#ro-bulk-copy', stop: true },
        { event: 'click', selector: '#ro-bulk-clear', stop: true },
    ]);
});

describe('bulk copy', () => {
    test('copies every full name and keeps feedback visible for 1100ms from the latest copy', () => {
        vi.useFakeTimers();
        setSelectedEntries([
            { key: 'prod/default/api-0', name: 'api-full-name' },
            { key: 'prod/default/db-0', name: 'db-full-name' },
        ]);
        rowSelection.roCopyText.mockImplementation((_text, done) => done(true));
        const button = document.getElementById('ro-bulk-copy') as HTMLElement;
        const label = button.querySelector('span:last-child');
        const copyBinding = binding('#ro-bulk-copy');

        expect(copyBinding.handler(new MouseEvent('click'), button)).toBe(true);
        expect(copyBinding.stop).toBe(true);
        expect(rowSelection.roCopyText).toHaveBeenCalledExactlyOnceWith(
            'api-full-name\ndb-full-name',
            expect.any(Function),
        );
        expect(label).toHaveTextContent('Copied');

        vi.advanceTimersByTime(1000);
        copyBinding.handler(new MouseEvent('click'), button);
        vi.advanceTimersByTime(100);
        expect(label).toHaveTextContent('Copied');
        vi.advanceTimersByTime(999);
        expect(label).toHaveTextContent('Copied');
        vi.advanceTimersByTime(1);
        expect(label).toHaveTextContent('Copy names');
    });

    test('does not claim success when the clipboard path fails', () => {
        rowSelection.roCopyText.mockImplementation((_text, done) => done(false));
        const button = document.getElementById('ro-bulk-copy') as HTMLElement;

        binding('#ro-bulk-copy').handler(new MouseEvent('click'), button);

        expect(button.querySelector('span:last-child')).toHaveTextContent('Copy names');
    });
});

describe('bulk download', () => {
    test('uses full names for a single-namespace list and URL-encodes the joined grammar', () => {
        setSelectedEntries([
            { key: 'prod/default/api-0', name: 'api full' },
            { key: 'prod/default/db-0', name: 'db/primary' },
        ]);
        const button = document.getElementById('ro-bulk-download') as HTMLElement;

        expect(binding('#ro-bulk-download').handler(new MouseEvent('click'), button)).toBe(true);

        expect(window.location.hash).toBe('#/bulk?format=yaml&names=api%20full%2Cdb%2Fprimary');
    });

    test('uses namespace/name in all-namespaces scope and falls back safely on another cluster', () => {
        const bar = document.getElementById('ro-bulkbar') as HTMLElement;
        bar.dataset.bulkAllns = 'true';
        setSelectedEntries([
            { key: 'prod/team-a/api-0', name: 'api-0' },
            { key: 'other/team-b/db-0', name: 'db fallback' },
        ]);

        binding('#ro-bulk-download').handler(
            new MouseEvent('click'),
            document.getElementById('ro-bulk-download'),
        );

        expect(window.location.hash).toBe(
            '#/bulk?format=yaml&names=team-a%2Fapi-0%2Cdb%20fallback',
        );
    });

    test('does not navigate for an empty or over-cap selection', () => {
        const button = document.getElementById('ro-bulk-download') as HTMLElement;
        const download = binding('#ro-bulk-download');
        window.location.hash = '#before';

        setSelectedEntries([]);
        download.handler(new MouseEvent('click'), button);
        expect(window.location.hash).toBe('#before');

        setSelectedEntries(
            Array.from({ length: 101 }, (_, index) => ({
                key: `prod/default/item-${index}`,
                name: `item-${index}`,
            })),
        );
        download.handler(new MouseEvent('click'), button);
        expect(window.location.hash).toBe('#before');
    });

    test('allows exactly the 100-name cap', () => {
        const button = document.getElementById('ro-bulk-download') as HTMLElement;
        setSelectedEntries(
            Array.from({ length: 100 }, (_, index) => ({
                key: `prod/default/item-${index}`,
                name: `item-${index}`,
            })),
        );

        binding('#ro-bulk-download').handler(new MouseEvent('click'), button);

        expect(window.location.hash).toContain('&names=item-0%2Citem-1%2C');
        expect(window.location.hash).toContain('item-99');
    });

    test('uses full names when an all-namespaces bar has no cluster identity', () => {
        const bar = document.getElementById('ro-bulkbar') as HTMLElement;
        bar.dataset.bulkAllns = 'true';
        delete bar.dataset.bulkCluster;
        setSelectedEntries([{ key: '/team-a/api-0', name: 'api full name' }]);

        binding('#ro-bulk-download').handler(
            new MouseEvent('click'),
            document.getElementById('ro-bulk-download'),
        );

        expect(window.location.hash).toBe('#/bulk?format=yaml&names=api%20full%20name');
    });

    test('does not navigate when the server did not provide a bulk href', () => {
        const bar = document.getElementById('ro-bulkbar') as HTMLElement;
        delete bar.dataset.bulkHref;
        setSelectedEntries([{ key: 'prod/default/api-0', name: 'api-0' }]);

        binding('#ro-bulk-download').handler(
            new MouseEvent('click'),
            document.getElementById('ro-bulk-download'),
        );

        expect(window.location.hash).toBe('');
    });
});

test('the Clear action delegates to the row-state owner and stops the bulk branch', () => {
    const clear = binding('#ro-bulk-clear');

    expect(clear.handler(new MouseEvent('click'), document.getElementById('ro-bulk-clear'))).toBe(
        true,
    );

    expect(clear.stop).toBe(true);
    expect(rowSelection.clearRowState).toHaveBeenCalledOnce();
});
