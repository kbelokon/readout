// init.ts -- the resident htmx-lifecycle ORCHESTRATION + the idempotent init
// chain, the last blocks lifted out of legacy.js. These are NOT leaf bindings
// (they do not slot into the delegated-event dispatcher): they are the
// document-level htmx hooks whose ORDER among each other is load-bearing, and
// the runInit step chain whose ORDER is pinned by the windowing/model contract.
// They are kept here, in one module, so the pinned orchestration lives in a
// single auditable place -- exactly the role legacy.js played for them.
//
// What lives here and WHY it is orchestration, not a leaf:
//   - the htmx:configRequest current-navigation gate: an unmodified click on
//     the already-active link at the exact current URL is a true no-op (no XHR,
//     body teardown or history entry);
//   - the htmx:beforeRequest sort-write hook: writes the sort pref ONLY for
//     a direct sort-header gesture, after every configRequest listener has run
//     (so the RO-No-Push programmatic marker is final);
//   - one post-list-update pipeline for both HTMX swaps and Live deltas;
//   - the htmx:beforeSwap body-swap teardown: selection clear plus the Live
//     wrong-page ownership reset;
//   - the history-cache swap intent: retires old Live ownership before HTMX
//     replaces the cached body and binds completion, cancellation, and failure
//     to the currently owned body attempt;
//   - setupStickyNamespace plus runInit on DOMContentLoaded and body settlement;
//     afterSwap runs only the jitter-sensitive YAML fold build.
//
// Cross-module surfaces are imported by name (the bundle inlines them); vendor
// globals (htmx) are reached through a typeof guard, never imported.

import { colsPopOpen, setColsPopOpen, syncColsPopState } from './columns.js';
import { closeRowMenu } from './context-menu.js';
import { applyLiveNameFilter, captureRowModelFromDocument, updateFilterAC } from './filters.js';
import { rememberListValidator, suppressListNotModified } from './list-etag.js';
import { liveApply, liveOnListSwap, liveResetPage } from './live.js';
import { LIST_DELTA_APPLIED_EVENT, type ListDeltaAppliedDetail } from './live-protocol.js';
import { initLogsFollow } from './logs.js';
import { collapseSectionsFromHash } from './misc-ui.js';
import { roPrefsSetSort } from './prefs.js';
import { syncLiveToggle } from './refresh.js';
import {
    applyLiveRowDeletions,
    clearRowState,
    reapplyRowState,
    updateBulkBar,
} from './row-selection.js';
import { clearListStale, isListRefreshEvent } from './stale.js';
import { syncThemeTogglePostTarget } from './theme.js';
import { showToast } from './toasts.js';
import { virtualizeAfterDelta, virtualizeAfterSwap, virtualizeInit } from './virtualizer.js';
import { buildYamlFolds, highlightYamlLine } from './yaml-folds.js';
// skeleton.ts attaches its OWN document listeners at module load (the
// loading-skeleton clone on htmx:beforeRequest + the failed-region clear) and
// has no named export this module uses, so it is pulled into the bundle for its
// side effects -- the same way legacy.js used to side-effect-import it before it
// was dismantled. Without this import the skeleton path drops out of the bundle.
import './skeleton.js';

// ---------------------------------------------------------------------------
// window seams (the detached-result toast bridge).
// ---------------------------------------------------------------------------
// roToast bridges showToast (toasts.ts -- a leaf with no delegated binding) to
// the window.roToast seam the polling layer reaches for its detached "Refresh
// resumed" trigger (refresh.ts) and the bulk over-cap notice (bulk-actions.ts).
// This assignment lived in legacy.js's window-seam tail; it moves here with the
// rest of the orchestration. The seam signature is the typed Window.roToast
// (types.ts global), so this is compiler-checked, not an `as unknown` cast.
window.roToast = showToast;

