// palette.ts -- the ⌘K jump-to command palette v2 DOM + dispatch (Unit 10).
// Migrated from the monolith (legacy.js) verbatim in behavior; the PURE ranking
// + group order lives in palette-rank.ts (node-tested), this file owns the DOM:
// reading the server feed, building rows, the active-row model, recents
// persistence, open/close + focus restore, and the dispatcher bindings.
//
// Architecture (D10/D21/D12, SPEC §6.3 + §8.7): a keyboard launcher that JUMPS
// to navigation targets. The feed-built groups come from the server JSON blob
// in #ro-palette-data (re-read on EVERY open so an hx-boost swap is picked up);
// the "On this page" group is harvested from the rendered list table. Selecting
// a row navigates to its server-built absolute href (a plain GET permalink) or
// runs a named client action. JSON.parse only (never eval); names via
// textContent; the ONLY innerHTML is the server-escaped kind icon markup ->
// CSP-clean.
//
// DISPATCH (the Unit 10 ordered-binding migration): the palette's click/input/
// keydown branches were the FIRST branches of the monolith's big click + input
// listeners and the head of its palette keydown listener, so they register
// FIRST here (ahead of the row-gesture + still-resident filter listeners). The
// inter-surface decoupling rides DOM GUARDS, not order: the palette-open keydown
// binding acts only while #ro-palette is open AND the target is not the filter
// editor (#ro-filter-input owns its own Escape via the still-resident filter
// keydown listener -- compound case 4), so the focus-routed Escape semantics are
// preserved exactly.

import { closeRowMenu } from './context-menu.js';
import type { Binding } from './events.js';
import { closeKbdOverlay } from './keyboard.js';
import {
    buildPaletteGroups,
    dedupeRecents,
    type PageObject,
    type PaletteFeed,
    type PaletteGroup,
    type RecentEntry,
    roFuzzyScore,
} from './palette-rank.js';
import { virtRows, virtualizerActive } from './virtualizer.js';

const PALETTE_ID = 'ro-palette';

// rankPaletteEntries + roFuzzyScore moved to palette-rank.ts; roFuzzyScore is
// re-exposed as the window.roFuzzy seam (the e2e suite unit-tests the ranker in
// isolation through it).
(window as unknown as { roFuzzy: typeof roFuzzyScore }).roFuzzy = roFuzzyScore;

// The parsed feed shape (a superset of PaletteFeed with the scope fields the
// rows flag against).
interface PaletteData extends PaletteFeed {
    currentCluster: string | null;
    currentNamespace: string | null;
}

// Parse the #ro-palette-data JSON blob into the grouped feed. Guarded end to
// end: a missing/empty/malformed blob yields an all-empty feed (the palette
// still opens with a "no targets" state) and NEVER throws. Re-read on every
// open so an hx-boost navigation that swapped the blob is picked up. JSON.parse
// only -- never eval -- so the blob can carry arbitrary cluster/namespace/CRD
// names safely.
function readPaletteData(): PaletteData {
    const empty: PaletteData = {
        currentCluster: null,
        currentNamespace: null,
        clusters: [],
        namespaces: [],
        kinds: [],
        actions: [],
    };
    const el = document.getElementById('ro-palette-data');
    if (!el) {
        return empty;
    }
    const raw = (el.textContent || '').trim();
    if (!raw) {
        return empty;
    }
    try {
        const data = JSON.parse(raw);
        if (!data || typeof data !== 'object') {
            return empty;
        }
        // Normalise: every group is an array even if the blob omitted/nulled it.
        (['clusters', 'namespaces', 'kinds', 'actions'] as const).forEach((k) => {
            if (!Array.isArray(data[k])) {
                data[k] = [];
            }
        });
        return data as PaletteData;
    } catch {
        return empty; // malformed blob -> empty palette, no throw
    }
}

