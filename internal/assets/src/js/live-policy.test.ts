// live-policy.test.ts -- Vitest for the PURE refresh + Live decision core
// (live-policy.ts). The cadence math, failure backoff, and morph-time discard
// gate are the load-bearing decisions exercised through the DOM by the e2e
// suite; pinning them here catches a regression before it reaches a frame.
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import {
    effectivePollSeconds,
    nextFailureStage,
    type PushDiscardFacts,
    refreshDelaySeconds,
    shouldDiscardPush,
} from './live-policy.js';

// --- effectivePollSeconds ---------------------------------------------------

test('a chosen numeric interval wins over everything', () => {
    expect(effectivePollSeconds('30', 30, 0)).toBe(30);
    expect(effectivePollSeconds('5', 5, 0)).toBe(5);
    // Even nominally in Live (it never happens together, but the interval wins).
    expect(effectivePollSeconds('Live', 10, 5)).toBe(10);
});

test('Off with no interval polls never', () => {
    expect(effectivePollSeconds('Off', 0, 0)).toBe(0);
    expect(effectivePollSeconds('', 0, 0)).toBe(0);
    // A stale fallback value must not make a non-Live mode start polling.
    expect(effectivePollSeconds('Off', 0, 5)).toBe(0);
});

test('Live with a riding stream (fallback 0) self-disarms the poll chain', () => {
    expect(effectivePollSeconds('Live', 0, 0)).toBe(0);
});

test('Live degraded to polling uses the 5s fallback cadence', () => {
    expect(effectivePollSeconds('Live', 0, 5)).toBe(5);
});

// --- refreshDelaySeconds (failure backoff) ----------------------------------

test('a non-positive cadence arms no timer', () => {
    expect(refreshDelaySeconds(0, 0)).toBe(0);
    expect(refreshDelaySeconds(-1, 3)).toBe(0);
    expect(refreshDelaySeconds(0, 2)).toBe(0);
});

test('healthy and the first failure both wait 1x', () => {
    expect(refreshDelaySeconds(5, 0)).toBe(5); // stage 0 healthy
    expect(refreshDelaySeconds(5, 1)).toBe(5); // stage 1: 1x retry
});

test('the second failure doubles, the third (terminal) quadruples', () => {
    expect(refreshDelaySeconds(5, 2)).toBe(10); // 2x
    expect(refreshDelaySeconds(5, 3)).toBe(20); // 4x
});

test('the backoff wait is capped at 60s', () => {
    // 30s base: 2x = 60 (at cap), 4x = 120 -> clamped to 60.
    expect(refreshDelaySeconds(30, 2)).toBe(60);
    expect(refreshDelaySeconds(30, 3)).toBe(60);
    // 60s base: even 1x is already the cap; 4x clamps.
    expect(refreshDelaySeconds(60, 3)).toBe(60);
});

test('nextFailureStage escalates 0->1->2->3 and clamps at 3', () => {
    expect(nextFailureStage(0)).toBe(1);
    expect(nextFailureStage(1)).toBe(2);
    expect(nextFailureStage(2)).toBe(3);
    expect(nextFailureStage(3)).toBe(3); // terminal stage stays
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
