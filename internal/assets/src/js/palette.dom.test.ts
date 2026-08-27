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

import { closePalette, openPalette, paletteBindings, paletteHrefSafe } from './palette.js';

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

function expectPaletteRowContract(row: HTMLElement): void {
    expect(row.classList.contains('ro-pal-item')).toBe(true);
    expect(row.dataset.roAction).toBe('pick-palette-row');
    expect(row.getAttribute('role')).toBe('option');
    expect(row.querySelector(':scope > .pal-label')).not.toBeNull();
}

function expectPaletteBindingContract(bindings: Binding[]): void {
    expect(bindings.map(({ event, selector, stop }) => ({ event, selector, stop }))).toStrictEqual([
        {
            event: 'click',
            selector: '[data-ro-action="pick-palette-row"]',
            stop: true,
        },
        { event: 'click', selector: '[data-ro-palette-open]', stop: true },
        { event: 'click', selector: '[data-ro-search-refine]', stop: true },
        { event: 'click', selector: '#ro-palette', stop: true },
        { event: 'input', selector: '#ro-palette-input', stop: true },
        { event: 'keydown', selector: undefined, stop: undefined },
        { event: 'keydown', selector: undefined, stop: undefined },
        { event: 'focusin', selector: '[data-ro-palette-open]', stop: undefined },
    ]);
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

function targetedMouseEvent(target: EventTarget): MouseEvent {
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

describe('binding contract', () => {
    test('exports the complete delegated palette contract in order', () => {
        expectPaletteBindingContract(paletteBindings);
        expect((window as unknown as { roOpenPalette: unknown }).roOpenPalette).toBe(openPalette);
    });

    test('re-evaluates module-owned static contracts under an isolated import', async () => {
        const seams = window as unknown as {
            roFuzzy: (query: string, text: string) => number;
            roOpenPalette: typeof openPalette;
        };
        const priorFuzzy = seams.roFuzzy;
        const priorOpen = seams.roOpenPalette;
        vi.resetModules();

        try {
            const freshRank = await import('./palette-rank.js');
            const freshPalette = await import('./palette.js');
            const feed = emptyFeed();
            feed.clusters = [{ name: 'Fresh cluster', href: '#fresh-cluster' }];
            const data = document.getElementById('ro-palette-data') as HTMLElement;
            data.textContent = JSON.stringify({
                ...feed,
                clusters: [null, ...feed.clusters],
            });
            window.localStorage.setItem(
                'ro-pref-recents',
                JSON.stringify([null, { label: 'Fresh recent', href: '#fresh-recent' }]),
            );

            expectPaletteBindingContract(freshPalette.paletteBindings);
            expect(seams.roFuzzy).toBe(freshRank.roFuzzyScore);
            expect(seams.roFuzzy('dpl', 'Deployments')).toBeGreaterThanOrEqual(0);
            expect(seams.roOpenPalette).toBe(freshPalette.openPalette);

            freshPalette.openPalette();

            expect(document.getElementById('ro-palette')?.classList.contains('open')).toBe(true);
            expect(rowByLabel('Fresh recent').dataset.href).toBe('#fresh-recent');
            expect(rowByLabel('Fresh cluster').dataset.href).toBe('#fresh-cluster');
        } finally {
            seams.roFuzzy = priorFuzzy;
            seams.roOpenPalette = priorOpen;
        }
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
        const empty = document.querySelector('.ro-pal-empty') as HTMLElement;
        expect(empty.className).toBe('ro-pal-empty');
        expect(empty.textContent).toBe('No matching targets.');
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
            name: 'null JSON',
            prepare: () => {
                const data = document.getElementById('ro-palette-data');
                if (data) data.textContent = 'null';
            },
        },
        ...['false', '42', '"scalar"'].map((json) => ({
            name: `${json} JSON primitive`,
            prepare: () => {
                const data = document.getElementById('ro-palette-data');
                if (data) data.textContent = json;
            },
        })),
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
                currentCluster: 42,
                currentNamespace: [],
                clusters: [
                    null,
                    42,
                    'invalid',
                    false,
                    [],
                    {},
                    { name: '', href: '#empty-name' },
                    { name: '   ', href: '#blank-name' },
                    { name: 42, href: '#numeric-name' },
                    { label: 'Cluster alias', href: '#cluster-alias' },
                    { name: 'Safe cluster', href: '#safe-cluster' },
                ],
                namespaces: [
                    null,
                    { label: '', href: '#empty-namespace' },
                    { name: 'Safe namespace', href: '#safe-namespace' },
                ],
                kinds: [
                    null,
                    { plural: '', href: '#empty-kind' },
                    { kind: 42, href: '#numeric-kind' },
                    { kind: 'Pod', namespaced: true, href: '#pods' },
                ],
                actions: [
                    null,
                    { label: '   ', action: 'theme' },
                    { label: 'No target' },
                    { label: 'Numeric action', action: 42 },
                    { label: 'Switch theme', action: 'theme' },
                ],
            }),
        );

        expect(() => openPalette()).not.toThrow();

        expect(
            Array.from(document.querySelectorAll<HTMLElement>('.ro-pal-item')).map(
                (row) => row.dataset.label,
            ),
        ).toStrictEqual(['Pod', 'Safe namespace', 'Cluster alias', 'Safe cluster', 'Switch theme']);
        const scope = document.getElementById('ro-palette-scope') as HTMLElement;
        expect(scope.hidden).toBe(true);
        expect(scope.textContent).toBe('');
        expect(document.querySelector('[data-label="No target"]')).toBeNull();
        expect(document.querySelector('[data-label="Numeric action"]')).toBeNull();
    });

    test('unsafe recent hrefs and malformed entries are re-validated before rendering', () => {
        window.localStorage.setItem(
            'ro-pref-recents',
            JSON.stringify([
                null,
                42,
                'scalar',
                [],
                { label: 'unsafe js', href: 'javascript:alert(1)' },
                { label: 'unsafe data', href: 'data:text/html,pwned' },
                { label: 'safe recent', href: '#safe-recent' },
                { label: 'theme recent', action: 'theme' },
                { label: ' padded recent ', action: ' future-action ' },
                { label: '', href: '#blank-label' },
                { label: '   ', href: '#whitespace-label' },
                { label: 42, href: '#numeric-label' },
                { label: 'numeric action', action: 42 },
                { label: 'whitespace action', action: '   ' },
                { label: 'numeric href', href: 42 },
                { label: 'whitespace href', href: '   ' },
                { label: 'invalid URL', href: 'http://[' },
                { label: 'no target' },
            ]),
        );

        openPalette();

        const safeRecent = rowByLabel('safe recent');
        expect(safeRecent.dataset.href).toBe('#safe-recent');
        expect(safeRecent.dataset.action).toBeUndefined();
        const themeRecent = rowByLabel('theme recent');
        expect(themeRecent.dataset.action).toBe('theme');
        expect(themeRecent.dataset.href).toBeUndefined();
        expect(rowByLabel('padded recent').dataset.action).toBe('future-action');
        expect(document.querySelector('[data-label="unsafe js"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="unsafe data"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="numeric action"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="whitespace action"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="numeric href"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="whitespace href"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="invalid URL"]')).not.toBeInTheDocument();
        expect(document.querySelector('[data-label="no target"]')).not.toBeInTheDocument();
    });

    test.each(['', '{}', '42', '"scalar"', 'null'])(
        'treats unusable recent payload %s as empty',
        (stored) => {
            window.localStorage.setItem('ro-pref-recents', stored);

            expect(() => openPalette()).not.toThrow();

            expect(document.querySelectorAll('.ro-pal-item')).toHaveLength(0);
            expect(document.querySelector('.ro-pal-empty')?.textContent).toBe(
                'No matching targets.',
            );
        },
    );

    test('accepts only same-origin HTTP destinations after trimming', () => {
        const feed = emptyFeed();
        const origin = window.location.origin;
        feed.clusters = [
            { name: 'relative', href: '  /clusters/prod  ' },
            { name: 'same origin', href: `${origin}/clusters/stage` },
            { name: 'external http', href: 'http://example.test/clusters' },
            { name: 'external https', href: 'https://example.test/clusters' },
            { name: 'protocol relative', href: '//example.test/clusters' },
            { name: 'same-origin blob', href: `blob:${origin}/opaque-id` },
            { name: 'invalid URL', href: 'http://[' },
            { name: 'blank href', href: '   ' },
            { name: 'numeric href', href: 42 },
        ];
        renderPaletteHarness(feed);

        openPalette();

        expect(rowByLabel('relative').dataset.href).toBe('/clusters/prod');
        expect(rowByLabel('same origin').dataset.href).toBe(`${origin}/clusters/stage`);
        for (const label of [
            'external http',
            'external https',
            'protocol relative',
            'same-origin blob',
            'invalid URL',
            'blank href',
            'numeric href',
        ]) {
            expect(document.querySelector(`[data-label="${label}"]`)).toBeNull();
        }
    });

    test('the href policy explicitly accepts both same-origin HTTP and HTTPS', () => {
        expect(paletteHrefSafe('/clusters/prod', 'http://readout.test/current')).toBe(
            '/clusters/prod',
        );
        expect(
            paletteHrefSafe('https://readout.test/clusters/prod', 'https://readout.test/current'),
        ).toBe('https://readout.test/clusters/prod');
        expect(paletteHrefSafe('blob:http://readout.test/id', 'http://readout.test/current')).toBe(
            '',
        );
        expect(
            paletteHrefSafe('blob:https://readout.test/id', 'https://readout.test/current'),
        ).toBe('');
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
                { label: 'Theme', href: '   ', action: 'theme' },
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
            { label: 'Theme', action: 'theme' },
        ]);
        expect(window.location.hash).toBe('#pods');
    });

    test('continues working when browser storage is unavailable', () => {
        const feed = emptyFeed();
        feed.clusters = [{ name: 'Pods', href: '#pods' }];
        renderPaletteHarness(feed);
        const getItem = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
            throw new DOMException('blocked', 'SecurityError');
        });
        const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
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
        expect(getItem).toHaveBeenCalledWith('ro-pref-recents');
        expect(setItem).toHaveBeenCalledWith(
            'ro-pref-recents',
            JSON.stringify([{ label: 'Pods', href: '#pods' }]),
        );
    });

    test('re-reads a swapped feed and replaces the active row model on every open', () => {
        const first = emptyFeed();
        first.clusters = [{ name: 'Alpha', href: '#alpha' }];
        renderPaletteHarness(first);
        openPalette();
        expect(activeLabel()).toBe('Alpha');

        const second = emptyFeed();
        second.clusters = [{ name: 'Beta', href: '#beta' }];
        const data = document.getElementById('ro-palette-data') as HTMLElement;
        data.textContent = JSON.stringify(second);
        openPalette();

        expect(document.querySelector('[data-label="Alpha"]')).toBeNull();
        expect(activeLabel()).toBe('Beta');
        const enter = targetedKeyboardEvent(
            'Enter',
            document.getElementById('ro-palette-input') as HTMLInputElement,
        );
        binding('keydown', undefined, 1).handler(enter, null);
        expect(enter.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#beta');
    });
});