// ---------------------------------------------------------------------------
// Exact current-navigation no-op: htmx:configRequest.
// ---------------------------------------------------------------------------
// Body-level hx-boost otherwise turns a click on the already-active sidebar /
// tab link into a fresh GET, full body replacement, and duplicate history
// entry. This avoids the unnecessary request, teardown, and state repair.
//
// Cancel at configRequest, before HTMX opens/sends the XHR or shows an
// indicator. The gate is deliberately narrow:
//   - only a real, unmodified primary click on an active navigation anchor;
//   - only a boosted GET targeting body;
//   - both the anchor href and HTMX's final configured path must resolve to the
//     exact current URL, including query; any fragment link is left alone.
// Therefore an active-path link still works when it intentionally clears
// ?sort / filters, Retry links still reload, modified clicks keep browser tab
// semantics, and list refresh / Live traffic cannot match this branch.
export function suppressRedundantActiveNavigation(event: Event): void {
    const detail = Object((event as CustomEvent).detail) as {
        boosted?: unknown;
        elt?: unknown;
        path?: unknown;
        target?: unknown;
        triggeringEvent?: unknown;
        verb?: unknown;
    };
    const source = detail.elt;
    const trigger = detail.triggeringEvent;
    if (
        detail.boosted !== true ||
        detail.target !== document.body ||
        detail.verb !== 'get' ||
        !(source instanceof HTMLAnchorElement) ||
        !(trigger instanceof MouseEvent) ||
        trigger.type !== 'click' ||
        trigger.button !== 0 ||
        trigger.altKey ||
        trigger.ctrlKey ||
        trigger.metaKey ||
        trigger.shiftKey
    ) {
        return;
    }
    if (
        !source.classList.contains('is-active') &&
        !source.parentElement?.classList.contains('is-active')
    ) {
        return;
    }
    const rawHref = source.getAttribute('href');
    if (!rawHref || rawHref.includes('#') || typeof detail.path !== 'string') {
        return;
    }
    try {
        const current = new URL(window.location.href);
        const resolvesToCurrentLocation = (candidate: string): boolean => {
            const resolved = new URL(candidate, current);
            return (
                resolved.origin === current.origin &&
                resolved.pathname === current.pathname &&
                resolved.search === current.search &&
                resolved.hash === current.hash
            );
        };
        // URL.href preserves an otherwise meaningless trailing `?`. Resource
        // detail tabs intentionally use href="?" for Default, so compare the
        // parsed location components: `/pod` and `/pod?` are the same screen,
        // while a non-empty query still makes Default a useful reset action.
        if (resolvesToCurrentLocation(rawHref) && resolvesToCurrentLocation(detail.path)) {
            event.preventDefault();
        }
    } catch {
        // Malformed public event/link input is not proof of a redundant nav.
    }
}
document.addEventListener('htmx:configRequest', suppressRedundantActiveNavigation);

// ---------------------------------------------------------------------------
// Sort-click pref write: htmx:beforeRequest.
// ---------------------------------------------------------------------------
// A USER-initiated sort rides the v2 loop as an hx-get issued by a sort-header
// anchor (inside a <thead> th) targeting #resource-list-content -- the SAME path
// that earns the canonical HX-Push-Url. Hooked on htmx:beforeRequest (which
// fires AFTER every configRequest listener, so the RO-No-Push programmatic
// marker is final): ticks/retries are issued BY the container (and marked
// RO-No-Push -- treated as do-not-write); filter-chip commits are sourced from
// the editor input. Neither matches a thead ancestor. A URL that merely ARRIVES
// with ?sort= (deep link, history restore) never passes here at all: only the
// direct interaction writes the pref.
export function handleSortPreferenceRequest(event: Event): void {
    // htmx owns this event shape, but document-level listeners are public: tests,
    // extensions, and browser tooling can dispatch a partial CustomEvent. Box
    // every unknown value so property reads remain total even for null/primitives.
    const detail = Object((event as CustomEvent).detail) as {
        elt?: unknown;
        requestConfig?: unknown;
        target?: unknown;
    };
    const rawCfg = Object(detail.requestConfig) as { headers?: unknown; path?: unknown };
    const cfg = {
        ...rawCfg,
        headers: Object(rawCfg.headers) as Record<string, unknown>,
    };
    const target = Object(detail.target) as { id?: unknown };
    if (target.id !== 'resource-list-content') {
        return;
    }
    if (cfg.headers['RO-No-Push']) {
        return; // programmatic traffic never writes prefs
    }
    const elt = Object(detail.elt) as { closest(selector: string): Element | null };
    let sortHeader: Element | null = null;
    try {
        sortHeader = elt.closest('thead th');
    } catch {
        // Absent/non-Element source: keep the null sentinel below.
    }
    if (!sortHeader) {
        return; // not a sort-header gesture
    }
    let plural: string;
    let sort: string;
    try {
        const requestURL = new URL(String(cfg.path), window.location.href);
        const rawSort = requestURL.searchParams.get('sort');
        if (!rawSort) {
            return;
        }
        const pathMatch = /\/([^/]+)\/_table$/.exec(requestURL.pathname);
        if (!pathMatch) {
            return;
        }
        sort = rawSort;
        // The route segment can contain percent escapes. Treat a malformed escape
        // exactly like an unparseable URL: it is not a trustworthy preference key,
        // and it must never throw out of the resident document listener.
        plural = decodeURIComponent(pathMatch[1]);
    } catch {
        return; // unparseable URL/route escape -> nothing trustworthy to persist
    }
    roPrefsSetSort(plural, sort);
}
document.addEventListener('htmx:beforeRequest', handleSortPreferenceRequest);

