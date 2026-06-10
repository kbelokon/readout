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

    // ⌘K palette: a click on a result row activates that row (navigates to its
    // server-built href or runs its named action, then closes). Matched before
    // everything else so a click inside the open palette never falls through to a
    // page handler. The row carries no <a> -- navigation goes through the dataset
    // href in choosePaletteRow (defends against a javascript: scheme).
    const paletteItem = target.closest('.ro-pal-item');
    if (paletteItem) {
        event.preventDefault();
        choosePaletteRow(paletteItem);
        return;
    }
    // The read-only topbar search box (data-palette-open) opens the palette on
    // click, instead of typing inline. (Keyboard focus is handled in focusin.)
    const paletteOpener = target.closest('[data-palette-open]');
    if (paletteOpener) {
        event.preventDefault();
        openPalette();
        return;
    }
    // A click on the palette backdrop ITSELF (the dimmed area outside the panel)
    // closes it, like Esc. A click inside the panel does not match (the panel is a
    // descendant, so target.closest stops at the panel, not the backdrop root).
    if (target.id === PALETTE_ID) {
        closePalette();
        return;
    }
    // Stale-banner retry: re-fire the (read-only) auto-refresh GET on
    // #resource-list-content through the shared refresh path (the v2 loop
    // derives the `_table` URL from location.href at click time; the v1
    // multi-type container triggers its baked ro:refresh). On success the morph
    // swaps fresh rows and the afterSwap handler clears the stale dim +
    // re-hides the banner; on another failure the responseError handler keeps
    // it stale. Pure DOM, GET-only -- the read-only floor is untouched.
    const staleRetry = target.closest('.ro-stale-retry');
    if (staleRetry) {
        event.preventDefault();
        requestListRefresh();
        return;
    }
    // Mobile hamburger: a delegated click on `.menu-toggle` reveals/hides the
    // sidebar by toggling `.is-active` on `.ro-sidebar` (the <760px reveal CSS +
    // the button itself are owned by Unit 15; this is the JS half of D11). No-op
    // when no sidebar is present (e.g. the Clusters entry page).
    const menuToggle = target.closest('.menu-toggle');
    if (menuToggle) {
        event.preventDefault();
        const sidebar = document.querySelector('.ro-sidebar');
        if (sidebar) {
            sidebar.classList.toggle('is-active');
        }
        return;
    }

    // Auto-refresh interval option (navbar #refresh-dropdown): store the chosen
    // interval client-side, re-arm the poll, and reflect it in the control. The
    // dropdown opens through CSS hover/focus, so there is no open/close handler
    // here -- only the selection.
    const refreshOption = target.closest('.refresh-option');
    if (refreshOption) {
        try {
            window.localStorage.setItem(REFRESH_KEY, refreshOption.dataset.interval);
        } catch (e) {
            // localStorage unavailable -> the choice just will not persist
        }
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

    // .ro-fold-toggle (NESTED YAML block fold): toggle the deeper-indented child
    // lines of a `key:`/`- key:` block in place. The toggle is a <button>
    // injected by buildYamlFolds() at load INTO the opener line's span, carrying
    // `data-fold="<lineId>"`; every child line span carries `data-fold-of` listing
    // the opener ids it nests under, so ONE delegated handler toggles the matching
    // children's `ro-line-folded` class (CSS display:none) and flips the opener's
    // `is-folded` + aria-expanded. The raw child lines stay in the DOM (only hidden),
    // so the per-section copy still yields the full YAML (it clones + strips the
    // injected fold controls before reading text -- see below). Presentation only;
    // no hash sync, no server. Matched BEFORE the section-fold + gutter-anchor
    // handlers so a nested-fold click never collapses the whole section or jumps a
    // line anchor. event.target.closest covers a click on the caret pseudo too.
    const foldToggle = target.closest('.ro-fold-toggle');
    if (foldToggle) {
        event.preventDefault();
        event.stopPropagation();
        toggleYamlFold(foldToggle);
        return;
    }

    // .ro-copy-btn (per-section YAML copy): copy THIS section's raw YAML to the
    // clipboard via navigator.clipboard.writeText -- CSP-clean (no inline handler,
    // no eval). The raw text is read from the section's Pygments `td.code` cell:
    // with linenos="table" the line-number gutter lives in a SEPARATE `td.linenos`
    // column, so textContent of `td.code` is exactly the source YAML (indentation
    // + newlines preserved, no gutter digits) -- no duplicated hidden payload. When
    // the nested-fold controls have been injected into the code cell, they are
    // STRIPPED from a shallow clone first (yamlCodeText) so the copied text is the
    // raw YAML regardless of fold state (folded child lines stay in the DOM, only
    // hidden, so their text is still copied). The button briefly flips its label to
    // "copied". Matched BEFORE the section-fold handler and returns, so a copy click
    // never toggles the section fold.
    const copyBtn = target.closest('.ro-copy-btn');
    if (copyBtn) {
        event.preventDefault();
        const section = copyBtn.closest('.collapsible');
        const codeCell = section && section.querySelector('.highlighttable td.code');
        const text = codeCell ? yamlCodeText(codeCell) : '';
        const label = copyBtn.querySelector('.ro-copy-text');
        const done = (ok) => {
            if (!label) {
                return;
            }
            label.textContent = ok ? 'copied' : 'press ⌘C';
            window.setTimeout(() => { label.textContent = 'copy'; }, 1500);
        };
        if (navigator.clipboard && navigator.clipboard.writeText && text) {
            navigator.clipboard.writeText(text).then(() => done(true), () => done(false));
        } else {
            done(false);
        }
        return;
    }

    // .collapsible h4.title: toggle `is-collapsed` on the section and sync the
    // URL fragment (collapsed=<names>) with all currently-collapsed sections. The
    // section is resolved via closest('.collapsible') (NOT parentElement): in a
    // Unit-10 YAML card the h4.title is nested inside .ro-card-head, so
    // parentElement is that head, not the [data-name] .collapsible card -- which
    // left the card fold toggling is-collapsed on the wrong node (no visual fold,
    // and a bogus empty `collapsed=` hash). closest() walks up to the actual
    // collapsible; for the bare Pods/Events collapsibles (h4.title is a direct
    // child) it resolves to the SAME element parentElement did, so their fold is
    // unchanged. This finds its section the same way the copy handler above does.
    const collapsibleTitle = target.closest('main .collapsible h4.title');
    if (collapsibleTitle) {
        const section = collapsibleTitle.closest('.collapsible');
        section.classList.toggle('is-collapsed');
        const names = [];
        document.querySelectorAll('main .is-collapsed').forEach((el) => {
            names.push(el.dataset.name);
        });
        if (names.length) {
            document.location.hash = `collapsed=${names.join(',')}`;
        } else {
            window.history.replaceState(null, '', window.location.pathname + window.location.search);
        }
        return;
    }

    // YAML line-number anchors (.linenos a): set the URL hash to the clicked
    // line, re-highlight, and suppress the default anchor jump.
    const lineNumber = target.closest('.linenos a');
    if (lineNumber) {
        location.hash = `#${lineNumber.href.split('#')[1]}`;
        highlightYamlLine();
        event.preventDefault();
        return;
    }

    // #namespace-dropdown .context-trigger: toggle `is-active`; focus the searchbox when opening.
    const nsTrigger = target.closest('#namespace-dropdown .context-trigger');
    if (nsTrigger) {
        const nsDropdown = nsTrigger.closest('#namespace-dropdown');
        nsDropdown.classList.toggle('is-active');
        if (nsDropdown.classList.contains('is-active')) {
            const searchbox = document.getElementById('namespace-searchbox');
            if (searchbox) {
                searchbox.focus();
            }
        }
        return;
    }
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
    }
});

