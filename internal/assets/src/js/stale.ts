// stale.ts -- the "this data is no longer current" surface. CLIENT-SIDE, and it
// never blanks the rows (data never disappears). There is no server-side
// last-good cache, so staleness is always something the browser observed.
//
// Three independent owners share one pre-rendered `.ro-banner.warn`:
//
//   - LIST STALE: a user-visible `_table` request failed. htmx does NOT swap on
//     error (htmx:responseError = a non-2xx reply, htmx:sendError = a transport
//     failure), so the existing rows stay exactly as they were; we dim them and
//     reveal the banner. The next successful morph clears it via clearListStale.
//   - LIVE STALE: the Live stream dropped. This owner is SPLIT in two halves --
//     markLiveStale marks the projection last-known IMMEDIATELY (the
//     `data-ro-stale` marker) but delays the dim and the banner by a three
//     second grace, so a reconnect that lands inside a pod rollout produces no
//     flicker. revealLiveStale ends the grace early, which is what the first
//     FAILED reconnect does. Only a committed full snapshot clears it.
//   - LIVE UNAVAILABLE: the server said this session will not stream again
//     (401/403, the `auth` terminal, a 204/404 gate). Terminal, so there is no
//     grace and no countdown: the banner swaps to its Unavailable copy, whose
//     action is Reload rather than Retry.
//
// Both banner copy variants are server-rendered (templates/errors.templ) and
// exactly one is unhidden here -- no copy lives in JS string literals.
//
// The Go needle contract (internal/web/states_redesign_test.go) pins the FORMS
// preserved here: ro-stale-banner / ro-stale-retry / resource-list-content /
// ro:refresh / the literal gate `id === 'resource-list-content'` / the
// `htmx:responseError.{0,200}isListRefreshEvent` gate / the combined stale-owner
// visibility assignment / NO `innerHTML = ''` (the data-never-disappears law).

// The one 1s ticker repainting "Retrying in Ns" while the recoverable banner is
// visible.
let staleCountdownId: number | null = null;
// Epoch ms of the next reconnect attempt (0 = none armed). The Live transport
// owns the schedule; the banner only paints what it publishes here.
let staleRetryAt = 0;
// The three warning owners are independent. An empty set is the natural
// fresh-page state; owner identity survives list morphs without mirroring
// runtime state into serialized DOM attributes.
const listStaleOwner = Symbol();
const liveStaleOwner = Symbol();
const liveUnavailableOwner = Symbol();
const staleOwners = new Set<symbol>();
// Semantic staleness is the SEPARATE, immediate half of the Live owner: the
// projection is last-known from the instant the stream drops, whatever the
// visual grace is still hiding.
let liveSemanticStale = false;
// The armed visual grace (undefined = none). Only markLiveStale arms it.
let liveGraceTimerId: number | undefined;

// How long a Live disconnect stays visually invisible. A pod rollout drops
// every stream at once and the replacement answers well inside this window, so
// the ordinary case must not flash a warning at the user.
export const LIVE_STALE_GRACE_MS = 3_000;

interface BannerParts {
    recoverable: HTMLElement | null;
    reload: HTMLElement | null;
    retry: HTMLElement | null;
    unavailable: HTMLElement | null;
}

function bannerElement(): HTMLElement | null {
    return document.querySelector('.ro-stale-banner') as HTMLElement | null;
}

// The banner is a closed server template. These four nodes are part of that
// component contract, just like the countdown span queried by its repaint path.
function bannerParts(banner: HTMLElement): BannerParts {
    return {
        recoverable: banner.querySelector('.bn-body:not(.ro-stale-unavailable)'),
        reload: banner.querySelector('.ro-stale-reload'),
        retry: banner.querySelector('.ro-stale-retry'),
        unavailable: banner.querySelector('.bn-body.ro-stale-unavailable'),
    };
}

// Exactly one copy variant and exactly one action are visible. Each node is
// optional so a partially rendered banner degrades instead of throwing.
function paintBannerVariant(banner: HTMLElement, unavailable: boolean): void {
    const parts = bannerParts(banner);
    if (parts.recoverable) parts.recoverable.hidden = unavailable;
    if (parts.unavailable) parts.unavailable.hidden = !unavailable;
    if (parts.retry) parts.retry.hidden = unavailable;
    if (parts.reload) parts.reload.hidden = !unavailable;
}

function stopStaleCountdown(): void {
    if (staleCountdownId !== null) {
        window.clearInterval(staleCountdownId);
        staleCountdownId = null;
    }
}

function startStaleCountdown(): void {
    if (staleCountdownId === null) {
        staleCountdownId = window.setInterval(updateStaleCountdown, 1000);
    }
    updateStaleCountdown();
}

function clearLiveGrace(): void {
    window.clearTimeout(liveGraceTimerId);
    liveGraceTimerId = undefined;
}

