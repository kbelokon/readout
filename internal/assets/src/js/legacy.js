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
// Auto-refresh interval (live table morph-refresh)
// ---------------------------------------------------------------------------
// OFF by default -- the page is static. The user picks an interval in the navbar
// (#refresh-dropdown: Off / 5 / 10 / 30 / 60s, D18); the choice persists in the
// ro_prefs cookie (D9 -- written by THIS script, no server write route: the
// read-only floor stays intact; the server merely renders the persisted state
// into the topbar at SSR). The legacy v1 home, the `roRefresh` localStorage
// key, survives only as refreshMode()'s read-once migration fallback. When an
// interval is set and a resource-list page is showing (the
// #resource-list-content container exists), the tick re-fetches the table
// fragment so it morphs in place.
//
// TWO container contracts (D1/D6):
//   - v2 single-type pages mark the container data-live-url="location" and bake
//     NO request URL: the tick (and every programmatic re-fetch) derives the
//     `_table` URL from location.href AT FIRE TIME, so a sort/filter the user
//     pushed into history is never reverted by a later tick (the old baked
//     PartialURL contract did exactly that). The request is issued with the
//     container as its htmx source, keeping its hx-ext="ro-morph" +
//     hx-swap="morph" + hx-indicator wiring.
//   - v1 multi-type pages keep the baked hx-get + ro:refresh trigger.
//
// Ticks are PROGRAMMATIC: htmx:configRequest marks every request issued by the
// container itself with RO-No-Push, so the `_table` handler never answers them
// with HX-Push-Url (htmx pushes one history entry per header occurrence with no
// same-URL dedupe -- an unconditional push would turn a 5s interval into junk
// history per tick). The timer is also SUPPRESSED while a user-initiated table
// request (a sort-header hx-get) is in flight, and a user action aborts an
// in-flight tick -- never the other way around (bare hx-sync would let a tick
// cancel the user's request). Polling PAUSES while the tab is hidden (no
// background API hammering), and refreshes once immediately on return.
// REFRESH_KEY (imported from prefs.ts) is the LEGACY v1 localStorage home of
// the interval choice. It is never written anymore -- refreshMode() reads it
// once as the migration fallback into the ro_prefs cookie (D9).
// THE pending tick timer -- a setTimeout CHAIN, not setInterval: the wait
// before the next tick depends on the failure backoff stage (SPEC §8.3), so
// every tick / failure / recovery re-derives it.
let refreshTimerId = null;
// Epoch ms of the next armed tick (0 = none) -- the stale banner's live
// "Retrying in Ns" countdown reads it.
let refreshNextAt = 0;
// Consecutive failed list-refresh attempts since the last success; 0 =
// healthy. Stage 1 retries at the base cadence (1x), stage 2 at 2x, stage 3
// (where it stays) at 4x, the wait capped at 60s; the first success resets it.
let refreshFailureStage = 0;

// userListRequestsInFlight tracks USER-initiated requests targeting
// #resource-list-content (requests from any element other than the container
// itself, e.g. a sort-header hx-get) by their XHR objects. While one is
// unsettled the refresh tick is suppressed.
//
// A Set of xhrs, deliberately NOT a counter: htmx dispatches htmx:afterRequest
// on the ISSUING element, and when a boosted navigation's body swap detaches
// that element mid-request the event cannot bubble to the document (htmx never
// aborts the in-flight xhrs of removed elements either) -- a counter would
// stick >= 1 forever, leaving fireRefresh dead until a hard reload. The xhrs
// themselves know when they settled, so the tick gate prunes settled entries
// by readyState instead of trusting the event to arrive.
//
// Preload warm-ups (HX-Preloaded) are excluded: the preload extension hijacks
// the XHR callbacks (htmx:afterRequest never fires for them) and a warm-up
// never swaps the table, so the tick gate must not wait on one.
const userListRequestsInFlight = new Set();

// containerListRequestsInFlight tracks the requests the container ITSELF
// issues (the refresh tick, the stale retry, commitColumnVisibility's
// re-render) the same xhr-Set way. fireRefresh skips while one is unsettled:
// issuing a second container request while the first is in flight would make
// htmx QUEUE it (the sync-less default queues "last" per element), and a
// queued tick replays SYNCHRONOUSLY on the next htmx:abort -- carrying its
// stale queue-time URL -- racing the very user request whose arrival triggered
// that abort. If the stale response lands last, the table reverts until the
// next tick. The fix is upstream: no queue may ever form.
const containerListRequestsInFlight = new Set();

