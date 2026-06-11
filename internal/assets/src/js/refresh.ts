// refresh.ts -- the auto-refresh tick chain (D18 / SPEC §8.3 + §6.1), migrated
// from legacy.js. OFF by default; the user picks an interval (or Live) in the
// navbar #refresh-dropdown and the choice persists in the ro_prefs cookie (D9).
// When an interval is set and a resource-list page is showing, the tick
// re-fetches the `_table` fragment so it morphs in place.
//
// This module owns the refresh STATE machine: the single pending tick timer
// (a setTimeout chain, not setInterval -- the backoff wait re-derives every
// tick), the failure-stage backoff, and the two xhr-Set in-flight gates that
// keep ticks from stomping user requests or queueing inside htmx. The pure
// cadence/backoff math lives in live-policy.ts (node-tested); this module is
// the DOM/htmx glue around it. The request-lifecycle listeners
// (configRequest/beforeRequest/afterRequest) are attached at module load.
//
// Cross-module seams (call-time, so the bundle's eval order is irrelevant):
//   - live.ts contributes liveFallbackSeconds() (the 5s degraded cadence) and
//     reuses scheduleRefreshTick + the in-flight Sets + pruneSettledListRequests;
//   - stale.ts reads refreshNextAt() for the banner countdown and owns
//     updateStaleCountdown (called here whenever the armed time changes).
// The Go needle contract (internal/web/list_redesign_test.go) pins
// userListRequestsInFlight / containerListRequestsInFlight / RO-No-Push /
// 'dataset.liveUrl === 'location'' / '/_table' / 'xhr.readyState === 4 || 0' /
// htmx:abort surviving in the bundle -- the FORMS are preserved here.

import type { Binding } from './events.js';
import { liveApply, liveFallbackSeconds } from './live.js';
import {
    nextFailureStage,
    effectivePollSeconds as policyEffectivePollSeconds,
    refreshDelaySeconds as policyRefreshDelaySeconds,
} from './live-policy.js';
import { REFRESH_KEY, readPrefs, roPrefsSetRefresh } from './prefs.js';
import { updateStaleCountdown } from './stale.js';

// htmx is a classic-script global loaded before this bundle; reach it through a
// typed accessor (the modules are vendor-agnostic otherwise). Only the surfaces
// the refresh path uses are typed.
interface Htmx {
    ajax(
        method: string,
        url: string,
        opts: { source: Element },
    ): { catch?: (cb: () => void) => void } | undefined;
    trigger(el: Element, name: string): void;
}
function getHtmx(): Htmx | undefined {
    return (window as unknown as { htmx?: Htmx }).htmx;
}

// THE pending tick timer -- a setTimeout CHAIN, not setInterval: the wait
// before the next tick depends on the failure backoff stage (SPEC §8.3), so
// every tick / failure / recovery re-derives it.
let refreshTimerId: number | null = null;
// Epoch ms of the next armed tick (0 = none) -- the stale banner's live
// "Retrying in Ns" countdown reads it through refreshNextAt().
let refreshNextAt = 0;
// Consecutive failed list-refresh attempts since the last success; 0 = healthy.
let refreshFailureStage = 0;

// refreshNextAt() -- the armed-time getter for stale.ts's banner countdown.
export function refreshNextAtMs(): number {
    return refreshNextAt;
}

// userListRequestsInFlight tracks USER-initiated requests targeting
// #resource-list-content (a sort-header hx-get, etc.) by their XHR objects.
// While one is unsettled the refresh tick is suppressed. A Set of xhrs,
// deliberately NOT a counter: htmx dispatches htmx:afterRequest on the ISSUING
// element, and a boosted body swap that detaches it mid-request swallows the
// event -- a counter would stick >= 1 forever. The xhrs know when they settled,
// so the tick gate prunes settled entries by readyState instead.
export const userListRequestsInFlight = new Set<XMLHttpRequest>();

// containerListRequestsInFlight tracks the requests the container ITSELF issues
// (the tick, the stale retry, commitColumnVisibility's re-render) the same way.
// fireRefresh skips while one is unsettled: a second container request would
// QUEUE inside htmx and replay on the next htmx:abort with its stale URL.
export const containerListRequestsInFlight = new Set<XMLHttpRequest>();

// A settled xhr is DONE (4: the load/error/timeout callbacks ran) or UNSENT
// (0: aborted). Either way htmx is no longer waiting on it, so the entry is
// reclaimable even when its htmx:afterRequest was dispatched on a detached
// element.
export function pruneSettledListRequests(requests: Set<XMLHttpRequest>): void {
    requests.forEach((xhr) => {
        if (xhr.readyState === 4 || xhr.readyState === 0) {
            requests.delete(xhr);
        }
    });
}

