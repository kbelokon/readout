// palette-rank.test.ts -- Vitest for the PURE ⌘K palette ranking + grouping.
// The fuzzy ranker, the group order, and the recents dedupe are the load-bearing
// decisions the e2e palette spec exercises through the DOM; pinning them here
// (no DOM) catches a regression at the unit boundary before it reaches a frame.
//
// Run: `npm test`.

import { expect, test, vi } from 'vitest';

import {
    buildPaletteGroups,
    dedupeRecents,
    feedEntryLabel,
    type PaletteFeed,
    paletteRecentTarget,
    type RecentEntry,
    rankPaletteEntries,
    roFuzzyScore,
} from './palette-rank.js';

const EMPTY_FEED: PaletteFeed = { clusters: [], namespaces: [], kinds: [], actions: [] };

// --- roFuzzyScore -----------------------------------------------------------

test('empty query is rank-neutral (matches everything at score 0)', () => {
    expect(roFuzzyScore('', 'anything')).toBe(0);
    expect(roFuzzyScore('', '')).toBe(0);
});

test('a non-subsequence returns -1', () => {
    expect(roFuzzyScore('z', 'pods')).toBe(-1);
    expect(roFuzzyScore('sys', 'pods')).toBe(-1);
    expect(roFuzzyScore('xyz', 'Deployments')).toBe(-1);
});

test('prefix beats word-start beats scattered (the tier law)', () => {
    const prefix = roFuzzyScore('sys', 'system-pods'); // contiguous from char 0
    const wordStart = roFuzzyScore('sys', 'kube-system'); // contiguous after "-"
    const scattered = roFuzzyScore('sys', 'misty-sales'); // s...y...s spread
    expect(prefix).toBeGreaterThanOrEqual(0);
    expect(wordStart).toBeGreaterThanOrEqual(0);
    expect(scattered).toBeGreaterThanOrEqual(0);
    expect(prefix, `prefix ${prefix} < wordStart ${wordStart}`).toBeLessThan(wordStart);
    expect(wordStart, `wordStart ${wordStart} < scattered ${scattered}`).toBeLessThan(scattered);
});

test('tier, gap, and first-position weights produce exact comparable scores', () => {
    expect(roFuzzyScore('abc', 'abc')).toBe(0);
    expect(roFuzzyScore('abc', 'x abc')).toBe(100002);
    expect(roFuzzyScore('abc', 'xabc')).toBe(200001);
    expect(roFuzzyScore('abc', 'aXbYc')).toBe(200200);

    // Very late matches stay inside their tier instead of bleeding into the
    // gap component: only the first-position contribution is capped at 99.
    expect(roFuzzyScore('abc', `${'x'.repeat(120)}abc`)).toBe(200099);
});

test('every documented separator creates a word-start boundary', () => {
    for (const separator of [' ', '-', '_', '.', '/', ':']) {
        expect(roFuzzyScore('abc', `x${separator}abc`), `separator ${separator}`).toBe(100002);
    }
});

test('a camelCase hump counts as a word-start boundary', () => {
    const camel = roFuzzyScore('vol', 'PersistentVolumes'); // hump at "Volumes"
    const scattered = roFuzzyScore('sys', 'misty-sales');
    expect(camel).toBeGreaterThanOrEqual(0);
    expect(camel, `camelHump ${camel} < scattered ${scattered}`).toBeLessThan(scattered);
});

test('camel humps use exact ASCII uppercase boundaries', () => {
    // A and Z pin both inclusive ends of the uppercase range.
    expect(roFuzzyScore('apple', 'xApple')).toBe(100001);
    expect(roFuzzyScore('zoo', 'xZoo')).toBe(100001);

    // A lowercase start is not a hump, and an uppercase predecessor means the
    // match is inside an acronym rather than at a camelCase word start.
    expect(roFuzzyScore('apple', 'xapple')).toBe(200001);
    expect(roFuzzyScore('@app', 'x@app')).toBe(200001);
    expect(roFuzzyScore('beta', 'ABeta')).toBe(200001);
    expect(roFuzzyScore('beta', 'ZBeta')).toBe(200001);
});

test('one source character cannot satisfy repeated query characters', () => {
    expect(roFuzzyScore('aa', 'a')).toBe(-1);
    expect(roFuzzyScore('aaa', 'aa')).toBe(-1);
});

