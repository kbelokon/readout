// palette-rank.test.ts -- node:test for the PURE ⌘K palette ranking + grouping.
// The fuzzy ranker, the group order, and the recents dedupe are the load-bearing
// decisions the e2e palette spec exercises through the DOM; pinning them here
// (no DOM) catches a regression at the unit boundary before it reaches a frame.
//
// Run: `node --test 'internal/assets/src/js/**/*.test.ts'`.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
    buildPaletteGroups,
    dedupeRecents,
    type PaletteFeed,
    paletteRecentTarget,
    type RecentEntry,
    rankPaletteEntries,
    roFuzzyScore,
} from './palette-rank.ts';

const EMPTY_FEED: PaletteFeed = { clusters: [], namespaces: [], kinds: [], actions: [] };

// --- roFuzzyScore -----------------------------------------------------------

test('empty query is rank-neutral (matches everything at score 0)', () => {
    assert.equal(roFuzzyScore('', 'anything'), 0);
    assert.equal(roFuzzyScore('', ''), 0);
});

test('a non-subsequence returns -1', () => {
    assert.equal(roFuzzyScore('sys', 'pods'), -1);
    assert.equal(roFuzzyScore('xyz', 'Deployments'), -1);
});

test('prefix beats word-start beats scattered (the tier law)', () => {
    const prefix = roFuzzyScore('sys', 'system-pods'); // contiguous from char 0
    const wordStart = roFuzzyScore('sys', 'kube-system'); // contiguous after "-"
    const scattered = roFuzzyScore('sys', 'misty-sales'); // s...y...s spread
    assert.ok(prefix >= 0 && wordStart >= 0 && scattered >= 0);
    assert.ok(prefix < wordStart, `prefix ${prefix} < wordStart ${wordStart}`);
    assert.ok(wordStart < scattered, `wordStart ${wordStart} < scattered ${scattered}`);
});

test('a camelCase hump counts as a word-start boundary', () => {
    const camel = roFuzzyScore('vol', 'PersistentVolumes'); // hump at "Volumes"
    const scattered = roFuzzyScore('sys', 'misty-sales');
    assert.ok(camel >= 0);
    assert.ok(camel < scattered, `camelHump ${camel} < scattered ${scattered}`);
});

test('subsequence works where a substring test would fail; tighter wins', () => {
    // "dply" is NOT a substring of Deployments, yet it IS a subsequence.
    const dply = roFuzzyScore('dply', 'Deployments');
    assert.ok(dply >= 0);
    // "po" spans 3 chars in Deployments vs 12 in PersistentVolumes -> tighter
    // (Deployments) ranks better.
    const depPo = roFuzzyScore('po', 'Deployments');
    const pvPo = roFuzzyScore('po', 'PersistentVolumes');
    assert.ok(depPo >= 0 && pvPo >= 0);
    assert.ok(depPo < pvPo, `Deployments ${depPo} < PersistentVolumes ${pvPo}`);
});

test('matching is case-insensitive and respects diacritics as literal chars', () => {
    // Case folds; the diacritic char matches itself (no ASCII fold) -- a query
    // carrying the same accented char is a clean prefix subsequence.
    assert.equal(roFuzzyScore('CAFE', 'cafeteria'), roFuzzyScore('cafe', 'cafeteria'));
    const accentPrefix = roFuzzyScore('café', 'Café-Latte'); // é matches é
    assert.ok(accentPrefix >= 0);
    assert.equal(accentPrefix, 0, 'an accented prefix is tier 0');
    // A plain "cafe" is NOT a subsequence of "café..." (e != é) -> rejected,
    // proving the diacritic is treated as a distinct literal char.
    assert.equal(roFuzzyScore('cafe', 'café'), -1);
});

// --- rankPaletteEntries -----------------------------------------------------

test('rankPaletteEntries keeps feed order on an empty query', () => {
    const list = [{ n: 'b' }, { n: 'a' }, { n: 'c' }];
    assert.deepEqual(
        rankPaletteEntries(list, '', (e) => e.n),
        list,
    );
});