// A jump target's destination href is ONLY ever read from the server-built blob
// (never user-typed), but as defence in depth we still refuse anything that is
// not a same-origin path / http(s) URL before navigating -- a javascript:,
// data:, or vbscript: scheme is never navigated.
function paletteHrefSafe(href: unknown): string {
    if (!href || typeof href !== 'string') {
        return '';
    }
    const trimmed = href.trim();
    if (/^[a-z][a-z0-9+.-]*:/i.test(trimmed) && !/^https?:/i.test(trimmed)) {
        return '';
    }
    return trimmed;
}

// --- Palette Recents (D21 / SPEC §8.7 + §8.4): the last 5 CHOSEN entries, in
// localStorage 'ro-pref-recents', deduped by destination, newest first. Shown
// as the FIRST group on an EMPTY query only. Reads are guarded end to end: a
// missing/corrupt/unavailable store yields no Recents (never a throw), and the
// next record rewrites it clean.
const PALETTE_RECENTS_KEY = 'ro-pref-recents';
const PALETTE_RECENTS_MAX = 5;

function readPaletteRecents(): RecentEntry[] {
    let raw: string | null = null;
    try {
        raw = window.localStorage.getItem(PALETTE_RECENTS_KEY);
    } catch {
        return []; // localStorage unavailable (privacy mode) -> no recents
    }
    if (!raw) {
        return [];
    }
    try {
        const list = JSON.parse(raw);
        if (!Array.isArray(list)) {
            return [];
        }
        // Shape-check every entry: a label plus a SAFE href or a named action.
        return list
            .filter(
                (entry: RecentEntry) =>
                    entry &&
                    typeof entry === 'object' &&
                    typeof entry.label === 'string' &&
                    entry.label !== '' &&
                    ((typeof entry.href === 'string' && paletteHrefSafe(entry.href) !== '') ||
                        (typeof entry.action === 'string' && entry.action !== '')),
            )
            .slice(0, PALETTE_RECENTS_MAX);
    } catch {
        return []; // corrupt store -> ignored (next record starts fresh)
    }
}

function recordPaletteRecent(label: string, href: string, action: string): void {
    if (!label || (!href && !action)) {
        return; // not a navigable choice -> never recorded
    }
    const entry: RecentEntry = { label: label };
    if (href) {
        entry.href = href;
    }
    if (action) {
        entry.action = action;
    }
    const kept = dedupeRecents(readPaletteRecents(), entry, PALETTE_RECENTS_MAX);
    try {
        window.localStorage.setItem(PALETTE_RECENTS_KEY, JSON.stringify(kept));
    } catch {
        // localStorage unavailable -> the recent just will not persist
    }
}

// The flat list of currently-rendered rows ({ el, item }) in visual order, and
// the index of the active one -- the model the arrows + Enter drive.
interface PaletteRow {
    el: HTMLElement;
    item: unknown;
    key: string;
}
let paletteRows: PaletteRow[] = [];
let paletteActive = 0;

// The current scope (cluster/namespace) of the page, set by readPaletteData via
// renderPalette so buildPaletteRow can flag the in-scope rows.
const paletteScope: { cluster: string | null; namespace: string | null } = {
    cluster: null,
    namespace: null,
};

// Build one row element for a blob entry in group `key`. Names go in via
// textContent; the kind `icon` (server-escaped markup) is the ONLY innerHTML.
function buildPaletteRow(entry: Record<string, unknown>, key: string): HTMLElement {
    const row = document.createElement('div');
    row.className = 'ro-pal-item';
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');

    if (key === 'kinds' && entry.icon) {
        const holder = document.createElement('template');
        holder.innerHTML = String(entry.icon); // server-escaped markup -- the only innerHTML
        row.appendChild(holder.content);
    }

    const labelText =
        key === 'kinds'
            ? String(entry.kind || entry.plural || '')
            : String(entry.name || entry.label || '');
    const display =
        typeof entry.display === 'string' && entry.display !== '' ? entry.display : labelText;
    const label = document.createElement('span');
    label.className = 'pal-label';
    label.textContent = display;
    if (display !== labelText) {
        row.title = labelText; // truncated -> full name in the tooltip
    }

    const isCurrent =
        (key === 'clusters' && entry.name && entry.name === paletteScope.cluster) ||
        (key === 'namespaces' && entry.name && entry.name === paletteScope.namespace);
    if (isCurrent) {
        const ctx = document.createElement('span');
        ctx.className = 'pal-ctx';
        ctx.textContent = 'current';
        label.appendChild(ctx);
    }
    row.appendChild(label);

    if (key === 'kinds') {
        const meta = document.createElement('span');
        meta.className = 'pal-meta';
        meta.textContent = String(entry.group || 'core');
        row.appendChild(meta);
        const scope = document.createElement('span');
        scope.className = `pal-scope ${entry.namespaced ? 'ns' : 'cluster'}`;
        scope.textContent = entry.namespaced ? 'namespaced' : 'cluster';
        row.appendChild(scope);
    }

    const href = paletteHrefSafe(entry.href);
    if (href) {
        row.dataset.href = href;
    }
    if (entry.action) {
        row.dataset.action = String(entry.action);
    }
    row.dataset.label = labelText;
    return row;
}