// A settled xhr is DONE (4: the load/error/timeout callbacks ran) or UNSENT
// (0: aborted -- the XHR abort steps reset readyState to 0 after firing).
// Either way htmx is no longer waiting on it, so the entry is reclaimable
// even when its htmx:afterRequest was dispatched on a detached element.
function pruneSettledListRequests(requests) {
    requests.forEach((xhr) => {
        if (xhr.readyState === 4 || xhr.readyState === 0) {
            requests.delete(xhr);
        }
    });
}

function isPreloadRequest(event) {
    const cfg = event.detail && event.detail.requestConfig;
    return !!cfg && !!cfg.headers && cfg.headers['HX-Preloaded'] === 'true';
}

// True for a USER-initiated request that will swap #resource-list-content: the
// target is the container but the issuing element is something else (the tick,
// the stale retry, and every other programmatic re-fetch are issued BY the
// container, so they do not match).
function isUserListRequest(event) {
    const detail = event && event.detail;
    if (!detail || !detail.elt || !detail.target) {
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
    const elt = event.detail && event.detail.elt;
    if (elt && elt.id === 'resource-list-content') {
        event.detail.headers['RO-No-Push'] = 'true';
    }
});

document.addEventListener('htmx:beforeRequest', (event) => {
    const detail = event.detail;
    if (detail && detail.xhr && detail.elt && detail.elt.id === 'resource-list-content') {
        containerListRequestsInFlight.add(detail.xhr);
        return; // container-issued (tick/retry/programmatic) -- tracked, never aborts anything
    }
    if (!isUserListRequest(event)) {
        return;
    }
    if (detail && detail.xhr) {
        userListRequestsInFlight.add(detail.xhr);
    }
    // The user action wins: abort the container's own in-flight request (a
    // tick that started before the click). htmx aborts the request belonging
    // to the element htmx:abort is triggered on -- the user's request lives on
    // its own element and is untouched. Inert when the container is idle, and
    // the container can never hold a QUEUED request for this abort to replay:
    // fireRefresh refuses to issue while one is already in flight.
    const content = document.getElementById('resource-list-content');
    if (content && typeof htmx !== 'undefined') {
        htmx.trigger(content, 'htmx:abort');
    }
});

// htmx:afterRequest fires on load, error, abort, AND timeout. When it reaches
// the document the entry is removed here; when it does not (htmx dispatched it
// on an element a boosted swap already detached, so it could not bubble), the
// readyState pruning in fireRefresh reclaims the entry instead.
document.addEventListener('htmx:afterRequest', (event) => {
    const xhr = event.detail && event.detail.xhr;
    if (xhr) {
        userListRequestsInFlight.delete(xhr);
        containerListRequestsInFlight.delete(xhr);
    }
});

