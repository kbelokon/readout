// генерится из истории readout.js; РАЗБИРАЕТСЯ по модулям, не дописывается
// (единственное исключение — IIFE-compat seam внизу: одна явная window-запись,
// восстанавливающая неявный глобал классик-скрипта, который съедает IIFE).
// readout.js -- modern ES, EVENT DELEGATION.
//
// All click/change/input/keyup/submit handlers are attached ONCE on `document`
// and dispatch via `event.target.closest(selector)`. Delegated handlers survive
// HTMX DOM swaps (hx-get partial refreshes, hx-boost body swaps) with no re-init,
// because the listener lives on a node that is never replaced.
//
// Init-time logic that is NOT event-driven (the auto-reload timer, the on-load
// YAML line highlight, the on-load hash-based section collapse) runs on
// DOMContentLoaded AND is re-run on htmx:load (hx-boost swaps the body via AJAX,
// so DOMContentLoaded does not fire again). These init steps are idempotent and
// the reload timer is guarded so swaps do not stack multiple timers.
//
// No framework, no build, no bundler, no CDN.

'use strict';

// The ro_prefs cookie codec + write surfaces live in the typed prefs.ts module
// (Unit 7 -- the first extracted module). esbuild resolves './prefs.js' to the
// .ts source at bundle time; the bundled IIFE inlines them, so the contract
// needles and the DOM glue below keep calling them by name. The import covers
// only the surfaces this file still uses; the pure encode/decode halves are
// imported by prefs.test.ts (node:test), not here.
import {
    readPrefs,
    roPrefsSetSort,
    roPrefsSetHiddenColumns,
    roPrefsSetRefresh,
    REFRESH_KEY,
} from './prefs.js';

// Delegated-event dispatcher (Unit 9). The ordered binding list (bindings.ts)
// is registered HERE, at the top of the legacy body, BEFORE any of the
// monolith's own `document.addEventListener` calls below -- so the migrated leaf
// bindings front-run the not-yet-migrated monolith listeners (the dispatch
// contract's "registered first"). esbuild inlines both modules into the IIFE.
import { registerBindings } from './events.js';
import { bindings } from './bindings.js';
registerBindings(bindings);

// Theme-toggle POST target (Unit 9 leaf): the function below is an idempotent
// runInit step consumed by the runInit chain; importing the module also attaches
// its one-time matchMedia change listener.
import { syncThemeTogglePostTarget } from './theme.js';

// Toasts (Unit 9 leaf): showToast is the detached-result notification surface;
// legacy calls it directly (bulk over-cap) and bridges it to window.roToast.
import { showToast } from './toasts.js';

// YAML folds + line-highlight (Unit 9 leaf): buildYamlFolds + highlightYamlLine
// are runInit steps. The .ro-fold-toggle and .linenos a click branches live as
// dispatcher bindings (bindings.ts).
import { buildYamlFolds, highlightYamlLine } from './yaml-folds.js';

// Logs page leaf (Unit 9): initLogsFollow is a runInit step; the Follow toggle
// and ts/wrap display toggles are dispatcher bindings (bindings.ts).
import { initLogsFollow } from './logs.js';

// Misc UI leaves (Unit 9): collapseSectionsFromHash is a runInit step; the
// sidebar / copy / section-fold / namespace-dropdown branches are dispatcher
// bindings (bindings.ts). roPrefsSetNamespace now rides misc-ui directly.
import { collapseSectionsFromHash } from './misc-ui.js';

// Cluster cluster (Unit 10): the ⌘K palette, keyboard nav, row selection, the
// context menu, and the bulk bar now live in dedicated modules; their dispatcher
// bindings ride bindings.ts. legacy.js keeps the htmx-lifecycle hooks (afterSwap
// row-state re-apply, the body-swap clear, history-restore repaint), so it
// imports the few functions those hooks call across the new boundary.
import { closeRowMenu } from './context-menu.js';
import { reapplyRowState, clearRowState, updateBulkBar } from './row-selection.js';

// Filters + columns + virtualizer cluster (Unit 12): the v2 chips editor, the D8
// column-visibility popover, and the >~500-row windowing now live in dedicated
// modules (filters.ts / columns.ts / virtualizer.ts), with the pure grammar +
// windowing math in filters-parse.ts / virtualizer-math.ts (node-tested). Their
// dispatcher bindings ride bindings.ts; the window.roClusterBridge seam this file
// populated is RETIRED (keyboard.ts / palette.ts import the windowed walk +
// colsPopOpen guard directly). legacy.js keeps ONLY the htmx-lifecycle
// ORCHESTRATION that interleaves these with the resident hooks: the ro-morph
// handleSwap (captureRowModel the full fragment, then virtualizePrepareSwap
// detaches its rows) and the big htmx:afterSwap pipeline (applyLiveNameFilter /
// updateFilterAC / columns-popover re-open / virtualizeAfterSwap, in the pinned
// order), plus the runInit engagement steps. It imports the cluster surfaces
// those hooks call across the new boundary.
import { virtualizeInit, virtualizePrepareSwap, virtualizeAfterSwap } from './virtualizer.js';
import {
    captureRowModel,
    captureRowModelFromDocument,
    applyLiveNameFilter,
    updateFilterAC,
} from './filters.js';
import { syncColsPopState, setColsPopOpen, colsPopOpen } from './columns.js';

