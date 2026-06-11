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
// row-state re-apply, the body-swap clear, history-restore repaint) and the
// virtualizer/columns popover (Unit 24/12, not migrated), so it imports the few
// functions those hooks call across the new boundary.
import { closeRowMenu } from './context-menu.js';
import { reapplyRowState, clearRowState, updateBulkBar } from './row-selection.js';

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
    // Chips editor (D7): a chip's ✕ is a real link (no-JS fallback) whose href
    // is the server-built removal URL; intercept it to ride the v2 partial loop
    // (morph + canonical push) instead of a full navigation.
    const chipRemove = target.closest('#ro-filter-field .chip-x');
    if (chipRemove) {
        event.preventDefault();
        const href = chipRemove.getAttribute('href');
        if (href) {
            issueFilterNavigation(href);
        }
        return;
    }
    // Autocomplete row: clicking accepts it (a complete value commits the chip,
    // a field fills `field:` and opens the value suggestions).
    const acItem = target.closest('#ro-filter-ac .ro-ac-item');
    if (acItem) {
        event.preventDefault();
        setFilterACActive(Number(acItem.dataset.acIndex) || 0);
        acceptFilterAC(true);
        const input = document.getElementById('ro-filter-input');
        if (input) {
            input.focus();
        }
        return;
    }
    // Clicking the editor field anywhere (the padding, a chip's text) lands the
    // caret in the input -- the whole field reads as one input.
    const filterField = target.closest('#ro-filter-field');
    if (filterField) {
        const input = document.getElementById('ro-filter-input');
        if (input && target !== input) {
            input.focus();
        }
        return;
    }
    // Column-visibility popover (D8): the ⊞ title-row button toggles the
    // popover open/closed. Open state is derived from the DOM (a boosted body
    // swap renders it closed, so a stale flag can never invert the gesture);
    // the colsPopOpen flag only re-applies `.is-open` after fragment morphs.
    const colsBtn = target.closest('[data-cols-toggle]');
    if (colsBtn) {
        event.preventDefault();
        const pop = document.getElementById('ro-cols-pop');
        setColsPopOpen(!!pop && !pop.classList.contains('is-open'));
        return;
    }
    // A column checkbox row: flip the checkbox optimistically, then commit the
    // COMPLETE hidden set (as the user now sees it) to the ro_prefs cookie and
    // re-render the fragment through the container's own programmatic path --
    // cookie-state, not URL-state: RO-No-Push, zero history entries (D6/D9).
    // The identity row is a disabled <button>, so its clicks never fire.
    const colToggle = target.closest('.col-toggle');
    if (colToggle) {
        event.preventDefault();
        const check = colToggle.querySelector('.ro-check');
        if (check) {
            check.checked = !check.checked;
        }
        commitColumnVisibility(colToggle.closest('.ro-pop'));
        return;
    }
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

    // Chips editor (D7): every keystroke re-runs the live name match (model-
    // driven, NO request) and the autocomplete; a fresh draft clears any
    // unknown-field hint.
    const filterInput = event.target.closest('#ro-filter-input');
    if (filterInput) {
        hideFilterFieldHint();
        applyLiveNameFilter();
        updateFilterAC();
        return;
    }

    // #namespace-searchbox input (substring filter) migrated to misc-ui.ts
    // (Unit 9 leaf): a stop:true dispatcher input-binding registered ahead of
    // this listener.
});

// Delegated KEYUP handlers: the sole monolith branch (#namespace-searchbox
// Enter-selects-first-visible) migrated to misc-ui.ts (Unit 9 leaf) as a
// dispatcher keyup-binding, so this listener is retired entirely.

// ---------------------------------------------------------------------------
// Delegated KEYDOWN handler -- the chips-editor protocol (filters, Unit 12)
// ---------------------------------------------------------------------------
// The ⌘K palette-open chord, the in-palette Arrow/Enter/Tab/Escape model, and
// the topbar-search focusin opener all migrated to palette.ts (Unit 10) as
// dispatcher keydown/focusin bindings registered ahead of this listener. What
// remains is the FILTER editor's own keyboard protocol (still resident, Unit 12):
// #ro-filter-input owns ⏎ commit/accept, Tab accept, esc dismiss, arrows, and
// ⌫-on-empty pop. This is the focus-routed half of compound case 4 -- an Escape
// with focus in #ro-filter-input reaches handleFilterInputKeydown here (a no-op
// with the autocomplete closed), and the migrated palette-open keydown binding
// excludes #ro-filter-input precisely so it never closes the palette first.
// keydown (not keyup) so we can preventDefault before the browser acts.
document.addEventListener('keydown', (event) => {
    if (event.target && event.target.id === 'ro-filter-input') {
        handleFilterInputKeydown(event);
    }
});

// ---------------------------------------------------------------------------
// Delegated SUBMIT handlers
// ---------------------------------------------------------------------------
// popFormMergedHref builds the D8 popover form's submit URL by MERGING its
// user-editable fields into the LIVE query instead of replacing the query
// wholesale (which is what a native GET submit does). Every location.search
// pair whose key the form does not own survives BYTE-EXACT -- above all the
// `?f=` chips, whose raw OR-commas are wire-significant: the server splits
// alternatives on raw commas BEFORE percent-decoding (filter.go), so the %2C
// a form-urlencoded input would produce turns an OR into a literal comma.
// Mirrors commitFilterChip's raw string-concatenation technique. The form's
// hidden round-trip inputs are NOT owned: their values are snapshots of the
// very pairs the merge already keeps byte-exact (they exist for the no-JS
// fallback); only the visible inputs (labelcols / selector) replace their
// pairs -- a cleared visible input drops its pair, exactly like the native
// path's blank-empty-names trick.
function popFormMergedHref(form) {
    const owned = new Set();
    const fields = [];
    Array.prototype.slice.call(form.elements).forEach((el) => {
        if (el.tagName !== 'INPUT' || el.type === 'hidden' || !el.name) {
            return;
        }
        owned.add(el.name);
        if (el.value) {
            fields.push(el.name + '=' + encodeURIComponent(el.value));
        }
    });
    const kept = [];
    window.location.search.replace(/^\?/, '').split('&').forEach((pair) => {
        if (pair && !owned.has(pair.split('=')[0])) {
            kept.push(pair); // byte-exact survival (raw f= commas included)
        }
    });
    const query = kept.concat(fields).join('&');
    return window.location.pathname + (query ? '?' + query : '');
}