// refreshMode returns the persisted auto-refresh mode ('Off', an interval in
// seconds as a string, the future 'Live'; '' = no preference) from the
// ro_prefs cookie. The legacy `roRefresh` localStorage key migrates here ONCE
// (D9 -- the migration is OWNED by this unit; Unit 21 verifies, never
// re-implements): it is a read-once fallback consulted only while the cookie
// carries no refresh value, written through to the cookie immediately, after
// which the cookie is canonical.
function refreshMode() {
    const stored = readPrefs().refresh;
    if (stored) {
        return stored;
    }
    let legacy = null;
    try {
        legacy = window.localStorage.getItem(REFRESH_KEY);
    } catch (e) {
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

function refreshInterval() {
    const secs = parseInt(refreshMode(), 10);
    return Number.isFinite(secs) && secs > 0 ? secs : 0; // 'Off'/'Live'/junk -> 0
}

// listTableURL derives the `_table` partial URL from the LIVE document URL at
// fire time (path + "/_table" + the current query) -- the D6 replacement for
// the render-time-baked PartialURL contract. location reflects every canonical
// URL the sort/filter loop pushed, so the tick always re-fetches what the user
// is looking at.
function listTableURL() {
    const u = new URL(window.location.href);
    return u.pathname.replace(/\/+$/, '') + '/_table' + u.search;
}

// requestListRefresh re-fetches the list fragment through the container's own
// htmx wiring: the v2 path issues a GET to the location-derived `_table` URL
// with the container as source (so its morph ext / target / indicator apply
// and configRequest marks it RO-No-Push); the v1 multi-type path triggers the
// element's baked ro:refresh request.
function requestListRefresh() {
    const content = document.getElementById('resource-list-content');
    if (!content || typeof htmx === 'undefined') {
        return;
    }
    if (content.dataset.liveUrl === 'location') {
        const request = htmx.ajax('GET', listTableURL(), { source: content });
        if (request && typeof request.catch === 'function') {
            // A transport failure rejects the htmx.ajax promise; the failure is
            // already handled via htmx:sendError (the stale dim + banner), so
            // swallow the rejection instead of spamming unhandled-rejection logs
            // once per failed tick.
            request.catch(() => {});
        }
    } else {
        htmx.trigger(content, 'ro:refresh');
    }
}
// IIFE-compat seam (strangler): the original no-build classic script ran in the
// global scope, so every top-level `function` declaration was implicitly a
// `window.*` global. Once esbuild wraps this file in an IIFE that implicit
// binding is gone. The e2e suite drives requestListRefresh through window (the
// production refresh path), so re-expose it explicitly -- the same convention
// the designed seams below already use (window.roFuzzy / window.roRowState).
// When this file is DISMANTLED into modules, this seam moves to the module that
// owns requestListRefresh; it is the one line added on top of the byte copy.
window.requestListRefresh = requestListRefresh;

function fireRefresh() {
    if (document.hidden) {
        return; // paused while the tab is in the background
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    if (userListRequestsInFlight.size > 0) {
        return; // a user-initiated table request is in flight -- never stomp it
    }
    if (containerListRequestsInFlight.size > 0) {
        // The previous container request has not settled (a response slower
        // than the interval). Issuing another would QUEUE it inside htmx, and
        // a queued tick replays on the next htmx:abort with its stale URL --
        // skip this tick entirely; the next one re-checks.
        return;
    }
    requestListRefresh();
}

// effectivePollSeconds is the poll cadence the tick chain actually arms:
// the chosen interval, or the Live mode's 5s FALLBACK cadence while the
// stream is degraded to polling (Unit 27/D19 taxonomy: terminal / 429 / 204 /
// unsupported page). Plain 'Live' with a riding stream stays 0 -- enabling
// Live stops the polling timer; the chain self-disarms.
function effectivePollSeconds() {
    const secs = refreshInterval();
    if (secs > 0) {
        return secs;
    }
    return refreshMode() === 'Live' ? liveFallbackSecs : 0;
}

// refreshDelaySeconds is the wait until the NEXT tick: the effective cadence
// while healthy (stage 0) and after the FIRST failure (stage 1 retries at the
// base cadence, 1x), then 2x / 4x of it for repeated failures, the backoff
// wait capped at 60s (SPEC §8.3: 1x -> 2x -> 4x, cap 60s, reset on success).
function refreshDelaySeconds() {
    const secs = effectivePollSeconds();
    if (secs <= 0) {
        return 0;
    }
    if (refreshFailureStage <= 1) {
        return secs;
    }
    const factor = refreshFailureStage === 2 ? 2 : 4;
    return Math.min(secs * factor, 60);
}

// scheduleRefreshTick (re)arms THE single pending tick refreshDelaySeconds()
// from NOW. Idempotent: any prior timer is cleared first, so init passes,
// interval picks, failures, and recoveries all converge on one armed timer
// (hx-boost body swaps can never stack timers, exactly like the old
// setInterval contract). A fired tick re-schedules BEFORE issuing its request
// so a skipped fire (hidden tab, in-flight gate) never kills the chain; a
// failure/recovery handler then re-arms again with the escalated/reset wait.
function scheduleRefreshTick() {
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

// (Re)arm the poll from the stored preference. Runs on every init pass (a
// fresh full-page render is by definition not stale) and on an interval pick
// (a deliberate cadence choice) -- both end any failure backoff: the next
// failure escalates from scratch.
function applyRefresh() {
    refreshFailureStage = 0;
    scheduleRefreshTick();
}

// Reflect the stored preference in the navbar control (label + active option +
// an "on" class for the spinning-icon/livedot styling). Live (Unit 27) labels
// "Live", activates ONLY the Live option (parseInt('Live') is NaN -> 0, which
// would otherwise light Off), and pulses the livedot through the same
// refresh-on hook in EVERY Live substate (stream riding or polling fallback)
// -- the mode is "on" either way; the server's refreshDropdownClass renders
// the identical state at SSR. Re-run on every init pass because an hx-boost
// swap re-renders the navbar.
function syncRefreshUI() {
    const live = refreshMode() === 'Live';
    const secs = refreshInterval();
    const label = document.getElementById('refresh-label');
    if (label) {
        label.textContent = live ? 'Live' : (secs > 0 ? `${secs}s` : 'Off');
    }
    document.querySelectorAll('.refresh-option').forEach((opt) => {
        const value = opt.dataset.interval;
        opt.classList.toggle('is-active', live
            ? value === 'Live'
            : value !== 'Live' && (parseInt(value, 10) || 0) === secs);
    });
    const dropdown = document.getElementById('refresh-dropdown');
    if (dropdown) {
        dropdown.classList.toggle('refresh-on', live || secs > 0);
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

// ---------------------------------------------------------------------------
// Stale data (auto-refresh failure) -- CLIENT-SIDE, never blanks the rows (D11)
// ---------------------------------------------------------------------------
// There is no server-side last-good cache. "Stale" is purely the AUTO-REFRESH
// failure case: when the #resource-list-content morph-refresh request errors
// (htmx:responseError = a non-2xx reply, htmx:sendError = a transport failure),
// htmx does NOT swap on error, so the existing rows stay exactly as they were.
// We mark the content stale (a dim class) and reveal the pre-rendered hidden
// `.ro-banner.warn` so the user knows the data is last-known, not current. A
// FIRST load that fails never reaches here (that is a full page/server response
// rendering forbidden/unreachable/empty, not a ro:refresh on existing rows). On
// the next successful refresh the morph swaps fresh rows and afterSwap clears the
// stale state. Pure DOM writes -> CSP-clean.
const STALE_DIM_CLASS = 'ro-stale';

// The 1s ticker repainting the live "Retrying in Ns" countdown while the
// stale banner is visible (started by markListStale, stopped by
// clearListStale -- the banner and its countdown share a lifetime).
let staleCountdownId = null;

// updateStaleCountdown paints seconds-to-next-retry into the banner's
// [data-stale-countdown] span. The span is re-queried on every paint (never
// cached -- the banner is chrome outside the swap target, but cheap lookups
// keep this safe against any re-render). With no retry armed (interval Off;
// the banner can still reveal when a user-initiated table request fails) the
// shipped "…" placeholder is restored -- Retry now stays the affordance.
function updateStaleCountdown() {
    const span = document.querySelector('.ro-stale-banner [data-stale-countdown]');
    if (!span) {
        return;
    }
    if (!refreshNextAt) {
        span.textContent = '…';
        return;
    }
    const remaining = Math.max(0, Math.ceil((refreshNextAt - Date.now()) / 1000));
    span.textContent = remaining + 's';
}

// noteRefreshFailure escalates the backoff one stage (1x -> 2x -> 4x, where
// it stays) and re-arms the pending tick at the escalated wait, measured from
// the failure itself -- so the banner's countdown always aims at the real
// next attempt. Every failed list fetch escalates: the scheduled tick, the
// Retry-now re-fire, a failed user sort -- each was a real failed attempt.
function noteRefreshFailure() {
    refreshFailureStage = Math.min(refreshFailureStage + 1, 3);
    scheduleRefreshTick();
}

// noteRefreshRecovery: the FIRST successful swap after >=1 failures resets
// the backoff to the base cadence and announces it -- "refresh resumed" is
// the SECOND sanctioned toast trigger (D24/SPEC §8.8). Plain successes
// (stage 0) stay silent: the toast is recovery-only, never per-tick.
function noteRefreshRecovery() {
    if (refreshFailureStage === 0) {
        return;
    }
    refreshFailureStage = 0;
    scheduleRefreshTick();
    if (typeof window.roToast === 'function') {
        window.roToast('Refresh resumed');
    }
}

// True when the htmx event belongs to a request that lands in the live
// resource-list region: issued BY #resource-list-content (the refresh tick /
// retry) or TARGETING it (a user sort/filter partial in the v2 loop). Guards
// so an unrelated boosted navigation error never dims the table. Preload
// warm-ups never swap, so they are excluded.
function isListRefreshEvent(event) {
    const detail = event && event.detail;
    if (!detail || isPreloadRequest(event)) {
        return false;
    }
    const elt = detail.elt;
    if (!!elt && elt.id === 'resource-list-content') {
        return true;
    }
    const target = detail.target;
    return !!target && target.id === 'resource-list-content';
}

function markListStale() {
    const content = document.getElementById('resource-list-content');
    if (content) {
        content.classList.add(STALE_DIM_CLASS);
    }
    const banner = document.querySelector('.ro-stale-banner');
    if (banner) {
        banner.hidden = false;
    }
    // Live countdown for the banner's "Retrying in Ns" (Unit 21 wiring of the
    // data-stale-countdown hook). The immediate paint lands the right number
    // before the ticker's first 1s beat.
    if (staleCountdownId === null) {
        staleCountdownId = window.setInterval(updateStaleCountdown, 1000);
    }
    updateStaleCountdown();
}

function clearListStale() {
    const content = document.getElementById('resource-list-content');
    if (content) {
        content.classList.remove(STALE_DIM_CLASS);
    }
    const banner = document.querySelector('.ro-stale-banner');
    if (banner) {
        banner.hidden = true;
    }
    if (staleCountdownId !== null) {
        window.clearInterval(staleCountdownId);
        staleCountdownId = null;
    }
}

// A non-2xx reply to the refresh GET: keep the rows (htmx does not swap on
// error), dim them, reveal the stale banner. The failure note FIRST: it
// re-aims the retry schedule, so the banner reveals with the countdown
// already pointing at the real next attempt.
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshFailure();
        markListStale();
    }
});
// A transport failure (the cluster could not be reached at all) on the refresh
// GET: same stale treatment -- the last-good rows stay, dimmed, with the banner.
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshFailure();
        markListStale();
    }
});
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
// Loading skeleton (D16 / SPEC §7.19) -- shown ONLY into an EMPTY swap target
// ---------------------------------------------------------------------------
// Every full page is server-rendered with rows already in place, and the morph
// refresh keeps the last-good rows, so the only valid skeleton moment is a
// partial request landing in a BLANK #resource-list-content (the first paint
// of a partial into an empty region). A POPULATED region NEVER gets a skeleton
// over its content (the data-never-disappears law); boosted full-page
// navigations keep the global #ro-progress sweep instead. The rows are cloned
// from the inert server-baked #ro-skel-template sibling (schema-mirroring
// column widths, bottom fade) -- pure DOM, CSP-clean.