function isPreloadRequest(event: Event): boolean {
    const cfg = (event as CustomEvent).detail?.requestConfig;
    return !!cfg && !!cfg.headers && cfg.headers['HX-Preloaded'] === 'true';
}

// Re-exported for stale.ts (isListRefreshEvent) so the preload exclusion lives
// in exactly one place.
export { isPreloadRequest };

// True for a USER-initiated request that will swap #resource-list-content: the
// target is the container but the issuing element is something else.
function isUserListRequest(event: Event): boolean {
    const detail = (event as CustomEvent).detail;
    if (!detail?.elt || !detail.target) {
        return false;
    }
    if (detail.elt.id === 'resource-list-content') {
        return false;
    }
    return detail.target.id === 'resource-list-content' && !isPreloadRequest(event);
}

// Mark every request the container itself issues (tick / retry / programmatic
// re-fetch) as non-push: the `_table` handler omits HX-Push-Url for these, so
// only genuine user gestures create history entries (D6).
document.addEventListener('htmx:configRequest', (event) => {
    const elt = (event as CustomEvent).detail?.elt;
    if (elt && elt.id === 'resource-list-content') {
        (event as CustomEvent).detail.headers['RO-No-Push'] = 'true';
    }
});

document.addEventListener('htmx:beforeRequest', (event) => {
    const detail = (event as CustomEvent).detail;
    if (detail?.xhr && detail.elt && detail.elt.id === 'resource-list-content') {
        containerListRequestsInFlight.add(detail.xhr);
        return; // container-issued (tick/retry/programmatic) -- tracked, never aborts anything
    }
    if (!isUserListRequest(event)) {
        return;
    }
    if (detail?.xhr) {
        userListRequestsInFlight.add(detail.xhr);
    }
    // The user action wins: abort the container's own in-flight request (a tick
    // that started before the click). htmx aborts the request belonging to the
    // element htmx:abort is triggered on -- the user's request lives on its own
    // element and is untouched.
    const content = document.getElementById('resource-list-content');
    const htmx = getHtmx();
    if (content && htmx) {
        htmx.trigger(content, 'htmx:abort');
    }
});

// htmx:afterRequest fires on load, error, abort, AND timeout. When it reaches
// the document the entry is removed here; when it does not (dispatched on a
// detached element), the readyState pruning in fireRefresh reclaims it instead.
document.addEventListener('htmx:afterRequest', (event) => {
    const xhr = (event as CustomEvent).detail?.xhr;
    if (xhr) {
        userListRequestsInFlight.delete(xhr);
        containerListRequestsInFlight.delete(xhr);
    }
});

// refreshMode returns the persisted auto-refresh mode ('Off', an interval in
// seconds as a string, 'Live'; '' = no preference) from the ro_prefs cookie.
// The legacy `roRefresh` localStorage key migrates here ONCE (D9 -- owned by
// this unit): a read-once fallback consulted only while the cookie carries no
// refresh value, written through to the cookie immediately.
export function refreshMode(): string {
    const stored = readPrefs().refresh;
    if (stored) {
        return stored;
    }
    let legacy: string | null = null;
    try {
        legacy = window.localStorage.getItem(REFRESH_KEY);
    } catch {
        return ''; // localStorage unavailable (e.g. privacy mode) -> no pref
    }
    if (legacy === null || legacy === '') {
        return '';
    }
    const secs = parseInt(legacy, 10) || 0;
    const mode = secs > 0 ? String(secs) : 'Off';
    roPrefsSetRefresh(mode); // write-through: the cookie is canonical from here
    return mode;
}

export function refreshInterval(): number {
    const secs = parseInt(refreshMode(), 10);
    return Number.isFinite(secs) && secs > 0 ? secs : 0; // 'Off'/'Live'/junk -> 0
}