document.addEventListener('submit', (event) => {
    // form.ro-pop-form (the D8 popover's labelcols/selector form): intercept
    // and MERGE into the live query, riding the v2 loop exactly like a chip
    // commit (a user-initiated `_table` GET the server answers with the
    // canonical HX-Push-Url; issueFilterNavigation falls back to a plain
    // navigation when the loop is unavailable -- the merged href is correct
    // either way). The native submit would rebuild the query from the
    // round-trip hidden inputs alone and wipe every `?f=` chip (chips cannot
    // ride hidden inputs -- see popFormMergedHref); it survives only as the
    // no-JS fallback, where the hidden `filter` input still round-trips the
    // legacy text filter and losing `f` is the accepted floor.
    const popForm = event.target.closest('form.ro-pop-form');
    if (popForm) {
        event.preventDefault();
        issueFilterNavigation(popFormMergedHref(popForm));
        return;
    }
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
        // toggle / tick never snaps it shut mid-interaction (D8).
        if (colsPopOpen) {
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
// (Unit 12, still resident) is read through the window.roClusterBridge seam.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Virtualization (Unit 24 / D20): client-side row windowing above ~500 rows
// ---------------------------------------------------------------------------
// Lists always render COMPLETE server-side (no pagination, ever). Above the
// threshold the server marks the table wrap `.ro-windowed` (the threshold has
// ONE owner: resource_table.templ; this script only follows the marker) and
// the virtualizer takes ownership of the tbody: it holds the FULL identity
// row set in memory and keeps only the viewport's slice (+ buffer) in the
// DOM, framed by two spacer rows whose heights stand in for everything
// off-window. The fixed row height (--row-py×2 + line-height, guaranteed by
// the windowed clamp CSS + the server-side expansion flattening) makes the
// offset math exact: it is MEASURED once per engagement as the mean row pitch
// of the full render, so no per-row rounding can accumulate across 600 rows.
//
// Morphs (ALL swap sources -- refresh tick, sort/filter swap, future Live
// push): a >threshold fragment's rows NEVER ride the morph. The ro-morph
// handleSwap hands them to virtualizePrepareSwap, which detaches them for
// adoption and leaves height-preserving spacers in the fragment (emptying the
// tbody outright would shrink the document mid-swap; any forced reflow before
// the adoption render would then CLAMP the scroll position). After the morph
// lands, virtualizeAfterSwap adopts the new full row set and re-renders the
// window -- selection/focus re-key by identity exactly like every other swap,
// and changed cells still flash (the idiomorph cell-flash callbacks never see
// windowed rows, so the diff runs here against the prior row set).
//
// The free-text matcher and the autocomplete frequency scan are UNTOUCHED by
// design: they read window.roRowModel, which is captured from the incoming
// server fragment BEFORE any windowing (D7/D20) -- the virtualizer only
// CONSUMES roRowModel.visibleKeys to decide which rows are renderable.
// Everything is pure DOM (createElement/classList/CSSOM writes): CSP-clean,
// read-only floor untouched.
const VIRT_BUFFER_ROWS = 12;

const virtState = {
    active: false,
    rows: [],            // the FULL identity row set, server order
    byKey: new Map(),    // key -> tr over `rows` (rendered or detached)
    visible: [],         // rows passing the live free-text filter, in order
    rowH: 0,             // the measured fixed row pitch (px)
    start: 0,            // rendered slice bounds over `visible`
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],    // engagement-time column widths (full-render truth)
    pendingRows: null,   // adoption handoff from the ro-morph handleSwap
    pendingScrollY: null,
};

function virtualizerActive() {
    return virtState.active && !!virtState.tbody && virtState.tbody.isConnected;
}

function virtReset() {
    virtState.active = false;
    virtState.rows = [];
    virtState.byKey = new Map();
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
}

// virtMakeSpacer builds one spacer row: a single cell whose height is the
// only thing that matters (the CSS zeroes its padding/border and detaches it
// from the sticky first-column rules). aria-hidden keeps it out of the
// accessibility tree.
function virtMakeSpacer() {
    const tr = document.createElement('tr');
    tr.className = 'ro-vspacer';
    tr.setAttribute('aria-hidden', 'true');
    tr.appendChild(document.createElement('td'));
    return tr;
}

function virtSetSpacerColspan() {
    const cols = virtState.table.querySelectorAll('thead th').length || 1;
    virtState.topSpacer.firstElementChild.colSpan = cols;
    virtState.bottomSpacer.firstElementChild.colSpan = cols;
}

// virtMeasureRowHeight returns the mean row pitch of the CURRENTLY RENDERED
// identity rows (exact at engagement, when the full set is in the DOM).
function virtMeasureRowHeight() {
    const rendered = virtState.tbody.querySelectorAll(':scope > tr[data-key]');
    if (rendered.length === 0) {
        return 0;
    }
    const first = rendered[0].getBoundingClientRect();
    const last = rendered[rendered.length - 1].getBoundingClientRect();
    const pitch = (last.bottom - first.top) / rendered.length;
    return pitch > 0 ? pitch : 0;
}

// virtFallbackRowHeight is the D20 formula (--row-py×2 + line-height + the
// row border) -- only a one-frame seed for the cold-adoption render before a
// real measurement corrects it.
function virtFallbackRowHeight() {
    let py = 9;
    let lh = 18;
    try {
        const cs = window.getComputedStyle(document.documentElement);
        py = parseFloat(cs.getPropertyValue('--row-py')) || py;
        const cell = virtState.tbody && virtState.tbody.querySelector('td');
        if (cell) {
            lh = parseFloat(window.getComputedStyle(cell).lineHeight) || lh;
        }
    } catch (e) {
        // keep the static seed
    }
    return py * 2 + lh + 1;
}

// virtApplyPins re-applies the stored engagement-time column widths (a morph
// syncs the server's attribute-less <th>s over the pins on every tick).
// Returns false when the column SET changed (the D8 popover re-rendered the
// table with different columns) -- the caller re-measures then.
function virtApplyPins() {
    const ths = virtState.table.querySelectorAll('thead th');
    if (virtState.pinnedWidths.length !== ths.length) {
        return false;
    }
    ths.forEach((th, i) => {
        th.style.width = virtState.pinnedWidths[i] + 'px';
    });
    virtState.table.classList.add('ro-virtualized');
    return true;
}

// virtPinColumns measures the auto-layout column widths and freezes them
// (style.width on the header cells + fixed table layout via .ro-virtualized),
// so the window's content can never re-derive column widths scroll-step by
// scroll-step. At engagement the measurement sees the FULL render -- the true
// content-driven widths.
function virtPinColumns() {
    const ths = Array.from(virtState.table.querySelectorAll('thead th'));
    virtState.pinnedWidths = ths.map((th) => th.getBoundingClientRect().width);
    virtApplyPins();
}

// virtComputeVisible derives the renderable row list from the full set and
// the live free-text match (roRowModel.visibleKeys; null = no filter). The
// MATCH itself ran on the full row model -- never the DOM window (D7/D20).
function virtComputeVisible() {
    const keys = roRowModel.visibleKeys;
    virtState.visible = keys
        ? virtState.rows.filter((tr) => keys.has(tr.dataset.key))
        : virtState.rows.slice();
}

// virtWindowBounds computes the desired slice from the page scroll position
// (the document is the vertical scroller; the wrap only scrolls horizontally).
// The tbody's viewport-relative top is exact regardless of what is rendered:
// the spacers preserve every off-window row's height.
function virtWindowBounds() {
    const rect = virtState.tbody.getBoundingClientRect();
    const rowH = virtState.rowH || 1;
    const n = virtState.visible.length;
    const first = Math.floor((0 - rect.top) / rowH);
    const last = Math.ceil((window.innerHeight - rect.top) / rowH);
    let start = Math.max(0, first - VIRT_BUFFER_ROWS);
    let end = Math.min(n, last + VIRT_BUFFER_ROWS);
    if (start > n) {
        start = n;
    }
    if (end < start) {
        end = start;
    }
    return { start: start, end: end };
}

// virtRenderWindow renders the current slice between the two spacers and
// re-keys the identity row state onto whatever is now in the DOM. Rendered
// rows are visible by construction, so any stale live-filter hide class from
// an earlier render is stripped.
function virtRenderWindow() {
    const s = virtState;
    const bounds = virtWindowBounds();
    s.start = bounds.start;
    s.end = bounds.end;
    const n = s.visible.length;
    s.topSpacer.firstElementChild.style.height = (s.start * s.rowH) + 'px';
    s.bottomSpacer.firstElementChild.style.height = ((n - s.end) * s.rowH) + 'px';
    const slice = s.visible.slice(s.start, s.end);
    slice.forEach((tr) => tr.classList.remove(FILTER_HIDE_CLASS));
    s.tbody.replaceChildren(s.topSpacer, ...slice, s.bottomSpacer);
    reapplyRowState();
}

// virtBindMounts re-resolves the live table/tbody from the document (a morph
// may have replaced the nodes the virtualizer held).
function virtBindMounts() {
    const content = document.getElementById('resource-list-content');
    const wrap = content && content.querySelector('.ro-table-wrap.ro-windowed');
    const table = wrap && wrap.querySelector('table.ro-table');
    const tbody = table && table.tBodies.length > 0 ? table.tBodies[0] : null;
    virtState.table = table || null;
    virtState.tbody = tbody || null;
    return !!tbody;
}

// virtualizeInit is the runInit engagement step. ORDER CONTRACT: it runs
// AFTER captureRowModelFromDocument -- at engagement the DOM still IS the
// complete dataset, and the model must capture it before the window prunes
// the rows.
function virtualizeInit() {
    const content = document.getElementById('resource-list-content');
    const wrap = content && content.querySelector('.ro-table-wrap.ro-windowed');
    if (!wrap) {
        virtReset(); // small list / non-list page: windowing disengaged
        return;
    }
    const table = wrap.querySelector('table.ro-table');
    const tbody = table && table.tBodies.length > 0 ? table.tBodies[0] : null;
    if (!tbody) {
        virtReset();
        return;
    }
    if (tbody.querySelector(':scope > tr.ro-vspacer')) {
        if (virtState.active && virtState.tbody === tbody) {
            return; // already engaged on this very tbody (idempotent re-init)
        }
        // A WINDOWED snapshot restored from the history cache: only the
        // cached window's rows exist, the full set is gone. Re-fetch the
        // complete fragment through the container's own programmatic path
        // (RO-No-Push); the adoption pipeline rebuilds the window from it.
        virtReset();
        requestListRefresh();
        return;
    }
    // A fresh full render (initial load or a boosted body swap): the DOM
    // holds the COMPLETE dataset right now -- collect it, measure the row
    // pitch and the true column widths against it, then window.
    const rows = Array.from(tbody.querySelectorAll(':scope > tr[data-key]'));
    if (rows.length === 0) {
        virtReset(); // a v1 multi-type page: no identity rows -> no windowing
        return;
    }
    virtReset();
    virtState.table = table;
    virtState.tbody = tbody;
    virtState.rows = rows;
    virtState.byKey = new Map(rows.map((tr) => [tr.dataset.key, tr]));
    virtState.topSpacer = virtMakeSpacer();
    virtState.bottomSpacer = virtMakeSpacer();
    virtSetSpacerColspan();
    virtState.rowH = virtMeasureRowHeight() || virtFallbackRowHeight();
    virtPinColumns();
    virtState.active = true;
    virtComputeVisible();
    virtRenderWindow();
}

// virtualizePrepareSwap runs INSIDE the ro-morph handleSwap, after the row
// model was captured from the fragment: a >threshold fragment's rows are
// detached for adoption and replaced with two height-preserving spacers, so
// 600 rows never ride the morph and the document height never dips mid-swap.
function virtualizePrepareSwap(fragment) {
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
    const wrap = fragment.querySelector('.ro-table-wrap.ro-windowed');
    const tbody = wrap ? wrap.querySelector('table.ro-table tbody') : null;
    if (!tbody) {
        return; // below-threshold fragment -> plain morph; afterSwap disengages
    }
    const rows = [];
    Array.prototype.forEach.call(tbody.children, (el) => {
        if (el.tagName === 'TR' && el.dataset.key) {
            rows.push(el);
        }
    });
    if (rows.length === 0) {
        return;
    }
    virtState.pendingRows = rows;
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const start = Math.min(virtState.active ? virtState.start : 0, rows.length);
    const topSpacer = virtMakeSpacer();
    const bottomSpacer = virtMakeSpacer();
    topSpacer.firstElementChild.style.height = (start * rowH) + 'px';
    bottomSpacer.firstElementChild.style.height = (Math.max(0, rows.length - start) * rowH) + 'px';
    tbody.replaceChildren(topSpacer, bottomSpacer);
}

// virtualizeAfterSwap completes the morph pipeline on htmx:afterSwap. It runs
// AFTER applyLiveNameFilter re-derived visibleKeys from the surviving draft,
// so the re-window consumes fresh filter state.
function virtualizeAfterSwap() {
    const pending = virtState.pendingRows;
    virtState.pendingRows = null;
    if (!pending) {
        // The fragment fell below the threshold (or was a whole-list state
        // block): the morph landed the complete content in the DOM, so the
        // virtualizer disengages and leaves it alone.
        if (virtState.active) {
            virtReset();
        }
        return;
    }
    const prior = virtState.byKey;
    const wasActive = virtState.active;
    if (!virtBindMounts()) {
        virtReset();
        return;
    }
    virtState.rows = pending;
    virtState.byKey = new Map(pending.map((tr) => [tr.dataset.key, tr]));
    if (!virtState.topSpacer) {
        virtState.topSpacer = virtMakeSpacer();
        virtState.bottomSpacer = virtMakeSpacer();
    }
    virtSetSpacerColspan();
    virtState.active = true;
    if (!virtState.rowH) {
        virtState.rowH = virtFallbackRowHeight();
    }
    virtComputeVisible();
    virtRenderWindow();
    if (!wasActive) {
        // Cold adoption (a chip removal jumped the list back over the
        // threshold): correct the seeded row pitch against real rows once.
        const measured = virtMeasureRowHeight();
        if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
            virtState.rowH = measured;
            virtRenderWindow();
        }
    }
    // The morph synced the server's <th>s over the width pins and the
    // .ro-virtualized class -- re-apply the engagement-time widths (or
    // re-measure when the column set itself changed, e.g. a D8 toggle).
    if (!virtApplyPins()) {
        virtPinColumns();
    }
    // A reflow between the morph and this render could have clamped the
    // scroll against the spacer-only table; the heights are exact again, so
    // the captured offset is reachable -- restore it.
    if (virtState.pendingScrollY !== null && window.scrollY !== virtState.pendingScrollY) {
        window.scrollTo(0, virtState.pendingScrollY);
        virtRenderWindow();
    }
    virtState.pendingScrollY = null;
    virtFlashChangedCells(prior);
}

// virtFlashChangedCells keeps the §8.3 changed-cell flash honest while
// windowed: rows bypass idiomorph (its cell-flash callbacks never fire), so
// the rendered window is diffed here against the prior row set by identity.
// Disabled under prefers-reduced-motion exactly like the idiomorph hooks.
function virtFlashChangedCells(prior) {
    if (!prior || prior.size === 0
        || window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
        return;
    }
    virtState.tbody.querySelectorAll(':scope > tr[data-key]').forEach((tr) => {
        const old = prior.get(tr.dataset.key);
        if (!old) {
            return;
        }
        const oldCells = old.children;
        const newCells = tr.children;
        for (let i = 0; i < newCells.length; i++) {
            const o = oldCells[i];
            const nd = newCells[i];
            if (o && nd && nd.tagName === 'TD' && o.textContent !== nd.textContent) {
                nd.classList.remove('ro-cell-changed');
                void nd.offsetWidth; // restart the animation, the idiomorph-hook pattern
                nd.classList.add('ro-cell-changed');
            }
        }
    });
}

// virtualizeOnFilterChange re-windows over the new visible set whenever the
// live free-text match changes (applyLiveNameFilter calls it last). The match
// ran on the FULL row model, so a name outside the rendered window still
// narrows to its row here. No-op mid-adoption: virtualizeAfterSwap is about
// to recompute everything anyway.
function virtualizeOnFilterChange() {
    if (!virtualizerActive() || virtState.pendingRows) {
        return;
    }
    virtComputeVisible();
    virtRenderWindow();
}

// virtualizeMoveFocus is the j/k walker while windowed: it steps through the
// FULL visible row list (the DOM only holds the window), scrolls the window
// to the target row, and hands the key to the identity focus store.
function virtualizeMoveFocus(delta) {
    const list = virtState.visible;
    if (list.length === 0) {
        return false;
    }
    let current = -1;
    const focusKey = window.roRowState.focusedKey();
    for (let i = 0; i < list.length; i++) {
        if (list[i].dataset.key === focusKey) {
            current = i;
            break;
        }
    }
    const next = Math.max(0, Math.min(list.length - 1, current + delta));
    virtualizeScrollToIndex(next);
    window.roRowState.setFocus(list[next].dataset.key);
    return true;
}

// virtualizeScrollToIndex makes the visible-list row at `index` rendered AND
// inside the viewport (under the sticky topbar) -- the focus jump that
// scrolls the window. scrollBy is synchronous, so the immediate re-render
// lands the row before the caller paints focus onto it.
function virtualizeScrollToIndex(index) {
    const rect = virtState.tbody.getBoundingClientRect();
    const rowTop = rect.top + index * virtState.rowH;
    const rowBottom = rowTop + virtState.rowH;
    const topbar = document.querySelector('header.ro-topbar');
    const topMin = topbar ? topbar.getBoundingClientRect().bottom : 0;
    if (rowTop < topMin) {
        window.scrollBy(0, rowTop - topMin);
    } else if (rowBottom > window.innerHeight) {
        window.scrollBy(0, rowBottom - window.innerHeight);
    }
    virtRenderWindow();
}

// The scroll re-window: one passive document-level listener, rAF-throttled,
// inert unless the virtualizer is engaged (the delegated-listener discipline
// of this file). Re-renders only when the slice bounds actually moved.
let virtScrollScheduled = false;
function virtOnScroll() {
    if (!virtualizerActive()) {
        return;
    }
    const bounds = virtWindowBounds();
    if (bounds.start !== virtState.start || bounds.end !== virtState.end) {
        virtRenderWindow();
    }
}
window.addEventListener('scroll', () => {
    if (!virtState.active || virtScrollScheduled) {
        return;
    }
    virtScrollScheduled = true;
    window.requestAnimationFrame(() => {
        virtScrollScheduled = false;
        virtOnScroll();
    });
}, { passive: true });
// Viewport growth widens the needed window (row pitch itself is re-measured
// only at engagement; the fixed-height law keeps it stable in between).
window.addEventListener('resize', virtOnScroll);
// Web-font activation can shift the line-height the row pitch was measured
// against (engagement at DOMContentLoaded can precede the Geist swap-in);
// re-measure once the fonts settle.
if (document.fonts && document.fonts.ready && typeof document.fonts.ready.then === 'function') {
    document.fonts.ready.then(() => {
        if (!virtualizerActive()) {
            return;
        }
        const measured = virtMeasureRowHeight();
        if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
            virtState.rowH = measured;
            virtRenderWindow();
        }
    });
}

