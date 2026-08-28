// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

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

import {
    virtMoveFocus,
    virtRowByKey,
    virtRows,
    virtualizeAfterSwap,
    virtualizeInit,
    virtualizeOnFilterChange,
    virtualizePrepareSwap,
    virtualizerActive,
    virtVisible,
} from './virtualizer.js';

interface VirtualizerSeam {
    active(): boolean;
    renderedBounds(): { start: number; end: number; total: number };
    scrollToKey(key: string): boolean;
}

interface ListOptions {
    changedValues?: Readonly<Record<string, string>>;
    columnCount?: number;
    filteredKeys?: ReadonlySet<string>;
    lineHeight?: string;
    windowed?: boolean;
}

const ROW_HEIGHT = 20;

let animationFrames: FrameRequestCallback[];
let focusedKey: string | null;
let geometryRowHeight: number;
let headerWidths: number[];
let reducedMotion: boolean;
let scrollYValue: number;
let tbodyDocumentTop: number;
let viewportHeight: number;
let scrollByMock: ReturnType<typeof vi.fn>;
let scrollToMock: ReturnType<typeof vi.fn>;
let setFocusMock: ReturnType<typeof vi.fn<(key: string) => void>>;

function rect(top: number, width: number, height: number): DOMRect {
    return {
        bottom: top + height,
        height,
        left: 0,
        right: width,
        top,
        width,
        x: 0,
        y: top,
        toJSON: () => ({}),
    } as DOMRect;
}

function rowKeys(count: number): string[] {
    return Array.from({ length: count }, (_, index) => `row-${index}`);
}

function buildList(keys: readonly string[], options: ListOptions = {}): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';

    const wrap = document.createElement('div');
    wrap.className = `ro-table-wrap${options.windowed === false ? '' : ' ro-windowed'}`;

    const table = document.createElement('table');
    table.className = 'ro-table';
    const thead = table.createTHead();
    const header = thead.insertRow();
    const columnCount = options.columnCount ?? 2;
    for (let column = 0; column < columnCount; column += 1) {
        header.append(document.createElement('th'));
    }
    const tbody = table.createTBody();

    keys.forEach((key, index) => {
        const row = tbody.insertRow();
        row.id = `id-${key}`;
        row.dataset.key = key;
        row.dataset.testIndex = String(index);
        if (options.filteredKeys?.has(key)) {
            row.classList.add('ro-row-filtered');
        }
        const identity = row.insertCell();
        identity.textContent = key;
        if (options.lineHeight) {
            identity.style.lineHeight = options.lineHeight;
        }
        for (let column = 1; column < columnCount; column += 1) {
            const value = row.insertCell();
            value.textContent =
                column === 1
                    ? (options.changedValues?.[key] ?? `value-${key}`)
                    : `extra-${column}-${key}`;
            if (options.lineHeight) {
                value.style.lineHeight = options.lineHeight;
            }
        }
    });

    wrap.append(table);
    content.append(wrap);
    return content;
}

function renderList(keys: readonly string[], options: ListOptions = {}): HTMLTableSectionElement {
    const content = buildList(keys, options);
    document.body.replaceChildren(content);
    return content.querySelector('tbody') as HTMLTableSectionElement;
}

function listFragment(keys: readonly string[], options: ListOptions = {}): DocumentFragment {
    const fragment = document.createDocumentFragment();
    fragment.append(buildList(keys, options));
    return fragment;
}

function directRows(tbody: ParentNode): HTMLElement[] {
    return Array.from(tbody.querySelectorAll(':scope > tr[data-key]')) as HTMLElement[];
}

function directRowKeys(tbody: ParentNode): string[] {
    return directRows(tbody).map((row) => row.dataset.key as string);
}

function setVisibleKeys(keys: ReadonlySet<string> | null): void {
    (window as unknown as { roRowModel: { visibleKeys: ReadonlySet<string> | null } }).roRowModel =
        {
            visibleKeys: keys,
        };
}

function virtualizerSeam(): VirtualizerSeam {
    return (window as unknown as { roVirtual: VirtualizerSeam }).roVirtual;
}

