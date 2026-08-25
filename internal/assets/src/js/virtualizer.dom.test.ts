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
    filteredKeys?: ReadonlySet<string>;
    windowed?: boolean;
}

const ROW_HEIGHT = 20;
const HEADER_WIDTHS = [120, 180];

let animationFrames: FrameRequestCallback[];
let focusedKey: string | null;
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
    header.append(document.createElement('th'), document.createElement('th'));
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
        const value = row.insertCell();
        value.textContent = options.changedValues?.[key] ?? `value-${key}`;
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
            return rect(0, HEADER_WIDTHS[index] ?? 100, ROW_HEIGHT);
        }
        if (element.tagName === 'TBODY') {
            return rect(tbodyDocumentTop - scrollYValue, 300, ROW_HEIGHT);
        }
        if (element.matches('tr[data-key]')) {
            const index = Number(element.dataset.testIndex ?? 0);
            return rect(tbodyDocumentTop - scrollYValue + index * ROW_HEIGHT, 300, ROW_HEIGHT);
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
    reducedMotion = false;
    scrollYValue = 0;
    tbodyDocumentTop = 0;
    viewportHeight = 100;
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
        const cachedSpacer = document.createElement('tr');
        cachedSpacer.className = 'ro-vspacer';
        cachedSpacer.append(document.createElement('td'));
        tbody.replaceChildren(cachedSpacer);

        virtualizeInit();

        expect(dependencies.requestListRefresh).toHaveBeenCalledOnce();
        expect(virtualizerActive()).toBe(false);
        expect(virtRows()).toStrictEqual([]);
        expect(tbody).toContainElement(cachedSpacer);
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
});

describe('viewport events', () => {
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
