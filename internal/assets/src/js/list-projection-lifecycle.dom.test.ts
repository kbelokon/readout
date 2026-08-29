// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    reapplyRowState: vi.fn(),
    requestListRefresh: vi.fn(),
}));

vi.mock('./refresh.js', () => ({
    requestListRefresh: dependencies.requestListRefresh,
}));

vi.mock('./row-selection.js', () => ({
    reapplyRowState: dependencies.reapplyRowState,
}));

import { applyLiveNameFilter, captureRowModelFromDocument } from './filters.js';
import { listProjectionRowModel, prepareListProjectionSwap } from './list-projection.js';
import {
    virtRows,
    virtualizeAfterSwap,
    virtualizeInit,
    virtualizePrepareSwap,
    virtVisible,
} from './virtualizer.js';

interface RowFixture {
    key: string;
    name: string;
}

interface ListFixtureOptions {
    cards?: boolean;
    draft?: string;
    windowed?: boolean;
}

function buildList(rows: readonly RowFixture[], options: ListFixtureOptions = {}): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    const filter = document.createElement('input');
    filter.id = 'ro-filter-input';
    filter.value = options.draft || '';
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
    rows.forEach((fixture, index) => {
        const row = tbody.insertRow();
        row.dataset.key = fixture.key;
        row.dataset.testIndex = String(index);
        row.innerHTML = `<td class="cell-name"><a>${fixture.name}</a></td><td>Ready</td>`;
        if (options.cards) {
            const card = document.createElement('article');
            card.className = 'ro-pcard';
            card.dataset.key = fixture.key;
            card.textContent = `${fixture.name} card`;
            cards.append(card);
        }
    });
    wrap.append(table);
    content.append(filter, wrap);
    if (options.cards) {
        content.append(cards);
    }
    return content;
}

function runListInit(): void {
    captureRowModelFromDocument();
    applyLiveNameFilter();
    virtualizeInit();
}

function moveChildren(from: ParentNode, to: ParentNode): void {
    while (from.firstChild) {
        to.appendChild(from.firstChild);
    }
}

afterEach(() => {
    document.body.replaceChildren();
    virtualizeInit();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
});

