// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';
import { applyLiveNameFilter } from './filters.js';
import {
    adoptListProjection,
    listProjectionCardByKey,
    listProjectionOrder,
    listProjectionRevision,
    listProjectionRowByKey,
    listProjectionRowModel,
    listProjectionRows,
    resetListProjection,
    setListProjectionVisibleKeys,
} from './list-projection.js';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    type LiveV2Cursor,
    type LiveV2Delta,
    type LiveV2DeltaEnvelope,
} from './live-protocol.js';
import { virtualizeInit } from './virtualizer.js';

interface FixtureOptions {
    cards?: boolean;
    draft?: string;
    windowed?: boolean;
}

function rowHTML(key: string, name: string, status = 'Ready'): string {
    let id = 'row-';
    for (const character of key) {
        const code = character.codePointAt(0) as number;
        id +=
            code <= 0x20 ||
            character === '"' ||
            character === '\\' ||
            character === '%' ||
            code === 0x7f
                ? `%${code.toString(16).toUpperCase().padStart(2, '0')}`
                : character;
    }
    return `<tr id="${id}" data-key="${key}" data-name="${name}"><td class="cell-name"><a href="#${name}">${name}</a></td><td>${status}</td></tr>`;
}

function cardHTML(key: string, name: string): string {
    return `<div class="ro-pcard" data-key="${key}"><a href="#${name}">${name} card</a></div>`;
}

function buildList(keys: readonly string[], options: FixtureOptions = {}): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.innerHTML = `
        <input id="ro-filter-input" value="${options.draft || ''}">
        <div class="ro-table-wrap${options.windowed ? ' ro-windowed' : ''}" tabindex="0">
            <table class="ro-table">
                <thead><tr><th data-hint="string">Name</th><th data-hint="enum">Status</th></tr></thead>
                <tbody></tbody>
            </table>
        </div>
        ${options.cards ? '<div class="ro-cardlist"></div>' : ''}
        <span class="ro-count" data-ro-live-region="count">0</span>
        <div class="ro-phase-strip" data-ro-live-region="phase" hidden></div>
        <span class="ro-foundline" data-ro-live-region="found">old found</span>
        <div id="ro-live-status" role="status" aria-live="polite"></div>`;
    const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
    const cards = content.querySelector('.ro-cardlist');
    keys.forEach((key) => {
        tbody.insertAdjacentHTML('beforeend', rowHTML(key, `Name ${key}`));
        if (cards) cards.insertAdjacentHTML('beforeend', cardHTML(key, `Name ${key}`));
    });
    return content;
}

function cursor(overrides: Partial<LiveV2Cursor> = {}): LiveV2Cursor {
    return {
        g: 'g-1',
        seq: 1,
        screen: '/pods?ns=dev',
        rev: 'rev-1',
        rv: '10',
        schema: 'schema-1',
        ...overrides,
    };
}

function rawEnvelope(
    delta: Partial<LiveV2Delta>,
    overrides: Partial<Omit<LiveV2DeltaEnvelope, 'delta'>> = {},
): LiveV2DeltaEnvelope {
    return {
        v: 2,
        kind: 'delta',
        g: 'g-1',
        seq: 2,
        screen: '/pods?ns=dev',
        rev: 'rev-2',
        rv: '11',
        schema: 'schema-1',
        ...overrides,
        delta: {
            base: 'rev-1',
            rev: 'rev-2',
            ...delta,
        },
    };
}

function envelope(
    delta: Partial<LiveV2Delta>,
    overrides: Partial<Omit<LiveV2DeltaEnvelope, 'delta'>> = {},
): LiveV2DeltaEnvelope {
    const result = decodeLiveV2Envelope(JSON.stringify(rawEnvelope(delta, overrides)));
    if (!result.ok || result.value.kind !== 'delta') {
        throw new Error(
            `test envelope did not decode: ${result.ok ? result.value.kind : result.error.code}`,
        );
    }
    return result.value;
}

function rawFrame(
    delta: Partial<LiveV2Delta>,
    overrides: Partial<Omit<LiveV2DeltaEnvelope, 'delta'>> = {},
): string {
    return JSON.stringify(rawEnvelope(delta, overrides));
}

function countRegionDelta(value = '1'): Partial<LiveV2Delta> {
    return {
        regions: [
            {
                region: 'count',
                html: `<span class="ro-count" data-ro-live-region="count">${value}</span>`,
            },
        ],
    };
}

function morphInPlace(current: HTMLElement, incoming: HTMLElement): void {
    for (const attribute of Array.from(current.attributes)) current.removeAttribute(attribute.name);
    for (const attribute of Array.from(incoming.attributes)) {
        current.setAttribute(attribute.name, attribute.value);
    }
    current.replaceChildren(...Array.from(incoming.childNodes, (child) => child.cloneNode(true)));
}

function mount(keys: readonly string[], options: FixtureOptions = {}): HTMLElement {
    const content = buildList(keys, options);
    const bulk = document.createElement('div');
    bulk.id = 'ro-bulkbar';
    bulk.setAttribute('inert', '');
    bulk.innerHTML =
        '<span id="ro-bulk-count">0 selected</span><button id="ro-bulk-clear" type="button">clear</button>';
    document.body.append(content, bulk);
    adoptListProjection(content);
    return content;
}

beforeEach(() => {
    document.body.replaceChildren();
    resetListProjection();
    setListProjectionVisibleKeys(null);
    window.roRowState.clear();
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
        this: Element,
    ) {
        const match = (this as HTMLElement).dataset.key?.match(/pod-(\d+)$/u);
        const top = match ? Number(match[1]) * 20 : 0;
        return {
            bottom: top + 20,
            height: 20,
            left: 0,
            right: 100,
            top,
            width: 100,
            x: 0,
            y: top,
            toJSON: () => ({}),
        } as DOMRect;
    });
});

