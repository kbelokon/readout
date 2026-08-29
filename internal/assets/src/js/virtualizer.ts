// virtualizer.ts -- client-side row windowing above ~500 rows,
// migrated from legacy.js. Lists always render COMPLETE server-side (no
// pagination, ever). Above the threshold the server marks the table wrap
// `.ro-windowed` (the threshold has ONE owner: resource_table.templ; this module
// only follows the marker) and the virtualizer takes ownership of tbody
// geometry: it renders a viewport slice (+ buffer) from the canonical full row
// set owned by list-projection.ts, framed by two spacer rows whose heights stand
// in for everything off-window. The fixed row height (--row-py×2 + line-height,
// guaranteed by the windowed clamp CSS + the server-side expansion flattening)
// makes the offset math exact: it is MEASURED once per engagement as the mean
// row pitch of the full render, so no per-row rounding accumulates across 600
// rows.
//
// The PURE arithmetic (window boundaries, spacer heights, the focus-jump scroll
// delta, the focus clamp) lives in virtualizer-math.ts (unit-tested); this
// module is the DOM + state machine around it: measuring, building spacers,
// re-rendering the slice, and the morph-adoption pipeline.
//
// HTMX morphs (refresh, sort and filter): a >threshold fragment's rows NEVER
// ride the morph. morph.ts hands them to virtualizePrepareSwap, which detaches them for
// adoption and leaves height-preserving spacers in the fragment. After the morph
// lands, virtualizeAfterSwap adopts the new full row set and re-renders the
// window -- selection/focus re-key by identity exactly like every other swap,
// and changed cells still flash (the idiomorph cell-flash callbacks never see
// windowed rows, so the diff runs here against the prior row set).
//
// The free-text matcher, autocomplete and virtualizer all consume the same
// list-projection snapshot.  This module never keeps a second rows/byKey/order
// store; its state is limited to geometry, mounts and the derived visible view.
// Everything is pure DOM: CSP-clean, read-only floor untouched.
//
// keyboard.ts / palette.ts import the windowed-walk + harvest surfaces here
// DIRECTLY (the Unit-12 dismantling of the window.roClusterBridge seam): the
// virtualizer is a module now, so those callers reach it by name at call time.

import { clearListValidator } from './list-etag.js';
import {
    commitListProjectionSwap,
    ensureListProjection,
    listProjectionRowByKey,
    listProjectionRows,
    listProjectionSwapPending,
    listProjectionVisibleRows,
    listProjectionWindowed,
    prepareListProjectionSwap,
    resetListProjection,
} from './list-projection.js';
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
    visible: HTMLElement[]; // derived rows passing the live free-text filter, in order
    rowH: number; // the measured fixed row pitch (px)
    start: number; // rendered slice bounds over `visible`
    end: number;
    table: HTMLTableElement | null;
    tbody: HTMLTableSectionElement | null;
    topSpacer: HTMLTableRowElement | null;
    bottomSpacer: HTMLTableRowElement | null;
    pinnedWidths: number[]; // engagement-time column widths (full-render truth)
    pendingScrollY: number | null;
}

interface HistoryRecoveryPending {
    content: HTMLElement;
    tbody: HTMLTableSectionElement;
}

const virtState: VirtState = {
    active: false,
    visible: [],
    rowH: 0,
    start: 0,
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],
    pendingScrollY: null,
};

// Keep history recovery one-shot for the exact mounted content/tbody pair. This
// also makes the exported initializer safe if a defensive caller repeats it
// before the forced rebuild settles; an actual list afterSwap (or a fresh
// complete model) clears the guard.
let historyRecoveryPending: HistoryRecoveryPending | null = null;

export function virtualizerActive(): boolean {
    return virtState.active && virtState.tbody?.isConnected === true;
}

function virtReset(): void {
    virtState.active = false;
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
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
    return Math.max(0, pitch);
}

// virtFallbackRowHeight is the fixed-row-height formula (--row-py×2 + line-height + the row
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
// false when the column SET changed (the columns popover re-rendered the table with
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

// virtComputeVisible derives the renderable row list from the canonical full
// projection and its live free-text match. The MATCH itself ran on the full row
// model -- never the DOM window.
function virtComputeVisible(): void {
    virtState.visible = listProjectionVisibleRows();
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
    const table = wrap?.querySelector<HTMLTableElement>('table.ro-table') ?? null;
    const tbody = table?.tBodies.item(0) ?? null;
    virtState.table = table;
    virtState.tbody = tbody;
    return tbody !== null;
}