// buildEverywhereRow is the D12 pinned-first search row, present ONLY while a
// query exists: `Search all clusters for "q"` -> a plain GET /search?q=. The
// leading glyph is a CLONE of the palette's own server-rendered search icon.
function buildEverywhereRow(query: string): HTMLElement {
    const row = document.createElement('div');
    row.className = 'ro-pal-item';
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');
    const glyph = document.querySelector(`#${PALETTE_ID} .ro-pal-search .ico`);
    if (glyph) {
        row.appendChild(glyph.cloneNode(true));
    }
    const label = document.createElement('span');
    label.className = 'pal-label';
    label.textContent = `Search all clusters for “${query}”`;
    row.appendChild(label);
    row.dataset.href = `/search?q=${encodeURIComponent(query)}`;
    row.dataset.label = label.textContent;
    return row;
}

// buildRecentRow renders one persisted recent: label-led (textContent), with the
// destination re-vetted through paletteHrefSafe before it lands in the dataset.
function buildRecentRow(entry: RecentEntry): HTMLElement {
    const row = document.createElement('div');
    row.className = 'ro-pal-item';
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');
    const label = document.createElement('span');
    label.className = 'pal-label';
    label.textContent = entry.label;
    row.appendChild(label);
    const href = paletteHrefSafe(entry.href);
    if (href) {
        row.dataset.href = href;
    }
    if (entry.action) {
        row.dataset.action = entry.action;
    }
    row.dataset.label = entry.label;
    return row;
}

// harvestPageObjects reads the rows of the rendered list table into
// {name, href, status, tone}. While the Unit-24 virtualizer is engaged the DOM
// holds only a window of the rows -- harvest from the full row set (the
// virtualizer module, imported directly) so ⌘K filters every object on the page.
function harvestPageObjects(): PageObject[] {
    const out: PageObject[] = [];
    const rows: ArrayLike<Element> = virtualizerActive()
        ? virtRows()
        : document.querySelectorAll('#resource-list-content table.ro-table tbody tr');
    Array.prototype.forEach.call(rows, (tr: Element) => {
        const a = tr.querySelector('td.cell-name a');
        if (!a) {
            return;
        }
        const href = a.getAttribute('href');
        const name = (a.textContent || '').trim();
        if (!href || !name) {
            return;
        }
        let status = '';
        let tone = '';
        const st = tr.querySelector('.cell-status');
        if (st) {
            status = (st.textContent || '').trim();
            ['ok', 'warn', 'err', 'info', 'mute'].forEach((t) => {
                if (!tone && st.classList.contains(t)) {
                    tone = t;
                }
            });
        }
        out.push({ name: name, href: href, status: status, tone: tone });
    });
    return out;
}

// buildObjectRow renders one harvested page object: its name (textContent) + a
// tone-coloured short status. The detail href rides in the dataset.
function buildObjectRow(o: PageObject): HTMLElement {
    const row = document.createElement('div');
    row.className = 'ro-pal-item';
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');
    const label = document.createElement('span');
    label.className = 'pal-label';
    label.textContent = o.name;
    row.appendChild(label);
    if (o.status) {
        const st = document.createElement('span');
        st.className = `pal-status${o.tone ? ` ${String(o.tone)}` : ''}`;
        st.textContent = String(o.status);
        row.appendChild(st);
    }
    row.dataset.href = String(o.href);
    row.dataset.label = o.name;
    return row;
}

