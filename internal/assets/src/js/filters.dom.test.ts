// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const virtualizer = vi.hoisted(() => ({
    virtualizeOnFilterChange: vi.fn(),
    virtualizerActive: vi.fn(() => false),
}));

vi.mock('./virtualizer.js', () => virtualizer);

let filters: typeof import('./filters.js');

function renderEditor(chips = ''): {
    autocomplete: HTMLElement;
    content: HTMLElement;
    error: HTMLElement;
    input: HTMLInputElement;
} {
    document.body.innerHTML = `
        <div id="resource-list-content">
            <div id="ro-filter-field">
                ${chips}
                <input id="ro-filter-input">
                <div id="ro-filter-error" hidden></div>
                <div id="ro-filter-ac" hidden></div>
            </div>
            <table class="ro-table">
                <thead>
                    <tr>
                        <th data-hint="string">Name</th>
                        <th data-hint="enum">Status</th>
                        <th>Created</th>
                    </tr>
                </thead>
                <tbody>
                    <tr data-key="dev/pods/web-alpha">
                        <td class="cell-name"><a>Web Alpha</a></td>
                        <td data-col="status">Running</td>
                        <td>1m</td>
                    </tr>
                    <tr data-key="dev/pods/worker-beta">
                        <td class="cell-name"><a>Worker Beta</a></td>
                        <td data-col="status">Pending</td>
                        <td>2m</td>
                    </tr>
                </tbody>
            </table>
        </div>
    `;

    return {
        autocomplete: document.getElementById('ro-filter-ac') as HTMLElement,
        content: document.getElementById('resource-list-content') as HTMLElement,
        error: document.getElementById('ro-filter-error') as HTMLElement,
        input: document.getElementById('ro-filter-input') as HTMLInputElement,
    };
}

function binding(event: string, selector?: string): Binding {
    const found = filters.filtersBindings.find(
        (candidate) => candidate.event === event && candidate.selector === selector,
    );
    expect(found).toBeDefined();
    return found as Binding;
}

function targetedClick(target: Element): MouseEvent {
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function targetedKey(target: Element, key: string): KeyboardEvent {
    const event = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, key });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function installHtmx(): ReturnType<typeof vi.fn> {
    const ajax = vi.fn(() => Promise.resolve());
    vi.stubGlobal('htmx', { ajax });
    return ajax;
}

beforeEach(async () => {
    vi.resetModules();
    virtualizer.virtualizerActive.mockReturnValue(false);
    filters = await import('./filters.js');
});

afterEach(() => {
    vi.unstubAllGlobals();
});

