// init.ts -- the resident htmx-lifecycle ORCHESTRATION + the idempotent init
// chain, the last blocks lifted out of legacy.js. These are NOT leaf bindings
// (they do not slot into the delegated-event dispatcher): they are the
// document-level htmx hooks whose ORDER among each other is load-bearing, and
// the runInit step chain whose ORDER is pinned by the windowing/model contract.
// They are kept here, in one module, so the pinned orchestration lives in a
// single auditable place -- exactly the role legacy.js played for them.
//
// What lives here and WHY it is orchestration, not a leaf:
//   - the htmx:beforeRequest sort-write hook: writes the sort pref ONLY for
//     a direct sort-header gesture, after every configRequest listener has run
//     (so the RO-No-Push programmatic marker is final);
//   - the htmx:afterSwap post-swap PIPELINE: a FIXED order of repairs across
//     four modules (recovery/stale -> row state -> filter -> columns -> window ->
//     live), interleaving migrated + resident surfaces;
//   - the htmx:beforeSwap body-swap teardown: the screen-change clear + the Live
//     wrong-page-gate reset (the reset LITERALS must live in this hook -- the Go
//     needle slices the hook out between its registration and the historyRestore
//     listener below, so the two stay in THIS order);
//   - the htmx:historyRestore repaint: scrubs a cached body's stale row state;
//   - setupStickyNamespace (the _all-view second sticky column) + runInit (the
//     idempotent step chain) on DOMContentLoaded / htmx:load / afterSettle /
//     resize.
//
// Cross-module surfaces are imported by name (the bundle inlines them); vendor
// globals (htmx) are reached through a typeof guard, never imported.

