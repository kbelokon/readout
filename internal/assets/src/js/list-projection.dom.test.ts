// @vitest-environment jsdom

import { beforeEach, describe, expect, test } from 'vitest';
import {
    adoptListProjection,
    commitListProjectionSwap,
    ensureListProjection,
    listProjectionCardByKey,
    listProjectionOrder,
    listProjectionRowByKey,
    listProjectionRowModel,
    listProjectionRows,
    listProjectionSwapPending,
    listProjectionVisibleRows,
    listProjectionWindowed,
    prepareListProjectionSwap,
    resetListProjection,
    setListProjectionVisibleKeys,
} from './list-projection.js';

interface ListOptions {
    keyedCards?: boolean;
    windowed?: boolean;
}

function buildList(keys: readonly string[], options: ListOptions = {}): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    const wrap = document.createElement('div');
    wrap.className = `ro-table-wrap${options.windowed ? ' ro-windowed' : ''}`;
    const table = document.createElement('table');
    table.className = 'ro-table';
    table.innerHTML = `
        <thead><tr><th data-hint="string">Name</th><th data-hint="enum">Status</th></tr></thead>
        <tbody></tbody>`;
    const tbody = table.tBodies.item(0) as HTMLTableSectionElement;
    const cards = document.createElement('div');
    cards.className = 'ro-cardlist';
    keys.forEach((key, index) => {
        const row = tbody.insertRow();
        row.dataset.key = key;
        row.innerHTML = `<td class="cell-name"><a> Workload ${index} </a></td><td> Ready ${index} </td>`;
        const card = document.createElement('div');
        card.className = 'ro-pcard';
        if (options.keyedCards) {
            card.dataset.key = key;
        }
        card.textContent = `Card ${key}`;
        cards.append(card);
    });
    wrap.append(table);
    content.append(wrap, cards);
    return content;
}

beforeEach(() => {
    document.body.replaceChildren();
    resetListProjection();
    setListProjectionVisibleKeys(null);
});

describe('complete snapshot adoption', () => {
    test('owns small-list rows, order, keyed cards and the stable row-model facade', () => {
        const content = buildList(['prod/a', 'prod/b'], { keyedCards: true });
        document.body.append(content);
        const modelFacade = window.roRowModel;
        setListProjectionVisibleKeys(new Set(['stale-page/key']));

        adoptListProjection(content);

        expect(listProjectionWindowed()).toBe(false);
        expect(listProjectionOrder()).toStrictEqual(['prod/a', 'prod/b']);
        expect(listProjectionRows()).toStrictEqual(
            Array.from(content.querySelectorAll('tbody tr[data-key]')),
        );
        expect(listProjectionRowByKey('prod/b')).toBe(
            content.querySelector('tr[data-key="prod/b"]'),
        );
        expect(listProjectionCardByKey('prod/a')).toBe(
            content.querySelector('.ro-pcard[data-key="prod/a"]'),
        );
        expect(listProjectionRowModel()).toBe(modelFacade);
        expect(window.roRowModel).toBe(modelFacade);
        expect(modelFacade).toStrictEqual({
            fields: [
                { hint: 'string', label: 'Name', name: 'name' },
                { hint: 'enum', label: 'Status', name: 'status' },
            ],
            rows: [
                { cells: ['Workload 0', 'Ready 0'], key: 'prod/a', name: 'Workload 0' },
                { cells: ['Workload 1', 'Ready 1'], key: 'prod/b', name: 'Workload 1' },
            ],
            visibleKeys: null,
        });
    });

    test('indexes legacy unkeyed cards positionally only for a complete 1:1 list', () => {
        const content = buildList(['a', 'b']);
        const extra = document.createElement('div');
        extra.className = 'ro-pcard';
        content.querySelector('.ro-cardlist')?.append(extra);

        adoptListProjection(content);
        expect(listProjectionCardByKey('a')).toBeNull();

        extra.remove();
        adoptListProjection(content);
        expect(listProjectionCardByKey('a')).toHaveTextContent('Card a');
        expect(listProjectionCardByKey('b')).toHaveTextContent('Card b');
    });

    test('commits a small morph to connected nodes rather than throwaway fragment nodes', () => {
        const oldContent = buildList(['a', 'b'], { keyedCards: true });
        document.body.append(oldContent);
        adoptListProjection(oldContent);

        const fragment = document.createDocumentFragment();
        const incoming = buildList(['b', 'a'], { keyedCards: true });
        fragment.append(incoming);
        const throwawayB = incoming.querySelector('tr[data-key="b"]');
        prepareListProjectionSwap(fragment);

        // Idiomorph may retain/move existing identities. Model that by mounting
        // a different set of connected nodes than the source fragment held.
        const landed = buildList(['b', 'a'], { keyedCards: true });
        document.body.replaceChildren(landed);
        expect(commitListProjectionSwap()).not.toBeNull();

        expect(listProjectionOrder()).toStrictEqual(['b', 'a']);
        expect(listProjectionRowByKey('b')).toBe(landed.querySelector('tr[data-key="b"]'));
        expect(listProjectionRowByKey('b')).not.toBe(throwawayB);
        expect(listProjectionRowByKey('b')?.isConnected).toBe(true);
        expect(listProjectionSwapPending()).toBe(false);
        expect(commitListProjectionSwap()).toBeNull();
    });
});