// virtualizeInit is the runInit engagement step. At engagement the DOM still IS
// the complete dataset, so the canonical projection adopts it before this step
// prunes the tbody to a viewport window.
export function virtualizeInit(): void {
    const content = document.getElementById('resource-list-content');
    const wrap = content?.querySelector('.ro-table-wrap.ro-windowed');
    if (!content) {
        resetListProjection();
        virtReset();
        return;
    }
    if (!wrap) {
        // A small list still belongs to the canonical projection; only viewport
        // geometry disengages.
        ensureListProjection(content);
        virtReset(); // small list / non-list page: windowing disengaged
        return;
    }
    const table = wrap.querySelector<HTMLTableElement>('table.ro-table');
    const tbody = table?.tBodies.item(0) ?? null;
    if (!tbody) {
        ensureListProjection(content);
        virtReset();
        return;
    }
    if (tbody.querySelector(':scope > tr.ro-vspacer')) {
        if (virtState.active && virtState.tbody === tbody) {
            return; // already engaged on this very tbody (idempotent re-init)
        }
        if (historyRecoveryPending?.content === content && historyRecoveryPending.tbody === tbody) {
            return; // this exact cached slice already has one rebuild in flight
        }
        // A WINDOWED snapshot restored from the history cache: only the cached
        // window's rows exist, the full set is gone. Re-fetch the complete
        // fragment through the container's own programmatic path (RO-No-Push);
        // the adoption pipeline rebuilds the window from it. The cached DOM is
        // only a viewport slice, so its otherwise-valid ETag cannot authorize a
        // bodyless 304: force one full 200 model before conditionals resume.
        virtReset();
        ensureListProjection(content);
        historyRecoveryPending = { content, tbody };
        clearListValidator();
        requestListRefresh();
        return;
    }
    // A fresh full render (initial load or a boosted body swap): the DOM holds
    // the COMPLETE dataset right now -- collect it, measure the row pitch and the
    // true column widths against it, then window.
    ensureListProjection(content);
    const rows = listProjectionRows();
    if (rows.length === 0) {
        virtReset(); // no identity rows -> no windowing
        return;
    }
    historyRecoveryPending = null;
    // Every engagement field below is replaced from the fresh full render. The
    // only state owned by an abandoned prepare is its captured scroll offset.
    virtState.pendingScrollY = null;
    virtState.table = table;
    virtState.tbody = tbody;
    virtState.topSpacer = virtMakeSpacer();
    virtState.bottomSpacer = virtMakeSpacer();
    virtSetSpacerColspan();
    virtState.rowH = virtMeasureRowHeight() || virtFallbackRowHeight();
    virtPinColumns();
    virtState.active = true;
    virtComputeVisible();
    virtRenderWindow();
}

// virtualizePrepareSwap runs INSIDE the ro-morph handleSwap, after the canonical
// projection was prepared from the fragment: a >threshold fragment's rows are
// detached for adoption and replaced with two height-preserving spacers, so 600
// rows never ride the morph and the document height never dips mid-swap.
export function virtualizePrepareSwap(fragment: DocumentFragment): void {
    virtState.pendingScrollY = null;
    // Idempotent for the same fragment: morph.ts prepares first, while this
    // defensive call keeps the virtualization boundary independently usable in
    // DOM tests and future swap adapters.
    const incoming = prepareListProjectionSwap(fragment);
    if (!incoming.windowed || incoming.rows.length === 0) {
        return;
    }
    const wrap = fragment.querySelector('.ro-table-wrap.ro-windowed');
    const tbody = wrap ? wrap.querySelector('table.ro-table tbody') : null;
    if (!tbody) {
        return; // below-threshold fragment -> plain morph; afterSwap disengages
    }
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const priorStart = virtState.active ? virtState.start : 0;
    const heights = prepareSwapSpacers(priorStart, incoming.rows.length, rowH);
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
    // htmx only emits afterSwap after a response actually landed. That success
    // resolves any one-shot history rebuild gate; HTTP failures never get here.
    historyRecoveryPending = null;
    const wasActive = virtState.active;
    const previousByKey = commitListProjectionSwap();
    if (!previousByKey || !listProjectionWindowed() || listProjectionRows().length === 0) {
        // The fragment fell below the threshold (or was a whole-list state
        // block): the morph landed the complete content in the DOM, so the
        // virtualizer disengages and leaves it alone.
        virtReset();
        return;
    }
    if (!virtBindMounts()) {
        resetListProjection();
        virtReset();
        return;
    }
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
    // when the column set itself changed, e.g. a columns-popover toggle).
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
    virtFlashChangedCells(previousByKey);
}