// (Re)render the grouped rows into #ro-palette-list. The ORDER + RANKING are
// decided by buildPaletteGroups (palette-rank.ts); this builds the DOM rows per
// group key, wires the mousemove active-seat, and seats the active row.
function renderPalette(query: string): void {
    const list = document.getElementById('ro-palette-list');
    if (!list) {
        return;
    }
    const data = readPaletteData();
    paletteScope.cluster = data.currentCluster || null;
    paletteScope.namespace = data.currentNamespace || null;

    const scope = document.getElementById('ro-palette-scope');
    if (scope) {
        const scopeText = paletteScope.namespace || paletteScope.cluster || '';
        scope.textContent = scopeText;
        (scope as HTMLElement).hidden = scopeText === '';
    }

    const q = (query || '').trim();
    list.textContent = '';
    paletteRows = [];

    const rowFor = (item: unknown, key: string): HTMLElement => {
        switch (key) {
            case 'everywhere':
                return buildEverywhereRow((item as { query: string }).query);
            case 'recents':
                return buildRecentRow(item as RecentEntry);
            case 'objects':
                return buildObjectRow(item as PageObject);
            default:
                return buildPaletteRow(item as Record<string, unknown>, key);
        }
    };

    const groups: PaletteGroup[] = buildPaletteGroups(
        q,
        {
            clusters: data.clusters,
            namespaces: data.namespaces,
            kinds: data.kinds,
            actions: data.actions,
        },
        readPaletteRecents(),
        harvestPageObjects(),
    );

    groups.forEach((group) => {
        const heading = document.createElement('div');
        heading.className = 'ro-pal-group';
        heading.textContent = group.title;
        list.appendChild(heading);
        group.entries.forEach((item) => {
            const row = rowFor(item, group.key);
            const idx = paletteRows.length;
            row.addEventListener('mousemove', () => setPaletteActive(idx));
            list.appendChild(row);
            paletteRows.push({ el: row, item: item, key: group.key });
        });
    });

    if (paletteRows.length === 0) {
        const none = document.createElement('div');
        none.className = 'ro-pal-empty';
        none.textContent = 'No matching targets.';
        list.appendChild(none);
    }
    paletteActive = 0;
    paintPaletteActive();
}