// ---------------------------------------------------------------------------
// One post-list-update pipeline for both HTMX swaps and direct Live deltas.
// ---------------------------------------------------------------------------

type ListUpdate = { kind: 'swap'; event: Event } | ListDeltaAppliedDetail;

function refreshFilterAutocomplete(): void {
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    if (input && document.activeElement === input && input.value) updateFilterAC();
}

function restoreColumnsPopover(): void {
    if (colsPopOpen()) setColsPopOpen(true);
}

// Every successful list representation reaches this exact repair order. Transport
// ownership stays at the edges: a swap records its validator and completes its
// XHR in the caller's finally block; a delta first applies true deletions to the
// identity-keyed selection store. Each repair is isolated so one optional UI
// failure cannot strand Live or skip the remaining model repairs.
export function afterListUpdate(update: ListUpdate): void {
    if (update.kind === 'swap') {
        runInitStep(() => rememberListValidator(update.event));
    } else {
        runInitStep(() => applyLiveRowDeletions(update.deletedKeys));
    }
    [
        clearListStale,
        reapplyRowState,
        applyLiveNameFilter,
        refreshFilterAutocomplete,
        restoreColumnsPopover,
    ].forEach(runInitStep);
    if (update.kind === 'swap') runInitStep(virtualizeAfterSwap);
    else runInitStep(() => virtualizeAfterDelta(update.previousByKey, update.focusKey));
    runInitStep(setupStickyNamespace);
}

document.addEventListener(LIST_DELTA_APPLIED_EVENT, (event) => {
    const detail = (event as CustomEvent<ListDeltaAppliedDetail>).detail;
    afterListUpdate(detail);
});

document.addEventListener('htmx:afterSwap', (event) => {
    const bodySwapped = event.target === document.body;
    // History cache restores swap the history element directly (there is no
    // htmx:beforeSwap). This is the first proof that the cached body, rather
    // than the old screen, is now installed.
    if (bodySwapTicket && bodySwapped) {
        if (bodySwapTicket.phase === 'swap') {
            completeBodySwap();
        } else {
            reloadCurrentHistoryEntry();
        }
    }
    if (isListRefreshEvent(event)) {
        // Even if an optional repair fails, the exact request/snapshot marker
        // must complete; otherwise a healthy 200 is misclassified as fallback.
        try {
            afterListUpdate({ kind: 'swap', event });
        } finally {
            liveOnListSwap(event);
        }
    }
    if (bodySwapped && !bodyReloading) {
        // HTMX applies old settle attributes/classes after afterSwap. Build the
        // jitter-sensitive YAML fold structure now, but defer all state reads
        // and Live initialization until this exact body reaches afterSettle.
        bodyInitPending = document.body;
        runInitStep(buildYamlFolds);
    }
});

