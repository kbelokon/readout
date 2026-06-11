// live-policy.test.ts -- node:test for the PURE refresh + Live decision core
// (live-policy.ts). The cadence math, the §8.3 backoff, the D19 stream-close
// taxonomy, and the morph-time discard gate are the load-bearing protocol
// decisions the e2e suite (live.spec.ts, refresh.spec.ts) exercises through the
// DOM; pinning every branch here (no DOM, no fetch) catches a regression at the
// unit boundary before it reaches a frame.
//
// Run: `node --test 'internal/assets/src/js/**/*.test.ts'`.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
    classifyStreamClose,
    effectivePollSeconds,
    nextFailureStage,
    type PushDiscardFacts,
    refreshDelaySeconds,
    type StreamCloseFacts,
    shouldDiscardPush,
} from './live-policy.ts';

// --- effectivePollSeconds ---------------------------------------------------

test('a chosen numeric interval wins over everything', () => {
    assert.equal(effectivePollSeconds('30', 30, 0), 30);
    assert.equal(effectivePollSeconds('5', 5, 0), 5);
    // Even nominally in Live (it never happens together, but the interval wins).
    assert.equal(effectivePollSeconds('Live', 10, 5), 10);
});

test('Off with no interval polls never', () => {
    assert.equal(effectivePollSeconds('Off', 0, 0), 0);
    assert.equal(effectivePollSeconds('', 0, 0), 0);
});

test('Live with a riding stream (fallback 0) self-disarms the poll chain', () => {
    assert.equal(effectivePollSeconds('Live', 0, 0), 0);
});

test('Live degraded to polling uses the 5s fallback cadence', () => {
    assert.equal(effectivePollSeconds('Live', 0, 5), 5);
});

// --- refreshDelaySeconds (SPEC §8.3 backoff) --------------------------------

test('a non-positive cadence arms no timer', () => {
    assert.equal(refreshDelaySeconds(0, 0), 0);
    assert.equal(refreshDelaySeconds(-1, 3), 0);
    assert.equal(refreshDelaySeconds(0, 2), 0);
});

test('healthy and the first failure both wait 1x', () => {
    assert.equal(refreshDelaySeconds(5, 0), 5); // stage 0 healthy
    assert.equal(refreshDelaySeconds(5, 1), 5); // stage 1: 1x retry
});

test('the second failure doubles, the third (terminal) quadruples', () => {
    assert.equal(refreshDelaySeconds(5, 2), 10); // 2x
    assert.equal(refreshDelaySeconds(5, 3), 20); // 4x
});

test('the backoff wait is capped at 60s', () => {
    // 30s base: 2x = 60 (at cap), 4x = 120 -> clamped to 60.
    assert.equal(refreshDelaySeconds(30, 2), 60);
    assert.equal(refreshDelaySeconds(30, 3), 60);
    // 60s base: even 1x is already the cap; 4x clamps.
    assert.equal(refreshDelaySeconds(60, 3), 60);
});

test('nextFailureStage escalates 0->1->2->3 and clamps at 3', () => {
    assert.equal(nextFailureStage(0), 1);
    assert.equal(nextFailureStage(1), 2);
    assert.equal(nextFailureStage(2), 3);
    assert.equal(nextFailureStage(3), 3); // terminal stage stays
});

// --- classifyStreamClose (D19 taxonomy, discriminated union) ----------------

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
        assert.deepEqual(classifyStreamClose(close(cause, true)), { kind: 'ignore' });
    }
});

test('connect-time failures degrade to SILENT polling (no banner, not terminal)', () => {
    assert.deepEqual(classifyStreamClose(close('connect-error')), {
        kind: 'fallback',
        banner: false,
        terminal: false,
    });
    // 204 watch-less / 429 stream cap / anything unexpected all surface here.
    assert.deepEqual(classifyStreamClose(close('bad-status')), {
        kind: 'fallback',
        banner: false,
        terminal: false,
    });
});

test('a mid-stream read drop degrades WITH the banner, not terminal', () => {
    assert.deepEqual(classifyStreamClose(close('read-error')), {
        kind: 'fallback',
        banner: true,
        terminal: false,
    });
});

test('a terminal-less EOF degrades WITH the banner, not terminal', () => {
    assert.deepEqual(classifyStreamClose(close('eof')), {
        kind: 'fallback',
        banner: true,
        terminal: false,
    });
});

test('an explicit ro-terminal frame degrades WITH the banner AND is terminal', () => {
    assert.deepEqual(classifyStreamClose(close('terminal-frame')), {
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
        assert.equal(verdict.kind, 'fallback');
    }
});

// --- shouldDiscardPush (D19 morph-time gate) --------------------------------

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
    assert.equal(shouldDiscardPush(push()), 'none');
});

test('a stale generation is discarded FIRST (before page / in-flight)', () => {
    // Even with a wrong page AND a request in flight, the generation gate wins:
    // ordering is part of the contract (the cheapest, most decisive check).
    assert.equal(
        shouldDiscardPush(
            push({
                frameGeneration: 'g0',
                currentGeneration: 'g1',
                liveStreamBase: '/other/_stream',
                requestInFlight: true,
            }),
        ),
        'stale-generation',
    );
});

test('a current-generation frame against a changed page is wrong-page', () => {
    assert.equal(
        shouldDiscardPush(
            push({
                liveStreamBase: '/clusters/c/pods/_stream?f=status:Running',
                openedStreamBase: '/clusters/c/pods/_stream',
            }),
        ),
        'wrong-page',
    );
});

test('a fresh, same-page frame while a _table request is in flight is discarded', () => {
    assert.equal(shouldDiscardPush(push({ requestInFlight: true })), 'request-in-flight');
});

test('wrong-page is checked before in-flight', () => {
    assert.equal(
        shouldDiscardPush(
            push({
                liveStreamBase: '/other/_stream',
                requestInFlight: true,
            }),
        ),
        'wrong-page',
    );
});