test('subsequence works where a substring test would fail; tighter wins', () => {
    // "dply" is NOT a substring of Deployments, yet it IS a subsequence.
    const dply = roFuzzyScore('dply', 'Deployments');
    expect(dply).toBeGreaterThanOrEqual(0);
    // "po" spans 3 chars in Deployments vs 12 in PersistentVolumes -> tighter
    // (Deployments) ranks better.
    const depPo = roFuzzyScore('po', 'Deployments');
    const pvPo = roFuzzyScore('po', 'PersistentVolumes');
    expect(depPo).toBeGreaterThanOrEqual(0);
    expect(pvPo).toBeGreaterThanOrEqual(0);
    expect(depPo, `Deployments ${depPo} < PersistentVolumes ${pvPo}`).toBeLessThan(pvPo);
});

test('matching is case-insensitive and respects diacritics as literal chars', () => {
    // Case folds; the diacritic char matches itself (no ASCII fold) -- a query
    // carrying the same accented char is a clean prefix subsequence.
    expect(roFuzzyScore('CAFE', 'cafeteria')).toBe(roFuzzyScore('cafe', 'cafeteria'));
    const accentPrefix = roFuzzyScore('café', 'Café-Latte'); // é matches é
    expect(accentPrefix).toBeGreaterThanOrEqual(0);
    expect(accentPrefix, 'an accented prefix is tier 0').toBe(0);
    // A plain "cafe" is NOT a subsequence of "café..." (e != é) -> rejected,
    // proving the diacritic is treated as a distinct literal char.
    expect(roFuzzyScore('cafe', 'café')).toBe(-1);
});

// --- rankPaletteEntries -----------------------------------------------------

test('rankPaletteEntries keeps feed order on an empty query', () => {
    const list = [{ n: 'b' }, { n: 'a' }, { n: 'c' }];
    const labelOf = vi.fn((entry: { n: string }) => entry.n);
    const ranked = rankPaletteEntries(list, '', labelOf);
    expect(ranked).toStrictEqual(list);
    expect(ranked).not.toBe(list);
    expect(labelOf).not.toHaveBeenCalled();
});

test('rankPaletteEntries drops non-matches and orders best-first', () => {
    const list = [{ n: 'misty-sales' }, { n: 'system-pods' }, { n: 'kube-system' }];
    const out = rankPaletteEntries(list, 'sys', (e) => e.n).map((e) => e.n);
    // prefix (system-pods) first, then word-start (kube-system), then scattered.
    expect(out).toStrictEqual(['system-pods', 'kube-system', 'misty-sales']);
});

test('rankPaletteEntries preserves feed order when non-empty-query scores tie', () => {
    const list = [
        { id: 1, n: 'x-alpha' },
        { id: 2, n: 'y-alpha' },
        { id: 3, n: 'z-alpha' },
    ];
    expect(rankPaletteEntries(list, 'alpha', (entry) => entry.n).map((entry) => entry.id)).toEqual([
        1, 2, 3,
    ]);
});

test('feed labels use their group-specific primary, fallback, and empty shapes', () => {
    expect(feedEntryLabel({ kind: 'Pod', plural: 'pods' }, 'kinds')).toBe('Pod');
    expect(feedEntryLabel({ kind: '', plural: 'pods' }, 'kinds')).toBe('pods');
    expect(feedEntryLabel({}, 'kinds')).toBe('');

    expect(feedEntryLabel({ name: 'prod', label: 'Production' }, 'clusters')).toBe('prod');
    expect(feedEntryLabel({ name: '', label: 'Toggle theme' }, 'actions')).toBe('Toggle theme');
    expect(feedEntryLabel({}, 'actions')).toBe('');
});

// --- recents dedupe ---------------------------------------------------------

test('paletteRecentTarget keys on href, falling back to action', () => {
    expect(paletteRecentTarget({ label: 'x', href: '/a' })).toBe('href:/a');
    expect(paletteRecentTarget({ label: 'x', action: 'theme' })).toBe('action:theme');
    // href wins when both are present.
    expect(paletteRecentTarget({ label: 'x', href: '/a', action: 'theme' })).toBe('href:/a');
});

