// @vitest-environment jsdom

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

import {
    ageBucket,
    clockNow,
    clockPaint,
    estimateClockOffset,
    humanDuration,
    paintClocks,
    resetClockCellsForTest,
    setClockOffset,
    startClockCells,
} from './clock-cells.js';

interface ClockCases {
    durations: { ms: number; text: string }[];
    buckets: { ms: number; graceSeconds: number; bucket: string }[];
}

// The same fixture pins the Go side (clock_cells_test.go) to apimachinery's
// HumanDuration and the server's age buckets.
const cases = JSON.parse(
    readFileSync(join(process.cwd(), 'internal/web/testdata/clock_cells_cases.json'), 'utf8'),
) as ClockCases;

const T0 = Date.UTC(2026, 10, 4, 16, 0, 0);
const CREATED = T0 / 1000 - 36;

function setVisibility(hidden: boolean): void {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
    document.dispatchEvent(new Event('visibilitychange'));
}

function renderRow(): void {
    document.body.innerHTML = `
        <table class="ro-table"><tbody>
            <tr data-key="c/ns/web">
                <td class="cell-name"><a>web</a></td>
                <td class="num"><span class="restarts some">3</span> <span class="ago" data-ro-t="${CREATED - 300}" data-ro-tf="rago">(5m ago)</span></td>
                <td class="age-fresh num" title="created" data-ro-t="${CREATED}" data-ro-tf="age">36s</td>
                <td class="age-fresh" data-ro-t="${CREATED}" data-ro-tf="created">2026-11-04 15:59:24</td>
            </tr>
        </tbody></table>
        <div class="ro-cardlist"><div class="ro-pcard" data-key="c/ns/web">
            <span class="age-fresh" data-ro-t="${CREATED}" data-ro-tf="age">36s</span>
        </div></div>`;
}

function text(selector: string): string {
    return (document.querySelector(selector) as HTMLElement).textContent || '';
}

beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(T0);
    resetClockCellsForTest();
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => false });
});

afterEach(() => {
    resetClockCellsForTest();
    vi.useRealTimers();
    document.body.innerHTML = '';
    document.head.innerHTML = '';
});

describe('formulas', () => {
    test('humanDuration matches apimachinery on the shared boundary fixture', () => {
        for (const c of cases.durations) {
            expect(humanDuration(c.ms), `${c.ms}ms`).toBe(c.text);
        }
    });

    test('ageBucket matches the server buckets on the shared fixture', () => {
        for (const c of cases.buckets) {
            expect(ageBucket(c.ms, c.graceSeconds), `${c.ms}ms grace ${c.graceSeconds}`).toBe(
                c.bucket,
            );
        }
    });

    test('every format renders its own shape', () => {
        expect(clockPaint('age', 125_000)).toEqual({ text: '2m5s', bucket: 'age-fresh' });
        expect(clockPaint('created', 125_000)).toEqual({ text: null, bucket: 'age-fresh' });
        expect(clockPaint('ago', 125_000)).toEqual({ text: '2m5s ago', bucket: 'age-fresh' });
        expect(clockPaint('dur', 125_000)).toEqual({ text: '2m5s', bucket: null });
        expect(clockPaint('rago', 125_000)).toEqual({ text: '(2m5s ago)', bucket: null });
        expect(clockPaint('ev', 125_000)).toEqual({ text: '2m5s', bucket: 'age-fresh' });
        expect(clockPaint('evfirst', 125_000)).toEqual({ text: '(first 2m5s ago)', bucket: null });
        expect(clockPaint('bogus', 125_000)).toBeNull();
    });
});