// ---------------------------------------------------------------------------
// Delegated INPUT handlers
// ---------------------------------------------------------------------------
document.addEventListener('input', (event) => {
    // ⌘K palette query box: re-render the grouped rows filtered by a
    // case-insensitive substring of the label, re-seating the active row.
    const paletteInput = event.target.closest('#ro-palette-input');
    if (paletteInput) {
        renderPalette(paletteInput.value);
        return;
    }

    // #namespace-searchbox: filter the .namespace-item links by case-insensitive substring.
    const searchbox = event.target.closest('#namespace-searchbox');
    if (searchbox) {
        const filterText = searchbox.value.toLowerCase();
        document.querySelectorAll('.namespace-item').forEach((element) => {
            if ((element.innerText || '').toLowerCase().indexOf(filterText) === -1) {
                element.classList.add('is-hidden');
            } else {
                element.classList.remove('is-hidden');
            }
        });
    }
});

// ---------------------------------------------------------------------------
// Delegated KEYUP handlers
// ---------------------------------------------------------------------------
document.addEventListener('keyup', (event) => {
    // #namespace-searchbox: Enter selects the first still-visible match.
    const searchbox = event.target.closest('#namespace-searchbox');
    if (searchbox) {
        if (event.key !== 'Enter') {
            return;
        }
        const elements = document.querySelectorAll('.namespace-item');
        for (let i = 0; i < elements.length; i++) {
            if (!elements[i].classList.contains('is-hidden')) {
                elements[i].click();
                break;
            }
        }
    }
});

// ---------------------------------------------------------------------------
// Delegated KEYDOWN handlers (⌘K / Ctrl-K palette open + in-palette navigation)
// ---------------------------------------------------------------------------
// keydown (not keyup) so we can preventDefault BEFORE the browser acts on the
// chord (e.g. Firefox's quick-find on a bare key, or a stray default for
// Ctrl/Cmd-K). One delegated listener on document covers both opening the
// palette from anywhere and driving it once open; it survives hx-boost swaps
// because document is never replaced. CSP-clean: pure DOM, no eval/inline.
document.addEventListener('keydown', (event) => {
    // Open on Meta+K (mac ⌘K) OR Ctrl+K (the decorative navbar <kbd>⌘K</kbd> is
    // the advertised hook). Ignore when a modifier combo also carries Alt/Shift
    // so we never hijack an unrelated browser/OS shortcut.
    if ((event.metaKey || event.ctrlKey) && !event.altKey && !event.shiftKey
        && (event.key === 'k' || event.key === 'K')) {
        event.preventDefault();
        openPalette();
        return;
    }
    // Everything else here only matters while the palette is open. The redesign
    // overlay reveals via the `open` class on the backdrop root (opacity +
    // pointer-events), not the old is-active/is-hidden pair.
    const palette = document.getElementById(PALETTE_ID);
    if (!palette || !palette.classList.contains('open')) {
        return;
    }
    if (event.key === 'Escape') {
        event.preventDefault();
        closePalette();
        return;
    }
    if (event.key === 'ArrowDown') {
        event.preventDefault();
        movePaletteActive(1);
        return;
    }
    if (event.key === 'ArrowUp') {
        event.preventDefault();
        movePaletteActive(-1);
        return;
    }
    if (event.key === 'Enter') {
        // Activate the currently-highlighted target (GET via its dataset href, or
        // its named client action). No-op when no row is active.
        event.preventDefault();
        activatePaletteSelection();
        return;
    }
    if (event.key === 'Tab') {
        // Trap focus inside the panel: with one text input + the (non-focusable)
        // rows, steer Tab/Shift-Tab through the visible rows via the same
        // active-row model the arrows use, so focus can never escape to the page
        // behind the modal.
        event.preventDefault();
        movePaletteActive(event.shiftKey ? -1 : 1);
        return;
    }
});

