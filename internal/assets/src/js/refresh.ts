// refresh.ts -- the list `_table` request path and the two update controls in
// the navbar (the Live toggle and the one-shot Refresh button). There is NO
// timer here: readout never re-fetches a list on a schedule. Either the user
// holds Live open (live.ts owns that transport) or the user asks for exactly one
// refresh; a stored preference of exactly "Live" is the only thing that turns
// the stream on, and it persists in the ro_prefs cookie.
//
// This module owns the ONE in-flight `_table` request tracker: the identity-keyed
// map that keeps a user gesture from being stomped by a programmatic re-fetch,
// tells live.ts when to suspend/resume the stream, and drives the Refresh
// button's in-flight disabled state. The request-lifecycle listeners
// (configRequest/beforeRequest/afterRequest) are attached at module load.
//
// Cross-module seams (call-time, so the bundle's eval order is irrelevant):
//   - live.ts subscribes to this module's one `_table` request tracker and
//     exposes liveApply/liveSetOff for the toggle;
//   - the pure reconnect math lives in live-policy.ts (unit-tested).
// The Go needle contract (internal/web/list_redesign_test.go) pins RO-No-Push /
// 'dataset.liveUrl === 'location'' / '/_table' / 'xhr.readyState === 4 || 0' /
// htmx:abort surviving in the bundle.

import type { Binding } from './events.js';
import { configureListValidatorRequest } from './list-etag.js';
import { liveApply, liveSetOff } from './live.js';
import { readPrefs, roPrefsSetRefresh } from './prefs.js';

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

interface HtmxRequestDetail {
    elt?: unknown;
    headers?: unknown;
    target?: unknown;
    xhr?: unknown;
}

export interface ListRequestActivity {
    phase: 'start' | 'settle';
    inFlight: number;
}

export interface ListRequestTrackerSnapshot {
    count: number;
}

let listRequestEpoch = 0;
const listRequestsInFlight = new Map<XMLHttpRequest, number>();
const listRequestSubscribers = new Set<(activity: ListRequestActivity) => void>();

function requestDetail(event: Event): HtmxRequestDetail {
    // HTMX normally supplies an object, but lifecycle events are public DOM
    // events. Box an absent or malformed detail so every resident handler is
    // total instead of relying on optional chains at each read site.
    return Object((event as CustomEvent).detail) as HtmxRequestDetail;
}

export function listRequestTrackerSnapshot(): ListRequestTrackerSnapshot {
    return { count: listRequestsInFlight.size };
}

export function subscribeListRequests(
    subscriber: (activity: ListRequestActivity) => void,
): () => void {
    listRequestSubscribers.add(subscriber);
    return () => listRequestSubscribers.delete(subscriber);
}

function publishListRequest(phase: ListRequestActivity['phase']): void {
    const activity: ListRequestActivity = {
        phase,
        inFlight: listRequestsInFlight.size,
    };
    listRequestSubscribers.forEach((subscriber) => {
        subscriber(activity);
    });
}

function settleListRequest(xhr: XMLHttpRequest, owner?: number): void {
    const currentOwner = listRequestsInFlight.get(xhr);
    if (currentOwner === undefined || (owner !== undefined && currentOwner !== owner)) return;
    listRequestsInFlight.delete(xhr);
    publishListRequest('settle');
}

function trackListRequest(xhr: XMLHttpRequest, requestEvent: Event): void {
    if (listRequestsInFlight.has(xhr)) return;
    const owner = ++listRequestEpoch;
    listRequestsInFlight.set(xhr, owner);
    xhr.addEventListener('loadend', () => settleListRequest(xhr, owner));
    publishListRequest('start');
    // beforeRequest is cancelable. htmx opens the XHR before dispatching it, so
    // a later listener can prevent send while readyState remains OPENED (1),
    // and native loadend will never fire. Observe the fully-dispatched event in
    // a microtask so that cancellation by later listeners retires the request.
    // settleListRequest remains the one authoritative owner check, including
    // reuse of the same XHR identity across a page reset.
    queueMicrotask(() => {
        if (requestEvent.defaultPrevented || xhr.readyState === 0) {
            settleListRequest(xhr, owner);
        }
    });
}