// virtFlashChangedCells keeps the changed-cell flash honest while windowed:
// rows bypass idiomorph (its cell-flash callbacks never fire), so the rendered
// window is diffed here against the prior row set by identity. Disabled under
// prefers-reduced-motion exactly like the idiomorph hooks.
function virtFlashChangedCells(prior: ReadonlyMap<string, HTMLElement>): void {
    if (prior.size === 0 || window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
        return;
    }
    (virtState.tbody as HTMLTableSectionElement)
        .querySelectorAll(':scope > tr[data-key]')
        .forEach((tr) => {
            const old = prior.get((tr as HTMLElement).dataset.key as string);
            if (!old) {
                return;
            }
            Array.from(tr.children).forEach((newCell, index) => {
                const oldCell = old.children.item(index);
                if (
                    oldCell &&
                    newCell.tagName === 'TD' &&
                    oldCell.textContent !== newCell.textContent
                ) {
                    newCell.classList.remove('ro-cell-changed');
                    void (newCell as HTMLElement).offsetWidth; // restart the animation
                    newCell.classList.add('ro-cell-changed');
                }
            });
        });
}

// virtualizeOnFilterChange re-windows over the new visible set whenever the live
// free-text match changes (applyLiveNameFilter calls it last). The match ran on
// the FULL row model, so a name outside the rendered window still narrows to its
// row here. No-op mid-adoption: virtualizeAfterSwap is about to recompute
// everything anyway.
export function virtualizeOnFilterChange(): void {
    if (!virtualizerActive() || listProjectionSwapPending()) {
        return;
    }
    virtComputeVisible();
    virtRenderWindow();
}

// The shared post-update pipeline has already re-rendered the current window.
// Diff only upserted pre-morph rows; no second re-window is needed.
export function virtualizeAfterDelta(
    previousByKey: ReadonlyMap<string, HTMLElement>,
    focusKey: string | null = null,
): void {
    if (!virtualizerActive()) return;
    if (focusKey) virtualizeRevealKey(focusKey);
    virtFlashChangedCells(previousByKey);
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
    const focusKey = roRowState().focusedKey();
    const current = list.findIndex((row) => row.dataset.key === focusKey);
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
    if (delta === 0) return;
    window.scrollBy(0, delta);
    virtRenderWindow();
}

// A Live reorder may move the row containing the real active descendant outside
// the current slice. Render that canonical key before list-projection restores it.
export function virtualizeRevealKey(key: string): boolean {
    if (!virtualizerActive()) return false;
    const index = virtState.visible.findIndex((row) => row.dataset.key === key);
    if (index === -1) return false;
    const row = virtState.visible[index] as HTMLElement;
    // A buffered row can be connected while still sitting beyond the actual
    // viewport. The geometry helper is the authority for both scrolling and the
    // synchronous re-window needed before focus restoration.
    virtualizeScrollToIndex(index);
    return row.isConnected;
}

// virtRows / virtVisible / virtRowByKey -- the full-set readers keyboard.ts /
// palette.ts harvest from while windowed (the DOM holds only a window). Imported
// directly (the Unit-12 cluster-bridge dismantling).
export function virtRows(): HTMLElement[] {
    return virtualizerActive() ? Array.from(listProjectionRows()) : [];
}
export function virtVisible(): HTMLElement[] {
    return virtState.visible;
}
export function virtRowByKey(key: string): HTMLElement | null {
    return virtualizerActive() ? listProjectionRowByKey(key) : null;
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
const fontReady = document.fonts?.ready;
if (fontReady && typeof fontReady.then === 'function') {
    void fontReady.then(() => {
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
        const tr = listProjectionRowByKey(key);
        const index = tr ? virtState.visible.indexOf(tr) : -1;
        if (index === -1) {
            return false;
        }
        virtualizeScrollToIndex(index);
        return true;
    },
};