// True when the list region is a BLANK region: zero element children. The
// probe is element-count, not a selector list, because ANY existing content is
// something the skeleton clone (replaceChildren) would wipe -- a selector
// denylist ('.ro-table, .ro-empty-lg') once misclassified a banner-only region
// (the all-cluster partial-failure banner with no table) as empty and the
// clone destroyed the only visible diagnostic.
function listRegionIsEmpty(content) {
    return content.childElementCount === 0;
}

document.addEventListener('htmx:beforeRequest', (event) => {
    if (!isListRefreshEvent(event)) {
        return;
    }
    const content = document.getElementById('resource-list-content');
    const template = document.getElementById('ro-skel-template');
    if (!content || !template || !listRegionIsEmpty(content)) {
        return;
    }
    content.replaceChildren(
        ...Array.from(template.children, (node) => node.cloneNode(true))
    );
});

// A failed request into a skeleton-only region removes the skeleton (htmx does
// not swap on error), so the region returns to empty instead of shimmering
// forever. Last-good rows are never involved: the skeleton only ever existed
// in a region that had none.
function clearListSkeleton() {
    const content = document.getElementById('resource-list-content');
    const skel = content && content.querySelector(':scope > .ro-skel');
    if (skel) {
        skel.remove();
    }
}
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        clearListSkeleton();
    }
});
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
        clearListSkeleton();
    }
});