describe('safe DOM rendering and actions', () => {
    test('all row types expose one exact delegated option contract', () => {
        const feed = emptyFeed();
        feed.clusters = [{ name: 'Production', href: '#production' }];
        feed.kinds = [{ kind: 'Pod', href: '#pods' }];
        renderPaletteHarness(feed);
        window.localStorage.setItem(
            'ro-pref-recents',
            JSON.stringify([{ label: 'Recent', href: '#recent' }]),
        );
        document.body.insertAdjacentHTML(
            'beforeend',
            `<div id="resource-list-content"><table class="ro-table"><tbody><tr>
                <td class="cell-name"><a href="#api">api-0</a></td>
            </tr></tbody></table></div>`,
        );

        openPalette();

        const rows = Array.from(document.querySelectorAll<HTMLElement>('.ro-pal-item'));
        expect(rows.map((row) => row.dataset.label)).toStrictEqual([
            'Recent',
            'api-0',
            'Pod',
            'Production',
        ]);
        rows.forEach(expectPaletteRowContract);
        expect(rows[0].getAttribute('aria-selected')).toBe('true');
        rows.slice(1).forEach((row) => {
            expect(row.getAttribute('aria-selected')).toBe('false');
        });
        expect(document.querySelector('.ro-pal-empty')).toBeNull();
        expect(HTMLElement.prototype.scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' });

        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        input.value = 'api';
        binding('input', '#ro-palette-input').handler(new Event('input'), input);
        const everywhere = document.querySelector<HTMLElement>('.ro-pal-item');
        expect(everywhere).not.toBeNull();
        expectPaletteRowContract(everywhere as HTMLElement);
        expect(everywhere?.querySelector(':scope > .pal-label')?.textContent).toBe(
            'Search all clusters for “api”',
        );
        expect(everywhere?.dataset.href).toBe('/search?q=api');
        expect(document.querySelector('[data-label="Recent"]')).toBeNull();
    });

    test('the everywhere row clones the optional glyph and URL-encodes the trimmed query', () => {
        const original = document.querySelector('#ro-palette .ro-pal-search .ico') as HTMLElement;

        openPalette('  api/v1 ? ready  ');

        const everywhere = document.querySelector('.ro-pal-item') as HTMLElement;
        const clone = everywhere.querySelector('.ico');
        expect(document.querySelector('.ro-pal-group')?.textContent).toBe('Everywhere');
        expect(everywhere.dataset.label).toBe('Search all clusters for “api/v1 ? ready”');
        expect(everywhere.querySelector('.pal-label')?.textContent).toBe(
            'Search all clusters for “api/v1 ? ready”',
        );
        expect(everywhere.dataset.href).toBe('/search?q=api%2Fv1%20%3F%20ready');
        expect(clone).not.toBe(original);
        expect(clone?.textContent).toBe('search');

        original.remove();
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        input.value = 'pods';
        binding('input', '#ro-palette-input').handler(new Event('input'), input);
        expect(document.querySelector('.ro-pal-item .ico')).toBeNull();
        expect(document.querySelector('.ro-pal-item')?.getAttribute('data-label')).toBe(
            'Search all clusters for “pods”',
        );
    });

    test('renders feed context, kind metadata, display names, and safe icon markup', () => {
        const feed = emptyFeed();
        feed.currentCluster = ' prod ';
        feed.currentNamespace = ' team-a ';
        feed.clusters = [
            { name: ' prod ', display: ' Production ', href: '#prod' },
            { name: 'staging', display: '', icon: '<b>not a kind</b>', href: '#staging' },
            { name: 'Primary cluster', label: 'Secondary cluster', href: '#primary' },
        ];
        feed.namespaces = [
            { name: 'team-a', href: '#team-a' },
            { name: 'team-b', href: '#team-b' },
        ];
        feed.kinds = [
            {
                kind: 'Deployment',
                display: 'Deploy',
                group: ' apps ',
                namespaced: true,
                icon: '<span class="kind-icon" aria-hidden="true">D</span>',
                name: 'team-a',
                href: '#deployments',
            },
            { plural: 'nodes', namespaced: false, icon: 42, href: '#nodes' },
            { kind: 'Service', plural: 'services', href: '#services' },
        ];
        feed.actions = [{ label: 'Switch theme', action: ' theme ', icon: '<b>not a kind</b>' }];
        renderPaletteHarness(feed);

        openPalette();

        const scope = document.getElementById('ro-palette-scope');
        expect(scope?.textContent).toBe('team-a');
        expect((scope as HTMLElement).hidden).toBe(false);

        expect(
            Array.from(document.querySelectorAll<HTMLElement>('.ro-pal-group')).map(
                (heading) => heading.textContent,
            ),
        ).toStrictEqual(['Resource types', 'Namespaces', 'Clusters', 'Actions']);

        const cluster = rowByLabel('prod');
        expect(cluster.getAttribute('title')).toBe('prod');
        expect(cluster.querySelector('.pal-label')?.firstChild?.textContent).toBe('Production');
        expect(cluster.querySelector('.pal-ctx')?.className).toBe('pal-ctx');
        expect(cluster.querySelector('.pal-ctx')?.textContent).toBe('current');
        expect(rowByLabel('team-a').querySelector('.pal-ctx')?.textContent).toBe('current');
        const staging = rowByLabel('staging');
        expect(staging.getAttribute('title')).toBeNull();
        expect(staging.querySelector('.pal-label')?.textContent).toBe('staging');
        expect(staging.querySelector('.pal-ctx')).toBeNull();
        expect(staging.querySelector('b')).toBeNull();
        expect(staging.dataset.action).toBeUndefined();
        expect(rowByLabel('Primary cluster').querySelector('.pal-label')?.textContent).toBe(
            'Primary cluster',
        );
        expect(document.querySelector('[data-label="Secondary cluster"]')).toBeNull();
        expect(rowByLabel('team-b').querySelector('.pal-ctx')).toBeNull();

        const deployment = rowByLabel('Deployment');
        expect(deployment.getAttribute('title')).toBe('Deployment');
        expect(deployment.querySelector('.kind-icon')?.textContent).toBe('D');
        expect(deployment.querySelector('.pal-meta')?.textContent).toBe('apps');
        expect(deployment.querySelector('.pal-scope')?.className).toBe('pal-scope ns');
        expect(deployment.querySelector('.pal-scope')?.textContent).toBe('namespaced');
        expect(deployment.querySelector('.pal-ctx')).toBeNull();

        const nodes = rowByLabel('nodes');
        expect(nodes.getAttribute('title')).toBeNull();
        expect(nodes.querySelector('.pal-meta')?.textContent).toBe('core');
        expect(nodes.querySelector('.pal-scope')?.className).toBe('pal-scope cluster');
        expect(nodes.querySelector('.pal-scope')?.textContent).toBe('cluster');
        expect(nodes.querySelector('.kind-icon')).toBeNull();
        expect(rowByLabel('Service').querySelector('.pal-label')?.textContent).toBe('Service');
        expect(document.querySelector('[data-label="services"]')).toBeNull();
        const action = rowByLabel('Switch theme');
        expect(action.dataset.action).toBe('theme');
        expect(action.dataset.href).toBeUndefined();
        expect(action.querySelector('.pal-meta')).toBeNull();
        expect(action.querySelector('.pal-scope')).toBeNull();
        expect(action.querySelector('b')).toBeNull();

        const nodeScroll = vi.spyOn(nodes, 'scrollIntoView');
        nodeScroll.mockClear();
        nodes.dispatchEvent(new MouseEvent('mousemove'));
        expect(activeLabel()).toBe('nodes');
        expect(nodes.getAttribute('aria-selected')).toBe('true');
        expect(deployment.getAttribute('aria-selected')).toBe('false');
        expect(nodeScroll).toHaveBeenCalledOnce();
        expect(nodeScroll).toHaveBeenCalledWith({ block: 'nearest' });
    });

    test.each([
        { cluster: 'prod', namespace: null, text: 'prod', hidden: false },
        { cluster: null, namespace: null, text: '', hidden: true },
    ])('renders the exact scope fallback for $text', ({ cluster, namespace, text, hidden }) => {
        const feed = emptyFeed();
        feed.currentCluster = cluster;
        feed.currentNamespace = namespace;
        renderPaletteHarness(feed);

        openPalette();

        const scope = document.getElementById('ro-palette-scope') as HTMLElement;
        expect(scope.textContent).toBe(text);
        expect(scope.hidden).toBe(hidden);
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
        expect(api.dataset.href).toBe('#api');
        expect(api.querySelector('.pal-status')?.textContent).toBe('CrashLoop');
        expect(api.querySelector('.pal-status')?.className).toBe('pal-status err');
        expect(rowByLabel('backend').querySelector('.pal-status')).toBeNull();
    });

    test('preserves every supported status tone with deterministic precedence', () => {
        const rows = [
            ['healthy', ' ok ', 'ok'],
            ['warning', ' warn ', 'warn'],
            ['failed', ' err ', 'err'],
            ['working', ' info ', 'info'],
            ['quiet', ' mute ', 'mute'],
            ['plain', ' custom ', 'other'],
            ['priority', ' first ', 'err warn info'],
            ['empty', '   ', 'ok'],
        ]
            .map(
                ([name, status, classes]) => `<tr>
                    <td class="cell-name"><a href="#${name}"> ${name} </a></td>
                    <td class="cell-status ${classes}">${status}</td>
                </tr>`,
            )
            .join('');
        document.body.insertAdjacentHTML(
            'beforeend',
            `<div id="resource-list-content"><table class="ro-table"><tbody>${rows}</tbody></table></div>`,
        );

        openPalette();

        for (const tone of ['ok', 'warn', 'err', 'info', 'mute']) {
            const status = rowByLabel(
                { ok: 'healthy', warn: 'warning', err: 'failed', info: 'working', mute: 'quiet' }[
                    tone
                ] as string,
            ).querySelector('.pal-status') as HTMLElement;
            expect(status.textContent).toBe(tone);
            expect(status.className).toBe(`pal-status ${tone}`);
        }
        const plain = rowByLabel('plain').querySelector('.pal-status') as HTMLElement;
        expect(plain.textContent).toBe('custom');
        expect(plain.className).toBe('pal-status');
        const priority = rowByLabel('priority').querySelector('.pal-status') as HTMLElement;
        expect(priority.textContent).toBe('first');
        expect(priority.className).toBe('pal-status warn');
        expect(rowByLabel('empty').querySelector('.pal-status')).toBeNull();
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
            expect(document.querySelector(`[data-label="${label}"]`)).toBeNull();
        }
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

    test('theme wins over a mixed href target and remains safe without a toggle', () => {
        const feed = emptyFeed();
        feed.actions = [{ label: 'Theme with href', action: 'theme', href: '#must-not-navigate' }];
        renderPaletteHarness(feed);
        window.location.hash = '#before';
        document.getElementById('btn-theme-toggle')?.remove();
        openPalette();

        expect(() =>
            binding('click', '[data-ro-action="pick-palette-row"]').handler(
                new MouseEvent('click', { cancelable: true }),
                rowByLabel('Theme with href'),
            ),
        ).not.toThrow();

        expect(window.location.hash).toBe('#before');
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
        expect(JSON.parse(window.localStorage.getItem('ro-pref-recents') || 'null')).toStrictEqual([
            { label: 'Theme with href', href: '#must-not-navigate', action: 'theme' },
        ]);
    });

    test('an unknown action may still carry its explicit navigation destination', () => {
        const feed = emptyFeed();
        feed.actions = [{ label: 'Navigate action', action: 'future-action', href: '#future' }];
        renderPaletteHarness(feed);
        openPalette();

        binding('click', '[data-ro-action="pick-palette-row"]').handler(
            new MouseEvent('click', { cancelable: true }),
            rowByLabel('Navigate action'),
        );

        expect(window.location.hash).toBe('#future');
        expect(JSON.parse(window.localStorage.getItem('ro-pref-recents') || 'null')).toStrictEqual([
            { label: 'Navigate action', href: '#future', action: 'future-action' },
        ]);
    });

    test('delegated malformed rows can act, but are never persisted as recents', () => {
        const choose = binding('click', '[data-ro-action="pick-palette-row"]');
        const missingLabel = document.createElement('div');
        missingLabel.dataset.href = '#raw-target';

        choose.handler(new MouseEvent('click', { cancelable: true }), missingLabel);

        expect(window.location.hash).toBe('#raw-target');
        expect(window.localStorage.getItem('ro-pref-recents')).toBeNull();

        const missingTarget = document.createElement('div');
        missingTarget.dataset.label = 'Dead row';
        choose.handler(new MouseEvent('click', { cancelable: true }), missingTarget);

        expect(window.localStorage.getItem('ro-pref-recents')).toBeNull();
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
        const inputEvent = new Event('input', { cancelable: true });
        expect(binding('input', '#ro-palette-input').handler(inputEvent, input)).toBe(true);
        expect(inputEvent.defaultPrevented).toBe(false);
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

    test('missing or non-string programmatic prefills reset to the empty query', () => {
        const feed = emptyFeed();
        feed.clusters = [{ name: 'Pods', href: '#pods' }];
        renderPaletteHarness(feed);
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        const refine = document.createElement('button');
        const refineEvent = targetedMouseEvent(refine);

        input.value = 'stale';
        expect(binding('click', '[data-ro-search-refine]').handler(refineEvent, refine)).toBe(true);
        expect(refineEvent.defaultPrevented).toBe(true);
        expect(input.value).toBe('');
        expect(document.querySelector('.ro-pal-group')?.textContent).toBe('Clusters');

        input.value = 'stale again';
        (
            window as unknown as {
                roOpenPalette: (prefill: unknown) => void;
            }
        ).roOpenPalette(42);
        expect(input.value).toBe('');
        expect(rowByLabel('Pods')).toBeInTheDocument();
    });

    test('only a click on the backdrop itself dismisses the palette', () => {
        openPalette('pods');
        const palette = document.getElementById('ro-palette') as HTMLElement;
        const panel = palette.querySelector('.ro-palette') as HTMLElement;
        const backdrop = binding('click', '#ro-palette');

        expect(backdrop.handler(targetedMouseEvent(panel), palette)).toBe(false);
        expect(palette).toHaveClass('open');

        const text = document.createTextNode('not an element');
        panel.appendChild(text);
        expect(() => backdrop.handler(targetedMouseEvent(text), palette)).not.toThrow();
        expect(backdrop.handler(targetedMouseEvent(text), palette)).toBe(false);
        expect(palette).toHaveClass('open');

        expect(backdrop.handler(targetedMouseEvent(palette), palette)).toBe(true);
        expect(palette).not.toHaveClass('open');
    });

    test('focus-opening restores the opener without immediately reopening the palette', () => {
        const opener = document.getElementById('palette-opener') as HTMLButtonElement;
        const blur = vi.spyOn(opener, 'blur');
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
            expect(blur).toHaveBeenCalledOnce();

            closePalette();

            expect(opener).toHaveFocus();
            expect(document.getElementById('ro-palette')).not.toHaveClass('open');
            expect(blur).toHaveBeenCalledOnce();
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

        const filterArrow = targetedKeyboardEvent('ArrowDown', filter);
        openKeys.handler(filterArrow, null);
        expect(filterArrow.defaultPrevented).toBe(false);

        const unrelated = targetedKeyboardEvent('x', document.body);
        openKeys.handler(unrelated, null);
        expect(unrelated.defaultPrevented).toBe(false);
        expect(document.getElementById('ro-palette')).toHaveClass('open');

        const escapeKey = targetedKeyboardEvent('Escape', document.body);
        openKeys.handler(escapeKey, null);
        expect(escapeKey.defaultPrevented).toBe(true);
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');

        openPalette();
        const targetlessEscape = new KeyboardEvent('keydown', {
            key: 'Escape',
            cancelable: true,
        });
        openKeys.handler(targetlessEscape, null);
        expect(targetlessEscape.defaultPrevented).toBe(true);
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
    });

    test('does not hijack modified or unrelated keyboard shortcuts', () => {
        const chord = binding('keydown', undefined, 0);
        const events = [
            targetedKeyboardEvent('k', document.body),
            targetedKeyboardEvent('k', document.body, { metaKey: true, altKey: true }),
            targetedKeyboardEvent('k', document.body, { ctrlKey: true, altKey: true }),
            targetedKeyboardEvent('k', document.body, { metaKey: true, shiftKey: true }),
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
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');

        renderPaletteHarness();
        document.getElementById('ro-palette')?.remove();
        expect(() => openPalette()).not.toThrow();

        renderPaletteHarness();
        document.getElementById('ro-palette-list')?.remove();
        expect(() => openPalette()).not.toThrow();
        expect(document.getElementById('ro-palette')).toHaveClass('open');

        renderPaletteHarness();
        document.getElementById('ro-palette-scope')?.remove();
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
        const restore = vi.spyOn(prior, 'focus');

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
        expect(restore).toHaveBeenCalledOnce();

        closePalette();
        expect(restore).toHaveBeenCalledOnce();
    });

    test('close refuses stale and inside-palette restore targets', () => {
        const palette = document.getElementById('ro-palette') as HTMLElement;
        const inside = document.createElement('button');
        palette.appendChild(inside);
        inside.focus();
        const insideRestore = vi.spyOn(inside, 'focus');

        openPalette();
        closePalette();

        expect(insideRestore).not.toHaveBeenCalled();

        const detached = document.getElementById('prior-focus') as HTMLButtonElement;
        detached.focus();
        const detachedRestore = vi.spyOn(detached, 'focus');
        openPalette();
        detached.remove();
        closePalette();
        expect(detachedRestore).not.toHaveBeenCalled();
    });

    test('close restores a connected focusable SVG target', () => {
        const palette = document.getElementById('ro-palette') as HTMLElement;

        const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
        svg.setAttribute('tabindex', '0');
        document.body.appendChild(svg);
        svg.focus();
        expect(document.activeElement).toBe(svg);
        const svgRestore = vi.spyOn(svg, 'focus');
        openPalette();
        closePalette();
        expect(svgRestore).toHaveBeenCalledOnce();
        expect(document.activeElement).toBe(svg);
        expect(palette).not.toHaveClass('open');
    });

    test('close safely skips a connected prior Element without a focus method', () => {
        const inert = document.createElementNS('http://www.w3.org/2000/svg', 'g');
        document.body.appendChild(inert);
        Object.defineProperty(inert, 'focus', { configurable: true, value: undefined });
        const activeElement = vi.spyOn(document, 'activeElement', 'get').mockReturnValue(inert);
        openPalette();
        activeElement.mockRestore();

        expect(() => closePalette()).not.toThrow();
        expect(document.getElementById('ro-palette')).not.toHaveClass('open');
    });

    test('a throwing focus restore always releases the reopen gate', () => {
        const prior = document.getElementById('prior-focus') as HTMLButtonElement;
        const opener = document.getElementById('palette-opener') as HTMLButtonElement;
        prior.focus();
        openPalette();
        vi.spyOn(prior, 'focus').mockImplementation(() => {
            throw new Error('focus failed');
        });

        expect(() => closePalette()).toThrow('focus failed');

        const blur = vi.spyOn(opener, 'blur');
        const event = new FocusEvent('focusin');
        Object.defineProperty(event, 'target', { configurable: true, value: opener });
        binding('focusin', '[data-ro-palette-open]').handler(event, opener);
        expect(document.getElementById('ro-palette')).toHaveClass('open');
        expect(blur).toHaveBeenCalledOnce();
    });

    test('Arrow and Tab keys wrap the active row, and Enter activates it', () => {
        const feed = emptyFeed();
        feed.clusters = [
            { name: 'Alpha', href: '#alpha' },
            { name: 'Beta', href: '#beta' },
            { name: 'Gamma', href: '#gamma' },
        ];
        renderPaletteHarness(feed);
        openPalette();
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        const openKeys = binding('keydown', undefined, 1);
        const alpha = rowByLabel('Alpha');
        const beta = rowByLabel('Beta');
        const gamma = rowByLabel('Gamma');
        const alphaScroll = vi.spyOn(alpha, 'scrollIntoView');
        const betaScroll = vi.spyOn(beta, 'scrollIntoView');
        const gammaScroll = vi.spyOn(gamma, 'scrollIntoView');

        expect(activeLabel()).toBe('Alpha');

        const up = targetedKeyboardEvent('ArrowUp', input);
        openKeys.handler(up, null);
        expect(up.defaultPrevented).toBe(true);
        expect(activeLabel()).toBe('Gamma');
        expect(alpha.getAttribute('aria-selected')).toBe('false');
        expect(gamma.getAttribute('aria-selected')).toBe('true');
        expect(gammaScroll).toHaveBeenLastCalledWith({ block: 'nearest' });

        const down = targetedKeyboardEvent('ArrowDown', input);
        openKeys.handler(down, null);
        expect(down.defaultPrevented).toBe(true);
        expect(activeLabel()).toBe('Alpha');
        expect(alphaScroll).toHaveBeenLastCalledWith({ block: 'nearest' });

        const tab = targetedKeyboardEvent('Tab', input);
        openKeys.handler(tab, null);
        expect(tab.defaultPrevented).toBe(true);
        expect(activeLabel()).toBe('Beta');
        expect(betaScroll).toHaveBeenLastCalledWith({ block: 'nearest' });

        const shiftTab = targetedKeyboardEvent('Tab', input, { shiftKey: true });
        openKeys.handler(shiftTab, null);
        expect(shiftTab.defaultPrevented).toBe(true);
        expect(activeLabel()).toBe('Alpha');
        expect(alphaScroll).toHaveBeenLastCalledWith({ block: 'nearest' });
        const secondUp = targetedKeyboardEvent('ArrowUp', input);
        openKeys.handler(secondUp, null);
        expect(secondUp.defaultPrevented).toBe(true);
        expect(activeLabel()).toBe('Gamma');
        expect(gamma.getAttribute('aria-selected')).toBe('true');

        const enter = targetedKeyboardEvent('Enter', input);
        openKeys.handler(enter, null);
        expect(enter.defaultPrevented).toBe(true);
        expect(window.location.hash).toBe('#gamma');
    });

    test('keyboard navigation is a safe no-op for an empty open palette', () => {
        renderPaletteHarness('{broken');
        openPalette();
        const input = document.getElementById('ro-palette-input') as HTMLInputElement;
        const openKeys = binding('keydown', undefined, 1);

        const events = [
            targetedKeyboardEvent('ArrowDown', input),
            targetedKeyboardEvent('ArrowUp', input),
            targetedKeyboardEvent('Enter', input),
            targetedKeyboardEvent('Tab', input),
            targetedKeyboardEvent('Tab', input, { shiftKey: true }),
        ];
        expect(() =>
            events.forEach((event) => {
                openKeys.handler(event, null);
            }),
        ).not.toThrow();

        events.forEach((event) => {
            expect(event.defaultPrevented).toBe(true);
        });
        expect(window.location.hash).toBe('');
        expect(window.localStorage.getItem('ro-pref-recents')).toBeNull();
        expect(document.getElementById('ro-palette')).toHaveClass('open');
        expect(HTMLElement.prototype.scrollIntoView).not.toHaveBeenCalled();
    });

    test.each([
        { key: 'k', modifiers: { metaKey: true } },
        { key: 'K', modifiers: { metaKey: true } },
        { key: 'k', modifiers: { ctrlKey: true } },
        { key: 'K', modifiers: { ctrlKey: true } },
        { key: 'k', modifiers: { metaKey: true, ctrlKey: true } },
    ])('the $key command chord closes competing surfaces for $modifiers', ({ key, modifiers }) => {
        const chord = binding('keydown', undefined, 0);
        const target = document.body;
        const event = targetedKeyboardEvent(key, target, modifiers);

        chord.handler(event, null);

        expect(event.defaultPrevented).toBe(true);
        expect(dependencies.closeKbdOverlay).toHaveBeenCalledOnce();
        expect(dependencies.closeRowMenu).toHaveBeenCalledOnce();
        expect(document.getElementById('ro-palette')).toHaveClass('open');
    });
});
