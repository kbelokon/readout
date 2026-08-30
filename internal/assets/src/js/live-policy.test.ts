// live-policy.test.ts -- Vitest for the PURE Live decision core
// (live-policy.ts). The reconnect schedule, the server-dictated Retry-After
// wait, the backoff reset predicate, and the morph-time discard gate are the
// load-bearing decisions exercised through the DOM by the e2e suite; pinning
// them here catches a regression before it reaches a frame.
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import {
    HEALTHY_CONTINUITY_MS,
    type PushDiscardFacts,
    RECONNECT_DELAY_LADDER_MS,
    reconnectDelayMs,
    retryAfterMs,
    shouldDiscardPush,
    shouldResetBackoff,
} from './live-policy.js';

// A seeded random so the jitter draw is reproducible: a tiny LCG over [0,1).
function seededRandom(seed: number): () => number {
    let state = seed >>> 0;
    return () => {
        state = (state * 1664525 + 1013904223) >>> 0;
        return state / 0x1_0000_0000;
    };
}

// --- reconnectDelayMs (full jitter over the ladder) --------------------------

test('the ladder is the pinned 1s,2s,5s,10s,30s schedule', () => {
    expect(RECONNECT_DELAY_LADDER_MS).toStrictEqual([1000, 2000, 5000, 10_000, 30_000]);
});

test('each attempt draws full jitter over its rung and later attempts stay at 30s', () => {
    // random() === 1 is never produced by Math.random, so the cap is the
    // supremum; drive the extremes explicitly.
    const caps = [1, 2, 3, 4, 5, 6, 12].map((attempt) => reconnectDelayMs(attempt, () => 1));
    expect(caps).toStrictEqual([1000, 2000, 5000, 10_000, 30_000, 30_000, 30_000]);
    expect([1, 2, 5].map((attempt) => reconnectDelayMs(attempt, () => 0))).toStrictEqual([0, 0, 0]);
    expect(reconnectDelayMs(3, () => 0.5)).toBe(2500);
});

test('a seeded random keeps every draw inside its rung', () => {
    const random = seededRandom(20_260_830);
    for (let attempt = 1; attempt <= 8; attempt += 1) {
        const cap = RECONNECT_DELAY_LADDER_MS[
            Math.min(attempt, RECONNECT_DELAY_LADDER_MS.length) - 1
        ] as number;
        for (let draw = 0; draw < 25; draw += 1) {
            const delay = reconnectDelayMs(attempt, random);
            expect(delay).toBeGreaterThanOrEqual(0);
            expect(delay).toBeLessThanOrEqual(cap);
            expect(Number.isInteger(delay)).toBe(true);
        }
    }
});

test('the draw actually varies rather than pinning one value', () => {
    const random = seededRandom(7);
    const draws = new Set(Array.from({ length: 20 }, () => reconnectDelayMs(5, random)));
    expect(draws.size).toBeGreaterThan(1);
});

test.each([0, -3, Number.NaN, Number.POSITIVE_INFINITY, 1.9])(
    'a non-positive or non-integral attempt %j falls back to the first rung',
    (attempt) => {
        expect(reconnectDelayMs(attempt, () => 1)).toBe(1000);
    },
);

test.each([-0.5, 1.5, Number.NaN])('an out-of-range random draw %j clamps to the cap', (roll) => {
    expect(reconnectDelayMs(2, () => roll)).toBeLessThanOrEqual(2000);
    expect(reconnectDelayMs(2, () => roll)).toBeGreaterThanOrEqual(0);
});

// --- retryAfterMs (the server-dictated wait) ---------------------------------

test('delta-seconds Retry-After becomes milliseconds', () => {
    expect(retryAfterMs('10')).toBe(10_000);
    expect(retryAfterMs(' 10 ')).toBe(10_000);
    expect(retryAfterMs('0')).toBe(0);
});

test('an HTTP-date Retry-After is measured from now', () => {
    const now = Date.parse('2026-08-30T10:00:00Z');
    expect(retryAfterMs('Sun, 30 Aug 2026 10:00:30 GMT', now)).toBe(30_000);
    // A date already in the past is a zero wait, never a negative one.
    expect(retryAfterMs('Sun, 30 Aug 2026 09:59:00 GMT', now)).toBe(0);
});

test.each([null, '', 'soon', '-5', '5.5', '5junk'])(
    'an absent or unparsable Retry-After %j yields no server wait',
    (header) => {
        expect(retryAfterMs(header)).toBeNull();
    },
);

test('an absurd Retry-After is capped at the ladder maximum reconnect wait', () => {
    expect(retryAfterMs('86400')).toBe(300_000);
});

// --- shouldResetBackoff -----------------------------------------------------

test('the healthy-continuity window is 30s of committed snapshot', () => {
    expect(HEALTHY_CONTINUITY_MS).toBe(30_000);
});

test('a connection that held a snapshot for 30s resets the attempt counter', () => {
    const at = 1_000_000;
    expect(shouldResetBackoff(at, at + 30_000)).toBe(true);
    expect(shouldResetBackoff(at, at + 45_000)).toBe(true);
});

test('a short-lived or never-committed connection keeps escalating', () => {
    const at = 1_000_000;
    expect(shouldResetBackoff(at, at + 29_999)).toBe(false);
    expect(shouldResetBackoff(0, at)).toBe(false);
    // A clock that jumped backwards must not look like continuity.
    expect(shouldResetBackoff(at, at - 60_000)).toBe(false);
});

// --- shouldDiscardPush (morph-time gate) ------------------------------------

function push(over: Partial<PushDiscardFacts> = {}): PushDiscardFacts {
    return {
        frameGeneration: 'g1',
        currentGeneration: 'g1',
        liveStreamBase: '/clusters/c/pods/_stream',
        openedStreamBase: '/clusters/c/pods/_stream',
        ...over,
    };
}

test('a fresh same-page frame morphs without a discard', () => {
    expect(shouldDiscardPush(push())).toBe(false);
});

test('a stale generation is discarded', () => {
    expect(
        shouldDiscardPush(
            push({
                frameGeneration: 'g0',
                currentGeneration: 'g1',
            }),
        ),
    ).toBe(true);
});

test('a current-generation frame against a changed page is discarded', () => {
    expect(
        shouldDiscardPush(
            push({
                liveStreamBase: '/clusters/c/pods/_stream?f=status:Running',
                openedStreamBase: '/clusters/c/pods/_stream',
            }),
        ),
    ).toBe(true);
});