// ---------------------------------------------------------------------------
// Live mode (Unit 27 / D19, client half) -- a fetch-based SSE bridge.
// ---------------------------------------------------------------------------
// 'Live' is the 6th refresh-dropdown mode: instead of polling, the client
// opens the read-only `GET …/{plural}/_stream` SSE endpoint and morphs every
// pushed `_table` fragment through the SAME ro-morph pipeline the polling
// ticks ride -- htmx.swap routes the 'morph' swap style through the
// extension's handleSwap (row-model capture, windowing adoption, idiomorph
// cell flash) and dispatches htmx:afterSwap on the container, so the standard
// post-swap repairs (stale clear, row state, live filter, re-window) run
// untouched; the event carries a roLivePush marker so a push never triggers
// the param-change reopen below.
//
// Native EventSource is deliberately NOT used: EventSource cannot observe the
// non-200 connect statuses the D19 wire contract assigns meaning to (204
// watch-less, 429 stream cap -- it only ever surfaces a generic error), and
// its built-in auto-reconnect fights the close-reason taxonomy (`ro-terminal`
// must close WITHOUT reconnecting). The vendored htmx SSE extension is
// equally unsuitable: it swaps every message sight-unseen, but stale frames
// must be discarded BEFORE morphing. A streaming fetch plus a ~20-line SSE
// line parser gives full control with no new dependency, and the strict CSP
// is untouched (fetch falls under connect-src, covered by default-src
// 'self').
//
// Generation discard (D19): the CLIENT mints a generation, sends it as the
// stream's `?g=` query param, and checks the echo in every frame AT MORPH
// TIME. A frame with a stale generation, or one arriving while any `_table`
// request is in flight (user sort/filter OR a container re-render -- both
// would race the push's older render), is DISCARDED, never deferred: every
// push is a full snapshot, so the next one carries everything a dropped one
// did.
//
// Close-reason taxonomy (D19):
//   - `ro-terminal` (idle / auth / watch-failed / shutdown): close WITHOUT
//     reconnecting -> 5s polling + the stale banner (the first successful
//     poll clears it, exactly like every other stale episode).
//   - 204 on connect (watch-less kind) and 429 on connect (stream cap):
//     SILENT 5s polling, no banner. Other connect failures degrade the same
//     silent way -- if polling then fails too, the standard failure machinery
//     raises the honest banner itself.
//   - document.hidden: close the stream; returning visibility reopens ONLY
//     after such a hidden-close -- never after a terminal/429/204 fallback
//     (sticky until a fresh page init or an explicit dropdown re-pick) and
//     never under user-selected polling.
//   - Any `f`/`sort`/columns change tears the stream down and reopens it with
//     the new query under a fresh generation (liveOnListSwap, hooked on the
//     container afterSwap -- each such change lands as exactly one container
//     request swap, cookie-only column changes included).
//
// Multi-type and multi-cluster pages render the Live option DISABLED (server
// truth, mirroring the `_stream` 404 scope); liveSupported() consults that
// rendered option plus the v2 container marker, so a persisted 'Live' on such
// a page silently degrades to 5s polling instead of dialing a dead endpoint.
const liveState = {
    status: 'idle', // 'idle' | 'connecting' | 'open' | 'fallback' | 'hidden'
    abort: null,    // AbortController of the current stream fetch
    gen: '',        // the minted generation every frame must echo (string compare)
    streamPath: '', // the stream URL sans ?g= -- the page/params identity
};
let liveGenSeq = 0;
// liveDiscards counts ro-table frames DISCARDED at morph time (stale
// generation, wrong page identity, in-flight `_table` request). Exposed via
// the window.roLive debug seam (the roVirtual pattern) so the e2e suite can
// await "the push arrived AND was discarded" deterministically instead of
// sleeping past an estimated delivery time.
let liveDiscards = 0;
// The Live FALLBACK poll cadence (seconds): 0 while a stream rides (or Live
// is off), 5 while degraded to polling. effectivePollSeconds() feeds it into
// the shared tick chain.
let liveFallbackSecs = 0;

