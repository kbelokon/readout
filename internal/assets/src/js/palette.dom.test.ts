// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const dependencies = vi.hoisted(() => ({
    closeKbdOverlay: vi.fn(),
    closeRowMenu: vi.fn(),
    virtRows: vi.fn<() => HTMLElement[]>(() => []),
    virtualizerActive: vi.fn<() => boolean>(() => false),
}));

vi.mock('./context-menu.js', () => ({
    closeRowMenu: dependencies.closeRowMenu,
}));
vi.mock('./keyboard.js', () => ({
    closeKbdOverlay: dependencies.closeKbdOverlay,
}));
vi.mock('./virtualizer.js', () => ({
    virtRows: dependencies.virtRows,
    virtualizerActive: dependencies.virtualizerActive,
}));

import { closePalette, openPalette, paletteBindings } from './palette.js';

interface PaletteFeed {
    currentCluster: string | null;
    currentNamespace: string | null;
    clusters: Record<string, unknown>[];
    namespaces: Record<string, unknown>[];
    kinds: Record<string, unknown>[];
    actions: Record<string, unknown>[];
}

const emptyFeed = (): PaletteFeed => ({
    currentCluster: null,
    currentNamespace: null,
    clusters: [],
    namespaces: [],
    kinds: [],
    actions: [],
});

function renderPaletteHarness(feed: PaletteFeed | string = emptyFeed()): void {
    document.body.innerHTML = `
        <button id="prior-focus">Before palette</button>
        <button id="btn-theme-toggle">Toggle theme</button>
        <button id="palette-opener" data-ro-palette-open>Search</button>
        <div id="ro-palette-data" hidden></div>
        <div id="ro-palette" class="ro-palette-backdrop" aria-hidden="true">
            <div class="ro-palette">
                <div class="ro-pal-search">
                    <span class="ico" aria-hidden="true">search</span>
                    <input id="ro-palette-input">
                    <span id="ro-palette-scope" hidden></span>
                </div>
                <div id="ro-palette-list"></div>
            </div>
        </div>
    `;
    const data = document.getElementById('ro-palette-data') as HTMLElement;
    data.textContent = typeof feed === 'string' ? feed : JSON.stringify(feed);
}

function binding(event: string, selector?: string, ordinal = 0): Binding {
    const matches = paletteBindings.filter(
        (candidate) => candidate.event === event && candidate.selector === selector,
    );
    expect(matches.length).toBeGreaterThan(ordinal);
    return matches[ordinal] as Binding;
}

function rowByLabel(label: string): HTMLElement {
    const row = Array.from(document.querySelectorAll<HTMLElement>('.ro-pal-item')).find(
        (candidate) => candidate.dataset.label === label,
    );
    expect(row).toBeDefined();
    return row as HTMLElement;
}

function activeLabel(): string | undefined {
    return document.querySelector<HTMLElement>('.ro-pal-item.active')?.dataset.label;
}

function targetedKeyboardEvent(
    key: string,
    target: Element,
    init: KeyboardEventInit = {},
): KeyboardEvent {
    const event = new KeyboardEvent('keydown', { ...init, key, bubbles: true, cancelable: true });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function targetedMouseEvent(target: Element): MouseEvent {
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

function tableRow(markup: string): HTMLTableRowElement {
    const table = document.createElement('table');
    table.innerHTML = `<tbody><tr>${markup}</tr></tbody>`;
    return table.querySelector('tr') as HTMLTableRowElement;
}

beforeEach(() => {
    renderPaletteHarness();
    window.localStorage.clear();
    window.history.replaceState(null, '', '/');
    dependencies.virtualizerActive.mockReturnValue(false);
    dependencies.virtRows.mockReturnValue([]);
    delete (window as unknown as { htmx?: unknown }).htmx;
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
        configurable: true,
        value: vi.fn(),
        writable: true,
    });
});