// The deliberate external seam (e2e / console), the roRowState/roFuzzy
// pattern: inspection plus the scroll-to-identity jump the specs drive.
window.roVirtual = {
    active: virtualizerActive,
    renderedBounds() {
        return { start: virtState.start, end: virtState.end, total: virtState.visible.length };
    },
    scrollToKey(key) {
        if (!virtualizerActive()) {
            return false;
        }
        const tr = virtState.byKey.get(key);
        const index = tr ? virtState.visible.indexOf(tr) : -1;
        if (index === -1) {
            return false;
        }
        virtualizeScrollToIndex(index);
        return true;
    },
};

// ---------------------------------------------------------------------------
// Column-visibility popover (D8, client half) -- the ⊞ title-row popover on
// single-type list pages. The popover itself is SERVER-rendered inside the
// morphed fragment (one checkbox per column of the full universe, hidden
// columns included; the identity row disabled; the absorbed labelcols/selector
// form); this script owns only the open state and the toggle gesture.
// ---------------------------------------------------------------------------
// A toggle is cookie-state, not URL-state (D9): it writes the COMPLETE hidden
// set through roPrefsSetHiddenColumns (an empty array is the explicit "hide
// nothing" that suppresses the config default) and re-renders by riding the
// container's own programmatic path (requestListRefresh -> source
// #resource-list-content -> RO-No-Push), so the server never answers with
// HX-Push-Url -- zero history entries, and the URL never changes. The morph
// re-renders the popover from server truth (checkbox states included) and
// wipes the client-added `.is-open`, so colsPopOpen re-applies it after every
// list swap; runInit re-derives the flag from the DOM so a boosted body swap
// (which renders the popover closed) can never leave a stale-open flag.
let colsPopOpen = false;