// liveSupported: can THIS page stream? The v2 single-type container must be
// present (data-live-url="location" -- the D1/D6 loop marker) and the
// server-rendered Live option must not be disabled (the D19 scope cut:
// multi-type/multi-cluster pages 404 the endpoint). Server truth drives the
// client; no URL parsing here.
function liveSupported() {
    const content = document.getElementById('resource-list-content');
    if (!content || content.dataset.liveUrl !== 'location') {
        return false;
    }
    const option = document.querySelector('.refresh-option[data-interval="Live"]');
    return !!option && !option.disabled;
}

// liveStreamBase derives the stream URL from the LIVE document URL at open
// time (the listTableURL pattern): path + "/_stream" + the RAW query -- raw
// string concat, never a URLSearchParams round-trip, so an `f` chip's
// wire-significant raw OR-commas survive byte-exactly.
function liveStreamBase() {
    const u = new URL(window.location.href);
    return u.pathname.replace(/\/+$/, '') + '/_stream' + u.search;
}

// liveTeardown aborts the current stream fetch (if any). The per-stream
// AbortController doubles as the supersession token: every async resumption
// in liveConnect checks `liveState.abort === ctrl` and goes inert when a
// newer stream (or a teardown) replaced it.
function liveTeardown() {
    const ctrl = liveState.abort;
    liveState.abort = null;
    if (ctrl) {
        try {
            ctrl.abort();
        } catch (e) {
            // already settled -- nothing to abort
        }
    }
}

