// stale.ts -- stale data (auto-refresh failure) handling, migrated from
// legacy.js. CLIENT-SIDE, never blanks the rows (data never disappears). There is no server-side
// last-good cache: "stale" is purely the AUTO-REFRESH failure case. When the
// #resource-list-content morph-refresh request errors (htmx:responseError = a
// non-2xx reply, htmx:sendError = a transport failure), htmx does NOT swap on
// error, so the existing rows stay exactly as they were. We mark the content
// stale (a dim class) and reveal the pre-rendered hidden `.ro-banner.warn` so
// the user knows the data is last-known, not current. On the next successful
// refresh the morph swaps fresh rows and the afterSwap pipeline (orchestrated
// in legacy.js) clears the stale state via clearListStale. Pure DOM writes ->
// CSP-clean.
//
// The Go needle contract (internal/web/states_redesign_test.go) pins the FORMS
// preserved here: ro-stale-banner / ro-stale-retry / resource-list-content /
// ro:refresh / the literal gate `id === 'resource-list-content'` / the
// `htmx:responseError.{0,200}isListRefreshEvent` gate / `banner.hidden = false`
// + `= true` / NO `innerHTML = ''` (the data-never-disappears law).

import { isPreloadRequest, noteRefreshFailure, refreshNextAtMs } from './refresh.js';

const STALE_DIM_CLASS = 'ro-stale';

// The 1s ticker repainting the live "Retrying in Ns" countdown while the stale
// banner is visible (started by markListStale, stopped by clearListStale -- the
// banner and its countdown share a lifetime).
let staleCountdownId: number | null = null;

// updateStaleCountdown paints seconds-to-next-retry into the banner's
// [data-stale-countdown] span. The span is re-queried on every paint. With no
// retry armed (interval Off; the banner can still reveal when a user-initiated
// table request fails) the shipped "…" placeholder is restored.
export function updateStaleCountdown(): void {
    const span = document.querySelector('.ro-stale-banner [data-stale-countdown]');
    if (!span) {
        return;
    }
    const nextAt = refreshNextAtMs();
    if (!nextAt) {
        span.textContent = '…';
        return;
    }
    const remaining = Math.max(0, Math.ceil((nextAt - Date.now()) / 1000));
    span.textContent = `${remaining}s`;
}

// True when the htmx event belongs to a request that lands in the live
// resource-list region: issued BY #resource-list-content (the refresh tick /
// retry) or TARGETING it (a user sort/filter partial). Guards so an unrelated
// boosted navigation error never dims the table. Preload warm-ups never swap,
// so they are excluded.
export function isListRefreshEvent(event: Event): boolean {
    const detail = (event as CustomEvent).detail;
    if (!detail || isPreloadRequest(event)) {
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
    const content = document.getElementById('resource-list-content');
    if (content) {
        content.classList.add(STALE_DIM_CLASS);
    }
    const banner = document.querySelector('.ro-stale-banner') as HTMLElement | null;
    if (banner) {
        banner.hidden = false;
    }
    // Live countdown for the banner's "Retrying in Ns" (the data-stale-countdown
    // hook). The immediate paint lands the right number before the ticker's
    // first 1s beat.
    if (staleCountdownId === null) {
        staleCountdownId = window.setInterval(updateStaleCountdown, 1000);
    }
    updateStaleCountdown();
}

export function clearListStale(): void {
    const content = document.getElementById('resource-list-content');
    if (content) {
        content.classList.remove(STALE_DIM_CLASS);
    }
    const banner = document.querySelector('.ro-stale-banner') as HTMLElement | null;
    if (banner) {
        banner.hidden = true;
    }
    if (staleCountdownId !== null) {
        window.clearInterval(staleCountdownId);
        staleCountdownId = null;
    }
}

// A non-2xx reply to the refresh GET: keep the rows (htmx does not swap on
// error), dim them, reveal the stale banner. The failure note FIRST: it re-aims
// the retry schedule, so the banner reveals with the countdown already pointing
// at the real next attempt.
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshFailure();
        markListStale();
    }
});
// A transport failure on the refresh GET: same stale treatment.
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshFailure();
        markListStale();
    }
});
