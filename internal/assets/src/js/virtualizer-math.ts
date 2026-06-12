// virtualizer-math.ts -- the PURE windowing arithmetic of the >~500-row list
// virtualizer, extracted from legacy.js so the offset/height/
// visible-range/spacer math is node-tested instead of trusted "by eye". No DOM,
// no globals: every input is a plain number, every output a plain object, so
// node:test can pin the window boundaries the e2e windowing spec only exercises
// through 600 rendered rows.
//
// The fixed-row-height contract (every windowed row has the SAME measured
// pitch, guaranteed by the windowed clamp CSS + the server-side expansion
// flattening) is what makes this arithmetic exact -- the offset from the tbody's
// viewport-relative top to any row index is `index * rowH`, with no per-row
// rounding to accumulate across the dataset. The virtualizer.ts module supplies
// the live measurements (tbody top, innerHeight, scrollY, the measured pitch)
// and applies the results to the DOM; this module decides nothing about the DOM.

// The buffer of rows rendered above/below the strict viewport slice, so a small
// scroll never blanks an edge before the next rAF re-window lands.
export const VIRT_BUFFER_ROWS = 12;

// A rendered slice over the visible-row list: [start, end). end is exclusive.
export interface WindowBounds {
    start: number;
    end: number;
}

// windowBounds computes the slice to render from the tbody's viewport-relative
// top (`tbodyTop`, a getBoundingClientRect().top -- negative once scrolled past),
// the viewport height, the fixed row pitch, and the visible-row count. The
// document is the scroller, so the slice is the rows whose y-range intersects
// [0, innerHeight), grown by `buffer` on each side and clamped to [0, n].
//
// rowH is guarded to >=1 so a not-yet-measured pitch can never divide by zero
// (the virtualizer seeds a fallback pitch before the first measurement). The
// clamps mirror the monolith exactly: start can never exceed n, and end never
// falls below start (a fully-scrolled-past list yields an empty [n, n] slice).
export function windowBounds(
    tbodyTop: number,
    innerHeight: number,
    rowH: number,
    visibleCount: number,
    buffer: number = VIRT_BUFFER_ROWS,
): WindowBounds {
    const pitch = rowH || 1;
    const n = visibleCount;
    const first = Math.floor((0 - tbodyTop) / pitch);
    const last = Math.ceil((innerHeight - tbodyTop) / pitch);
    let start = Math.max(0, first - buffer);
    let end = Math.min(n, last + buffer);
    if (start > n) {
        start = n;
    }
    if (end < start) {
        end = start;
    }
    return { start, end };
}

// spacerHeights returns the two spacer-row heights that stand in for the rows
// OUTSIDE the rendered slice: the top spacer covers `start` rows, the bottom
// spacer covers the `n - end` rows below. Their combined height plus the
// rendered slice equals the full virtual list height, so the document scrollbar
// matches the true dataset and an off-window scroll position stays reachable.
export interface SpacerHeights {
    top: number;
    bottom: number;
}
export function spacerHeights(
    start: number,
    end: number,
    visibleCount: number,
    rowH: number,
): SpacerHeights {
    return {
        top: start * rowH,
        bottom: Math.max(0, visibleCount - end) * rowH,
    };
}

// prepareSwapSpacers sizes the two height-preserving spacers the ro-morph
// handleSwap leaves in place of a >threshold fragment's detached rows (so the
// 600-row morph never rides the DOM and the document height never dips
// mid-swap). The top spacer preserves the rows above the prior window start
// (clamped to the incoming row count); the bottom spacer preserves the rest.
export function prepareSwapSpacers(
    priorStart: number,
    incomingRowCount: number,
    rowH: number,
): SpacerHeights {
    const start = Math.min(priorStart, incomingRowCount);
    return {
        top: start * rowH,
        bottom: Math.max(0, incomingRowCount - start) * rowH,
    };
}

// rowOffsetTop returns the viewport-relative y of the row at `index` given the
// tbody's current top -- the focus-jump target the j/k walker scrolls to.
export function rowOffsetTop(tbodyTop: number, index: number, rowH: number): number {
    return tbodyTop + index * rowH;
}

// scrollAdjustToReveal returns the scrollBy delta that brings the row spanning
// [rowTop, rowTop+rowH) fully into the band [topMin, innerHeight) -- under the
// sticky topbar (topMin) and above the viewport bottom. 0 = already in band.
// Mirrors virtualizeScrollToIndex: scroll up when the row is above topMin,
// scroll down when its bottom is past innerHeight, else leave the scroll alone.
export function scrollAdjustToReveal(
    rowTop: number,
    rowH: number,
    topMin: number,
    innerHeight: number,
): number {
    const rowBottom = rowTop + rowH;
    if (rowTop < topMin) {
        return rowTop - topMin;
    }
    if (rowBottom > innerHeight) {
        return rowBottom - innerHeight;
    }
    return 0;
}

// clampFocusIndex steps a focus index by delta and clamps to [0, n-1]; when the
// current index is unknown (-1, the focus key is not in the visible list) a
// forward step lands on row 0 (current -1 + delta 1 -> 0), mirroring the
// monolith's `Math.max(0, Math.min(n-1, current + delta))`.
export function clampFocusIndex(current: number, delta: number, visibleCount: number): number {
    return Math.max(0, Math.min(visibleCount - 1, current + delta));
}