describe('complete row-model capture', () => {
    test('captures field labels/names and keyed rows with link and cell-name fallbacks', () => {
        const template = document.createElement('template');
        template.innerHTML = `
            <table class="ro-table">
                <thead><tr>
                    <th data-hint="string"> Name </th>
                    <th data-hint="map"> App Label </th>
                    <th> Created </th>
                </tr></thead>
                <tbody>
                    <tr data-key="dev/pods/web-alpha">
                        <td class="cell-name"><a> Web Alpha </a></td>
                        <td> frontend </td>
                        <td> 1m </td>
                    </tr>
                    <tr data-key="dev/pods/fallback">
                        <td> Fallback Name </td>
                        <td> worker </td>
                        <td> 2m </td>
                    </tr>
                    <tr data-key="dev/pods/empty"></tr>
                    <tr><td>Unkeyed row</td><td>ignored</td><td>3m</td></tr>
                </tbody>
            </table>
        `;

        filters.captureRowModel(template.content);

        expect(window.roRowModel).toStrictEqual({
            fields: [
                { label: 'Name', name: 'name', hint: 'string' },
                { label: 'App Label', name: 'app-label', hint: 'map' },
                { label: 'Created', name: 'created', hint: '' },
            ],
            rows: [
                {
                    key: 'dev/pods/web-alpha',
                    name: 'Web Alpha',
                    cells: ['Web Alpha', 'frontend', '1m'],
                },
                {
                    key: 'dev/pods/fallback',
                    name: 'Fallback Name',
                    cells: ['Fallback Name', 'worker', '2m'],
                },
                { key: 'dev/pods/empty', name: '', cells: [] },
            ],
            visibleKeys: null,
        });
    });

    test('clears stale fields and rows when the incoming fragment has no table', () => {
        const { content } = renderEditor();
        filters.captureRowModel(content);
        expect(window.roRowModel.rows).toHaveLength(2);

        filters.captureRowModel(document.createDocumentFragment());

        expect(window.roRowModel.fields).toStrictEqual([]);
        expect(window.roRowModel.rows).toStrictEqual([]);
    });

    test('normalizes empty header and row text without inventing model values', () => {
        const template = document.createElement('template');
        template.innerHTML = `
            <table class="ro-table">
                <thead><tr><th data-hint="string"></th></tr></thead>
                <tbody><tr data-key="dev/pods/empty"><td class="cell-name"><a></a></td></tr></tbody>
            </table>
        `;

        filters.captureRowModel(template.content);

        expect(window.roRowModel.fields).toStrictEqual([{ label: '', name: '', hint: 'string' }]);
        expect(window.roRowModel.rows).toStrictEqual([
            { key: 'dev/pods/empty', name: '', cells: [''] },
        ]);
    });

    test('header capture trims only Go unicode.IsSpace and preserves U+FEFF data', () => {
        const template = document.createElement('template');
        template.innerHTML = `
            <table class="ro-table">
                <thead><tr>
                    <th data-hint="enum">\u0085 Workload\u0085 Status \u0085</th>
                    <th data-hint="enum">\uFEFFBuild\uFEFFState\uFEFF</th>
                </tr></thead>
                <tbody>
                    <tr data-key="dev/pods/odd">
                        <td class="cell-name"><a>﻿api﻿</a></td>
                        <td>﻿Ready﻿</td>
                    </tr>
                </tbody>
            </table>
        `;

        filters.captureRowModel(template.content);

        expect(window.roRowModel.fields).toStrictEqual([
            { label: 'Workload Status', name: 'workload-status', hint: 'enum' },
            {
                label: '\uFEFFBuild\uFEFFState\uFEFF',
                name: '\uFEFFbuild\uFEFFstate\uFEFF',
                hint: 'enum',
            },
        ]);
        expect(window.roRowModel.rows).toStrictEqual([
            {
                key: 'dev/pods/odd',
                name: '\uFEFFapi\uFEFF',
                cells: ['\uFEFFapi\uFEFF', '\uFEFFReady\uFEFF'],
            },
        ]);
    });

    test('does not replace the complete model with a virtualized DOM window', () => {
        const { content } = renderEditor();
        filters.captureRowModelFromDocument();
        expect(window.roRowModel.rows[0]?.name).toBe('Web Alpha');

        const firstName = content.querySelector('td.cell-name a') as HTMLAnchorElement;
        firstName.textContent = 'Changed window row';
        virtualizer.virtualizerActive.mockReturnValue(true);

        filters.captureRowModelFromDocument();
        expect(window.roRowModel.rows[0]?.name).toBe('Web Alpha');

        virtualizer.virtualizerActive.mockReturnValue(false);
        filters.captureRowModelFromDocument();
        expect(window.roRowModel.rows[0]?.name).toBe('Web Alpha');

        // The init path is identity-idempotent; an explicit forced capture is
        // still available to callers that mutate a mounted projection in place.
        filters.captureRowModel(content);
        expect(window.roRowModel.rows[0]?.name).toBe('Changed window row');
    });
});