function flushAnimationFrames(): void {
    while (animationFrames.length > 0) {
        const callbacks = animationFrames.splice(0);
        callbacks.forEach((callback) => {
            callback(performance.now());
        });
    }
}

function installBrowserGeometry(): void {
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
        this: Element,
    ) {
        const element = this as HTMLElement;
        if (element.tagName === 'TH') {
            const index = Array.from(element.parentElement?.children ?? []).indexOf(element);
            return rect(0, headerWidths[index] ?? 100, ROW_HEIGHT);
        }
        if (element.tagName === 'TBODY') {
            return rect(tbodyDocumentTop - scrollYValue, 300, ROW_HEIGHT);
        }
        if (element.matches('tr[data-key]')) {
            const index = Number(element.dataset.testIndex ?? 0);
            return rect(
                tbodyDocumentTop - scrollYValue + index * geometryRowHeight,
                300,
                geometryRowHeight,
            );
        }
        if (element.matches('header.ro-topbar')) {
            return rect(0, 300, 30);
        }
        return rect(0, 0, 0);
    });

    Object.defineProperty(window, 'innerHeight', {
        configurable: true,
        get: () => viewportHeight,
    });
    Object.defineProperty(window, 'scrollY', {
        configurable: true,
        get: () => scrollYValue,
    });

    scrollByMock = vi.fn((_x: number, delta: number) => {
        scrollYValue = Math.max(0, scrollYValue + delta);
    });
    scrollToMock = vi.fn((_x: number, nextY: number) => {
        scrollYValue = Math.max(0, nextY);
    });
    Object.defineProperty(window, 'scrollBy', {
        configurable: true,
        value: scrollByMock,
    });
    Object.defineProperty(window, 'scrollTo', {
        configurable: true,
        value: scrollToMock,
    });

    Object.defineProperty(window, 'requestAnimationFrame', {
        configurable: true,
        value: vi.fn((callback: FrameRequestCallback) => {
            animationFrames.push(callback);
            return animationFrames.length;
        }),
    });
    Object.defineProperty(window, 'matchMedia', {
        configurable: true,
        value: vi.fn((query: string) => ({
            matches: reducedMotion && query === '(prefers-reduced-motion: reduce)',
            media: query,
            onchange: null,
            addEventListener: vi.fn(),
            removeEventListener: vi.fn(),
            addListener: vi.fn(),
            removeListener: vi.fn(),
            dispatchEvent: vi.fn(),
        })),
    });
}

beforeEach(() => {
    animationFrames = [];
    focusedKey = null;
    geometryRowHeight = ROW_HEIGHT;
    headerWidths = [120, 180];
    reducedMotion = false;
    scrollYValue = 0;
    tbodyDocumentTop = 0;
    viewportHeight = 100;
    document.documentElement.style.removeProperty('--row-py');
    setFocusMock = vi.fn((key: string) => {
        focusedKey = key;
    });

    installBrowserGeometry();
    setVisibleKeys(null);
    (
        window as unknown as {
            roRowState: { focusedKey(): string | null; setFocus(key: string): void };
        }
    ).roRowState = {
        focusedKey: () => focusedKey,
        setFocus: (key: string) => setFocusMock(key),
    };

    document.body.replaceChildren();
    virtualizeInit();
    dependencies.reapplyRowState.mockClear();
    dependencies.requestListRefresh.mockClear();
});

afterEach(() => {
    document.body.replaceChildren();
    virtualizeInit();
    flushAnimationFrames();
});