describe('atomic small-list delta application', () => {
    test('does not snapshot the cross-filter selection map for a delta without deletes', () => {
        mount(['dev/a'], { cards: true });
        window.roRowState.setSelected('dev/selected-outside-projection', true);
        const originalEntries = Map.prototype.entries;
        let selectionSnapshots = 0;
        const entriesSpy = vi.spyOn(Map.prototype, 'entries').mockImplementation(function (
            this: Map<unknown, unknown>,
        ) {
            if (this.has('dev/selected-outside-projection')) selectionSnapshots += 1;
            return originalEntries.call(this);
        });

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Changed'),
                        card: cardHTML('dev/a', 'Changed'),
                    },
                ],
            }),
            cursor(),
            { morph: morphInPlace },
        );
        entriesSpy.mockRestore();

        expect(result.ok).toBe(true);
        expect(selectionSnapshots).toBe(0);
    });

    test('updates by identity, changes topology/order/cards/regions, and reconciles state once', () => {
        const content = mount(['dev/a', 'dev/b', 'dev/c'], { cards: true, draft: 'next' });
        const oldA = listProjectionRowByKey('dev/a') as HTMLElement;
        const oldCardA = listProjectionCardByKey('dev/a') as HTMLElement;
        const count = content.querySelector('[data-ro-live-region="count"]') as HTMLElement;
        const phase = content.querySelector('[data-ro-live-region="phase"]') as HTMLElement;
        const found = content.querySelector('[data-ro-live-region="found"]') as HTMLElement;
        const revision = listProjectionRevision();
        window.roRowState.setSelected('dev/a', true);
        window.roRowState.setFocus('dev/a');

        const result = applyLiveV2Delta(
            envelope({
                remove: [
                    { key: 'dev/b', cause: 'delete' },
                    { key: 'dev/c', cause: 'project' },
                ],
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Next API', 'Running'),
                        card: cardHTML('dev/a', 'Next API'),
                    },
                    {
                        key: 'dev/d',
                        row: rowHTML('dev/d', 'New Worker'),
                        card: cardHTML('dev/d', 'New Worker'),
                    },
                ],
                order: ['dev/d', 'dev/a'],
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span> ',
                    },
                    {
                        region: 'phase',
                        html: '<div class="ro-phase-strip" data-ro-live-region="phase"><b>healthy</b></div>',
                    },
                    {
                        region: 'found',
                        html: '<span class="ro-foundline" data-ro-live-region="found">2 found</span>',
                    },
                ],
            }),
            cursor(),
            { morph: morphInPlace },
        );

        expect(result).toMatchObject({
            ok: true,
            cursor: { seq: 2, rev: 'rev-2', rv: '11' },
            summary: {
                inserted: 1,
                updated: 1,
                deleted: 1,
                projected: 1,
                reordered: true,
                regions: ['count', 'phase', 'found'],
            },
        });
        if (!result.ok) return;
        expect(listProjectionRowByKey('dev/a')).toBe(oldA);
        expect(listProjectionCardByKey('dev/a')).toBe(oldCardA);
        expect(listProjectionOrder()).toStrictEqual(['dev/d', 'dev/a']);
        expect(listProjectionRows().map((row) => row.dataset.key)).toStrictEqual([
            'dev/d',
            'dev/a',
        ]);
        expect(content.querySelectorAll('tbody > tr[data-key]')).toHaveLength(2);
        expect(content.querySelectorAll('.ro-cardlist > .ro-pcard')).toHaveLength(2);
        expect(oldA).toHaveTextContent('Next API');
        expect(oldA).toHaveClass('is-selected', 'kfocus');
        expect(content.querySelector('.ro-table-wrap')).toHaveAttribute(
            'aria-activedescendant',
            oldA.id,
        );
        expect(listProjectionCardByKey('dev/a')).not.toHaveClass('ro-row-filtered');
        expect(listProjectionCardByKey('dev/d')).toHaveClass('ro-row-filtered');
        expect(listProjectionRowModel().rows).toStrictEqual([
            { key: 'dev/d', name: 'New Worker', cells: ['New Worker', 'Ready'] },
            { key: 'dev/a', name: 'Next API', cells: ['Next API', 'Running'] },
        ]);
        expect(Array.from(listProjectionRowModel().visibleKeys || [])).toStrictEqual(['dev/a']);
        expect(count).toHaveTextContent('2');
        expect(phase).not.toHaveAttribute('hidden');
        expect(found).toHaveTextContent('2 found');
        expect(content.querySelector('#ro-live-status')).toHaveTextContent(
            'Live update: 4 rows, order changed, 3 regions',
        );
        expect(listProjectionRevision()).toBe(revision + 1);
    });

    test('preserves an active draft and focused input while applying a row-scoped morph', () => {
        const content = mount(['dev/a'], { cards: true, draft: 'api' });
        const input = content.querySelector('#ro-filter-input') as HTMLInputElement;
        input.focus();
        input.setSelectionRange(1, 2);
        const row = listProjectionRowByKey('dev/a');

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'API server'),
                        card: cardHTML('dev/a', 'API server'),
                    },
                ],
            }),
            cursor(),
            { morph: morphInPlace },
        );

        expect(result.ok).toBe(true);
        expect(listProjectionRowByKey('dev/a')).toBe(row);
        expect(document.activeElement).toBe(input);
        expect(input.value).toBe('api');
        expect(input.selectionStart).toBe(1);
        expect(input.selectionEnd).toBe(2);
    });

    test('uses row-scoped classic Idiomorph with CSP-clean capacity styles', () => {
        mount(['dev/a'], { cards: true });
        const morph = vi.fn((current: HTMLElement, incoming: HTMLElement, _config?: unknown) =>
            morphInPlace(current, incoming),
        );
        vi.stubGlobal('Idiomorph', { morph });
        const row = rowHTML('dev/a', 'Capacity').replace(
            '<td>Ready</td>',
            '<td><span class="cap"><span class="cap-bar"><i style="width:42%"></i></span></span></td>',
        );
        const card = cardHTML('dev/a', 'Capacity').replace(
            '</div>',
            '<span class="cap-bar"><i style="width:42%"></i></span></div>',
        );

        const result = applyLiveV2Delta(
            envelope({ upsert: [{ key: 'dev/a', row, card }] }),
            cursor(),
        );

        expect(result.ok).toBe(true);
        expect(morph).toHaveBeenCalledTimes(2);
        expect(morph.mock.calls[0]?.[2]).toStrictEqual({
            morphStyle: 'outerHTML',
            ignoreActiveValue: true,
        });
        expect(listProjectionRowByKey('dev/a')?.querySelector('i')).toHaveAttribute(
            'style',
            'width:42%',
        );
    });

    test('accepts real Idiomorph cell-flash residue after canonical content lands', () => {
        mount(['dev/a'], { windowed: true });
        const row = listProjectionRowByKey('dev/a') as HTMLElement;

        const result = applyLiveV2Delta(
            envelope({ upsert: [{ key: 'dev/a', row: rowHTML('dev/a', 'Changed') }] }),
            cursor(),
            {
                morph(current, incoming) {
                    morphInPlace(current, incoming);
                    current.querySelectorAll('td').item(1)?.classList.add('ro-cell-changed');
                },
            },
        );

        expect(result.ok).toBe(true);
        expect(listProjectionRowByKey('dev/a')).toBe(row);
        expect(row).toHaveTextContent('Changed');
        expect(row.querySelectorAll('td').item(1)).toHaveClass('ro-cell-changed');
    });

    test('applies a reorder-only delta and announces the order change', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        const oldA = listProjectionRowByKey('dev/a');
        const oldB = listProjectionRowByKey('dev/b');

        const result = applyLiveV2Delta(envelope({ order: ['dev/b', 'dev/a'] }), cursor());

        expect(result).toMatchObject({
            ok: true,
            summary: {
                inserted: 0,
                updated: 0,
                deleted: 0,
                projected: 0,
                reordered: true,
                regions: [],
            },
        });
        expect(listProjectionRows()).toStrictEqual([oldB, oldA]);
        expect(content.querySelector('#ro-live-status')).toHaveTextContent(
            'Live update: order changed',
        );
    });

    test('reorders while updating an existing connected row by identity', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        const oldA = listProjectionRowByKey('dev/a');
        const oldCardA = listProjectionCardByKey('dev/a');

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'A updated'),
                        card: cardHTML('dev/a', 'A updated'),
                    },
                ],
                order: ['dev/b', 'dev/a'],
            }),
            cursor(),
            { morph: morphInPlace },
        );

        expect(result).toMatchObject({ ok: true, summary: { updated: 1, reordered: true } });
        expect(listProjectionRowByKey('dev/a')).toBe(oldA);
        expect(listProjectionCardByKey('dev/a')).toBe(oldCardA);
        expect(oldA).toHaveTextContent('A updated');
        expect(Array.from(content.querySelectorAll('tbody > tr'))).toStrictEqual([
            listProjectionRowByKey('dev/b'),
            oldA,
        ]);
    });
});