describe('feed and recent-storage validation', () => {
    test('malformed feed and localStorage degrade to an open empty palette', () => {
        renderPaletteHarness('{not-json');
        window.localStorage.setItem('ro-pref-recents', '{also-not-json');

        expect(() => openPalette()).not.toThrow();

        expect(document.getElementById('ro-palette')).toHaveClass('open');
        expect(document.getElementById('ro-palette-list')).toHaveTextContent(
            'No matching targets.',
        );
        expect(document.querySelectorAll('.ro-pal-item')).toHaveLength(0);
    });

    test.each([
        {
            name: 'missing blob',
            prepare: () => document.getElementById('ro-palette-data')?.remove(),
        },
        {
            name: 'empty blob',
            prepare: () => {
                const data = document.getElementById('ro-palette-data');
                if (data) data.textContent = '   ';
            },
        },
        {
            name: 'non-object JSON',
            prepare: () => {
                const data = document.getElementById('ro-palette-data');
                if (data) data.textContent = 'null';
            },
        },
        {
            name: 'array JSON',
            prepare: () => {
                const data = document.getElementById('ro-palette-data');
                if (data) data.textContent = '[]';
            },
        },
        {
            name: 'missing group arrays',
            prepare: () => {
                const data = document.getElementById('ro-palette-data');
                if (data) data.textContent = JSON.stringify({ clusters: null, kinds: 'invalid' });
            },
        },
    ])('treats $name as an empty server feed', ({ prepare }) => {
        prepare();

        expect(() => openPalette()).not.toThrow();

        expect(document.getElementById('ro-palette-list')).toHaveTextContent(
            'No matching targets.',
        );
    });

    test('drops null, scalar, and array feed entries while keeping valid records', () => {
        renderPaletteHarness(
            JSON.stringify({
                currentCluster: null,
                currentNamespace: null,
                clusters: [
                    null,
                    42,
                    'invalid',
                    false,
                    [],
                    { name: 'Safe cluster', href: '#safe-cluster' },
                ],
                namespaces: [null, { name: 'Safe namespace', href: '#safe-namespace' }],
                kinds: [null, { kind: 'Pod', namespaced: true, href: '#pods' }],
                actions: [null, { label: 'Switch theme', action: 'theme' }],
            }),
        );

        expect(() => openPalette()).not.toThrow();

        expect(
            Array.from(document.querySelectorAll<HTMLElement>('.ro-pal-item')).map(
                (row) => row.dataset.label,
            ),
        ).toStrictEqual(['Pod', 'Safe namespace', 'Safe cluster', 'Switch theme']);
    });

    test('unsafe recent hrefs and malformed entries are re-validated before rendering', () => {
        window.localStorage.setItem(
            'ro-pref-recents',
            JSON.stringify([
                { label: 'unsafe js', href: 'javascript:alert(1)' },
                { label: 'unsafe data', href: 'data:text/html,pwned' },
                { label: 'safe recent', href: '#safe-recent' },
                { label: 'theme recent', action: 'theme' },
                { label: '', href: '#blank-label' },
                { label: 'no target' },
            ]),
        );

        openPalette();

        expect(rowByLabel('safe recent')).toHaveAttribute('data-href', '#safe-recent');
        expect(rowByLabel('theme recent')).toHaveAttribute('data-action', 'theme');
        expect(document.querySelector('[data-label="unsafe js"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="unsafe data"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="no target"]')).not.toBeInTheDocument();
    });

    test('dedupes recents by normalized destination, keeps the newest, and caps distinct rows', () => {
        window.localStorage.setItem(
            'ro-pref-recents',
            JSON.stringify([
                { label: 'Newest pods', href: '  #pods  ' },
                { label: 'Older pods', href: '#pods' },
                { label: 'Newest theme', action: 'theme' },
                { label: 'Older theme', action: 'theme' },
                { label: 'Alpha', href: '#alpha' },
                { label: 'Beta', href: '#beta' },
                { label: 'Gamma', href: '#gamma' },
                { label: 'Beyond cap', href: '#beyond' },
            ]),
        );

        openPalette();

        const labels = Array.from(document.querySelectorAll<HTMLElement>('.ro-pal-item')).map(
            (row) => row.dataset.label,
        );
        expect(labels).toStrictEqual(['Newest pods', 'Newest theme', 'Alpha', 'Beta', 'Gamma']);
        expect(rowByLabel('Newest pods')).toHaveAttribute('data-href', '#pods');
        expect(document.querySelector('[data-label="Older pods"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="Older theme"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="Beyond cap"]')).not.toBeInTheDocument();
    });

    test('choosing an existing target dedupes it to the front and drops invalid stored entries', () => {
        const feed = emptyFeed();
        feed.clusters = [{ name: 'Pods', href: '#pods' }];
        renderPaletteHarness(feed);
        window.localStorage.setItem(
            'ro-pref-recents',
            JSON.stringify([
                { label: 'Old pods label', href: '#pods' },
                { label: 'Other', href: '#other' },
                { label: 'Unsafe', href: 'vbscript:msgbox(1)' },
            ]),
        );
        openPalette();
        const pods = rowByLabel('Pods');

        binding('click', '[data-ro-action="pick-palette-row"]').handler(
            new MouseEvent('click', { cancelable: true }),
            pods,
        );

        expect(JSON.parse(window.localStorage.getItem('ro-pref-recents') || 'null')).toStrictEqual([
            { label: 'Pods', href: '#pods' },
            { label: 'Other', href: '#other' },
        ]);
        expect(window.location.hash).toBe('#pods');
    });

    test('continues working when browser storage is unavailable', () => {
        const feed = emptyFeed();
        feed.clusters = [{ name: 'Pods', href: '#pods' }];
        renderPaletteHarness(feed);
        vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
            throw new DOMException('blocked', 'SecurityError');
        });
        vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
            throw new DOMException('blocked', 'SecurityError');
        });

        expect(() => openPalette()).not.toThrow();
        expect(() =>
            binding('click', '[data-ro-action="pick-palette-row"]').handler(
                new MouseEvent('click', { cancelable: true }),
                rowByLabel('Pods'),
            ),
        ).not.toThrow();

        expect(window.location.hash).toBe('#pods');
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
    });
});