// ---------------------------------------------------------------------------
// Selection lifecycle + Live wrong-page teardown: htmx:beforeSwap (body swap).
// ---------------------------------------------------------------------------
// An hx-boost navigation swaps the <body> -- THE "screen change" moment
// where selection clears. Content morphs target #resource-list-content,
// never body, so sort/filter/refresh keep selection; full-page navigations reset
// script state for free. The fresh body renders its own closed menu + empty bar.
// clearListStale rides along for its clearInterval half: the stale-countdown 1s
// ticker is otherwise stopped only by a successful LIST swap, so navigating away
// from a stale list would leak it across the body swap (repainting a banner the
// fresh body renders hidden).
//
document.addEventListener('htmx:beforeSwap', (event) => {
    const detail = (event as CustomEvent).detail;
    // htmx 2.0 classifies every 3xx as swapping. An exact app-managed 304 has
    // no body to morph: keep the last-good DOM, recover stale/backoff state,
    // and return before the ordinary afterSwap repair/Live-reopen pipeline.
    if (suppressListNotModified(event)) {
        clearListStale();
        return;
    }
    if (detail && detail.target === document.body) {
        if (bodySwapTicket || bodyReloading) {
            // afterSwap carries no request identity. A second body response
            // cannot safely complete the ownership held by the first attempt,
            // so cancel it and keep the document inert until one canonical
            // full reload wins.
            event.preventDefault();
            reloadCurrentHistoryEntry();
            return;
        }
        // HTMX deliberately treats 4xx/5xx responses as non-swapping by
        // default. That is the right policy for an in-place list refresh --
        // the last-good table must stay visible -- but it leaves a failed
        // boosted navigation looking like a dead click. A body response is a
        // complete, server-rendered screen (including our designed error
        // states), so let that response replace the old page.
        const status = detail.xhr?.status;
        if (typeof status === 'number' && status >= 400 && status <= 599) {
            detail.shouldSwap = true;
        }
        const ticket = claimBodySwap('normal', 'swap', null);
        closeRowMenu();
        clearRowState();
        clearListStale();
        // Retire the old page before HTMX exposes the new mount; no push may
        // cross that ownership boundary. The settled page opens from clean Off.
        liveResetPage(); // aborts Live and invalidates old continuations
        // Later beforeSwap listeners may cancel this response. Observe their
        // final decision in a microtask, but do not assume an accepted swap is
        // synchronous: HTMX permits an explicit swap delay. afterSwap or
        // swapError remains the event-driven owner of every accepted response.
        queueMicrotask(() => {
            if (!event.defaultPrevented && detail.shouldSwap !== false) return;
            reloadFailedBodySwap(ticket);
        });
    }
});

// HTMX's history cache path does not emit beforeSwap. Claim body ownership on
// the cache hit/miss events instead, before either the cached body is installed
// or the cache-miss request can complete. The ticket is event-driven: afterSwap
// completes an accepted body, the cache-miss XHR/domain events report request
// failure, and a microtask observes cancellation by later listeners.
type BodySwapKind = 'normal' | 'hit' | 'miss';
type BodySwapPhase = 'request' | 'swap';

interface BodySwapTicket {
    kind: BodySwapKind;
    phase: BodySwapPhase;
    xhr: EventTarget | null;
}

let bodySwapTicket: BodySwapTicket | null = null;
let bodyReloading: true | undefined;
let bodyInitPending: HTMLElement | null = null;

function clearBodySwap(): void {
    bodySwapTicket = null;
}

function completeBodySwap(): void {
    clearBodySwap();
    bodyReloading = undefined;
}

function retireCurrentScreenForBodySwap(): void {
    clearListStale();
    liveResetPage();
}

function reloadCurrentHistoryEntry(): void {
    if (bodyReloading) return;
    if (!bodySwapTicket) retireCurrentScreenForBodySwap();
    clearBodySwap();
    bodyReloading = true;
    window.history.go(0);
}

function reloadFailedBodySwap(ticket: BodySwapTicket | null): void {
    if (!ticket || bodySwapTicket !== ticket) return;
    // Popstate already published the destination URL. If its cached body was
    // cancelled or failed, old DOM + new URL has no coherent Live
    // owner. Clear the ticket before reloading so a late duplicate error or
    // cancellation microtask cannot request another navigation.
    reloadCurrentHistoryEntry();
}