describe('painting', () => {
    test('re-derives text and colour from the timestamp, table and card alike', () => {
        renderRow();
        expect(paintClocks(document, T0 + 3 * 60 * 60 * 1000)).toBe(true);
        expect(text('tr td[data-ro-tf="age"]')).toBe('3h');
        expect(text('.ro-pcard [data-ro-tf="age"]')).toBe('3h');
        expect(text('.ago')).toBe('(3h5m ago)');
        expect(text('td[data-ro-tf="created"]')).toBe('2026-11-04 15:59:24');
        const age = document.querySelector('tr td[data-ro-tf="age"]') as HTMLElement;
        expect(age.className).toBe('num age-recent');
        expect(document.querySelector('td[data-ro-tf="created"]')?.className).toBe('age-recent');
        expect(paintClocks(document, T0 + 3 * 60 * 60 * 1000)).toBe(false);
    });

    test('paints a detached row and ignores a malformed timestamp', () => {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td data-ro-t="x" data-ro-tf="age">1s</td>';
        const span = document.createElement('span');
        span.dataset.roT = String(CREATED);
        span.dataset.roTf = 'dur';
        span.textContent = '1s';
        expect(paintClocks(tr, T0)).toBe(false);
        expect(tr.textContent).toBe('1s');
        expect(paintClocks(span, T0)).toBe(true);
        expect(span.textContent).toBe('36s');
    });
});

describe('the page timer', () => {
    test('advances a cell with no server event, once a second', () => {
        renderRow();
        startClockCells();
        expect(text('tr td[data-ro-tf="age"]')).toBe('36s');
        vi.advanceTimersByTime(1000);
        expect(text('tr td[data-ro-tf="age"]')).toBe('37s');
        vi.advanceTimersByTime(90_000);
        expect(text('tr td[data-ro-tf="age"]')).toBe('2m7s');
    });

    test('repeated init keeps one timer', () => {
        renderRow();
        startClockCells();
        startClockCells();
        startClockCells();
        expect(vi.getTimerCount()).toBe(1);
    });

    test('stops while hidden and catches up on return', () => {
        renderRow();
        startClockCells();
        setVisibility(true);
        expect(vi.getTimerCount()).toBe(0);
        vi.setSystemTime(T0 + 10 * 60 * 1000);
        expect(text('tr td[data-ro-tf="age"]')).toBe('36s');
        setVisibility(false);
        expect(text('tr td[data-ro-tf="age"]')).toBe('10m');
        expect(vi.getTimerCount()).toBe(1);
    });

    test('a hidden document starts paused', () => {
        renderRow();
        Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
        startClockCells();
        expect(vi.getTimerCount()).toBe(0);
        expect(text('tr td[data-ro-tf="age"]')).toBe('36s');
    });
});

describe('server clock offset', () => {
    function navigation(entry: Partial<PerformanceNavigationTiming> | null): void {
        vi.spyOn(performance, 'getEntriesByType').mockReturnValue(
            entry ? [entry as PerformanceEntry] : [],
        );
    }

    function meta(content: string): void {
        document.head.innerHTML = `<meta name="ro-server-time" content="${content}">`;
    }

    test('estimates server minus browser at the first response byte', () => {
        meta(String(performance.timeOrigin + 500 + 4000));
        navigation({ type: 'navigate', transferSize: 1200, responseStart: 500 });
        expect(estimateClockOffset()).toBeCloseTo(4000, 3);
    });

    test('trusts no cached or restored document', () => {
        meta(String(performance.timeOrigin + 500 + 4000));
        navigation({ type: 'back_forward', transferSize: 1200, responseStart: 500 });
        expect(estimateClockOffset()).toBe(0);
        navigation({ type: 'navigate', transferSize: 0, responseStart: 500 });
        expect(estimateClockOffset()).toBe(0);
        navigation(null);
        expect(estimateClockOffset()).toBe(0);
    });

    test('ignores a missing or malformed meta', () => {
        navigation({ type: 'navigate', transferSize: 1200, responseStart: 500 });
        expect(estimateClockOffset()).toBe(0);
        meta('soon');
        expect(estimateClockOffset()).toBe(0);
    });

    test('applies the offset to every derived time', () => {
        setClockOffset(5000);
        expect(clockNow()).toBe(T0 + 5000);
        renderRow();
        paintClocks(document);
        expect(text('tr td[data-ro-tf="age"]')).toBe('41s');
    });
});