function setColsPopOpen(open) {
    colsPopOpen = open;
    const pop = document.getElementById('ro-cols-pop');
    if (pop) {
        pop.classList.toggle('is-open', open);
    }
    const btn = document.getElementById('ro-cols-btn');
    if (btn) {
        btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    }
}

// syncColsPopState re-derives the open flag from the freshly-rendered DOM
// (init + boosted swaps render the popover closed; no popover -> closed).
function syncColsPopState() {
    const pop = document.getElementById('ro-cols-pop');
    colsPopOpen = !!pop && pop.classList.contains('is-open');
}

// window.roClusterBridge -- the seam the migrated Unit-10 cluster reads for the
// pieces that still live in this monolith (the roRowState/roVirtual/roFuzzy
// seam pattern): the Unit-24 virtualizer internals (the windowed j/k walk +
// harvest's full row set) and the Unit-12 columns popover open flag (the
// keyboardSurfaceBusy guard). Assigned at module load -- before any dispatcher
// binding can fire (bindings run only inside user events). keyboard.ts /
// palette.ts read it at call time, so the bundle's evaluation order is
// irrelevant. Type: ./cluster-bridge.ts (ClusterBridge).
window.roClusterBridge = {
    virtualizerActive: virtualizerActive,
    virtRows() {
        return virtState.rows;
    },
    virtVisible() {
        return virtState.visible;
    },
    virtRowByKey(key) {
        return virtState.byKey.get(key) || null;
    },
    virtMoveFocus(delta) {
        return virtualizeMoveFocus(delta);
    },
    colsPopOpen() {
        return colsPopOpen;
    },
};

