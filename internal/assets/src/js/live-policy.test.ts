// live-policy.test.ts -- Vitest for the PURE refresh + Live decision core
// (live-policy.ts). The cadence math, the failure backoff, the Live stream-close
// taxonomy, and the morph-time discard gate are the load-bearing protocol
// decisions the e2e suite (live.spec.ts, refresh.spec.ts) exercises through the
// DOM; pinning every branch here (no DOM, no fetch) catches a regression at the
// unit boundary before it reaches a frame.
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import {
    classifyStreamClose,
    effectivePollSeconds,
    nextFailureStage,
    type PushDiscardFacts,
    refreshDelaySeconds,
    type StreamCloseFacts,
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

// --- classifyStreamClose (close-reason taxonomy, discriminated union) -------

function close(cause: StreamCloseFacts['cause'], superseded = false): StreamCloseFacts {
    return { superseded, cause };
}

test('a superseded close is ignored regardless of cause', () => {
    for (const cause of [
        'connect-error',
        'bad-status',
        'read-error',
        'eof',
        'terminal-frame',
    ] as const) {
        expect(classifyStreamClose(close(cause, true))).toStrictEqual({ kind: 'ignore' });
    }
});

test('connect-time failures degrade to SILENT polling (no banner, not terminal)', () => {
    expect(classifyStreamClose(close('connect-error'))).toStrictEqual({
        kind: 'fallback',
        banner: false,
        terminal: false,
    });
    // 204 watch-less / 429 stream cap / anything unexpected all surface here.
    expect(classifyStreamClose(close('bad-status'))).toStrictEqual({
        kind: 'fallback',
        banner: false,
        terminal: false,
    });
});

test('a mid-stream read drop degrades WITH the banner, not terminal', () => {
    expect(classifyStreamClose(close('read-error'))).toStrictEqual({
        kind: 'fallback',
        banner: true,
        terminal: false,
    });
});

test('a terminal-less EOF degrades WITH the banner, not terminal', () => {
    expect(classifyStreamClose(close('eof'))).toStrictEqual({
        kind: 'fallback',
        banner: true,
        terminal: false,
    });
});

test('an explicit ro-terminal frame degrades WITH the banner AND is terminal', () => {
    expect(classifyStreamClose(close('terminal-frame'))).toStrictEqual({
        kind: 'fallback',
        banner: true,
        terminal: true,
    });
});

test('the close verdict is always a fallback or an ignore (the union is total)', () => {
    for (const cause of [
        'connect-error',
        'bad-status',
        'read-error',
        'eof',
        'terminal-frame',
    ] as const) {
        const verdict = classifyStreamClose(close(cause));
        expect(verdict.kind).toBe('fallback');
    }
});

// --- shouldDiscardPush (morph-time gate) ------------------------------------

function push(over: Partial<PushDiscardFacts> = {}): PushDiscardFacts {
    return {
        frameGeneration: 'g1',
        currentGeneration: 'g1',
        liveStreamBase: '/clusters/c/pods/_stream',
        openedStreamBase: '/clusters/c/pods/_stream',
        requestInFlight: false,
        ...over,
    };
}

test('a fresh, same-page, idle-request frame morphs (no discard)', () => {
    expect(shouldDiscardPush(push())).toBe('none');
});

test('a stale generation is discarded FIRST (before page / in-flight)', () => {
    // Even with a wrong page AND a request in flight, the generation gate wins:
    // ordering is part of the contract (the cheapest, most decisive check).
    expect(
        shouldDiscardPush(
            push({
                frameGeneration: 'g0',
                currentGeneration: 'g1',
                liveStreamBase: '/other/_stream',
                requestInFlight: true,
            }),
        ),
    ).toBe('stale-generation');
});

test('a current-generation frame against a changed page is wrong-page', () => {
    expect(
        shouldDiscardPush(
            push({
                liveStreamBase: '/clusters/c/pods/_stream?f=status:Running',
                openedStreamBase: '/clusters/c/pods/_stream',
            }),
        ),
    ).toBe('wrong-page');
});

test('a fresh, same-page frame while a _table request is in flight is discarded', () => {
    expect(shouldDiscardPush(push({ requestInFlight: true }))).toBe('request-in-flight');
});

test('wrong-page is checked before in-flight', () => {
    expect(
        shouldDiscardPush(
            push({
                liveStreamBase: '/other/_stream',
                requestInFlight: true,
            }),
        ),
    ).toBe('wrong-page');
});