// Refresh + Live + stale + skeleton cluster (Unit 11): the auto-refresh tick
// chain, the SSE Live bridge, the stale banner, and the loading skeleton now
// live in dedicated modules. legacy.js keeps the still-resident Unit-12/24
// surfaces (the ro-morph extension + cell-flash callbacks, the chips-editor
// filter, the columns popover, the virtualizer) and the lifecycle ORCHESTRATION
// hooks that interleave migrated and not-yet-migrated repairs (the big
// htmx:afterSwap post-swap pipeline, the htmx:beforeSwap body-swap clear, the
// delegated click branches for the refresh-option pick + the stale Retry), so it
// imports the cluster surfaces those hooks call across the new boundary.
//   - requestListRefresh / syncRefreshUI / applyRefresh: refresh.ts (init +
//     the virtualizer/columns re-fetch path);
//   - clearListStale / noteRefreshRecovery: stale.ts / refresh.ts (the afterSwap
//     recovery half + the body-swap clearInterval);
//   - liveApply / liveOnListSwap / liveTeardown / liveState: live.ts (init
//     reconcile, the param-change reopen, the body-swap teardown -- liveState is
//     imported so the wrong-page-gate needle literals stay INSIDE the body hook).
import {
    requestListRefresh,
    syncRefreshUI,
    applyRefresh,
    noteRefreshRecovery,
} from './refresh.js';
import { clearListStale, isListRefreshEvent } from './stale.js';
import { liveApply, liveOnListSwap, liveTeardown, liveState } from './live.js';
// stale.ts + skeleton.ts attach their own document listeners (the stale
// responseError/sendError handlers, the skeleton clone/clear) at module load;
// importing them for side effects wires those into the bundle even though their
// only named exports legacy.js uses are above.
import './skeleton.js';