// The read-only topbar search box also opens the palette on keyboard FOCUS
// (Tab-into / programmatic focus): focusin bubbles to document, so one delegated
// listener covers it without a per-element handler that an hx-boost swap would
// drop. We immediately blur the box so the caret never lands in the inert input
// and hand focus to the palette's own query box via openPalette().
document.addEventListener('focusin', (event) => {
    const opener = event.target.closest('[data-palette-open]');
    if (opener) {
        if (typeof event.target.blur === 'function') {
            event.target.blur();
        }
        openPalette();
    }
});

// ---------------------------------------------------------------------------
// Delegated SUBMIT handlers
// ---------------------------------------------------------------------------
document.addEventListener('submit', (event) => {
    // form.tools-form: blank the `name` of empty inputs so they do not become
    // empty query parameters in the resulting GET URL.
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

// Highlight the YAML line named by location.hash (#<id>): clear prior
// highlights, then add the highlight class to `#yaml-<id>` and scroll it into
// view. Toggling a class (vs el.style.background) keeps the colour in CSS.
function highlightYamlLine() {
    const fragment = location.hash;
    if (!fragment) {
        return;
    }
    document.querySelectorAll('pre > span.yaml-line-highlight').forEach((el) => {
        el.classList.remove('yaml-line-highlight');
    });
    const element = document.getElementById(`yaml-${fragment.substring(1)}`);
    if (element) {
        element.classList.add('yaml-line-highlight');
        element.scrollIntoView({ block: 'center' });
    }
}

// On load, collapse every section named in the URL fragment (collapsed=a,b,c).
// Idempotent: adding `is-collapsed` to an already-collapsed section is a no-op.
function collapseSectionsFromHash() {
    const hash = document.location.hash;
    if (!hash) {
        return;
    }
    hash.substring(1).split(';').forEach((param) => {
        const keyVal = param.split('=');
        if (keyVal[0] === 'collapsed' && keyVal[1]) {
            keyVal[1].split(',').forEach((name) => {
                document
                    .querySelectorAll(`main .collapsible[data-name="${CSS.escape(name)}"]`)
                    .forEach((el) => {
                        el.classList.add('is-collapsed');
                    });
            });
        }
    });
}

// ---------------------------------------------------------------------------
// Nested-YAML-block folding -- CSP-clean, no build, graceful.
// ---------------------------------------------------------------------------
// Over the EXISTING Pygments `linenos="table"` output (a `table.highlighttable`
// whose `td.code > pre` holds one `<span id="yaml-...-line-N">` per source line),
// we compute each line's indentation at load and inject a fold toggle before any
// line that OPENS a nested block (a `key:` / `- key:` whose following lines indent
// deeper). Clicking the toggle hides that block's deeper-indented child lines and
// shows a `{ N lines }` summary -- the `containers: > { ... }` affordance.
//
// Everything is DOM-only (document.createElement, classList, dataset) -- no eval,
// no innerHTML-with-handlers, no inline style. The whole pass is wrapped in
// try/catch and bails cleanly (leaving the plain highlighted block) on anything
// unexpected, so a weird CRD object can never break the page -- it just does not
// get nested folds. The line anchors (`yaml-...-line-N` ids, the `.linenos a`
// gutter), the section-level fold, and the per-section copy are all untouched:
// child lines are HIDDEN (display:none), never removed, and copy strips the
// injected controls from a clone before reading the raw text.

// YAML indent semantics: leading spaces give the depth, but a YAML block-sequence
// item ("- ...") sits at the SAME visual indent as its parent key, so we count a
// leading "- " (or a bare "-") as one extra level (+2). This makes "- name: x"
// nest under "containers:" exactly as the object structure does.
function yamlEffectiveIndent(text) {
    const stripped = text.replace(/^\n+/, '');
    let i = 0;
    while (i < stripped.length && stripped[i] === ' ') {
        i++;
    }
    const rest = stripped.slice(i);
    if (rest === '-' || rest.startsWith('- ') || rest.startsWith('-\t')) {
        return i + 2;
    }
    return i;
}

// The raw YAML text of a Pygments `td.code` cell, with any injected fold controls
// removed. Folded child lines stay in the DOM (only hidden), so this is the FULL
// source YAML in any fold state -- the per-section copy stays correct. Clones the
// cell (cheap, code-only) so the live DOM is untouched, then drops the toggle +
// summary nodes before reading textContent.
function yamlCodeText(codeCell) {
    if (!codeCell.querySelector('.ro-fold-toggle, .ro-fold-summary')) {
        return codeCell.textContent; // no folds injected -> raw text already clean
    }
    const clone = codeCell.cloneNode(true);
    clone.querySelectorAll('.ro-fold-toggle, .ro-fold-summary').forEach((el) => {
        el.remove();
    });
    return clone.textContent;
}

// Toggle one nested block: flip the opener's `is-folded` + aria-expanded and
// hide/show every child line that lists this opener's id in `data-fold-of`.
function toggleYamlFold(toggle) {
    const id = toggle.dataset.fold;
    if (!id) {
        return;
    }
    const pre = toggle.closest('pre');
    if (!pre) {
        return;
    }
    const folded = !toggle.classList.contains('is-folded');
    toggle.classList.toggle('is-folded', folded);
    toggle.setAttribute('aria-expanded', folded ? 'false' : 'true');
    pre.querySelectorAll('[data-fold-of]').forEach((line) => {
        const owners = line.dataset.foldOf.split(' ');
        if (owners.indexOf(id) !== -1) {
            line.classList.toggle('ro-line-folded', folded);
        }
    });
}

// Build the nested folds for every YAML code block on the page. Idempotent: a cell
// already processed carries `data-ro-folds`, so an hx-boost re-init never doubles
// the controls. Fully guarded -- any error leaves that cell as a plain highlighted
// block (the accepted graceful fallback to the section-level fold).
function buildYamlFolds() {
    document.querySelectorAll('.highlighttable td.code pre').forEach((pre) => {
        if (pre.dataset.roFolds) {
            return; // already processed (idempotent across re-inits)
        }
        try {
            // The per-line spans Pygments emits (linespans="yaml-...-line"). Direct
            // children of <pre>, in source order; their textContent preserves exact
            // indentation + the trailing newline (the empty <a> anchor adds nothing).
            const lines = Array.prototype.filter.call(
                pre.children,
                (el) => el.tagName === 'SPAN' && el.id && el.id.indexOf('line-') !== -1
            );
            pre.dataset.roFolds = '1'; // mark BEFORE work so a throw still bails once
            if (lines.length < 3) {
                return; // too small to have a meaningful nested block
            }
            const indents = lines.map((el) => yamlEffectiveIndent(el.textContent));
            const isBlank = lines.map((el) => el.textContent.trim() === '');

            for (let i = 0; i < lines.length; i++) {
                if (isBlank[i]) {
                    continue;
                }
                // next non-blank line
                let j = i + 1;
                while (j < lines.length && isBlank[j]) {
                    j++;
                }
                if (j >= lines.length || indents[j] <= indents[i]) {
                    continue; // not an opener (no deeper-indented body follows)
                }
                // body = contiguous following lines indented deeper than the opener
                let end = i + 1;
                let bodyCount = 0;
                let itemCount = 0;
                while (end < lines.length) {
                    if (isBlank[end]) {
                        end++;
                        continue;
                    }
                    if (indents[end] > indents[i]) {
                        const t = lines[end].textContent.replace(/^\s+/, '');
                        // a direct sequence item of THIS block (one level deeper,
                        // list indicator) -> counts toward the "N items" summary
                        if (
                            indents[end] === indents[i] + 2 &&
                            (t === '-' || t.startsWith('- ') || t.startsWith('-\t'))
                        ) {
                            itemCount++;
                        }
                        lines[end].dataset.foldOf = lines[end].dataset.foldOf
                            ? `${lines[end].dataset.foldOf} ${lines[i].id}`
                            : lines[i].id;
                        bodyCount++;
                        end++;
                    } else {
                        break;
                    }
                }
                if (bodyCount === 0) {
                    continue;
                }
                injectFoldControls(lines[i], bodyCount, itemCount);
            }
        } catch (e) {
            // Anything unexpected -> leave this block plainly highlighted (the
            // accepted graceful fallback). The cell is already marked, so we do not
            // retry it; the section-level fold + line anchors keep working.
        }
    });
}

// Inject the fold toggle + collapsed summary into an opener line span. The toggle
// is a real <button> (keyboard-focusable, CSP-clean) placed right after the line's
// leading <a> anchor so the caret sits at the start of the line; the summary is a
// hidden <span> appended at the line's end, shown by CSS only when folded. Both
// carry a class the copy path strips, so neither pollutes the copied raw YAML.
function injectFoldControls(lineSpan, bodyCount, itemCount) {
    const toggle = document.createElement('button');
    toggle.type = 'button';
    toggle.className = 'ro-fold-toggle';
    toggle.setAttribute('aria-expanded', 'true');
    toggle.setAttribute('aria-label', 'Toggle block');
    toggle.dataset.fold = lineSpan.id;

    const summary = document.createElement('span');
    summary.className = 'ro-fold-summary';
    const lineWord = bodyCount === 1 ? 'line' : 'lines';
    if (itemCount > 0) {
        const itemWord = itemCount === 1 ? 'item' : 'items';
        summary.textContent = ` { ${itemCount} ${itemWord} · ${bodyCount} ${lineWord} }`;
    } else {
        summary.textContent = ` { ${bodyCount} ${lineWord} }`;
    }

    // Place the toggle after the leading anchor (so it reads at the line start,
    // left of the key); fall back to prepend if no anchor is present.
    const anchor = lineSpan.querySelector('a');
    if (anchor && anchor.nextSibling) {
        lineSpan.insertBefore(toggle, anchor.nextSibling);
    } else if (anchor) {
        lineSpan.appendChild(toggle);
    } else {
        lineSpan.insertBefore(toggle, lineSpan.firstChild);
    }
    // Append the summary at the very end of the line content, BEFORE the trailing
    // newline text node so the collapsed summary renders on the opener's own line.
    const last = lineSpan.lastChild;
    if (last && last.nodeType === 3 && last.textContent.indexOf('\n') !== -1) {
        lineSpan.insertBefore(summary, last);
    } else {
        lineSpan.appendChild(summary);
    }
}

// ---------------------------------------------------------------------------
// ⌘K jump-to command palette -- data-driven, grouped, CSP-clean, GET-only (D10).
// ---------------------------------------------------------------------------
// A keyboard launcher that JUMPS to navigation targets. It owns NO live DOM
// harvest: it reads the server-built JSON blob in #ro-palette-data (emitted by
// the layout from the same context the sidebar/navbar already have) and builds
// grouped rows -- Clusters / Namespaces / Resource types / Actions. Selecting a
// row navigates to its server-built absolute href (a plain GET permalink, never
// the POST theme form, so the read-only floor is untouched) or runs a named
// client action (e.g. theme). The blob is parsed with JSON.parse (NEVER eval);
// names are written via textContent (defence in depth against a hostile
// cluster/namespace/CRD name) and the ONLY field set via innerHTML is the
// server-escaped kind `icon` markup. The overlay reveals via the `open` class on
// the backdrop root (opacity + pointer-events). Pure DOM -> no dynamic-code
// execution, no inline handler -> CSP-clean.
const PALETTE_ID = 'ro-palette';

// The render order + display titles of the four palette groups, keyed to the
// blob fields. Empty groups are skipped at render time.
const PALETTE_GROUPS = [
    { title: 'Clusters', key: 'clusters' },
    { title: 'Namespaces', key: 'namespaces' },
    { title: 'Resource types', key: 'kinds' },
    { title: 'Actions', key: 'actions' },
];

// Parse the #ro-palette-data JSON blob into the grouped feed. Guarded end to
// end: a missing/empty/malformed blob yields an all-empty feed (the palette still
// opens with a "no targets" state) and NEVER throws. We re-read on every open so
// an hx-boost navigation that swapped the blob is picked up. JSON.parse only --
// never eval -- so the blob can carry arbitrary cluster/namespace/CRD names
// safely.
function readPaletteData() {
    const empty = { currentCluster: null, currentNamespace: null,
        clusters: [], namespaces: [], kinds: [], actions: [] };
    const el = document.getElementById('ro-palette-data');
    if (!el) {
        return empty;
    }
    const raw = (el.textContent || '').trim();
    if (!raw) {
        return empty;
    }
    try {
        const data = JSON.parse(raw);
        if (!data || typeof data !== 'object') {
            return empty;
        }
        // Normalise: every group is an array even if the blob omitted/nulled it.
        ['clusters', 'namespaces', 'kinds', 'actions'].forEach((k) => {
            if (!Array.isArray(data[k])) {
                data[k] = [];
            }
        });
        return data;
    } catch (e) {
        return empty; // malformed blob -> empty palette, no throw
    }
}

// A jump target's destination href is ONLY ever read from the server-built blob
// (never user-typed), but as defence in depth we still refuse anything that is
// not a same-origin path / http(s) URL before navigating -- a javascript:,
// data:, or vbscript: scheme is never navigated.
function paletteHrefSafe(href) {
    if (!href || typeof href !== 'string') {
        return '';
    }
    const trimmed = href.trim();
    // A scheme-relative or absolute URL with a non-http(s) scheme is rejected;
    // a path (starting "/") or an http(s) URL is allowed.
    if (/^[a-z][a-z0-9+.-]*:/i.test(trimmed) && !/^https?:/i.test(trimmed)) {
        return '';
    }
    return trimmed;
}

// The flat list of currently-rendered rows ({ el, item }) in visual order, and
// the index of the active one -- the model the arrows + Enter drive.
let paletteRows = [];
let paletteActive = 0;

// Build one row element for a blob entry in group `key`. Names go in via
// textContent; the kind `icon` (server-escaped markup) is the ONLY innerHTML.
// The destination (href) and optional client action are stashed in the dataset,
// read back by choosePaletteRow -- navigation never touches innerHTML.
function buildPaletteRow(entry, key) {
    const row = document.createElement('div');
    row.className = 'ro-pal-item';
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');

    // Resource types carry a server-rendered icon (already a `<span class="ico
    // sm">…</span>` string, HTML-escaped by the server). This is the SOLE field
    // assigned via innerHTML; all other groups lead with the label (no icon). We
    // parse the markup in a throwaway container and move its nodes in, so the
    // `.ico` span becomes a DIRECT child of the row (the `.ro-pal-item .ico` flex
    // sizing applies) rather than nesting under an extra wrapper.
    if (key === 'kinds' && entry.icon) {
        const holder = document.createElement('template');
        holder.innerHTML = entry.icon; // server-escaped markup -- the only innerHTML
        row.appendChild(holder.content);
    }

    // The visible label: kinds use `kind`, every other group uses `name`/`label`.
    const labelText = key === 'kinds'
        ? (entry.kind || entry.plural || '')
        : (entry.name || entry.label || '');
    const label = document.createElement('span');
    label.className = 'pal-label';
    label.textContent = labelText; // textContent -> a hostile name cannot inject

    // The "current" scope marker (the cluster/namespace in scope) rides as a
    // .pal-ctx chip after the label, also via textContent.
    const isCurrent = (key === 'clusters' && entry.name && entry.name === paletteScope.cluster)
        || (key === 'namespaces' && entry.name && entry.name === paletteScope.namespace);
    if (isCurrent) {
        const ctx = document.createElement('span');
        ctx.className = 'pal-ctx';
        ctx.textContent = 'current';
        label.appendChild(ctx);
    }
    row.appendChild(label);

    // Resource-type rows show the api group (faint) + a compact namespaced/cluster
    // scope badge, so a kind reads as e.g. "Certificates  cert-manager.io  NS".
    if (key === 'kinds') {
        const meta = document.createElement('span');
        meta.className = 'pal-meta';
        meta.textContent = entry.group || 'core'; // textContent -> hostile group cannot inject
        row.appendChild(meta);
        const scope = document.createElement('span');
        scope.className = 'pal-scope ' + (entry.namespaced ? 'ns' : 'cluster');
        scope.textContent = entry.namespaced ? 'NS' : 'CL';
        scope.title = entry.namespaced ? 'namespaced' : 'cluster-scoped';
        row.appendChild(scope);
    }

    // Destination: a navigable href (server-built absolute path) and/or a named
    // client action. Stored in the dataset; the click/Enter path reads it back.
    const href = paletteHrefSafe(entry.href);
    if (href) {
        row.dataset.href = href;
    }
    if (entry.action) {
        row.dataset.action = entry.action;
    }
    return row;
}

// The current scope (cluster/namespace) of the page, set by readPaletteData via
// renderPalette so buildPaletteRow can flag the in-scope rows.
const paletteScope = { cluster: null, namespace: null };

// harvestPageObjects reads the rows of the rendered list table (desktop
// `.ro-table`, not the mobile card projection) into {name, href, status, tone}
// so the palette can filter the objects ALREADY on the page -- no server call.
// The status (+ tone) comes from the row's `.cell-status` when the kind has one
// (pods, namespaces, ...); kinds with no status cell just yield an empty status.
function harvestPageObjects() {
    const out = [];
    const rows = document.querySelectorAll('#resource-list-content table.ro-table tbody tr');
    rows.forEach((tr) => {
        const a = tr.querySelector('td.cell-name a');
        if (!a) {
            return;
        }
        const href = a.getAttribute('href');
        const name = (a.textContent || '').trim();
        if (!href || !name) {
            return;
        }
        let status = '';
        let tone = '';
        const st = tr.querySelector('.cell-status');
        if (st) {
            status = (st.textContent || '').trim();
            ['ok', 'warn', 'err', 'info', 'mute'].forEach((t) => {
                if (!tone && st.classList.contains(t)) {
                    tone = t;
                }
            });
        }
        out.push({ name: name, href: href, status: status, tone: tone });
    });
    return out;
}

// buildObjectRow renders one harvested page object: its name (textContent, never
// innerHTML) + a tone-coloured short status. The detail href rides in the dataset
// like every other palette row, so choosePaletteRow navigates it identically.
function buildObjectRow(o) {
    const row = document.createElement('div');
    row.className = 'ro-pal-item';
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');
    const label = document.createElement('span');
    label.className = 'pal-label';
    label.textContent = o.name;
    row.appendChild(label);
    if (o.status) {
        const st = document.createElement('span');
        st.className = 'pal-status' + (o.tone ? ' ' + o.tone : '');
        st.textContent = o.status;
        row.appendChild(st);
    }
    row.dataset.href = o.href;
    return row;
}

// (Re)render the grouped rows into #ro-palette-list, filtered by a
// case-insensitive substring of the label. Empty groups (and groups with no
// match) are skipped; when nothing matches at all we show a "no targets" line so
// the palette never looks broken. Rebuilds paletteRows + seats the active row.
function renderPalette(query) {
    const list = document.getElementById('ro-palette-list');
    if (!list) {
        return;
    }
    const data = readPaletteData();
    paletteScope.cluster = data.currentCluster || null;
    paletteScope.namespace = data.currentNamespace || null;

    // Reflect the scope chip in the search row (textContent -- never innerHTML).
    const scope = document.getElementById('ro-palette-scope');
    if (scope) {
        const scopeText = paletteScope.namespace || paletteScope.cluster || '';
        scope.textContent = scopeText;
        scope.hidden = scopeText === '';
    }

    const q = (query || '').toLowerCase().trim();
    list.textContent = '';
    paletteRows = [];

    const appendGroup = (title, rows) => {
        if (rows.length === 0) {
            return;
        }
        const heading = document.createElement('div');
        heading.className = 'ro-pal-group';
        heading.textContent = title;
        list.appendChild(heading);
        rows.forEach((entry) => {
            const row = entry.el;
            const idx = paletteRows.length;
            row.addEventListener('mousemove', () => setPaletteActive(idx));
            list.appendChild(row);
            paletteRows.push({ el: row, item: entry.item, key: entry.key });
        });
    };

    // Objects on THIS list page, harvested from the rendered table so ⌘K filters
    // the very rows you are looking at (with a short status), no server round-trip.
    // First group -- the most relevant target on a list page.
    const pageObjects = harvestPageObjects().filter((o) => !q || o.name.toLowerCase().indexOf(q) !== -1);
    appendGroup('On this page', pageObjects.map((o) => ({ el: buildObjectRow(o), item: o, key: 'objects' })));

    PALETTE_GROUPS.forEach((group) => {
        const entries = (data[group.key] || []).filter((entry) => {
            if (!q) {
                return true;
            }
            const label = group.key === 'kinds'
                ? (entry.kind || entry.plural || '')
                : (entry.name || entry.label || '');
            return label.toLowerCase().indexOf(q) !== -1;
        });
        appendGroup(group.title, entries.map((entry) => ({ el: buildPaletteRow(entry, group.key), item: entry, key: group.key })));
    });

    if (paletteRows.length === 0) {
        const none = document.createElement('div');
        none.className = 'ro-pal-empty';
        none.textContent = 'No matching targets.';
        list.appendChild(none);
    }
    paletteActive = 0;
    paintPaletteActive();
}

// Paint exactly the active row with `.active` (+ aria-selected) and scroll it
// into view; a no-op when the list is empty.
function paintPaletteActive() {
    paletteRows.forEach((r, i) => {
        const on = i === paletteActive;
        r.el.classList.toggle('active', on);
        r.el.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    if (paletteRows[paletteActive]) {
        paletteRows[paletteActive].el.scrollIntoView({ block: 'nearest' });
    }
}

// Seat the active row at a clamped index (guards empty + out-of-range).
function setPaletteActive(index) {
    if (paletteRows.length === 0) {
        return;
    }
    let i = index;
    if (i < 0) { i = 0; }
    if (i > paletteRows.length - 1) { i = paletteRows.length - 1; }
    paletteActive = i;
    paintPaletteActive();
}

// Move the active row by delta, wrapping at the ends (ArrowDown past the last
// lands on the first, ArrowUp past the first lands on the last). Guards empty.
function movePaletteActive(delta) {
    if (paletteRows.length === 0) {
        return;
    }
    paletteActive = (paletteActive + delta + paletteRows.length) % paletteRows.length;
    paintPaletteActive();
}

// Act on a chosen row: run its named client action (only `theme` is wired today,
// clicking the server-POST theme toggle) and/or navigate to its server-built
// href, then close. Navigation reads ONLY dataset.href (a vetted same-origin
// path) -- never innerHTML, never a javascript: scheme.
function choosePaletteRow(rowEl) {
    if (!rowEl) {
        return;
    }
    const action = rowEl.dataset.action;
    const href = rowEl.dataset.href;
    closePalette();
    if (action === 'theme') {
        const toggle = document.getElementById('btn-theme-toggle');
        if (toggle) {
            toggle.click(); // the server POST /preferences toggle (read-only-safe)
        }
        return;
    }
    if (href) {
        window.location.assign(href); // plain GET to a server permalink
    }
}

// Activate the currently-highlighted row (Enter). No-op when no row is active.
function activatePaletteSelection() {
    const active = paletteRows[paletteActive];
    if (active) {
        choosePaletteRow(active.el);
    }
}

// Remember what had focus before the palette opened so Esc/close can restore it
// (keyboard users land back where they were instead of on <body>).
let palettePriorFocus = null;

// Open the palette: reveal the overlay (the `open` class -- never inline style),
// build the grouped rows from the blob, clear + focus the query box, and seat the
// first row active. Idempotent: re-opening just rebuilds from the (possibly
// hx-boost-swapped) blob.
function openPalette() {
    const palette = document.getElementById(PALETTE_ID);
    const input = document.getElementById('ro-palette-input');
    if (!palette || !input) {
        return; // overlay not present (defensive) -> no-op
    }
    palettePriorFocus = document.activeElement;
    palette.classList.add('open');
    palette.setAttribute('aria-hidden', 'false');
    input.value = '';
    renderPalette('');
    input.focus(); // focus after it is shown so the caret lands in the box
}

// Close the palette: drop the `open` class and restore focus to wherever it was
// before opening (if that element is still in the document).
function closePalette() {
    const palette = document.getElementById(PALETTE_ID);
    if (!palette) {
        return;
    }
    palette.classList.remove('open');
    palette.setAttribute('aria-hidden', 'true');
    if (palettePriorFocus && document.contains(palettePriorFocus)
        && typeof palettePriorFocus.focus === 'function') {
        palettePriorFocus.focus();
    }
    palettePriorFocus = null;
}

// ---------------------------------------------------------------------------
// Auto-refresh interval (live table morph-refresh)
// ---------------------------------------------------------------------------
// OFF by default -- the page is static. The user picks an interval in the navbar
// (#refresh-dropdown: Off / 5 / 15 / 30 / 60s); the choice is a CLIENT preference
// in localStorage (no server write -- the read-only floor stays intact, and it
// persists across navigation). When an interval is set and a resource-list page
// is showing (the #resource-list-content container exists), the tick re-fetches
// the table fragment so it morphs in place.
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
const REFRESH_KEY = 'roRefresh';
let refreshTimerId = null;

// userListRequestsInFlight counts USER-initiated requests targeting
// #resource-list-content (requests from any element other than the container
// itself, e.g. a sort-header hx-get). While > 0 the refresh tick is suppressed.
// Preload warm-ups (HX-Preloaded) are excluded: the preload extension hijacks
// the XHR callbacks, so htmx:afterRequest never fires for them and counting one
// would suppress ticks forever.
let userListRequestsInFlight = 0;

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
    if (!isUserListRequest(event)) {
        return;
    }
    userListRequestsInFlight++;
    // The user action wins: abort the container's own in-flight request (a
    // tick that started before the click). htmx aborts the request belonging
    // to the element htmx:abort is triggered on -- the user's request lives on
    // its own element and is untouched. Inert when the container is idle.
    const content = document.getElementById('resource-list-content');
    if (content && typeof htmx !== 'undefined') {
        htmx.trigger(content, 'htmx:abort');
    }
});

// htmx:afterRequest fires on load, error, abort, AND timeout, so the in-flight
// count always returns to zero.
document.addEventListener('htmx:afterRequest', (event) => {
    if (isUserListRequest(event)) {
        userListRequestsInFlight = Math.max(0, userListRequestsInFlight - 1);
    }
});

function refreshInterval() {
    try {
        return parseInt(window.localStorage.getItem(REFRESH_KEY) || '0', 10) || 0;
    } catch (e) {
        return 0; // localStorage unavailable (e.g. privacy mode) -> stay static
    }
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

function fireRefresh() {
    if (document.hidden) {
        return; // paused while the tab is in the background
    }
    if (userListRequestsInFlight > 0) {
        return; // a user-initiated table request is in flight -- never stomp it
    }
    requestListRefresh();
}

// (Re)arm the interval from the stored preference. Idempotent: clears any prior
// timer first, so hx-boost body swaps and repeated init passes never stack timers.
function applyRefresh() {
    if (refreshTimerId !== null) {
        window.clearInterval(refreshTimerId);
        refreshTimerId = null;
    }
    const secs = refreshInterval();
    if (secs > 0) {
        refreshTimerId = window.setInterval(fireRefresh, secs * 1000);
    }
}

// Reflect the stored preference in the navbar control (label + active option +
// an "on" class for the spinning-icon styling). Re-run on every init pass because
// an hx-boost swap re-renders the navbar.
function syncRefreshUI() {
    const secs = refreshInterval();
    const label = document.getElementById('refresh-label');
    if (label) {
        label.textContent = secs > 0 ? `${secs}s` : 'Off';
    }
    document.querySelectorAll('.refresh-option').forEach((opt) => {
        opt.classList.toggle('is-active', parseInt(opt.dataset.interval, 10) === secs);
    });
    const dropdown = document.getElementById('refresh-dropdown');
    if (dropdown) {
        dropdown.classList.toggle('refresh-on', secs > 0);
    }
}

// Refresh once immediately when returning to a backgrounded tab, so stale data
// updates right away instead of waiting up to a full interval.
document.addEventListener('visibilitychange', () => {
    if (!document.hidden && refreshInterval() > 0) {
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
}

// A non-2xx reply to the refresh GET: keep the rows (htmx does not swap on
// error), dim them, reveal the stale banner.
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        markListStale();
    }
});
// A transport failure (the cluster could not be reached at all) on the refresh
// GET: same stale treatment -- the last-good rows stay, dimmed, with the banner.
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
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
        clearListStale();
        reapplyRowState();
    }
});

