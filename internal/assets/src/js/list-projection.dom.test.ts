// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';
import {
    adoptListProjection,
    applyListProjectionDeltaTransaction,
    commitListProjectionSwap,
    ensureListProjection,
    type ListProjectionDeltaPlan,
    type ListProjectionDeltaResult,
    listProjectionCardByKey,
    listProjectionOrder,
    listProjectionRevision,
    listProjectionRowByKey,
    listProjectionRowModel,
    listProjectionRows,
    listProjectionSwapPending,
    listProjectionVisibleRows,
    listProjectionWindowed,
    prepareListProjectionSwap,
    resetListProjection,
    setListProjectionVisibleKeys,
} from './list-projection.js';

interface ListOptions {
    keyedCards?: boolean;
    windowed?: boolean;
}

function rowDOMID(key: string): string {
    let result = 'row-';
    for (const character of key) {
        const code = character.codePointAt(0) as number;
        result +=
            code <= 0x20 ||
            character === '"' ||
            character === '\\' ||
            character === '%' ||
            code === 0x7f
                ? `%${code.toString(16).toUpperCase().padStart(2, '0')}`
                : character;
    }
    return result;
}

function escapeHTML(value: string): string {
    return value
        .replaceAll('&', '&amp;')
        .replaceAll('"', '&quot;')
        .replaceAll('<', '&lt;')
        .replaceAll('>', '&gt;');
}

function buildList(keys: readonly string[], options: ListOptions = {}): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    const wrap = document.createElement('div');
    wrap.className = `ro-table-wrap${options.windowed ? ' ro-windowed' : ''}`;
    const table = document.createElement('table');
    table.className = 'ro-table';
    table.innerHTML = `
        <thead><tr><th data-hint="string">Name</th><th data-hint="enum">Status</th></tr></thead>
        <tbody></tbody>`;
    const tbody = table.tBodies.item(0) as HTMLTableSectionElement;
    const cards = document.createElement('div');
    cards.className = 'ro-cardlist';
    keys.forEach((key, index) => {
        const row = tbody.insertRow();
        row.id = rowDOMID(key);
        row.dataset.key = key;
        row.innerHTML = `<td class="cell-name"><a> Workload ${index} </a></td><td> Ready ${index} </td>`;
        if (!options.windowed) {
            const card = document.createElement('div');
            card.className = 'ro-pcard';
            if (options.keyedCards) {
                card.dataset.key = key;
            }
            card.textContent = `Card ${key}`;
            cards.append(card);
        }
    });
    wrap.append(table);
    content.append(wrap, cards);
    return content;
}

function rowFragment(key: string, name = `Next ${key}`, body = ''): string {
    return `<tr id="${escapeHTML(rowDOMID(key))}" data-key="${escapeHTML(key)}"><td class="cell-name"><a>${escapeHTML(name)}</a></td><td>${body || 'Ready'}</td></tr>`;
}

function cardFragment(key: string, name = `Next ${key}`, body = ''): string {
    return `<div class="ro-pcard" data-key="${escapeHTML(key)}"><b>${escapeHTML(name)}</b>${body}</div>`;
}

function morphInPlace(current: HTMLElement, incoming: HTMLElement): void {
    for (const attribute of Array.from(current.attributes)) current.removeAttribute(attribute.name);
    for (const attribute of Array.from(incoming.attributes)) {
        current.setAttribute(attribute.name, attribute.value);
    }
    current.replaceChildren(...Array.from(incoming.childNodes, (child) => child.cloneNode(true)));
}

function applyDelta(
    overrides: Partial<ListProjectionDeltaPlan>,
    options: {
        morph?: (current: HTMLElement, incoming: HTMLElement) => unknown;
        reconcile?: () => void;
        restoreExternalState?: () => void;
    } = {},
): ListProjectionDeltaResult {
    return applyListProjectionDeltaTransaction(
        { remove: [], upsert: [], regions: [], ...overrides },
        {
            morph: options.morph,
            reconcile: options.reconcile || (() => {}),
            restoreExternalState: options.restoreExternalState || (() => {}),
        },
    );
}

function mountList(keys: readonly string[], options: ListOptions = {}): HTMLElement {
    const content = buildList(keys, options);
    content.insertAdjacentHTML(
        'beforeend',
        '<span class="ro-count" data-ro-live-region="count">old</span>' +
            '<div class="ro-phase-strip" data-ro-live-region="phase"></div>' +
            '<span class="ro-foundline" data-ro-live-region="found">old</span>' +
            '<div id="ro-live-status"></div>',
    );
    document.body.append(content);
    adoptListProjection(content);
    return content;
}

function remountList(keys: readonly string[], options: ListOptions = {}): HTMLElement {
    document.body.replaceChildren();
    resetListProjection();
    return mountList(keys, options);
}

function expectDeltaError(
    result: ListProjectionDeltaResult,
    code: string,
    message: string,
    fatal = false,
): void {
    expect(result).toStrictEqual({ ok: false, error: { code, message, fatal } });
}

beforeEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    document.body.replaceChildren();
    resetListProjection();
    setListProjectionVisibleKeys(null);
});

describe('complete snapshot adoption', () => {
    test('owns small-list rows, order, keyed cards and the stable row-model facade', () => {
        const content = buildList(['prod/a', 'prod/b'], { keyedCards: true });
        document.body.append(content);
        const modelFacade = window.roRowModel;
        setListProjectionVisibleKeys(new Set(['stale-page/key']));

        adoptListProjection(content);

        expect(listProjectionWindowed()).toBe(false);
        expect(listProjectionOrder()).toStrictEqual(['prod/a', 'prod/b']);
        expect(listProjectionRows()).toStrictEqual(
            Array.from(content.querySelectorAll('tbody tr[data-key]')),
        );
        expect(listProjectionRowByKey('prod/b')).toBe(
            content.querySelector('tr[data-key="prod/b"]'),
        );
        expect(listProjectionCardByKey('prod/a')).toBe(
            content.querySelector('.ro-pcard[data-key="prod/a"]'),
        );
        expect(listProjectionRowModel()).toBe(modelFacade);
        expect(window.roRowModel).toBe(modelFacade);
        expect(modelFacade).toStrictEqual({
            fields: [
                { hint: 'string', label: 'Name', name: 'name' },
                { hint: 'enum', label: 'Status', name: 'status' },
            ],
            rows: [
                { cells: ['Workload 0', 'Ready 0'], key: 'prod/a', name: 'Workload 0' },
                { cells: ['Workload 1', 'Ready 1'], key: 'prod/b', name: 'Workload 1' },
            ],
            visibleKeys: null,
        });
    });

    test('indexes legacy unkeyed cards positionally only for a complete 1:1 list', () => {
        const content = buildList(['a', 'b']);
        const extra = document.createElement('div');
        extra.className = 'ro-pcard';
        content.querySelector('.ro-cardlist')?.append(extra);

        adoptListProjection(content);
        expect(listProjectionCardByKey('a')).toBeNull();

        extra.remove();
        adoptListProjection(content);
        expect(listProjectionCardByKey('a')).toHaveTextContent('Card a');
        expect(listProjectionCardByKey('b')).toHaveTextContent('Card b');
    });

    test('does not reinterpret a partial or already-keyed card snapshot positionally', () => {
        const partial = buildList(['a', 'b'], { keyedCards: true });
        partial.querySelector('.ro-pcard[data-key="b"]')?.remove();

        adoptListProjection(partial);
        expect(listProjectionCardByKey('a')).toHaveTextContent('Card a');
        expect(listProjectionCardByKey('b')).toBeNull();

        const reorderedKeys = buildList(['a', 'b'], { keyedCards: true });
        const cards = reorderedKeys.querySelector('.ro-cardlist') as HTMLElement;
        cards.prepend(cards.lastElementChild as Element);

        adoptListProjection(reorderedKeys);
        expect(listProjectionCardByKey('a')?.dataset.key).toBe('a');
        expect(listProjectionCardByKey('b')?.dataset.key).toBe('b');
    });

    test('advances revision once per adopted, prepared, or reset projection', () => {
        const first = buildList(['a'], { keyedCards: true });
        const incoming = buildList(['b'], { keyedCards: true });
        const start = listProjectionRevision();

        adoptListProjection(first);
        expect(listProjectionRevision()).toBe(start + 1);

        prepareListProjectionSwap(incoming);
        expect(listProjectionRevision()).toBe(start + 2);
        expect(listProjectionSwapPending()).toBe(true);
        prepareListProjectionSwap(incoming);
        expect(listProjectionRevision()).toBe(start + 2);

        resetListProjection();
        expect(listProjectionRevision()).toBe(start + 3);
        expect(listProjectionWindowed()).toBe(false);
        expect(listProjectionRowModel()).toMatchObject({ fields: [], rows: [] });
    });

    test('commits a small morph to connected nodes rather than throwaway fragment nodes', () => {
        const oldContent = buildList(['a', 'b'], { keyedCards: true });
        document.body.append(oldContent);
        adoptListProjection(oldContent);

        const fragment = document.createDocumentFragment();
        const incoming = buildList(['b', 'a'], { keyedCards: true });
        fragment.append(incoming);
        const throwawayB = incoming.querySelector('tr[data-key="b"]');
        prepareListProjectionSwap(fragment);

        // Idiomorph may retain/move existing identities. Model that by mounting
        // a different set of connected nodes than the source fragment held.
        const landed = buildList(['b', 'a'], { keyedCards: true });
        landed.querySelector('tr[data-key="b"] a')?.replaceChildren('Landed B');
        document.body.replaceChildren(landed);
        expect(commitListProjectionSwap()).not.toBeNull();

        expect(listProjectionOrder()).toStrictEqual(['b', 'a']);
        expect(listProjectionRowByKey('b')).toBe(landed.querySelector('tr[data-key="b"]'));
        expect(listProjectionRowByKey('b')).not.toBe(throwawayB);
        expect(listProjectionRowByKey('b')?.isConnected).toBe(true);
        expect(listProjectionRowModel().rows[0]?.name).toBe('Landed B');
        expect(listProjectionSwapPending()).toBe(false);
        expect(commitListProjectionSwap()).toBeNull();
    });

    test('keeps a detached windowed fragment as its root when no content mount landed', () => {
        const incoming = buildList(['a'], { windowed: true });
        prepareListProjectionSwap(incoming);

        expect(commitListProjectionSwap()).not.toBeNull();
        expect(ensureListProjection(incoming)).toBe(false);
        expect(listProjectionWindowed()).toBe(true);
    });
});