describe('engagement', () => {
    test('follows the server threshold marker, initializes a marked list, and is idempotent', () => {
        const plainTbody = renderList(rowKeys(40), { windowed: false });

        virtualizeInit();

        expect(virtualizerActive()).toBe(false);
        expect(directRows(plainTbody)).toHaveLength(40);
        expect(plainTbody.querySelector('.ro-vspacer')).toBeNull();
        expect(dependencies.reapplyRowState).not.toHaveBeenCalled();

        const markedTbody = renderList(['only-row']);
        virtualizeInit();

        expect(virtualizerActive()).toBe(true);
        expect(virtRows().map((row) => row.dataset.key)).toStrictEqual(['only-row']);
        expect(markedTbody.children).toHaveLength(3);
        const spacers = Array.from(markedTbody.querySelectorAll(':scope > tr.ro-vspacer'));
        expect(spacers).toHaveLength(2);
        expect(spacers[0]).toHaveAttribute('aria-hidden', 'true');
        expect(spacers[0].firstElementChild).toHaveAttribute('colspan', '2');
        expect((spacers[0].firstElementChild as HTMLElement).style.height).toBe('0px');
        expect(markedTbody.closest('table')).toHaveClass('ro-virtualized');
        expect(markedTbody.closest('table')?.querySelectorAll('th')[0]).toHaveStyle({
            width: '120px',
        });
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();

        const bounds = virtualizerSeam().renderedBounds();
        virtualizeInit();

        expect(Array.from(markedTbody.querySelectorAll(':scope > tr.ro-vspacer'))).toStrictEqual(
            spacers,
        );
        expect(virtualizerSeam().renderedBounds()).toStrictEqual(bounds);
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
        expect(dependencies.requestListRefresh).not.toHaveBeenCalled();
    });

    test('refetches a history snapshot that contains spacers but not the full row set', () => {
        const tbody = renderList(['cached-row']);
        const content = tbody.closest('#resource-list-content') as HTMLElement;
        content.dataset.roEtag = 'W/"cached-window"';
        content.dataset.roEtagPath = '/clusters/prod/pods/_table';
        const cachedSpacer = document.createElement('tr');
        cachedSpacer.className = 'ro-vspacer';
        cachedSpacer.append(document.createElement('td'));
        tbody.replaceChildren(cachedSpacer);
        dependencies.requestListRefresh.mockImplementationOnce(() => {
            // The forced full-model rebuild must be unconditional at request
            // configuration time, not merely clear the pair afterwards.
            expect(content.dataset.roEtag).toBeUndefined();
            expect(content.dataset.roEtagPath).toBeUndefined();
        });

        virtualizeInit();
        virtualizeInit();

        expect(dependencies.requestListRefresh).toHaveBeenCalledOnce();
        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);
        expect(tbody).toContainElement(cachedSpacer);
    });

    test('allows a replacement cached tbody and resets the one-shot gate after adoption', () => {
        const first = renderList(['first-cached-row']);
        const firstContent = first.closest('#resource-list-content') as HTMLElement;
        const firstSpacer = document.createElement('tr');
        firstSpacer.className = 'ro-vspacer';
        firstSpacer.append(document.createElement('td'));
        first.replaceChildren(firstSpacer);

        virtualizeInit();
        virtualizeInit();
        expect(dependencies.requestListRefresh).toHaveBeenCalledOnce();

        const second = renderList(['second-cached-row']);
        const secondContent = second.closest('#resource-list-content') as HTMLElement;
        const secondSpacer = document.createElement('tr');
        secondSpacer.className = 'ro-vspacer';
        secondSpacer.append(document.createElement('td'));
        second.replaceChildren(secondSpacer);

        virtualizeInit();
        virtualizeInit();
        expect(dependencies.requestListRefresh).toHaveBeenCalledTimes(2);

        const recovered = listFragment(rowKeys(20));
        virtualizePrepareSwap(recovered);
        document.body.replaceChildren(recovered.firstElementChild as Element);
        virtualizeAfterSwap();
        expect(virtualizerActive()).toBe(true);

        // Re-mounting the exact prior cached pair models a later history restore:
        // the successful adoption cleared its old one-shot recovery gate.
        document.body.replaceChildren(secondContent);
        virtualizeInit();

        expect(dependencies.requestListRefresh).toHaveBeenCalledTimes(3);
        expect(firstContent).not.toBe(secondContent);
    });

    test('refetches a cached spacer mounted on a different tbody from the active one', () => {
        renderList(rowKeys(20));
        virtualizeInit();
        expect(virtualizerActive()).toBe(true);
        const cached = renderList(['cached-row']);
        const cachedSpacer = document.createElement('tr');
        cachedSpacer.className = 'ro-vspacer';
        cachedSpacer.append(document.createElement('td'));
        cached.replaceChildren(cachedSpacer);

        virtualizeInit();

        expect(dependencies.requestListRefresh).toHaveBeenCalledOnce();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);
    });

    test('fully disengages when a live list becomes plain, malformed, or empty', () => {
        const engage = () => {
            renderList(['row-0', 'row-1']);
            virtualizeInit();
            expect(virtualizerActive()).toBe(true);
        };

        engage();
        const plain = renderList(['plain-row'], { windowed: false });
        virtualizeInit();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);
        expect(virtVisible()).toStrictEqual([]);
        expect(directRowKeys(plain)).toStrictEqual(['plain-row']);
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 0, end: 0, total: 0 });
        expect(virtualizerSeam().scrollToKey('plain-row')).toBe(false);

        engage();
        document.body.innerHTML = `
            <div id="resource-list-content">
                <div class="ro-table-wrap ro-windowed">
                    <table class="ro-table"><thead><tr><th>Name</th></tr></thead></table>
                </div>
            </div>`;
        virtualizeInit();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);

        engage();
        renderList([]);
        virtualizeInit();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);
    });

    test('reports a detached mount as inactive and leaves its viewport events inert', () => {
        const tbody = renderList(rowKeys(40));
        virtualizeInit();
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();

        tbody.closest('#resource-list-content')?.remove();

        expect(virtualizerActive()).toBe(false);
        expect(virtualizerSeam().scrollToKey('row-0')).toBe(false);
        // If the inactive guard regresses, the changed geometry would re-window
        // the detached tbody and call reapplyRowState a second time.
        scrollYValue = 400;
        window.dispatchEvent(new Event('scroll'));
        expect(animationFrames).toHaveLength(1);
        flushAnimationFrames();
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
    });

    test('uses CSS fallback geometry for a non-positive measured pitch and survives style errors', () => {
        geometryRowHeight = 0;
        document.documentElement.style.setProperty('--row-py', '5px');
        let tbody = renderList(rowKeys(40), { lineHeight: '24px' });

        virtualizeInit();

        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 0, end: 15, total: 40 });
        expect(tbody.querySelector(':scope > tr.ro-vspacer:last-child td')).toHaveStyle({
            height: '875px',
        });

        const computedStyle = vi.spyOn(window, 'getComputedStyle').mockImplementation(() => {
            throw new Error('style engine unavailable');
        });
        tbody = renderList(rowKeys(40));

        expect(() => virtualizeInit()).not.toThrow();
        computedStyle.mockRestore();
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 0, end: 15, total: 40 });
        expect(tbody.querySelector(':scope > tr.ro-vspacer:last-child td')).toHaveStyle({
            height: '925px',
        });
    });

    test('a fresh full render clears any abandoned pending adoption', () => {
        renderList(rowKeys(20));
        virtualizeInit();
        virtualizePrepareSwap(listFragment(rowKeys(25)));

        renderList(rowKeys(30));
        virtualizeInit();
        setVisibleKeys(new Set(['row-29']));
        virtualizeOnFilterChange();

        expect(virtVisible().map((row) => row.dataset.key)).toStrictEqual(['row-29']);
        expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(3);
    });
});

