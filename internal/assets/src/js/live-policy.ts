// live-policy.ts -- the PURE decision core of the refresh + Live
// cluster, lifted out of the DOM/protocol modules (refresh.ts, live.ts) so the
// load-bearing protocol decisions are node-testable with no DOM, no fetch, no
// htmx. The DOM modules read state from the page and the wire; THIS module is
// the math/taxonomy those modules consult once they have the facts.
//
// node:test exercises every branch (live-policy.test.ts). Per the node-test
// contract this module carries ONLY erasable (type-only) imports -- it imports
// nothing at runtime, so `node --test` strips its types and runs it directly.
//
// Three decisions live here, each pinned by the refresh + Live protocol:
//   1. effectivePollSeconds -- the poll cadence the tick chain arms, folding the
//      chosen interval together with Live's degraded-to-polling 5s fallback.
//   2. refreshDelaySeconds -- the backoff wait until the NEXT tick:
//      1x -> 2x -> 4x of the cadence, capped 60s, reset on success.
//   3. classifyStreamClose -- the close-reason TAXONOMY as a discriminated
//      union: every way a stream connect/read can end maps to exactly one
//      action (silent polling, banner polling, or a terminal close), so the
//      transport core in live.ts is a thin switch over this verdict.
//   4. shouldDiscardPush -- the morph-time generation/page/in-flight discard
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
    if (effectiveSeconds <= 0) {
        return 0;
    }
    if (failureStage <= 1) {
        return effectiveSeconds;
    }
    const factor = failureStage === 2 ? 2 : 4;
    return Math.min(effectiveSeconds * factor, 60);
}

// nextFailureStage escalates the consecutive-failure counter one notch, clamped
// at 3 (the terminal 4x backoff stage). The first success resets it to 0
// elsewhere; this is the only escalation step.
export function nextFailureStage(stage: number): number {
    return Math.min(stage + 1, 3);
}

// --- 3. stream-close taxonomy ----------------------------------------------

// A StreamCloseAction is the VERDICT classifyStreamClose returns -- the single
// thing live.ts acts on when a connect or read ends. The discriminated union
// makes the three distinct degradations impossible to confuse:
//   - 'fallback': degrade Live to 5s polling. `banner` says whether the stale
//     banner reveals (honest degradations) or the polling is SILENT (the
//     watch-less 204 / stream-cap 429 / opaque connect failures the user need
//     not see -- if polling then fails too, the standard failure machinery
//     raises the honest banner itself). `terminal` records whether a server
//     `ro-terminal` frame drove it (idle/auth/watch-failed/shutdown) versus a
//     transport-level drop -- both banner, but the distinction is the taxonomy.
//   - 'ignore': the close belongs to a SUPERSEDED stream (our own abort / a
//     newer stream replaced this one) -- do nothing, the live stream owns the
//     state now.
export type StreamCloseAction =
    | { kind: 'fallback'; banner: boolean; terminal: boolean }
    | { kind: 'ignore' };

// The facts live.ts hands the classifier. `superseded` is the supersession
// check (this stream's AbortController is no longer the current one) -- when
// true every close is ignored. `cause` is WHAT happened, named by the wire:
//   - 'connect-error'  : the fetch() itself rejected (could not connect)
//   - 'bad-status'     : the response was not a streamable 200 (204 watch-less,
//                        429 stream cap, or anything unexpected)
//   - 'read-error'     : the body read threw mid-stream (transport drop)
//   - 'eof'            : the server closed the body WITHOUT a terminal frame
//   - 'terminal-frame' : a server `ro-terminal` frame was received
export interface StreamCloseFacts {
    superseded: boolean;
    cause: 'connect-error' | 'bad-status' | 'read-error' | 'eof' | 'terminal-frame';
}

// classifyStreamClose maps one close fact to its action. Supersession trumps
// everything (our own teardown surfaces as a connect/read error -- never act on
// it). Then the close-reason taxonomy: connect-time failures (the fetch reject, a
// non-200 status) degrade SILENTLY; mid/post-stream failures (a read drop, a
// terminal frame, a terminal-less EOF) degrade WITH the banner. Only the
// terminal frame is `terminal: true` -- the wire said so explicitly.
export function classifyStreamClose(facts: StreamCloseFacts): StreamCloseAction {
    if (facts.superseded) {
        return { kind: 'ignore' };
    }
    switch (facts.cause) {
        case 'connect-error':
        case 'bad-status':
            return { kind: 'fallback', banner: false, terminal: false };
        case 'read-error':
        case 'eof':
            return { kind: 'fallback', banner: true, terminal: false };
        case 'terminal-frame':
            return { kind: 'fallback', banner: true, terminal: true };
    }
}

// --- 4. morph-time push discard ---------------------------------------------

// A PushDiscardReason names WHY a pushed `_table` frame is dropped at morph
// time, or 'none' when it should morph. Every reason but 'none' increments the
// observability counter (window.roLive.discards) in live.ts. The frame is
// dropped WHOLE, never queued -- every push is a full snapshot, so the next one
// carries everything a dropped one did.
export type PushDiscardReason =
    | 'none' // morph it
    | 'stale-generation' // the frame's ?g= echo is not the current generation
    | 'wrong-page' // the live location no longer matches this stream's identity
    | 'request-in-flight'; // a user/container _table request would race this push

// The facts live.ts gathers at dispatch (which IS morph time -- synchronously
// before htmx.swap). All are derived without touching this module: the frame's
// echoed generation vs the minted one, the live stream identity vs the one this
// stream was opened against, and whether ANY _table request is unsettled.
export interface PushDiscardFacts {
    frameGeneration: string;
    currentGeneration: string;
    liveStreamBase: string;
    openedStreamBase: string;
    requestInFlight: boolean;
}

// shouldDiscardPush is the ordered gate, mirroring liveHandleFrame's checks:
// stale generation first (a frame from any superseded stream can never carry
// the current value), then page identity (a boosted body swap pushes the new
// URL before liveApply reconciles, so an old stream's push must not morph the
// old resource's table into the new container), then the in-flight race (a
// _table request in flight would render against an older state).
export function shouldDiscardPush(facts: PushDiscardFacts): PushDiscardReason {
    if (facts.frameGeneration !== facts.currentGeneration) {
        return 'stale-generation';
    }
    if (facts.liveStreamBase !== facts.openedStreamBase) {
        return 'wrong-page';
    }
    if (facts.requestInFlight) {
        return 'request-in-flight';
    }
    return 'none';
}
