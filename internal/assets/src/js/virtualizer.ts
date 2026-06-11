// virtualizer.ts -- client-side row windowing above ~500 rows (Unit 24 / D20,
// migrated from legacy.js). Lists always render COMPLETE server-side (no
// pagination, ever). Above the threshold the server marks the table wrap
// `.ro-windowed` (the threshold has ONE owner: resource_table.templ; this module
// only follows the marker) and the virtualizer takes ownership of the tbody: it
// holds the FULL identity row set in memory and keeps only the viewport's slice
// (+ buffer) in the DOM, framed by two spacer rows whose heights stand in for
// everything off-window. The fixed row height (--row-py×2 + line-height,
// guaranteed by the windowed clamp CSS + the server-side expansion flattening)
// makes the offset math exact: it is MEASURED once per engagement as the mean
// row pitch of the full render, so no per-row rounding accumulates across 600
// rows.
//
// The PURE arithmetic (window boundaries, spacer heights, the focus-jump scroll
// delta, the focus clamp) lives in virtualizer-math.ts (node-tested); this
// module is the DOM + state machine around it: measuring, building spacers,
// re-rendering the slice, and the morph-adoption pipeline.
//
// Morphs (ALL swap sources -- refresh tick, sort/filter swap, Live push): a
// >threshold fragment's rows NEVER ride the morph. The ro-morph handleSwap (in
// legacy.js) hands them to virtualizePrepareSwap, which detaches them for
// adoption and leaves height-preserving spacers in the fragment. After the morph
// lands, virtualizeAfterSwap adopts the new full row set and re-renders the
// window -- selection/focus re-key by identity exactly like every other swap,
// and changed cells still flash (the idiomorph cell-flash callbacks never see
// windowed rows, so the diff runs here against the prior row set).
//
// The free-text matcher and the autocomplete frequency scan (filters.ts) are
// UNTOUCHED by design: they read window.roRowModel, captured from the incoming
// server fragment BEFORE any windowing -- the virtualizer only CONSUMES
// roRowModel.visibleKeys to decide which rows are renderable. It reads that
// through the window seam (NOT a module import) so filters.ts depends on the
// virtualizer, not the other way round (the matcher calls
// virtualizeOnFilterChange). Everything is pure DOM: CSP-clean, read-only floor
// untouched.
//
// keyboard.ts / palette.ts import the windowed-walk + harvest surfaces here
// DIRECTLY (the Unit-12 dismantling of the window.roClusterBridge seam): the
// virtualizer is a module now, so those callers reach it by name at call time.

import { requestListRefresh } from './refresh.js';
import { reapplyRowState } from './row-selection.js';
import {
    clampFocusIndex,
    prepareSwapSpacers,
    rowOffsetTop,
    scrollAdjustToReveal,
    spacerHeights,
    windowBounds,
} from './virtualizer-math.js';

// The live free-text hide class (owned by filters.ts; the literal is shared
// rather than imported so the matcher->virtualizer dependency stays one-way).
const FILTER_HIDE_CLASS = 'ro-row-filtered';

// roRowModel seam (owned by filters.ts), read at call time so the bundle's
// module-eval order is irrelevant. Only visibleKeys is read here.
function roRowModel(): { visibleKeys: Set<string> | null } {
    return (window as unknown as { roRowModel: { visibleKeys: Set<string> | null } }).roRowModel;
}

// roRowState focus seam (owned by row-selection.ts), read at call time.
function roRowState(): { setFocus(key: string): void; focusedKey(): string | null } {
    return (
        window as unknown as {
            roRowState: { setFocus(key: string): void; focusedKey(): string | null };
        }
    ).roRowState;
}

interface VirtState {
    active: boolean;
    rows: HTMLElement[]; // the FULL identity row set, server order
    byKey: Map<string, HTMLElement>; // key -> tr over `rows` (rendered or detached)
    visible: HTMLElement[]; // rows passing the live free-text filter, in order
    rowH: number; // the measured fixed row pitch (px)
    start: number; // rendered slice bounds over `visible`
    end: number;
    table: HTMLTableElement | null;
    tbody: HTMLTableSectionElement | null;
    topSpacer: HTMLTableRowElement | null;
    bottomSpacer: HTMLTableRowElement | null;
    pinnedWidths: number[]; // engagement-time column widths (full-render truth)
    pendingRows: HTMLElement[] | null; // adoption handoff from the ro-morph handleSwap
    pendingScrollY: number | null;
}

const virtState: VirtState = {
    active: false,
    rows: [],
    byKey: new Map(),
    visible: [],
    rowH: 0,
    start: 0,
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],
    pendingRows: null,
    pendingScrollY: null,
};