// commitColumnVisibility reads the popover's checkbox state into the complete
// hidden-column list, persists it, and re-renders the fragment. The identity
// row (disabled) never contributes; an in-flight container request (a tick or
// a rapid prior toggle) is aborted first so a stale response can never land
// over the newer cookie state.
function commitColumnVisibility(pop) {
    if (!pop) {
        return;
    }
    const plural = pop.dataset.plural || '';
    if (!plural) {
        return;
    }
    const hidden = [];
    pop.querySelectorAll('.col-toggle').forEach((toggle) => {
        const check = toggle.querySelector('.ro-check');
        if (!toggle.disabled && check && !check.checked && toggle.dataset.col) {
            hidden.push(toggle.dataset.col);
        }
    });
    roPrefsSetHiddenColumns(plural, hidden);
    const content = document.getElementById('resource-list-content');
    if (content && typeof htmx !== 'undefined') {
        htmx.trigger(content, 'htmx:abort');
    }
    requestListRefresh();
}

// A click outside the popover (and not on its ⊞ opener) closes it -- the same
// dismissal contract the autocomplete dropdown uses.
document.addEventListener('click', (event) => {
    if (!colsPopOpen) {
        return;
    }
    if (event.target.closest('#ro-cols-pop') || event.target.closest('[data-cols-toggle]')) {
        return;
    }
    setColsPopOpen(false);
});