describe('windowed and history snapshots', () => {
    test('keeps every prepared row and model entry authoritative while nodes are off-DOM', () => {
        const fragment = document.createDocumentFragment();
        const incoming = buildList(['a', 'b', 'c', 'd'], { windowed: true });
        fragment.append(incoming);
        const priorVisibility = new Set(['old-page/key']);
        setListProjectionVisibleKeys(priorVisibility);

        const firstPreparation = prepareListProjectionSwap(fragment);
        expect(listProjectionRowModel().visibleKeys).toBe(priorVisibility);
        const preparedRows = [...firstPreparation.rows];
        const tbody = incoming.querySelector('tbody') as HTMLTableSectionElement;
        const top = document.createElement('tr');
        top.className = 'ro-vspacer';
        top.append(document.createElement('td'));
        const bottom = top.cloneNode(true) as HTMLTableRowElement;
        tbody.replaceChildren(top, bottom);

        // The second projection call made by the virtualizer must not recapture
        // the spacer-only fragment and erase the complete snapshot.
        const secondPreparation = prepareListProjectionSwap(fragment);
        expect(secondPreparation.rows).toBe(firstPreparation.rows);
        document.body.append(incoming);
        expect(commitListProjectionSwap()).not.toBeNull();
        expect(listProjectionRowModel().visibleKeys).toBe(priorVisibility);

        expect(listProjectionWindowed()).toBe(true);
        expect(listProjectionOrder()).toStrictEqual(['a', 'b', 'c', 'd']);
        expect(listProjectionRows()).toStrictEqual(preparedRows);
        expect(preparedRows.every((row) => !row.isConnected)).toBe(true);
        expect(listProjectionRowModel().rows.map((row) => row.key)).toStrictEqual([
            'a',
            'b',
            'c',
            'd',
        ]);
        setListProjectionVisibleKeys(new Set(['d']));
        expect(listProjectionVisibleRows()).toStrictEqual([preparedRows[3]]);
        expect(listProjectionRowByKey('d')).toBe(preparedRows[3]);
    });

    test('rejects history-restored spacer slices idempotently instead of adopting partial rows', () => {
        const complete = buildList(['old-a', 'old-b'], { windowed: true });
        adoptListProjection(complete);
        const facade = listProjectionRowModel();

        const cached = buildList(['cached-only'], { windowed: true });
        const tbody = cached.querySelector('tbody') as HTMLTableSectionElement;
        const spacer = document.createElement('tr');
        spacer.className = 'ro-vspacer';
        spacer.append(document.createElement('td'));
        tbody.prepend(spacer);

        expect(ensureListProjection(cached)).toBe(true);
        expect(ensureListProjection(cached)).toBe(false);

        expect(listProjectionRows()).toStrictEqual([]);
        expect(listProjectionOrder()).toStrictEqual([]);
        expect(listProjectionRowByKey('cached-only')).toBeNull();
        expect(listProjectionRowModel()).toBe(facade);
        expect(facade.fields).toStrictEqual([]);
        expect(facade.rows).toStrictEqual([]);
        expect(facade.visibleKeys).toBeNull();
    });

    test('reset clears the full projection and stale visibility together', () => {
        const content = buildList(['a'], { keyedCards: true });
        adoptListProjection(content);
        setListProjectionVisibleKeys(new Set(['a']));

        resetListProjection();

        expect(listProjectionRows()).toStrictEqual([]);
        expect(listProjectionRowModel().visibleKeys).toBeNull();
    });
});

