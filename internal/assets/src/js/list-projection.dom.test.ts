// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';
import type { ListProjectionDeltaPlan } from './list-projection.js';
import {
    adoptListProjection,
    applyListProjectionDelta,
    ensureListProjection,
    listProjectionCardByKey,
    listProjectionOrder,
    listProjectionRevision,
    listProjectionRowByKey,
    listProjectionRowModel,
    listProjectionRows,
    listProjectionSwapPending,
    listProjectionWindowed,
    prepareListProjectionSwap,
    resetListProjection,
    setListProjectionVisibleKeys,
} from './list-projection.js';

function row(key: string, name: string, extra = ''): string {
    return `<tr id="server-${key}" data-key="${key}" data-name="${name}"><td class="cell-name"><a href="#${key}">${name}</a></td><td>${extra || 'Ready'}</td></tr>`;
}

function card(key: string, name: string): string {
    return `<article class="ro-pcard" data-key="${key}"><a href="#${key}">${name} card</a></article>`;
}

function buildList(
    keys: readonly string[],
    options: { cards?: boolean; windowed?: boolean } = {},
): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.innerHTML = `
        <div class="ro-table-wrap${options.windowed ? ' ro-windowed' : ''}">
            <table class="ro-table">
                <thead><tr><th data-hint="string">Name</th><th data-hint="enum">Status</th></tr></thead>
                <tbody>${keys.map((key) => row(key, `Name ${key}`)).join('')}</tbody>
            </table>
        </div>
        ${options.cards ? `<div class="ro-cardlist">${keys.map((key) => card(key, `Name ${key}`)).join('')}</div>` : ''}
        <span data-ro-live-region="count">old count</span>
        <div data-ro-live-region="phase">old phase</div>
        <span data-ro-live-region="found">old found</span>
        <div id="ro-live-status"></div>`;
    return content;
}

function morphInPlace(current: HTMLElement, incoming: HTMLElement): HTMLElement[] {
    if (current.tagName !== incoming.tagName) {
        current.replaceWith(incoming);
        return [incoming];
    }
    for (const attribute of Array.from(current.attributes)) current.removeAttribute(attribute.name);
    for (const attribute of Array.from(incoming.attributes)) {
        current.setAttribute(attribute.name, attribute.value);
    }
    current.replaceChildren(...Array.from(incoming.childNodes, (child) => child.cloneNode(true)));
    return [current];
}

function mount(
    keys: readonly string[],
    options: { cards?: boolean; windowed?: boolean } = {},
): HTMLElement {
    const content = buildList(keys, options);
    document.body.append(content);
    adoptListProjection(content);
    return content;
}

function apply(overrides: Partial<ListProjectionDeltaPlan>) {
    return applyListProjectionDelta({ remove: [], upsert: [], regions: [], ...overrides });
}

beforeEach(() => {
    document.body.replaceChildren();
    resetListProjection();
    vi.stubGlobal('Idiomorph', { morph: vi.fn(morphInPlace) });
});

describe('canonical projection', () => {
    test('captures one keyed row/card/model snapshot and keeps identity-idempotent ensure', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true });
        const revision = listProjectionRevision();

        expect(ensureListProjection(content)).toBe(false);
        expect(listProjectionRevision()).toBe(revision);
        expect(listProjectionOrder()).toStrictEqual(['dev/a', 'dev/b']);
        expect(listProjectionRows()).toHaveLength(2);
        expect(listProjectionRowModel()).toMatchObject({
            fields: [
                { label: 'Name', name: 'name', hint: 'string' },
                { label: 'Status', name: 'status', hint: 'enum' },
            ],
            rows: [
                { key: 'dev/a', name: 'Name dev/a' },
                { key: 'dev/b', name: 'Name dev/b' },
            ],
        });
        expect(listProjectionCardByKey('dev/b')).toHaveTextContent('Name dev/b card');

        setListProjectionVisibleKeys(new Set(['dev/b']));
        expect(listProjectionRowModel().visibleKeys).toStrictEqual(new Set(['dev/b']));
    });

    test('adopts a fresh identity once and reset clears the complete public model', () => {
        const beforeAdopt = listProjectionRevision();
        const content = buildList(['dev/a'], { cards: true, windowed: true });
        document.body.append(content);

        expect(ensureListProjection(content)).toBe(true);
        expect(listProjectionRevision()).toBe(beforeAdopt + 1);
        expect(listProjectionWindowed()).toBe(true);
        setListProjectionVisibleKeys(new Set(['dev/a']));

        const beforeReset = listProjectionRevision();
        resetListProjection();

        expect(listProjectionRevision()).toBe(beforeReset + 1);
        expect(listProjectionWindowed()).toBe(false);
        expect(listProjectionRows()).toStrictEqual([]);
        expect(listProjectionRowModel()).toMatchObject({
            fields: [],
            rows: [],
            visibleKeys: null,
        });
    });

    test('keeps the last prepared snapshot when a reentrant preparation replaces it', () => {
        mount(['dev/a']);
        const revision = listProjectionRevision();
        const older = document.createDocumentFragment();
        older.append(buildList(['dev/b']));
        const replacement = document.createDocumentFragment();
        replacement.append(buildList(['dev/c']));

        prepareListProjectionSwap(older);
        prepareListProjectionSwap(replacement);

        expect(listProjectionSwapPending()).toBe(true);
        expect(listProjectionRevision()).toBe(revision + 2);
        expect(listProjectionRowModel().rows.map((model) => model.key)).toStrictEqual(['dev/c']);
    });
});