function paintStaleState(): void {
    const listStale = staleOwners.has(listStaleOwner) || staleOwners.has(liveStaleOwner);
    const liveUnavailable = staleOwners.has(liveUnavailableOwner);
    const content = document.getElementById('resource-list-content');
    if (content) {
        content.classList.toggle('ro-stale', listStale || liveUnavailable);
        // The semantic marker is what a test (or a future consumer) reads to
        // learn the rows are last-known, independent of the visual grace.
        if (liveSemanticStale) content.dataset.roStale = 'true';
        else delete content.dataset.roStale;
    }
    const banner = bannerElement();
    if (!banner) {
        stopStaleCountdown();
        return;
    }
    paintBannerVariant(banner, liveUnavailable);
    banner.hidden = !(listStale || liveUnavailable);
    // The terminal variant is visible but has no retry to count down to, so its
    // ticker must never be installed -- otherwise a 1s interval would repaint
    // the hidden recoverable copy forever.
    if (banner.hidden || liveUnavailable) stopStaleCountdown();
    else startStaleCountdown();
}

// noteStaleRetryAt publishes the armed reconnect time (0 clears it) and
// repaints, so the banner's countdown always aims at the real next attempt.
export function noteStaleRetryAt(atMs: number): void {
    staleRetryAt = atMs;
    updateStaleCountdown();
}

// updateStaleCountdown paints seconds-to-next-retry into the banner's
// [data-stale-countdown] span. The span is re-queried on every paint. With no
// retry armed (Live off, or a terminal Unavailable state) the shipped "…"
// placeholder is restored.
export function updateStaleCountdown(): void {
    const banner = bannerElement();
    if (!banner) return;
    const span = banner.querySelector('[data-stale-countdown]');
    if (!span) {
        return;
    }
    const nextAt = staleRetryAt;
    if (!nextAt) {
        span.textContent = '…';
        return;
    }
    const remaining = Math.max(0, Math.ceil((nextAt - Date.now()) / 1000));
    span.textContent = `${remaining}s`;
}

// True when the htmx event belongs to a request that lands in the live
// resource-list region: issued BY #resource-list-content (the retry /
// programmatic re-fetch) or TARGETING it (a user sort/filter partial). Guards
// so an unrelated boosted navigation error never dims the table.
export function isListRefreshEvent(event: Event): boolean {
    const detail = (event as CustomEvent).detail;
    if (!detail) {
        return false;
    }
    const elt = detail.elt;
    if (elt && elt.id === 'resource-list-content') {
        return true;
    }
    const target = detail.target;
    return !!target && target.id === 'resource-list-content';
}

export function markListStale(): void {
    staleOwners.add(listStaleOwner);
    // Live countdown for the banner's "Retrying in Ns" (the data-stale-countdown
    // hook). The immediate paint lands the right number before the ticker's
    // first 1s beat.
    paintStaleState();
}

// markLiveStale is the disconnect entry point: semantic staleness lands NOW,
// the visible dim + banner only after the grace. Re-marking an already-stale
// projection must not restart the grace, or a retry loop would keep the warning
// permanently three seconds away.
export function markLiveStale(): void {
    liveSemanticStale = true;
    if (!staleOwners.has(liveStaleOwner) && liveGraceTimerId === undefined) {
        liveGraceTimerId = window.setTimeout(revealLiveStale, LIVE_STALE_GRACE_MS);
    }
    paintStaleState();
}

// pauseLiveStaleGrace disarms an UNELAPSED grace when the transport parks
// deliberately (a user request takes over, the tab hides, the browser goes
// offline). Nothing failed and nothing is retrying, so the delayed warning must
// not land on top of rows the user's own request just refreshed. The next real
// disconnect re-arms it through markLiveStale; an already-revealed warning is
// left alone, because that one was earned by a failed attempt.
export function pauseLiveStaleGrace(): void {
    clearLiveGrace();
}

// revealLiveStale ends the grace early -- the first FAILED reconnect proves the
// drop is not a rollout blip.
export function revealLiveStale(): void {
    clearLiveGrace();
    staleOwners.add(liveStaleOwner);
    paintStaleState();
}

// markLiveUnavailable is terminal: no retry is coming, so there is no grace and
// no countdown, and the banner shows its Reload copy instead of Retry.
export function markLiveUnavailable(): void {
    clearLiveGrace();
    liveSemanticStale = true;
    staleOwners.add(liveUnavailableOwner);
    paintStaleState();
}

// clearLiveStale retires the WHOLE Live-owned warning -- semantic marker, armed
// grace, dim, and the Unavailable copy. Only a committed full snapshot (or the
// user turning Live off) earns it; an ordinary list swap does not.
export function clearLiveStale(): void {
    clearLiveGrace();
    liveSemanticStale = false;
    staleOwners.delete(liveStaleOwner);
    staleOwners.delete(liveUnavailableOwner);
    paintStaleState();
}

export function clearListStale(): void {
    staleOwners.delete(listStaleOwner);
    paintStaleState();
}

// A non-2xx reply to the list GET: keep the rows (htmx does not swap on
// error), dim them, reveal the stale banner. Nothing is retried on a schedule
// -- the user's Refresh button (or Live's own reconnect) is the next attempt.
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        markListStale();
    }
});
// A transport failure on the list GET: same stale treatment.
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
        markListStale();
    }
});