// ---------------------------------------------------------------------------
// Filters v2 chips editor (D7, client half) -- free-text live match, operator
// chips, autocomplete, ⌫ pop, unknown-field hint. CSP-clean, GET-only.
// ---------------------------------------------------------------------------
// The editor lives INSIDE the morphed fragment (server renders the chips + the
// #ro-filter-input with a stable id), so a shareable URL lands with chips
// visible and the ignoreActiveValue morph keeps a focused draft + caret across
// refresh ticks. The client owns: the live name match (NO request until an
// operator chip commits), the chip-commit/pop requests (riding the v2 loop --
// user-initiated `_table` GETs that the server answers with the canonical
// HX-Push-Url), and the schema/value autocomplete.
//
// THE FULL ROW MODEL (D20): every matcher/frequency scan reads roRowModel, a
// capture of the COMPLETE server-rendered table -- taken from the incoming
// server fragment in the ro-morph handleSwap (before the morph, and before any
// client windowing layer touches the DOM) and from the full server-rendered
// document at init. Unit 24's windowing must either run AFTER the init capture
// (runInit order below) or feed this model itself; the matcher computes a
// visible-key SET from the model (roRowModel.visibleKeys), and only the
// APPLICATION step touches whatever rows happen to be in the DOM.
const roRowModel = {
    fields: [],      // [{ label, name, hint }] -- hint '' = not filterable
    rows: [],        // [{ key, name, cells: [string] }] -- cells align with fields
    visibleKeys: null, // Set of keys passing the live name match; null = no live filter
};
window.roRowModel = roRowModel;

// Field-name normalization mirror of the server's resolveFilterColumn: typed
// fields resolve case-insensitively with dashes and spaces interchangeable.
function normalizeFieldName(s) {
    return (s || '').toLowerCase().replace(/-/g, ' ').trim();
}

// The suggestion text for a column label ("Nominated Node" -> nominated-node):
// the dashed lowercase form the server resolves via its normalized match.
function fieldSuggestionText(label) {
    return (label || '').toLowerCase().trim().replace(/\s+/g, '-');
}

// captureRowModel reads the chips-editor model from `root` -- the incoming
// server fragment (a DocumentFragment) or the live container at init. The
// header cells carry data-hint ONLY on filterable columns (server-resolved
// Table columns), so synthetic headers (Created, Cluster, Namespace) are
// captured as alignment-only fields with hint ''.
function captureRowModel(root) {
    const table = root.querySelector('table.ro-table');
    if (!table) {
        roRowModel.fields = [];
        roRowModel.rows = [];
        return;
    }
    const fields = [];
    table.querySelectorAll('thead th').forEach((th) => {
        const label = (th.textContent || '').trim();
        fields.push({ label: label, name: fieldSuggestionText(label), hint: th.dataset.hint || '' });
    });
    const rows = [];
    table.querySelectorAll('tbody tr[data-key]').forEach((tr) => {
        const cells = [];
        tr.querySelectorAll('td').forEach((td) => {
            cells.push((td.textContent || '').trim());
        });
        const nameLink = tr.querySelector('td.cell-name a');
        rows.push({
            key: tr.dataset.key,
            name: nameLink ? (nameLink.textContent || '').trim() : (cells[0] || ''),
            cells: cells,
        });
    });
    roRowModel.fields = fields;
    roRowModel.rows = rows;
}

// Init-time capture: the first paint is the full server-rendered list, so the
// live DOM IS the complete model here. Must run before the windowing init
// step (Unit 24) prunes rows -- and must NEVER re-capture once the
// virtualizer is engaged: runInit re-runs on htmx:load (a fresh page fires it
// right after DOMContentLoaded), and by then the DOM is a window, not the
// dataset. The engaged model stays whatever the full render / the last
// server fragment captured.
function captureRowModelFromDocument() {
    const content = document.getElementById('resource-list-content');
    if (content && document.getElementById('ro-filter-input') && !virtualizerActive()) {
        captureRowModel(content);
    }
}

// splitFilterDraft mirrors the server's splitFilterOperator: the FIRST operator
// occurrence (`!=` / `:` / `>` / `<`) splits field from value; null = free text.
function splitFilterDraft(s) {
    for (let i = 0; i < s.length; i++) {
        const c = s[i];
        if (c === '!' && s[i + 1] === '=') {
            return { field: s.slice(0, i).trim(), op: '!=', value: s.slice(i + 2) };
        }
        if (c === ':' || c === '>' || c === '<') {
            return { field: s.slice(0, i).trim(), op: c, value: s.slice(i + 1) };
        }
    }
    return null;
}

// The filterable fields offered by autocomplete: every data-hint column EXCEPT
// the bare cpu/memory capacity columns (the server's cpu/memory ALIASES bind
// only the joined usage columns -- suggesting the capacity column under those
// names would commit chips that match zero rows), plus the virtual fields: the
// `label` grammar always, the cpu/memory aliases when the metrics join is on.
function filterSuggestionFields() {
    const out = [];
    roRowModel.fields.forEach((f) => {
        if (!f.hint) {
            return;
        }
        const norm = normalizeFieldName(f.label);
        if (norm === 'cpu' || norm === 'memory') {
            return; // capacity columns: the alias never binds them (filter.go)
        }
        out.push({ text: f.name, hint: f.hint });
    });
    out.push({ text: 'label', hint: 'key=value' });
    if (hasModelColumn('cpu usage')) {
        out.push({ text: 'cpu', hint: 'quantity' });
    }
    if (hasModelColumn('memory usage')) {
        out.push({ text: 'memory', hint: 'quantity' });
    }
    return out;
}

function hasModelColumn(normName) {
    return roRowModel.fields.some((f) => f.hint && normalizeFieldName(f.label) === normName);
}