// Paint exactly the active row with `.active` (+ aria-selected) and scroll it
// into view; a no-op when the list is empty.
function paintPaletteActive(): void {
    paletteRows.forEach((r, i) => {
        const on = i === paletteActive;
        r.el.classList.toggle('active', on);
        r.el.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    if (paletteRows[paletteActive]) {
        paletteRows[paletteActive].el.scrollIntoView({ block: 'nearest' });
    }
}

// Seat the active row at a clamped index (guards empty + out-of-range).
function setPaletteActive(index: number): void {
    if (paletteRows.length === 0) {
        return;
    }
    let i = index;
    if (i < 0) {
        i = 0;
    }
    if (i > paletteRows.length - 1) {
        i = paletteRows.length - 1;
    }
    paletteActive = i;
    paintPaletteActive();
}

// Move the active row by delta, wrapping at the ends. Guards empty.
function movePaletteActive(delta: number): void {
    if (paletteRows.length === 0) {
        return;
    }
    paletteActive = (paletteActive + delta + paletteRows.length) % paletteRows.length;
    paintPaletteActive();
}

// Act on a chosen row: run its named client action and/or navigate to its
// server-built href, then close. EVERY choice is first recorded into Recents
// (D21) -- click and ⏎ both land here.
function choosePaletteRow(rowEl: HTMLElement | null): void {
    if (!rowEl) {
        return;
    }
    const action = rowEl.dataset.action;
    const href = rowEl.dataset.href;
    recordPaletteRecent(rowEl.dataset.label || '', href || '', action || '');
    closePalette();
    if (action === 'theme') {
        const toggle = document.getElementById('btn-theme-toggle');
        if (toggle) {
            (toggle as HTMLElement).click(); // the server POST /preferences toggle
        }
        return;
    }
    if (href) {
        window.location.assign(href); // plain GET to a server permalink
    }
}

// Activate the currently-highlighted row (Enter). No-op when no row is active.
function activatePaletteSelection(): void {
    const active = paletteRows[paletteActive];
    if (active) {
        choosePaletteRow(active.el);
    }
}

// Remember what had focus before the palette opened so Esc/close can restore it.
let palettePriorFocus: Element | null = null;

// True only while closePalette is handing focus back to the prior element: when
// that element is the topbar [data-palette-open] box, the focus restore itself
// fires focusin, which would re-open the palette the user just closed.
let paletteRestoringFocus = false;

// Open the palette: reveal the overlay (the `open` class), build the grouped
// rows from the blob, seed + focus the query box, and seat the first row active.
// Idempotent. `prefill` (optional) opens mid-query -- the Refine·⌘K entry point.
export function openPalette(prefill?: string): void {
    const palette = document.getElementById(PALETTE_ID);
    const input = document.getElementById('ro-palette-input') as HTMLInputElement | null;
    if (!palette || !input) {
        return; // overlay not present (defensive) -> no-op
    }
    if (!palette.classList.contains('open')) {
        palettePriorFocus = document.activeElement;
    }
    palette.classList.add('open');
    palette.setAttribute('aria-hidden', 'false');
    input.value = typeof prefill === 'string' ? prefill : '';
    renderPalette(input.value);
    input.focus(); // focus after it is shown so the caret lands in the box
}
// The deliberate external seam (e2e / console): programmatic palette opening,
// optionally prefilled. The search page's Refine·⌘K rides the [data-search-refine]
// click binding below.
(window as unknown as { roOpenPalette: typeof openPalette }).roOpenPalette = openPalette;

// Close the palette: drop the `open` class and restore focus to wherever it was
// before opening. A restore target INSIDE the palette is refused.
export function closePalette(): void {
    const palette = document.getElementById(PALETTE_ID);
    if (!palette) {
        return;
    }
    palette.classList.remove('open');
    palette.setAttribute('aria-hidden', 'true');
    if (
        palettePriorFocus &&
        document.contains(palettePriorFocus) &&
        !palette.contains(palettePriorFocus) &&
        typeof (palettePriorFocus as HTMLElement).focus === 'function'
    ) {
        paletteRestoringFocus = true;
        (palettePriorFocus as HTMLElement).focus();
        paletteRestoringFocus = false;
    }
    palettePriorFocus = null;
}

// --- dispatcher bindings ----------------------------------------------------
// These were the FIRST branches of the monolith's big click + input listeners
// and the head of its palette keydown listener; they register ahead of the row-
// gesture cluster (and the still-resident filter listeners) in bindings.ts. Each
// branch that early-returned in the monolith is a stop:true binding (the
// delegated mirror of that return).

export const paletteBindings: Binding[] = [
    // ⌘K palette result row: a click on a result row activates it (navigate or
    // run its named action, then close). FIRST so a click inside the open
    // palette never falls through to a page handler. (C1 head, returned.)
    {
        event: 'click',
        selector: '.ro-pal-item',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            choosePaletteRow(matched as HTMLElement);
            return true;
        },
    },
    // The read-only topbar search box ([data-palette-open]) opens the palette on
    // click instead of typing inline. (C1, returned.)
    {
        event: 'click',
        selector: '[data-palette-open]',
        stop: true,
        handler: (event) => {
            event.preventDefault();
            openPalette();
            return true;
        },
    },
    // The search page's "Refine · ⌘K" button (D12): open the palette PREFILLED
    // with the query the page searched (server-baked data-query). (C1, returned.)
    {
        event: 'click',
        selector: '[data-search-refine]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            openPalette((matched as HTMLElement).dataset.query || '');
            return true;
        },
    },
    // A click on the palette backdrop ITSELF (the dimmed area outside the panel)
    // closes it, like Esc. A click inside the panel does not match. The selector
    // is the backdrop root id; the handler still verifies target.id === PALETTE_ID
    // so a click that bubbles from a descendant (closest matched the root) does
    // NOT close it -- the monolith's exact `target.id === PALETTE_ID` test.
    {
        event: 'click',
        selector: `#${PALETTE_ID}`,
        stop: true,
        handler: (event) => {
            if ((event.target as Element).id === PALETTE_ID) {
                closePalette();
                return true;
            }
            return false;
        },
    },
    // ⌘K palette query box: re-render the grouped rows fuzzy-matched + ranked
    // against the label, re-seating the active row. (Monolith input head, returned.)
    {
        event: 'input',
        selector: '#ro-palette-input',
        stop: true,
        handler: (_event, matched) => {
            renderPalette((matched as HTMLInputElement).value);
            return true;
        },
    },
    // ⌘K / Ctrl+K chord opens the palette from anywhere (ignored with Alt/Shift,
    // so an unrelated OS/browser shortcut is never hijacked). The palette is
    // exclusive: an open "?" overlay or row menu closes FIRST so one Esc later
    // closes exactly one surface. No selector (it keys off the chord, not a
    // delegated target). Does NOT stop: the still-resident gesture keydown (K3)
    // returns on the modifier chord on its own, and the filter editor's keydown
    // is unaffected -- mirroring the monolith's separate listeners.
    {
        event: 'keydown',
        handler: (event) => {
            const e = event as KeyboardEvent;
            if (
                (e.metaKey || e.ctrlKey) &&
                !e.altKey &&
                !e.shiftKey &&
                (e.key === 'k' || e.key === 'K')
            ) {
                e.preventDefault();
                closeKbdOverlay();
                closeRowMenu();
                openPalette();
            }
        },
    },
    // Palette-open keyboard model (Esc/Arrow/Enter/Tab). Acts ONLY while the
    // palette is open AND the target is not the filter editor: in the monolith
    // the filter-input keydown branch RETURNED before this palette branch, so an
    // Escape with focus in #ro-filter-input routed to the filter handler and
    // never reached closePalette (compound case 4). The still-resident filter
    // keydown listener keeps owning #ro-filter-input keys; this binding excludes
    // that target so the focus-routed Escape semantics are byte-identical. No
    // stop: the gesture keydown (K3) is kept inert by keyboardSurfaceBusy()
    // (palette `.open`), the real decoupler.
    {
        event: 'keydown',
        handler: (event) => {
            const e = event as KeyboardEvent;
            const target = e.target as Element | null;
            if (target && (target as HTMLElement).id === 'ro-filter-input') {
                return; // the filter editor owns its keys (its own keydown listener)
            }
            const palette = document.getElementById(PALETTE_ID);
            if (!palette?.classList.contains('open')) {
                return;
            }
            if (e.key === 'Escape') {
                e.preventDefault();
                closePalette();
            } else if (e.key === 'ArrowDown') {
                e.preventDefault();
                movePaletteActive(1);
            } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                movePaletteActive(-1);
            } else if (e.key === 'Enter') {
                e.preventDefault();
                activatePaletteSelection();
            } else if (e.key === 'Tab') {
                // Trap focus inside the panel: steer Tab/Shift-Tab through the
                // visible rows via the same active-row model the arrows use.
                e.preventDefault();
                movePaletteActive(e.shiftKey ? -1 : 1);
            }
        },
    },
    // The topbar search box also opens the palette on keyboard FOCUS (Tab-into /
    // programmatic focus): focusin bubbles to document. openPalette runs FIRST
    // (while the box still holds focus) so it captures the box as the Esc restore
    // target; the blur after only matters when openPalette no-opped. The
    // paletteRestoringFocus gate keeps the close-restore from re-opening: focusing
    // the box FROM closePalette fires this very binding.
    {
        event: 'focusin',
        selector: '[data-palette-open]',
        handler: (event) => {
            if (paletteRestoringFocus) {
                return;
            }
            openPalette();
            const t = event.target as HTMLElement;
            if (typeof t.blur === 'function') {
                t.blur();
            }
        },
    },
];