// liveEngageFallback degrades Live to 5s polling: silently (204/429/connect
// failure) or with the stale banner (terminal, transport drop). The banner
// rides the SAME markListStale machinery every stale episode uses -- the
// armed 5s tick shows in its countdown, and the first successful poll clears
// it via the standard afterSwap recovery. Without a list container (Live
// persisted on a detail page) the cadence stays 0: there is nothing to poll.
function liveEngageFallback(banner) {
    liveTeardown();
    liveState.status = 'fallback';
    liveFallbackSecs = document.getElementById('resource-list-content') ? 5 : 0;
    scheduleRefreshTick();
    if (banner) {
        markListStale();
    }
}

// liveOpen tears down any current stream and opens a fresh one against
// `base` (the stream URL sans generation). An empty base means "this page
// cannot stream": silent polling fallback. Minting the generation HERE --
// one per open -- is what makes the morph-time echo check sufficient: a
// frame from any superseded stream can never carry the current value.
function liveOpen(base) {
    liveTeardown();
    liveFallbackSecs = 0;
    liveState.streamPath = base;
    if (!base) {
        liveEngageFallback(false);
        return;
    }
    liveState.status = 'connecting';
    liveGenSeq += 1;
    liveState.gen = Date.now().toString(36) + '.' + liveGenSeq;
    const ctrl = new AbortController();
    liveState.abort = ctrl;
    const url = base + (base.indexOf('?') === -1 ? '?' : '&')
        + 'g=' + encodeURIComponent(liveState.gen);
    scheduleRefreshTick(); // effective cadence is 0 now -> the poll chain disarms
    liveConnect(url, ctrl);
}

// liveConnect is the transport core: a streaming fetch + an SSE line parser.
// Frames are `event: <name>` + `data: <one JSON line>` + a blank line (the
// Unit 26 wire contract JSON-escapes newlines, so multi-line data: only
// exists for spec-defensive completeness). Every await resumption re-checks
// the supersession token; all exits funnel into the taxonomy above.
async function liveConnect(url, ctrl) {
    let res = null;
    try {
        res = await fetch(url, { signal: ctrl.signal });
    } catch (e) {
        if (liveState.abort !== ctrl) {
            return; // superseded/torn down -- our own abort
        }
        liveEngageFallback(false); // could not connect: silent polling
        return;
    }
    if (liveState.abort !== ctrl) {
        return;
    }
    if (res.status !== 200 || !res.body) {
        // 204 watch-less / 429 cap / anything unexpected: silent 5s polling.
        liveEngageFallback(false);
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
    } catch (e) {
        if (liveState.abort !== ctrl) {
            return; // our own abort surfaced as a read error
        }
        liveEngageFallback(true); // transport drop mid-stream: banner + polling
        return;
    }
    if (liveState.abort !== ctrl) {
        return;
    }
    // The server closed without a terminal frame (its graceful paths always
    // send one): treat it like a terminal -- banner + 5s polling.
    liveEngageFallback(true);
}