test('dedupeRecents dedupes by href: re-choosing moves to front, no duplicate', () => {
    const prior: RecentEntry[] = [
        { label: 'Pods', href: '/pods' },
        { label: 'Nodes', href: '/nodes' },
    ];
    // Re-choose Nodes (same href) -> Nodes to front, Pods second, length 2.
    const out = dedupeRecents(prior, { label: 'Nodes', href: '/nodes' }, 5);
    expect(out).toStrictEqual([
        { label: 'Nodes', href: '/nodes' },
        { label: 'Pods', href: '/pods' },
    ]);
});

test('dedupeRecents caps at max as a WRITE-side bound (oldest evicted)', () => {
    const prior: RecentEntry[] = [1, 2, 3, 4, 5].map((n) => ({
        label: `Seed ${n}`,
        href: `/nodes?seed=${n}`,
    }));
    const out = dedupeRecents(prior, { label: 'Nodes', href: '/nodes' }, 5);
    expect(out.length).toBe(5);
    expect(out.map((e) => e.label)).toStrictEqual([
        'Nodes',
        'Seed 1',
        'Seed 2',
        'Seed 3',
        'Seed 4',
    ]);
});

// --- buildPaletteGroups (group order) ---------------------------------------

test('empty query: Recents first (when present), then On this page, then feed', () => {
    const feed: PaletteFeed = {
        ...EMPTY_FEED,
        kinds: [{ kind: 'Pods' }],
    };
    const recents: RecentEntry[] = [{ label: 'Nodes', href: '/nodes' }];
    const pageObjects = [{ name: 'nginx', href: '/p/nginx' }];
    const groups = buildPaletteGroups('', feed, recents, pageObjects);
    expect(groups.map((g) => g.title)).toStrictEqual(['Recents', 'On this page', 'Resource types']);
    expect(groups[0]).toStrictEqual({
        title: 'Recents',
        key: 'recents',
        entries: recents,
    });
    expect(groups[0].entries).not.toBe(recents);
    // Everywhere is ABSENT on the empty query.
    expect(groups.map((group) => group.title)).not.toContain('Everywhere');
});

test('empty query with no recents: groups lead with On this page', () => {
    const feed: PaletteFeed = { ...EMPTY_FEED, kinds: [{ kind: 'Pods' }] };
    const groups = buildPaletteGroups('', feed, [], [{ name: 'nginx' }]);
    expect(groups.map((g) => g.title)).toStrictEqual(['On this page', 'Resource types']);
});

test('typing: Everywhere is pinned FIRST, then On this page, then ranked feed', () => {
    const feed: PaletteFeed = {
        ...EMPTY_FEED,
        kinds: [{ kind: 'Ingresses' }, { kind: 'Pods' }],
    };
    const groups = buildPaletteGroups('ng', feed, [], [{ name: 'nginx' }, { name: 'my-app' }]);
    expect(groups.map((g) => g.title)).toStrictEqual([
        'Everywhere',
        'On this page',
        'Resource types',
    ]);
    // Everywhere carries the live query verbatim as its single entry.
    expect(groups[0].entries).toStrictEqual([{ query: 'ng' }]);
    // On this page is ranked: nginx matches "ng", my-app does not.
    expect((groups[1].entries as { name: string }[]).map((e) => e.name)).toStrictEqual(['nginx']);
});

test('typing trims the query before both Everywhere output and ranking', () => {
    expect(buildPaletteGroups('  ng  ', EMPTY_FEED, [], [{ name: 'nginx' }])).toStrictEqual([
        { title: 'Everywhere', key: 'everywhere', entries: [{ query: 'ng' }] },
        { title: 'On this page', key: 'objects', entries: [{ name: 'nginx' }] },
    ]);
});

test('empty groups are skipped entirely', () => {
    const groups = buildPaletteGroups('zzz-no-match', EMPTY_FEED, [], [{ name: 'nginx' }]);
    // Only Everywhere survives: nothing else matches "zzz-no-match".
    expect(groups.map((g) => g.title)).toStrictEqual(['Everywhere']);
});

test('feed group order is Resource types -> Namespaces -> Clusters -> Actions', () => {
    const feed: PaletteFeed = {
        clusters: [{ name: 'prod' }],
        namespaces: [{ name: 'default' }],
        kinds: [{ kind: 'Pods' }],
        actions: [{ label: 'Toggle theme', action: 'theme' }],
    };
    const groups = buildPaletteGroups('', feed, [], []);
    expect(groups.map((g) => g.title)).toStrictEqual([
        'Resource types',
        'Namespaces',
        'Clusters',
        'Actions',
    ]);
});