describe('windowed delta application', () => {
    test('updates a 600-row offscreen canonical node without inserting it into the DOM', () => {
        const keys = Array.from({ length: 600 }, (_, index) => `dev/pod-${index}`);
        const content = mount(keys, { windowed: true });
        virtualizeInit();
        const oldOffscreen = listProjectionRowByKey('dev/pod-599') as HTMLElement;
        expect(oldOffscreen.isConnected).toBe(false);
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const childCount = tbody.children.length;
        const revision = listProjectionRevision();
        const morph = vi.fn(morphInPlace);

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/pod-599',
                        row: rowHTML('dev/pod-599', 'Offscreen changed', 'Pending'),
                    },
                ],
            }),
            cursor(),
            { morph },
        );

        expect(result.ok).toBe(true);
        expect(morph).toHaveBeenCalledTimes(1);
        const changed = listProjectionRowByKey('dev/pod-599') as HTMLElement;
        expect(changed).toBe(oldOffscreen);
        expect(changed).toHaveTextContent('Offscreen changed');
        expect(changed.isConnected).toBe(false);
        expect(tbody.querySelector('[data-key="dev/pod-599"]')).toBeNull();
        expect(tbody.children.length).toBe(childCount);
        expect(listProjectionRowModel().rows).toHaveLength(600);
        expect(listProjectionRowModel().rows[599]).toMatchObject({
            key: 'dev/pod-599',
            name: 'Offscreen changed',
        });
        expect(listProjectionRevision()).toBe(revision + 1);
    });

    test('stages one existing-row upsert independently of a 600-row projection', () => {
        const keys = Array.from({ length: 600 }, (_, index) => `dev/pod-${index}`);
        mount(keys, { windowed: true });
        virtualizeInit();
        const cloneSpy = vi.spyOn(Node.prototype, 'cloneNode');
        const querySpy = vi.spyOn(Element.prototype, 'querySelectorAll');
        let clonesBeforeReconcile = -1;
        let fullTraversalsBeforeReconcile = -1;

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/pod-599',
                        row: rowHTML('dev/pod-599', 'One changed row'),
                    },
                ],
            }),
            cursor(),
            {
                morph: morphInPlace,
                beforeReconcile() {
                    clonesBeforeReconcile = cloneSpy.mock.calls.length;
                    const fullSelectors = new Set([
                        ':scope > tr[data-key]',
                        '.ro-cardlist > .ro-pcard',
                        'thead th',
                    ]);
                    fullTraversalsBeforeReconcile = querySpy.mock.calls.filter(([selector]) =>
                        fullSelectors.has(selector),
                    ).length;
                },
            },
        );

        cloneSpy.mockRestore();
        querySpy.mockRestore();
        expect(result.ok).toBe(true);
        // Two child clones land the test morph; two root clones perform the
        // normalized postcondition. Neither count depends on projection size.
        expect(clonesBeforeReconcile).toBe(4);
        expect(fullTraversalsBeforeReconcile).toBe(0);
    });

    test('restores exact spacer geometry and window nodes when reconcile rolls back', () => {
        const keys = Array.from({ length: 600 }, (_, index) => `dev/pod-${index}`);
        const content = mount(keys, { draft: 'Name', windowed: true });
        applyLiveNameFilter();
        virtualizeInit();
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const beforeChildren = Array.from(tbody.childNodes);
        const beforeOuterHTML = Array.from(tbody.children, (child) => child.outerHTML);
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const beforeVisible = listProjectionRowModel().visibleKeys;
        const beforeOffscreen = listProjectionRowByKey('dev/pod-599');
        const revision = listProjectionRevision();
        let duringOuterHTML: string[] = [];

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/pod-599',
                        row: rowHTML('dev/pod-599', 'No longer matched', 'Pending'),
                    },
                ],
            }),
            cursor(),
            {
                morph: morphInPlace,
                afterReconcile() {
                    duringOuterHTML = Array.from(tbody.children, (child) => child.outerHTML);
                    throw new Error('induced window reconcile failure');
                },
            },
        );

        expect(result).toMatchObject({
            ok: false,
            error: { code: 'reconcile-failed', fatal: false },
        });
        expect(duringOuterHTML).not.toStrictEqual(beforeOuterHTML);
        expect(Array.from(tbody.childNodes)).toStrictEqual(beforeChildren);
        expect(Array.from(tbody.children, (child) => child.outerHTML)).toStrictEqual(
            beforeOuterHTML,
        );
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowByKey('dev/pod-599')).toBe(beforeOffscreen);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRowModel().visibleKeys).toBe(beforeVisible);
        expect(listProjectionRevision()).toBe(revision);
        expect(result).not.toHaveProperty('cursor');
    });

    test('applies windowed insert/delete/project and full reorder through one rewindow', () => {
        const keys = Array.from({ length: 600 }, (_, index) => `dev/pod-${index}`);
        const content = mount(keys, { windowed: true });
        virtualizeInit();
        const deleted = listProjectionRowByKey('dev/pod-0') as HTMLElement;
        const projected = listProjectionRowByKey('dev/pod-599') as HTMLElement;
        const finalOrder = [
            'dev/new',
            ...keys.filter((key) => key !== 'dev/pod-0' && key !== 'dev/pod-599'),
        ];

        const result = applyLiveV2Delta(
            envelope({
                remove: [
                    { key: 'dev/pod-0', cause: 'delete' },
                    { key: 'dev/pod-599', cause: 'project' },
                ],
                upsert: [{ key: 'dev/new', row: rowHTML('dev/new', 'Newest') }],
                order: finalOrder,
            }),
            cursor(),
        );

        expect(result).toMatchObject({
            ok: true,
            summary: { inserted: 1, deleted: 1, projected: 1, reordered: true },
        });
        expect(listProjectionOrder()).toStrictEqual(finalOrder);
        expect(listProjectionRows()).toHaveLength(599);
        expect(listProjectionRowModel().rows).toHaveLength(599);
        expect(listProjectionRowByKey('dev/new')).toHaveTextContent('Newest');
        expect(content.querySelector('tbody > tr[data-key="dev/new"]')).toBe(
            listProjectionRowByKey('dev/new'),
        );
        expect(deleted.isConnected).toBe(false);
        expect(projected.isConnected).toBe(false);
    });
});