// ---------------------------------------------------------------------------
// HTMX config: native View Transitions, reduced-motion-aware
// ---------------------------------------------------------------------------
// This script loads AFTER htmx.min.js and runs BEFORE htmx processes the DOM, so
// setting htmx.config here governs every boosted navigation and swap. Enabling
// globalViewTransitions makes htmx wrap swaps in document.startViewTransition()
// for a native crossfade. It degrades automatically where the API is
// unsupported (htmx just swaps). We turn it OFF entirely under
// prefers-reduced-motion so those users get no animation at all. Guard for htmx
// in case the vendored lib failed to load.
if (typeof htmx !== 'undefined') {
    htmx.config.globalViewTransitions =
        !window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

// ---------------------------------------------------------------------------
// Auto-refresh CHANGED-CELL flash -- honest + reduced-motion-safe.
// ---------------------------------------------------------------------------
// The live table refresh morphs the fragment in place via idiomorph
// (hx-swap="morph:innerHTML"), so the page never jumps. To gently surface WHICH
// cells actually changed, we hook idiomorph's per-node morph callbacks: capture a
// cell's text BEFORE the merge (beforeNodeMorphed) and, if it differs AFTER
// (afterNodeMorphed), add a short-lived `ro-cell-changed` class whose CSS plays a
// brief tint fade. Only cells whose rendered text genuinely changed flash -- not
// the whole table on every poll. Pure DOM property writes (no eval, no inline
// handler) -> CSP-clean. The morph ext calls Idiomorph.morph WITHOUT passing
// callbacks, so it inherits Idiomorph.defaults.callbacks (set once here); the
// vendored ext exposes Idiomorph as a classic-script global.
//
// Disabled entirely under prefers-reduced-motion: we never register the callbacks,
// so those users get a silent in-place morph (the progress bar handles that case
// too, and refresh-spin is dropped in CSS). beforeNodeMorphed returns undefined
// (NOT false) so it never cancels a morph; we read text only on element nodes.
if (typeof Idiomorph !== 'undefined'
    && Idiomorph.defaults && Idiomorph.defaults.callbacks
    && !window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    const PRIOR = new WeakMap();
    Idiomorph.defaults.callbacks.beforeNodeMorphed = (oldNode) => {
        if (oldNode && oldNode.nodeType === 1 && oldNode.tagName === 'TD') {
            PRIOR.set(oldNode, oldNode.textContent);
        }
        // return undefined -> idiomorph proceeds with the morph (false would skip it)
    };
    Idiomorph.defaults.callbacks.afterNodeMorphed = (oldNode) => {
        if (!oldNode || oldNode.nodeType !== 1 || oldNode.tagName !== 'TD') {
            return;
        }
        if (!PRIOR.has(oldNode)) {
            return;
        }
        const before = PRIOR.get(oldNode);
        PRIOR.delete(oldNode);
        if (before !== oldNode.textContent) {
            oldNode.classList.remove('ro-cell-changed');
            // force a reflow so re-adding the class restarts the animation if the
            // same cell changes again within the fade window
            void oldNode.offsetWidth;
            oldNode.classList.add('ro-cell-changed');
        }
    };
}

// ---------------------------------------------------------------------------
// ro-morph: the CSP-safe idiomorph swap of the v2 list loop (D6).
// ---------------------------------------------------------------------------
// The vendored idiomorph extension parses any non-trivial hx-swap config
// ("morph:{…}") through Function() -- dynamic code evaluation that the strict
// CSP (script-src 'self', no unsafe-eval) blocks at runtime. The v2 list loop
// NEEDS non-default morph config: ignoreActiveValue keeps the user's filter
// draft + caret when a refresh tick morphs the fragment mid-typing (the server
// fragment would otherwise sync the stale value over the draft; hx-preserve is
// no alternative -- htmx 2.0.4 detaches/reattaches preserved nodes, dropping
// focus). So the config is delivered FROM JS: this handleSwap hook calls
// Idiomorph.morph with an explicit config OBJECT -- no attribute eval anywhere.
// Used by #resource-list-content (hx-ext="ro-morph" + hx-swap="morph") and the
// sort-header partial requests inside it (hx-ext is inherited). morphStyle
// "innerHTML" swaps the fragment INTO the persistent container; rows carry
// data-key-derived ids, so idiomorph matches them by object identity and a
// re-sorted fragment MOVES the existing <tr> nodes instead of rewriting them
// positionally. defaults.callbacks (the cell-flash hooks above) still merge in:
// an explicit config object without `callbacks` inherits Idiomorph.defaults.
if (typeof htmx !== 'undefined' && typeof Idiomorph !== 'undefined') {
    htmx.defineExtension('ro-morph', {
        isInlineSwap: (swapStyle) => swapStyle === 'morph',
        handleSwap: (swapStyle, target, fragment) => {
            if (swapStyle !== 'morph') {
                return false; // not ours -> htmx falls through to its native swaps
            }
            // Filters v2 (D7/D20): capture the FULL row model from the incoming
            // SERVER fragment before the morph. The server always renders the
            // complete list (no pagination), so the fragment is the full dataset
            // even when a client-side windowing layer (Unit 24) keeps only a
            // window of rows in the live DOM -- the free-text matcher and the
            // value-frequency autocomplete must never read the windowed DOM.
            if (target && target.id === 'resource-list-content') {
                captureRowModel(fragment);
                // Virtualization (Unit 24/D20), AFTER the model capture: a
                // >threshold fragment's rows are detached for adoption so
                // they never ride the morph (height-preserving spacers stand
                // in); virtualizeAfterSwap re-windows once the morph lands.
                virtualizePrepareSwap(fragment);
            }
            return Idiomorph.morph(target, fragment.children, {
                morphStyle: 'innerHTML',
                ignoreActiveValue: true,
            });
        },
    });
}

// ---------------------------------------------------------------------------
// Delegated CLICK handlers
// ---------------------------------------------------------------------------
document.addEventListener('click', (event) => {
    const target = event.target;

    // ⌘K palette click branches (result row / [data-palette-open] / Refine·⌘K
    // [data-search-refine] / backdrop) migrated to palette.ts (Unit 10): they
    // were the HEAD of this listener and now ride dispatcher click bindings
    // registered ahead of it (bindings.ts).

    // Stale-banner retry: re-fire the (read-only) auto-refresh GET on
    // #resource-list-content through the shared refresh path (the v2 loop
    // derives the `_table` URL from location.href at click time; the v1
    // multi-type container triggers its baked ro:refresh). On success the morph
    // swaps fresh rows and the afterSwap handler clears the stale dim +
    // re-hides the banner; on another failure the responseError handler keeps
    // it stale. An in-flight container request (a HUNG tick is exactly the
    // state this button exists for) is aborted first -- issuing a second
    // container request would make htmx QUEUE it, and a queued request replays
    // on the next htmx:abort with its stale queue-time URL (the
    // commitColumnVisibility pattern; no queue may ever form). Pure DOM,
    // GET-only -- the read-only floor is untouched.
    const staleRetry = target.closest('.ro-stale-retry');
    if (staleRetry) {
        event.preventDefault();
        const content = document.getElementById('resource-list-content');
        if (content && typeof htmx !== 'undefined') {
            htmx.trigger(content, 'htmx:abort');
        }
        requestListRefresh();
        return;
    }
    // Logs Follow toggle (D25) migrated to logs.ts (Unit 9 leaf): handled by a
    // stop:true dispatcher binding registered ahead of this listener.
    // Chips editor (D7) chip-✕ / autocomplete-item / field-focus click branches
    // migrated to filters.ts (Unit 12): stop:true dispatcher click bindings
    // registered ahead of this listener (the chip-✕ ride-the-loop, the AC accept,
    // the field caret-landing). The filter-AC outside-click (C5) is a no-selector
    // dispatcher binding in the same module.
    // Column-visibility popover (D8) ⊞ toggle + column-checkbox commit migrated to
    // columns.ts (Unit 12): the [data-cols-toggle] toggle binding (NOT stop:true,
    // so the C4 outside-click guard stays the single-close mechanism) and the
    // .col-toggle commit (stop:true) ride dispatcher click bindings ahead of this
    // listener; the columns outside-click (C4) is a no-selector binding alongside.
    // In-cell +N overflow (SPEC §4.9/§4.10): the `.ro-chip.more[data-more]`
    // button toggles `.expanded` on its OWN `.ro-chips` strip, revealing the
    // `.xtra` chips in place (the button face flips +N <-> "less" in CSS).
    // Delegated so it survives every morph; aria-expanded mirrors the state.
    // Note: a refresh morph re-renders the strip collapsed (server truth) --
    // expansion is a transient peek, not persisted state.
    const moreChips = target.closest('[data-more]');
    if (moreChips) {
        event.preventDefault();
        const chips = moreChips.closest('.ro-chips');
        if (chips) {
            const expanded = chips.classList.toggle('expanded');
            moreChips.setAttribute('aria-expanded', expanded ? 'true' : 'false');
        }
        return;
    }
    // Long-annotation toggle (SPEC §7.15): a >120-char annotation renders as a
    // collapsed `key · size` button + a hidden scrollable <pre> payload. The
    // delegated click flips the [hidden] attribute on the sibling .anno-pre,
    // mirrors the state into aria-expanded, and rotates the chevron via the
    // .open class -- CSP-clean (no inline handler) and morph-safe (server truth
    // re-renders collapsed; expansion is a transient peek, like the chip
    // overflow above).
    const annoToggle = target.closest('[data-annolong]');
    if (annoToggle) {
        event.preventDefault();
        const pre = annoToggle.parentElement && annoToggle.parentElement.querySelector('.anno-pre');
        if (pre) {
            const open = pre.hidden;
            pre.hidden = !open;
            annoToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
            annoToggle.classList.toggle('open', open);
        }
        return;
    }
    // Mobile hamburger (.menu-toggle) migrated to misc-ui.ts (Unit 9 leaf):
    // handled by a stop:true dispatcher binding registered ahead of this listener.

    // Auto-refresh interval option (navbar #refresh-dropdown): persist the
    // chosen mode in the ro_prefs cookie (D9 -- the legacy roRefresh
    // localStorage write is retired; refreshMode() still reads that key once as
    // a migration fallback), re-arm the poll, and reflect it in the control.
    // The Live option (Unit 27/D19) persists the literal 'Live' (schema-valid
    // per D9) and rides the same path: liveApply opens/tears down the stream,
    // applyRefresh then arms the poll chain per the EFFECTIVE seconds (0 while
    // a stream is riding -- "enabling Live stops the polling timer"). A
    // disabled Live option (multi-type/multi-cluster page) never fires: the
    // browser suppresses clicks on disabled buttons. The dropdown opens
    // through CSS hover/focus, so there is no open/close handler here -- only
    // the selection.
    const refreshOption = target.closest('.refresh-option');
    if (refreshOption) {
        if (refreshOption.dataset.interval === 'Live') {
            roPrefsSetRefresh('Live');
        } else {
            const interval = parseInt(refreshOption.dataset.interval, 10) || 0;
            roPrefsSetRefresh(interval > 0 ? String(interval) : 'Off');
        }
        liveApply(true); // force: an explicit pick re-attempts even after a fallback
        syncRefreshUI();
        applyRefresh();
        refreshOption.blur(); // close the hover-dropdown after a keyboard/touch pick
        event.preventDefault();
        return;
    }

    // .toggle-tools: toggle `is-active` on the control itself and on the
    // element named by its `data-target`.
    const toggle = target.closest('.toggle-tools');
    if (toggle) {
        event.preventDefault();
        toggle.classList.toggle('is-active');
        const targetEl = document.getElementById(toggle.dataset.target);
        if (targetEl) {
            targetEl.classList.toggle('is-active');
        }
        return;
    }

    // .ro-fold-toggle (NESTED YAML block fold) migrated to yaml-folds.ts (Unit 9
    // leaf): registered as a stop:true dispatcher binding ahead of this listener,
    // so the nested-fold click is handled before the section-fold/gutter branches
    // below ever run.

    // .ro-copy-btn (per-section YAML copy) migrated to misc-ui.ts (Unit 9 leaf):
    // a stop:true dispatcher binding registered ahead of this listener (and
    // ahead of the section-fold binding, so a copy click never folds the section).

    // .collapsible h4.title (section collapse + hash sync) migrated to misc-ui.ts
    // (Unit 9 leaf): a stop:true dispatcher binding registered ahead of this
    // listener.

    // YAML line-number anchors (.linenos a) migrated to yaml-folds.ts (Unit 9
    // leaf): handled by a stop:true dispatcher binding registered ahead of this
    // listener.

    // Namespace switch + .context-trigger toggle (D9) migrated to misc-ui.ts
    // (Unit 9 leaf): stop:true dispatcher bindings registered ahead of this
    // listener. The dropdown's `.is-active` flag (read by keyboardSurfaceBusy
    // below) is set on the same element, so the gesture keydown's DOM guard is
    // unchanged.
});

// ---------------------------------------------------------------------------
// Delegated CHANGE handlers
// ---------------------------------------------------------------------------
document.addEventListener('change', (event) => {
    // Search-button enable: a checkbox carries `data-toggle-button="<id>"`. The
    // named button is enabled iff any checkbox sharing that same value is
    // checked, else disabled. Replaces the per-page toggleSearchButton().
    const checkbox = event.target.closest('input[data-toggle-button]');
    if (checkbox) {
        const buttonId = checkbox.dataset.toggleButton;
        const button = document.getElementById(buttonId);
        if (button) {
            const anyChecked = document.querySelectorAll(
                `input[data-toggle-button="${buttonId}"]:checked`
            ).length > 0;
            button.disabled = !anyChecked;
        }
        return;
    }

    // Logs display toggles (D25) #logTs / #logWrap migrated to logs.ts (Unit 9
    // leaf): handled by stop:true dispatcher change-bindings registered ahead of
    // this listener.
});

// ---------------------------------------------------------------------------
// Delegated INPUT handlers
// ---------------------------------------------------------------------------
document.addEventListener('input', (event) => {
    // ⌘K palette query box (#ro-palette-input) migrated to palette.ts (Unit 10):
    // it was the HEAD of this listener and now rides a stop:true dispatcher
    // input-binding registered ahead of it (bindings.ts).

    // Chips editor (D7) #ro-filter-input input migrated to filters.ts (Unit 12):
    // a stop:true dispatcher input-binding registered ahead of this listener
    // re-runs the live name match + autocomplete and clears the unknown-field
    // hint on every keystroke.

    // #namespace-searchbox input (substring filter) migrated to misc-ui.ts
    // (Unit 9 leaf): a stop:true dispatcher input-binding registered ahead of
    // this listener.
});

// Delegated KEYUP handlers: the sole monolith branch (#namespace-searchbox
// Enter-selects-first-visible) migrated to misc-ui.ts (Unit 9 leaf) as a
// dispatcher keyup-binding, so this listener is retired entirely.

// ---------------------------------------------------------------------------
// Delegated KEYDOWN handler -- the chips-editor protocol (filters, Unit 12)
// migrated to filters.ts: #ro-filter-input owns ⏎ commit/accept, Tab accept, esc
// dismiss, arrows, and ⌫-on-empty pop (the focus-routed half of compound case 4:
// an Escape with focus in #ro-filter-input routes to the filter handler, not
// closePalette). It now rides a no-selector dispatcher keydown binding registered
// ahead of the palette keydown (which excludes #ro-filter-input), so this whole
// listener is retired. The popover-submit merge (popFormMergedHref + the
// form.ro-pop-form intercept, the Go-needle contract
// issueFilterNavigation(popFormMergedHref(popForm))) migrated to columns.ts as a
// stop:true dispatcher submit binding.

// ---------------------------------------------------------------------------
// Delegated SUBMIT handlers (the resident v1 tools form)
// ---------------------------------------------------------------------------
document.addEventListener('submit', (event) => {
    // form.tools-form (the v1 multi-type tools form): blank the `name` of
    // empty inputs so they do not become empty query parameters in the
    // resulting GET URL.
    const form = event.target.closest('form.tools-form');
    if (form) {
        Array.prototype.slice.call(form.getElementsByTagName('input')).forEach((input) => {
            if (input.name && !input.value) {
                input.name = '';
            }
        });
    }
});

// ---------------------------------------------------------------------------
// Init-time (NOT event-driven) logic -- idempotent, re-runnable after swaps.
// ---------------------------------------------------------------------------

// highlightYamlLine migrated to yaml-folds.ts (Unit 9 leaf); imported above for
// the runInit chain + the section-collapse line-anchor path still here.

// collapseSectionsFromHash (on-load section collapse from the URL fragment)
// migrated to misc-ui.ts (Unit 9 leaf); imported above for the runInit chain.

// Nested-YAML-block folding migrated to yaml-folds.ts (Unit 9 leaf):
// yamlEffectiveIndent / yamlCodeText / toggleYamlFold / buildYamlFolds /
// injectFoldControls. legacy imports buildYamlFolds (runInit step) and
// yamlCodeText (the still-resident per-section copy branch).

// Logs page Follow (D25) migrated to logs.ts (Unit 9 leaf): logsScrollToTail /
// logsPinTailIfFollowing / initLogsFollow. legacy imports initLogsFollow for
// the runInit chain.

// ---------------------------------------------------------------------------
// ⌘K jump-to command palette v2 migrated to palette.ts + palette-rank.ts
// (Unit 10). The PURE ranker (roFuzzyScore -> window.roFuzzy) + group order
// live in palette-rank.ts (node-tested); the DOM (feed read, recents, row
// build, active model, open/close + focus restore) and the dispatcher
// bindings (click/input/keydown/focusin) live in palette.ts. window.roFuzzy /
// window.roOpenPalette are re-exposed there. PALETTE_ID is now a palette.ts
// constant; keyboardSurfaceBusy (keyboard.ts) reads the same '#ro-palette.open'
// from the live DOM.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ro_prefs preference cookie (D9) -- THE pref write path (the server only reads)
// ---------------------------------------------------------------------------
// The cookie codec lives in the typed prefs.ts module (Unit 7): the pure
// encode/decode halves are exercised by node:test against the SAME golden
// fixtures the Go codec uses, so the Go<->JS wire seam is pinned from both
// sides. The write surfaces (roPrefsSet*) and the cookie reader (readPrefs)
// are imported at the top of this file; the DOM glue below (the sort-write
// htmx hook, the refresh-mode migration) stays here.

// Sort-click pref write: a USER-initiated sort rides the v2 loop as an hx-get
// issued by a sort-header anchor (inside a <thead> th) targeting
// #resource-list-content -- the SAME path that earns the canonical
// HX-Push-Url. Hooked on htmx:beforeRequest (which fires AFTER every
// configRequest listener, so the RO-No-Push programmatic marker is final):
// ticks/retries are issued BY the container (and marked RO-No-Push -- treated
// as do-not-write), preload warm-ups carry HX-Preloaded, filter-chip commits
// are sourced from the editor input -- none of them match a thead ancestor.
// A URL that merely ARRIVES with ?sort= (deep link, history restore) never
// passes here at all: only the direct interaction writes (D9).
document.addEventListener('htmx:beforeRequest', (event) => {
    const detail = event.detail;
    const cfg = detail && detail.requestConfig;
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
    } catch (e) {
        return; // unparseable request URL -> nothing trustworthy to persist
    }
    const plural = decodeURIComponent(pathMatch[1]);
    if (plural && sort) {
        roPrefsSetSort(plural, sort);
    }
});

