// virtualizer-math.test.ts -- node:test for the PURE windowing arithmetic. The
// window boundaries, the spacer heights, and the focus-jump scroll delta are the
// load-bearing decisions the e2e windowing spec exercises through 600 rendered
// rows; pinning them here (no DOM) catches an off-by-one at the unit boundary
// before it reaches a frame.
//
// Run: `node --test 'internal/assets/src/js/**/*.test.ts'`.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
    VIRT_BUFFER_ROWS,
    windowBounds,
    spacerHeights,
    prepareSwapSpacers,
    rowOffsetTop,
    scrollAdjustToReveal,
    clampFocusIndex,
} from './virtualizer-math.ts';

// --- windowBounds -----------------------------------------------------------

test('at the top (tbody at viewport y=0) the slice starts at 0 with no upward buffer', () => {
    // tbodyTop 0, innerHeight 600, rowH 20 -> first 0, last 30; start clamps to
    // 0 (0 - buffer floored at 0), end 30 + buffer.
    const b = windowBounds(0, 600, 20, 600);
    assert.equal(b.start, 0);
    assert.equal(b.end, 30 + VIRT_BUFFER_ROWS);
});

test('scrolled down, the slice tracks the viewport and carries the buffer both sides', () => {
    // tbodyTop -1000 (scrolled 1000px past the top), innerHeight 600, rowH 20.
    // first = floor(1000/20) = 50, last = ceil(1600/20) = 80.
    const b = windowBounds(-1000, 600, 20, 600);
    assert.equal(b.start, 50 - VIRT_BUFFER_ROWS);
    assert.equal(b.end, 80 + VIRT_BUFFER_ROWS);
});

test('end clamps to the visible count (never renders past the dataset)', () => {
    const b = windowBounds(-100000, 600, 20, 600);
    assert.equal(b.end, 600);
    assert.ok(b.start <= 600);
});

test('fully scrolled past the list yields an empty [n, n] slice', () => {
    // tbodyTop far negative beyond the whole list: first/last both exceed n, so
    // start clamps to n and end (< start) is pulled up to start.
    const n = 600;
    const b = windowBounds(-1_000_000, 600, 20, n);
    assert.equal(b.start, n);
    assert.equal(b.end, n);
});

test('a zero rowH is guarded to 1 (no divide-by-zero before the first measurement)', () => {
    const b = windowBounds(0, 600, 0, 600);
    assert.ok(Number.isFinite(b.start));
    assert.ok(Number.isFinite(b.end));
    assert.equal(b.start, 0);
});

test('a custom buffer overrides the default span', () => {
    const b = windowBounds(-1000, 600, 20, 600, 0);
    assert.equal(b.start, 50);
    assert.equal(b.end, 80);
});

// --- spacerHeights ----------------------------------------------------------

test('spacer heights stand in for the off-window rows above and below', () => {
    const s = spacerHeights(50, 90, 600, 20);
    assert.equal(s.top, 50 * 20); // 50 rows above
    assert.equal(s.bottom, (600 - 90) * 20); // 510 rows below
});

test('the full virtual height is conserved: top + rendered + bottom = n*rowH', () => {
    const start = 50;
    const end = 90;
    const n = 600;
    const rowH = 20;
    const s = spacerHeights(start, end, n, rowH);
    const rendered = (end - start) * rowH;
    assert.equal(s.top + rendered + s.bottom, n * rowH);
});

test('a window at the end has a zero bottom spacer (never negative)', () => {
    const s = spacerHeights(580, 600, 600, 20);
    assert.equal(s.bottom, 0);
});

// --- prepareSwapSpacers -----------------------------------------------------

test('prepare-swap spacers preserve the prior window start, clamped to the incoming count', () => {
    const s = prepareSwapSpacers(50, 600, 20);
    assert.equal(s.top, 50 * 20);
    assert.equal(s.bottom, (600 - 50) * 20);
});

test('prepare-swap clamps the start when the incoming fragment shrank below it', () => {
    // A chip removal could land a smaller list; start clamps to the new count.
    const s = prepareSwapSpacers(500, 100, 20);
    assert.equal(s.top, 100 * 20);
    assert.equal(s.bottom, 0);
});

// --- rowOffsetTop -----------------------------------------------------------

test('rowOffsetTop is the tbody top plus index*rowH (exact under the fixed-height law)', () => {
    assert.equal(rowOffsetTop(-1000, 85, 20), -1000 + 85 * 20);
    assert.equal(rowOffsetTop(0, 0, 20), 0);
});

// --- scrollAdjustToReveal ---------------------------------------------------

test('a row above the sticky topbar scrolls up by the deficit', () => {
    // rowTop 10, topMin 56 -> scroll up (negative) by 10 - 56.
    assert.equal(scrollAdjustToReveal(10, 20, 56, 800), 10 - 56);
});

test('a row below the viewport bottom scrolls down by the overflow', () => {
    // rowTop 790, rowH 20 -> bottom 810 > innerHeight 800 -> +10.
    assert.equal(scrollAdjustToReveal(790, 20, 56, 800), 10);
});

test('a row already in the band needs no scroll', () => {
    assert.equal(scrollAdjustToReveal(100, 20, 56, 800), 0);
});

// --- clampFocusIndex --------------------------------------------------------

test('a forward step from an unknown focus (-1) lands on row 0', () => {
    assert.equal(clampFocusIndex(-1, 1, 600), 0);
});

test('focus clamps at both ends of the visible list', () => {
    assert.equal(clampFocusIndex(0, -1, 600), 0);
    assert.equal(clampFocusIndex(599, 1, 600), 599);
    assert.equal(clampFocusIndex(85, 1, 600), 86);
});