describe('delta fragment validation', () => {
    test('preserves defensive canonical row IDs through a whitespace-wrapped transaction', () => {
        // This exercises the transaction's client-side ID defense directly;
        // it does not broaden which keys the wire protocol promises to send.
        const keys = ['safe\u0001 key', 'quote"key', 'slash\\key', 'percent%key', 'del\u007fkey'];
        for (const key of keys) {
            document.body.replaceChildren();
            mountList([key], { windowed: true });
            const result = applyDelta(
                { upsert: [{ key, row: ` \n${rowFragment(key)}\t ` }] },
                { morph: morphInPlace },
            );
            expect(result).toMatchObject({ ok: true, summary: { updated: 1 } });
            expect(listProjectionRowByKey(key)?.id).toBe(rowDOMID(key));
        }
    });

    test('rejects zero, multiple, text, and comment roots as one canonical row', () => {
        mountList(['a'], { windowed: true });
        const invalid = [
            '',
            `${rowFragment('a')} ${rowFragment('a')}`,
            `stray${rowFragment('a')}`,
            `<!-- stray -->${rowFragment('a')}`,
            `<!---->${rowFragment('a')}`,
        ];
        for (const row of invalid) {
            expectDeltaError(
                applyDelta({ upsert: [{ key: 'a', row }] }, { morph: morphInPlace }),
                'fragment-invalid',
                'row fragment for a is not one canonical keyed tr',
            );
        }
    });

    test('rejects every non-canonical row and card root contract independently', () => {
        mountList(['a'], { keyedCards: true });
        const invalidRows = [
            '<div id="row-a" data-key="a"></div>',
            rowFragment('wrong'),
            rowFragment('a').replace('data-key="a"', 'data-key="wrong"'),
            rowFragment('a').replace('id="row-a"', 'id="wrong"'),
            rowFragment('a').replace('data-key="a"', 'data-key="a" data-ro-live-region="count"'),
            rowFragment('a').replace('<tr ', '<tr class="ro-vspacer" '),
            rowFragment('a', 'A', '<i id="nested"></i>'),
            rowFragment('a', 'A', '<i data-ro-live-region="count"></i>'),
            rowFragment('a', 'A', '<script>bad()</script>'),
        ];
        for (const row of invalidRows) {
            expectDeltaError(
                applyDelta({ upsert: [{ key: 'a', row, card: cardFragment('a') }] }),
                'fragment-invalid',
                'row fragment for a is not one canonical keyed tr',
            );
        }

        const invalidCards = [
            '<article class="ro-pcard" data-key="a"></article>',
            '<div data-key="a"></div>',
            cardFragment('wrong'),
            cardFragment('a').replace('<div ', '<div id="card-a" '),
            cardFragment('a').replace('<div ', '<div data-ro-live-region="count" '),
            cardFragment('a', 'A', '<i id="nested"></i>'),
            cardFragment('a', 'A', '<i data-ro-live-region="count"></i>'),
            cardFragment('a', 'A', '<iframe></iframe>'),
        ];
        for (const card of invalidCards) {
            expectDeltaError(
                applyDelta({ upsert: [{ key: 'a', row: rowFragment('a'), card }] }),
                'fragment-invalid',
                'card fragment for a is not one canonical keyed card',
            );
        }

        expect(
            applyDelta(
                {
                    upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
                },
                { morph: morphInPlace },
            ).ok,
        ).toBe(true);
    });

    test('validates fixed-region roots, mounts, classes, identities, and safe descendants', () => {
        const content = mountList(['a'], { windowed: true });
        const validRegions = [
            {
                region: 'count' as const,
                html: '<span class="ro-count" data-ro-live-region="count">1</span>',
            },
            {
                region: 'phase' as const,
                html: '<div class="ro-phase-strip" data-ro-live-region="phase"><b>ok</b></div>',
            },
            {
                region: 'found' as const,
                html: '<span class="ro-foundline" data-ro-live-region="found">found</span>',
            },
        ];
        expect(applyDelta({ regions: validRegions }, { morph: morphInPlace })).toMatchObject({
            ok: true,
            summary: { regions: ['count', 'phase', 'found'] },
        });
        expect(content.querySelector('#ro-live-status')?.textContent).toBe(
            'Live update: 3 regions',
        );

        const malformed = [
            '<div class="ro-count" data-ro-live-region="count"></div>',
            '<span class="wrong" data-ro-live-region="count"></span>',
            '<span class="ro-count" data-ro-live-region="found"></span>',
            '<span id="new-count" class="ro-count" data-ro-live-region="count"></span>',
            '<span class="ro-count" data-ro-live-region="count"><i id="nested"></i></span>',
            '<span class="ro-count" data-ro-live-region="count"><i data-ro-live-region="found"></i></span>',
            '<span class="ro-count" data-ro-live-region="count"><object></object></span>',
            'stray<span class="ro-count" data-ro-live-region="count"></span>',
        ];
        for (const html of malformed) {
            expectDeltaError(
                applyDelta({ regions: [{ region: 'count', html }] }),
                'fragment-invalid',
                'region count is not one canonical fixed-region root',
            );
        }

        const count = content.querySelector('[data-ro-live-region="count"]') as HTMLElement;
        count.remove();
        expectDeltaError(
            applyDelta({ regions: [validRegions[0]] }),
            'projection-mismatch',
            'region count does not have exactly one fixed mount',
        );
        content.append(count, count.cloneNode(true));
        expectDeltaError(
            applyDelta({ regions: [validRegions[0]] }),
            'projection-mismatch',
            'region count does not have exactly one fixed mount',
        );

        content.querySelectorAll('[data-ro-live-region="count"]').item(1).remove();
        const wrongTag = document.createElement('div');
        wrongTag.className = 'ro-count';
        wrongTag.dataset.roLiveRegion = 'count';
        count.replaceWith(wrongTag);
        expectDeltaError(
            applyDelta({ regions: [validRegions[0]] }),
            'fragment-invalid',
            'region count is not one canonical fixed-region root',
        );
        wrongTag.replaceWith(count);
        count.classList.remove('ro-count');
        expectDeltaError(
            applyDelta({ regions: [validRegions[0]] }),
            'fragment-invalid',
            'region count is not one canonical fixed-region root',
        );
    });

    test('allows only the bounded capacity-bar style grammar', () => {
        mountList(['a'], { windowed: true });
        for (const style of ['width:0%', 'width : 42% ;', 'width:99%;', 'width:100%']) {
            const row = rowFragment(
                'a',
                'A',
                `<span class="cap-bar"><i style="${style}"></i></span>`,
            );
            expect(applyDelta({ upsert: [{ key: 'a', row }] }, { morph: morphInPlace }).ok).toBe(
                true,
            );
        }
        for (const body of [
            '<i style="width:42%"></i>',
            '<span class="cap-bar"><b style="width:42%"></b></span>',
            '<span class="wrong"><i style="width:42%"></i></span>',
            '<span class="cap-bar"><i style="xwidth:42%"></i></span>',
            '<span class="cap-bar"><i style="widthx:42%"></i></span>',
            '<span class="cap-bar"><i style="width:x42%"></i></span>',
            '<span class="cap-bar"><i style="width:42%x"></i></span>',
            '<span class="cap-bar"><i style="width:101%"></i></span>',
        ]) {
            expectDeltaError(
                applyDelta({ upsert: [{ key: 'a', row: rowFragment('a', 'A', body) }] }),
                'fragment-invalid',
                'row fragment for a is not one canonical keyed tr',
            );
        }
    });

    test('rejects executable attributes after stripping URL control characters', () => {
        mountList(['a'], { windowed: true });
        const unsafeBodies = [
            '<i srcdoc="bad"></i>',
            '<i onclick="bad()"></i>',
            '<a href="javascript:bad()">x</a>',
            '<img src="vbscript:bad()">',
            '<svg><a xlink:href="data:text/html,bad">x</a></svg>',
            '<form action="java&#x20;script:bad()"></form>',
            '<button formaction="java&#x1f;script:bad()"></button>',
            '<a href="java&#x7f;script:bad()">x</a>',
            '<a href="java\u0080script:bad()">x</a>',
            '<a href="java\u009fscript:bad()">x</a>',
        ];
        for (const body of unsafeBodies) {
            expectDeltaError(
                applyDelta({ upsert: [{ key: 'a', row: rowFragment('a', 'A', body) }] }),
                'fragment-invalid',
                'row fragment for a is not one canonical keyed tr',
            );
        }

        const safeRow = rowFragment(
            'a',
            'A',
            '<a href="https://example.test/javascript:still-a-path">https</a>' +
                '<a href="java\u00a0script:still-safe">unicode</a>' +
                '<img src="data:image/png;base64,AA=="><i class="javascript:" title="onclick"></i>',
        );
        expect(
            applyDelta({ upsert: [{ key: 'a', row: safeRow }] }, { morph: morphInPlace }).ok,
        ).toBe(true);
    });

    test('enforces the production parsed-node ceiling through delta validation', () => {
        const bodyAtLimit = '<i></i>'.repeat(4091);
        mountList(['a'], { windowed: true });
        expect(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'A', bodyAtLimit) }] },
                { morph: morphInPlace },
            ).ok,
        ).toBe(true);

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a', 'A', `${bodyAtLimit}<i></i>`) }],
            }),
            'fragment-invalid',
            'row fragment for a is not one canonical keyed tr',
        );
    });

    test('enforces the production parsed-depth ceiling through delta validation', () => {
        const nested = (levels: number) => '<i>'.repeat(levels) + '</i>'.repeat(levels);
        mountList(['a'], { windowed: true });
        expect(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'A', nested(62)) }] },
                { morph: morphInPlace },
            ).ok,
        ).toBe(true);

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a', 'A', nested(63)) }] }),
            'fragment-invalid',
            'row fragment for a is not one canonical keyed tr',
        );
    });

    test('enforces individual UTF-8 and aggregate fragment byte limits at the exact boundary', () => {
        mountList(['a'], { windowed: true });
        const base = rowFragment('a', 'A', '');
        const baseBytesWithoutPayload = new TextEncoder().encode(base).byteLength - 'Ready'.length;
        const fill = (bytes: number) =>
            base.replace('Ready', 'x'.repeat(bytes - baseBytesWithoutPayload));
        const exactASCII = fill(128 * 1024);
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: exactASCII }] }),
            'morph-failed',
            'Idiomorph is unavailable',
        );
        const encode = vi.spyOn(TextEncoder.prototype, 'encode');
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: `${exactASCII}x` }] }),
            'limit-exceeded',
            'Live delta fragment exceeds its limit',
        );
        expect(encode).not.toHaveBeenCalled();
        encode.mockRestore();

        const multibyte = base.replace('Ready', 'é'.repeat(64 * 1024));
        expect(multibyte.length).toBeLessThanOrEqual(128 * 1024);
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: multibyte }] }),
            'limit-exceeded',
            'Live delta fragment exceeds its limit',
        );

        const regionShell = '<span class="ro-count" data-ro-live-region="count"></span>';
        const region = regionShell.replace(
            '></span>',
            `>${'x'.repeat(128 * 1024 - regionShell.length)}</span>`,
        );
        expect(new TextEncoder().encode(region).byteLength).toBe(128 * 1024);
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: exactASCII }],
                regions: [{ region: 'count', html: `${region}x` }],
            }),
            'limit-exceeded',
            'Live delta fragment exceeds its limit',
        );
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: exactASCII }],
                regions: [{ region: 'count', html: region }],
            }),
            'morph-failed',
            'Idiomorph is unavailable',
        );
        expectDeltaError(
            applyDelta({
                upsert: [
                    { key: 'a', row: exactASCII },
                    { key: 'a', row: rowFragment('a') },
                ],
                regions: [{ region: 'count', html: region }],
            }),
            'limit-exceeded',
            'Live delta fragments exceed their aggregate limit',
        );

        document.body.replaceChildren();
        mountList(['a'], { keyedCards: true });
        const cardBase = cardFragment('a');
        const cardBytesWithoutPayload =
            new TextEncoder().encode(cardBase).byteLength - 'Next a'.length;
        const oversizedCard = cardBase.replace(
            'Next a',
            'x'.repeat(128 * 1024 + 1 - cardBytesWithoutPayload),
        );
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a'), card: oversizedCard }],
            }),
            'limit-exceeded',
            'Live delta fragment exceeds its limit',
        );
    });
});