describe('live filtering and autocomplete', () => {
    test('toggles rendered rows from the full model without filtering operator drafts', () => {
        const { content, input } = renderEditor();
        filters.captureRowModel(content);
        const rows = Array.from(content.querySelectorAll('tbody tr'));

        input.value = 'ALPHA';
        filters.applyLiveNameFilter();

        expect(Array.from(window.roRowModel.visibleKeys ?? [])).toStrictEqual([
            'dev/pods/web-alpha',
        ]);
        expect(rows[0]).not.toHaveClass('ro-row-filtered');
        expect(rows[1]).toHaveClass('ro-row-filtered');

        input.value = 'status:Running';
        filters.applyLiveNameFilter();

        expect(window.roRowModel.visibleKeys).toBe(null);
        expect(rows[0]).not.toHaveClass('ro-row-filtered');
        expect(rows[1]).not.toHaveClass('ro-row-filtered');
        expect(virtualizer.virtualizeOnFilterChange).toHaveBeenCalledTimes(2);
    });

    test('keeps keyed mobile cards in lockstep with their canonical table rows', () => {
        const { content, input } = renderEditor();
        const cards = document.createElement('div');
        cards.className = 'ro-cardlist';
        cards.innerHTML = `
            <article class="ro-pcard" data-key="dev/pods/web-alpha">Web Alpha card</article>
            <article class="ro-pcard" data-key="dev/pods/worker-beta">Worker Beta card</article>
            <article class="ro-pcard">unkeyed state card</article>`;
        content.append(cards);
        filters.captureRowModel(content);
        const webCard = cards.querySelector('[data-key="dev/pods/web-alpha"]');
        const workerCard = cards.querySelector('[data-key="dev/pods/worker-beta"]');
        const unkeyedCard = cards.querySelector('.ro-pcard:not([data-key])');

        input.value = 'alpha';
        filters.applyLiveNameFilter();

        expect(webCard).not.toHaveClass('ro-row-filtered');
        expect(workerCard).toHaveClass('ro-row-filtered');
        expect(unkeyedCard).not.toHaveClass('ro-row-filtered');

        input.value = '';
        filters.applyLiveNameFilter();

        expect(webCard).not.toHaveClass('ro-row-filtered');
        expect(workerCard).not.toHaveClass('ro-row-filtered');
    });

    test('never hides surviving rows against an empty model', () => {
        const { content, input } = renderEditor();
        const rows = Array.from(content.querySelectorAll('tbody tr'));

        // A narrowing draft applied against a live model, then the model goes
        // away underneath it: a history-restored windowed tbody (spacers ->
        // empty snapshot) and the fail-closed delta reset both land here with
        // rendered rows still mounted.
        filters.captureRowModel(content);
        input.value = 'alpha';
        filters.applyLiveNameFilter();
        expect(rows[1]).toHaveClass('ro-row-filtered');

        const tbody = content.querySelector('tbody') as HTMLElement;
        tbody.prepend(document.createElement('tr'));
        (tbody.firstElementChild as HTMLElement).className = 'ro-vspacer';
        filters.captureRowModel(content);
        expect(window.roRowModel.rows).toStrictEqual([]);

        filters.applyLiveNameFilter();

        expect(window.roRowModel.visibleKeys).toBe(null);
        expect(rows[0]).not.toHaveClass('ro-row-filtered');
        expect(rows[1]).not.toHaveClass('ro-row-filtered');
    });

    test('deduplicates descendant-load repairs by projection revision, root and draft', () => {
        const { content, input } = renderEditor();
        filters.captureRowModel(content);
        input.value = 'beta';

        for (let load = 0; load < 8; load += 1) {
            filters.applyLiveNameFilter();
        }

        expect(virtualizer.virtualizeOnFilterChange).toHaveBeenCalledOnce();
        expect(Array.from(window.roRowModel.visibleKeys || [])).toStrictEqual([
            'dev/pods/worker-beta',
        ]);

        input.value = 'alpha';
        filters.applyLiveNameFilter();
        filters.applyLiveNameFilter();
        expect(virtualizer.virtualizeOnFilterChange).toHaveBeenCalledTimes(2);

        // A fresh server projection with the same root/draft must re-apply: its
        // DOM classes may have been synchronized away by the morph.
        filters.captureRowModel(content);
        filters.applyLiveNameFilter();
        expect(virtualizer.virtualizeOnFilterChange).toHaveBeenCalledTimes(3);
    });

    test('degrades safely when the list or editor input is absent', () => {
        filters.applyLiveNameFilter();
        expect(virtualizer.virtualizeOnFilterChange).not.toHaveBeenCalled();

        const { content, input } = renderEditor();
        filters.captureRowModel(content);
        input.remove();
        content.querySelector('tbody tr')?.classList.add('ro-row-filtered');

        filters.applyLiveNameFilter();

        expect(window.roRowModel.visibleKeys).toBe(null);
        expect(content.querySelector('tbody tr')).not.toHaveClass('ro-row-filtered');
        expect(virtualizer.virtualizeOnFilterChange).toHaveBeenCalledOnce();
    });

    test('renders hostile cell values as text in an accessible autocomplete', () => {
        const { autocomplete, content, input } = renderEditor();
        autocomplete.textContent = 'stale suggestion';
        const hostile = '<img src=x onerror="window.__filterAutocompleteXss = true">';
        content.querySelectorAll<HTMLElement>('[data-col="status"]').forEach((cell) => {
            cell.textContent = hostile;
        });
        filters.captureRowModel(content);

        input.value = 'status:';
        filters.updateFilterAC();

        const option = autocomplete.querySelector('[role="option"]') as HTMLElement;
        expect(autocomplete.hidden).toBe(false);
        expect(autocomplete).toHaveAttribute('role', 'listbox');
        expect(autocomplete.childNodes).toHaveLength(1);
        expect(option).toHaveClass('ro-ac-item', 'active');
        expect(option).toHaveAttribute('aria-selected', 'true');
        expect(option.querySelector('.ac-name')?.textContent).toBe(hostile);
        expect(option.querySelector('.ac-hint')?.textContent).toBe('×2');
        expect(autocomplete.querySelectorAll('img')).toHaveLength(0);
        expect(
            (window as unknown as Record<string, unknown>).__filterAutocompleteXss,
        ).toBeUndefined();
    });

    test('supports field completion, value navigation, commit, and dismissal from the keyboard', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods');

        input.value = 'sta';
        filters.updateFilterAC();
        expect(autocomplete.querySelector('.ac-name')).toHaveTextContent('status');

        const tab = targetedKey(input, 'Tab');
        binding('keydown').handler(tab, null);
        expect(tab.defaultPrevented).toBe(true);
        expect(input.value).toBe('status:');
        expect(autocomplete.hidden).toBe(false);
        expect(autocomplete.querySelectorAll('[role="option"]')).toHaveLength(2);

        const arrowUp = targetedKey(input, 'ArrowUp');
        binding('keydown').handler(arrowUp, null);
        expect(arrowUp.defaultPrevented).toBe(true);
        expect(autocomplete.querySelectorAll('[role="option"]')[0]).not.toHaveClass('active');
        expect(autocomplete.querySelectorAll('[role="option"]')[0]).toHaveAttribute(
            'aria-selected',
            'false',
        );
        expect(autocomplete.querySelectorAll('[role="option"]')[1]).toHaveClass('active');
        expect(autocomplete.querySelectorAll('[role="option"]')[1]).toHaveAttribute(
            'aria-selected',
            'true',
        );

        const arrowDown = targetedKey(input, 'ArrowDown');
        binding('keydown').handler(arrowDown, null);
        expect(arrowDown.defaultPrevented).toBe(true);
        expect(autocomplete.querySelectorAll('[role="option"]')[0]).toHaveClass('active');
        expect(autocomplete.querySelectorAll('[role="option"]')[0]).toHaveAttribute(
            'aria-selected',
            'true',
        );
        expect(autocomplete.querySelectorAll('[role="option"]')[1]).not.toHaveClass('active');
        expect(autocomplete.querySelectorAll('[role="option"]')[1]).toHaveAttribute(
            'class',
            'ro-ac-item',
        );
        expect(autocomplete.querySelectorAll('[role="option"]')[1]).toHaveAttribute(
            'aria-selected',
            'false',
        );

        const enter = targetedKey(input, 'Enter');
        binding('keydown').handler(enter, null);
        expect(enter.defaultPrevented).toBe(true);
        expect(input.value).toBe('');
        expect(autocomplete.hidden).toBe(true);
        expect(ajax).toHaveBeenCalledWith('GET', '/pods/_table?f=status%3ARunning', {
            source: input,
            target: '#resource-list-content',
            swap: 'morph',
        });

        input.value = 'sta';
        filters.updateFilterAC();
        const escapeKey = targetedKey(input, 'Escape');
        binding('keydown').handler(escapeKey, null);
        expect(escapeKey.defaultPrevented).toBe(true);
        expect(autocomplete.hidden).toBe(true);
        expect(autocomplete).toBeEmptyDOMElement();
    });

    test('mouse selection follows the hovered value and restores focus to the input', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods');
        input.value = 'status:';
        filters.updateFilterAC();
        const options = autocomplete.querySelectorAll<HTMLElement>('[role="option"]');

        options[1].dispatchEvent(new MouseEvent('mousemove', { bubbles: true }));
        expect(options[1]).toHaveAttribute('aria-selected', 'true');
        const click = targetedClick(options[1]);
        const suggestion = binding('click', '#ro-filter-ac [data-ro-action="pick-suggestion"]');

        expect(suggestion.stop).toBe(true);
        expect(suggestion.handler(click, options[1])).toBe(true);
        expect(click.defaultPrevented).toBe(true);
        expect(document.activeElement).toBe(input);
        expect(ajax).toHaveBeenCalledWith('GET', '/pods/_table?f=status%3APending', {
            source: input,
            target: '#resource-list-content',
            swap: 'morph',
        });
    });

    test('a clicked suggestion uses and clamps its declared index without a prior hover', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods');
        input.value = 'status:';
        filters.updateFilterAC();
        const second = autocomplete.querySelectorAll<HTMLElement>('[role="option"]')[1];
        second.dataset.acIndex = '99';

        const click = targetedClick(second);
        binding('click', '#ro-filter-ac [data-ro-action="pick-suggestion"]').handler(click, second);

        expect(ajax).toHaveBeenCalledExactlyOnceWith('GET', '/pods/_table?f=status%3APending', {
            source: input,
            target: '#resource-list-content',
            swap: 'morph',
        });
    });

    test('field click and value Tab acceptance fill without committing and clear live filtering', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        const inputBinding = binding('input', '#ro-filter-input');
        input.value = 'sta';
        inputBinding.handler(new Event('input'), input);
        expect(window.roRowModel.visibleKeys).toStrictEqual(new Set());
        expect(content.querySelectorAll('.ro-row-filtered')).toHaveLength(2);

        const fieldOption = autocomplete.querySelector<HTMLElement>(
            '[role="option"]',
        ) as HTMLElement;
        binding('click', '#ro-filter-ac [data-ro-action="pick-suggestion"]').handler(
            targetedClick(fieldOption),
            fieldOption,
        );

        expect(input.value).toBe('status:');
        expect(ajax).not.toHaveBeenCalled();
        expect(window.roRowModel.visibleKeys).toBe(null);
        expect(content.querySelectorAll('.ro-row-filtered')).toHaveLength(0);
        expect(autocomplete.querySelectorAll('[role="option"]')).toHaveLength(2);

        const tab = targetedKey(input, 'Tab');
        binding('keydown').handler(tab, null);

        expect(tab.defaultPrevented).toBe(true);
        expect(input.value).toBe('status:Running');
        expect(ajax).not.toHaveBeenCalled();
    });

    test('three-item keyboard navigation has directional movement and ignores unrelated keys', () => {
        const { autocomplete, content, input } = renderEditor();
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        tbody.insertAdjacentHTML(
            'beforeend',
            `<tr data-key="dev/pods/done-gamma">
                <td class="cell-name"><a>Done Gamma</a></td>
                <td data-col="status">Succeeded</td>
                <td>3m</td>
            </tr>`,
        );
        filters.captureRowModel(content);
        input.value = 'status:';
        filters.updateFilterAC();
        const options = autocomplete.querySelectorAll<HTMLElement>('[role="option"]');
        expect(options).toHaveLength(3);
        expect(Array.from(options, (option) => option.className)).toStrictEqual([
            'ro-ac-item active',
            'ro-ac-item',
            'ro-ac-item',
        ]);
        expect(Array.from(options, (option) => option.getAttribute('aria-selected'))).toStrictEqual(
            ['true', 'false', 'false'],
        );

        const unrelated = targetedKey(input, 'x');
        binding('keydown').handler(unrelated, null);
        expect(unrelated.defaultPrevented).toBe(false);
        expect(options[0]).toHaveAttribute('aria-selected', 'true');

        const down = targetedKey(input, 'ArrowDown');
        binding('keydown').handler(down, null);
        expect(options[1]).toHaveAttribute('aria-selected', 'true');

        filters.updateFilterAC();
        const up = targetedKey(input, 'ArrowUp');
        binding('keydown').handler(up, null);
        expect(autocomplete.querySelectorAll('[role="option"]')[2]).toHaveAttribute(
            'aria-selected',
            'true',
        );
    });

    test('closed autocomplete does not hijack arrows, Tab, or Escape', () => {
        const { input } = renderEditor();
        for (const key of ['ArrowDown', 'ArrowUp', 'Tab', 'Escape']) {
            const event = targetedKey(input, key);
            binding('keydown').handler(event, null);
            expect(event.defaultPrevented, key).toBe(false);
        }
    });

    test('closes misleading or empty autocomplete states instead of offering values', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);

        for (const draft of ['status!=Running', 'label:app=web', 'bogus:value']) {
            input.value = 'sta';
            filters.updateFilterAC();
            expect(autocomplete.hidden).toBe(false);

            input.value = draft;
            filters.updateFilterAC();
            expect(autocomplete.hidden).toBe(true);
            expect(autocomplete).toBeEmptyDOMElement();
        }

        input.value = 'sta';
        filters.updateFilterAC();
        expect(autocomplete.hidden).toBe(false);
        content.querySelectorAll<HTMLElement>('[data-col="status"]').forEach((cell) => {
            cell.textContent = '';
        });
        filters.captureRowModel(content);
        input.value = 'status:';
        filters.updateFilterAC();
        expect(autocomplete.hidden).toBe(true);
        expect(autocomplete).toBeEmptyDOMElement();

        input.value = 'sta';
        filters.updateFilterAC();
        expect(autocomplete.hidden).toBe(false);
        input.value = '   ';
        filters.updateFilterAC();
        expect(autocomplete.hidden).toBe(true);
        expect(autocomplete).toBeEmptyDOMElement();

        input.value = 'sta';
        autocomplete.remove();
        expect(() => filters.updateFilterAC()).not.toThrow();

        input.remove();
        expect(() => filters.updateFilterAC()).not.toThrow();
    });

    test('outside click closes suggestions while editor clicks keep them open and focus input', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        input.value = 'sta';
        filters.updateFilterAC();
        const outside = document.createElement('button');
        document.body.appendChild(outside);
        const outsideBinding = binding('click');

        outsideBinding.handler(targetedClick(input), null);
        expect(autocomplete.hidden).toBe(false);

        const field = document.getElementById('ro-filter-field') as HTMLElement;
        expect(binding('click', '#ro-filter-field').handler(targetedClick(field), field)).toBe(
            true,
        );
        expect(document.activeElement).toBe(input);

        outsideBinding.handler(targetedClick(outside), null);
        expect(autocomplete.hidden).toBe(true);
    });

    test('survives autocomplete mounts disappearing during a morph', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        input.value = 'sta';
        filters.updateFilterAC();
        const detachedOption = autocomplete.querySelector('[role="option"]') as HTMLElement;

        autocomplete.remove();
        expect(() => detachedOption.dispatchEvent(new MouseEvent('mousemove'))).not.toThrow();

        input.value = ' ';
        filters.updateFilterAC();
        const staleAutocomplete = document.createElement('div');
        staleAutocomplete.id = 'ro-filter-ac';
        staleAutocomplete.hidden = false;
        const staleOption = document.createElement('div');
        staleOption.dataset.roAction = 'pick-suggestion';
        staleOption.dataset.acIndex = '4';
        staleAutocomplete.appendChild(staleOption);
        document.getElementById('ro-filter-field')?.appendChild(staleAutocomplete);

        const arrowDown = targetedKey(input, 'ArrowDown');
        expect(() => binding('keydown').handler(arrowDown, null)).not.toThrow();
        expect(arrowDown.defaultPrevented).toBe(true);

        const click = targetedClick(staleOption);
        expect(() =>
            binding('click', '#ro-filter-ac [data-ro-action="pick-suggestion"]').handler(
                click,
                staleOption,
            ),
        ).not.toThrow();
        expect(document.activeElement).toBe(input);
    });

    test('a detached stale option cannot reactivate an emptied autocomplete', () => {
        const { autocomplete, content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods');
        input.value = 'sta';
        filters.updateFilterAC();
        const detachedOption = autocomplete.querySelector('[role="option"]') as HTMLElement;

        const escapeKey = targetedKey(input, 'Escape');
        binding('keydown').handler(escapeKey, null);
        detachedOption.dispatchEvent(new MouseEvent('mousemove'));
        autocomplete.hidden = false; // stale mount exposed by an interrupted morph
        input.value = 'status:Running';
        const enter = targetedKey(input, 'Enter');
        binding('keydown').handler(enter, null);

        expect(ajax).toHaveBeenCalledExactlyOnceWith('GET', '/pods/_table?f=status%3ARunning', {
            source: input,
            target: '#resource-list-content',
            swap: 'morph',
        });
        expect(autocomplete.hidden).toBe(true);
        expect(autocomplete).toBeEmptyDOMElement();
    });
});

