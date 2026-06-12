// live.ts -- Live mode (client half), migrated from legacy.js.
// 'Live' is the 6th refresh-dropdown mode: instead of polling, the client opens
// the read-only `GET …/{plural}/_stream` SSE endpoint and morphs every pushed
// `_table` fragment through the SAME ro-morph pipeline the polling ticks ride
// (the extension still lives in legacy.js; htmx.swap routes the 'morph' style
// through it and dispatches htmx:afterSwap on the container, so the standard
// post-swap repairs run untouched). The event carries a roLivePush marker so a
// push never triggers the param-change reopen.
//
// Native EventSource is deliberately NOT used: it cannot observe the non-200
// connect statuses the wire contract assigns meaning to (204 watch-less, 429
// stream cap), and its auto-reconnect fights the close-reason taxonomy. A
// streaming fetch + a ~20-line SSE line parser gives full control with no new
// dependency, and the strict CSP is untouched (fetch is connect-src).
//
// The PURE decisions -- the close-reason taxonomy (classifyStreamClose) and the
// morph-time discard gate (shouldDiscardPush) -- live in live-policy.ts
// (node-tested as a discriminated union); this module is the transport + state
// machine around them. The refresh tick chain (refresh.ts) is reused for the 5s
// fallback; the stale banner (stale.ts) for the honest degradations.
//
// The Go needle contract (internal/web/list_redesign_test.go,
// TestLiveWrongPageGateReadoutJSContract) pins the FORMS preserved here:
// 'liveStreamBase() !== liveState.streamPath' / liveDiscards / window.roLive.
// The body-swap teardown (liveTeardown + the liveState reset literals) is
// orchestrated in legacy.js's htmx:beforeSwap hook, which imports liveState +
// liveTeardown from this module so the needle literals stay inside that hook.

import { classifyStreamClose, shouldDiscardPush } from './live-policy.js';
import {
    containerListRequestsInFlight,
    pruneSettledListRequests,
    refreshMode,
    scheduleRefreshTick,
    userListRequestsInFlight,
} from './refresh.js';
import { markListStale } from './stale.js';

// The htmx surface this module uses (the vendored ro-morph extension resolves
// the 'morph' swap style itself). Typed minimally; htmx is a classic-script
// global loaded before this bundle.
interface Htmx {
    swap(
        target: Element,
        content: string,
        swapSpec: { swapStyle: string },
        swapOptions: { contextElement: Element; eventInfo: { target: Element; roLivePush: true } },
    ): void;
}
function getHtmx(): Htmx | undefined {
    return (window as unknown as { htmx?: Htmx }).htmx;
}

type LiveStatus = 'idle' | 'connecting' | 'open' | 'fallback' | 'hidden';

// liveState is the single source of truth for the stream lifecycle. EXPORTED so
// legacy.js's htmx:beforeSwap body-swap hook can reset it inline (the
// wrong-page-gate needle contract requires the reset literals to live in that
// hook). The AbortController doubles as the supersession token: every async
// resumption checks `liveState.abort === ctrl` and goes inert when a newer
// stream (or a teardown) replaced it.
export const liveState: {
    status: LiveStatus;
    abort: AbortController | null;
    gen: string;
    streamPath: string;
} = {
    status: 'idle', // 'idle' | 'connecting' | 'open' | 'fallback' | 'hidden'
    abort: null, // AbortController of the current stream fetch
    gen: '', // the minted generation every frame must echo (string compare)
    streamPath: '', // the stream URL sans ?g= -- the page/params identity
};
let liveGenSeq = 0;
// liveDiscards counts ro-table frames DISCARDED at morph time (stale
// generation, wrong page identity, in-flight `_table` request). Exposed via the
// window.roLive debug seam so the e2e suite can await "the push arrived AND was
// discarded" deterministically.
let liveDiscards = 0;
// The Live FALLBACK poll cadence (seconds): 0 while a stream rides (or Live is
// off), 5 while degraded to polling. refresh.ts's effectivePollSeconds reads it
// through liveFallbackSeconds().
let liveFallbackSecs = 0;

