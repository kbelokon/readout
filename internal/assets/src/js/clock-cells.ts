// Clock cells: the list cells whose text is a function of "now" (Age, a
// restart's "(5m ago)", a running Job's Duration, a CronJob's Last Schedule, an
// Event's Last/First Seen) and the Created cell's age colour. The server renders
// each one current for its instant and marks the element with the object
// timestamp it counts from:
//
//   data-ro-t   the timestamp, Unix seconds
//   data-ro-tf  the rendering format (see paintElement)
//
// One page-level timer re-derives every connected element from its timestamp
// once a second -- never by incrementing the previous text, so a suspended tab
// or a skipped tick cannot drift. Live delivers object changes; elapsed time
// since a known timestamp needs no server round trip, so the cells advance with
// Live on or off. The Go twin is internal/web/clock_cells.go; humanDuration and
// ageBucket must stay identical to duration.HumanDuration and elapsedAgeClass.

const DAY_SECONDS = 24 * 60 * 60;
const AGE_GRACE_SECONDS = 60;
const TICK_MS = 1000;
const CLOCK_SELECTOR = '[data-ro-t]';

// humanDuration is apimachinery's duration.HumanDuration over a millisecond
// difference (Go truncates toward zero at every unit; so does Math.trunc).
export function humanDuration(ms: number): string {
    const seconds = Math.trunc(ms / 1000);
    if (seconds < -1) {
        return '<invalid>';
    }
    if (seconds < 0) {
        return '0s';
    }
    if (seconds < 60 * 2) {
        return `${seconds}s`;
    }
    const minutes = Math.trunc(ms / 60000);
    if (minutes < 10) {
        const s = seconds % 60;
        return s === 0 ? `${minutes}m` : `${minutes}m${s}s`;
    }
    if (minutes < 60 * 3) {
        return `${minutes}m`;
    }
    const hours = Math.trunc(ms / 3600000);
    if (hours < 8) {
        const m = minutes % 60;
        return m === 0 ? `${hours}h` : `${hours}h${m}m`;
    }
    if (hours < 48) {
        return `${hours}h`;
    }
    if (hours < 24 * 8) {
        const h = hours % 24;
        const days = Math.trunc(hours / 24);
        return h === 0 ? `${days}d` : `${days}d${h}h`;
    }
    if (hours < 24 * 365 * 2) {
        return `${Math.trunc(hours / 24)}d`;
    }
    if (hours < 24 * 365 * 8) {
        const dy = Math.trunc(hours / 24) % 365;
        const years = Math.trunc(hours / 24 / 365);
        return dy === 0 ? `${years}y` : `${years}y${dy}d`;
    }
    return `${Math.trunc(hours / 24 / 365)}y`;
}

// ageBucket is the .age-* colour of an elapsed time as a fraction of 24h,
// after the grace (60s for Age and Created, none for the duration cells).
export function ageBucket(elapsedMs: number, graceSeconds: number): string {
    const seconds = Math.max(0, elapsedMs / 1000 - graceSeconds);
    const fraction = seconds / DAY_SECONDS;
    if (fraction < 0.1) return 'age-fresh';
    if (fraction < 0.35) return 'age-recent';
    if (fraction < 0.65) return 'age-day';
    if (fraction < 1) return 'age-week';
    return 'age-old';
}

interface ClockPaint {
    text: string | null;
    bucket: string | null;
}

// clockPaint resolves one element's text and colour for a format.
export function clockPaint(format: string, elapsedMs: number): ClockPaint | null {
    const age = humanDuration(elapsedMs);
    switch (format) {
        case 'age':
            return { text: age, bucket: ageBucket(elapsedMs, AGE_GRACE_SECONDS) };
        case 'created':
            return { text: null, bucket: ageBucket(elapsedMs, AGE_GRACE_SECONDS) };
        case 'ago':
            return { text: `${age} ago`, bucket: ageBucket(elapsedMs, 0) };
        case 'dur':
            return { text: age, bucket: null };
        case 'rago':
            return { text: `(${age} ago)`, bucket: null };
        case 'ev':
            return { text: age, bucket: ageBucket(elapsedMs, 0) };
        case 'evfirst':
            return { text: `(first ${age} ago)`, bucket: null };
    }
    return null;
}

