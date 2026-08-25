// palette-rank.test.ts -- Vitest for the PURE ⌘K palette ranking + grouping.
// The fuzzy ranker, the group order, and the recents dedupe are the load-bearing
// decisions the e2e palette spec exercises through the DOM; pinning them here
// (no DOM) catches a regression at the unit boundary before it reaches a frame.
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import {
    buildPaletteGroups,
    dedupeRecents,
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

test('a camelCase hump counts as a word-start boundary', () => {
    const camel = roFuzzyScore('vol', 'PersistentVolumes'); // hump at "Volumes"
    const scattered = roFuzzyScore('sys', 'misty-sales');
    expect(camel).toBeGreaterThanOrEqual(0);
    expect(camel, `camelHump ${camel} < scattered ${scattered}`).toBeLessThan(scattered);
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
    expect(rankPaletteEntries(list, '', (e) => e.n)).toStrictEqual(list);
});

test('rankPaletteEntries drops non-matches and orders best-first', () => {
    const list = [{ n: 'misty-sales' }, { n: 'system-pods' }, { n: 'kube-system' }];
    const out = rankPaletteEntries(list, 'sys', (e) => e.n).map((e) => e.n);
    // prefix (system-pods) first, then word-start (kube-system), then scattered.
    expect(out).toStrictEqual(['system-pods', 'kube-system', 'misty-sales']);
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