// ---------------------------------------------------------------------------
// Auto-refresh tick chain + stale banner + Live SSE bridge + skeleton (Unit 11)
// ---------------------------------------------------------------------------
// The auto-refresh interval/Live tick chain (D18/D19), the stale-banner
// machinery (D11), the loading skeleton (D16), and the fetch-based SSE Live
// bridge (Unit 27) migrated to refresh.ts / stale.ts / skeleton.ts / live.ts
// (imported above). legacy.js keeps ONLY the lifecycle ORCHESTRATION that
// interleaves these with the still-resident Unit-12/24 repairs: the big
// htmx:afterSwap post-swap pipeline below (it calls the migrated clearListStale
// / noteRefreshRecovery / liveOnListSwap AND the resident applyLiveNameFilter /
// columns-popover / virtualizeAfterSwap, in the pinned order), plus the
// delegated refresh-option pick + stale Retry click branches (inside the
// not-yet-migrated big click listener) and the body-swap teardown further down.
// A successful refresh swap on #resource-list-content lands fresh rows -> clear
// any prior stale dim + hide the banner. htmx:afterSwap fires only on a 2xx that
// actually swapped, so a recovered refresh self-heals the stale state. The same
// moment re-applies the identity-keyed row state (selection / j-k focus): the
// morph syncs server HTML over client classes, so they must be re-keyed onto
// the rows by data-key after EVERY swap (tick or user sort/filter).
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
        const filterInput = document.getElementById('ro-filter-input');
        if (filterInput && document.activeElement === filterInput && filterInput.value) {
            updateFilterAC();
        }
        // The columns popover re-rendered closed (server truth carries no
        // `.is-open`); re-open it when it was open before the swap so a column
        // toggle / tick never snaps it shut mid-interaction (D8). colsPopOpen()
        // is the columns.ts module flag read (the seam is retired).
        if (colsPopOpen()) {
            setColsPopOpen(true);
        }
        // Re-window (Unit 24/D20) -- EVERY swap source lands here: tick, sort/
        // filter swap, retry, AND the Live push (htmx.swap dispatches this
        // same event with target=container + the roLivePush marker, so pushes
        // ride the identical post-swap pipeline). LAST among the repairs, so
        // the adoption render consumes the visibleKeys applyLiveNameFilter
        // just re-derived; it ends in its own reapplyRowState over the slice.
        virtualizeAfterSwap();
        // Live (Unit 27/D19): a REQUEST swap of the container while a stream
        // rides is a param change (`f`/sort via URL, columns via cookie) --
        // tear the stream down and reopen it against the new query under a
        // fresh generation. Pushes themselves (roLivePush) never reopen.
        liveOnListSwap(event);
    }
});


