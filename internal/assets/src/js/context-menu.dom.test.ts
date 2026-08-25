// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const rowSelection = vi.hoisted(() => ({
    lastKeySegment: vi.fn((key: string) => key.split('/').pop() || ''),
    roCopyText: vi.fn<(text: string, done: (ok: boolean) => void) => void>(),
}));

vi.mock('./row-selection.js', () => ({
    lastKeySegment: rowSelection.lastKeySegment,
    roCopyText: rowSelection.roCopyText,
}));

import { closeRowMenu, contextMenuBindings, openRowMenu } from './context-menu.js';

function binding(event: string, selector?: string): Binding {
    const found = contextMenuBindings.find(
        (candidate) => candidate.event === event && candidate.selector === selector,
    );
    expect(found).toBeDefined();
    return found as Binding;
}

function targetedMouseEvent(type: string, target: Element, x = 0, y = 0): MouseEvent {
    const event = new MouseEvent(type, {
        bubbles: true,
        cancelable: true,
        clientX: x,
        clientY: y,
    });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function renderMenu(): HTMLElement {
    document.body.innerHTML = `
        <div id="resource-list-content">
            <table><tbody><tr id="row" data-key="prod/default/api-0"
                data-name="api-full-name" data-href="#details" data-yaml="#yaml"
                data-download="#download"><td><span id="cell">api</span></td></tr></tbody></table>
        </div>
        <div id="ro-ctxmenu" aria-hidden="true">
            <button data-ro-action="open">Open</button>
            <button data-ro-action="yaml">View YAML</button>
            <button data-ro-action="logs" data-href="#stale">View logs</button>
            <button data-ro-action="download">Download YAML</button>
            <button data-ro-action="copy" data-href="#must-not-navigate">Copy name</button>
        </div>
    `;
    return document.getElementById('ro-ctxmenu') as HTMLElement;
}

beforeEach(() => {
    renderMenu();
    window.history.replaceState(null, '', '/');
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 800 });
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 600 });
});

describe('context menu state and row binding', () => {
    test('binds row labels and hrefs, hides unavailable actions, and clamps to the viewport', () => {
        const menu = document.getElementById('ro-ctxmenu') as HTMLElement;
        const row = document.getElementById('row') as HTMLElement;

        openRowMenu(row, 790, 590);

        expect(menu).toHaveClass('is-open');
        expect(menu).toHaveAttribute('aria-hidden', 'false');
        expect(menu).toHaveStyle({ left: '580px', top: '360px' });
        expect(menu.dataset.name).toBe('api-full-name');
        expect(menu.querySelector('[data-ro-action="open"]')).toHaveAttribute(
            'data-href',
            '#details',
        );
        expect(menu.querySelector('[data-ro-action="yaml"]')).toHaveAttribute('data-href', '#yaml');
        expect(menu.querySelector('[data-ro-action="download"]')).toHaveAttribute(
            'data-href',
            '#download',
        );
        expect(menu.querySelector('[data-ro-action="logs"]')).toHaveAttribute('hidden');
        expect(menu.querySelector('[data-ro-action="logs"]')).not.toHaveAttribute('data-href');

        openRowMenu(row, -20, -40);
        expect(menu).toHaveStyle({ left: '8px', top: '8px' });
    });

    test('falls back to the identity-key tail when the row has no explicit name', () => {
        const row = document.getElementById('row') as HTMLElement;
        delete row.dataset.name;

        openRowMenu(row, 20, 20);

        expect(rowSelection.lastKeySegment).toHaveBeenCalledExactlyOnceWith('prod/default/api-0');
        expect(document.getElementById('ro-ctxmenu')?.dataset.name).toBe('api-0');
    });

    test('closeRowMenu is idempotent and synchronizes class with aria-hidden', () => {
        const menu = document.getElementById('ro-ctxmenu') as HTMLElement;
        openRowMenu(document.getElementById('row') as HTMLElement, 20, 20);

        closeRowMenu();
        closeRowMenu();

        expect(menu).not.toHaveClass('is-open');
        expect(menu).toHaveAttribute('aria-hidden', 'true');
    });

    test('right-click opens only for an identity row and prevents only the handled event', () => {
        const cell = document.getElementById('cell') as HTMLElement;
        const contextBinding = binding('contextmenu');
        const rowEvent = targetedMouseEvent('contextmenu', cell, 123, 234);

        contextBinding.handler(rowEvent, null);

        expect(rowEvent.defaultPrevented).toBe(true);
        expect(document.getElementById('ro-ctxmenu')).toHaveClass('is-open');
        expect(document.getElementById('ro-ctxmenu')).toHaveStyle({ left: '123px', top: '234px' });

        const outside = document.createElement('div');
        document.body.appendChild(outside);
        const outsideEvent = targetedMouseEvent('contextmenu', outside);
        contextBinding.handler(outsideEvent, null);

        expect(outsideEvent.defaultPrevented).toBe(false);
        expect(document.getElementById('ro-ctxmenu')).not.toHaveClass('is-open');
    });
});

