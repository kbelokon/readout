// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

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
    target: Element,
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

function targetedClick(target: Element): MouseEvent {
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

describe('keyboard help overlay', () => {
    test('opens on ?, traps Tab, and restores prior focus on Escape', () => {
        const prior = document.getElementById('prior-focus') as HTMLButtonElement;
        const overlay = document.getElementById('ro-kbd-overlay') as HTMLElement;
        const card = overlay.querySelector('.kbd-card') as HTMLElement;
        prior.focus();
        const open = targetedKey(prior, '?');

        binding('keydown').handler(open, null);

        expect(open.defaultPrevented).toBe(true);
        expect(overlay).toHaveClass('open');
        expect(overlay).toHaveAttribute('aria-hidden', 'false');
        expect(card).toHaveFocus();

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

    test('closes only when the backdrop itself is clicked', () => {
        const overlay = document.getElementById('ro-kbd-overlay') as HTMLElement;
        const card = overlay.querySelector('.kbd-card') as HTMLElement;
        overlay.classList.add('open');

        binding('click').handler(targetedClick(card), null);
        expect(overlay).toHaveClass('open');

        binding('click').handler(targetedClick(overlay), null);
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

    test('Enter opens the focused rendered or virtual row, never a filtered row', () => {
        const wrap = document.getElementById('table-wrap') as HTMLElement;
        let focused: string | null = 'two';
        (window as unknown as { roRowState: RowState }).roRowState = {
            focusedKey: () => focused,
            setFocus: vi.fn(),
        };

        const rendered = targetedKey(wrap, 'Enter');
        binding('keydown').handler(rendered, null);
        expect(rendered.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#two');

        focused = 'hidden';
        window.history.replaceState(null, '', '/');
        const hidden = targetedKey(wrap, 'Enter');
        binding('keydown').handler(hidden, null);
        expect(hidden.defaultPrevented).toBe(false);
        expect(window.location.hash).toBe('');

        const detached = document.createElement('tr');
        detached.dataset.key = 'virtual';
        detached.dataset.href = '#virtual';
        focused = 'virtual';
        dependencies.virtualizerActive.mockReturnValue(true);
        dependencies.virtRowByKey.mockReturnValue(detached);
        dependencies.virtVisible.mockReturnValue([detached]);
        const virtual = targetedKey(wrap, 'Enter');
        binding('keydown').handler(virtual, null);
        expect(virtual.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#virtual');
    });

    test('does not hijack text entry, chords, controls, or open surfaces', () => {
        const input = document.createElement('input');
        const button = document.createElement('button');
        document.body.append(input, button);
        const inertEvents = [
            targetedKey(input, 'j'),
            targetedKey(document.body, 'j', { metaKey: true }),
            targetedKey(button, 'Enter'),
        ];
        for (const event of inertEvents) {
            binding('keydown').handler(event, null);
            expect(event.defaultPrevented).toBe(false);
        }

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