// ---------------------------------------------------------------------------
// Identity-keyed row state (D6): selection + j/k focus survive every morph
// ---------------------------------------------------------------------------
// Single-type list rows carry data-key="cluster/ns/name" (and an id derived
// from it, which idiomorph uses to match rows by OBJECT identity, never by
// position). Row-level client state lives here, keyed by that identity:
//   - rowSelection: the multi-select set (the bulk-bar feed, Unit 16)
//   - rowFocusKey:  the single j/k keyboard-focus row (gesture lands in Unit 16)
// A morph syncs the server's class attribute over any client-added class (the
// cell-flash WeakMap machinery proved this), so the classes are RE-APPLIED from
// this store on htmx:afterSwap above. Because the keys are object identities,
// a re-sorted or filtered fragment re-decorates the SAME objects wherever their
// rows land. window.roRowState is the deliberate seam the selection gesture
// (Unit 16) and the e2e suite drive; everything is pure DOM classList writes
// (CSP-clean, read-only floor untouched).
const rowSelection = new Set();
let rowFocusKey = null;

function reapplyRowState() {
    const content = document.getElementById('resource-list-content');
    if (!content) {
        return;
    }
    content.querySelectorAll('tr[data-key]').forEach((tr) => {
        tr.classList.toggle('is-selected', rowSelection.has(tr.dataset.key));
        tr.classList.toggle('kfocus', tr.dataset.key === rowFocusKey);
    });
}

