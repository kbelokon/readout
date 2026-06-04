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
// Delegated CLICK handlers
// ---------------------------------------------------------------------------
document.addEventListener('click', (event) => {
    const target = event.target;

    // ⌘K palette: a click on a result row navigates to that target's href (a
    // plain GET to an existing permalink -- the anchor's default href click,
    // which we DO NOT preventDefault, carries the navigation). We just close the
    // overlay so it does not linger over the new page (under hx-boost the click
    // is an AJAX nav, not a teardown). Matched before everything else so a click
    // inside the open palette never falls through to a page handler.
    const paletteRow = target.closest('.ro-palette-row');
    if (paletteRow) {
        closePalette();
        return; // let the anchor's native GET navigation proceed
    }
    // A click on the palette backdrop (the dimmed area outside the panel) closes
    // it, like Esc. The panel itself carries no close-marker, so an in-panel
    // click (e.g. into the input) does nothing here.
    const paletteClose = target.closest('[data-palette-close]');
    if (paletteClose) {
        event.preventDefault();
        closePalette();
        return;
    }

    // Auto-refresh interval option (navbar #refresh-dropdown): store the chosen
    // interval client-side, re-arm the poll, and reflect it in the control. The
    // dropdown opens on hover (Bulma is-hoverable), so there is no open/close
    // handler -- only the selection.
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

    // navbar-burger / aside-burger / toggle-tools: toggle `is-active` on the
    // control itself and on the element named by its `data-target`.
    const toggle = target.closest('.navbar-burger, .aside-burger, .toggle-tools');
    if (toggle) {
        toggle.classList.toggle('is-active');
        const targetEl = document.getElementById(toggle.dataset.target);
        if (targetEl) {
            targetEl.classList.toggle('is-active');
        }
        return;
    }

    // .unselect: uncheck every checkbox inside the element named by data-target.
    const unselect = target.closest('.unselect');
    if (unselect) {
        const container = document.getElementById(unselect.dataset.target);
        if (container) {
            container.querySelectorAll('input[type=checkbox]').forEach((inp) => {
                inp.checked = false;
            });
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
        document.location.hash = names.length ? `collapsed=${names.join(',')}` : '';
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

    // #namespace-dropdown: toggle `is-active`; focus the searchbox when opening.
    const nsDropdown = target.closest('#namespace-dropdown');
    if (nsDropdown) {
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
    // ⌘K palette query box: filter the harvested target rows by substring of
    // their label+path (case-insensitive) and keep the active row valid.
    const paletteInput = event.target.closest('#ro-palette-input');
    if (paletteInput) {
        filterPalette(paletteInput.value);
        return;
    }

    // #namespace-searchbox: filter the .namespace-item links by substring.
    const searchbox = event.target.closest('#namespace-searchbox');
    if (searchbox) {
        const filterText = searchbox.value;
        document.querySelectorAll('.namespace-item').forEach((element) => {
            if (element.innerText.indexOf(filterText) === -1) {
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
    // Everything else here only matters while the palette is open.
    const palette = document.getElementById(PALETTE_ID);
    if (!palette || !palette.classList.contains('is-active')) {
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
        // Navigate to the currently-highlighted target (GET via its href).
        event.preventDefault();
        activatePaletteSelection();
        return;
    }
    if (event.key === 'Tab') {
        // Trap focus inside the panel: with one text input + the row anchors, we
        // simply keep focus on the query box and steer Tab/Shift-Tab through the
        // visible rows via the same active-row model the arrows use, so focus can
        // never escape to the page behind the modal.
        event.preventDefault();
        movePaletteActive(event.shiftKey ? -1 : 1);
        return;
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
                    .querySelectorAll(`main .collapsible[data-name=${name}]`)
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
// ⌘K jump-to command palette -- additive, CSP-clean, GET-only.
// ---------------------------------------------------------------------------
// A keyboard launcher that JUMPS to navigation targets ALREADY rendered on the
// page. It owns NO data and hits NO backend: openPalette() harvests the existing
// <a> links (the sidebar `.menu-item`s, the navbar `.namespace-item`s, the
// Clusters `.toplink`, and the breadcrumb path), reads each one's href + text,
// and lists them. Selecting a row navigates via that anchor's existing GET
// permalink (the row IS an <a>, so Enter/click is a plain GET) -- never the POST
// theme form, so the read-only floor is untouched. Everything is DOM-only
// (createElement/cloneNode/classList/textContent) -> no dynamic-code execution
// and no inline handler -> CSP-clean. The overlay markup ships hidden in
// partials/command-palette.html; we toggle the `is-active` class to show it.
const PALETTE_ID = 'ro-palette';

// Derive a short mono kind-tag (2 letters, matching .ro-ktag) for a target,
// generalising to ANY kind/CRD. We key off the resource PLURAL in the href when
// present (…/<plural>) else fall back to the label. A tiny abbreviation map
// covers the common kinds with a nicer tag than the first two letters; anything
// unmapped takes the first two alphanumerics uppercased (so a CRD still gets a
// sensible tag). Namespace dropdown items and the Clusters link get explicit tags.
const PALETTE_TAGS = {
    namespaces: 'NS', nodes: 'ND', persistentvolumes: 'PV',
    persistentvolumeclaims: 'PC', deployments: 'DE', cronjobs: 'CJ',
    jobs: 'JO', daemonsets: 'DS', statefulsets: 'SS', replicasets: 'RS',
    ingresses: 'IN', services: 'SV', pods: 'PO', configmaps: 'CM',
    secrets: 'SE', events: 'EV',
};

function paletteTagFor(plural, label) {
    if (plural && PALETTE_TAGS[plural]) {
        return PALETTE_TAGS[plural];
    }
    const basis = (plural || label || '').replace(/[^a-z0-9]/gi, '');
    if (basis.length >= 2) {
        return basis.slice(0, 2).toUpperCase();
    }
    return (basis.toUpperCase() + '··').slice(0, 2);
}

// The plural resource segment of a readout permalink, if any. Paths look like
// /clusters/<c>/namespaces/<ns>/<plural>[/<name>] or /clusters/<c>/<plural>;
// the meta links end in `_resource-types` / `events`. We return the trailing
// path segment that is the resource plural for tag derivation (best-effort).
function palettePluralFromHref(href) {
    try {
        const path = new URL(href, window.location.origin).pathname;
        const parts = path.split('/').filter((p) => p.length > 0);
        if (parts.length === 0) {
            return '';
        }
        const last = parts[parts.length - 1];
        if (last === '_resource-types') {
            return '';
        }
        // /clusters/<c>/namespaces/<ns>/<plural>  -> plural at index 4
        // /clusters/<c>/<plural>                  -> plural at index 2
        const nsIdx = parts.indexOf('namespaces');
        if (nsIdx !== -1 && parts.length > nsIdx + 2) {
            return parts[nsIdx + 2];
        }
        return last;
    } catch (e) {
        return '';
    }
}

// Harvest the jump targets from the LIVE page. Each source is an existing <a>
// already rendered (no fetch). We read href + trimmed text, group + tag them,
// and DEDUPE by href (the same target can appear in more than one place). Order:
// Clusters, namespaces, then sidebar sections, then breadcrumb -- a stable,
// scannable order. Returns an array of {href, label, group, tag}.
function collectPaletteTargets() {
    const targets = [];
    const seen = Object.create(null);
    const push = (anchor, group) => {
        if (!anchor) {
            return;
        }
        const href = anchor.getAttribute('href');
        const label = (anchor.textContent || '').replace(/\s+/g, ' ').trim();
        // Only real same-document GET permalinks: skip empty / hash-only / the
        // decorative placeholder and any javascript:/external scheme.
        if (!href || href === '#' || href.charAt(0) === '#'
            || /^[a-z]+:/i.test(href) && !/^https?:/i.test(href)) {
            return;
        }
        if (!label || seen[href]) {
            return;
        }
        seen[href] = true;
        const plural = palettePluralFromHref(href);
        targets.push({ href: href, label: label, group: group, tag: paletteTagFor(plural, label) });
    };
    // Clusters top link (the navbar .toplink to /clusters).
    document.querySelectorAll('.navbar .toplink').forEach((a) => push(a, 'Jump to'));
    // Namespace dropdown items (navbar).
    document.querySelectorAll('.namespace-item').forEach((a) => push(a, 'Namespaces'));
    // Sidebar groups (resource types + the Meta links).
    document.querySelectorAll('.menu .menu-item').forEach((a) => push(a, 'Sidebar'));
    // Breadcrumb path (ancestor links; the current segment is pointer-events:none
    // and usually has no href, so it is naturally skipped).
    document.querySelectorAll('nav.breadcrumb a[href]').forEach((a) => push(a, 'Jump to'));
    return targets;
}

// Render the harvested targets into #ro-palette-list by cloning the <template>
// row per target. Pure DOM writes (textContent + setAttribute) -- the labels are
// assigned via textContent so even a hostile resource name can never inject
// markup. Returns the list of created row anchors (for the active-row model).
function renderPaletteTargets(targets) {
    const list = document.getElementById('ro-palette-list');
    const tmpl = document.getElementById('ro-palette-row-tmpl');
    if (!list || !tmpl || !('content' in tmpl)) {
        return [];
    }
    list.textContent = '';
    targets.forEach((t) => {
        const frag = tmpl.content.cloneNode(true);
        const row = frag.querySelector('.ro-palette-row');
        if (!row) {
            return;
        }
        row.setAttribute('href', t.href);
        const tag = row.querySelector('.ro-palette-tag');
        const label = row.querySelector('.ro-palette-label');
        const path = row.querySelector('.ro-palette-path');
        if (tag) { tag.textContent = t.tag; }
        if (label) { label.textContent = t.label; }
        if (path) { path.textContent = t.group; }
        list.appendChild(frag);
    });
    return Array.prototype.slice.call(list.querySelectorAll('.ro-palette-row'));
}

// The currently-highlighted row index among the VISIBLE rows. We track it on the
// list element so it survives across filter passes within one open session.
function paletteVisibleRows() {
    const list = document.getElementById('ro-palette-list');
    if (!list) {
        return [];
    }
    return Array.prototype.filter.call(
        list.querySelectorAll('.ro-palette-row'),
        (row) => !row.closest('.ro-palette-item').classList.contains('is-hidden')
    );
}

// Mark exactly one visible row active (highlight + aria-selected + scroll into
// view); clamp the index into range. Index -1 clears the highlight.
function setPaletteActive(index) {
    const rows = paletteVisibleRows();
    rows.forEach((row) => {
        row.classList.remove('is-active');
        row.setAttribute('aria-selected', 'false');
    });
    if (rows.length === 0) {
        return;
    }
    let i = index;
    if (i < 0) { i = 0; }
    if (i > rows.length - 1) { i = rows.length - 1; }
    const active = rows[i];
    active.classList.add('is-active');
    active.setAttribute('aria-selected', 'true');
    active.scrollIntoView({ block: 'nearest' });
}

// Move the active row by delta among the visible rows, wrapping at the ends so
// ArrowDown past the last lands on the first (and ArrowUp past the first lands
// on the last) -- a small, predictable cycle.
function movePaletteActive(delta) {
    const rows = paletteVisibleRows();
    if (rows.length === 0) {
        return;
    }
    let current = rows.findIndex((row) => row.classList.contains('is-active'));
    if (current === -1) {
        current = delta > 0 ? -1 : 0;
    }
    let next = current + delta;
    if (next < 0) { next = rows.length - 1; }
    if (next > rows.length - 1) { next = 0; }
    setPaletteActive(next);
}

// Filter the rows by a case-insensitive substring of label/group/tag/href. This
// href matters because namespace jump rows point to the default Pods view while
// their visible label is only the namespace name. Hides non-matches via the
// is-hidden CLASS (no inline style), toggles the empty-state line, and re-seats
// the active row on the first match so Enter always targets something sensible.
function filterPalette(query) {
    const list = document.getElementById('ro-palette-list');
    const empty = document.getElementById('ro-palette-empty');
    if (!list) {
        return;
    }
    const q = (query || '').toLowerCase().trim();
    let visible = 0;
    Array.prototype.forEach.call(list.querySelectorAll('.ro-palette-item'), (item) => {
        const row = item.querySelector('.ro-palette-row');
        const hay = row ? `${row.textContent || ''} ${row.getAttribute('href') || ''}`.toLowerCase() : '';
        const match = q === '' || hay.indexOf(q) !== -1;
        item.classList.toggle('is-hidden', !match);
        if (match) { visible++; }
    });
    if (empty) {
        empty.classList.toggle('is-hidden', visible !== 0);
    }
    setPaletteActive(visible > 0 ? 0 : -1);
}

// Navigate to the active row's target. The row is an <a>, so .click() performs a
// plain GET to its existing permalink (honoured by hx-boost as an AJAX nav with
// real history) -- no POST, no fetch, no eval. Closing happens via the click
// handler (which sees the .ro-palette-row and lets the navigation proceed).
function activatePaletteSelection() {
    const rows = paletteVisibleRows();
    const active = rows.find((row) => row.classList.contains('is-active')) || rows[0];
    if (active) {
        active.click();
    }
}

// Remember what had focus before the palette opened so Esc/close can restore it
// (keyboard users land back where they were instead of on <body>).
let palettePriorFocus = null;

// Open the palette: (re)harvest the live targets, render them, reveal the overlay
// (is-active class -- never inline style), clear + focus the query box, and seat
// the first row active. Idempotent: re-opening just refreshes the target list
// (which may have changed after an hx-boost navigation to another page).
function openPalette() {
    const palette = document.getElementById(PALETTE_ID);
    const input = document.getElementById('ro-palette-input');
    if (!palette || !input) {
        return; // partial not present (defensive) -> no-op
    }
    palettePriorFocus = document.activeElement;
    renderPaletteTargets(collectPaletteTargets());
    input.value = '';
    filterPalette('');
    // Reveal by dropping the Bulma `is-hidden` utility (display:none) -- it
    // carries !important, so the visibility MUST be toggled on that class, not
    // shadowed by an app-layer rule. `is-active` is the state hook the keydown
    // guard + the styling key off; both flip together.
    // Reveal by dropping the Bulma `is-hidden` utility (display:none) -- it
    // carries !important, so the visibility MUST be toggled on that class, not
    // shadowed by an app-layer rule. `is-active` is the state hook the keydown
    // guard + the styling key off; both flip together.
    palette.classList.remove('is-hidden');
    palette.classList.add('is-active');
    palette.setAttribute('aria-hidden', 'false');
    // Focus after it is shown so the caret lands in the box.
    input.focus();
}

// Close the palette: hide the overlay (drop is-active) and restore focus to
// wherever it was before opening (if that element is still in the document).
function closePalette() {
    const palette = document.getElementById(PALETTE_ID);
    if (!palette) {
        return;
    }
    // Hide by re-adding the Bulma `is-hidden` utility (its !important display:none
    // is what actually removes it from the layout); drop the state hook too.
    palette.classList.add('is-hidden');
    palette.classList.remove('is-active');
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
// is showing (the #resource-list-content container exists), we dispatch the
// element's OWN `ro:refresh` htmx event on that interval. Triggering the
// element's configured request (not a standalone htmx.ajax) keeps its
// hx-ext="morph" + hx-swap="morph:innerHTML" path, so the table morphs in place
// (scroll / focus / row selection survive). Polling PAUSES while the tab is
// hidden (no background API hammering), and refreshes once immediately on return.
const REFRESH_KEY = 'roRefresh';
let refreshTimerId = null;

function refreshInterval() {
    try {
        return parseInt(window.localStorage.getItem(REFRESH_KEY) || '0', 10) || 0;
    } catch (e) {
        return 0; // localStorage unavailable (e.g. privacy mode) -> stay static
    }
}

function fireRefresh() {
    if (document.hidden) {
        return; // paused while the tab is in the background
    }
    const container = document.getElementById('resource-list-content');
    if (container && typeof htmx !== 'undefined') {
        htmx.trigger(container, 'ro:refresh');
    }
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

// Run all init-time steps. Called on DOMContentLoaded and on htmx:load so the
// steps re-apply after an hx-boost body swap (which does not refire
// DOMContentLoaded). Each step is idempotent.
function runInit() {
    syncRefreshUI();
    applyRefresh();
    buildYamlFolds();
    collapseSectionsFromHash();
    highlightYamlLine();
    syncThemeTogglePostTarget();
}

document.addEventListener('DOMContentLoaded', runInit);
// hx-boost swaps <body> via AJAX rather than a full navigation, so
// DOMContentLoaded will not fire on those transitions; htmx:load re-runs init.
// HTMX events bubble, so we listen on `document` (this script runs in <head>
// before <body> exists, so document.body would be null at this point anyway).
document.addEventListener('htmx:load', runInit);