describe('delete versus projection row state', () => {
    test('prunes selection/focus only for delete while preserving projected selection', () => {
        const content = mount(['dev/a', 'dev/b', 'dev/c'], { cards: true });
        window.roRowState.setSelected('dev/a', true);
        window.roRowState.setSelected('dev/b', true);
        window.roRowState.setFocus('dev/a');

        const result = applyLiveV2Delta(
            envelope({
                remove: [
                    { key: 'dev/a', cause: 'delete' },
                    { key: 'dev/b', cause: 'project' },
                ],
                order: ['dev/c'],
            }),
            cursor(),
        );

        expect(result.ok).toBe(true);
        expect(window.roRowState.selectedKeys()).toStrictEqual(['dev/b']);
        expect(window.roRowState.focusedKey()).toBeNull();
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('1 selected');
        expect(document.getElementById('ro-bulkbar')).toHaveClass('is-open');
        expect(document.getElementById('ro-bulkbar')).not.toHaveAttribute('inert');
        expect(content.querySelector('.ro-table-wrap')).not.toHaveAttribute(
            'aria-activedescendant',
        );
    });

    test('preserves projected focus in the store while clearing its absent aria target', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        window.roRowState.setSelected('dev/a', true);
        window.roRowState.setFocus('dev/a');

        const result = applyLiveV2Delta(
            envelope({
                remove: [{ key: 'dev/a', cause: 'project' }],
                order: ['dev/b'],
            }),
            cursor(),
        );

        expect(result.ok).toBe(true);
        expect(window.roRowState.selectedKeys()).toStrictEqual(['dev/a']);
        expect(window.roRowState.focusedKey()).toBe('dev/a');
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('1 selected');
        expect(content.querySelector('.ro-table-wrap')).not.toHaveAttribute(
            'aria-activedescendant',
        );
    });

    test('restores exact selection, bulk descendants, and aria after reconcile rollback', () => {
        const content = mount(['dev/a', 'dev/b', 'dev/c'], { cards: true });
        window.roRowState.setSelected('dev/a', true);
        window.roRowState.setSelected('dev/b', true);
        window.roRowState.setFocus('dev/a');
        const bulk = document.getElementById('ro-bulkbar') as HTMLElement;
        const count = document.getElementById('ro-bulk-count') as HTMLElement;
        const countText = count.firstChild;
        const clear = document.getElementById('ro-bulk-clear') as HTMLButtonElement;
        const wrap = content.querySelector('.ro-table-wrap') as HTMLElement;
        const oldA = listProjectionRowByKey('dev/a') as HTMLElement;
        const oldB = listProjectionRowByKey('dev/b') as HTMLElement;
        const beforeBulkHTML = bulk.outerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const revision = listProjectionRevision();
        let clicks = 0;
        clear.addEventListener('click', () => {
            clicks += 1;
        });
        let duringState: { selected: string[]; focus: string | null; count: string } | null = null;

        const result = applyLiveV2Delta(
            envelope({
                remove: [
                    { key: 'dev/a', cause: 'delete' },
                    { key: 'dev/b', cause: 'project' },
                ],
                order: ['dev/c'],
            }),
            cursor(),
            {
                afterReconcile() {
                    duringState = {
                        selected: window.roRowState.selectedKeys(),
                        focus: window.roRowState.focusedKey(),
                        count: count.textContent || '',
                    };
                    throw new Error('induced row-state reconcile failure');
                },
            },
        );

        expect(duringState).toStrictEqual({
            selected: ['dev/b'],
            focus: null,
            count: '1 selected',
        });
        expect(result).toMatchObject({
            ok: false,
            error: { code: 'reconcile-failed', fatal: false },
        });
        expect(result).not.toHaveProperty('cursor');
        expect(window.roRowState.selectedKeys()).toStrictEqual(['dev/a', 'dev/b']);
        expect(window.roRowState.focusedKey()).toBe('dev/a');
        expect(bulk.outerHTML).toBe(beforeBulkHTML);
        expect(document.getElementById('ro-bulk-count')).toBe(count);
        expect(count.firstChild).toBe(countText);
        expect(document.getElementById('ro-bulk-clear')).toBe(clear);
        clear.click();
        expect(clicks).toBe(1);
        expect(wrap).toHaveAttribute('aria-activedescendant', oldA.id);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowByKey('dev/a')).toBe(oldA);
        expect(listProjectionRowByKey('dev/b')).toBe(oldB);
        expect(oldA).toHaveClass('is-selected', 'kfocus');
        expect(oldB).toHaveClass('is-selected');
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRevision()).toBe(revision);
    });
});