window.roRowState = {
    setSelected(key, on) {
        if (on) {
            rowSelection.add(key);
        } else {
            rowSelection.delete(key);
        }
        reapplyRowState();
    },
    setFocus(key) {
        rowFocusKey = key || null;
        reapplyRowState();
    },
    clear() {
        rowSelection.clear();
        rowFocusKey = null;
        reapplyRowState();
    },
    selectedKeys() {
        return Array.from(rowSelection);
    },
};

// ---------------------------------------------------------------------------
// Theme-toggle POST target (prefers-aware, cookieless-safe)
// ---------------------------------------------------------------------------
// The navbar theme toggle POSTs /preferences with a hidden `theme` value that
// must flip the EFFECTIVE palette. With an explicit choice (a theme cookie or
// ?theme=) the server already renders the correct opposite value AND pins
// data-theme on <html>, so we leave it alone (data-theme-explicit="true").
//
// With NO explicit choice (data-theme-explicit="false") the palette is driven
// by prefers-color-scheme, NOT the server theme.name -- under the dark default
// a cookieless light-OS user is theme.name="dark" server-side (so the server
// pre-fills theme="light") while their real palette is LIGHT, which would make
// the first click a no-op. So we derive the value here from the effective
// palette: post the OPPOSITE of matchMedia('(prefers-color-scheme: dark)'). The
// matching icon is chosen purely in CSS (both glyphs render); this only fixes
// the POST target, which CSS cannot reach. Pure CSP-clean DOM writes (no eval,
// no inline handler).
const PREFERS_DARK = window.matchMedia('(prefers-color-scheme: dark)');