// A page/body boundary retires the old screen's requests immediately. Each
// loadend closure carries the owner token minted when it was tracked, so a late
// terminal event from the retired page cannot settle a newer use of the same
// XHR identity.
export function resetListRequestTracker(): void {
    listRequestEpoch += 1;
    if (listRequestsInFlight.size === 0) return;
    listRequestsInFlight.clear();
    publishListRequest('settle');
}

// A settled xhr is DONE (4: load/error/timeout completed) or UNSENT (0:
// aborted/cancelled). Reclaiming by XHR identity survives a detached issuer
// whose htmx:afterRequest never bubbles to document.
export function pruneSettledListRequests(): void {
    listRequestsInFlight.forEach((owner, xhr) => {
        if (xhr.readyState === 4 || xhr.readyState === 0) settleListRequest(xhr, owner);
    });
}

// Mark every request the container itself issues (tick / retry / programmatic
// re-fetch) as non-push: the `_table` handler omits HX-Push-Url for these, so
// only genuine user gestures create history entries.
export function handleRefreshConfigRequest(event: Event): void {
    const detail = requestDetail(event);
    const content = document.getElementById('resource-list-content');
    const sourceIsContent = content !== null && detail.elt === content;
    const targetIsContent = content !== null && detail.target === content;
    if (sourceIsContent || targetIsContent) {
        const headers = Object(detail.headers) as Record<string, unknown>;
        for (const name of Object.keys(headers)) {
            if (name.toLowerCase() === 'ro-no-push') {
                delete headers[name];
            }
        }
        if (sourceIsContent && (detail.target === undefined || targetIsContent)) {
            // This marker is app-owned: exactly one canonical spelling exists
            // only on a current-container request that swaps itself.
            headers['RO-No-Push'] = 'true';
        }
    }
    // This also strips a spoofed conditional header from user list traffic.
    // Only the current container + exact stored `_table` URL earns the
    // app-managed If-None-Match value.
    configureListValidatorRequest(event);
}

document.addEventListener('htmx:configRequest', handleRefreshConfigRequest);

export function handleRefreshBeforeRequest(event: Event): void {
    const detail = requestDetail(event);
    const content = document.getElementById('resource-list-content');
    const xhr = detail.xhr;
    if (!content || !(xhr instanceof XMLHttpRequest) || detail.target !== content) return;
    const sourceIsContent = detail.elt === content;
    if (!sourceIsContent && !(detail.elt instanceof Element)) return;
    trackListRequest(xhr, event);
    if (sourceIsContent) return;
    // The user action wins: abort the container's own in-flight request (a tick
    // that started before the click). htmx aborts the request belonging to the
    // element htmx:abort is triggered on -- the user's request lives on its own
    // element and is untouched.
    const htmx = getHtmx();
    htmx?.trigger(content, 'htmx:abort');
}

document.addEventListener('htmx:beforeRequest', handleRefreshBeforeRequest);

// htmx:afterRequest fires on load, error, abort, AND timeout. When it reaches
// the document the entry is removed here; when it does not (dispatched on a
// detached element), the readyState pruning in fireRefresh reclaims it instead.
export function handleRefreshAfterRequest(event: Event): void {
    const xhr = requestDetail(event).xhr;
    if (xhr instanceof XMLHttpRequest) settleListRequest(xhr);
}

document.addEventListener('htmx:afterRequest', handleRefreshAfterRequest);