// liveFallbackSeconds() -- the cross-module getter refresh.ts folds into the
// effective poll cadence.
export function liveFallbackSeconds(): number {
    return liveFallbackSecs;
}

// liveSupported: can THIS page stream? The v2 single-type container must be
// present (data-live-url="location") and the server-rendered Live option must
// not be disabled (Live is unsupported on multi-type/multi-cluster pages). Server truth drives the client.
function liveSupported(): boolean {
    const content = document.getElementById('resource-list-content') as HTMLElement | null;
    if (content?.dataset.liveUrl !== 'location') {
        return false;
    }
    const option = document.querySelector(
        '[data-ro-action="set-refresh"][data-ro-interval="Live"]',
    ) as HTMLButtonElement | null;
    return !!option && !option.disabled;
}

// liveStreamBase derives the stream URL from the LIVE document URL at open time:
// path + "/_stream" + the RAW query -- raw string concat, never a
// URLSearchParams round-trip, so an `f` chip's wire-significant raw OR-commas
// survive byte-exactly.
function liveStreamBase(): string {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, '')}/_stream${u.search}`;
}

// liveTeardown aborts the current stream fetch (if any) and zeroes the fallback
// cadence -- a torn-down stream is no longer degraded-to-polling, so the shared
// tick chain must not keep the 5s fallback armed on its behalf. Every caller
// that wants a fallback cadence (liveEngageFallback) sets it AFTER tearing down,
// so this zero is always the correct floor. EXPORTED for the body-swap hook in
// legacy.js, where it resets the live state for the OLD page before the new
// page's init reopens (the body hook's `liveState.status = 'idle'` /
// `liveState.streamPath = ''` literals stay inline there per the wrong-page-gate
// needle contract; this zero rides along so the hook need not touch the private
// liveFallbackSecs).
export function liveTeardown(): void {
    const ctrl = liveState.abort;
    liveState.abort = null;
    liveFallbackSecs = 0;
    if (ctrl) {
        try {
            ctrl.abort();
        } catch {
            // already settled -- nothing to abort
        }
    }
}

// liveEngageFallback degrades Live to 5s polling: silently (204/429/connect
// failure) or with the stale banner (terminal, transport drop). The banner
// rides the SAME markListStale machinery every stale episode uses. Without a
// list container the cadence stays 0: there is nothing to poll.
function liveEngageFallback(banner: boolean): void {
    liveTeardown();
    liveState.status = 'fallback';
    liveFallbackSecs = document.getElementById('resource-list-content') ? 5 : 0;
    scheduleRefreshTick();
    if (banner) {
        markListStale();
    }
}

// liveOpen tears down any current stream and opens a fresh one against `base`
// (the stream URL sans generation). An empty base means "this page cannot
// stream": silent polling fallback. Minting the generation HERE -- one per open
// -- is what makes the morph-time echo check sufficient.
function liveOpen(base: string): void {
    liveTeardown();
    liveFallbackSecs = 0;
    liveState.streamPath = base;
    if (!base) {
        liveEngageFallback(false);
        return;
    }
    liveState.status = 'connecting';
    liveGenSeq += 1;
    liveState.gen = `${Date.now().toString(36)}.${liveGenSeq}`;
    const ctrl = new AbortController();
    liveState.abort = ctrl;
    const url = `${base + (base.indexOf('?') === -1 ? '?' : '&')}g=${encodeURIComponent(liveState.gen)}`;
    scheduleRefreshTick(); // effective cadence is 0 now -> the poll chain disarms
    void liveConnect(url, ctrl);
}

// liveConnect is the transport core: a streaming fetch + an SSE line parser.
// Frames are `event: <name>` + `data: <one JSON line>` + a blank line. Every
// await resumption re-checks the supersession token; all exits funnel into the
// taxonomy (classifyStreamClose).
async function liveConnect(url: string, ctrl: AbortController): Promise<void> {
    let res: Response;
    try {
        res = await fetch(url, { signal: ctrl.signal });
    } catch {
        applyClose({ superseded: liveState.abort !== ctrl, cause: 'connect-error' });
        return;
    }
    if (liveState.abort !== ctrl) {
        return;
    }
    if (res.status !== 200 || !res.body) {
        // 204 watch-less / 429 cap / anything unexpected: silent 5s polling.
        applyClose({ superseded: false, cause: 'bad-status' });
        return;
    }
    liveState.status = 'open';
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffered = '';
    let eventName = '';
    let dataText = '';
    try {
        for (;;) {
            const chunk = await reader.read();
            if (liveState.abort !== ctrl) {
                return; // torn down while awaiting -- go inert
            }
            if (chunk.done) {
                break;
            }
            buffered += decoder.decode(chunk.value, { stream: true });
            let nl = buffered.indexOf('\n');
            while (nl !== -1) {
                const line = buffered.slice(0, nl).replace(/\r$/, '');
                buffered = buffered.slice(nl + 1);
                if (line === '') {
                    const ended = liveHandleFrame(eventName, dataText, ctrl);
                    eventName = '';
                    dataText = '';
                    if (ended || liveState.abort !== ctrl) {
                        return; // terminal handled (or superseded mid-frame)
                    }
                } else if (line.indexOf('event:') === 0) {
                    eventName = line.slice(6).trim();
                } else if (line.indexOf('data:') === 0) {
                    const piece = line.slice(5).replace(/^ /, '');
                    dataText = dataText === '' ? piece : `${dataText}\n${piece}`;
                }
                nl = buffered.indexOf('\n');
            }
        }
    } catch {
        applyClose({ superseded: liveState.abort !== ctrl, cause: 'read-error' });
        return;
    }
    if (liveState.abort !== ctrl) {
        return;
    }
    // The server closed without a terminal frame (its graceful paths always send
    // one): treat it like a terminal -- banner + 5s polling.
    applyClose({ superseded: false, cause: 'eof' });
}

// applyClose maps a close fact to its action via the pure taxonomy. 'ignore'
// means a superseded stream's close surfaced -- do nothing.
function applyClose(facts: Parameters<typeof classifyStreamClose>[0]): void {
    const action = classifyStreamClose(facts);
    if (action.kind === 'ignore') {
        return;
    }
    liveEngageFallback(action.banner);
}

// liveHandleFrame dispatches one parsed SSE frame. Returns true when the stream
// must stop reading (a terminal was handled). THE morph-time gates live here:
// the generation echo, the page identity, and the in-flight `_table` check all
// run at dispatch -- which IS morph time, synchronously before htmx.swap -- so
// a stale or racing push is dropped whole (shouldDiscardPush), never queued.
function liveHandleFrame(name: string, text: string, ctrl: AbortController): boolean {
    if (liveState.abort !== ctrl || text === '') {
        return false;
    }
    let payload: { g?: unknown; html?: unknown } | null = null;
    try {
        payload = JSON.parse(text);
    } catch {
        return false; // malformed frame -> skipped (the next push is a full snapshot)
    }
    if (!payload || typeof payload !== 'object') {
        return false;
    }
    if (name === 'ro-terminal') {
        // idle / auth / watch-failed / shutdown: close WITHOUT reconnecting.
        applyClose({ superseded: false, cause: 'terminal-frame' });
        return true;
    }
    if (name !== 'ro-table') {
        return false;
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    // The morph-time discard gate, as the ordered taxonomy of
    // live-policy.ts (node-tested): stale generation -> wrong page -> a _table
    // request in flight. The WRONG-PAGE fact is computed here with its literal
    // form, `liveStreamBase() !== liveState.streamPath`: it is the independent
    // morph-time layer that backs the structural body-swap teardown (a boosted
    // body swap pushes the new URL BEFORE liveApply reconciles, so an old
    // stream's push must not morph the old resource's table into the new
    // container), and the Go wrong-page-gate contract pins exactly that form in
    // the bundle (internal/web/list_redesign_test.go).
    const reason = shouldDiscardPush({
        frameGeneration: String(payload.g),
        currentGeneration: liveState.gen,
        liveStreamBase: liveStreamBase(),
        openedStreamBase: liveState.streamPath,
        requestInFlight:
            userListRequestsInFlight.size > 0 || containerListRequestsInFlight.size > 0,
    });
    if (reason !== 'none' || liveStreamBase() !== liveState.streamPath) {
        // STALE GENERATION / WRONG PAGE / a _table request in flight -> dropped
        // whole at morph time, never deferred (every push is a full snapshot).
        liveDiscards += 1;
        return false;
    }
    liveMorph(String(payload.html));
    return false;
}

// liveMorph swaps one pushed fragment into the list container through the htmx
// swap pipeline with the 'morph' style: htmx resolves the container's
// hx-ext="ro-morph" extension (which runs the EXACT tick path) and dispatches
// htmx:afterSwap on the container, so the standard post-swap pipeline runs
// through the existing listener. eventInfo carries target (so isListRefreshEvent
// matches) and the roLivePush marker (so the reopen hook skips pushes).
function liveMorph(html: string): void {
    const content = document.getElementById('resource-list-content');
    const htmx = getHtmx();
    if (!content || !htmx || typeof htmx.swap !== 'function') {
        return;
    }
    htmx.swap(
        content,
        html,
        { swapStyle: 'morph' },
        {
            contextElement: content,
            eventInfo: { target: content, roLivePush: true },
        },
    );
}

// liveOnListSwap is the param-change reopen (called from the container afterSwap
// pipeline in legacy.js): while a stream rides, ANY request swap of the
// container is a query/cookie change, so the stream reopens against the new
// state under a fresh generation. The new query is taken from the REQUEST path
// (byte-exact) rather than location. In FALLBACK nothing reopens.
export function liveOnListSwap(event: Event): void {
    const detail = (event as CustomEvent).detail;
    if (detail?.roLivePush) {
        return; // a push is not a param change
    }
    if (liveState.status !== 'open' && liveState.status !== 'connecting') {
        return;
    }
    let base = liveStreamBase();
    const pathInfo = detail?.pathInfo;
    const requestPath = pathInfo && (pathInfo.finalRequestPath || pathInfo.requestPath);
    if (requestPath && requestPath.indexOf('/_table') !== -1) {
        base = requestPath.replace('/_table', '/_stream');
    }
    liveOpen(base);
}

// liveApply is the init-time (and dropdown-pick) reconciliation: open, keep,
// degrade, or tear down the stream per the persisted mode and THIS page's
// capability. Idempotent across the htmx:load re-inits every swap fires.
// `force` (the explicit dropdown pick) always reopens.
export function liveApply(force?: boolean): void {
    if (refreshMode() !== 'Live') {
        if (liveState.status !== 'idle') {
            liveTeardown();
            liveState.status = 'idle';
            liveState.streamPath = '';
            liveFallbackSecs = 0;
        }
        return;
    }
    const base = liveSupported() ? liveStreamBase() : '';
    if (!force && base === liveState.streamPath && liveState.status !== 'idle') {
        return; // same page + params: the standing state holds
    }
    liveOpen(base);
}

// Visibility close/reopen: hiding the tab closes a riding stream;
// returning reopens ONLY after such a hidden-close. A terminal/429/204 fallback
// and user-selected polling never reopen here.
document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
        if (liveState.status === 'open' || liveState.status === 'connecting') {
            liveTeardown();
            liveState.status = 'hidden';
        }
        return;
    }
    if (liveState.status === 'hidden' && refreshMode() === 'Live') {
        liveOpen(liveSupported() ? liveStreamBase() : '');
    }
});

// The deliberate external seam (e2e / console), the roVirtual/roRowState
// pattern: morph-time discard observability. The specs poll discards() to prove
// a held push ARRIVED and was dropped before asserting the view stayed
// unchanged.
(window as unknown as { roLive: { discards(): number } }).roLive = {
    discards() {
        return liveDiscards;
    },
};