export function virtualizerActive(): boolean {
    return virtState.active && !!virtState.tbody && virtState.tbody.isConnected;
}

function virtReset(): void {
    virtState.active = false;
    virtState.rows = [];
    virtState.byKey = new Map();
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
}

// virtMakeSpacer builds one spacer row: a single cell whose height is the only
// thing that matters (the CSS zeroes its padding/border and detaches it from the
// sticky first-column rules). aria-hidden keeps it out of the a11y tree.
function virtMakeSpacer(): HTMLTableRowElement {
    const tr = document.createElement('tr');
    tr.className = 'ro-vspacer';
    tr.setAttribute('aria-hidden', 'true');
    tr.appendChild(document.createElement('td'));
    return tr;
}

function virtSetSpacerColspan(): void {
    const cols = (virtState.table as HTMLTableElement).querySelectorAll('thead th').length || 1;
    (
        (virtState.topSpacer as HTMLTableRowElement).firstElementChild as HTMLTableCellElement
    ).colSpan = cols;
    (
        (virtState.bottomSpacer as HTMLTableRowElement).firstElementChild as HTMLTableCellElement
    ).colSpan = cols;
}

// virtMeasureRowHeight returns the mean row pitch of the CURRENTLY RENDERED
// identity rows (exact at engagement, when the full set is in the DOM).
function virtMeasureRowHeight(): number {
    const rendered = (virtState.tbody as HTMLTableSectionElement).querySelectorAll(
        ':scope > tr[data-key]',
    );
    if (rendered.length === 0) {
        return 0;
    }
    const first = rendered[0].getBoundingClientRect();
    const last = rendered[rendered.length - 1].getBoundingClientRect();
    const pitch = (last.bottom - first.top) / rendered.length;
    return pitch > 0 ? pitch : 0;
}

// virtFallbackRowHeight is the D20 formula (--row-py×2 + line-height + the row
// border) -- only a one-frame seed for the cold-adoption render before a real
// measurement corrects it.
function virtFallbackRowHeight(): number {
    let py = 9;
    let lh = 18;
    try {
        const cs = window.getComputedStyle(document.documentElement);
        py = parseFloat(cs.getPropertyValue('--row-py')) || py;
        const cell = virtState.tbody?.querySelector('td');
        if (cell) {
            lh = parseFloat(window.getComputedStyle(cell).lineHeight) || lh;
        }
    } catch {
        // keep the static seed
    }
    return py * 2 + lh + 1;
}

// virtApplyPins re-applies the stored engagement-time column widths (a morph
// syncs the server's attribute-less <th>s over the pins on every tick). Returns
// false when the column SET changed (the D8 popover re-rendered the table with
// different columns) -- the caller re-measures then.
function virtApplyPins(): boolean {
    const ths = (virtState.table as HTMLTableElement).querySelectorAll('thead th');
    if (virtState.pinnedWidths.length !== ths.length) {
        return false;
    }
    ths.forEach((th, i) => {
        (th as HTMLElement).style.width = `${virtState.pinnedWidths[i]}px`;
    });
    (virtState.table as HTMLTableElement).classList.add('ro-virtualized');
    return true;
}

// virtPinColumns measures the auto-layout column widths and freezes them
// (style.width on the header cells + fixed table layout via .ro-virtualized), so
// the window's content can never re-derive column widths scroll-step by
// scroll-step. At engagement the measurement sees the FULL render -- the true
// content-driven widths.
function virtPinColumns(): void {
    const ths = Array.from((virtState.table as HTMLTableElement).querySelectorAll('thead th'));
    virtState.pinnedWidths = ths.map((th) => th.getBoundingClientRect().width);
    virtApplyPins();
}

// virtComputeVisible derives the renderable row list from the full set and the
// live free-text match (roRowModel.visibleKeys; null = no filter). The MATCH
// itself ran on the full row model -- never the DOM window (D7/D20).
function virtComputeVisible(): void {
    const keys = roRowModel().visibleKeys;
    virtState.visible = keys
        ? virtState.rows.filter((tr) => keys.has(tr.dataset.key as string))
        : virtState.rows.slice();
}

