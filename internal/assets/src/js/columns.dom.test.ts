// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const dependencies = vi.hoisted(() => ({
    issueFilterNavigation: vi.fn(),
    requestListRefresh: vi.fn(),
    roPrefsSetHiddenColumns: vi.fn(),
}));

vi.mock('./filters.js', () => ({
    issueFilterNavigation: dependencies.issueFilterNavigation,
}));
vi.mock('./prefs.js', () => ({
    roPrefsSetHiddenColumns: dependencies.roPrefsSetHiddenColumns,
}));
vi.mock('./refresh.js', () => ({
    requestListRefresh: dependencies.requestListRefresh,
}));

import { colsPopOpen, columnsBindings, setColsPopOpen, syncColsPopState } from './columns.js';

function binding(selector: string | undefined, event = 'click'): Binding {
    const found = columnsBindings.find(
        (item) => item.event === event && item.selector === selector,
    );
    expect(found).toBeDefined();
    return found as Binding;
}

function targetedEvent(type: string, target: Element): Event {
    const event = new Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function renderPopover(): HTMLElement {
    document.body.innerHTML = `
        <button id="ro-cols-btn" data-ro-cols-toggle aria-expanded="false">Columns</button>
        <div id="ro-cols-pop" class="ro-pop" data-plural="pods">
            <button data-ro-action="toggle-column" data-col="Name" disabled>
                <input class="ro-check" type="checkbox">
            </button>
            <button id="toggle-status" data-ro-action="toggle-column" data-col="Status">
                <input class="ro-check" type="checkbox" checked>
            </button>
            <button data-ro-action="toggle-column" data-col="Node">
                <input class="ro-check" type="checkbox">
            </button>
        </div>
        <div id="resource-list-content"></div>
    `;
    return document.getElementById('ro-cols-pop') as HTMLElement;
}

describe('column popover state', () => {
    beforeEach(() => {
        renderPopover();
        setColsPopOpen(false);
        delete (window as unknown as { htmx?: unknown }).htmx;
    });

    test('keeps class and aria-expanded in sync', () => {
        setColsPopOpen(true);

        expect(colsPopOpen()).toBe(true);
        expect(document.getElementById('ro-cols-pop')).toHaveClass('is-open');
        expect(document.getElementById('ro-cols-btn')).toHaveAttribute('aria-expanded', 'true');

        setColsPopOpen(false);
        expect(document.getElementById('ro-cols-pop')).not.toHaveClass('is-open');
        expect(document.getElementById('ro-cols-btn')).toHaveAttribute('aria-expanded', 'false');
    });

    test('re-derives state from freshly rendered DOM', () => {
        document.getElementById('ro-cols-pop')?.classList.add('is-open');
        syncColsPopState();
        expect(colsPopOpen()).toBe(true);

        document.getElementById('ro-cols-pop')?.remove();
        syncColsPopState();
        expect(colsPopOpen()).toBe(false);
    });

    test('opener toggles from the DOM state and prevents navigation', () => {
        const opener = document.getElementById('ro-cols-btn') as HTMLElement;
        const event = targetedEvent('click', opener);

        binding('[data-ro-cols-toggle]').handler(event, opener);

        expect(event.defaultPrevented).toBe(true);
        expect(colsPopOpen()).toBe(true);
        expect(document.getElementById('ro-cols-pop')).toHaveClass('is-open');
    });

    test('outside click closes while clicks inside the popover do not', () => {
        const pop = document.getElementById('ro-cols-pop') as HTMLElement;
        const outside = document.createElement('button');
        document.body.appendChild(outside);
        const outsideBinding = binding(undefined);
        setColsPopOpen(true);

        outsideBinding.handler(targetedEvent('click', pop), null);
        expect(colsPopOpen()).toBe(true);

        outsideBinding.handler(targetedEvent('click', outside), null);
        expect(colsPopOpen()).toBe(false);
    });
});

describe('column visibility commits', () => {
    beforeEach(() => {
        renderPopover();
        delete (window as unknown as { htmx?: unknown }).htmx;
    });

    test('persists the complete hidden set, aborts stale work, then refreshes', () => {
        const trigger = vi.fn();
        (window as unknown as { htmx: { trigger: typeof trigger } }).htmx = { trigger };
        const toggle = document.getElementById('toggle-status') as HTMLElement;
        const check = toggle.querySelector('.ro-check') as HTMLInputElement;
        const event = targetedEvent('click', toggle);

        expect(binding('[data-ro-action="toggle-column"]').handler(event, toggle)).toBe(true);

        expect(check).not.toBeChecked();
        expect(event.defaultPrevented).toBe(true);
        expect(dependencies.roPrefsSetHiddenColumns).toHaveBeenCalledExactlyOnceWith('pods', [
            'Status',
            'Node',
        ]);
        expect(trigger).toHaveBeenCalledExactlyOnceWith(
            document.getElementById('resource-list-content'),
            'htmx:abort',
        );
        expect(dependencies.requestListRefresh).toHaveBeenCalledOnce();
        expect(dependencies.roPrefsSetHiddenColumns.mock.invocationCallOrder[0]).toBeLessThan(
            trigger.mock.invocationCallOrder[0] as number,
        );
        expect(trigger.mock.invocationCallOrder[0]).toBeLessThan(
            dependencies.requestListRefresh.mock.invocationCallOrder[0] as number,
        );
    });

    test('does not persist a popover without a resource plural', () => {
        const pop = renderPopover();
        delete pop.dataset.plural;
        const toggle = document.getElementById('toggle-status') as HTMLElement;

        binding('[data-ro-action="toggle-column"]').handler(targetedEvent('click', toggle), toggle);

        expect(dependencies.roPrefsSetHiddenColumns).not.toHaveBeenCalled();
        expect(dependencies.requestListRefresh).not.toHaveBeenCalled();
    });
});

describe('column form navigation', () => {
    test('merges visible fields into the live query without rewriting filter commas', () => {
        window.history.replaceState(
            null,
            '',
            '/pods?f=status:Running,Pending&labelcols=old&selector=app%3Dold&sort=Name',
        );
        document.body.innerHTML = `
            <form class="ro-pop-form">
                <input type="hidden" name="f" value="stale-hidden-copy">
                <input name="labelcols" value="metadata.name">
                <input name="selector" value="">
            </form>
        `;
        const form = document.querySelector('form') as HTMLFormElement;
        const event = targetedEvent('submit', form);

        expect(binding('form.ro-pop-form', 'submit').handler(event, form)).toBe(true);

        expect(event.defaultPrevented).toBe(true);
        expect(dependencies.issueFilterNavigation).toHaveBeenCalledExactlyOnceWith(
            '/pods?f=status:Running,Pending&sort=Name&labelcols=metadata.name',
        );
    });
});