// ---------------------------------------------------------------------------
// Identity-keyed row state (D6) + row gestures migrated to row-selection.ts /
// bulk-actions.ts / context-menu.ts / keyboard.ts (Unit 10). The selection
// store + j/k focus + the window.roRowState seam + reapplyRowState +
// updateBulkBar live in row-selection.ts (the needle contract still finds
// reapplyRowState / roRowState / tr[data-key] in the bundle); the bulk Copy /
// Download actions in bulk-actions.ts; the right-click context menu in
// context-menu.ts. legacy.js imports closeRowMenu / clearRowState /
// reapplyRowState / updateBulkBar for the htmx-lifecycle hooks below.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Toasts (D24 / SPEC §8.8): bottom-right, 3.5s, mono caption voice. A toast
// exists ONLY for an async result detached from its trigger -- exactly two
// sanctioned triggers: the bulk download refused over the selection cap
// (below) and "refresh resumed" after a failed-then-recovered auto-refresh
// (the polling layer calls window.roToast). Inline state changes (copy ->
// "Copied") stay inline, and there is deliberately NO "download ready" toast:
// the bulk download is a plain GET the browser handles, so no detached ready
// moment exists. The #ro-toasts host is layout chrome OUTSIDE every swap
// target, so an active toast survives list morphs.
// showToast lives in toasts.ts (Unit 9 leaf migration); legacy keeps the
// window.roToast bridge so the polling layer's detached "Refresh resumed"
// trigger (and any non-module caller) still reaches it by the documented name.
// ---------------------------------------------------------------------------
window.roToast = showToast;