describe('delta projection validation and application', () => {
    const reorder = () => applyDelta({ order: ['b', 'a'] });

    test('rejects missing, replaced, detached, or swap-pending projection roots', () => {
        let content = remountList(['a'], { windowed: true });
        content.remove();
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'resource list content is missing',
        );

        content = remountList(['a'], { windowed: true });
        const replacement = buildList(['a'], { windowed: true });
        content.replaceWith(replacement);
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'canonical projection is not stably mounted',
        );

        content = remountList(['a'], { windowed: true });
        content.remove();
        const getElementById = vi
            .spyOn(document, 'getElementById')
            .mockImplementation((id) => (id === 'resource-list-content' ? content : null));
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'canonical projection is not stably mounted',
        );
        getElementById.mockRestore();

        remountList(['a'], { windowed: true });
        prepareListProjectionSwap(buildList(['b'], { windowed: true }));
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'canonical projection is not stably mounted',
        );
    });

    test('rejects empty, mixed, or ambiguous projection modes and mounts', () => {
        remountList([], { windowed: true });
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'empty projections require a snapshot',
        );

        let content = remountList(['a'], { windowed: true });
        const card = document.createElement('div');
        card.className = 'ro-pcard';
        card.dataset.key = 'a';
        content.querySelector('.ro-cardlist')?.append(card);
        adoptListProjection(content);
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'windowed projection unexpectedly has cards',
        );

        content = remountList(['a', 'b']);
        content.querySelector('.ro-pcard:last-child')?.remove();
        adoptListProjection(content);
        expectDeltaError(
            applyDelta({ regions: [] }),
            'projection-mismatch',
            'projection mode is not delta-capable',
        );

        content = remountList(['a', 'b'], { keyedCards: true });
        content.append(content.querySelector('table.ro-table')?.cloneNode(true) as Node);
        expectDeltaError(reorder(), 'projection-mismatch', 'projection table mount is ambiguous');

        content = remountList(['a', 'b'], { keyedCards: true });
        content.querySelector('tbody')?.remove();
        expectDeltaError(reorder(), 'projection-mismatch', 'projection table mount is ambiguous');

        content = remountList(['a', 'b'], { keyedCards: true });
        content.append(content.querySelector('.ro-cardlist')?.cloneNode(true) as Node);
        expectDeltaError(reorder(), 'projection-mismatch', 'card mount is ambiguous');
    });

    test('audits every externally mutable canonical row and model invariant', () => {
        remountList(['a', 'b'], { windowed: true });
        const duplicateOrder = listProjectionOrder() as string[];
        const duplicateRows = listProjectionRows() as HTMLElement[];
        const duplicateModels = listProjectionRowModel().rows;
        duplicateOrder[1] = 'a';
        duplicateRows[1] = duplicateRows[0] as HTMLElement;
        duplicateModels[1] = duplicateModels[0] as (typeof duplicateModels)[number];
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        const extraRow = (listProjectionRowByKey('a') as HTMLElement).cloneNode(
            true,
        ) as HTMLElement;
        (listProjectionRows() as HTMLElement[]).push(extraRow);
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        listProjectionRowModel().rows.push({
            ...(listProjectionRowModel().rows[0] as (typeof duplicateModels)[number]),
        });
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        (listProjectionRows() as unknown as Array<HTMLElement | undefined>)[0] = undefined;
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        (
            listProjectionRowModel().rows as unknown as Array<
                (typeof duplicateModels)[number] | undefined
            >
        )[0] = undefined;
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        (listProjectionRowByKey('a') as HTMLElement).dataset.key = 'wrong';
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        (listProjectionRowByKey('a') as HTMLElement).id = 'wrong';
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );

        remountList(['a', 'b'], { windowed: true });
        (listProjectionRowModel().rows[0] as { key: string }).key = 'wrong';
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical projection invariants are broken',
        );
    });

    test('audits card and table placement separately for small and windowed lists', () => {
        let content = remountList(['a', 'b'], { keyedCards: true });
        (listProjectionCardByKey('a') as HTMLElement).dataset.key = 'wrong';
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical keyed-card invariants are broken',
        );

        content = remountList(['a', 'b'], { keyedCards: true });
        content.append(listProjectionCardByKey('a') as HTMLElement);
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'canonical keyed-card invariants are broken',
        );

        content = remountList(['a', 'b'], { keyedCards: true });
        content.append(listProjectionRowByKey('a') as HTMLElement);
        expectDeltaError(reorder(), 'projection-mismatch', 'small-list rows are not fully mounted');

        content = remountList(['a', 'b'], { windowed: true });
        content.append(listProjectionRowByKey('a') as HTMLElement);
        expectDeltaError(
            reorder(),
            'projection-mismatch',
            'windowed rows are mounted outside tbody',
        );
    });

    test('applies one atomic small-list topology, identity, model, and status transaction', () => {
        const content = remountList(['a', 'b', 'c'], { keyedCards: true });
        const oldA = listProjectionRowByKey('a');
        const oldCardA = listProjectionCardByKey('a');
        const morph = vi.fn(morphInPlace);
        const oldRevision = listProjectionRevision();

        const result = applyDelta(
            {
                remove: [
                    { key: 'b', cause: 'delete' },
                    { key: 'c', cause: 'project' },
                ],
                upsert: [
                    { key: 'a', row: rowFragment('a', 'Updated A'), card: cardFragment('a') },
                    { key: 'd', row: rowFragment('d', 'Inserted D'), card: cardFragment('d') },
                ],
                order: ['d', 'a'],
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            },
            { morph },
        );

        expect(result).toStrictEqual({
            ok: true,
            summary: {
                inserted: 1,
                updated: 1,
                deleted: 1,
                projected: 1,
                reordered: true,
                regions: ['count'],
            },
        });
        expect(morph).toHaveBeenCalledTimes(3);
        expect(listProjectionRowByKey('a')).toBe(oldA);
        expect(listProjectionCardByKey('a')).toBe(oldCardA);
        expect(listProjectionOrder()).toStrictEqual(['d', 'a']);
        expect(listProjectionRevision()).toBe(oldRevision + 1);
        expect(listProjectionWindowed()).toBe(false);
        expect(listProjectionRowModel().rows.map((row) => row.name)).toStrictEqual([
            'Inserted D',
            'Updated A',
        ]);
        expect(
            Array.from(content.querySelectorAll('tbody > tr')).map((row) => row.id),
        ).toStrictEqual(['row-d', 'row-a']);
        expect(
            Array.from(content.querySelectorAll('.ro-cardlist > .ro-pcard')).map(
                (card) => (card as HTMLElement).dataset.key,
            ),
        ).toStrictEqual(['d', 'a']);
        expect(content.querySelector('#ro-live-status')?.textContent).toBe(
            'Live update: 4 rows, order changed, 1 region',
        );
    });

    test('treats insertion-only and removal-only plans as independent topology changes', () => {
        remountList(['a', 'b'], { keyedCards: true });
        expect(
            applyDelta({
                upsert: [{ key: 'c', row: rowFragment('c'), card: cardFragment('c') }],
                order: ['a', 'b', 'c'],
            }),
        ).toStrictEqual({
            ok: true,
            summary: {
                inserted: 1,
                updated: 0,
                deleted: 0,
                projected: 0,
                reordered: true,
                regions: [],
            },
        });
        expect(listProjectionOrder()).toStrictEqual(['a', 'b', 'c']);

        remountList(['a', 'b'], { keyedCards: true });
        expect(applyDelta({ remove: [{ key: 'b', cause: 'delete' }], order: ['a'] })).toStrictEqual(
            {
                ok: true,
                summary: {
                    inserted: 0,
                    updated: 0,
                    deleted: 1,
                    projected: 0,
                    reordered: true,
                    regions: [],
                },
            },
        );
        expect(listProjectionOrder()).toStrictEqual(['a']);
        expect(listProjectionRowByKey('b')).toBeNull();
        expect(listProjectionCardByKey('b')).toBeNull();
        expect(listProjectionRowModel().rows.map((row) => row.key)).toStrictEqual(['a']);
    });

    test('rolls back structural row and card topology after reconciliation fails', () => {
        const content = remountList(['a', 'b'], { keyedCards: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const cardMount = content.querySelector('.ro-cardlist') as HTMLElement;
        const originalRows = Array.from(tbody.children);
        const originalCards = Array.from(cardMount.children);
        const originalB = listProjectionRowByKey('b');
        const originalCardB = listProjectionCardByKey('b');
        const originalRevision = listProjectionRevision();

        expectDeltaError(
            applyDelta(
                {
                    remove: [{ key: 'b', cause: 'delete' }],
                    upsert: [{ key: 'c', row: rowFragment('c'), card: cardFragment('c') }],
                    order: ['c', 'a'],
                },
                {
                    reconcile() {
                        throw new Error('reconcile failed');
                    },
                },
            ),
            'reconcile-failed',
            'Live delta reconcile failed and was rolled back',
        );
        expect(Array.from(tbody.children)).toStrictEqual(originalRows);
        expect(Array.from(cardMount.children)).toStrictEqual(originalCards);
        expect(listProjectionOrder()).toStrictEqual(['a', 'b']);
        expect(listProjectionRowByKey('b')).toBe(originalB);
        expect(listProjectionCardByKey('b')).toBe(originalCardB);
        expect(listProjectionRowByKey('c')).toBeNull();
        expect(listProjectionCardByKey('c')).toBeNull();
        expect(listProjectionRowModel().rows.map((row) => row.key)).toStrictEqual(['a', 'b']);
        expect(listProjectionRevision()).toBe(originalRevision);
    });

    test('restores fast row placement dependencies and contents after a morph throws', () => {
        const content = remountList(['a', 'b'], { keyedCards: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const rowA = listProjectionRowByKey('a') as HTMLElement;
        const rowB = listProjectionRowByKey('b') as HTMLElement;
        const holding = document.createDocumentFragment();

        expectDeltaError(
            applyDelta(
                {
                    upsert: [
                        { key: 'a', row: rowFragment('a', 'New A'), card: cardFragment('a') },
                        { key: 'b', row: rowFragment('b', 'New B'), card: cardFragment('b') },
                    ],
                },
                {
                    morph(current, incoming) {
                        if (current === rowA) {
                            morphInPlace(current, incoming);
                            holding.append(rowA, rowB);
                            throw new Error('row roots moved');
                        }
                        morphInPlace(current, incoming);
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(Array.from(tbody.children)).toStrictEqual([rowA, rowB]);
        expect(rowA).toHaveTextContent('Workload 0');
        expect(rowB).toHaveTextContent('Workload 1');
        expect(holding.childNodes).toHaveLength(0);
    });

    test('restores each adjacent fast placement once after a morph throws', () => {
        const keys = ['a', 'b', 'c', 'd'];
        const content = remountList(keys, { keyedCards: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const cardMount = content.querySelector('.ro-cardlist') as HTMLElement;
        const rows = keys.map((key) => listProjectionRowByKey(key) as HTMLElement);
        const cards = keys.map((key) => listProjectionCardByKey(key) as HTMLElement);
        const rowNodes = new Set<Node>(rows);
        const holding = document.createDocumentFragment();
        const nativeInsertBefore = Node.prototype.insertBefore;
        let rowRestoreCalls = 0;
        const insertBefore = vi.spyOn(Node.prototype, 'insertBefore').mockImplementation(function <
            T extends Node,
        >(this: Node, node: T, child: Node | null): T {
            if (this === tbody && rowNodes.has(node)) rowRestoreCalls += 1;
            return nativeInsertBefore.call(this, node, child) as T;
        });

        const result = applyDelta(
            {
                upsert: keys.map((key) => ({
                    key,
                    row: rowFragment(key, `New ${key}`),
                    card: cardFragment(key),
                })),
            },
            {
                morph(current) {
                    if (current === rows[0]) {
                        holding.append(...rows);
                        throw new Error('adjacent row roots moved');
                    }
                },
            },
        );
        insertBefore.mockRestore();

        expectDeltaError(result, 'morph-failed', 'Live delta DOM morph failed and was rolled back');
        expect(Array.from(tbody.children)).toStrictEqual(rows);
        expect(Array.from(cardMount.children)).toStrictEqual(cards);
        expect(holding.childNodes).toHaveLength(0);
        expect(rowRestoreCalls).toBe(rows.length);
    });

    test('restores fast card placement dependencies and contents after a morph throws', () => {
        const content = remountList(['a', 'b'], { keyedCards: true });
        const cardMount = content.querySelector('.ro-cardlist') as HTMLElement;
        const cardA = listProjectionCardByKey('a') as HTMLElement;
        const cardB = listProjectionCardByKey('b') as HTMLElement;
        const holding = document.createDocumentFragment();

        expectDeltaError(
            applyDelta(
                {
                    upsert: [
                        { key: 'a', row: rowFragment('a', 'New A'), card: cardFragment('a') },
                        { key: 'b', row: rowFragment('b', 'New B'), card: cardFragment('b') },
                    ],
                },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        if (current === cardA) {
                            holding.append(cardA, cardB);
                            throw new Error('card roots moved');
                        }
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(Array.from(cardMount.children)).toStrictEqual([cardA, cardB]);
        expect(cardA).toHaveTextContent('Card a');
        expect(cardB).toHaveTextContent('Card b');
        expect(holding.childNodes).toHaveLength(0);
    });

    test('restores fixed-region identity, contents, parent, and sibling position', () => {
        const content = remountList(['a'], { windowed: true });
        const count = content.querySelector('[data-ro-live-region="count"]') as HTMLElement;
        const phase = content.querySelector('[data-ro-live-region="phase"]') as HTMLElement;
        const restoreParent = vi.spyOn(content, 'replaceChildren');

        expectDeltaError(
            applyDelta(
                {
                    regions: [
                        {
                            region: 'count',
                            html: '<span class="ro-count" data-ro-live-region="count">9</span>',
                        },
                    ],
                },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        current.replaceWith(document.createElement('i'));
                        throw new Error('region replaced');
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(content.querySelector('[data-ro-live-region="count"]')).toBe(count);
        expect(count.textContent).toBe('old');
        expect(count.nextElementSibling).toBe(phase);
        expect(restoreParent).toHaveBeenCalledOnce();
    });

    test('restores a fixed region independently when the live-status mount is absent', () => {
        const content = remountList(['a'], { windowed: true });
        content.querySelector('#ro-live-status')?.remove();
        const count = content.querySelector('[data-ro-live-region="count"]') as HTMLElement;

        expectDeltaError(
            applyDelta(
                {
                    regions: [
                        {
                            region: 'count',
                            html: '<span class="ro-count" data-ro-live-region="count">9</span>',
                        },
                    ],
                },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        current.replaceWith(document.createElement('i'));
                        throw new Error('region replaced without status');
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(content.querySelector('[data-ro-live-region="count"]')).toBe(count);
        expect(count.textContent).toBe('old');
    });

    test('restores virtual spacers and bulk-bar identity after reconciliation fails', () => {
        const content = remountList(['a'], { windowed: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const spacer = tbody.insertRow();
        spacer.className = 'ro-vspacer';
        spacer.innerHTML = '<td>original gap</td>';
        const bulk = document.createElement('div');
        bulk.id = 'ro-bulkbar';
        bulk.dataset.state = 'original';
        bulk.innerHTML = '<span>original bulk</span>';
        content.append(bulk);
        const status = content.querySelector('#ro-live-status') as HTMLElement;
        status.textContent = 'original status';
        const statusText = status.firstChild;

        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Updated') }] },
                {
                    morph: morphInPlace,
                    reconcile() {
                        spacer.className = 'poison';
                        spacer.replaceChildren(document.createElement('td'));
                        bulk.dataset.state = 'poison';
                        bulk.replaceChildren('poison bulk');
                        bulk.replaceWith(document.createElement('aside'));
                        status.replaceChildren('poison status');
                        status.replaceWith(document.createElement('output'));
                        throw new Error('reconcile failed');
                    },
                },
            ),
            'reconcile-failed',
            'Live delta reconcile failed and was rolled back',
        );
        expect(tbody.querySelector('tr.ro-vspacer')).toBe(spacer);
        expect(spacer.outerHTML).toBe('<tr class="ro-vspacer"><td>original gap</td></tr>');
        expect(content.querySelector('#ro-bulkbar')).toBe(bulk);
        expect(bulk.dataset.state).toBe('original');
        expect(bulk.textContent).toBe('original bulk');
        expect(content.querySelector('#ro-live-status')).toBe(status);
        expect(status.firstChild).toBe(statusText);
        expect(status.textContent).toBe('original status');
        expect(listProjectionRowByKey('a')).toHaveTextContent('Workload 0');
    });

    test('restores the bulk bar independently when the live-status mount is absent', () => {
        const content = remountList(['a'], { windowed: true });
        content.querySelector('#ro-live-status')?.remove();
        const bulk = document.createElement('div');
        bulk.id = 'ro-bulkbar';
        bulk.textContent = 'original bulk';
        content.append(bulk);

        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Updated') }] },
                {
                    morph: morphInPlace,
                    reconcile() {
                        bulk.replaceChildren('poison bulk');
                        bulk.replaceWith(document.createElement('aside'));
                        throw new Error('bulk replaced without status');
                    },
                },
            ),
            'reconcile-failed',
            'Live delta reconcile failed and was rolled back',
        );
        expect(content.querySelector('#ro-bulkbar')).toBe(bulk);
        expect(bulk.textContent).toBe('original bulk');
    });

    test('restores absent and present class and active-descendant attributes', () => {
        const content = remountList(['a', 'b'], { keyedCards: true });
        const rowA = listProjectionRowByKey('a') as HTMLElement;
        const cardA = listProjectionCardByKey('a') as HTMLElement;
        const wrap = content.querySelector('.ro-table-wrap') as HTMLElement;
        wrap.setAttribute('aria-activedescendant', 'row-a');

        expectDeltaError(
            applyDelta(
                { order: ['b', 'a'] },
                {
                    reconcile() {
                        rowA.className = 'poison-row';
                        cardA.className = 'poison-card';
                        wrap.setAttribute('aria-activedescendant', 'wrong');
                        throw new Error('reconcile failed');
                    },
                },
            ),
            'reconcile-failed',
            'Live delta reconcile failed and was rolled back',
        );
        expect(rowA.hasAttribute('class')).toBe(false);
        expect(cardA.className).toBe('ro-pcard');
        expect(wrap.getAttribute('aria-activedescendant')).toBe('row-a');
    });

    test('keeps detached windowed identity on fast updates but replaces it during structural deltas', () => {
        let content = remountList(['a', 'b'], { windowed: true });
        let tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const oldFastA = listProjectionRowByKey('a') as HTMLElement;
        tbody.replaceChildren();
        const fastMorph = vi.fn(morphInPlace);
        expect(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Fast A') }] },
                { morph: fastMorph },
            ),
        ).toMatchObject({ ok: true, summary: { updated: 1, reordered: false } });
        expect(fastMorph).toHaveBeenCalledOnce();
        expect(listProjectionWindowed()).toBe(true);
        expect(listProjectionRowByKey('a')).toBe(oldFastA);
        expect(oldFastA).toHaveTextContent('Fast A');
        expect(oldFastA.isConnected).toBe(false);
        expect(listProjectionRowModel().rows[0]?.name).toBe('Fast A');

        content = remountList(['a', 'b'], { windowed: true });
        tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const oldStructuralA = listProjectionRowByKey('a') as HTMLElement;
        tbody.replaceChildren();
        const structuralMorph = vi.fn(morphInPlace);
        const result = applyDelta(
            {
                remove: [{ key: 'b', cause: 'project' }],
                upsert: [
                    { key: 'a', row: rowFragment('a', 'Structural A') },
                    { key: 'c', row: rowFragment('c', 'Inserted C') },
                ],
                order: ['a', 'c'],
            },
            { morph: structuralMorph },
        );
        expect(result).toMatchObject({
            ok: true,
            summary: { inserted: 1, updated: 1, projected: 1, reordered: true },
        });
        expect(structuralMorph).not.toHaveBeenCalled();
        expect(listProjectionWindowed()).toBe(true);
        expect(listProjectionRowByKey('a')).not.toBe(oldStructuralA);
        expect(listProjectionRowByKey('a')).toHaveTextContent('Structural A');
        expect(oldStructuralA).toHaveTextContent('Workload 0');
    });

    test('rejects duplicate, absent, conflicting, or malformed row and region operations', () => {
        remountList(['a', 'b'], { keyedCards: true });
        expectDeltaError(
            applyDelta({
                remove: [
                    { key: 'a', cause: 'delete' },
                    { key: 'a', cause: 'project' },
                ],
            }),
            'projection-mismatch',
            'remove key a is duplicate',
        );

        remountList(['a', 'b'], { keyedCards: true });
        expectDeltaError(
            applyDelta({ remove: [{ key: 'missing', cause: 'delete' }] }),
            'projection-mismatch',
            'remove key missing is absent',
        );

        remountList(['a', 'b'], { keyedCards: true });
        expectDeltaError(
            applyDelta({
                upsert: [
                    { key: 'a', row: rowFragment('a'), card: cardFragment('a') },
                    { key: 'a', row: rowFragment('a'), card: cardFragment('a') },
                ],
            }),
            'projection-mismatch',
            'upsert key a is duplicate or also removed',
        );

        remountList(['a', 'b'], { keyedCards: true });
        expectDeltaError(
            applyDelta({
                remove: [{ key: 'a', cause: 'delete' }],
                upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
            }),
            'projection-mismatch',
            'upsert key a is duplicate or also removed',
        );

        remountList(['a'], { keyedCards: true });
        (listProjectionRowByKey('a') as HTMLElement).id = 'legacy-row-a';
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
            }),
            'fragment-invalid',
            'row fragment for a changed its canonical id',
        );

        remountList(['a'], { keyedCards: true });
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'fragment-invalid',
            'card-mode upsert a is missing its card',
        );

        let content = remountList(['a'], { keyedCards: true });
        content.append(listProjectionCardByKey('a') as HTMLElement);
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
            }),
            'projection-mismatch',
            'card a is not canonically mounted',
        );

        content = remountList(['a'], { keyedCards: true });
        (listProjectionCardByKey('a') as HTMLElement).dataset.key = 'wrong';
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
            }),
            'projection-mismatch',
            'card a is not canonically mounted',
        );

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
            }),
            'fragment-invalid',
            'windowed upsert a unexpectedly carries a card',
        );

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">1</span>',
                    },
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            }),
            'projection-mismatch',
            'region count is duplicate',
        );
    });

    test('fast updates revalidate the addressed row index, model, and live mount', () => {
        remountList(['a', 'b'], { windowed: true });
        (listProjectionRows() as HTMLElement[]).reverse();
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'projection-mismatch',
            'row a is not at its canonical index',
        );

        remountList(['a', 'b'], { windowed: true });
        (listProjectionRowModel().rows as unknown as Array<unknown>)[0] = undefined;
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'projection-mismatch',
            'row a is not at its canonical index',
        );

        remountList(['a', 'b'], { windowed: true });
        (listProjectionRowModel().rows[0] as { key: string }).key = 'wrong';
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'projection-mismatch',
            'row a is not at its canonical index',
        );

        const content = remountList(['a', 'b'], { windowed: true });
        content.append(listProjectionRowByKey('a') as HTMLElement);
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'projection-mismatch',
            'row a is not at its canonical index',
        );
    });

    test('rejects non-atomic topology, non-exact order, and document id collisions', () => {
        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({ remove: [{ key: 'a', cause: 'delete' }], order: [] }),
            'projection-mismatch',
            'empty projection boundary requires snapshot',
        );

        remountList(['a', 'b'], { windowed: true });
        expectDeltaError(
            applyDelta({ remove: [{ key: 'b', cause: 'delete' }] }),
            'projection-mismatch',
            'topology-changing delta requires full order',
        );

        remountList(['a', 'b'], { windowed: true });
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'c', row: rowFragment('c') }] }),
            'projection-mismatch',
            'topology-changing delta requires full order',
        );

        remountList(['a', 'b'], { windowed: true });
        expectDeltaError(
            applyDelta({
                upsert: [
                    { key: 'a', row: rowFragment('a') },
                    { key: 'c', row: rowFragment('c') },
                ],
            }),
            'projection-mismatch',
            'topology-changing delta requires full order',
        );

        for (const order of [['a'], ['a', 'a'], ['a', 'missing']]) {
            remountList(['a', 'b'], { windowed: true });
            expectDeltaError(
                applyDelta({
                    remove: [{ key: 'b', cause: 'delete' }],
                    upsert: [{ key: 'c', row: rowFragment('c') }],
                    order,
                }),
                'projection-mismatch',
                'delta order is not the exact final key set',
            );
        }

        remountList(['a', 'b'], { windowed: true });
        expectDeltaError(
            applyDelta({ order: ['a', 'b'] }),
            'projection-mismatch',
            'redundant unchanged order is not allowed',
        );

        let content = remountList(['a', 'b'], { keyedCards: true });
        const connectedCollision = document.createElement('div');
        connectedCollision.id = 'row-a';
        document.body.append(connectedCollision);
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
            }),
            'fragment-invalid',
            'row fragment for a collides with a document id',
        );

        content = remountList(['a', 'b'], { windowed: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        tbody.replaceChildren();
        const detachedCollision = document.createElement('div');
        detachedCollision.id = 'row-a';
        document.body.append(detachedCollision);
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'fragment-invalid',
            'row fragment for a collides with a document id',
        );

        remountList(['a', 'b'], { windowed: true });
        const insertedCollision = document.createElement('div');
        insertedCollision.id = 'row-c';
        document.body.append(insertedCollision);
        expectDeltaError(
            applyDelta({
                upsert: [{ key: 'c', row: rowFragment('c') }],
                order: ['a', 'b', 'c'],
            }),
            'fragment-invalid',
            'row fragment for c collides with a document id',
        );

        remountList(['a', 'b'], { keyedCards: true });
        const untouchedCollision = document.createElement('div');
        untouchedCollision.id = 'row-b';
        document.body.append(untouchedCollision);
        expectDeltaError(
            applyDelta({ order: ['b', 'a'] }),
            'fragment-invalid',
            'final row b collides with a document id',
        );

        content = remountList(['a', 'b'], { windowed: true });
        (content.querySelector('tbody') as HTMLTableSectionElement).replaceChildren();
        const detachedFinalCollision = document.createElement('div');
        detachedFinalCollision.id = 'row-b';
        document.body.append(detachedFinalCollision);
        expectDeltaError(
            applyDelta({ order: ['b', 'a'] }),
            'fragment-invalid',
            'final row b collides with a document id',
        );
    });

    test('uses the global morph adapter and reports singular row and region status', () => {
        let content = remountList(['a'], { windowed: true });
        const morph = vi.fn(morphInPlace);
        vi.stubGlobal('Idiomorph', { morph });
        expect(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a', 'Global') }] }),
        ).toMatchObject({ ok: true, summary: { updated: 1 } });
        expect(morph).toHaveBeenCalledWith(expect.any(HTMLElement), expect.any(HTMLElement), {
            morphStyle: 'outerHTML',
            ignoreActiveValue: true,
        });
        expect(content.querySelector('#ro-live-status')?.textContent).toBe('Live update: 1 row');

        content = remountList(['a'], { windowed: true });
        expect(
            applyDelta({
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            }),
        ).toMatchObject({ ok: true, summary: { regions: ['count'] } });
        expect(content.querySelector('#ro-live-status')?.textContent).toBe('Live update: 1 region');

        content = remountList(['a', 'b'], { windowed: true });
        expect(applyDelta({ order: ['b', 'a'] })).toMatchObject({
            ok: true,
            summary: { reordered: true, regions: [] },
        });
        expect(content.querySelector('#ro-live-status')?.textContent).toBe(
            'Live update: order changed',
        );

        vi.stubGlobal('Idiomorph', { morph: null });
        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({ upsert: [{ key: 'a', row: rowFragment('a') }] }),
            'morph-failed',
            'Idiomorph is unavailable',
        );

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta({
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">2</span>',
                    },
                ],
            }),
            'morph-failed',
            'Idiomorph is unavailable',
        );
    });

    test('accepts transient selection, focus, filter, and flash classes after canonical morphs', () => {
        for (const className of ['is-selected', 'kfocus', 'ro-row-filtered', 'ro-cell-changed']) {
            remountList(['a'], { windowed: true });
            const result = applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', `Transient ${className}`) }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        const target =
                            className === 'ro-cell-changed'
                                ? (current.querySelector('td') as HTMLElement)
                                : current;
                        target.classList.add(className);
                    },
                },
            );
            expect(result.ok).toBe(true);
            const row = listProjectionRowByKey('a') as HTMLElement;
            const target = className === 'ro-cell-changed' ? row.querySelector('td') : row;
            expect(target).toHaveClass(className);
        }
    });

    test('rejects canonical class, connectivity, parent, sibling, result, and content drift', () => {
        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Wrong class') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        current.classList.add('not-transient');
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'False result') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        return false;
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Missing morph') }] },
                { morph: () => true },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );

        let content = remountList(['a'], { windowed: true });
        let row = listProjectionRowByKey('a') as HTMLElement;
        const connectedHolding = document.createElement('div');
        document.body.append(connectedHolding);
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Wrong parent') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        connectedHolding.append(current);
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(content.querySelector('tbody > tr')).toBe(row);

        content = remountList(['a', 'b'], { windowed: true });
        row = listProjectionRowByKey('a') as HTMLElement;
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Wrong sibling') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        tbody.append(current);
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(Array.from(tbody.children)).toStrictEqual([
            row,
            listProjectionRowByKey('b') as HTMLElement,
        ]);

        content = remountList(['a'], { windowed: true });
        const table = content.querySelector('table') as HTMLTableElement;
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Disconnected') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        table.remove();
                    },
                },
            ),
            'rollback-failed',
            'Live delta rollback could not restore the original mounts',
            true,
        );
    });

    test('returns a formerly parentless detached row to no parent after a successful morph', () => {
        const content = remountList(['a'], { windowed: true });
        const row = listProjectionRowByKey('a') as HTMLElement;
        (content.querySelector('tbody') as HTMLTableSectionElement).replaceChildren();
        const temporaryParent = document.createDocumentFragment();

        const result = applyDelta(
            { upsert: [{ key: 'a', row: rowFragment('a', 'Parentless') }] },
            {
                morph(current, incoming) {
                    morphInPlace(current, incoming);
                    temporaryParent.append(current);
                },
            },
        );

        expect(result.ok).toBe(true);
        expect(row.parentNode).toBeNull();
        expect(temporaryParent.childNodes).toHaveLength(0);
        expect(row).toHaveTextContent('Parentless');

        const failedContent = remountList(['a'], { windowed: true });
        const failedRow = listProjectionRowByKey('a') as HTMLElement;
        (failedContent.querySelector('tbody') as HTMLTableSectionElement).replaceChildren();
        const failedTemporaryParent = document.createDocumentFragment();
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Must roll back') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        failedTemporaryParent.append(current);
                        throw new Error('parentless morph failed');
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(failedRow.parentNode).toBeNull();
        expect(failedTemporaryParent.childNodes).toHaveLength(0);
        expect(failedRow).toHaveTextContent('Workload 0');
    });

    test('does not churn a connected row root during a canonical fast morph', () => {
        const content = remountList(['a', 'b'], { windowed: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const observer = new MutationObserver(() => {});
        observer.observe(tbody, { childList: true });

        expect(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Stable root') }] },
                { morph: morphInPlace },
            ).ok,
        ).toBe(true);
        expect(observer.takeRecords()).toStrictEqual([]);
        observer.disconnect();
    });

    test('does not reinsert small-list row or card roots after fast in-place morphs', () => {
        const content = remountList(['a', 'b'], { keyedCards: true });
        const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
        const cardMount = content.querySelector('.ro-cardlist') as HTMLElement;
        const rowObserver = new MutationObserver(() => {});
        const cardObserver = new MutationObserver(() => {});
        rowObserver.observe(tbody, { childList: true });
        cardObserver.observe(cardMount, { childList: true });

        expect(
            applyDelta(
                {
                    upsert: [
                        { key: 'a', row: rowFragment('a', 'Stable'), card: cardFragment('a') },
                    ],
                },
                { morph: morphInPlace },
            ).ok,
        ).toBe(true);
        expect(rowObserver.takeRecords()).toStrictEqual([]);
        expect(cardObserver.takeRecords()).toStrictEqual([]);
        rowObserver.disconnect();
        cardObserver.disconnect();
    });

    test('restores a detached row to its exact fragment position after a scoped morph', () => {
        remountList(['a'], { windowed: true });
        const row = listProjectionRowByKey('a') as HTMLElement;
        const fragment = document.createDocumentFragment();
        const next = document.createElement('i');
        fragment.append(row, next);

        const result = applyDelta(
            { upsert: [{ key: 'a', row: rowFragment('a', 'Detached') }] },
            {
                morph(current, incoming) {
                    current.remove();
                    morphInPlace(current, incoming);
                },
            },
        );
        expect(result.ok).toBe(true);
        expect(Array.from(fragment.childNodes)).toStrictEqual([row, next]);
        expect(row).toHaveTextContent('Detached');
        expect(row.isConnected).toBe(false);
    });

    test('restores a row parked in an originally detached element parent', () => {
        remountList(['a'], { windowed: true });
        const row = listProjectionRowByKey('a') as HTMLElement;
        const detachedTbody = document.createElement('tbody');
        const next = document.createElement('tr');
        detachedTbody.append(row, next);

        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Detached failure') }] },
                {
                    morph(current, incoming) {
                        current.remove();
                        morphInPlace(current, incoming);
                        throw new Error('detached morph failed');
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(Array.from(detachedTbody.children)).toStrictEqual([row, next]);
        expect(row).toHaveTextContent('Workload 0');
        expect(row.isConnected).toBe(false);
    });

    test('restores recursively captured text-node values after a morph throws', () => {
        remountList(['a'], { windowed: true });
        const row = listProjectionRowByKey('a') as HTMLElement;
        const text = row.querySelector('a')?.firstChild as Text;
        const original = text.nodeValue;

        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Unused') }] },
                {
                    morph() {
                        text.nodeValue = 'poison';
                        throw new Error('text mutated');
                    },
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(text.nodeValue).toBe(original);
        expect(row.querySelector('a')?.firstChild).toBe(text);
    });

    test('fails rollback when a placement parent or only one structural mount disappears', () => {
        let content = remountList(['a'], { keyedCards: true });
        let table = content.querySelector('table') as HTMLTableElement;
        expectDeltaError(
            applyDelta(
                {
                    upsert: [{ key: 'a', row: rowFragment('a'), card: cardFragment('a') }],
                },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        table.remove();
                        throw new Error('row parent disappeared');
                    },
                },
            ),
            'rollback-failed',
            'Live delta rollback could not restore the original mounts',
            true,
        );

        content = remountList(['a', 'b'], { keyedCards: true });
        table = content.querySelector('table') as HTMLTableElement;
        expectDeltaError(
            applyDelta(
                { order: ['b', 'a'] },
                {
                    reconcile() {
                        table.remove();
                        throw new Error('one structural parent disappeared');
                    },
                },
            ),
            'rollback-failed',
            'Live delta rollback could not restore the original mounts',
            true,
        );
    });

    test('maps morph, reconcile, and rollback failures exactly', () => {
        let content = remountList(['a'], { windowed: true });
        const row = listProjectionRowByKey('a') as HTMLElement;
        const before = row.cloneNode(true);
        const restoreExternalState = vi.fn();
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Broken') }] },
                {
                    morph(current, incoming) {
                        morphInPlace(current, incoming);
                        throw new Error('morph failed');
                    },
                    restoreExternalState,
                },
            ),
            'morph-failed',
            'Live delta DOM morph failed and was rolled back',
        );
        expect(restoreExternalState).toHaveBeenCalledOnce();
        expect(listProjectionRowByKey('a')).toBe(row);
        expect(row.isEqualNode(before)).toBe(true);

        content = remountList(['a'], { windowed: true });
        const original = listProjectionRowByKey('a');
        const originalModelName = listProjectionRowModel().rows[0]?.name;
        const originalRevision = listProjectionRevision();
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Reconcile') }] },
                {
                    morph: morphInPlace,
                    reconcile() {
                        throw new Error('reconcile failed');
                    },
                },
            ),
            'reconcile-failed',
            'Live delta reconcile failed and was rolled back',
        );
        expect(listProjectionRowByKey('a')).toBe(original);
        expect(content.querySelector('tbody > tr')).toBe(original);
        expect(listProjectionRowModel().rows[0]?.name).toBe(originalModelName);
        expect(listProjectionRevision()).toBe(originalRevision);

        remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Fatal') }] },
                {
                    morph() {
                        throw new Error('morph failed');
                    },
                    restoreExternalState() {
                        throw new Error('external rollback failed');
                    },
                },
            ),
            'rollback-failed',
            'Live delta rollback could not restore the original mounts',
            true,
        );

        content = remountList(['a'], { windowed: true });
        expectDeltaError(
            applyDelta(
                { upsert: [{ key: 'a', row: rowFragment('a', 'Fatal DOM') }] },
                {
                    morph() {
                        content.remove();
                        throw new Error('mount removed');
                    },
                },
            ),
            'rollback-failed',
            'Live delta rollback could not restore the original mounts',
            true,
        );
    });
});