// filterFieldKnown mirrors the server's field resolution: `label` always
// resolves; `cpu`/`memory` resolve ONLY via the joined usage columns (never the
// capacity columns); everything else resolves against the data-hint headers.
function filterFieldKnown(field) {
    const want = normalizeFieldName(field);
    if (!want) {
        return false;
    }
    if (want === 'label') {
        return true;
    }
    if (want === 'cpu' || want === 'memory') {
        return hasModelColumn(want + ' usage');
    }
    return roRowModel.fields.some((f) => f.hint && normalizeFieldName(f.label) === want);
}

// fieldColumnIndex resolves a typed field to its model column (for the value
// autocomplete), applying the same cpu/memory aliasing the server does.
function fieldColumnIndex(field) {
    let want = normalizeFieldName(field);
    if (want === 'cpu' || want === 'memory') {
        want += ' usage';
    }
    for (let i = 0; i < roRowModel.fields.length; i++) {
        const f = roRowModel.fields[i];
        if (f.hint && normalizeFieldName(f.label) === want) {
            return i;
        }
    }
    return -1;
}

// ---- live free-text name match (NO request, D7) ----------------------------
const FILTER_HIDE_CLASS = 'ro-row-filtered';

// applyLiveNameFilter narrows the rows to the names containing the draft text,
// entirely client-side. The MATCH runs on the full row model (never the DOM
// window); only the application toggles classes on whatever rows are rendered.
// A draft containing an operator is a chip in progress -- no live narrowing.
function applyLiveNameFilter() {
    const content = document.getElementById('resource-list-content');
    if (!content) {
        return;
    }
    const input = document.getElementById('ro-filter-input');
    const draft = input ? input.value : '';
    const text = (!draft || splitFilterDraft(draft)) ? '' : draft.trim().toLowerCase();
    let visible = null;
    if (text) {
        visible = new Set();
        roRowModel.rows.forEach((row) => {
            if (row.name.toLowerCase().indexOf(text) !== -1) {
                visible.add(row.key);
            }
        });
    }
    roRowModel.visibleKeys = visible;
    content.querySelectorAll('tbody tr[data-key]').forEach((tr) => {
        tr.classList.toggle(FILTER_HIDE_CLASS, !!visible && !visible.has(tr.dataset.key));
    });
    // Virtualization (Unit 24/D20): the class application above only reaches
    // the rendered window -- re-window over the new visible set so a match
    // currently OUTSIDE the window becomes a rendered row.
    virtualizeOnFilterChange();
}

// ---- chip commit / pop: ride the v2 loop ------------------------------------
// issueFilterNavigation GETs the `_table` partial for a CANONICAL list href,
// sourced from the editor input -- a USER-initiated request (no RO-No-Push), so
// the in-flight guard counts it, an in-flight tick is aborted, and the server
// answers with the canonical HX-Push-Url. Falls back to a plain navigation when
// the loop is unavailable.
function issueFilterNavigation(href) {
    const content = document.getElementById('resource-list-content');
    const input = document.getElementById('ro-filter-input');
    if (!content || !input || typeof htmx === 'undefined') {
        window.location.assign(href);
        return;
    }
    const u = new URL(href, window.location.href);
    const partial = u.pathname.replace(/\/+$/, '') + '/_table' + u.search;
    const request = htmx.ajax('GET', partial, {
        source: input,
        target: '#resource-list-content',
        swap: 'morph',
    });
    if (request && typeof request.catch === 'function') {
        request.catch(() => {}); // failures surface via the stale banner path
    }
}

// commitFilterChip materializes the draft as a `?f=` chip. The raw value is
// encodeURIComponent with the OR-commas RESTORED raw -- typed input treats
// every comma as OR (filter.go parses alternatives on raw commas), and the
// `?f=` pair is appended by STRING CONCATENATION so sibling raw params keep
// their exact wire encoding (never URLSearchParams over the whole query).
function commitFilterChip(draft) {
    const text = draft.trim();
    const parsed = splitFilterDraft(text);
    if (!parsed) {
        return; // free text never commits -- it live-matches only
    }
    if (!filterFieldKnown(parsed.field)) {
        showFilterFieldHint();
        return;
    }
    const raw = encodeURIComponent(text).replace(/%2C/gi, ',');
    const search = window.location.search;
    const href = window.location.pathname + (search ? search + '&' : '?') + 'f=' + raw;
    clearFilterDraft();
    issueFilterNavigation(href);
}

// popLastFilterChip (⌫ on empty input) removes the LAST chip by riding its
// server-built removal href (delQueryRawValue keeps sibling chips byte-exact).
function popLastFilterChip() {
    const removers = document.querySelectorAll('#ro-filter-field .ro-scope-chip .chip-x');
    if (removers.length === 0) {
        return;
    }
    const href = removers[removers.length - 1].getAttribute('href');
    if (href) {
        issueFilterNavigation(href);
    }
}

function clearFilterDraft() {
    const input = document.getElementById('ro-filter-input');
    if (input) {
        input.value = '';
    }
    closeFilterAC();
    applyLiveNameFilter();
}

// ---- unknown-field hint ------------------------------------------------------
// "no such field — try status, node, age…" -- the suggestion list is built from
// the ACTUAL schema (first three filterable fields) so the hint is never a lie.
function showFilterFieldHint() {
    const el = document.getElementById('ro-filter-error');
    if (!el) {
        return;
    }
    const names = filterSuggestionFields().slice(0, 3).map((f) => f.text);
    el.textContent = 'no such field — try ' + (names.length ? names.join(', ') : 'status, node, age') + '…';
    el.hidden = false;
}

function hideFilterFieldHint() {
    const el = document.getElementById('ro-filter-error');
    if (el) {
        el.hidden = true;
    }
}

// ---- autocomplete -------------------------------------------------------------
// Client-side only (D7): field names (with type hints) while the draft has no
// operator; after `field:` (the equality form, on a known real column) the top 8
// distinct values by frequency computed from the FULL row model. The operator
// forms (!= > <) autocomplete the field then leave the value free. Tab/⏎
// accepts, esc dismisses. All nodes are built with createElement/textContent.
let filterACItems = []; // [{ label, hint, insert, kind: 'field'|'value' }]
let filterACActive = -1;

function filterACOpen() {
    const ac = document.getElementById('ro-filter-ac');
    return !!ac && !ac.hidden;
}

