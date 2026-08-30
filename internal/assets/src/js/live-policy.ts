// live-policy.ts -- the PURE decision core of the Live cluster, lifted out of
// the DOM/protocol modules (live.ts, refresh.ts) so the load-bearing decisions
// are unit-testable with no DOM, no fetch, no htmx. The DOM modules read state
// from the page and the wire; this module handles the small calculations and
// identity predicate they consult once they have facts.
//
// Vitest exercises every branch (live-policy.test.ts). This module deliberately
// carries only erasable types and no runtime dependencies.
//
// Four decisions live here, each pinned by the Live transport protocol:
//   1. reconnectDelayMs -- how long the browser waits before re-opening a
//      dropped `_stream`, full jitter over a bounded ladder.
//   2. retryAfterMs -- the server-dictated wait a 429 admission reject carries.
//   3. shouldResetBackoff -- when a connection lived long enough to count as
//      healthy, so the next drop starts the ladder from the top again.

// --- 1. reconnect schedule --------------------------------------------------

// The reconnect ladder: attempt N waits somewhere in [0, ladder[N]). The rungs
// are deliberately short at the head (a pod rolling out drops every stream at
// once and the replacement answers immediately) and flat at 30s in the tail (a
// down control plane must not be hammered by every open tab).
export const RECONNECT_DELAY_LADDER_MS = [1000, 2000, 5000, 10_000, 30_000];

// reconnectDelayMs is the wait before reconnect attempt `attempt` (1-based).
// FULL jitter, not equal jitter: the whole [0, cap] range is drawn, because the
// herd this de-synchronizes is every browser tab watching a pod that just
// restarted -- spreading them across the entire window is the point. `random`
// is injected so the schedule is testable with a seeded generator.
export function reconnectDelayMs(attempt: number, random: () => number = Math.random): number {
    const rung = Number.isInteger(attempt) && attempt >= 1 ? attempt : 1;
    const cap = RECONNECT_DELAY_LADDER_MS[
        Math.min(rung, RECONNECT_DELAY_LADDER_MS.length) - 1
    ] as number;
    const roll = random();
    const fraction = Number.isFinite(roll) ? Math.min(Math.max(roll, 0), 1) : 1;
    return Math.round(cap * fraction);
}

// A Retry-After is clamped at both ends: a hostile or mistaken header can
// neither park a tab for an hour nor -- with `0`, or a date already in the past
// -- turn the reconnect into an unthrottled loop.
const RETRY_AFTER_MAX_MS = 300_000;
const RETRY_AFTER_MIN_MS = 1000;

// retryAfterMs parses an RFC 9110 Retry-After (delta-seconds or an HTTP-date)
// into a wait in milliseconds, or null when the header is absent or malformed
// (the caller then falls back to its own ladder). Only the integer spelling is
// accepted for delta-seconds -- a partial parse ("5junk") is malformed, not 5.
export function retryAfterMs(header: string | null, now: number = Date.now()): number | null {
    if (header === null) return null;
    const value = header.trim();
    if (value === '') return null;
    if (/^\d+$/.test(value)) {
        return clampRetryAfter(Number(value) * 1000);
    }
    // All three HTTP-date spellings begin with a weekday NAME. Requiring one
    // keeps Date.parse's permissive readings of numeric junk ("-5" as a year,
    // "5junk" as a day) out of the accepted set.
    if (!/^[a-z]{3}/i.test(value)) return null;
    const at = Date.parse(value);
    if (Number.isNaN(at)) return null;
    return clampRetryAfter(at - now);
}

function clampRetryAfter(ms: number): number {
    return Math.min(Math.max(ms, RETRY_AFTER_MIN_MS), RETRY_AFTER_MAX_MS);
}

// --- 2. backoff reset -------------------------------------------------------

// A connection counts as healthy once its committed snapshot has stood for this
// long. Shorter than the server's heartbeat budget would misread a stream that
// only ever delivers its first frame as healthy.
export const HEALTHY_CONTINUITY_MS = 30_000;

// shouldResetBackoff answers whether the connection that just dropped had held
// a committed snapshot long enough to restart the ladder at rung 1.
// `snapshotAt` is 0 when the connection never committed one.
export function shouldResetBackoff(snapshotAt: number, now: number): boolean {
    if (snapshotAt <= 0) return false;
    return now - snapshotAt >= HEALTHY_CONTINUITY_MS;
}
