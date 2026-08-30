import { beforeEach, expect, test, vi } from 'vitest';

const leaves = vi.hoisted(() => ({
    bulk: [{ event: 'bulk:first', handler: vi.fn() }],
    columns: [{ event: 'columns:first', handler: vi.fn() }],
    contextMenu: [
        { event: 'context:first', handler: vi.fn() },
        { event: 'context:second', handler: vi.fn() },
    ],
    filters: [{ event: 'filters:first', handler: vi.fn() }],
    folds: [{ event: 'folds:first', handler: vi.fn() }],
    keyboard: [{ event: 'keyboard:first', handler: vi.fn() }],
    logs: [{ event: 'logs:first', handler: vi.fn() }],
    misc: [
        { event: 'misc:first', handler: vi.fn() },
        { event: 'misc:second', handler: vi.fn() },
    ],
    palette: [{ event: 'palette:first', handler: vi.fn() }],
    refresh: [
        { event: 'refresh:first', handler: vi.fn() },
        { event: 'refresh:second', handler: vi.fn() },
        { event: 'refresh:third', handler: vi.fn() },
        { event: 'refresh:fourth', handler: vi.fn() },
    ],
    rowSelection: [{ event: 'row-selection:first', handler: vi.fn() }],
}));

vi.mock('./bulk-actions.js', () => ({ bulkBindings: leaves.bulk }));
vi.mock('./columns.js', () => ({ columnsBindings: leaves.columns }));
vi.mock('./context-menu.js', () => ({ contextMenuBindings: leaves.contextMenu }));
vi.mock('./filters.js', () => ({ filtersBindings: leaves.filters }));
vi.mock('./keyboard.js', () => ({ keyboardBindings: leaves.keyboard }));
vi.mock('./logs.js', () => ({ logsBindings: leaves.logs }));
vi.mock('./misc-ui.js', () => ({ miscBindings: leaves.misc }));
vi.mock('./palette.js', () => ({ paletteBindings: leaves.palette }));
vi.mock('./refresh.js', () => ({ refreshBindings: leaves.refresh }));
vi.mock('./row-selection.js', () => ({ rowSelectionBindings: leaves.rowSelection }));
vi.mock('./yaml-folds.js', () => ({ foldBindings: leaves.folds }));

beforeEach(() => {
    vi.resetModules();
});

test('flattens every feature binding in the single load-bearing registration order', async () => {
    const { bindings } = await import('./bindings.js');
    const expected = [
        ...leaves.contextMenu,
        ...leaves.bulk,
        ...leaves.rowSelection,
        ...leaves.columns,
        ...leaves.filters,
        ...leaves.palette,
        ...leaves.keyboard,
        ...leaves.folds,
        ...leaves.logs,
        ...leaves.misc,
        ...leaves.refresh,
    ];

    expect(bindings).toStrictEqual(expected);
    bindings.forEach((binding, index) => {
        expect(binding).toBe(expected[index]);
    });
});