describe('swap adoption', () => {
    test('preserves spacer height, adopts all incoming rows, restores scroll, and flashes changes', () => {
        const oldKeys = rowKeys(60);
        scrollYValue = 600;
        renderList(oldKeys, { changedValues: { 'row-20': 'old value' } });
        virtualizeInit();
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({
            end: 47,
            start: 18,
            total: 60,
        });

        const incomingKeys = rowKeys(65);
        const originalSpacers = Array.from(document.querySelectorAll('tr.ro-vspacer'));
        headerWidths = [140, 200];
        geometryRowHeight = 30;
        const fragment = listFragment(incomingKeys, {
            changedValues: { 'row-20': 'new value' },
        });
        virtualizePrepareSwap(fragment);

        const preparedTbody = fragment.querySelector('tbody') as HTMLTableSectionElement;
        const preparedSpacers = preparedTbody.querySelectorAll(':scope > tr.ro-vspacer');
        expect(preparedTbody.children).toHaveLength(2);
        expect(preparedSpacers[0].firstElementChild).toHaveStyle({ height: '360px' });
        expect(preparedSpacers[1].firstElementChild).toHaveStyle({ height: '940px' });

        document
            .getElementById('resource-list-content')
            ?.replaceWith(fragment.firstElementChild as Element);
        scrollYValue = 100;
        virtualizeAfterSwap();

        expect(scrollToMock).toHaveBeenCalledExactlyOnceWith(0, 600);
        expect(virtualizerActive()).toBe(true);
        expect(virtRows()).toHaveLength(65);
        expect(virtRows().map((row) => row.dataset.key)).toStrictEqual(incomingKeys);
        expect(virtRowByKey('row-64')).toBe(virtRows()[64]);
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({
            end: 47,
            start: 18,
            total: 65,
        });
        const changedCell = document.querySelector('#id-row-20 td:nth-child(2)');
        expect(changedCell).toHaveTextContent('new value');
        expect(changedCell).toHaveClass('ro-cell-changed');
        const liveTable = document.querySelector('table.ro-table');
        expect(liveTable).toHaveClass('ro-virtualized');
        expect(liveTable?.querySelectorAll('th')[1]).toHaveStyle({ width: '180px' });
        expect(Array.from(document.querySelectorAll('tr.ro-vspacer'))).toStrictEqual(
            originalSpacers,
        );
    });

    test('does not animate adopted cell changes when reduced motion is requested', () => {
        reducedMotion = true;
        renderList(['row-0', 'row-1'], { changedValues: { 'row-1': 'before' } });
        virtualizeInit();
        const fragment = listFragment(['row-0', 'row-1'], {
            changedValues: { 'row-1': 'after' },
        });
        virtualizePrepareSwap(fragment);
        document
            .getElementById('resource-list-content')
            ?.replaceWith(fragment.firstElementChild as Element);

        virtualizeAfterSwap();

        const changedCell = document.querySelector('#id-row-1 td:nth-child(2)');
        expect(changedCell).toHaveTextContent('after');
        expect(changedCell).not.toHaveClass('ro-cell-changed');
        expect(window.matchMedia).toHaveBeenCalledWith('(prefers-reduced-motion: reduce)');
    });

    test('cold-adopts a windowed fragment without a redundant correction at the exact fallback pitch', () => {
        document.documentElement.style.setProperty('--row-py', '0.5px');
        const fragment = listFragment(rowKeys(40));

        virtualizePrepareSwap(fragment);

        const prepared = fragment.querySelector('tbody') as HTMLTableSectionElement;
        expect(
            (prepared.querySelector(':scope > tr.ro-vspacer:first-child td') as HTMLElement).style
                .height,
        ).toBe('0px');
        expect(prepared.querySelector(':scope > tr.ro-vspacer:last-child td')).toHaveStyle({
            height: '800px',
        });
        document.body.replaceChildren(fragment.firstElementChild as Element);
        virtualizeAfterSwap();

        expect(virtualizerActive()).toBe(true);
        expect(virtRows()).toHaveLength(40);
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
        expect(window.matchMedia).not.toHaveBeenCalled();
        expect(scrollToMock).not.toHaveBeenCalled();
    });

    test('cold adoption corrects an approximate fallback pitch exactly once', () => {
        const fragment = listFragment(rowKeys(40));
        virtualizePrepareSwap(fragment);
        document.body.replaceChildren(fragment.firstElementChild as Element);

        virtualizeAfterSwap();

        expect(virtualizerActive()).toBe(true);
        expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(2);
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 0, end: 17, total: 40 });
    });

    test('does not correct a cold pitch at the exact half-pixel tolerance', () => {
        document.documentElement.style.setProperty('--row-py', '0.5px');
        const fragment = listFragment(rowKeys(40));
        virtualizePrepareSwap(fragment);
        geometryRowHeight = 20.5;
        document.body.replaceChildren(fragment.firstElementChild as Element);

        virtualizeAfterSwap();

        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 0, end: 17, total: 40 });
    });

    test('remeasures column pins when an adopted table changes its column set', () => {
        renderList(rowKeys(20));
        virtualizeInit();
        const fragment = listFragment(rowKeys(20), { columnCount: 3 });
        virtualizePrepareSwap(fragment);
        document.body.replaceChildren(fragment.firstElementChild as Element);

        virtualizeAfterSwap();

        const headers = document.querySelectorAll('table.ro-table th');
        expect(headers).toHaveLength(3);
        expect(headers[0]).toHaveStyle({ width: '120px' });
        expect(headers[1]).toHaveStyle({ width: '180px' });
        expect(headers[2]).toHaveStyle({ width: '100px' });
        expect(document.querySelector('table.ro-table')).toHaveClass('ro-virtualized');
        expect(document.querySelectorAll('tr.ro-vspacer td')[0]).toHaveAttribute('colspan', '3');
        expect(document.querySelectorAll('tr.ro-vspacer td')[1]).toHaveAttribute('colspan', '3');
    });

    test('disengages after a below-threshold fragment or failed adoption mount', () => {
        renderList(rowKeys(20));
        virtualizeInit();
        const plain = listFragment(['plain'], { windowed: false });
        virtualizePrepareSwap(plain);
        document.body.replaceChildren(plain.firstElementChild as Element);
        virtualizeAfterSwap();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);

        renderList(rowKeys(20));
        virtualizeInit();
        const pending = listFragment(rowKeys(20));
        virtualizePrepareSwap(pending);
        document.body.innerHTML = '<div id="resource-list-content">state block</div>';
        virtualizeAfterSwap();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);
    });

    test('leaves a windowed fragment with no keyed rows untouched', () => {
        const fragment = listFragment([]);
        const tbody = fragment.querySelector('tbody') as HTMLTableSectionElement;
        const unkeyed = tbody.insertRow();
        unkeyed.insertCell().textContent = 'state row';

        virtualizePrepareSwap(fragment);

        expect(tbody.children).toHaveLength(1);
        expect(tbody).toHaveTextContent('state row');
        document.body.replaceChildren(fragment.firstElementChild as Element);
        virtualizeAfterSwap();
        expect(virtualizerActive()).toBe(false);
    });

    test('ignores unkeyed incoming rows and does not animate unchanged, added, or non-data cells', () => {
        renderList(['row-0']);
        virtualizeInit();
        const fragment = listFragment(['row-0', 'row-1'], { columnCount: 3 });
        const incomingBody = fragment.querySelector('tbody') as HTMLTableSectionElement;
        const unkeyed = incomingBody.insertRow();
        unkeyed.insertCell().textContent = 'not an identity row';
        const row0 = incomingBody.querySelector('#id-row-0') as HTMLTableRowElement;
        const replacementHeader = document.createElement('th');
        replacementHeader.textContent = 'changed but not a data cell';
        row0.replaceChild(replacementHeader, row0.children[1]);

        virtualizePrepareSwap(fragment);
        document.body.replaceChildren(fragment.firstElementChild as Element);
        virtualizeAfterSwap();

        expect(virtRows().map((row) => row.dataset.key)).toStrictEqual(['row-0', 'row-1']);
        expect(document.querySelectorAll('.ro-cell-changed')).toHaveLength(0);
    });
});

