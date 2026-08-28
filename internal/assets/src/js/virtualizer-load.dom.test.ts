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

afterEach(() => {
    document.body.replaceChildren();
    Reflect.deleteProperty(document, 'fonts');
    vi.restoreAllMocks();
});

test('registers passive viewport hooks and remeasures only an active changed font pitch', async () => {
    vi.resetModules();
    let fontReady: (() => void) | undefined;
    const then = vi.fn((callback: () => void) => {
        fontReady = callback;
    });
    Object.defineProperty(document, 'fonts', {
        configurable: true,
        value: { ready: { then } },
    });
    const addEventListener = vi.spyOn(window, 'addEventListener');
    const requestAnimationFrame = vi.fn();
    Object.defineProperty(window, 'requestAnimationFrame', {
        configurable: true,
        value: requestAnimationFrame,
    });
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 100 });
    Object.defineProperty(window, 'matchMedia', {
        configurable: true,
        value: vi.fn(() => ({ matches: false })),
    });
    let rowHeight = 20;
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
        this: Element,
    ) {
        const element = this as HTMLElement;
        const index = Number(element.dataset.testIndex ?? 0);
        return {
            bottom: element.matches('tr[data-key]') ? index * rowHeight + rowHeight : rowHeight,
            height: rowHeight,
            left: 0,
            right: 100,
            top: element.matches('tr[data-key]') ? index * rowHeight : 0,
            width: 100,
            x: 0,
            y: 0,
            toJSON: () => ({}),
        } as DOMRect;
    });
    window.roRowModel = { fields: [], rows: [], visibleKeys: null };
    window.roRowState = {
        setSelected: vi.fn(),
        setFocus: vi.fn(),
        focusedKey: () => null,
        clear: vi.fn(),
        selectedKeys: () => [],
        selectedEntries: () => [],
    };

    const virtualizer = await import('./virtualizer.js');

    // Passive scroll handling is itself the performance contract; resize is
    // proven below by dispatching the real window event and observing a re-window.
    expect(addEventListener).toHaveBeenCalledWith('scroll', expect.any(Function), {
        passive: true,
    });
    expect(then).toHaveBeenCalledOnce();
    expect(fontReady).toBeTypeOf('function');
    expect(Object.keys(window.roVirtual)).toStrictEqual([
        'active',
        'renderedBounds',
        'scrollToKey',
    ]);

    window.dispatchEvent(new Event('scroll'));
    expect(requestAnimationFrame).not.toHaveBeenCalled();
    expect(() => fontReady?.()).not.toThrow();

    document.body.innerHTML = `
        <div id="resource-list-content">
            <div class="ro-table-wrap ro-windowed">
                <table class="ro-table">
                    <thead><tr><th>Name</th><th>Value</th></tr></thead>
                    <tbody>${Array.from(
                        { length: 30 },
                        (_, index) =>
                            `<tr data-key="row-${index}" data-test-index="${index}"><td>row-${index}</td><td>value-${index}</td></tr>`,
                    ).join('')}</tbody>
                </table>
            </div>
        </div>`;
    virtualizer.virtualizeInit();
    expect(dependencies.reapplyRowState).toHaveBeenCalledOnce();
    expect(window.roVirtual.renderedBounds()).toStrictEqual({ start: 0, end: 17, total: 30 });

    window.dispatchEvent(new Event('scroll'));
    expect(requestAnimationFrame).toHaveBeenCalledOnce();

    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 200 });
    window.dispatchEvent(new Event('resize'));
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(2);
    expect(window.roVirtual.renderedBounds()).toStrictEqual({ start: 0, end: 22, total: 30 });

    rowHeight = 30;
    fontReady?.();
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(3);
    expect(window.roVirtual.renderedBounds()).toStrictEqual({ start: 0, end: 19, total: 30 });

    fontReady?.();
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(3);

    rowHeight = 30.5;
    fontReady?.();
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(3);

    rowHeight = 0;
    fontReady?.();
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(3);

    window.roRowModel.visibleKeys = new Set();
    virtualizer.virtualizeOnFilterChange();
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(4);
    expect(() => fontReady?.()).not.toThrow();
    expect(dependencies.reapplyRowState).toHaveBeenCalledTimes(4);
});

test.each([
    ['no FontFaceSet', undefined],
    ['no ready member', {}],
    ['non-thenable ready member', { ready: 42 }],
])('loads safely with %s', async (_case, fonts) => {
    vi.resetModules();
    if (fonts === undefined) {
        Reflect.deleteProperty(document, 'fonts');
    } else {
        Object.defineProperty(document, 'fonts', { configurable: true, value: fonts });
    }

    const virtualizer = await import('./virtualizer.js');
    expect(virtualizer.virtualizerActive()).toBe(false);
});