function syncThemeTogglePostTarget() {
    const toggle = document.getElementById('btn-theme-toggle');
    if (!toggle) {
        return;
    }
    // Explicit choice -> the server value is authoritative; never override it.
    if (toggle.dataset.themeExplicit !== 'false') {
        return;
    }
    const input = toggle.form && toggle.form.querySelector('input[name="theme"]');
    if (input) {
        // Effective palette is dark -> the toggle should switch to light, and
        // vice versa (post the opposite of the current effective scheme).
        input.value = PREFERS_DARK.matches ? 'light' : 'dark';
    }
}

// Re-derive the cookieless toggle target if the OS scheme changes while the page
// is open (so the no-cookie toggle keeps matching the live effective palette).
// Attached ONCE at module load -- not inside runInit -- so hx-boost re-init never
// stacks duplicate listeners. addEventListener is the modern matchMedia API
// (addListener is deprecated); the listener body is idempotent.
PREFERS_DARK.addEventListener('change', syncThemeTogglePostTarget);

// _all-view sticky offset. CSS pins the FIRST column at left:0; in the _all view
// the first column is the namespace, so the NAME column (2nd) must pin right after
// it -- but its offset is the namespace column's content-driven width, which CSS
// can't know. Measure it, hand it to CSS as --ns-col-w, and mark the table with
// .ro-sticky2. A single-namespace list (name IS the first column) needs neither.
// Idempotent; re-run on swap and resize since the column width can change.
function setupStickyNamespace() {
    document.querySelectorAll('.ro-table-wrap table.ro-table').forEach((table) => {
        const firstCell = table.querySelector('tbody tr td:first-child');
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
        applyRefresh,
        buildYamlFolds,
        collapseSectionsFromHash,
        highlightYamlLine,
        syncThemeTogglePostTarget,
        setupStickyNamespace,
        // Row state is keyed by OBJECT identity and survives boosted body swaps
        // (script state persists); re-paint it on every init pass so a return
        // to a list re-decorates the same objects immediately, not only on the
        // next morph. Lifecycle policy (when to clear) lands with the selection
        // gesture in Unit 16.
        reapplyRowState,
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
