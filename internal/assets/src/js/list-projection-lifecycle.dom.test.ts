// @vitest-environment jsdom

import { beforeEach, expect, test } from 'vitest';
import {
    adoptListProjection,
    commitListProjectionSwap,
    ensureListProjection,
    listProjectionOrder,
    listProjectionRevision,
    listProjectionRowByKey,
    listProjectionRowModel,
    listProjectionRows,
    listProjectionSwapPending,
    prepareListProjectionSwap,
    resetListProjection,
} from './list-projection.js';

interface RowFixture {
    key: string;
    name: string;
}

function buildList(rows: readonly RowFixture[], windowed = false): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.innerHTML = `
        <div class="ro-table-wrap${windowed ? ' ro-windowed' : ''}">
            <table class="ro-table">
                <thead><tr><th data-hint="string">Name</th><th data-hint="enum">Status</th></tr></thead>
                <tbody>${rows
                    .map(
                        ({ key, name }) =>
                            `<tr data-key="${key}"><td class="cell-name"><a>${name}</a></td><td>Ready</td></tr>`,
                    )
                    .join('')}</tbody>
            </table>
        </div>`;
    return content;
}

function fragmentFrom(content: HTMLElement): DocumentFragment {
    const fragment = document.createDocumentFragment();
    fragment.append(...Array.from(content.childNodes));
    return fragment;
}

beforeEach(() => {
    document.body.replaceChildren();
    resetListProjection();
});

test('prepares one incoming model and commits the connected small-list projection', () => {
    const content = buildList([
        { key: 'pods/a', name: 'Alpha' },
        { key: 'pods/b', name: 'Beta' },
    ]);
    document.body.append(content);
    adoptListProjection(content);
    const initialRevision = listProjectionRevision();
    const oldA = listProjectionRowByKey('pods/a');

    const incoming = fragmentFrom(
        buildList([
            { key: 'pods/b', name: 'Beta next' },
            { key: 'pods/c', name: 'Gamma' },
        ]),
    );
    const prepared = prepareListProjectionSwap(incoming);

    expect(prepared).toMatchObject({ windowed: false });
    expect(prepared.rows.map((row) => row.dataset.key)).toStrictEqual(['pods/b', 'pods/c']);
    expect(listProjectionSwapPending()).toBe(true);
    expect(listProjectionRevision()).toBe(initialRevision + 1);
    expect(listProjectionRowModel().rows.map((row) => row.name)).toStrictEqual([
        'Beta next',
        'Gamma',
    ]);
    expect(prepareListProjectionSwap(incoming).rows).toStrictEqual(prepared.rows);
    expect(listProjectionRevision()).toBe(initialRevision + 1);

    content.replaceChildren(...Array.from(incoming.childNodes));
    const connectedBeta = content.querySelector('[data-key="pods/b"] .cell-name a');
    if (connectedBeta) connectedBeta.textContent = 'Beta connected';
    const previous = commitListProjectionSwap();

    expect(previous?.get('pods/a')).toBe(oldA);
    expect(listProjectionSwapPending()).toBe(false);
    expect(listProjectionOrder()).toStrictEqual(['pods/b', 'pods/c']);
    expect(listProjectionRowByKey('pods/b')?.isConnected).toBe(true);
    expect(listProjectionRowModel().rows.map((row) => row.name)).toStrictEqual([
        'Beta connected',
        'Gamma',
    ]);
    expect(ensureListProjection(content)).toBe(false);
    expect(listProjectionRevision()).toBe(initialRevision + 1);
});

test('commits a windowed incoming snapshot as the complete detached row model', () => {
    const content = buildList([{ key: 'pods/a', name: 'Alpha' }], true);
    document.body.append(content);
    adoptListProjection(content);

    const incoming = fragmentFrom(
        buildList(
            [
                { key: 'pods/b', name: 'Beta' },
                { key: 'pods/c', name: 'Gamma' },
            ],
            true,
        ),
    );
    const prepared = prepareListProjectionSwap(incoming);
    const incomingRows = [...prepared.rows];

    expect(prepared.windowed).toBe(true);
    expect(commitListProjectionSwap()).not.toBeNull();
    expect(listProjectionRows()).toStrictEqual(incomingRows);
    expect(listProjectionOrder()).toStrictEqual(['pods/b', 'pods/c']);
    expect(listProjectionRowModel().rows.map((row) => row.key)).toStrictEqual(['pods/b', 'pods/c']);
    expect(ensureListProjection(content)).toBe(false);
});

test('does not treat a history-restored viewport slice as a complete projection', () => {
    const content = buildList([{ key: 'pods/partial', name: 'Partial' }], true);
    const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
    const spacer = document.createElement('tr');
    spacer.className = 'ro-vspacer';
    spacer.append(document.createElement('td'));
    tbody.prepend(spacer);
    document.body.append(content);

    adoptListProjection(content);

    expect(listProjectionRows()).toStrictEqual([]);
    expect(listProjectionOrder()).toStrictEqual([]);
    expect(listProjectionRowModel().rows).toStrictEqual([]);
});
