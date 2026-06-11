// palette-rank.ts -- the PURE ⌘K palette ranking + grouping (Unit 10). Split
// out of palette.ts so it has NO runtime imports: Node's native type-stripping
// (`node --test`) resolves `.js` specifiers literally and cannot follow a
// runtime `./x.js` import to its `.ts` source, so a node-tested module must
// stay free of runtime cross-module imports (the collapse-hash.ts precedent).
// palette.ts (which DOES carry runtime imports for its DOM + bindings) re-uses
// every export here.
//
// This module owns the SPEC §8.7 / §6.3 + D21/D12 decisions that are pure data
// transforms -- the fuzzy SUBSEQUENCE ranker, per-group ranking, the recents
// write-side dedupe/cap, and the GROUP ORDER (Everywhere/Recents first, then On
// this page, then the feed groups). The DOM rendering (row elements, the active
// model, open/close) stays in palette.ts; this file decides ONLY shape + order.

// roFuzzyScore is THE palette matcher (SPEC §8.7 / D21): a case-insensitive
// SUBSEQUENCE match -- replacing the old substring test -- scored so a prefix
// match always ranks above a word-start match, which always ranks above a
// scattered one. Returns -1 when query is not a subsequence of text, else a
// score where LOWER is better:
//   tier*100000 + gaps*100 + min(first, 99)
//     tier  0 = prefix      (contiguous from the first character)
//           1 = word-start  (contiguous from a word boundary: after a space,
//                            -, _, ., /, : separator or at a camelCase hump)
//           2 = scattered   (any other subsequence; within the tier a tighter
//                            and earlier match still wins -- "dply" lands
//                            Deployments above wide scatters)
//     gaps  = matched span minus query length (tighter matches first)
//     first = index of the first matched character (earlier matches first)
// Greedy leftmost matching keeps it linear in the text. PURE (no DOM, no module
// state) and re-exported as window.roFuzzy by palette.ts -- the e2e suite
// unit-tests the ranking in isolation through that seam.
export function roFuzzyScore(query: string, text: string): number {
    const source = String(text || '');
    const q = String(query || '').toLowerCase();
    const t = source.toLowerCase();
    if (!q) {
        return 0; // empty query matches everything, rank-neutral
    }
    let from = 0;
    let first = -1;
    let last = -1;
    for (let i = 0; i < q.length; i++) {
        const at = t.indexOf(q[i], from);
        if (at === -1) {
            return -1; // not a subsequence
        }
        if (first === -1) {
            first = at;
        }
        last = at;
        from = at + 1;
    }
    const gaps = last - first + 1 - q.length;
    const camelHump =
        source[first] >= 'A' &&
        source[first] <= 'Z' &&
        !(source[first - 1] >= 'A' && source[first - 1] <= 'Z');
    const wordStart = first === 0 || ' -_./:'.indexOf(t[first - 1]) !== -1 || camelHump;
    let tier = 2;
    if (gaps === 0 && first === 0) {
        tier = 0;
    } else if (gaps === 0 && wordStart) {
        tier = 1;
    }
    return tier * 100000 + gaps * 100 + Math.min(first, 99);
}

// rankPaletteEntries filters a group's entries to the fuzzy matches of query
// (against the label labelOf extracts) and orders them best-score-first; equal
// scores keep feed order (Array.sort is stable). An empty query keeps the whole
// group in feed order.
export function rankPaletteEntries<T>(
    list: T[],
    query: string,
    labelOf: (entry: T) => string,
): T[] {
    if (!query) {
        return list.slice();
    }
    const scored: { entry: T; score: number }[] = [];
    list.forEach((entry) => {
        const score = roFuzzyScore(query, labelOf(entry));
        if (score >= 0) {
            scored.push({ entry: entry, score: score });
        }
    });
    scored.sort((a, b) => a.score - b.score);
    return scored.map((it) => it.entry);
}

// --- recents write-side codec (D21) ----------------------------------------

// A persisted recent: a label plus a navigation target (a SAFE href and/or a
// named client action). The store is the last PALETTE_RECENTS_MAX chosen
// entries, newest first, deduped by destination.
export interface RecentEntry {
    label: string;
    href?: string;
    action?: string;
}

// The dedupe identity of a recents entry: its navigation target. href wins when
// both are present (the same identity the monolith used).
export function paletteRecentTarget(entry: RecentEntry): string {
    return entry.href ? `href:${entry.href}` : `action:${entry.action}`;
}