function closeFilterAC() {
    const ac = document.getElementById('ro-filter-ac');
    if (ac) {
        ac.hidden = true;
        ac.textContent = '';
    }
    filterACItems = [];
    filterACActive = -1;
}

function openFilterAC(items) {
    const ac = document.getElementById('ro-filter-ac');
    if (!ac || items.length === 0) {
        closeFilterAC();
        return;
    }
    ac.textContent = '';
    ac.setAttribute('role', 'listbox');
    filterACItems = items;
    filterACActive = 0;
    items.forEach((item, idx) => {
        const row = document.createElement('div');
        row.className = 'ro-ac-item' + (idx === 0 ? ' active' : '');
        row.setAttribute('role', 'option');
        row.setAttribute('aria-selected', idx === 0 ? 'true' : 'false');
        row.dataset.acIndex = String(idx);
        const name = document.createElement('span');
        name.className = 'ac-name';
        name.textContent = item.label; // textContent -> hostile cell values cannot inject
        row.appendChild(name);
        if (item.hint) {
            const hint = document.createElement('span');
            hint.className = 'ac-hint';
            hint.textContent = item.hint;
            row.appendChild(hint);
        }
        row.addEventListener('mousemove', () => setFilterACActive(idx));
        ac.appendChild(row);
    });
    ac.hidden = false;
}

function setFilterACActive(index) {
    if (filterACItems.length === 0) {
        return;
    }
    filterACActive = Math.max(0, Math.min(filterACItems.length - 1, index));
    const ac = document.getElementById('ro-filter-ac');
    if (!ac) {
        return;
    }
    ac.querySelectorAll('.ro-ac-item').forEach((el) => {
        const on = Number(el.dataset.acIndex) === filterACActive;
        el.classList.toggle('active', on);
        el.setAttribute('aria-selected', on ? 'true' : 'false');
    });
}

function moveFilterACActive(delta) {
    if (filterACItems.length === 0) {
        return;
    }
    setFilterACActive((filterACActive + delta + filterACItems.length) % filterACItems.length);
}

// updateFilterAC re-derives the dropdown from the current draft.
function updateFilterAC() {
    const input = document.getElementById('ro-filter-input');
    if (!input) {
        return;
    }
    const draft = input.value;
    if (!draft.trim()) {
        closeFilterAC();
        return;
    }
    const parsed = splitFilterDraft(draft);
    if (!parsed) {
        // Field-name suggestions: substring match, prefix matches ranked first.
        const q = normalizeFieldName(draft);
        const fields = filterSuggestionFields().filter(
            (f) => normalizeFieldName(f.text).indexOf(q) !== -1
        );
        fields.sort((a, b) => {
            const ap = normalizeFieldName(a.text).indexOf(q) === 0 ? 0 : 1;
            const bp = normalizeFieldName(b.text).indexOf(q) === 0 ? 0 : 1;
            return ap - bp;
        });
        openFilterAC(fields.map((f) => ({
            label: f.text, hint: f.hint, insert: f.text + ':', kind: 'field',
        })));
        return;
    }
    const isLabel = normalizeFieldName(parsed.field) === 'label';
    if (parsed.op !== ':' || isLabel || !filterFieldKnown(parsed.field)) {
        // Operator forms leave the value free; `label` values are not in the row
        // model (metadata.labels never renders for most kinds); unknown fields
        // get the ⏎ hint, not suggestions.
        closeFilterAC();
        return;
    }
    const idx = fieldColumnIndex(parsed.field);
    if (idx < 0) {
        closeFilterAC();
        return;
    }
    // Top 8 distinct values by frequency, computed from the FULL row model.
    const freq = new Map();
    roRowModel.rows.forEach((row) => {
        const v = row.cells[idx];
        if (v) {
            freq.set(v, (freq.get(v) || 0) + 1);
        }
    });
    const typed = parsed.value.trim().toLowerCase();
    let entries = Array.from(freq.entries());
    if (typed) {
        entries = entries.filter(([v]) => v.toLowerCase().indexOf(typed) !== -1);
    }
    entries.sort((a, b) => b[1] - a[1]);
    openFilterAC(entries.slice(0, 8).map(([v, n]) => ({
        label: v, hint: '×' + n, insert: parsed.field.trim() + ':' + v, kind: 'value',
    })));
}

// acceptFilterAC fills the input with the active suggestion. Accepting a FIELD
// readies the value (`field:` + value suggestions open); accepting a complete
// VALUE is a finished chip -- ⏎ commits it directly (Tab only fills).
function acceptFilterAC(commitValues) {
    const input = document.getElementById('ro-filter-input');
    const item = filterACItems[filterACActive];
    if (!input || !item) {
        return;
    }
    input.value = item.insert;
    closeFilterAC();
    if (item.kind === 'value' && commitValues) {
        commitFilterChip(input.value);
    } else {
        applyLiveNameFilter();
        updateFilterAC();
    }
}

// handleFilterInputKeydown is the editor's keyboard protocol, dispatched from
// the delegated document keydown handler.
function handleFilterInputKeydown(event) {
    const input = event.target;
    if (event.key === 'Enter') {
        event.preventDefault();
        if (filterACOpen() && filterACActive >= 0) {
            acceptFilterAC(true);
            return;
        }
        commitFilterChip(input.value);
        return;
    }
    if (event.key === 'Tab' && filterACOpen()) {
        event.preventDefault();
        acceptFilterAC(false);
        return;
    }
    if (event.key === 'Escape' && filterACOpen()) {
        event.preventDefault();
        closeFilterAC();
        return;
    }
    if (event.key === 'ArrowDown' && filterACOpen()) {
        event.preventDefault();
        moveFilterACActive(1);
        return;
    }
    if (event.key === 'ArrowUp' && filterACOpen()) {
        event.preventDefault();
        moveFilterACActive(-1);
        return;
    }
    if (event.key === 'Backspace' && input.value === '') {
        event.preventDefault();
        popLastFilterChip();
    }
}

// A click anywhere outside the editor dismisses the dropdown (esc-equivalent).
document.addEventListener('click', (event) => {
    if (!event.target.closest('#ro-filter-field')) {
        closeFilterAC();
    }
});

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
