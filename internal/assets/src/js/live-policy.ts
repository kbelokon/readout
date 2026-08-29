// live-policy.ts -- the PURE decision core of the refresh + Live
// cluster, lifted out of the DOM/protocol modules (refresh.ts, live.ts) so the
// load-bearing decisions are unit-testable with no DOM, no fetch, no htmx. The
// DOM modules read state from the page and the wire; this module handles the
// small calculations and identity predicate they consult once they have facts.
//
// Vitest exercises every branch (live-policy.test.ts). This module deliberately
// carries only erasable types and no runtime dependencies.
//
// Three decisions live here, each pinned by the refresh + Live protocol:
//   1. effectivePollSeconds -- the poll cadence the tick chain arms, folding the
//      chosen interval together with Live's degraded-to-polling 5s fallback.
//   2. refreshDelaySeconds -- the backoff wait until the NEXT tick:
//      1x -> 2x -> 4x of the cadence, capped 60s, reset on success.
//   3. shouldDiscardPush -- the morph-time generation/page discard
//      gate: a Live frame is dropped whole, never deferred, when it is stale.

// --- 1. effective poll cadence ----------------------------------------------

// effectivePollSeconds is the cadence the shared tick chain actually arms. A
// chosen numeric interval wins outright; otherwise Live contributes its
// degraded-to-polling fallback (liveFallbackSecs: 0 while a stream rides or
// Live is off, 5 while degraded). Plain 'Live' with a riding stream therefore
// returns 0 -- enabling Live stops the polling timer; the chain self-disarms.
// `intervalSeconds` is the parsed numeric interval (0 for 'Off'/'Live'/junk).
export function effectivePollSeconds(
    mode: string,
    intervalSeconds: number,
    liveFallbackSeconds: number,
): number {
    if (intervalSeconds > 0) {
        return intervalSeconds;
    }
    return mode === 'Live' ? liveFallbackSeconds : 0;
}

// --- 2. failure backoff -----------------------------------------------------

// refreshDelaySeconds is the wait until the NEXT tick given the effective
// cadence and the consecutive-failure stage. Healthy (stage 0) and the first
// failure (stage 1) both wait the base cadence (1x); stage 2 doubles it, stage
// 3 (where it stays) quadruples it, the backoff wait capped at 60s. A
// non-positive cadence means "no timer" -> 0 (the chain disarms).
export function refreshDelaySeconds(effectiveSeconds: number, failureStage: number): number {
    const baseSeconds = Math.max(effectiveSeconds, 0);
    if (failureStage <= 1) {
        return baseSeconds;
    }
    const factor = failureStage === 2 ? 2 : 4;
    return Math.min(baseSeconds * factor, 60);
}

// nextFailureStage escalates the consecutive-failure counter one notch, clamped
// at 3 (the terminal 4x backoff stage). The first success resets it to 0
// elsewhere; this is the only escalation step.
export function nextFailureStage(stage: number): number {
    return Math.min(stage + 1, 3);
}

// --- 3. morph-time push discard ---------------------------------------------

// The facts live.ts gathers at dispatch (which IS morph time -- synchronously
// before htmx.swap). All are derived without touching this module: the frame's
// echoed generation vs the minted one and the live stream identity vs the one
// this stream was opened against. A current-list request synchronously aborts
// the stream before dispatch, so request ownership cannot race this gate.
export interface PushDiscardFacts {
    frameGeneration: string;
    currentGeneration: string;
    liveStreamBase: string;
    openedStreamBase: string;
}

// A boosted body swap changes the URL before liveApply reconciles. Reject a
// pushed snapshot unless both its generation and its current page identity
// still belong to this connection.
export function shouldDiscardPush(facts: PushDiscardFacts): boolean {
    return (
        facts.frameGeneration !== facts.currentGeneration ||
        facts.liveStreamBase !== facts.openedStreamBase
    );
}