describe('full-set filtering and focus', () => {
    test('windows over the full model visibility set and exposes detached rows by identity', () => {
        const keys = rowKeys(40);
        const tbody = renderList(keys, {
            filteredKeys: new Set(['row-30', 'row-35']),
        });
        virtualizeInit();
        const detachedRow = virtRowByKey('row-0');

        setVisibleKeys(new Set(['row-30', 'row-35']));
        virtualizeOnFilterChange();

        expect(virtRows()).toHaveLength(40);
        expect(virtVisible().map((row) => row.dataset.key)).toStrictEqual(['row-30', 'row-35']);
        expect(directRowKeys(tbody)).toStrictEqual(['row-30', 'row-35']);
        expect(directRows(tbody)[0]).not.toHaveClass('ro-row-filtered');
        expect(directRows(tbody)[1]).not.toHaveClass('ro-row-filtered');
        expect(virtRowByKey('row-0')).toBe(detachedRow);
        expect(virtRowByKey('missing')).toBeNull();
    });

    test('moves focus across off-DOM rows and scrolls public identity lookups into the window', () => {
        const tbody = renderList(rowKeys(40));
        virtualizeInit();
        focusedKey = 'row-38';

        expect(virtMoveFocus(1)).toBe(true);

        expect(setFocusMock).toHaveBeenCalledExactlyOnceWith('row-39');
        expect(scrollByMock).toHaveBeenCalledExactlyOnceWith(0, 700);
        expect(scrollYValue).toBe(700);
        expect(directRowKeys(tbody)).toContain('row-39');

        expect(virtualizerSeam().scrollToKey('row-0')).toBe(true);
        expect(scrollByMock).toHaveBeenLastCalledWith(0, -700);
        expect(scrollYValue).toBe(0);
        expect(directRowKeys(tbody)).toContain('row-0');
        expect(virtualizerSeam().scrollToKey('missing')).toBe(false);

        setVisibleKeys(new Set());
        virtualizeOnFilterChange();
        expect(virtMoveFocus(1)).toBe(false);
        expect(virtualizerSeam().scrollToKey('row-0')).toBe(false);
    });

    test('starts an unknown focus at the first row and does not scroll a row already in view', () => {
        renderList(rowKeys(20));
        virtualizeInit();
        focusedKey = 'detached-key';

        expect(virtMoveFocus(1)).toBe(true);
        expect(setFocusMock).toHaveBeenCalledExactlyOnceWith('row-0');
        expect(scrollByMock).not.toHaveBeenCalled();
        expect(virtualizerSeam().scrollToKey('row-1')).toBe(true);
        expect(scrollByMock).not.toHaveBeenCalled();
    });

    test('keeps filtering inert while an adoption is pending and when disengaged', () => {
        setVisibleKeys(new Set());
        virtualizeOnFilterChange();
        expect(dependencies.reapplyRowState).not.toHaveBeenCalled();

        renderList(rowKeys(20));
        virtualizeInit();
        const visibleBefore = virtVisible();
        const fragment = listFragment(rowKeys(25));
        virtualizePrepareSwap(fragment);
        setVisibleKeys(new Set(['row-24']));

        virtualizeOnFilterChange();

        expect(virtVisible()).toBe(visibleBefore);
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
    });
});