// virtRenderWindow renders the current slice between the two spacers and re-keys
// the identity row state onto whatever is now in the DOM. Rendered rows are
// visible by construction, so any stale live-filter hide class from an earlier
// render is stripped.
function virtRenderWindow(): void {
    const s = virtState;
    const tbody = s.tbody as HTMLTableSectionElement;
    const rect = tbody.getBoundingClientRect();
    const bounds = windowBounds(rect.top, window.innerHeight, s.rowH, s.visible.length);
    s.start = bounds.start;
    s.end = bounds.end;
    const heights = spacerHeights(s.start, s.end, s.visible.length, s.rowH);
    ((s.topSpacer as HTMLTableRowElement).firstElementChild as HTMLElement).style.height =
        `${heights.top}px`;
    ((s.bottomSpacer as HTMLTableRowElement).firstElementChild as HTMLElement).style.height =
        `${heights.bottom}px`;
    const slice = s.visible.slice(s.start, s.end);
    slice.forEach((tr) => {
        tr.classList.remove(FILTER_HIDE_CLASS);
    });
    tbody.replaceChildren(s.topSpacer as Node, ...slice, s.bottomSpacer as Node);
    reapplyRowState();
}

// virtBindMounts re-resolves the live table/tbody from the document (a morph may
// have replaced the nodes the virtualizer held).
function virtBindMounts(): boolean {
    const content = document.getElementById('resource-list-content');
    const wrap = content?.querySelector('.ro-table-wrap.ro-windowed');
    const table = wrap?.querySelector('table.ro-table');
    const tbody =
        table && (table as HTMLTableElement).tBodies.length > 0
            ? (table as HTMLTableElement).tBodies[0]
            : null;
    virtState.table = (table as HTMLTableElement) || null;
    virtState.tbody = tbody || null;
    return !!tbody;
}

// virtualizeInit is the runInit engagement step. ORDER CONTRACT: it runs AFTER
// captureRowModelFromDocument -- at engagement the DOM still IS the complete
// dataset, and the model must capture it before the window prunes the rows.
export function virtualizeInit(): void {
    const content = document.getElementById('resource-list-content');
    const wrap = content?.querySelector('.ro-table-wrap.ro-windowed');
    if (!wrap) {
        virtReset(); // small list / non-list page: windowing disengaged
        return;
    }
    const table = wrap.querySelector('table.ro-table') as HTMLTableElement | null;
    const tbody = table && table.tBodies.length > 0 ? table.tBodies[0] : null;
    if (!tbody) {
        virtReset();
        return;
    }
    if (tbody.querySelector(':scope > tr.ro-vspacer')) {
        if (virtState.active && virtState.tbody === tbody) {
            return; // already engaged on this very tbody (idempotent re-init)
        }
        // A WINDOWED snapshot restored from the history cache: only the cached
        // window's rows exist, the full set is gone. Re-fetch the complete
        // fragment through the container's own programmatic path (RO-No-Push);
        // the adoption pipeline rebuilds the window from it.
        virtReset();
        requestListRefresh();
        return;
    }
    // A fresh full render (initial load or a boosted body swap): the DOM holds
    // the COMPLETE dataset right now -- collect it, measure the row pitch and the
    // true column widths against it, then window.
    const rows = Array.from(tbody.querySelectorAll(':scope > tr[data-key]')) as HTMLElement[];
    if (rows.length === 0) {
        virtReset(); // a v1 multi-type page: no identity rows -> no windowing
        return;
    }
    virtReset();
    virtState.table = table;
    virtState.tbody = tbody;
    virtState.rows = rows;
    virtState.byKey = new Map(rows.map((tr) => [tr.dataset.key as string, tr]));
    virtState.topSpacer = virtMakeSpacer();
    virtState.bottomSpacer = virtMakeSpacer();
    virtSetSpacerColspan();
    virtState.rowH = virtMeasureRowHeight() || virtFallbackRowHeight();
    virtPinColumns();
    virtState.active = true;
    virtComputeVisible();
    virtRenderWindow();
}

// virtualizePrepareSwap runs INSIDE the ro-morph handleSwap, after the row model
// was captured from the fragment: a >threshold fragment's rows are detached for
// adoption and replaced with two height-preserving spacers, so 600 rows never
// ride the morph and the document height never dips mid-swap.
export function virtualizePrepareSwap(fragment: DocumentFragment): void {
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
    const wrap = fragment.querySelector('.ro-table-wrap.ro-windowed');
    const tbody = wrap ? wrap.querySelector('table.ro-table tbody') : null;
    if (!tbody) {
        return; // below-threshold fragment -> plain morph; afterSwap disengages
    }
    const rows: HTMLElement[] = [];
    Array.prototype.forEach.call(tbody.children, (el: HTMLElement) => {
        if (el.tagName === 'TR' && el.dataset.key) {
            rows.push(el);
        }
    });
    if (rows.length === 0) {
        return;
    }
    virtState.pendingRows = rows;
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const priorStart = virtState.active ? virtState.start : 0;
    const heights = prepareSwapSpacers(priorStart, rows.length, rowH);
    const topSpacer = virtMakeSpacer();
    const bottomSpacer = virtMakeSpacer();
    (topSpacer.firstElementChild as HTMLElement).style.height = `${heights.top}px`;
    (bottomSpacer.firstElementChild as HTMLElement).style.height = `${heights.bottom}px`;
    tbody.replaceChildren(topSpacer, bottomSpacer);
}