function claimBodySwap(
    kind: BodySwapKind,
    phase: BodySwapPhase,
    xhr: EventTarget | null,
): BodySwapTicket {
    const ticket: BodySwapTicket = { kind, phase, xhr };
    bodySwapTicket = ticket;
    return ticket;
}

function beginHistoryBodySwap(event: Event): void {
    if (bodySwapTicket || bodyReloading) {
        // A cache miss can still be in flight when another history intent
        // arrives. HTMX gives the eventual body afterSwap no request identity,
        // so accepting both would let either body reopen Live under the other's
        // URL. Serialize fail-closed: both early events gate their work inside
        // HTMX, so prevent the second cached swap or request from starting.
        event.preventDefault();
        reloadCurrentHistoryEntry();
        return;
    }
    const miss = event.type === 'htmx:historyCacheMiss';
    const detail = Object((event as CustomEvent).detail) as { xhr?: unknown };
    const xhr = miss && detail.xhr instanceof EventTarget ? detail.xhr : null;
    const ticket = claimBodySwap(miss ? 'miss' : 'hit', miss ? 'request' : 'swap', xhr);
    retireCurrentScreenForBodySwap();
    // Both historyCacheHit and the early historyCacheMiss event are cancelable
    // and their dispatch result gates the cached swap / XHR send respectively.
    // This listener runs before integrations may veto either, so observe the
    // final result in a ticketed microtask.
    queueMicrotask(() => {
        if (event.defaultPrevented) reloadFailedBodySwap(ticket);
    });
    if (miss) {
        if (!xhr) {
            event.preventDefault();
            return;
        }
        // Vendored HTMX installs only XHR.onload for history misses. Network
        // errors and aborts therefore have no HTMX domain event; loadend is the
        // total terminal seam. A successful onload first advances this exact
        // ticket through historyCacheMissLoad, so only request-phase loadend is
        // a failure.
        xhr.addEventListener('loadend', () => {
            if (ticket.phase === 'request') {
                reloadFailedBodySwap(ticket);
            }
        });
    }
}

document.addEventListener('htmx:historyCacheHit', beginHistoryBodySwap);
document.addEventListener('htmx:historyCacheMiss', beginHistoryBodySwap);

document.addEventListener('htmx:historyCacheMissLoad', (event) => {
    const detail = Object((event as CustomEvent).detail) as { xhr?: unknown };
    const ticket = bodySwapTicket;
    if (ticket?.kind !== 'miss' || ticket.phase !== 'request' || detail.xhr !== ticket.xhr) {
        // HTMX ignores the MissLoad dispatch result and still calls swap. The
        // reload gate remains the actual safety boundary for this known-stale,
        // unavoidable response.
        event.preventDefault();
        reloadCurrentHistoryEntry();
        return;
    }
    ticket.phase = 'swap';
});

document.addEventListener('htmx:historyCacheMissLoadError', reloadCurrentHistoryEntry);

document.addEventListener('htmx:swapError', (event) => {
    const detail = Object((event as CustomEvent).detail) as { target?: unknown };
    if (event.target === document.body || detail.target === document.body) {
        reloadFailedBodySwap(bodySwapTicket);
    }
});

document.addEventListener('htmx:historyRestore', () => {
    // Marker only. Row/model/bulk repair and Live initialization belong to the
    // restored body's preceding htmx:afterSettle runInit pass, never to this
    // ambiguously ordered event. Keep the resident listener as an explicit
    // lifecycle boundary.
});

window.addEventListener('pageshow', () => {
    // A real document navigation replaces this module. `pageshow` is the
    // equivalent reset boundary for a browser-restored document and gives DOM
    // tests a faithful way to model that new page lifecycle.
    completeBodySwap();
    bodyInitPending = null;
});