describe('safe DOM rendering and actions', () => {
    test('renders feed context, kind metadata, display names, and safe icon markup', () => {
        const feed = emptyFeed();
        feed.currentCluster = 'prod';
        feed.currentNamespace = 'team-a';
        feed.clusters = [{ name: 'prod', display: 'Production', href: '#prod' }];
        feed.namespaces = [{ name: 'team-a', href: '#team-a' }];
        feed.kinds = [
            {
                kind: 'Deployment',
                display: 'Deploy',
                group: 'apps',
                namespaced: true,
                icon: '<span class="kind-icon" aria-hidden="true">D</span>',
                href: '#deployments',
            },
            { plural: 'nodes', namespaced: false, href: '#nodes' },
        ];
        feed.actions = [{ label: 'Switch theme', action: 'theme' }];
        renderPaletteHarness(feed);

        openPalette();

        const scope = document.getElementById('ro-palette-scope');
        expect(scope).toHaveTextContent('team-a');
        expect(scope).not.toHaveAttribute('hidden');

        const cluster = rowByLabel('prod');
        expect(cluster).toHaveAttribute('title', 'prod');
        expect(cluster.querySelector('.pal-label')).toHaveTextContent('Productioncurrent');
        expect(cluster.querySelector('.pal-ctx')).toHaveTextContent('current');
        expect(rowByLabel('team-a').querySelector('.pal-ctx')).toHaveTextContent('current');

        const deployment = rowByLabel('Deployment');
        expect(deployment).toHaveAttribute('title', 'Deployment');
        expect(deployment.querySelector('.kind-icon')).toHaveTextContent('D');
        expect(deployment.querySelector('.pal-meta')).toHaveTextContent('apps');
        expect(deployment.querySelector('.pal-scope')).toHaveClass('ns');
        expect(deployment.querySelector('.pal-scope')).toHaveTextContent('namespaced');

        const nodes = rowByLabel('nodes');
        expect(nodes.querySelector('.pal-meta')).toHaveTextContent('core');
        expect(nodes.querySelector('.pal-scope')).toHaveClass('cluster');
        expect(nodes.querySelector('.pal-scope')).toHaveTextContent('cluster');
        expect(rowByLabel('Switch theme')).toHaveAttribute('data-action', 'theme');

        nodes.dispatchEvent(new MouseEvent('mousemove'));
        expect(activeLabel()).toBe('nodes');
        expect(nodes).toHaveAttribute('aria-selected', 'true');
    });

    test('harvests usable table rows with status tone and skips incomplete rows', () => {
        document.body.insertAdjacentHTML(
            'beforeend',
            `<div id="resource-list-content">
                <table class="ro-table"><tbody>
                    <tr><td class="cell-name">No link</td></tr>
                    <tr><td class="cell-name"><a href="#blank">   </a></td></tr>
                    <tr>
                        <td class="cell-name"><a href="#api"> api-0 </a></td>
                        <td class="cell-status err"> CrashLoop </td>
                    </tr>
                    <tr><td class="cell-name"><a href="#backend"> backend </a></td></tr>
                </tbody></table>
            </div>`,
        );

        openPalette();

        expect(document.querySelector('.ro-pal-group')).toHaveTextContent('On this page');
        expect(document.querySelector('[data-label="No link"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label=""]')).not.toBeInTheDocument();
        const api = rowByLabel('api-0');
        expect(api).toHaveAttribute('data-href', '#api');
        expect(api.querySelector('.pal-status')).toHaveTextContent('CrashLoop');
        expect(api.querySelector('.pal-status')).toHaveClass('err');
        expect(rowByLabel('backend').querySelector('.pal-status')).not.toBeInTheDocument();
    });

    test('rejects unsafe href schemes harvested from rendered table rows', () => {
        document.body.insertAdjacentHTML(
            'beforeend',
            `<div id="resource-list-content">
                <table class="ro-table"><tbody>
                    <tr><td class="cell-name"><a href="javascript:alert(1)">unsafe js</a></td></tr>
                    <tr><td class="cell-name"><a href="data:text/html,pwned">unsafe data</a></td></tr>
                    <tr><td class="cell-name"><a href="vbscript:msgbox(1)">unsafe vb</a></td></tr>
                    <tr><td class="cell-name"><a href="#safe-object">safe object</a></td></tr>
                </tbody></table>
            </div>`,
        );

        openPalette();

        expect(rowByLabel('safe object')).toHaveAttribute('data-href', '#safe-object');
        for (const label of ['unsafe js', 'unsafe data', 'unsafe vb']) {
            expect(document.querySelector(`[data-label="${label}"]`)).not.toBeInTheDocument();
        }
    });

    test('searches the virtualizer full row model instead of only the visible DOM slice', () => {
        document.body.insertAdjacentHTML(
            'beforeend',
            `<div id="resource-list-content">
                <table class="ro-table"><tbody>
                    <tr><td class="cell-name"><a href="#visible">visible-only</a></td></tr>
                </tbody></table>
            </div>`,
        );
        const offscreen = tableRow(
            '<td class="cell-name"><a href="#offscreen">offscreen-api</a></td>' +
                '<td class="cell-status">Ready</td>',
        );
        dependencies.virtualizerActive.mockReturnValue(true);
        dependencies.virtRows.mockReturnValue([offscreen]);

        openPalette('offscreen');

        expect(dependencies.virtRows).toHaveBeenCalledOnce();
        expect(rowByLabel('offscreen-api')).toHaveAttribute('data-href', '#offscreen');
        expect(rowByLabel('offscreen-api').querySelector('.pal-status')).toHaveTextContent('Ready');
        expect(document.querySelector('[data-label="visible-only"]')).not.toBeInTheDocument();
    });

    test('hostile labels stay text and unsafe schemes never become navigation targets', () => {
        const hostile = '<img src=x onerror="window.__palettePwned=1">';
        const feed = emptyFeed();
        feed.clusters = [
            { name: hostile, href: '#safe' },
            { name: 'javascript target', href: 'javascript:alert(1)' },
            { name: 'data target', href: 'data:text/html,pwned' },
            { name: 'vbscript target', href: 'vbscript:msgbox(1)' },
        ];
        renderPaletteHarness(feed);

        openPalette();

        expect(rowByLabel(hostile).querySelector('.pal-label')).toHaveTextContent(hostile);
        expect(document.querySelector('#ro-palette-list img')).not.toBeInTheDocument();
        expect(document.querySelector('#ro-palette-list script')).not.toBeInTheDocument();
        for (const label of ['javascript target', 'data target', 'vbscript target']) {
            expect(rowByLabel(label)).not.toHaveAttribute('data-href');
        }

        const unsafe = rowByLabel('javascript target');
        binding('click', '[data-ro-action="pick-palette-row"]').handler(
            new MouseEvent('click', { cancelable: true }),
            unsafe,
        );
        expect(window.location.hash).toBe('');
    });

    test('safe choices use plain location navigation and do not involve htmx', () => {
        const feed = emptyFeed();
        feed.clusters = [{ name: 'Pods', href: '#pods' }];
        renderPaletteHarness(feed);
        const ajax = vi.fn();
        const trigger = vi.fn();
        (window as unknown as { htmx: { ajax: typeof ajax; trigger: typeof trigger } }).htmx = {
            ajax,
            trigger,
        };
        openPalette();
        const row = rowByLabel('Pods');
        const event = new MouseEvent('click', { cancelable: true });

        expect(binding('click', '[data-ro-action="pick-palette-row"]').handler(event, row)).toBe(
            true,
        );

        expect(event.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#pods');
        expect(ajax).not.toHaveBeenCalled();
        expect(trigger).not.toHaveBeenCalled();
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
    });

    test('an unknown named action closes and records the choice but performs no action', () => {
        const feed = emptyFeed();
        feed.actions = [{ label: 'Mystery', action: 'explode' }];
        renderPaletteHarness(feed);
        const theme = document.getElementById('btn-theme-toggle') as HTMLButtonElement;
        const themeClick = vi.spyOn(theme, 'click');
        openPalette();
        window.location.hash = '#before';
        const mystery = rowByLabel('Mystery');

        binding('click', '[data-ro-action="pick-palette-row"]').handler(
            new MouseEvent('click', { cancelable: true }),
            mystery,
        );

        expect(themeClick).not.toHaveBeenCalled();
        expect(window.location.hash).toBe('#before');
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
        expect(JSON.parse(window.localStorage.getItem('ro-pref-recents') || 'null')).toStrictEqual([
            { label: 'Mystery', action: 'explode' },
        ]);
    });

    test('the theme action clicks the real toggle, records the action, and never navigates', () => {
        const feed = emptyFeed();
        feed.actions = [{ label: 'Switch theme', action: 'theme' }];
        renderPaletteHarness(feed);
        const toggle = document.getElementById('btn-theme-toggle') as HTMLButtonElement;
        const click = vi.spyOn(toggle, 'click').mockImplementation(() => {});
        window.location.hash = '#before';
        openPalette();

        binding('click', '[data-ro-action="pick-palette-row"]').handler(
            new MouseEvent('click', { cancelable: true }),
            rowByLabel('Switch theme'),
        );

        expect(click).toHaveBeenCalledOnce();
        expect(window.location.hash).toBe('#before');
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
        expect(JSON.parse(window.localStorage.getItem('ro-pref-recents') || 'null')).toStrictEqual([
            { label: 'Switch theme', action: 'theme' },
        ]);
    });
});

describe('delegated entry points and dismissal', () => {
    test('opener, live query, and Refine entry points preserve their user-visible query', () => {
        const feed = emptyFeed();
        feed.clusters = [
            { name: 'Deployments', href: '#deployments' },
            { name: 'Services', href: '#services' },
        ];
        renderPaletteHarness(feed);
        const opener = document.getElementById('palette-opener') as HTMLButtonElement;
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        const openerEvent = targetedMouseEvent(opener);

        expect(binding('click', '[data-ro-palette-open]').handler(openerEvent, opener)).toBe(true);
        expect(openerEvent.defaultPrevented).toBe(true);
        expect(input).toHaveFocus();

        input.value = 'deploy';
        expect(binding('input', '#ro-palette-input').handler(new Event('input'), input)).toBe(true);
        expect(rowByLabel('Deployments')).toBeInTheDocument();
        expect(document.querySelector('.ro-pal-item .ico')).toHaveTextContent('search');
        expect(document.querySelector('.ro-pal-item')).toHaveAttribute(
            'data-href',
            '/search?q=deploy',
        );

        closePalette();
        const refine = document.createElement('button');
        refine.dataset.roSearchRefine = '';
        refine.dataset.query = '  services  ';
        document.body.appendChild(refine);
        const refineEvent = targetedMouseEvent(refine);

        expect(binding('click', '[data-ro-search-refine]').handler(refineEvent, refine)).toBe(true);
        expect(refineEvent.defaultPrevented).toBe(true);
        expect(input).toHaveValue('  services  ');
        expect(document.querySelector('.ro-pal-item')).toHaveAttribute(
            'data-href',
            '/search?q=services',
        );
        expect(rowByLabel('Services')).toBeInTheDocument();
    });

    test('only a click on the backdrop itself dismisses the palette', () => {
        openPalette('pods');
        const palette = document.getElementById('ro-palette') as HTMLElement;
        const panel = palette.querySelector('.ro-palette') as HTMLElement;
        const backdrop = binding('click', '#ro-palette');

        expect(backdrop.handler(targetedMouseEvent(panel), palette)).toBe(false);
        expect(palette).toHaveClass('open');

        expect(backdrop.handler(targetedMouseEvent(palette), palette)).toBe(true);
        expect(palette).not.toHaveClass('open');
    });

    test('focus-opening restores the opener without immediately reopening the palette', () => {
        const opener = document.getElementById('palette-opener') as HTMLButtonElement;
        const focusBinding = binding('focusin', '[data-ro-palette-open]');
        const listener = (event: FocusEvent): void => {
            if (event.target === opener) {
                focusBinding.handler(event, opener);
            }
        };
        document.addEventListener('focusin', listener);

        try {
            opener.focus();
            expect(document.getElementById('ro-palette')).toHaveClass('open');
            expect(document.getElementById('ro-palette-input')).toHaveFocus();

            closePalette();

            expect(opener).toHaveFocus();
            expect(document.getElementById('ro-palette')).not.toHaveClass('open');
        } finally {
            document.removeEventListener('focusin', listener);
        }
    });

    test('Escape is focus-routed, while closed palettes and unrelated keys stay inert', () => {
        const openKeys = binding('keydown', undefined, 1);
        const filter = document.createElement('input');
        filter.id = 'ro-filter-input';
        document.body.appendChild(filter);

        const closedEscape = targetedKeyboardEvent('Escape', document.body);
        openKeys.handler(closedEscape, null);
        expect(closedEscape.defaultPrevented).toBe(false);

        openPalette('pods');
        const filterEscape = targetedKeyboardEvent('Escape', filter);
        openKeys.handler(filterEscape, null);
        expect(filterEscape.defaultPrevented).toBe(false);
        expect(document.getElementById('ro-palette')).toHaveClass('open');

        const unrelated = targetedKeyboardEvent('x', document.body);
        openKeys.handler(unrelated, null);
        expect(unrelated.defaultPrevented).toBe(false);
        expect(document.getElementById('ro-palette')).toHaveClass('open');

        const escapeKey = targetedKeyboardEvent('Escape', document.body);
        openKeys.handler(escapeKey, null);
        expect(escapeKey.defaultPrevented).toBe(true);
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
    });

    test('does not hijack modified or unrelated keyboard shortcuts', () => {
        const chord = binding('keydown', undefined, 0);
        const events = [
            targetedKeyboardEvent('k', document.body),
            targetedKeyboardEvent('k', document.body, { metaKey: true, altKey: true }),
            targetedKeyboardEvent('k', document.body, { ctrlKey: true, shiftKey: true }),
            targetedKeyboardEvent('x', document.body, { metaKey: true }),
        ];

        for (const event of events) {
            chord.handler(event, null);
            expect(event.defaultPrevented).toBe(false);
        }
        expect(dependencies.closeKbdOverlay).not.toHaveBeenCalled();
        expect(dependencies.closeRowMenu).not.toHaveBeenCalled();
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
    });

    test('defensive DOM gaps are no-ops instead of breaking the page', () => {
        document.getElementById('ro-palette-input')?.remove();
        expect(() => openPalette()).not.toThrow();

        renderPaletteHarness();
        document.getElementById('ro-palette-list')?.remove();
        expect(() => openPalette()).not.toThrow();
        expect(document.getElementById('ro-palette')).toHaveClass('open');

        document.getElementById('ro-palette')?.remove();
        expect(() => closePalette()).not.toThrow();
    });
});

describe('focus and keyboard model', () => {
    test('open is idempotent and close restores the original outside focus', () => {
        const prior = document.getElementById('prior-focus') as HTMLButtonElement;
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        prior.focus();

        openPalette('pods');
        openPalette('services');

        expect(document.getElementById('ro-palette')).toHaveClass('open');
        expect(document.getElementById('ro-palette')).toHaveAttribute('aria-hidden', 'false');
        expect(input).toHaveValue('services');
        expect(input).toHaveFocus();

        closePalette();

        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
        expect(document.getElementById('ro-palette')).toHaveAttribute('aria-hidden', 'true');
        expect(prior).toHaveFocus();
    });

    test('Arrow and Tab keys wrap the active row, and Enter activates it', () => {
        const feed = emptyFeed();
        feed.clusters = [
            { name: 'Alpha', href: '#alpha' },
            { name: 'Beta', href: '#beta' },
        ];
        renderPaletteHarness(feed);
        openPalette();
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        const openKeys = binding('keydown', undefined, 1);

        expect(activeLabel()).toBe('Alpha');

        const up = targetedKeyboardEvent('ArrowUp', input);
        openKeys.handler(up, null);
        expect(up.defaultPrevented).toBe(true);
        expect(activeLabel()).toBe('Beta');

        openKeys.handler(targetedKeyboardEvent('ArrowDown', input), null);
        expect(activeLabel()).toBe('Alpha');

        openKeys.handler(targetedKeyboardEvent('Tab', input), null);
        expect(activeLabel()).toBe('Beta');

        openKeys.handler(targetedKeyboardEvent('Tab', input, { shiftKey: true }), null);
        expect(activeLabel()).toBe('Alpha');
        openKeys.handler(targetedKeyboardEvent('ArrowUp', input), null);
        expect(activeLabel()).toBe('Beta');
        expect(rowByLabel('Beta')).toHaveAttribute('aria-selected', 'true');

        const enter = targetedKeyboardEvent('Enter', input);
        openKeys.handler(enter, null);
        expect(enter.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#beta');
    });

    test('keyboard navigation is a safe no-op for an empty open palette', () => {
        renderPaletteHarness('{broken');
        openPalette();
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        const openKeys = binding('keydown', undefined, 1);

        expect(() => {
            openKeys.handler(targetedKeyboardEvent('ArrowDown', input), null);
            openKeys.handler(targetedKeyboardEvent('ArrowUp', input), null);
            openKeys.handler(targetedKeyboardEvent('Enter', input), null);
        }).not.toThrow();

        expect(window.location.hash).toBe('');
        expect(document.getElementById('ro-palette')).toHaveClass('open');
    });

    test('the command chord closes competing surfaces before opening the palette', () => {
        const chord = binding('keydown', undefined, 0);
        const target = document.body;
        const event = targetedKeyboardEvent('k', target, { metaKey: true });

        chord.handler(event, null);

        expect(event.defaultPrevented).toBe(true);
        expect(dependencies.closeKbdOverlay).toHaveBeenCalledOnce();
        expect(dependencies.closeRowMenu).toHaveBeenCalledOnce();
        expect(document.getElementById('ro-palette')).toHaveClass('open');
    });
});