// liveHandleFrame dispatches one parsed SSE frame. Returns true when the
// stream must stop reading (a terminal was handled). THE morph-time gates
// live here: the generation echo and the in-flight `_table` check both run at
// dispatch -- which IS morph time, synchronously before htmx.swap -- so a
// stale or racing push is dropped whole, never queued.
function liveHandleFrame(name, text, ctrl) {
    if (liveState.abort !== ctrl || text === '') {
        return false;
    }
    let payload = null;
    try {
        payload = JSON.parse(text);
    } catch (e) {
        return false; // malformed frame -> skipped (the next push is a full snapshot)
    }
    if (!payload || typeof payload !== 'object') {
        return false;
    }
    if (name === 'ro-terminal') {
        // idle / auth / watch-failed / shutdown: close WITHOUT reconnecting.
        liveEngageFallback(true);
        return true;
    }
    if (name !== 'ro-table') {
        return false;
    }
    if (String(payload.g) !== liveState.gen) {
        liveDiscards += 1;
        return false; // STALE GENERATION -> discarded at morph time
    }
    if (liveStreamBase() !== liveState.streamPath) {
        // WRONG PAGE: the location no longer matches the page/params identity
        // this stream was opened against. A boosted body swap pushes the new
        // URL BEFORE htmx:load's liveApply reconciles the stream, so a push
        // from the old page's still-open stream would otherwise morph the OLD
        // resource's table into the NEW page's container. The body-swap hook
        // tears the stream down structurally; this gate is the independent
        // morph-time layer.
        liveDiscards += 1;
        return false;
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    if (userListRequestsInFlight.size > 0 || containerListRequestsInFlight.size > 0) {
        liveDiscards += 1;
        return false; // a _table request is in flight -> the push is discarded
    }
    liveMorph(String(payload.html));
    return false;
}

// liveMorph swaps one pushed fragment into the list container through the
// htmx swap pipeline with the 'morph' style: htmx resolves the container's
// hx-ext="ro-morph" extension, whose handleSwap runs the EXACT tick path
// (captureRowModel -> virtualizePrepareSwap -> Idiomorph with the explicit
// config), then htmx dispatches htmx:afterSwap on the container -- the
// standard post-swap pipeline (clearListStale, reapplyRowState,
// applyLiveNameFilter, virtualizeAfterSwap) runs through the existing
// listener. eventInfo carries target (so isListRefreshEvent matches; htmx
// overwrites detail.elt with the dispatch element, which is the container
// too) and the roLivePush marker (so the reopen hook skips pushes).
function liveMorph(html) {
    const content = document.getElementById('resource-list-content');
    if (!content || typeof htmx === 'undefined' || typeof htmx.swap !== 'function') {
        return;
    }
    htmx.swap(content, html, { swapStyle: 'morph' }, {
        contextElement: content,
        eventInfo: { target: content, roLivePush: true },
    });
}

// liveOnListSwap is the param-change reopen (called from the container
// afterSwap pipeline): while a stream rides, ANY request swap of the
// container is a query/cookie change (`f`/sort via the URL, column
// visibility via the prefs cookie), so the stream reopens against the new
// state under a fresh generation. The new query is taken from the REQUEST
// path (byte-exact, raw f= commas included) rather than location, which may
// not have received the canonical push yet at afterSwap time. In FALLBACK
// (terminal/429/204) nothing reopens -- polling swaps land here too, and the
// taxonomy pins the fallback sticky.
function liveOnListSwap(event) {
    const detail = event && event.detail;
    if (detail && detail.roLivePush) {
        return; // a push is not a param change
    }
    if (liveState.status !== 'open' && liveState.status !== 'connecting') {
        return;
    }
    let base = liveStreamBase();
    const pathInfo = detail && detail.pathInfo;
    const requestPath = pathInfo && (pathInfo.finalRequestPath || pathInfo.requestPath);
    if (requestPath && requestPath.indexOf('/_table') !== -1) {
        base = requestPath.replace('/_table', '/_stream');
    }
    liveOpen(base);
}

// liveApply is the init-time (and dropdown-pick) reconciliation: open, keep,
// degrade, or tear down the stream per the persisted mode and THIS page's
// capability. Idempotent across the htmx:load re-inits every swap fires --
// with an unchanged page/params identity the standing state is respected
// (a riding stream keeps riding; a fallback stays sticky; a hidden-close
// waits for the visibility handler). `force` (the explicit dropdown pick)
// always reopens: a deliberate re-pick of Live is the sanctioned way to
// re-attempt after a fallback without leaving the page.
function liveApply(force) {
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

// Visibility close/reopen (D19): hiding the tab closes a riding stream (no
// background streaming, matching the polling pause); returning reopens ONLY
// after such a hidden-close. A terminal/429/204 fallback ('fallback') and
// user-selected polling ('idle') never reopen here -- the catch-up poll on
// the shared visibilitychange handler above covers those.
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
// pattern: morph-time discard observability. The specs poll discards() to
// prove a held push ARRIVED and was dropped (not merely "has not arrived
// yet") before asserting the view stayed unchanged.
window.roLive = {
    discards() {
        return liveDiscards;
    },
};

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
        liveTeardown();
        liveState.status = 'idle';
        liveState.streamPath = '';
        liveFallbackSecs = 0;
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