describe('server-trusting delta apply', () => {
    test('accepts real template markup without reimplementing ids, classes, styles or nested schema', () => {
        mount(['events/e1'], { cards: true });
        const result = apply({
            upsert: [
                {
                    key: 'events/e1',
                    row: `<tr id="event-row-from-server" data-key="events/e1" data-name="Event"><td class="cell-name"><a href="#event">Event</a></td><td><span class="kind-tile" style="--kh:37"><svg viewBox="0 0 1 1"><path d="M0 0h1v1z"></path></svg>UnknownKind</span></td></tr>`,
                    card: `<section data-key="events/e1"><span style="--kh:37">UnknownKind card</span></section>`,
                },
            ],
            regions: [
                {
                    region: 'count',
                    html: '<strong data-ro-live-region="count"><em>1 event</em></strong>',
                },
            ],
        });

        expect(result.ok).toBe(true);
        expect(listProjectionRowByKey('events/e1')).toHaveAttribute('id', 'event-row-from-server');
        expect(listProjectionRowByKey('events/e1')?.querySelector('.kind-tile')).toHaveStyle({
            '--kh': '37',
        });
        expect(listProjectionCardByKey('events/e1')).toHaveTextContent('UnknownKind card');
        expect(document.querySelector('[data-ro-live-region="count"]')).toHaveTextContent(
            '1 event',
        );
        expect(document.getElementById('ro-live-status')?.textContent).toBe(
            'Live update: 1 row, 1 region',
        );
    });

    test.each([
        ['two row roots', `${row('dev/a', 'A')}${row('dev/a', 'A again')}`],
        ['wrong data key', row('dev/other', 'Other')],
        ['missing data key', '<tr><td>Missing key</td></tr>'],
        ['meaningful sibling text', `${row('dev/a', 'A')}not whitespace`],
        ['an empty sibling element', `<i></i>${row('dev/a', 'A')}`],
    ])('rejects an upsert with %s before changing the projection', (_name, html) => {
        const content = mount(['dev/a']);
        const before = content.innerHTML;

        const result = apply({ upsert: [{ key: 'dev/a', row: html }] });

        expect(result).toStrictEqual({ ok: false });
        expect(content.innerHTML).toBe(before);
        expect(listProjectionOrder()).toStrictEqual(['dev/a']);
    });

    test('rejects a mismatched card or region root before changing the projection', () => {
        const content = mount(['dev/a'], { cards: true });
        const before = content.innerHTML;

        expect(
            apply({
                upsert: [
                    {
                        key: 'dev/a',
                        row: row('dev/a', 'Replacement'),
                        card: card('dev/other', 'Wrong card'),
                    },
                ],
            }),
        ).toStrictEqual({ ok: false });
        expect(
            apply({
                regions: [
                    {
                        region: 'count',
                        html: '<span data-ro-live-region="phase">wrong region</span>',
                    },
                ],
            }),
        ).toStrictEqual({ ok: false });
        expect(content.innerHTML).toBe(before);
    });

    test('requires the connected content to still own the adopted projection', () => {
        const content = mount(['dev/a']);
        const before = content.innerHTML;
        resetListProjection();

        expect(
            apply({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }] }),
        ).toStrictEqual({ ok: false });
        expect(content.innerHTML).toBe(before);
    });

    test('fails closed when the morph implementation is unavailable', () => {
        const content = mount(['dev/a']);
        const before = content.innerHTML;
        vi.stubGlobal('Idiomorph', {});

        expect(
            apply({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }] }),
        ).toStrictEqual({ ok: false });
        expect(content.innerHTML).toBe(before);
    });

    test('inserts, removes and reorders existing nodes in place while preserving keyboard focus', () => {
        const content = mount(['dev/a', 'dev/b', 'dev/c']);
        const retained = listProjectionRowByKey('dev/b');
        const focused = retained?.querySelector('a') as HTMLAnchorElement;
        focused.focus();

        const result = apply({
            remove: [{ key: 'dev/a', cause: 'project' }],
            upsert: [{ key: 'dev/d', row: row('dev/d', 'Name dev/d') }],
            order: ['dev/c', 'dev/b', 'dev/d'],
        });
        expect(result.ok).toBe(true);
        if (result.ok) result.restoreFocus();

        expect(listProjectionOrder()).toStrictEqual(['dev/c', 'dev/b', 'dev/d']);
        expect(
            Array.from(
                content.querySelectorAll('tbody > tr[data-key]'),
                (element) => (element as HTMLElement).dataset.key,
            ),
        ).toStrictEqual(['dev/c', 'dev/b', 'dev/d']);
        expect(listProjectionRowByKey('dev/b')).toBe(retained);
        expect(listProjectionRowByKey('dev/a')).toBeNull();
        expect(document.activeElement).toBe(focused);
    });

    test('restores focus by key and cell when a row morph replaces the focused descendant', () => {
        mount(['dev/a']);
        const oldLink = listProjectionRowByKey('dev/a')?.querySelector('a') as HTMLAnchorElement;
        oldLink.focus();

        const result = apply({
            upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }],
        });
        expect(result.ok).toBe(true);
        if (result.ok) {
            expect(result.previousByKey.get('dev/a')).toHaveTextContent('Name dev/a');
            const replacement = listProjectionRowByKey('dev/a')?.querySelector(
                'a',
            ) as HTMLAnchorElement;
            const focus = vi.spyOn(replacement, 'focus');
            result.restoreFocus();
            expect(focus).toHaveBeenCalledExactlyOnceWith({ preventScroll: true });
        }

        const replacement = listProjectionRowByKey('dev/a')?.querySelector('a');
        expect(replacement).not.toBe(oldLink);
        expect(replacement).toHaveTextContent('Replacement');
        expect(document.activeElement).toBe(replacement);
    });

    test('leaves focus unrestored when the replacement cell has no corresponding target', () => {
        mount(['dev/a']);
        const oldLink = listProjectionRowByKey('dev/a')?.querySelector('a') as HTMLAnchorElement;
        oldLink.focus();

        const result = apply({
            upsert: [
                {
                    key: 'dev/a',
                    row: '<tr data-key="dev/a"><td class="cell-name">No link</td><td>Ready</td></tr>',
                },
            ],
        });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        expect(() => result.restoreFocus()).not.toThrow();
        expect(document.activeElement).not.toBe(oldLink);
    });

    test('captures and restores a table cell that is itself the focused target', () => {
        mount(['dev/a']);
        const currentRow = listProjectionRowByKey('dev/a') as HTMLTableRowElement;
        const cell = currentRow.cells.item(0) as HTMLTableCellElement;
        cell.tabIndex = 0;
        cell.focus();

        const result = apply({
            upsert: [
                {
                    key: 'dev/a',
                    row: '<tr data-key="dev/a"><td class="cell-name" tabindex="0">Replacement</td><td>Ready</td></tr>',
                },
            ],
        });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        const replacementRow = listProjectionRowByKey('dev/a') as HTMLTableRowElement;
        const replacement = replacementRow.cells.item(0) as HTMLTableCellElement;
        const focus = vi.spyOn(replacement, 'focus');
        result.restoreFocus();
        expect(result.focusKey).toBe('dev/a');
        expect(focus).toHaveBeenCalledExactlyOnceWith({ preventScroll: true });
        expect(document.activeElement).toBe(replacement);
    });

    test('does not bookmark a focus target inside a nested table cell', () => {
        mount(['dev/a']);
        const rowElement = listProjectionRowByKey('dev/a') as HTMLTableRowElement;
        const cell = rowElement.cells.item(0) as HTMLTableCellElement;
        cell.innerHTML =
            '<table><tbody><tr><td><a href="#nested">Nested control</a></td></tr></tbody></table>';
        const nested = cell.querySelector('a') as HTMLAnchorElement;
        nested.focus();

        const result = apply({
            upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }],
        });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        expect(result.focusKey).toBeNull();
    });

    test('does not bookmark a browser-focusable control outside the restore selector', () => {
        mount(['dev/a']);
        const rowElement = listProjectionRowByKey('dev/a') as HTMLTableRowElement;
        const cell = rowElement.cells.item(0) as HTMLTableCellElement;
        cell.innerHTML = '<span contenteditable="true">Editable control</span>';
        const editable = cell.querySelector('span') as HTMLSpanElement;
        editable.focus();
        expect(document.activeElement).toBe(editable);

        const result = apply({
            upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }],
        });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        expect(result.focusKey).toBeNull();
    });

    test('restores focus when a morph throws after displacing the active element', () => {
        mount(['dev/a']);
        const link = listProjectionRowByKey('dev/a')?.querySelector('a') as HTMLAnchorElement;
        link.focus();
        vi.stubGlobal('Idiomorph', {
            morph: vi.fn(() => {
                link.blur();
                throw new Error('morph failed');
            }),
        });

        expect(
            apply({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }] }),
        ).toStrictEqual({ ok: false });
        expect(document.activeElement).toBe(link);
    });

    test('drops the projection when a morph throws after rows were already removed', () => {
        const content = mount(['dev/a', 'dev/b']);
        vi.stubGlobal('Idiomorph', {
            morph: vi.fn(() => {
                throw new Error('morph failed');
            }),
        });

        expect(
            apply({
                remove: [{ key: 'dev/a', cause: 'delete' }],
                upsert: [{ key: 'dev/b', row: row('dev/b', 'Replacement') }],
            }),
        ).toStrictEqual({ ok: false });

        // The remove already hit the DOM. The index must not keep claiming the
        // row exists -- the caller's answer to !ok is a full resync, and a stale
        // index would decide what that resync morphs against.
        expect(content.querySelector('[data-key="dev/a"]')).toBeNull();
        expect(listProjectionOrder()).toStrictEqual([]);
        expect(listProjectionRowByKey('dev/a')).toBeNull();
        expect(listProjectionRowByKey('dev/b')).toBeNull();
    });

    test('rejects an invalid explicit order before any valid upsert can morph the DOM', () => {
        const content = mount(['dev/a', 'dev/b']);
        const before = content.innerHTML;

        const result = apply({
            upsert: [{ key: 'dev/a', row: row('dev/a', 'Changed too early') }],
            order: ['dev/a', 'dev/missing'],
        });

        expect(result).toStrictEqual({ ok: false });
        expect(content.innerHTML).toBe(before);
        expect(listProjectionRowByKey('dev/a')).toHaveTextContent('Name dev/a');
    });

    test('appends an implicit insertion and reports a tail removal as an order change', () => {
        mount(['dev/a', 'dev/b']);
        const inserted = apply({ upsert: [{ key: 'dev/c', row: row('dev/c', 'Name dev/c') }] });

        expect(inserted.ok).toBe(true);
        if (!inserted.ok) return;
        expect(inserted.summary).toMatchObject({ inserted: 1, reordered: true });
        expect(listProjectionOrder()).toStrictEqual(['dev/a', 'dev/b', 'dev/c']);

        const removed = apply({ remove: [{ key: 'dev/c', cause: 'project' }] });
        expect(removed.ok).toBe(true);
        if (!removed.ok) return;
        expect(removed.summary).toMatchObject({ projected: 1, reordered: true });
        expect(document.getElementById('ro-live-status')?.textContent).toBe(
            'Live update: 1 row, order changed',
        );
    });

    test('detects a reorder even when one position is unchanged', () => {
        mount(['dev/a', 'dev/b', 'dev/c']);

        const result = apply({ order: ['dev/c', 'dev/b', 'dev/a'] });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        expect(result.summary.reordered).toBe(true);
        expect(document.getElementById('ro-live-status')?.textContent).toBe(
            'Live update: order changed',
        );
    });

    test('classifies delete/project removals, prunes visibility and forgets removed cards', () => {
        mount(['dev/a', 'dev/b', 'dev/c', 'dev/d'], { cards: true });
        setListProjectionVisibleKeys(new Set(['dev/a', 'dev/c', 'dev/d', 'missing']));
        const beforeRevision = listProjectionRevision();

        const result = apply({
            remove: [
                { key: 'dev/a', cause: 'delete' },
                { key: 'dev/b', cause: 'delete' },
                { key: 'dev/c', cause: 'project' },
            ],
        });

        expect(result.ok).toBe(true);
        if (!result.ok) return;
        expect(listProjectionRevision()).toBe(beforeRevision + 1);
        expect(result.summary).toMatchObject({ deleted: 2, projected: 1, reordered: true });
        expect(listProjectionRowModel().visibleKeys).toStrictEqual(new Set(['dev/d']));
        expect(listProjectionCardByKey('dev/a')).toBeNull();
        expect(listProjectionCardByKey('dev/b')).toBeNull();
        expect(listProjectionCardByKey('dev/c')).toBeNull();
        expect(document.getElementById('ro-live-status')?.textContent).toBe(
            'Live update: 3 rows, order changed',
        );
    });

    test('reports multiple region-only changes without inventing a zero-row update', () => {
        mount(['dev/a']);

        const result = apply({
            regions: [
                { region: 'count', html: '<span data-ro-live-region="count">new count</span>' },
                { region: 'phase', html: '<div data-ro-live-region="phase">new phase</div>' },
            ],
        });

        expect(result.ok).toBe(true);
        expect(document.getElementById('ro-live-status')?.textContent).toBe(
            'Live update: 2 regions',
        );
    });

    test('morphs a card only when the server includes one', () => {
        mount(['dev/a'], { cards: true });
        const original = listProjectionCardByKey('dev/a');

        expect(apply({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Row only') }] }).ok).toBe(true);
        expect(listProjectionCardByKey('dev/a')).toBe(original);
        expect(original).toHaveTextContent('Name dev/a card');

        expect(
            apply({
                upsert: [
                    {
                        key: 'dev/a',
                        row: row('dev/a', 'Row and card'),
                        card: card('dev/a', 'Replacement'),
                    },
                ],
            }).ok,
        ).toBe(true);
        expect(listProjectionCardByKey('dev/a')).toBe(original);
        expect(original).toHaveTextContent('Replacement card');
    });

    test('publishes the full windowed model while leaving viewport placement to the virtualizer', () => {
        const content = mount(['dev/a', 'dev/b'], { windowed: true });

        const result = apply({
            upsert: [{ key: 'dev/c', row: row('dev/c', 'Name dev/c') }],
            order: ['dev/c', 'dev/a', 'dev/b'],
        });

        expect(result.ok).toBe(true);
        expect(listProjectionOrder()).toStrictEqual(['dev/c', 'dev/a', 'dev/b']);
        expect(listProjectionRowModel().rows.map((model) => model.key)).toStrictEqual([
            'dev/c',
            'dev/a',
            'dev/b',
        ]);
        expect(content.querySelector('tbody > tr[data-key="dev/c"]')).toBeNull();
    });

    test('places supplied cards independently of windowed row ownership', () => {
        const content = mount(['dev/a', 'dev/b'], { cards: true, windowed: true });

        const result = apply({
            upsert: [
                {
                    key: 'dev/c',
                    row: row('dev/c', 'Name dev/c'),
                    card: card('dev/c', 'Name dev/c'),
                },
            ],
            order: ['dev/c', 'dev/b', 'dev/a'],
        });

        expect(result.ok).toBe(true);
        expect(content.querySelector('tbody > tr[data-key="dev/c"]')).toBeNull();
        expect(
            Array.from(
                content.querySelectorAll('.ro-cardlist > [data-key]'),
                (element) => (element as HTMLElement).dataset.key,
            ),
        ).toStrictEqual(['dev/c', 'dev/b', 'dev/a']);
        expect(listProjectionCardByKey('dev/c')?.isConnected).toBe(true);
    });
});