// virtualizeAfterSwap completes the morph pipeline on htmx:afterSwap. It runs
// AFTER applyLiveNameFilter re-derived visibleKeys from the surviving draft, so
// the re-window consumes fresh filter state.
export function virtualizeAfterSwap(): void {
    const pending = virtState.pendingRows;
    virtState.pendingRows = null;
    if (!pending) {
        // The fragment fell below the threshold (or was a whole-list state
        // block): the morph landed the complete content in the DOM, so the
        // virtualizer disengages and leaves it alone.
        if (virtState.active) {
            virtReset();
        }
        return;
    }
    const prior = virtState.byKey;
    const wasActive = virtState.active;
    if (!virtBindMounts()) {
        virtReset();
        return;
    }
    virtState.rows = pending;
    virtState.byKey = new Map(pending.map((tr) => [tr.dataset.key as string, tr]));
    if (!virtState.topSpacer) {
        virtState.topSpacer = virtMakeSpacer();
        virtState.bottomSpacer = virtMakeSpacer();
    }
    virtSetSpacerColspan();
    virtState.active = true;
    if (!virtState.rowH) {
        virtState.rowH = virtFallbackRowHeight();
    }
    virtComputeVisible();
    virtRenderWindow();
    if (!wasActive) {
        // Cold adoption (a chip removal jumped the list back over the
        // threshold): correct the seeded row pitch against real rows once.
        const measured = virtMeasureRowHeight();
        if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
            virtState.rowH = measured;
            virtRenderWindow();
        }
    }
    // The morph synced the server's <th>s over the width pins and the
    // .ro-virtualized class -- re-apply the engagement-time widths (or re-measure
    // when the column set itself changed, e.g. a D8 toggle).
    if (!virtApplyPins()) {
        virtPinColumns();
    }
    // A reflow between the morph and this render could have clamped the scroll
    // against the spacer-only table; the heights are exact again, so the
    // captured offset is reachable -- restore it.
    if (virtState.pendingScrollY !== null && window.scrollY !== virtState.pendingScrollY) {
        window.scrollTo(0, virtState.pendingScrollY);
        virtRenderWindow();
    }
    virtState.pendingScrollY = null;
    virtFlashChangedCells(prior);
}

// virtFlashChangedCells keeps the §8.3 changed-cell flash honest while windowed:
// rows bypass idiomorph (its cell-flash callbacks never fire), so the rendered
// window is diffed here against the prior row set by identity. Disabled under
// prefers-reduced-motion exactly like the idiomorph hooks.
function virtFlashChangedCells(prior: Map<string, HTMLElement>): void {
    if (
        !prior ||
        prior.size === 0 ||
        window.matchMedia('(prefers-reduced-motion: reduce)').matches
    ) {
        return;
    }
    (virtState.tbody as HTMLTableSectionElement)
        .querySelectorAll(':scope > tr[data-key]')
        .forEach((tr) => {
            const old = prior.get((tr as HTMLElement).dataset.key as string);
            if (!old) {
                return;
            }
            const oldCells = old.children;
            const newCells = tr.children;
            for (let i = 0; i < newCells.length; i++) {
                const o = oldCells[i];
                const nd = newCells[i];
                if (o && nd && nd.tagName === 'TD' && o.textContent !== nd.textContent) {
                    nd.classList.remove('ro-cell-changed');
                    void (nd as HTMLElement).offsetWidth; // restart the animation
                    nd.classList.add('ro-cell-changed');
                }
            }
        });
}

// virtualizeOnFilterChange re-windows over the new visible set whenever the live
// free-text match changes (applyLiveNameFilter calls it last). The match ran on
// the FULL row model, so a name outside the rendered window still narrows to its
// row here. No-op mid-adoption: virtualizeAfterSwap is about to recompute
// everything anyway.
export function virtualizeOnFilterChange(): void {
    if (!virtualizerActive() || virtState.pendingRows) {
        return;
    }
    virtComputeVisible();
    virtRenderWindow();
}