// listTableURL derives the `_table` partial URL from the LIVE document URL at
// fire time (path + "/_table" + the current query) -- the D6 replacement for
// the render-time-baked PartialURL contract.
function listTableURL(): string {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, '')}/_table${u.search}`;
}

// requestListRefresh re-fetches the list fragment through the container's own
// htmx wiring: the v2 path issues a GET to the location-derived `_table` URL
// with the container as source; the v1 multi-type path triggers ro:refresh.
export function requestListRefresh(): void {
    const content = document.getElementById('resource-list-content');
    const htmx = getHtmx();
    if (!content || !htmx) {
        return;
    }
    if ((content as HTMLElement).dataset.liveUrl === 'location') {
        const request = htmx.ajax('GET', listTableURL(), { source: content });
        if (request && typeof request.catch === 'function') {
            // A transport failure rejects the htmx.ajax promise; the failure is
            // already handled via htmx:sendError (the stale dim + banner), so
            // swallow the rejection instead of spamming unhandled-rejection logs.
            request.catch(() => {});
        }
    } else {
        htmx.trigger(content, 'ro:refresh');
    }
}
// IIFE-compat seam (strangler): the e2e suite drives requestListRefresh through
// window (the production refresh path), so re-expose it explicitly -- the
// roFuzzy / roRowState convention.
(window as unknown as { requestListRefresh: typeof requestListRefresh }).requestListRefresh =
    requestListRefresh;

export function fireRefresh(): void {
    if (document.hidden) {
        return; // paused while the tab is in the background
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    if (userListRequestsInFlight.size > 0) {
        return; // a user-initiated table request is in flight -- never stomp it
    }
    if (containerListRequestsInFlight.size > 0) {
        // The previous container request has not settled. Issuing another would
        // QUEUE it inside htmx, and a queued tick replays on the next htmx:abort
        // with its stale URL -- skip this tick; the next one re-checks.
        return;
    }
    requestListRefresh();
}

// effectivePollSeconds is the poll cadence the tick chain actually arms: the
// chosen interval, or Live's 5s FALLBACK while the stream is degraded. Plain
// 'Live' with a riding stream stays 0 (the chain self-disarms). The fold lives
// in live-policy.ts; this reads the live facts.
export function effectivePollSeconds(): number {
    return policyEffectivePollSeconds(refreshMode(), refreshInterval(), liveFallbackSeconds());
}

// refreshDelaySeconds is the wait until the NEXT tick: the §8.3 backoff over
// the effective cadence and the failure stage (live-policy.ts).
function refreshDelaySeconds(): number {
    return policyRefreshDelaySeconds(effectivePollSeconds(), refreshFailureStage);
}

// scheduleRefreshTick (re)arms THE single pending tick refreshDelaySeconds()
// from NOW. Idempotent: any prior timer is cleared first, so init passes,
// interval picks, failures, and recoveries all converge on one armed timer
// (hx-boost body swaps can never stack timers). A fired tick re-schedules
// BEFORE issuing its request so a skipped fire never kills the chain.
export function scheduleRefreshTick(): void {
    if (refreshTimerId !== null) {
        window.clearTimeout(refreshTimerId);
        refreshTimerId = null;
    }
    const delay = refreshDelaySeconds();
    if (delay <= 0) {
        refreshNextAt = 0;
        updateStaleCountdown();
        return;
    }
    refreshNextAt = Date.now() + delay * 1000;
    refreshTimerId = window.setTimeout(() => {
        refreshTimerId = null;
        scheduleRefreshTick();
        fireRefresh();
    }, delay * 1000);
    updateStaleCountdown();
}

// (Re)arm the poll from the stored preference. Runs on every init pass (a fresh
// full-page render is by definition not stale) and on an interval pick -- both
// end any failure backoff: the next failure escalates from scratch.
export function applyRefresh(): void {
    refreshFailureStage = 0;
    scheduleRefreshTick();
}

// Reflect the stored preference in the navbar control (label + active option +
// an "on" class for the livedot styling). Live labels "Live", activates ONLY
// the Live option, and pulses the livedot through the refresh-on hook in EVERY
// Live substate. Re-run on every init pass because an hx-boost swap re-renders
// the navbar.
export function syncRefreshUI(): void {
    const live = refreshMode() === 'Live';
    const secs = refreshInterval();
    const label = document.getElementById('refresh-label');
    if (label) {
        label.textContent = live ? 'Live' : secs > 0 ? `${secs}s` : 'Off';
    }
    document.querySelectorAll('[data-ro-action="set-refresh"]').forEach((opt) => {
        const value = (opt as HTMLElement).dataset.roInterval ?? '';
        opt.classList.toggle(
            'is-active',
            live ? value === 'Live' : value !== 'Live' && (parseInt(value, 10) || 0) === secs,
        );
    });
    const dropdown = document.getElementById('refresh-dropdown');
    if (dropdown) {
        dropdown.classList.toggle('refresh-on', live || secs > 0);
    }
}

// noteRefreshFailure escalates the backoff one stage and re-arms the pending
// tick at the escalated wait, measured from the failure itself -- so the
// banner's countdown always aims at the real next attempt.
export function noteRefreshFailure(): void {
    refreshFailureStage = nextFailureStage(refreshFailureStage);
    scheduleRefreshTick();
}

// noteRefreshRecovery: the FIRST successful swap after >=1 failures resets the
// backoff and announces it -- "refresh resumed" is the SECOND sanctioned toast
// trigger (D24/SPEC §8.8). Plain successes (stage 0) stay silent.
export function noteRefreshRecovery(): void {
    if (refreshFailureStage === 0) {
        return;
    }
    refreshFailureStage = 0;
    scheduleRefreshTick();
    const toast = window.roToast; // the typed roToast seam (types.ts global)
    if (typeof toast === 'function') {
        toast('Refresh resumed');
    }
}

// Refresh once immediately when returning to a backgrounded tab, so stale data
// updates right away instead of waiting up to a full poll cadence (the Live
// fallback's 5s counts -- effectivePollSeconds; a RIDING stream needs no
// catch-up poll: its reopen pushes a fresh full frame).
document.addEventListener('visibilitychange', () => {
    if (!document.hidden && effectivePollSeconds() > 0) {
        fireRefresh();
    }
});

// --- dispatcher bindings ----------------------------------------------------
// The two refresh-domain click branches that were the LAST resident tails of the
// monolith's big click listener (C1). Both early-returned in the monolith ->
// stop:true. They never co-match (one is .ro-stale-retry, the other
// .refresh-option), and neither co-matches a row/palette/columns selector, so
// their position at the END of the dispatcher's leaf list (after the migrated
// clusters) preserves the C1 contract: every migrated leaf still front-ran the
// monolith, and these were the monolith's own trailing branches. Both now route
// on data-ro-action ("retry" / "set-refresh") instead of the presentational
// .ro-stale-retry / .refresh-option classes -- the interval rides data-ro-interval.
export const refreshBindings: Binding[] = [
    // Stale-banner retry: re-fire the (read-only) auto-refresh GET on
    // #resource-list-content through the shared refresh path (the v2 loop derives
    // the `_table` URL from location.href at click time; the v1 multi-type
    // container triggers its baked ro:refresh). On success the morph swaps fresh
    // rows and the afterSwap handler clears the stale dim + re-hides the banner;
    // on another failure the responseError handler keeps it stale. An in-flight
    // container request (a HUNG tick is exactly the state this button exists for)
    // is aborted first -- issuing a second container request would make htmx
    // QUEUE it, and a queued request replays on the next htmx:abort with its stale
    // queue-time URL (no queue may ever form). Pure DOM, GET-only -- the
    // read-only floor is untouched.
    {
        event: 'click',
        selector: '[data-ro-action="retry"]',
        stop: true,
        handler: (event) => {
            event.preventDefault();
            const content = document.getElementById('resource-list-content');
            const htmx = getHtmx();
            if (content && htmx) {
                htmx.trigger(content, 'htmx:abort');
            }
            requestListRefresh();
            return true;
        },
    },
    // Auto-refresh interval option (navbar #refresh-dropdown): persist the chosen
    // mode in the ro_prefs cookie (D9), re-arm the poll, and reflect it in the
    // control. The Live option (Unit 27/D19) persists the literal 'Live' and rides
    // the same path: liveApply opens/tears down the stream, applyRefresh then arms
    // the poll chain per the EFFECTIVE seconds (0 while a stream is riding). A
    // disabled Live option (multi-type/multi-cluster page) never fires (the
    // browser suppresses clicks on disabled buttons). The dropdown opens through
    // CSS hover/focus, so there is no open/close handler here -- only the
    // selection. Kept its early-return (stop:true).
    {
        event: 'click',
        selector: '[data-ro-action="set-refresh"]',
        stop: true,
        handler: (event, matched) => {
            const option = matched as HTMLElement;
            if (option.dataset.roInterval === 'Live') {
                roPrefsSetRefresh('Live');
            } else {
                const interval = parseInt(option.dataset.roInterval ?? '', 10) || 0;
                roPrefsSetRefresh(interval > 0 ? String(interval) : 'Off');
            }
            liveApply(true); // force: an explicit pick re-attempts even after a fallback
            syncRefreshUI();
            applyRefresh();
            option.blur(); // close the hover-dropdown after a keyboard/touch pick
            event.preventDefault();
            return true;
        },
    },
];