test('keeps filter state page-local and captures each actual projection exactly once', () => {
    vi.stubGlobal(
        'matchMedia',
        vi.fn(() => ({
            matches: false,
            addEventListener: vi.fn(),
            addListener: vi.fn(),
            dispatchEvent: vi.fn(),
            media: '',
            onchange: null,
            removeEventListener: vi.fn(),
            removeListener: vi.fn(),
        })),
    );
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 120 });
    Object.defineProperty(window, 'scrollY', { configurable: true, value: 0 });
    Object.defineProperty(window, 'scrollTo', { configurable: true, value: vi.fn() });
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
        this: Element,
    ) {
        const index = Number((this as HTMLElement).dataset.testIndex || 0);
        const top = this.matches('tr[data-key]') ? index * 20 : 0;
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
    const classToggle = vi.spyOn(DOMTokenList.prototype, 'toggle');
    const filterToggleCount = () =>
        classToggle.mock.calls.filter(([token]) => token === 'ro-row-filtered').length;

    const capturedRoots: ParentNode[] = [];
    const elementQuerySelector = Element.prototype.querySelector;
    vi.spyOn(Element.prototype, 'querySelector').mockImplementation(function (
        this: Element,
        selector: string,
    ) {
        if (selector === 'table.ro-table' && this.id === 'resource-list-content') {
            capturedRoots.push(this);
        }
        return elementQuerySelector.call(this, selector);
    });
    const fragmentQuerySelector = DocumentFragment.prototype.querySelector;
    vi.spyOn(DocumentFragment.prototype, 'querySelector').mockImplementation(function (
        this: DocumentFragment,
        selector: string,
    ) {
        if (selector === 'table.ro-table') {
            capturedRoots.push(this);
        }
        return fragmentQuerySelector.call(this, selector);
    });

    // Initial small page: capture -> filter -> virtualize visits one content
    // root, and cards follow the same draft visibility as rows.
    const initial = buildList(
        [
            { key: 'pods/alpha', name: 'Alpha' },
            { key: 'pods/beta', name: 'Beta' },
        ],
        { cards: true, draft: 'alpha' },
    );
    document.body.append(initial);
    runListInit();
    for (let repeatInit = 0; repeatInit < 6; repeatInit += 1) {
        runListInit();
    }
    expect(capturedRoots).toStrictEqual([initial]);
    expect(filterToggleCount()).toBe(4);
    expect(initial.querySelector('[data-key="pods/alpha"].ro-pcard')).not.toHaveClass(
        'ro-row-filtered',
    );
    expect(initial.querySelector('[data-key="pods/beta"].ro-pcard')).toHaveClass('ro-row-filtered');

    // A small morph has two real projections: the complete incoming fragment
    // and the connected DOM Idiomorph may retain. Each is captured once; the
    // a defensive later init repeat is identity-idempotent.
    const smallIncoming = buildList(
        [
            { key: 'pods/beta', name: 'Beta' },
            { key: 'pods/gamma', name: 'Gamma' },
        ],
        { cards: true, draft: 'beta' },
    );
    const fragment = document.createDocumentFragment();
    moveChildren(smallIncoming, fragment);
    prepareListProjectionSwap(fragment);
    virtualizePrepareSwap(fragment);
    initial.replaceChildren();
    moveChildren(fragment, initial);
    applyLiveNameFilter();
    virtualizeAfterSwap();
    for (let descendantLoad = 0; descendantLoad < 6; descendantLoad += 1) {
        runListInit();
    }
    expect(capturedRoots).toStrictEqual([initial, fragment, initial]);
    expect(filterToggleCount()).toBe(8);
    expect(initial.querySelector('[data-key="pods/beta"].ro-pcard')).not.toHaveClass(
        'ro-row-filtered',
    );
    expect(initial.querySelector('[data-key="pods/gamma"].ro-pcard')).toHaveClass(
        'ro-row-filtered',
    );

    // Cross-page navigation gets a new content identity. The old page's
    // visibleKeys are cleared, then the active draft is re-derived BEFORE the
    // new full set is windowed.
    const windowed = buildList(
        [
            { key: 'jobs/one', name: 'One' },
            { key: 'jobs/target', name: 'Target' },
            { key: 'jobs/three', name: 'Three' },
        ],
        { draft: 'target', windowed: true },
    );
    document.body.replaceChildren(windowed);
    runListInit();
    for (let descendantLoad = 0; descendantLoad < 6; descendantLoad += 1) {
        runListInit();
    }
    expect(capturedRoots).toStrictEqual([initial, fragment, initial, windowed]);
    expect(filterToggleCount()).toBe(11);
    expect(Array.from(listProjectionRowModel().visibleKeys || [])).toStrictEqual(['jobs/target']);
    expect(virtRows()).toHaveLength(3);
    expect(virtVisible().map((row) => row.dataset.key)).toStrictEqual(['jobs/target']);

    // A cached window is a new, incomplete projection. It is rejected exactly
    // once, clears the previous page's keys, and repeated init only maintains
    // the one-shot recovery request.
    const cached = buildList([{ key: 'cached/partial', name: 'Partial' }], {
        windowed: true,
    });
    const cachedBody = cached.querySelector('tbody') as HTMLTableSectionElement;
    const spacer = document.createElement('tr');
    spacer.className = 'ro-vspacer';
    spacer.append(document.createElement('td'));
    cachedBody.prepend(spacer);
    document.body.replaceChildren(cached);
    for (let descendantLoad = 0; descendantLoad < 6; descendantLoad += 1) {
        runListInit();
    }

    expect(capturedRoots).toStrictEqual([initial, fragment, initial, windowed, cached]);
    expect(filterToggleCount()).toBe(12);
    expect(listProjectionRowModel().rows).toStrictEqual([]);
    expect(listProjectionRowModel().visibleKeys).toBeNull();
    expect(dependencies.requestListRefresh).toHaveBeenCalledOnce();
});
