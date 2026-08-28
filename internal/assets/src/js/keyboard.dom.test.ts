// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const dependencies = vi.hoisted(() => ({
    colsPopOpen: vi.fn(() => false),
    virtMoveFocus: vi.fn(() => false),
    virtRowByKey: vi.fn<(_key: string) => HTMLElement | null>(() => null),
    virtualizerActive: vi.fn(() => false),
    virtVisible: vi.fn<() => HTMLElement[]>(() => []),
}));

vi.mock('./columns.js', () => ({ colsPopOpen: dependencies.colsPopOpen }));
vi.mock('./virtualizer.js', () => ({
    virtMoveFocus: dependencies.virtMoveFocus,
    virtRowByKey: dependencies.virtRowByKey,
    virtualizerActive: dependencies.virtualizerActive,
    virtVisible: dependencies.virtVisible,
}));

import { closeKbdOverlay, keyboardBindings } from './keyboard.js';

interface RowState {
    focusedKey(): string | null;
    setFocus(key: string): void;
}

function binding(event: string): Binding {
    const found = keyboardBindings.find((item) => item.event === event);
    expect(found).toBeDefined();
    return found as Binding;
}

function targetedKey(
    target: EventTarget | null,
    key: string,
    modifiers: KeyboardEventInit = {},
): KeyboardEvent {
    const event = new KeyboardEvent('keydown', {
        bubbles: true,
        cancelable: true,
        key,
        ...modifiers,
    });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function targetedClick(target: EventTarget | null): MouseEvent {
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function renderKeyboard(): { first: HTMLElement; second: HTMLElement; wrap: HTMLElement } {
    document.body.innerHTML = `
        <button id="prior-focus">Prior</button>
        <div id="ro-palette"></div>
        <div id="ro-ctxmenu"></div>
        <div id="namespace-dropdown"></div>
        <div id="resource-list-content">
            <div id="table-wrap" tabindex="0"><table><tbody>
                <tr id="row-1" data-key="one" data-href="#one"><td>one</td></tr>
                <tr id="row-2" data-key="two" data-href="#two"><td>two</td></tr>
                <tr id="row-hidden" class="ro-row-filtered" data-key="hidden" data-href="#hidden"><td>hidden</td></tr>
            </tbody></table></div>
        </div>
        <div id="ro-kbd-overlay" aria-hidden="true">
            <div class="kbd-card" tabindex="-1">Keyboard help</div>
        </div>
    `;
    const first = document.getElementById('row-1') as HTMLElement;
    const second = document.getElementById('row-2') as HTMLElement;
    first.scrollIntoView = vi.fn();
    second.scrollIntoView = vi.fn();
    return {
        first,
        second,
        wrap: document.getElementById('table-wrap') as HTMLElement,
    };
}

beforeEach(() => {
    renderKeyboard();
    closeKbdOverlay();
    let focused: string | null = null;
    const state: RowState = {
        focusedKey: () => focused,
        setFocus: vi.fn((key: string) => {
            focused = key;
        }),
    };
    (window as unknown as { roRowState: RowState }).roRowState = state;
    dependencies.colsPopOpen.mockReturnValue(false);
    dependencies.virtualizerActive.mockReturnValue(false);
    dependencies.virtMoveFocus.mockReturnValue(false);
    dependencies.virtRowByKey.mockReturnValue(null);
    dependencies.virtVisible.mockReturnValue([]);
    window.history.replaceState(null, '', '/');
});

afterEach(() => {
    vi.restoreAllMocks();
});

describe('binding contract', () => {
    test('exports non-stopping click and keydown bindings with undefined returns', () => {
        expect(
            keyboardBindings.map(({ event, selector, stop }) => ({ event, selector, stop })),
        ).toStrictEqual([
            { event: 'click', selector: undefined, stop: undefined },
            { event: 'keydown', selector: undefined, stop: undefined },
        ]);

        expect(binding('click').handler(targetedClick(document.body), null)).toBeUndefined();
        expect(binding('keydown').handler(targetedKey(document.body, 'x'), null)).toBeUndefined();
    });
});

describe('keyboard help overlay', () => {
    test('opens on ?, traps Tab, and restores prior focus on Escape', () => {
        const prior = document.getElementById('prior-focus') as HTMLButtonElement;
        const overlay = document.getElementById('ro-kbd-overlay') as HTMLElement;
        const card = overlay.querySelector('.kbd-card') as HTMLElement;
        prior.focus();
        const open = targetedKey(prior, '?');

        expect(binding('keydown').handler(open, null)).toBeUndefined();

        expect(open.defaultPrevented).toBe(true);
        expect(overlay).toHaveClass('open');
        expect(overlay).toHaveAttribute('aria-hidden', 'false');
        expect(card).toHaveFocus();

        const other = targetedKey(card, 'x');
        binding('keydown').handler(other, null);
        expect(other.defaultPrevented).toBe(false);
        expect(overlay).toHaveClass('open');

        const tab = targetedKey(card, 'Tab');
        binding('keydown').handler(tab, null);
        expect(tab.defaultPrevented).toBe(true);
        expect(overlay).toHaveClass('open');

        const escapeEvent = targetedKey(card, 'Escape');
        binding('keydown').handler(escapeEvent, null);
        expect(escapeEvent.defaultPrevented).toBe(true);
        expect(overlay).not.toHaveClass('open');
        expect(overlay).toHaveAttribute('aria-hidden', 'true');
        expect(prior).toHaveFocus();
    });

    test('? closes an open overlay and restores prior focus', () => {
        const prior = document.getElementById('prior-focus') as HTMLButtonElement;
        const overlay = document.getElementById('ro-kbd-overlay') as HTMLElement;
        const card = overlay.querySelector('.kbd-card') as HTMLElement;
        prior.focus();
        binding('keydown').handler(targetedKey(prior, '?'), null);

        const close = targetedKey(card, '?');
        binding('keydown').handler(close, null);

        expect(close.defaultPrevented).toBe(true);
        expect(overlay).not.toHaveClass('open');
        expect(overlay.getAttribute('aria-hidden')).toBe('true');
        expect(prior).toHaveFocus();
    });

    test('does not try to restore prior focus after that element detaches', () => {
        const prior = document.getElementById('prior-focus') as HTMLButtonElement;
        const overlay = document.getElementById('ro-kbd-overlay') as HTMLElement;
        prior.focus();
        binding('keydown').handler(targetedKey(prior, '?'), null);
        const restoreFocus = vi.spyOn(prior, 'focus');
        restoreFocus.mockClear();
        prior.remove();

        closeKbdOverlay();

        expect(overlay).not.toHaveClass('open');
        expect(restoreFocus).not.toHaveBeenCalled();
    });

    test('closes only when the backdrop itself is clicked', () => {
        const overlay = document.getElementById('ro-kbd-overlay') as HTMLElement;
        const card = overlay.querySelector('.kbd-card') as HTMLElement;
        overlay.classList.add('open');

        binding('click').handler(targetedClick(card), null);
        expect(overlay).toHaveClass('open');

        const text = document.createTextNode('not the backdrop');
        card.append(text);
        expect(() => binding('click').handler(targetedClick(text), null)).not.toThrow();
        expect(overlay).toHaveClass('open');

        expect(binding('click').handler(targetedClick(overlay), null)).toBeUndefined();
        expect(overlay).not.toHaveClass('open');
    });

    test('close is safe without an overlay', () => {
        document.getElementById('ro-kbd-overlay')?.remove();
        expect(() => closeKbdOverlay()).not.toThrow();
    });
});

describe('row keyboard navigation', () => {
    test('j/k walk only visible rows and clamp at both ends', () => {
        const { first, second, wrap } = renderKeyboard();
        let focused: string | null = null;
        const setFocus = vi.fn((key: string) => {
            focused = key;
        });
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => focused,
            setFocus,
        };

        for (const [key, expected] of [
            ['j', 'one'],
            ['j', 'two'],
            ['j', 'two'],
            ['k', 'one'],
            ['k', 'one'],
        ] as const) {
            const event = targetedKey(wrap, key);
            binding('keydown').handler(event, null);
            expect(event.defaultPrevented).toBe(true);
            expect(focused).toBe(expected);
        }

        expect(setFocus).not.toHaveBeenCalledWith('hidden');
        expect(first.scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' });
        expect(second.scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' });
    });

    test('j/k are inert and safe when there are no identity rows', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        document.querySelector('#resource-list-content tbody')?.replaceChildren();
        const setFocus = vi.fn();
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => null,
            setFocus,
        };
        const event = targetedKey(wrap, 'j');

        expect(() => binding('keydown').handler(event, null)).not.toThrow();

        expect(event.defaultPrevented).toBe(false);
        expect(setFocus).not.toHaveBeenCalled();
    });

    test('a detached prior focus starts the rendered walk at the first visible row', () => {
        const { first, wrap } = renderKeyboard();
        let focused: string | null = 'detached';
        const setFocus = vi.fn((key: string) => {
            focused = key;
        });
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => focused,
            setFocus,
        };
        const event = targetedKey(wrap, 'k');

        binding('keydown').handler(event, null);

        expect(event.defaultPrevented).toBe(true);
        expect(setFocus).toHaveBeenCalledExactlyOnceWith('one');
        expect(first.scrollIntoView).toHaveBeenCalledExactlyOnceWith({ block: 'nearest' });
    });

    test('delegates movement to the virtualizer and prevents only a handled key', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        dependencies.virtualizerActive.mockReturnValue(true);
        dependencies.virtMoveFocus.mockReturnValueOnce(true).mockReturnValueOnce(false);

        const down = targetedKey(wrap, 'j');
        binding('keydown').handler(down, null);
        expect(dependencies.virtMoveFocus).toHaveBeenCalledWith(1);
        expect(down.defaultPrevented).toBe(true);

        const up = targetedKey(wrap, 'k');
        binding('keydown').handler(up, null);
        expect(dependencies.virtMoveFocus).toHaveBeenCalledWith(-1);
        expect(up.defaultPrevented).toBe(false);
    });

    test('Enter prefers an already rendered row even while virtualization is active', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => 'two',
            setFocus: vi.fn(),
        };
        const unrelatedVirtualRow = document.createElement('tr');
        unrelatedVirtualRow.dataset.key = 'two';
        unrelatedVirtualRow.dataset.href = '#wrong-virtual-row';
        dependencies.virtualizerActive.mockReturnValue(true);
        dependencies.virtRowByKey.mockReturnValue(unrelatedVirtualRow);
        dependencies.virtVisible.mockReturnValue([unrelatedVirtualRow]);

        const rendered = targetedKey(wrap, 'Enter');
        binding('keydown').handler(rendered, null);

        expect(rendered.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#two');
        expect(dependencies.virtRowByKey).not.toHaveBeenCalled();
    });

    test('Enter ignores a focused filtered row and an absent focus key', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        let focused: string | null = 'hidden';
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => focused,
            setFocus: vi.fn(),
        };

        const hidden = targetedKey(wrap, 'Enter');
        binding('keydown').handler(hidden, null);
        expect(hidden.defaultPrevented).toBe(false);
        expect(window.location.hash).toBe('');

        focused = null;
        const absent = targetedKey(wrap, 'Enter');
        binding('keydown').handler(absent, null);
        expect(absent.defaultPrevented).toBe(false);
        expect(window.location.hash).toBe('');
    });

    test('Enter opens a detached virtual row only when it is logically visible', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        const detached = document.createElement('tr');
        detached.dataset.key = 'virtual';
        detached.dataset.href = '#virtual';
        const firstVirtual = document.createElement('tr');
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => 'virtual',
            setFocus: vi.fn(),
        };
        dependencies.virtualizerActive.mockReturnValue(true);
        dependencies.virtRowByKey.mockReturnValue(detached);
        dependencies.virtVisible.mockReturnValue([firstVirtual, detached]);

        const virtual = targetedKey(wrap, 'Enter');
        binding('keydown').handler(virtual, null);

        expect(virtual.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#virtual');

        window.history.replaceState(null, '', '/');
        dependencies.virtVisible.mockReturnValue([firstVirtual]);
        const filteredVirtual = targetedKey(wrap, 'Enter');
        binding('keydown').handler(filteredVirtual, null);
        expect(filteredVirtual.defaultPrevented).toBe(false);
        expect(window.location.hash).toBe('');
    });

    test.each(['input', 'textarea', 'select'] as const)(
        'does not hijack j from a focused %s',
        (tag) => {
            const control = document.createElement(tag);
            document.body.append(control);
            const event = targetedKey(control, 'j');

            binding('keydown').handler(event, null);

            expect(event.defaultPrevented).toBe(false);
            expect(
                (window as unknown as { roRowState: RowState }).roRowState.focusedKey(),
            ).toBeNull();
        },
    );

    test('does not hijack j from a contenteditable surface', () => {
        const editor = document.createElement('div');
        Object.defineProperty(editor, 'isContentEditable', {
            configurable: true,
            value: true,
        });
        document.body.append(editor);
        const event = targetedKey(editor, 'j');

        binding('keydown').handler(event, null);

        expect(event.defaultPrevented).toBe(false);
        expect((window as unknown as { roRowState: RowState }).roRowState.focusedKey()).toBeNull();
    });

    test.each([
        ['meta', { metaKey: true }],
        ['control', { ctrlKey: true }],
        ['alt', { altKey: true }],
    ] as const)('does not hijack a %s-modified j chord', (_name, modifiers) => {
        const event = targetedKey(document.body, 'j', modifiers);

        binding('keydown').handler(event, null);

        expect(event.defaultPrevented).toBe(false);
        expect((window as unknown as { roRowState: RowState }).roRowState.focusedKey()).toBeNull();
    });

    test.each(['a', 'button', 'summary'] as const)(
        'does not hijack Enter from a descendant of %s',
        (tag) => {
            (window as unknown as { roRowState: RowState }).roRowState = {
                focusedKey: () => 'two',
                setFocus: vi.fn(),
            };
            const control = document.createElement(tag);
            const child = document.createElement('span');
            control.append(child);
            document.body.append(control);
            const event = targetedKey(child, 'Enter');

            binding('keydown').handler(event, null);

            expect(event.defaultPrevented).toBe(false);
            expect(window.location.hash).toBe('');
        },
    );

    test('handles non-Element targets without losing row navigation', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        const text = document.createTextNode('plain text target');
        wrap.append(text);
        let focused: string | null = null;
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => focused,
            setFocus: (key) => {
                focused = key;
            },
        };

        const move = targetedKey(text, 'j');
        expect(() => binding('keydown').handler(move, null)).not.toThrow();
        expect(move.defaultPrevented).toBe(true);
        expect(focused).toBe('one');

        focused = 'two';
        const enter = targetedKey(text, 'Enter');
        expect(() => binding('keydown').handler(enter, null)).not.toThrow();
        expect(enter.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#two');
    });

    test('does not treat an unrelated key as Enter when a row is focused', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => 'two',
            setFocus: vi.fn(),
        };
        const event = targetedKey(wrap, 'x');

        binding('keydown').handler(event, null);

        expect(event.defaultPrevented).toBe(false);
        expect(window.location.hash).toBe('');
    });

    test('does not hijack keys while another surface is open', () => {
        const surfaces = [
            document.getElementById('ro-palette'),
            document.getElementById('ro-ctxmenu'),
            document.getElementById('namespace-dropdown'),
        ];
        const classes = ['open', 'is-open', 'is-active'];
        surfaces.forEach((surface, index) => {
            surface?.classList.add(classes[index]);
            const event = targetedKey(document.body, 'j');
            binding('keydown').handler(event, null);
            expect(event.defaultPrevented).toBe(false);
            surface?.classList.remove(classes[index]);
        });

        dependencies.colsPopOpen.mockReturnValue(true);
        const columns = targetedKey(document.body, 'j');
        binding('keydown').handler(columns, null);
        expect(columns.defaultPrevented).toBe(false);

        expect((window as unknown as { roRowState: RowState }).roRowState.focusedKey()).toBeNull();
    });
});