describe('windowed and history snapshots', () => {
    test('keeps every prepared row and model entry authoritative while nodes are off-DOM', () => {
        const fragment = document.createDocumentFragment();
        const incoming = buildList(['a', 'b', 'c', 'd'], { windowed: true });
        fragment.append(incoming);
        const priorVisibility = new Set(['old-page/key']);
        setListProjectionVisibleKeys(priorVisibility);

        const firstPreparation = prepareListProjectionSwap(fragment);
        expect(listProjectionRowModel().visibleKeys).toBe(priorVisibility);
        const preparedRows = [...firstPreparation.rows];
        const tbody = incoming.querySelector('tbody') as HTMLTableSectionElement;
        const top = document.createElement('tr');
        top.className = 'ro-vspacer';
        top.append(document.createElement('td'));
        const bottom = top.cloneNode(true) as HTMLTableRowElement;
        tbody.replaceChildren(top, bottom);

        // The second projection call made by the virtualizer must not recapture
        // the spacer-only fragment and erase the complete snapshot.
        const secondPreparation = prepareListProjectionSwap(fragment);
        expect(secondPreparation.rows).toBe(firstPreparation.rows);
        document.body.append(incoming);
        expect(commitListProjectionSwap()).not.toBeNull();
        expect(listProjectionRowModel().visibleKeys).toBe(priorVisibility);

        expect(listProjectionWindowed()).toBe(true);
        expect(listProjectionOrder()).toStrictEqual(['a', 'b', 'c', 'd']);
        expect(listProjectionRows()).toStrictEqual(preparedRows);
        expect(preparedRows.every((row) => !row.isConnected)).toBe(true);
        expect(listProjectionRowModel().rows.map((row) => row.key)).toStrictEqual([
            'a',
            'b',
            'c',
            'd',
        ]);
        setListProjectionVisibleKeys(new Set(['d']));
        expect(listProjectionVisibleRows()).toStrictEqual([preparedRows[3]]);
        expect(listProjectionRowByKey('d')).toBe(preparedRows[3]);
    });

    test('rejects history-restored spacer slices idempotently instead of adopting partial rows', () => {
        const complete = buildList(['old-a', 'old-b'], { windowed: true });
        adoptListProjection(complete);
        const facade = listProjectionRowModel();

        const cached = buildList(['cached-only'], { windowed: true });
        const tbody = cached.querySelector('tbody') as HTMLTableSectionElement;
        const spacer = document.createElement('tr');
        spacer.className = 'ro-vspacer';
        spacer.append(document.createElement('td'));
        tbody.prepend(spacer);

        expect(ensureListProjection(cached)).toBe(true);
        expect(ensureListProjection(cached)).toBe(false);

        expect(listProjectionRows()).toStrictEqual([]);
        expect(listProjectionOrder()).toStrictEqual([]);
        expect(listProjectionRowByKey('cached-only')).toBeNull();
        expect(listProjectionRowModel()).toBe(facade);
        expect(facade.fields).toStrictEqual([]);
        expect(facade.rows).toStrictEqual([]);
        expect(facade.visibleKeys).toBeNull();
    });

    test('reset clears the full projection and stale visibility together', () => {
        const content = buildList(['a'], { keyedCards: true });
        adoptListProjection(content);
        setListProjectionVisibleKeys(new Set(['a']));

        resetListProjection();

        expect(listProjectionRows()).toStrictEqual([]);
        expect(listProjectionRowModel().visibleKeys).toBeNull();
    });
});