describe('context menu actions', () => {
    test('copy uses the bound full name, closes first, and never follows a stray href', () => {
        const menu = document.getElementById('ro-ctxmenu') as HTMLElement;
        const row = document.getElementById('row') as HTMLElement;
        openRowMenu(row, 20, 20);
        const copy = menu.querySelector('[data-ro-action="copy"]') as HTMLElement;
        const event = targetedMouseEvent('click', copy);

        expect(binding('click', '#ro-ctxmenu [data-ro-action]').handler(event, copy)).toBe(true);

        expect(event.defaultPrevented).toBe(true);
        expect(menu).not.toHaveClass('is-open');
        expect(rowSelection.roCopyText).toHaveBeenCalledOnce();
        expect(rowSelection.roCopyText.mock.calls[0][0]).toBe('api-full-name');
        expect(rowSelection.roCopyText.mock.calls[0][1]).toBeTypeOf('function');
        expect(window.location.hash).toBe('');
    });

    test('a bound navigation action follows its href after closing the menu', () => {
        const menu = document.getElementById('ro-ctxmenu') as HTMLElement;
        openRowMenu(document.getElementById('row') as HTMLElement, 20, 20);
        const yaml = menu.querySelector('[data-ro-action="yaml"]') as HTMLElement;
        const event = targetedMouseEvent('click', yaml);

        binding('click', '#ro-ctxmenu [data-ro-action]').handler(event, yaml);

        expect(menu).not.toHaveClass('is-open');
        expect(window.location.hash).toBe('#yaml');
        expect(rowSelection.roCopyText).not.toHaveBeenCalled();
    });

    test('an action without an href closes safely without navigating', () => {
        const menu = document.getElementById('ro-ctxmenu') as HTMLElement;
        openRowMenu(document.getElementById('row') as HTMLElement, 20, 20);
        window.location.hash = '#before';
        const logs = menu.querySelector('[data-ro-action="logs"]') as HTMLElement;

        binding('click', '#ro-ctxmenu [data-ro-action]').handler(
            targetedMouseEvent('click', logs),
            logs,
        );

        expect(menu).not.toHaveClass('is-open');
        expect(window.location.hash).toBe('#before');
        expect(rowSelection.roCopyText).not.toHaveBeenCalled();
    });

    test('the unconditional click and Escape bindings dismiss without stopping sibling work', () => {
        const menu = document.getElementById('ro-ctxmenu') as HTMLElement;
        openRowMenu(document.getElementById('row') as HTMLElement, 20, 20);
        const dismiss = binding('click');

        expect(dismiss.stop).not.toBe(true);
        expect(dismiss.handler(new MouseEvent('click'), null)).toBeUndefined();
        expect(menu).not.toHaveClass('is-open');

        openRowMenu(document.getElementById('row') as HTMLElement, 20, 20);
        binding('keydown').handler(new KeyboardEvent('keydown', { key: 'Escape' }), null);
        expect(menu).not.toHaveClass('is-open');
    });
});