// virtMoveFocus is the j/k walker while windowed: it steps through the FULL
// visible row list (the DOM only holds the window), scrolls the window to the
// target row, and hands the key to the identity focus store. Imported by
// keyboard.ts (the windowed half of moveRowFocus).
export function virtMoveFocus(delta: number): boolean {
    const list = virtState.visible;
    if (list.length === 0) {
        return false;
    }
    let current = -1;
    const focusKey = roRowState().focusedKey();
    for (let i = 0; i < list.length; i++) {
        if (list[i].dataset.key === focusKey) {
            current = i;
            break;
        }
    }
    const next = clampFocusIndex(current, delta, list.length);
    virtualizeScrollToIndex(next);
    roRowState().setFocus(list[next].dataset.key as string);
    return true;
}

// virtualizeScrollToIndex makes the visible-list row at `index` rendered AND
// inside the viewport (under the sticky topbar) -- the focus jump that scrolls
// the window. scrollBy is synchronous, so the immediate re-render lands the row
// before the caller paints focus onto it.
function virtualizeScrollToIndex(index: number): void {
    const rect = (virtState.tbody as HTMLTableSectionElement).getBoundingClientRect();
    const rowTop = rowOffsetTop(rect.top, index, virtState.rowH);
    const topbar = document.querySelector('header.ro-topbar');
    const topMin = topbar ? topbar.getBoundingClientRect().bottom : 0;
    const delta = scrollAdjustToReveal(rowTop, virtState.rowH, topMin, window.innerHeight);
    if (delta !== 0) {
        window.scrollBy(0, delta);
    }
    virtRenderWindow();
}

// virtRows / virtVisible / virtRowByKey -- the full-set readers keyboard.ts /
// palette.ts harvest from while windowed (the DOM holds only a window). Imported
// directly (the Unit-12 cluster-bridge dismantling).
export function virtRows(): HTMLElement[] {
    return virtState.rows;
}
export function virtVisible(): HTMLElement[] {
    return virtState.visible;
}
export function virtRowByKey(key: string): HTMLElement | null {
    return virtState.byKey.get(key) || null;
}

// The scroll re-window: one passive document-level listener, rAF-throttled,
// inert unless the virtualizer is engaged. Re-renders only when the slice bounds
// actually moved.
let virtScrollScheduled = false;
function virtOnScroll(): void {
    if (!virtualizerActive()) {
        return;
    }
    const rect = (virtState.tbody as HTMLTableSectionElement).getBoundingClientRect();
    const bounds = windowBounds(
        rect.top,
        window.innerHeight,
        virtState.rowH,
        virtState.visible.length,
    );
    if (bounds.start !== virtState.start || bounds.end !== virtState.end) {
        virtRenderWindow();
    }
}
window.addEventListener(
    'scroll',
    () => {
        if (!virtState.active || virtScrollScheduled) {
            return;
        }
        virtScrollScheduled = true;
        window.requestAnimationFrame(() => {
            virtScrollScheduled = false;
            virtOnScroll();
        });
    },
    { passive: true },
);
// Viewport growth widens the needed window (row pitch itself is re-measured only
// at engagement; the fixed-height law keeps it stable in between).
window.addEventListener('resize', virtOnScroll);
// Web-font activation can shift the line-height the row pitch was measured
// against (engagement at DOMContentLoaded can precede the Geist swap-in);
// re-measure once the fonts settle.
if (document.fonts?.ready && typeof document.fonts.ready.then === 'function') {
    document.fonts.ready.then(() => {
        if (!virtualizerActive()) {
            return;
        }
        const measured = virtMeasureRowHeight();
        if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
            virtState.rowH = measured;
            virtRenderWindow();
        }
    });
}

// The deliberate external seam (e2e / console), the roRowState/roFuzzy pattern:
// inspection plus the scroll-to-identity jump the specs drive. window.roVirtual
// is an e2e contract (windowing.spec.ts) -- the names active/renderedBounds/
// scrollToKey are frozen.
(
    window as unknown as {
        roVirtual: {
            active(): boolean;
            renderedBounds(): { start: number; end: number; total: number };
            scrollToKey(key: string): boolean;
        };
    }
).roVirtual = {
    active: virtualizerActive,
    renderedBounds() {
        return { start: virtState.start, end: virtState.end, total: virtState.visible.length };
    },
    scrollToKey(key: string) {
        if (!virtualizerActive()) {
            return false;
        }
        const tr = virtState.byKey.get(key);
        const index = tr ? virtState.visible.indexOf(tr) : -1;
        if (index === -1) {
            return false;
        }
        virtualizeScrollToIndex(index);
        return true;
    },
};