describe('navigation and binding contracts', () => {
    test('falls back to a plain same-document navigation when htmx is unavailable', () => {
        renderEditor();
        window.history.replaceState(null, '', '/pods');

        filters.issueFilterNavigation('#filter-help');

        expect(window.location.pathname).toBe('/pods');
        expect(window.location.hash).toBe('#filter-help');
    });

    test('falls back when either partial-loop mount is missing', () => {
        const first = renderEditor();
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods');
        first.input.remove();

        filters.issueFilterNavigation('#missing-input');
        expect(window.location.hash).toBe('#missing-input');
        expect(ajax).not.toHaveBeenCalled();

        const second = renderEditor();
        document.body.appendChild(second.input);
        second.content.remove();
        window.history.replaceState(null, '', '/pods');

        filters.issueFilterNavigation('#missing-content');
        expect(window.location.hash).toBe('#missing-content');
        expect(ajax).not.toHaveBeenCalled();
    });

    test('routes canonical list URLs through the exact htmx partial contract', () => {
        const { input } = renderEditor();
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/current');

        filters.issueFilterNavigation(
            '/clusters/dev/namespaces/default/pods///?sort=Name&f=status%3ARunning',
        );

        expect(ajax).toHaveBeenCalledExactlyOnceWith(
            'GET',
            '/clusters/dev/namespaces/default/pods/_table?sort=Name&f=status%3ARunning',
            {
                source: input,
                target: '#resource-list-content',
                swap: 'morph',
            },
        );
    });

    test('observes the optional htmx request promise without requiring one', () => {
        renderEditor();
        const catchHandler = vi.fn();
        const ajax = vi.fn(() => ({ catch: catchHandler }));
        vi.stubGlobal('htmx', { ajax });

        filters.issueFilterNavigation('/pods');

        expect(catchHandler).toHaveBeenCalledOnce();
        expect(catchHandler.mock.calls[0][0]).toBeTypeOf('function');

        ajax.mockReturnValue(undefined as never);
        expect(() => filters.issueFilterNavigation('/pods')).not.toThrow();
    });

    test('Enter commits a known chip while preserving sibling raw query bytes and OR commas', () => {
        const { content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods?f=label%3Aapp%3Dweb,api&sort=Name');
        input.value = ' status:Running,Pending ';
        const event = targetedKey(input, 'Enter');

        binding('keydown').handler(event, null);

        expect(event.defaultPrevented).toBe(true);
        expect(input.value).toBe('');
        expect(window.roRowModel.visibleKeys).toBe(null);
        expect(virtualizer.virtualizeOnFilterChange).toHaveBeenCalledOnce();
        expect(ajax).toHaveBeenCalledExactlyOnceWith(
            'GET',
            '/pods/_table?f=label%3Aapp%3Dweb,api&sort=Name&f=status%3ARunning,Pending',
            {
                source: input,
                target: '#resource-list-content',
                swap: 'morph',
            },
        );
    });

    test('Enter trims Go whitespace but preserves U+FEFF field and value data', () => {
        const { content, input } = renderEditor();
        const status = content.querySelectorAll('thead th')[1];
        status.textContent = '\uFEFFStatus\uFEFF';
        filters.captureRowModel(content);
        const ajax = installHtmx();
        window.history.replaceState(null, '', '/pods');
        input.value = '\u0085\uFEFFstatus\uFEFF:\uFEFFRunning\uFEFF\u0085';

        binding('keydown').handler(targetedKey(input, 'Enter'), null);

        expect(ajax).toHaveBeenCalledExactlyOnceWith(
            'GET',
            '/pods/_table?f=%EF%BB%BFstatus%EF%BB%BF%3A%EF%BB%BFRunning%EF%BB%BF',
            {
                source: input,
                target: '#resource-list-content',
                swap: 'morph',
            },
        );
    });

    test('Enter rejects an unknown field with schema-derived guidance', () => {
        const { content, error, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        input.value = 'bogus:value';
        const event = targetedKey(input, 'Enter');

        binding('keydown').handler(event, null);

        expect(event.defaultPrevented).toBe(true);
        expect(input.value).toBe('bogus:value');
        expect(error.hidden).toBe(false);
        expect(error).toHaveTextContent('no such field — try name, status, label…');
        expect(ajax).not.toHaveBeenCalled();
    });

    test('unknown-field guidance is capped to the first three schema suggestions', () => {
        const { content, error, input } = renderEditor();
        const head = content.querySelector('thead tr') as HTMLTableRowElement;
        head.insertAdjacentHTML(
            'beforeend',
            '<th data-hint="string">Node</th><th data-hint="string">Zone</th>',
        );
        content.querySelectorAll('tbody tr').forEach((row) => {
            row.insertAdjacentHTML('beforeend', '<td>node-a</td><td>zone-a</td>');
        });
        filters.captureRowModel(content);
        input.value = 'bogus:value';

        binding('keydown').handler(targetedKey(input, 'Enter'), null);

        expect(error).toHaveTextContent('no such field — try name, status, node…');
        expect(error).not.toHaveTextContent('zone');
        expect(error).not.toHaveTextContent('label');
    });

    test('keeps free text live-only and makes Backspace without chips a safe no-op', () => {
        const { content, input } = renderEditor();
        filters.captureRowModel(content);
        const ajax = installHtmx();
        input.value = 'web';
        const enter = targetedKey(input, 'Enter');

        binding('keydown').handler(enter, null);

        expect(enter.defaultPrevented).toBe(true);
        expect(input.value).toBe('web');
        expect(ajax).not.toHaveBeenCalled();

        input.value = '';
        const backspace = targetedKey(input, 'Backspace');
        binding('keydown').handler(backspace, null);
        expect(backspace.defaultPrevented).toBe(true);
        expect(ajax).not.toHaveBeenCalled();
    });

    test('Backspace only pops chips for an actually empty editor', () => {
        const chips = `
            <span class="ro-scope-chip">
                <a class="chip-x" data-ro-action="remove-chip" href="/pods">remove</a>
            </span>
        `;
        const { input } = renderEditor(chips);
        const ajax = installHtmx();
        input.value = 'draft';
        const nonemptyBackspace = targetedKey(input, 'Backspace');
        binding('keydown').handler(nonemptyBackspace, null);
        expect(nonemptyBackspace.defaultPrevented).toBe(false);
        expect(ajax).not.toHaveBeenCalled();

        input.value = '';
        const unrelated = targetedKey(input, 'Delete');
        binding('keydown').handler(unrelated, null);
        expect(unrelated.defaultPrevented).toBe(false);
        expect(ajax).not.toHaveBeenCalled();
    });

    test('input clears a field error and immediately reapplies live visibility', () => {
        const { content, error, input } = renderEditor();
        filters.captureRowModel(content);
        input.value = 'bogus:value';
        binding('keydown').handler(targetedKey(input, 'Enter'), null);
        expect(error.hidden).toBe(false);

        input.value = 'web';
        const inputBinding = binding('input', '#ro-filter-input');

        expect(inputBinding.stop).toBe(true);
        expect(inputBinding.handler(new Event('input'), input)).toBe(true);
        expect(error.hidden).toBe(true);
        expect(content.querySelector('[data-key="dev/pods/web-alpha"]')).not.toHaveClass(
            'ro-row-filtered',
        );
        expect(content.querySelector('[data-key="dev/pods/worker-beta"]')).toHaveClass(
            'ro-row-filtered',
        );

        input.value = 'sta';
        inputBinding.handler(new Event('input'), input);
        expect(document.getElementById('ro-filter-ac')).not.toHaveAttribute('hidden');
        expect(document.querySelector('#ro-filter-ac .ac-name')).toHaveTextContent('status');
    });

    test('unknown-field handling remains safe without an error mount or captured schema', () => {
        const { error, input } = renderEditor();
        error.remove();
        input.value = 'bogus:value';
        expect(() => binding('keydown').handler(targetedKey(input, 'Enter'), null)).not.toThrow();

        filters.captureRowModel(document.createDocumentFragment());
        const fallbackError = document.createElement('div');
        fallbackError.id = 'ro-filter-error';
        fallbackError.hidden = true;
        document.getElementById('ro-filter-field')?.appendChild(fallbackError);

        binding('keydown').handler(targetedKey(input, 'Enter'), null);
        expect(fallbackError).toHaveTextContent('no such field — try label…');
        expect(fallbackError.hidden).toBe(false);
    });

    test('the remove binding uses its own href and Backspace pops the last chip', () => {
        const chips = `
            <span class="ro-scope-chip">
                <a class="chip-x" data-ro-action="remove-chip" href="/pods?f=second">remove first</a>
            </span>
            <span class="ro-scope-chip">
                <a class="chip-x" data-ro-action="remove-chip" href="/pods?f=first">remove last</a>
            </span>
        `;
        const { input } = renderEditor(chips);
        const ajax = installHtmx();
        const removers = document.querySelectorAll<HTMLAnchorElement>('.chip-x');
        const removeBinding = binding('click', '#ro-filter-field [data-ro-action="remove-chip"]');
        const click = targetedClick(removers[0]);

        expect(removeBinding.stop).toBe(true);
        expect(removeBinding.handler(click, removers[0])).toBe(true);
        expect(click.defaultPrevented).toBe(true);
        expect(ajax).toHaveBeenCalledExactlyOnceWith('GET', '/pods/_table?f=second', {
            source: input,
            target: '#resource-list-content',
            swap: 'morph',
        });

        ajax.mockClear();
        const backspace = targetedKey(input, 'Backspace');
        binding('keydown').handler(backspace, null);

        expect(backspace.defaultPrevented).toBe(true);
        expect(ajax).toHaveBeenCalledExactlyOnceWith('GET', '/pods/_table?f=first', {
            source: input,
            target: '#resource-list-content',
            swap: 'morph',
        });
    });

    test('binding descriptors preserve dispatcher routing and stop semantics', () => {
        expect(
            filters.filtersBindings.map(({ event, selector, stop }) => ({ event, selector, stop })),
        ).toStrictEqual([
            {
                event: 'click',
                selector: '#ro-filter-field [data-ro-action="remove-chip"]',
                stop: true,
            },
            {
                event: 'click',
                selector: '#ro-filter-ac [data-ro-action="pick-suggestion"]',
                stop: true,
            },
            { event: 'click', selector: '#ro-filter-field', stop: true },
            { event: 'click', selector: undefined, stop: undefined },
            { event: 'input', selector: '#ro-filter-input', stop: true },
            { event: 'keydown', selector: undefined, stop: undefined },
        ]);
    });

    test('field focus and keydown bindings ignore events already owned by another target', () => {
        const { input } = renderEditor();
        const field = document.getElementById('ro-filter-field') as HTMLElement;
        const focus = vi.spyOn(input, 'focus');

        binding('click', '#ro-filter-field').handler(targetedClick(input), field);
        expect(focus).not.toHaveBeenCalled();

        const other = document.createElement('button');
        const enter = targetedKey(other, 'Enter');
        binding('keydown').handler(enter, null);
        expect(enter.defaultPrevented).toBe(false);
        expect(input.value).toBe('');
    });
});