// ---------------------------------------------------------------------------
// _all-view sticky offset (setupStickyNamespace).
// ---------------------------------------------------------------------------
// CSS pins the FIRST column at left:0; in the _all view the first column is the
// namespace, so the NAME column (2nd) must pin right after it -- but its offset
// is the namespace column's content-driven width, which CSS can't know. Measure
// it, hand it to CSS as --ns-col-w, and mark the table with .ro-sticky2. A
// single-namespace list (name IS the first column) needs neither. Idempotent;
// re-run on swap and resize since the column width can change.
function setupStickyNamespace(): void {
    document.querySelectorAll('.ro-table-wrap table.ro-table').forEach((table) => {
        // :not(.ro-vspacer): on a windowed table the first tbody row
        // is the top spacer -- measure a real row, or the _all view loses its
        // second sticky column exactly on the lists big enough to window.
        const firstCell = table.querySelector('tbody tr:not(.ro-vspacer) td:first-child');
        if (firstCell?.classList.contains('cell-ns')) {
            (table as HTMLElement).style.setProperty(
                '--ns-col-w',
                `${firstCell.getBoundingClientRect().width}px`,
            );
            table.classList.add('ro-sticky2');
        } else {
            table.classList.remove('ro-sticky2');
            (table as HTMLElement).style.removeProperty('--ns-col-w');
        }
    });
}

function runInitStep(step: () => void): void {
    try {
        step();
    } catch (e) {
        console.warn('readout init step failed', e);
    }
}

// Run all init-time steps. Called on DOMContentLoaded and after the exact body
// replacement settles, because an hx-boost body replacement does not refire
// DOMContentLoaded. Each step remains idempotent for defensive direct callers
// and partial-failure recovery.
function runInit(yamlFoldsBuilt = false): void {
    // An unowned completion must not reopen Live on an untrusted body. The
    // accepted body afterSwap clears its ownership gate before settlement;
    // failed and overlapping attempts stay inert.
    if (bodySwapTicket || bodyReloading) return;
    const steps = [
        syncLiveToggle,
        collapseSectionsFromHash,
        highlightYamlLine,
        initLogsFollow,
        syncThemeTogglePostTarget,
        setupStickyNamespace,
        // Chips-editor row model: captured from the full server-rendered
        // document. ORDER CONTRACT: this step must stay BEFORE the windowing
        // init that prunes rows from the DOM -- at this point
        // the DOM still IS the complete dataset.
        captureRowModelFromDocument,
        // A new projection deliberately clears stale visibleKeys. Re-derive
        // them from the current draft before windowing so navigation/history
        // cannot carry an old page's filter set into this one.
        applyLiveNameFilter,
        // Virtualization engagement: windows the >threshold
        // table the server marked `.ro-windowed`. AFTER the model capture,
        // per the order contract above.
        virtualizeInit,
        // Columns-popover open flag: re-derived from the fresh DOM so a
        // boosted body swap (rendered closed) never leaves a stale-open flag.
        syncColsPopState,
        // Row state is keyed by OBJECT identity; the store clears when an
        // hx-boost navigation swaps the body (the htmx:beforeSwap hook above),
        // so this init re-paint scrubs any stale is-selected classes a
        // cached/boosted body carried in -- and the bulk bar re-syncs to the
        // same store right after.
        reapplyRowState,
        updateBulkBar,
        // Live opens only after every synchronous body/model repair, and is the
        // LAST step for that reason. In particular, virtualizeInit may detect a
        // history-restored viewport slice and synchronously issue the mandatory
        // full `_table` rebuild; its beforeRequest ownership must exist before
        // liveApply decides whether to open or suspend.
        liveApply,
    ];
    if (!yamlFoldsBuilt) steps.splice(1, 0, buildYamlFolds);
    steps.forEach(runInitStep);
}

document.addEventListener('DOMContentLoaded', () => runInit());
document.addEventListener('htmx:afterSettle', (event) => {
    const pending = bodyInitPending;
    const detail = Object((event as CustomEvent).detail) as { target?: unknown };
    if (pending && event.target !== pending && detail.target !== pending) {
        return;
    }
    if (!pending) {
        if (!isListRefreshEvent(event)) setupStickyNamespace();
        return;
    }
    bodyInitPending = null;
    runInit(true);
});
// Auto-layout column widths also shift with the viewport.
window.addEventListener('resize', setupStickyNamespace);