import { colsPopOpen, setColsPopOpen, syncColsPopState } from './columns.js';
import { closeRowMenu } from './context-menu.js';
import { applyLiveNameFilter, captureRowModelFromDocument, updateFilterAC } from './filters.js';
import { liveApply, liveOnListSwap, liveState, liveTeardown } from './live.js';
import { initLogsFollow } from './logs.js';
import { collapseSectionsFromHash } from './misc-ui.js';
import { roPrefsSetSort } from './prefs.js';
import { applyRefresh, noteRefreshRecovery, syncRefreshUI } from './refresh.js';
import { clearRowState, reapplyRowState, updateBulkBar } from './row-selection.js';
import { clearListStale, isListRefreshEvent } from './stale.js';
import { syncThemeTogglePostTarget } from './theme.js';
import { showToast } from './toasts.js';
import { virtualizeAfterSwap, virtualizeInit } from './virtualizer.js';
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
// Sort-click pref write: htmx:beforeRequest.
// ---------------------------------------------------------------------------
// A USER-initiated sort rides the v2 loop as an hx-get issued by a sort-header
// anchor (inside a <thead> th) targeting #resource-list-content -- the SAME path
// that earns the canonical HX-Push-Url. Hooked on htmx:beforeRequest (which
// fires AFTER every configRequest listener, so the RO-No-Push programmatic
// marker is final): ticks/retries are issued BY the container (and marked
// RO-No-Push -- treated as do-not-write), preload warm-ups carry HX-Preloaded,
// filter-chip commits are sourced from the editor input -- none of them match a
// thead ancestor. A URL that merely ARRIVES with ?sort= (deep link, history
// restore) never passes here at all: only the direct interaction writes the pref.
document.addEventListener('htmx:beforeRequest', (event) => {
    const detail = (event as CustomEvent).detail;
    const cfg = detail?.requestConfig;
    if (!cfg || !detail.elt || !detail.target || detail.target.id !== 'resource-list-content') {
        return;
    }
    if (cfg.headers && (cfg.headers['RO-No-Push'] || cfg.headers['HX-Preloaded'] === 'true')) {
        return; // programmatic / warm-up traffic never writes prefs
    }
    if (typeof detail.elt.closest !== 'function' || !detail.elt.closest('thead th')) {
        return; // not a sort-header gesture
    }
    const pathMatch = /\/([^/]+)\/_table(?:[?#]|$)/.exec(cfg.path || '');
    if (!pathMatch) {
        return;
    }
    let sort = '';
    try {
        sort = new URL(cfg.path, window.location.href).searchParams.get('sort') || '';
    } catch {
        return; // unparseable request URL -> nothing trustworthy to persist
    }
    const plural = decodeURIComponent(pathMatch[1]);
    if (plural && sort) {
        roPrefsSetSort(plural, sort);
    }
});

// ---------------------------------------------------------------------------
// Post-swap PIPELINE: htmx:afterSwap (the FIXED order of repairs).
// ---------------------------------------------------------------------------
// A successful refresh swap on #resource-list-content lands fresh rows -> clear
// any prior stale dim + hide the banner. htmx:afterSwap fires only on a 2xx that
// actually swapped, so a recovered refresh self-heals the stale state. The same
// moment re-applies the identity-keyed row state (selection / j-k focus): the
// morph syncs server HTML over client classes, so they must be re-keyed onto the
// rows by data-key after EVERY swap (tick or user sort/filter).
document.addEventListener('htmx:afterSwap', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshRecovery();
        clearListStale();
        reapplyRowState();
        // The morph synced server HTML over the client-added filter classes and
        // emptied the JS-owned autocomplete mount; re-apply the live name match
        // from the surviving draft (ignoreActiveValue kept it) and re-open the
        // dropdown when the user is mid-draft. The row model itself was already
        // re-captured from the fragment in the ro-morph handleSwap.
        applyLiveNameFilter();
        const filterInput = document.getElementById('ro-filter-input') as HTMLInputElement | null;
        if (filterInput && document.activeElement === filterInput && filterInput.value) {
            updateFilterAC();
        }
        // The columns popover re-rendered closed (server truth carries no
        // `.is-open`); re-open it when it was open before the swap so a column
        // toggle / tick never snaps it shut mid-interaction. colsPopOpen()
        // is the columns.ts module flag read (the seam is retired).
        if (colsPopOpen()) {
            setColsPopOpen(true);
        }
        // Re-window -- EVERY swap source lands here: tick, sort/
        // filter swap, retry, AND the Live push (htmx.swap dispatches this
        // same event with target=container + the roLivePush marker, so pushes
        // ride the identical post-swap pipeline). LAST among the repairs, so
        // the adoption render consumes the visibleKeys applyLiveNameFilter
        // just re-derived; it ends in its own reapplyRowState over the slice.
        virtualizeAfterSwap();
        // Live: a REQUEST swap of the container while a stream
        // rides is a param change (`f`/sort via URL, columns via cookie) --
        // tear the stream down and reopen it against the new query under a
        // fresh generation. Pushes themselves (roLivePush) never reopen.
        liveOnListSwap(event);
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
// NEEDLE CONTRACT: the Go test (list_redesign_test.go) slices THIS hook out
// between its registration line and the htmx:historyRestore listener below, then
// asserts the body-swap gate + clearRowState + the three Live-reset literals are
// INSIDE it. The two listeners stay in THIS order; the reset literals stay here.
document.addEventListener('htmx:beforeSwap', (event) => {
    const detail = (event as CustomEvent).detail;
    if (detail && detail.target === document.body) {
        closeRowMenu();
        clearRowState();
        clearListStale();
        // The riding Live stream belongs to the OLD page. liveApply (on the
        // htmx:load re-init) would reconcile it anyway, but only AFTER the
        // body swap -- a push delivered inside that gap would pass the
        // generation check (nothing reset it yet) and morph the old
        // resource's table into the new page's container. Tear it down NOW;
        // the new page's init opens its own stream from the clean idle state
        // (a fresh page init is a fresh attempt, so a sticky fallback resets
        // here exactly like it does on a full-page navigation).
        liveTeardown(); // also zeroes the private liveFallbackSecs (live.ts)
        liveState.status = 'idle';
        liveState.streamPath = '';
    }
});

// A history restore (back/forward) re-paints a CACHED body whose rows may carry
// stale is-selected classes and an is-open bulk-bar snapshot from before the
// navigate-away clear; re-painting from the (cleared) store scrubs both.
// Idempotent with the htmx:load init pass.
document.addEventListener('htmx:historyRestore', () => {
    reapplyRowState();
    updateBulkBar();
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

// Run all init-time steps. Called on DOMContentLoaded and on htmx:load so the
// steps re-apply after an hx-boost body swap (which does not refire
// DOMContentLoaded). Each step is idempotent.
function runInit(): void {
    [
        syncRefreshUI,
        // Live stream reconciliation, BEFORE applyRefresh so
        // the poll chain arms against fresh live state: a riding stream
        // disarms it (effective 0), a fallback sets the 5s cadence.
        liveApply,
        applyRefresh,
        buildYamlFolds,
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
    ].forEach(runInitStep);
}

document.addEventListener('DOMContentLoaded', runInit);
// hx-boost swaps <body> via AJAX rather than a full navigation, so
// DOMContentLoaded will not fire on those transitions; htmx:load re-runs init.
// HTMX events bubble, so we listen on `document` (this script runs in <head>
// before <body> exists, so document.body would be null at this point anyway).
document.addEventListener('htmx:load', runInit);
// The list table morphs in place on ro:refresh; re-measure after the swap settles
// and on resize (auto-layout column widths shift with the viewport).
document.addEventListener('htmx:afterSettle', setupStickyNamespace);
window.addEventListener('resize', setupStickyNamespace);