// updateBulkBar / roCopyText / toggleRowSelection migrated to row-selection.ts;
// bulkCopyNames / bulkDownloadYAML to bulk-actions.ts; the context menu
// (closeRowMenu / openRowMenu) + the row-gesture click listener (C2: menu-item
// activation, the UNCONDITIONAL dismiss, bulk buttons, row-select) + the
// Esc-closes-menu keydown (K2) to context-menu.ts / bulk-actions.ts /
// row-selection.ts dispatcher bindings (bindings.ts). The intra-listener
// close-menu-then-select sequence (compound case 1) is reproduced as ordered
// bindings with NO stop between the dismiss and the row-select.

// Selection lifecycle: an hx-boost navigation swaps the <body> -- THE "screen
// change" moment (SPEC §6.4) where selection clears. Content morphs target
// #resource-list-content, never body, so sort/filter/refresh keep selection;
// full-page navigations reset script state for free. The fresh body renders
// its own closed menu + empty bar. clearListStale rides along for its
// clearInterval half: the stale-countdown 1s ticker is otherwise stopped only
// by a successful LIST swap, so navigating away from a stale list would leak
// it across the body swap (repainting a banner the fresh body renders hidden).
document.addEventListener('htmx:beforeSwap', (event) => {
    if (event.detail && event.detail.target === document.body) {
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

// A history restore (back/forward) re-paints a CACHED body whose rows may
// carry stale is-selected classes and an is-open bulk-bar snapshot from
// before the navigate-away clear; re-painting from the (cleared) store
// scrubs both. Idempotent with the htmx:load init pass.
document.addEventListener('htmx:historyRestore', () => {
    reapplyRowState();
    updateBulkBar();
});

// ---------------------------------------------------------------------------
// Keyboard row navigation + the "?" overlay (Unit 18) migrated to keyboard.ts
// (Unit 10). keyboardTargetIsTextEntry / keyboardSurfaceBusy / visibleKeyRows /
// moveRowFocus / openFocusedRow + the kbd overlay + the kbd-backdrop click (C3)
// + the gesture keydown (K3) ride dispatcher bindings (bindings.ts). The
// surface-busy DOM guard (palette .open, ctxmenu .is-open, ns-dropdown
// .is-active, columns popover) is the real decoupler; the columns popover flag
// is now imported DIRECTLY from columns.ts (the window.roClusterBridge seam is
// retired, Unit 12).
//
// ---------------------------------------------------------------------------
// Filters + columns + virtualizer (Unit 12) migrated to filters.ts / columns.ts
// / virtualizer.ts (pure halves in filters-parse.ts / virtualizer-math.ts). The
// v2 chips editor (live name match, autocomplete, chip commit/pop, the row
// model), the D8 column-visibility popover (open flag, the ⊞ toggle, the
// checkbox commit, the popover-submit merge), and the >~500-row windowing (the
// spacer math, the morph-adoption pipeline, the j/k windowed walk, window.roVirtual)
// all live in those modules now. legacy.js keeps ONLY the orchestration that
// crosses them: the ro-morph handleSwap (captureRowModel + virtualizePrepareSwap,
// above) and the htmx:afterSwap pipeline (applyLiveNameFilter / updateFilterAC /
// the columns-popover re-open / virtualizeAfterSwap, above), plus the runInit
// engagement steps (captureRowModelFromDocument / virtualizeInit / syncColsPopState,
// below). window.roClusterBridge is gone -- keyboard.ts / palette.ts import the
// windowed walk + the colsPopOpen guard from the modules directly.
// ---------------------------------------------------------------------------

// _all-view sticky offset. CSS pins the FIRST column at left:0; in the _all view
// the first column is the namespace, so the NAME column (2nd) must pin right after
// it -- but its offset is the namespace column's content-driven width, which CSS
// can't know. Measure it, hand it to CSS as --ns-col-w, and mark the table with
// .ro-sticky2. A single-namespace list (name IS the first column) needs neither.
// Idempotent; re-run on swap and resize since the column width can change.
function setupStickyNamespace() {
    document.querySelectorAll('.ro-table-wrap table.ro-table').forEach((table) => {
        // :not(.ro-vspacer): on a windowed table (Unit 24) the first tbody row
        // is the top spacer -- measure a real row, or the _all view loses its
        // second sticky column exactly on the lists big enough to window.
        const firstCell = table.querySelector('tbody tr:not(.ro-vspacer) td:first-child');
        if (firstCell && firstCell.classList.contains('cell-ns')) {
            table.style.setProperty('--ns-col-w', firstCell.getBoundingClientRect().width + 'px');
            table.classList.add('ro-sticky2');
        } else {
            table.classList.remove('ro-sticky2');
            table.style.removeProperty('--ns-col-w');
        }
    });
}

function runInitStep(step) {
    try {
        step();
    } catch (e) {
        console.warn('readout init step failed', e);
    }
}

// Run all init-time steps. Called on DOMContentLoaded and on htmx:load so the
// steps re-apply after an hx-boost body swap (which does not refire
// DOMContentLoaded). Each step is idempotent.
function runInit() {
    [
        syncRefreshUI,
        // Live stream reconciliation (Unit 27/D19), BEFORE applyRefresh so
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
        // Chips-editor row model (D7/D20): captured from the full server-rendered
        // document. ORDER CONTRACT: this step must stay BEFORE the windowing
        // init (Unit 24) that prunes rows from the DOM -- at this point
        // the DOM still IS the complete dataset.
        captureRowModelFromDocument,
        // Virtualization engagement (Unit 24/D20): windows the >threshold
        // table the server marked `.ro-windowed`. AFTER the model capture,
        // per the order contract above.
        virtualizeInit,
        // Columns-popover open flag (D8): re-derived from the fresh DOM so a
        // boosted body swap (rendered closed) never leaves a stale-open flag.
        syncColsPopState,
        // Row state is keyed by OBJECT identity; the store clears when an
        // hx-boost navigation swaps the body (the Unit-16 htmx:beforeSwap
        // hook), so this init re-paint scrubs any stale is-selected classes a
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