describe('fail-closed validation and rollback', () => {
    test.each([
        [
            'generation',
            envelope(countRegionDelta(), { g: 'other' }),
            'generation-mismatch',
            'delta generation does not match the cursor',
        ],
        [
            'screen',
            envelope(countRegionDelta(), { screen: '/other' }),
            'screen-mismatch',
            'delta screen does not match the cursor',
        ],
        [
            'sequence',
            envelope(countRegionDelta(), { seq: 3 }),
            'sequence-gap',
            'delta sequence is not the cursor successor',
        ],
        [
            'base revision',
            envelope({ ...countRegionDelta(), base: 'other' }),
            'base-mismatch',
            'delta base does not match the cursor revision',
        ],
        [
            'schema',
            envelope(countRegionDelta(), { schema: 'other' }),
            'schema-mismatch',
            'delta schema does not match the cursor schema',
        ],
    ] as const)('returns the exact %s cursor diagnostic', (_name, frame, code, message) => {
        expect(applyLiveV2Delta(frame, cursor())).toStrictEqual({
            ok: false,
            error: { code, message, fatal: false },
        });
    });

    test.each([
        {
            name: 'inherits the previous resource version',
            previous: '10',
            expected: '10',
            hasResourceVersion: true,
        },
        {
            name: 'keeps resource version absent',
            previous: undefined,
            expected: undefined,
            hasResourceVersion: false,
        },
    ])('$name when a delta omits rv', ({ previous, expected, hasResourceVersion }) => {
        mount(['dev/a'], { cards: true });
        const frame = envelope(countRegionDelta(), { rv: undefined });
        const result = applyLiveV2Delta(frame, cursor({ rv: previous }), { morph: morphInPlace });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        expect(result.cursor.rv).toBe(expected);
        expect(Object.hasOwn(result.cursor, 'rv')).toBe(hasResourceVersion);
    });

    test.each([
        {
            name: 'generation mismatch',
            frame: () => envelope(countRegionDelta(), { g: 'other' }),
            code: 'generation-mismatch',
        },
        {
            name: 'sequence gap',
            frame: () => envelope(countRegionDelta(), { seq: 4 }),
            code: 'sequence-gap',
        },
        {
            name: 'screen mismatch',
            frame: () => envelope(countRegionDelta(), { screen: '/deployments' }),
            code: 'screen-mismatch',
        },
        {
            name: 'schema mismatch',
            frame: () => envelope(countRegionDelta(), { schema: 'schema-2' }),
            code: 'schema-mismatch',
        },
        {
            name: 'base mismatch',
            frame: () => envelope({ ...countRegionDelta(), base: 'other' }),
            code: 'base-mismatch',
        },
        {
            name: 'envelope revision disagreement',
            frame: () =>
                rawFrame(
                    {
                        upsert: [
                            {
                                key: 'dev/a',
                                row: rowHTML('dev/a', 'Changed'),
                                card: cardHTML('dev/a', 'Changed'),
                            },
                        ],
                    },
                    { rev: 'different-revision' },
                ),
            code: 'invalid-field',
        },
        {
            name: 'semantic no-op',
            frame: () => rawFrame({}),
            code: 'no-op',
        },
        {
            name: 'only empty operation arrays',
            frame: () => rawFrame({ remove: [], upsert: [], order: [], regions: [] }),
            code: 'no-op',
        },
        {
            name: 'unknown remove key',
            frame: () =>
                envelope({
                    remove: [{ key: 'dev/missing', cause: 'delete' }],
                    order: ['dev/a', 'dev/b'],
                }),
            code: 'projection-mismatch',
        },
        {
            name: 'topology without order',
            frame: () => envelope({ remove: [{ key: 'dev/b', cause: 'delete' }] }),
            code: 'projection-mismatch',
        },
        {
            name: 'non-exact order',
            frame: () =>
                envelope({
                    remove: [{ key: 'dev/b', cause: 'delete' }],
                    order: ['dev/a', 'dev/missing'],
                }),
            code: 'projection-mismatch',
        },
        {
            name: 'multiple row roots',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: `${rowHTML('dev/a', 'A')}${rowHTML('dev/a', 'B')}`,
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'wrong fragment key',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/x', 'X'),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'non-canonical row id',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A').replace(
                                'id="row-dev/a"',
                                'id="row-not-canonical"',
                            ),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'row root aliases a live region',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A').replace(
                                '<tr ',
                                '<tr data-ro-live-region="count" ',
                            ),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'row descendant collides with live status id',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A').replace(
                                '<td>Ready</td>',
                                '<td><span id="ro-live-status">Ready</span></td>',
                            ),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'row descendant introduces a live region',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A').replace(
                                '<td>Ready</td>',
                                '<td><span data-ro-live-region="count">Ready</span></td>',
                            ),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'missing card',
            frame: () => envelope({ upsert: [{ key: 'dev/a', row: rowHTML('dev/a', 'A') }] }),
            code: 'fragment-invalid',
        },
        {
            name: 'non-canonical card tag',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A'),
                            card: '<article class="ro-pcard" data-key="dev/a">A</article>',
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'card root collides with live status id',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A'),
                            card: cardHTML('dev/a', 'A').replace(
                                '<div ',
                                '<div id="ro-live-status" ',
                            ),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'card root aliases a live region',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A'),
                            card: cardHTML('dev/a', 'A').replace(
                                '<div ',
                                '<div data-ro-live-region="count" ',
                            ),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'card descendant introduces an id',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A'),
                            card: cardHTML('dev/a', 'A').replace(
                                '</div>',
                                '<span id="ro-live-status"></span></div>',
                            ),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'unsafe fragment',
            frame: () =>
                envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', '<img src=x onerror=alert(1)>'),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'malformed region root',
            frame: () =>
                envelope({
                    regions: [
                        {
                            region: 'count',
                            html: '<div data-ro-live-region="count">2</div>',
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'region root introduces an id',
            frame: () =>
                envelope({
                    regions: [
                        {
                            region: 'count',
                            html: '<span id="ro-live-status" class="ro-count" data-ro-live-region="count">2</span>',
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'region descendant introduces an id',
            frame: () =>
                envelope({
                    regions: [
                        {
                            region: 'phase',
                            html: '<div class="ro-phase-strip" data-ro-live-region="phase"><span id="ro-live-status">bad</span></div>',
                        },
                    ],
                }),
            code: 'fragment-invalid',
        },
        {
            name: 'inserted row id collides elsewhere in the document',
            frame: () => {
                document.body.insertAdjacentHTML('beforeend', '<aside id="row-dev/new"></aside>');
                return envelope({
                    upsert: [
                        {
                            key: 'dev/new',
                            row: rowHTML('dev/new', 'New'),
                            card: cardHTML('dev/new', 'New'),
                        },
                    ],
                    order: ['dev/a', 'dev/b', 'dev/new'],
                });
            },
            code: 'fragment-invalid',
        },
        {
            name: 'updated row id has a global duplicate',
            frame: () => {
                document.body.insertAdjacentHTML('beforeend', '<aside id="row-dev/a"></aside>');
                return envelope({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: rowHTML('dev/a', 'A changed'),
                            card: cardHTML('dev/a', 'A changed'),
                        },
                    ],
                });
            },
            code: 'fragment-invalid',
        },
        {
            name: 'untouched row id has a global duplicate during reorder',
            frame: () => {
                document.body.insertAdjacentHTML('beforeend', '<aside id="row-dev/a"></aside>');
                return envelope({ order: ['dev/b', 'dev/a'] });
            },
            code: 'fragment-invalid',
        },
    ])('leaves DOM/model/cursor identity unchanged for $name', ({ frame, code }) => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeA = listProjectionRowByKey('dev/a');
        const beforeModelRows = listProjectionRowModel().rows;
        const visible = new Set(['dev/a']);
        setListProjectionVisibleKeys(visible);
        const revision = listProjectionRevision();

        const result = applyLiveV2Delta(frame(), cursor(), { morph: morphInPlace });

        expect(result).toMatchObject({ ok: false, error: { code, fatal: false } });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowByKey('dev/a')).toBe(beforeA);
        expect(listProjectionRowModel().rows).toBe(beforeModelRows);
        expect(listProjectionRowModel().visibleKeys).toBe(visible);
        expect(listProjectionRevision()).toBe(revision);
        expect(result).not.toHaveProperty('cursor');
    });

    test.each([
        {
            name: 'untrusted structural lookalike',
            input: () => rawEnvelope(countRegionDelta()),
            code: 'invalid-frame',
        },
        {
            name: 'wrong protocol version',
            input: () => JSON.stringify({ ...rawEnvelope(countRegionDelta()), v: 3 }),
            code: 'unsupported-version',
        },
        {
            name: 'missing delta object',
            input: () => {
                const value = rawEnvelope(countRegionDelta()) as unknown as Record<string, unknown>;
                delete value.delta;
                return JSON.stringify(value);
            },
            code: 'invalid-field',
        },
        {
            name: 'object instead of remove array',
            input: () => {
                const value = rawEnvelope(countRegionDelta());
                (value.delta as unknown as Record<string, unknown>).remove = {};
                return JSON.stringify(value);
            },
            code: 'invalid-field',
        },
        {
            name: 'null operation collection',
            input: () => {
                const value = rawEnvelope(countRegionDelta());
                (value.delta as unknown as Record<string, unknown>).upsert = null;
                return JSON.stringify(value);
            },
            code: 'invalid-field',
        },
        {
            name: 'unknown removal cause',
            input: () => {
                const value = rawEnvelope(countRegionDelta());
                (value.delta as unknown as Record<string, unknown>).remove = [
                    { key: 'dev/a', cause: 'evaporate' },
                ];
                return JSON.stringify(value);
            },
            code: 'invalid-field',
        },
        {
            name: 'oversized row fragment',
            input: () =>
                rawFrame({
                    upsert: [
                        {
                            key: 'dev/a',
                            row: 'x'.repeat(128 * 1024 + 1),
                            card: cardHTML('dev/a', 'A'),
                        },
                    ],
                }),
            code: 'limit-exceeded',
        },
        {
            name: 'null apply input',
            input: () => null,
            code: 'invalid-frame',
        },
        {
            name: 'cyclic apply object',
            input: () => {
                const value: Record<string, unknown> = {};
                value.self = value;
                return value;
            },
            code: 'invalid-frame',
        },
    ])('runtime-total apply rejects $name before mutation', ({ input, code }) => {
        const content = mount(['dev/a'], { cards: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const revision = listProjectionRevision();

        const result = applyLiveV2Delta(input(), cursor(), { morph: morphInPlace });

        expect(result).toMatchObject({ ok: false, error: { code, fatal: false } });
        expect(result).not.toHaveProperty('cursor');
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRevision()).toBe(revision);
    });

    test('rejects base equal to revision without publishing any state or cursor', () => {
        const content = mount(['dev/a'], { cards: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const revision = listProjectionRevision();

        const result = applyLiveV2Delta(
            rawFrame({
                base: 'rev-2',
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">1</span>',
                    },
                ],
            }),
            cursor({ rev: 'rev-2' }),
        );

        expect(result).toMatchObject({ ok: false, error: { code: 'no-op', fatal: false } });
        expect(result).not.toHaveProperty('cursor');
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRevision()).toBe(revision);
    });

    test('rejects the remove-last empty boundary even with an exact empty order', () => {
        const content = mount(['dev/a'], { cards: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const revision = listProjectionRevision();

        const result = applyLiveV2Delta(
            envelope({
                remove: [{ key: 'dev/a', cause: 'delete' }],
                order: [],
            }),
            cursor(),
        );

        expect(result).toMatchObject({
            ok: false,
            error: { code: 'projection-mismatch', fatal: false },
        });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRevision()).toBe(revision);
        expect(window.roRowState.selectedKeys()).toStrictEqual([]);
        expect(result).not.toHaveProperty('cursor');
    });

    test.each([
        {
            name: 'wide node fanout',
            row: rowHTML('dev/a', 'Wide').replace(
                '<td>Ready</td>',
                `<td>${'<!---->'.repeat(4_097)}</td>`,
            ),
        },
        {
            name: 'deep descendant chain',
            row: rowHTML('dev/a', 'Deep').replace(
                '<td>Ready</td>',
                `<td>${'<span>'.repeat(65)}deep${'</span>'.repeat(65)}</td>`,
            ),
        },
        {
            name: 'attribute fanout',
            row: rowHTML('dev/a', 'Attributes').replace(
                '<td>Ready</td>',
                `<td><span ${Array.from({ length: 8_193 }, (_, index) => `x-${index}=""`).join(
                    ' ',
                )}>wide</span></td>`,
            ),
        },
    ])('rejects bounded parsed-DOM amplification: $name', ({ row }) => {
        const content = mount(['dev/a'], { windowed: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;

        const result = applyLiveV2Delta(envelope({ upsert: [{ key: 'dev/a', row }] }), cursor(), {
            morph: morphInPlace,
        });

        expect(result).toMatchObject({
            ok: false,
            error: { code: 'fragment-invalid', fatal: false },
        });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(result).not.toHaveProperty('cursor');
    });

    test('enforces individual and aggregate fragment byte caps before parsing', () => {
        const content = mount(['dev/a'], { cards: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const hugeRow = rowHTML('dev/a', 'Huge').replace(
            '<td>Ready</td>',
            `<td>${'x'.repeat(128 * 1024)}</td>`,
        );

        const individual = applyLiveV2Delta(
            rawFrame({
                upsert: [
                    {
                        key: 'dev/a',
                        row: hugeRow,
                        card: cardHTML('dev/a', 'Huge'),
                    },
                ],
            }),
            cursor(),
            { morph: morphInPlace },
        );
        expect(individual).toMatchObject({
            ok: false,
            error: { code: 'limit-exceeded', fatal: false },
        });

        const payload = 'y'.repeat(90 * 1024);
        const aggregate = applyLiveV2Delta(
            rawFrame({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Aggregate').replace(
                            '<td>Ready</td>',
                            `<td>${payload}</td>`,
                        ),
                        card: cardHTML('dev/a', 'Aggregate').replace(
                            '</div>',
                            `<span>${payload}</span></div>`,
                        ),
                    },
                ],
                regions: [
                    {
                        region: 'count',
                        html: `<span class="ro-count" data-ro-live-region="count">${payload}</span>`,
                    },
                ],
            }),
            cursor(),
            { morph: morphInPlace },
        );
        expect(aggregate).toMatchObject({
            ok: false,
            error: { code: 'limit-exceeded', fatal: false },
        });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(individual).not.toHaveProperty('cursor');
        expect(aggregate).not.toHaveProperty('cursor');
    });

    test('rejects a no-op or explicitly cancelled morph and restores exact roots', () => {
        const content = mount(['dev/a'], { cards: true });
        const beforeHTML = content.innerHTML;
        const row = listProjectionRowByKey('dev/a');
        const card = listProjectionCardByKey('dev/a');

        const noOp = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Changed'),
                        card: cardHTML('dev/a', 'Changed'),
                    },
                ],
            }),
            cursor(),
            { morph: () => undefined },
        );
        expect(noOp).toMatchObject({ ok: false, error: { code: 'morph-failed' } });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRowByKey('dev/a')).toBe(row);
        expect(listProjectionCardByKey('dev/a')).toBe(card);

        const cancelled = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Changed'),
                        card: cardHTML('dev/a', 'Changed'),
                    },
                ],
            }),
            cursor(),
            {
                morph(current, incoming) {
                    morphInPlace(current, incoming);
                    return false;
                },
            },
        );
        expect(cancelled).toMatchObject({ ok: false, error: { code: 'morph-failed' } });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRowByKey('dev/a')).toBe(row);
        expect(listProjectionCardByKey('dev/a')).toBe(card);
    });

    test('rolls back partial row morphs with exact root identities and parent order', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const oldA = listProjectionRowByKey('dev/a') as HTMLElement;
        const oldB = listProjectionRowByKey('dev/b') as HTMLElement;
        const beforeModel = listProjectionRowModel().rows;
        const revision = listProjectionRevision();
        let calls = 0;

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Changed A'),
                        card: cardHTML('dev/a', 'Changed A'),
                    },
                    {
                        key: 'dev/b',
                        row: rowHTML('dev/b', 'Changed B'),
                        card: cardHTML('dev/b', 'Changed B'),
                    },
                ],
            }),
            cursor(),
            {
                morph(current, incoming) {
                    calls += 1;
                    if (calls === 2) throw new Error('induced morph failure');
                    morphInPlace(current, incoming);
                },
            },
        );

        expect(result).toMatchObject({ ok: false, error: { code: 'morph-failed', fatal: false } });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowByKey('dev/a')).toBe(oldA);
        expect(listProjectionRowByKey('dev/b')).toBe(oldB);
        expect(Array.from(content.querySelectorAll('tbody > tr'))).toStrictEqual([oldA, oldB]);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRevision()).toBe(revision);
    });

    test('rollback restores exact nested node identities and their listeners', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        const row = listProjectionRowByKey('dev/a') as HTMLElement;
        const rowB = listProjectionRowByKey('dev/b') as HTMLElement;
        const link = row.querySelector('a') as HTMLAnchorElement;
        const text = link.firstChild;
        const beforeHTML = content.innerHTML;
        const beforeModel = listProjectionRowModel().rows;
        const revision = listProjectionRevision();
        let clicks = 0;
        link.addEventListener('click', (event) => {
            event.preventDefault();
            clicks += 1;
        });

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Changed nested content'),
                        card: cardHTML('dev/a', 'Changed nested content'),
                    },
                ],
            }),
            cursor(),
            {
                morph: morphInPlace,
                afterReconcile() {
                    content.querySelector('tbody')?.append(row);
                    throw new Error('induced identity rollback');
                },
            },
        );

        expect(result).toMatchObject({
            ok: false,
            error: { code: 'reconcile-failed', fatal: false },
        });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRowByKey('dev/a')).toBe(row);
        expect(Array.from(content.querySelectorAll('tbody > tr'))).toStrictEqual([row, rowB]);
        expect(row.querySelector('a')).toBe(link);
        expect(link.firstChild).toBe(text);
        link.click();
        expect(clicks).toBe(1);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRevision()).toBe(revision);
        expect(result).not.toHaveProperty('cursor');
    });

    test('rolls back filter classes, selection aria, model and DOM when reconcile throws', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true, draft: 'old' });
        applyLiveNameFilter();
        window.roRowState.setSelected('dev/a', true);
        window.roRowState.setFocus('dev/a');
        const input = content.querySelector('#ro-filter-input') as HTMLInputElement;
        const beforeHTML = content.innerHTML;
        const beforeRows = listProjectionRows();
        const beforeModel = listProjectionRowModel().rows;
        const beforeVisible = listProjectionRowModel().visibleKeys;
        const oldA = listProjectionRowByKey('dev/a');
        const revision = listProjectionRevision();

        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'old changed'),
                        card: cardHTML('dev/a', 'old changed'),
                    },
                ],
            }),
            cursor(),
            {
                morph: morphInPlace,
                afterReconcile() {
                    throw new Error('induced reconcile failure');
                },
            },
        );

        expect(result).toMatchObject({
            ok: false,
            error: { code: 'reconcile-failed', fatal: false },
        });
        expect(content.innerHTML).toBe(beforeHTML);
        expect(listProjectionRows()).toBe(beforeRows);
        expect(listProjectionRowByKey('dev/a')).toBe(oldA);
        expect(listProjectionRowModel().rows).toBe(beforeModel);
        expect(listProjectionRowModel().visibleKeys).toBe(beforeVisible);
        expect(input.value).toBe('old');
        expect(listProjectionRevision()).toBe(revision);

        const retry = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'old changed'),
                        card: cardHTML('dev/a', 'old changed'),
                    },
                ],
            }),
            cursor(),
            { morph: morphInPlace },
        );
        expect(retry.ok).toBe(true);
        expect(Array.from(listProjectionRowModel().visibleKeys || [])).toStrictEqual(['dev/a']);
        expect(listProjectionRowByKey('dev/b')).toHaveClass('ro-row-filtered');
    });

    test('returns fatal rollback-failed when an orchestrator removes the old mount', () => {
        const content = mount(['dev/a'], { cards: true });
        const result = applyLiveV2Delta(
            envelope({
                upsert: [
                    {
                        key: 'dev/a',
                        row: rowHTML('dev/a', 'Changed'),
                        card: cardHTML('dev/a', 'Changed'),
                    },
                ],
            }),
            cursor(),
            {
                morph: morphInPlace,
                afterReconcile() {
                    content.remove();
                    throw new Error('mount removed');
                },
            },
        );

        expect(result).toStrictEqual({
            ok: false,
            error: {
                code: 'rollback-failed',
                message: 'Live delta rollback could not restore the original mounts',
                fatal: true,
            },
        });
        expect(result).not.toHaveProperty('cursor');
    });
});