test('rankPaletteEntries drops non-matches and orders best-first', () => {
    const list = [{ n: 'misty-sales' }, { n: 'system-pods' }, { n: 'kube-system' }];
    const out = rankPaletteEntries(list, 'sys', (e) => e.n).map((e) => e.n);
    // prefix (system-pods) first, then word-start (kube-system), then scattered.
    assert.deepEqual(out, ['system-pods', 'kube-system', 'misty-sales']);
});

// --- recents dedupe ---------------------------------------------------------

test('paletteRecentTarget keys on href, falling back to action', () => {
    assert.equal(paletteRecentTarget({ label: 'x', href: '/a' }), 'href:/a');
    assert.equal(paletteRecentTarget({ label: 'x', action: 'theme' }), 'action:theme');
    // href wins when both are present.
    assert.equal(paletteRecentTarget({ label: 'x', href: '/a', action: 'theme' }), 'href:/a');
});

test('dedupeRecents dedupes by href: re-choosing moves to front, no duplicate', () => {
    const prior: RecentEntry[] = [
        { label: 'Pods', href: '/pods' },
        { label: 'Nodes', href: '/nodes' },
    ];
    // Re-choose Nodes (same href) -> Nodes to front, Pods second, length 2.
    const out = dedupeRecents(prior, { label: 'Nodes', href: '/nodes' }, 5);
    assert.deepEqual(out, [
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
    assert.equal(out.length, 5);
    assert.deepEqual(
        out.map((e) => e.label),
        ['Nodes', 'Seed 1', 'Seed 2', 'Seed 3', 'Seed 4'],
    );
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
    assert.deepEqual(
        groups.map((g) => g.title),
        ['Recents', 'On this page', 'Resource types'],
    );
    // Everywhere is ABSENT on the empty query.
    assert.ok(!groups.some((g) => g.title === 'Everywhere'));
});

test('empty query with no recents: groups lead with On this page', () => {
    const feed: PaletteFeed = { ...EMPTY_FEED, kinds: [{ kind: 'Pods' }] };
    const groups = buildPaletteGroups('', feed, [], [{ name: 'nginx' }]);
    assert.deepEqual(
        groups.map((g) => g.title),
        ['On this page', 'Resource types'],
    );
});

test('typing: Everywhere is pinned FIRST, then On this page, then ranked feed', () => {
    const feed: PaletteFeed = {
        ...EMPTY_FEED,
        kinds: [{ kind: 'Ingresses' }, { kind: 'Pods' }],
    };
    const groups = buildPaletteGroups('ng', feed, [], [{ name: 'nginx' }, { name: 'my-app' }]);
    assert.deepEqual(
        groups.map((g) => g.title),
        ['Everywhere', 'On this page', 'Resource types'],
    );
    // Everywhere carries the live query verbatim as its single entry.
    assert.deepEqual(groups[0].entries, [{ query: 'ng' }]);
    // On this page is ranked: nginx matches "ng", my-app does not.
    assert.deepEqual(
        (groups[1].entries as { name: string }[]).map((e) => e.name),
        ['nginx'],
    );
});

test('empty groups are skipped entirely', () => {
    const groups = buildPaletteGroups('zzz-no-match', EMPTY_FEED, [], [{ name: 'nginx' }]);
    // Only Everywhere survives: nothing else matches "zzz-no-match".
    assert.deepEqual(
        groups.map((g) => g.title),
        ['Everywhere'],
    );
});

test('feed group order is Resource types -> Namespaces -> Clusters -> Actions', () => {
    const feed: PaletteFeed = {
        clusters: [{ name: 'prod' }],
        namespaces: [{ name: 'default' }],
        kinds: [{ kind: 'Pods' }],
        actions: [{ label: 'Toggle theme', action: 'theme' }],
    };
    const groups = buildPaletteGroups('', feed, [], []);
    assert.deepEqual(
        groups.map((g) => g.title),
        ['Resource types', 'Namespaces', 'Clusters', 'Actions'],
    );
});