// listTableURL derives the `_table` partial URL from the LIVE document URL at
// fire time (path + "/_table" + the current query) -- the replacement for
// the render-time-baked PartialURL contract, so a tick keeps the user's sort/filter.
function listTableURL(): string {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, '')}/_table${u.search}`;
}

// requestListRefresh re-fetches the list fragment through the container's own
// htmx wiring: location-backed lists issue a GET to the derived `_table` URL;
// multi-type lists retain their server-rendered ro:refresh wiring.
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

// --- the Live preference ----------------------------------------------------

// isLiveEnabled is THE stored-preference read: Live is on only when the
// ro_prefs cookie carries exactly "Live". Everything else -- absent, "Off", a
// numeric cadence written by an older build, junk -- is off, so a profile that
// predates this version renders an unpressed toggle and issues no request. The
// legacy `roRefresh` localStorage key is no longer consulted at all.
export function isLiveEnabled(): boolean {
    return readPrefs().refresh === 'Live';
}

// setLivePreference persists the ONLY two values this build writes.
function setLivePreference(on: boolean): void {
    roPrefsSetRefresh(on ? 'Live' : 'Off');
}

// --- the navbar update controls ---------------------------------------------

function liveToggleButton(): HTMLElement | null {
    return document.querySelector('[data-ro-action="toggle-live"]');
}

// syncLiveToggle reflects the stored preference in the toggle's aria-pressed --
// the single attribute the .ro-livedot styling and the e2e suite both read. The
// server renders it from the same cookie; this re-derives it after a boosted
// body swap and after every toggle click. The toggle is absent on pages the
// server says cannot stream (Navbar.LiveAvailable), which is not an error.
export function syncLiveToggle(): void {
    liveToggleButton()?.setAttribute('aria-pressed', isLiveEnabled() ? 'true' : 'false');
}

// The Refresh button is disabled exactly while a `_table` request is in flight
// (its own, or a user sort/filter), so a second click cannot queue a request
// inside htmx. The tracker is the authority; this is its paint.
function syncRefreshNowButton(): void {
    const button = document.querySelector(
        '[data-ro-action="refresh-now"]',
    ) as HTMLButtonElement | null;
    if (button) button.disabled = listRequestsInFlight.size > 0;
}

subscribeListRequests(syncRefreshNowButton);

// --- dispatcher bindings ----------------------------------------------------
// The refresh-domain click branches. All four early-returned in the monolith's
// one big click listener -> stop:true, and none co-matches another leaf's
// selector (or each other's), so their position at the END of the dispatcher's
// leaf list preserves the C1 contract.
export const refreshBindings: Binding[] = [
    // Stale-banner retry: re-fire the (read-only) list GET on
    // #resource-list-content through the shared refresh path (location-backed
    // lists derive `_table` from location.href; multi-type containers trigger
    // their baked ro:refresh). On success the morph swaps fresh rows and the
    // afterSwap handler clears the stale dim + re-hides the banner; on another
    // failure the responseError handler keeps it stale. An in-flight container
    // request is aborted first -- issuing a second container request would make
    // htmx QUEUE it, and a queued request replays on the next htmx:abort with
    // its stale queue-time URL (no queue may ever form). Pure DOM, GET-only --
    // the read-only floor is untouched.
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
            if (isLiveEnabled()) {
                liveApply(true);
            } else {
                requestListRefresh();
            }
            return true;
        },
    },
    // The Live toggle: the whole update mode is this one boolean. Persist it
    // first (the cookie is what a reload, and the server-rendered aria-pressed,
    // read), then hand the transport its instruction -- liveApply(true) forces
    // a fresh attempt even from a previously failed state, liveSetOff aborts
    // and clears the warning surface without issuing any request. The toggle
    // renders only where the server said `_stream` answers.
    {
        event: 'click',
        selector: '[data-ro-action="toggle-live"]',
        stop: true,
        handler: (event) => {
            event.preventDefault();
            const on = !isLiveEnabled();
            setLivePreference(on);
            syncLiveToggle();
            if (on) {
                liveApply(true);
            } else {
                liveSetOff();
            }
            return true;
        },
    },
    // Refresh now: EXACTLY one `_table` request per click, no timer armed, no
    // preference written. The disabled paint is defence in depth -- a click
    // arriving while the tracker is occupied (a queued keyboard repeat, a
    // synthetic click) must not stack a second container request. Prune first:
    // this click is the one gate that can rescue a tracker entry whose issuing
    // element detached mid-request (its htmx:afterRequest never bubbled), so a
    // swallowed terminal event cannot disable Refresh until a hard reload.
    {
        event: 'click',
        selector: '[data-ro-action="refresh-now"]',
        stop: true,
        handler: (event) => {
            event.preventDefault();
            pruneSettledListRequests();
            if (listRequestsInFlight.size === 0) {
                requestListRefresh();
            }
            syncRefreshNowButton();
            return true;
        },
    },
    // The Unavailable banner's Reload: this session can no longer stream (an
    // auth terminal or a rejected admission), and no in-page retry can fix it.
    // A full document load is the only recovery, so it is the only action.
    {
        event: 'click',
        selector: '[data-ro-action="reload"]',
        stop: true,
        handler: (event) => {
            event.preventDefault();
            window.location.reload();
            return true;
        },
    },
];