// dedupeRecents is the recents WRITE rule: drop any prior entry sharing the new
// entry's destination, prepend the new entry (newest first), and cap at `max`.
// PURE -- the localStorage read/write stays in palette.ts. The cap is a
// write-side bound (the store never grows past `max`), matching the e2e
// "caps at five" assertion that the persisted store itself holds exactly five.
export function dedupeRecents(
    prior: RecentEntry[],
    entry: RecentEntry,
    max: number,
): RecentEntry[] {
    const kept = prior.filter((it) => paletteRecentTarget(it) !== paletteRecentTarget(entry));
    kept.unshift(entry);
    return kept.slice(0, max);
}

// --- group order (SPEC §6.3 + D21/D12) --------------------------------------

// A ranked, ordered palette group descriptor: the display title, the feed key
// (drives the row builder in palette.ts), and the entries in render order. The
// `entry` payloads are opaque here -- palette.ts turns each into a row element.
export interface PaletteGroup<E = unknown> {
    title: string;
    key: string;
    entries: E[];
}

// The render order + display titles of the FEED-built groups (the SPEC §6.3
// order AFTER the synthesized Everywhere/Recents slot and the page-objects
// group). Empty groups are skipped by buildPaletteGroups.
export const FEED_GROUPS: { title: string; key: string }[] = [
    { title: 'Resource types', key: 'kinds' },
    { title: 'Namespaces', key: 'namespaces' },
    { title: 'Clusters', key: 'clusters' },
    { title: 'Actions', key: 'actions' },
];

// The label a feed entry ranks/displays by: kinds use `kind`/`plural`, every
// other group uses `name`/`label`. Mirrors buildPaletteRow's labelText.
export function feedEntryLabel(entry: Record<string, unknown>, key: string): string {
    if (key === 'kinds') {
        return String(entry.kind || entry.plural || '');
    }
    return String(entry.name || entry.label || '');
}

// The inputs buildPaletteGroups decides over -- all already-parsed data, no DOM.
export interface PaletteFeed {
    clusters: Record<string, unknown>[];
    namespaces: Record<string, unknown>[];
    kinds: Record<string, unknown>[];
    actions: Record<string, unknown>[];
}

// A harvested page object ({ name, ... }); ranked by its name.
export interface PageObject {
    name: string;
    [k: string]: unknown;
}

// buildPaletteGroups produces the ORDERED, ranked group list the palette
// renders -- the single owner of group order (SPEC §6.3 + D21/D12):
//   - while TYPING (q non-empty): Everywhere (pinned first, D12) -> On this page
//     -> Resource types -> Namespaces -> Clusters -> Actions;
//   - on an EMPTY query: the persisted Recents lead (Everywhere absent), then On
//     this page -> the feed groups.
// Empty groups (and groups with no match) are skipped. The Everywhere row is a
// synthesized single-entry group ({ query }); Recents entries are the persisted
// RecentEntry shape; page objects + feed entries pass through ranked. PURE: the
// caller (palette.ts) turns each entry into a DOM row by its group key.
export function buildPaletteGroups(
    query: string,
    feed: PaletteFeed,
    recents: RecentEntry[],
    pageObjects: PageObject[],
): PaletteGroup[] {
    const q = (query || '').trim();
    const groups: PaletteGroup[] = [];

    // First slot: Everywhere while typing (pinned, so ⏎ on a fresh query
    // searches all clusters), else the persisted Recents on the empty query.
    if (q) {
        groups.push({ title: 'Everywhere', key: 'everywhere', entries: [{ query: q }] });
    } else if (recents.length > 0) {
        groups.push({ title: 'Recents', key: 'recents', entries: recents.slice() });
    }

    // Objects on THIS list page, ranked by name (skipped when none match).
    const objects = rankPaletteEntries(pageObjects, q, (o) => o.name);
    if (objects.length > 0) {
        groups.push({ title: 'On this page', key: 'objects', entries: objects });
    }

    // The feed groups in SPEC §6.3 order; each ranked, empty ones skipped.
    FEED_GROUPS.forEach((group) => {
        const list = (feed[group.key as keyof PaletteFeed] || []) as Record<string, unknown>[];
        const ranked = rankPaletteEntries(list, q, (entry) => feedEntryLabel(entry, group.key));
        if (ranked.length > 0) {
            groups.push({ title: group.title, key: group.key, entries: ranked });
        }
    });

    return groups;
}