describe('viewport events', () => {
    test('is inert before engagement', () => {
        window.dispatchEvent(new Event('scroll'));

        expect(animationFrames).toHaveLength(0);
        expect(dependencies.reapplyRowState).not.toHaveBeenCalled();
    });

    test('rerenders when only the window start changes', () => {
        renderList(rowKeys(15));
        virtualizeInit();
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 0, end: 15, total: 15 });

        scrollYValue = 400;
        window.dispatchEvent(new Event('scroll'));
        flushAnimationFrames();

        expect(virtualizerSeam().renderedBounds()).toStrictEqual({ start: 8, end: 15, total: 15 });
        expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(2);
    });

    test('rAF-throttles scrolls, skips unchanged bounds, and reacts to viewport resize', () => {
        const tbody = renderList(rowKeys(80));
        virtualizeInit();
        expect(virtualizerSeam().renderedBounds()).toStrictEqual({
            end: 17,
            start: 0,
            total: 80,
        });

        scrollYValue = 400;
        window.dispatchEvent(new Event('scroll'));
        window.dispatchEvent(new Event('scroll'));

        expect(animationFrames).toHaveLength(1);
        expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
        expect(directRowKeys(tbody)[0]).toBe('row-0');

        flushAnimationFrames();

        expect(virtualizerSeam().renderedBounds()).toStrictEqual({
            end: 37,
            start: 8,
            total: 80,
        });
        expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(2);
        expect(directRowKeys(tbody)[0]).toBe('row-8');

        window.dispatchEvent(new Event('scroll'));
        flushAnimationFrames();
        expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(2);

        viewportHeight = 300;
        window.dispatchEvent(new Event('resize'));

        expect(virtualizerSeam().renderedBounds()).toStrictEqual({
            end: 47,
            start: 8,
            total: 80,
        });
        expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(3);
    });
});