// Server-minus-browser clock offset, estimated once per document from the
// ro-server-time meta the server rendered and the moment its first byte
// arrived. A document served from the HTTP cache or restored by history
// carries an old server time, so it estimates nothing (offset 0).
let offsetMs = 0;

export function estimateClockOffset(doc: Document = document): number {
    const meta = doc.querySelector<HTMLMetaElement>('meta[name="ro-server-time"]');
    const serverMs = Number(meta?.content);
    if (!meta || !Number.isFinite(serverMs) || serverMs <= 0) {
        return 0;
    }
    const entries =
        typeof performance.getEntriesByType === 'function'
            ? performance.getEntriesByType('navigation')
            : [];
    const nav = entries[0] as PerformanceNavigationTiming | undefined;
    if (!nav || nav.type === 'back_forward' || nav.transferSize === 0 || !nav.responseStart) {
        return 0;
    }
    return serverMs - (performance.timeOrigin + nav.responseStart);
}

export function setClockOffset(ms: number): void {
    offsetMs = ms;
}

export function clockNow(): number {
    return Date.now() + offsetMs;
}

function paintElement(el: HTMLElement, nowMs: number): boolean {
    const since = Number(el.dataset.roT);
    if (!Number.isFinite(since)) {
        return false;
    }
    const paint = clockPaint(el.dataset.roTf || '', nowMs - since * 1000);
    if (!paint) {
        return false;
    }
    let changed = false;
    if (paint.text !== null && el.textContent !== paint.text) {
        el.textContent = paint.text;
        changed = true;
    }
    if (paint.bucket !== null && !el.classList.contains(paint.bucket)) {
        for (const cls of Array.from(el.classList)) {
            if (cls.startsWith('age-')) el.classList.remove(cls);
        }
        el.classList.add(paint.bucket);
        changed = true;
    }
    return changed;
}

// paintClocks re-derives every clock element under root (connected or not: the
// virtualizer paints a detached slice before mounting it). Returns whether any
// text or colour changed.
export function paintClocks(root: ParentNode, nowMs: number = clockNow()): boolean {
    let changed = false;
    if (root instanceof HTMLElement && root.matches(CLOCK_SELECTOR)) {
        changed = paintElement(root, nowMs) || changed;
    }
    root.querySelectorAll<HTMLElement>(CLOCK_SELECTOR).forEach((el) => {
        changed = paintElement(el, nowMs) || changed;
    });
    return changed;
}

let timer: ReturnType<typeof setInterval> | null = null;

function tick(): void {
    paintClocks(document);
}

function startTimer(): void {
    if (timer === null) {
        timer = setInterval(tick, TICK_MS);
    }
}

function stopTimer(): void {
    if (timer !== null) {
        clearInterval(timer);
        timer = null;
    }
}

function onVisibilityChange(): void {
    if (document.hidden) {
        stopTimer();
        return;
    }
    tick();
    startTimer();
}

let started = false;

// startClockCells is idempotent: repeated init keeps one timer and one
// visibility listener. A hidden document starts paused.
export function startClockCells(): void {
    if (!started) {
        started = true;
        setClockOffset(estimateClockOffset());
        document.addEventListener('visibilitychange', onVisibilityChange);
    }
    tick();
    if (!document.hidden) {
        startTimer();
    }
}

// Test seam: forget the started state and the timer.
export function resetClockCellsForTest(): void {
    stopTimer();
    if (started) {
        document.removeEventListener('visibilitychange', onVisibilityChange);
    }
    started = false;
    offsetMs = 0;
}
