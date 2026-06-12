// yaml-folds.ts -- nested-YAML-block folding + YAML line-anchor highlight
// (leaf migration from legacy.js). CSP-clean, no build, graceful.
//
// Over the EXISTING Pygments `linenos="table"` output (a `table.highlighttable`
// whose `td.code > pre` holds one `<span id="yaml-...-line-N">` per source line),
// we compute each line's indentation at load and inject a fold toggle before any
// line that OPENS a nested block (a `key:` / `- key:` whose following lines
// indent deeper). Clicking the toggle hides that block's deeper-indented child
// lines and shows the faint italic `… N lines` note on the opener line.
//
// Everything is DOM-only (document.createElement, classList, dataset) -- no
// eval, no innerHTML-with-handlers, no inline style. The whole pass is wrapped
// in try/catch and bails cleanly (leaving the plain highlighted block) on
// anything unexpected. The line anchors (`yaml-...-line-N` ids, the `.linenos a`
// gutter), the section-level fold, and the per-section copy are all untouched:
// child lines are HIDDEN (display:none), never removed, and copy strips the
// injected controls from a clone before reading the raw text.
//
// LEAF per the listener inventory: the two delegated click branches
// (`[data-ro-action="toggle-fold"]`, `.linenos a`) have no inter-listener dependency. The
// fold-toggle branch in the monolith called stopPropagation()+return; the
// inventory records that stopPropagation is INERT for the sibling document
// listeners (it only halts bubbling, not dispatch to other document listeners),
// and the branch returns regardless -- so the dispatcher reproduces it as a
// stop:true binding that still calls event.stopPropagation() to keep the exact
// monolith behavior (no semantic change). buildYamlFolds + highlightYamlLine are
// idempotent runInit steps consumed by legacy.js's runInit chain.

import type { Binding } from './events.js';

// yamlEffectiveIndent -- PURE: a YAML line's effective indent depth. Leading
// spaces give the depth, but a YAML block-sequence item ("- ...") sits at the
// SAME visual indent as its parent key, so we count a leading "- " (or a bare
// "-") as one extra level (+2). This makes "- name: x" nest under "containers:"
// exactly as the object structure does. Exported for node:test (pure, no DOM).
export function yamlEffectiveIndent(text: string): number {
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

// The raw YAML text of a Pygments `td.code` cell, with any injected fold
// controls removed. Folded child lines stay in the DOM (only hidden), so this is
// the FULL source YAML in any fold state -- the per-section copy stays correct.
export function yamlCodeText(codeCell: Element): string {
    if (!codeCell.querySelector('[data-ro-action="toggle-fold"], [data-ro-fold-control]')) {
        return codeCell.textContent || ''; // no folds injected -> raw text already clean
    }
    const clone = codeCell.cloneNode(true) as Element;
    clone
        .querySelectorAll('[data-ro-action="toggle-fold"], [data-ro-fold-control]')
        .forEach((el) => {
            el.remove();
        });
    return clone.textContent || '';
}

// Toggle one nested block: flip the opener's `is-folded` + aria-expanded and
// hide/show every child line that lists this opener's id in `data-fold-of`.
function toggleYamlFold(toggle: HTMLElement): void {
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
        const owners = ((line as HTMLElement).dataset.foldOf || '').split(' ');
        if (owners.indexOf(id) !== -1) {
            line.classList.toggle('ro-line-folded', folded);
        }
    });
}

// Inject the fold toggle + collapsed fold-note into an opener line span. The
// toggle is a real <button> (keyboard-focusable, CSP-clean) placed right after
// the line's leading <a> anchor so the caret sits at the start of the line; the
// note is a hidden <span class="ro-fold-note"> ("… N lines") appended at the
// line's end, shown by CSS only when folded. Both carry a data-ro-* hook the copy
// path strips (data-ro-action="toggle-fold" / data-ro-fold-control), so neither
// pollutes the copied raw YAML even after a CSS-class rename.
function injectFoldControls(lineSpan: Element, bodyCount: number): void {
    const toggle = document.createElement('button');
    toggle.type = 'button';
    toggle.className = 'ro-fold-toggle';
    toggle.dataset.roAction = 'toggle-fold';
    toggle.setAttribute('aria-expanded', 'true');
    toggle.setAttribute('aria-label', 'Toggle block');
    toggle.dataset.fold = lineSpan.id;

    const note = document.createElement('span');
    note.className = 'ro-fold-note';
    note.dataset.roFoldControl = 'note';
    const lineWord = bodyCount === 1 ? 'line' : 'lines';
    note.textContent = ` … ${bodyCount} ${lineWord}`;

    // Place the toggle after the leading anchor (so it reads at the line start,
    // left of the key); fall back to prepend if no anchor is present.
    const anchor = lineSpan.querySelector('a');
    if (anchor?.nextSibling) {
        lineSpan.insertBefore(toggle, anchor.nextSibling);
    } else if (anchor) {
        lineSpan.appendChild(toggle);
    } else {
        lineSpan.insertBefore(toggle, lineSpan.firstChild);
    }
    // Append the note at the very end of the line content, BEFORE the trailing
    // newline text node so the collapsed note renders on the opener's own line.
    const last = lineSpan.lastChild;
    if (last && last.nodeType === 3 && (last.textContent || '').indexOf('\n') !== -1) {
        lineSpan.insertBefore(note, last);
    } else {
        lineSpan.appendChild(note);
    }
}

// Build the nested folds for every YAML code block on the page. Idempotent: a
// cell already processed carries `data-ro-folds`, so an hx-boost re-init never
// doubles the controls. Fully guarded -- any error leaves that cell as a plain
// highlighted block (the accepted graceful fallback to the section-level fold).
export function buildYamlFolds(): void {
    document.querySelectorAll('.highlighttable td.code pre').forEach((pre) => {
        if ((pre as HTMLElement).dataset.roFolds) {
            return; // already processed (idempotent across re-inits)
        }
        try {
            // The per-line spans Pygments emits (linespans="yaml-...-line").
            // Direct children of <pre>, in source order; their textContent
            // preserves exact indentation + the trailing newline.
            const lines = Array.prototype.filter.call(
                pre.children,
                (el: Element) => el.tagName === 'SPAN' && el.id && el.id.indexOf('line-') !== -1,
            ) as Element[];
            (pre as HTMLElement).dataset.roFolds = '1'; // mark BEFORE work so a throw still bails once
            if (lines.length < 3) {
                return; // too small to have a meaningful nested block
            }
            const indents = lines.map((el) => yamlEffectiveIndent(el.textContent || ''));
            const isBlank = lines.map((el) => (el.textContent || '').trim() === '');

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
                while (end < lines.length) {
                    if (isBlank[end]) {
                        end++;
                        continue;
                    }
                    if (indents[end] > indents[i]) {
                        const cur = lines[end] as HTMLElement;
                        cur.dataset.foldOf = cur.dataset.foldOf
                            ? `${cur.dataset.foldOf} ${lines[i].id}`
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
                injectFoldControls(lines[i], bodyCount);
            }
        } catch (_e) {
            // Anything unexpected -> leave this block plainly highlighted (the
            // accepted graceful fallback). The cell is already marked, so we do
            // not retry it; the section-level fold + line anchors keep working.
        }
    });
}

// Highlight the YAML line named by location.hash (#<id>): clear prior
// highlights, then add the highlight class to `#yaml-<id>` and scroll it into
// view. Toggling a class (vs el.style.background) keeps the colour in CSS.
export function highlightYamlLine(): void {
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

// --- dispatcher bindings ---------------------------------------------------

// foldBindings are appended to the ordered registration list in bindings.ts.
// Both are LEAF branches lifted verbatim from the monolith's big click listener.
export const foldBindings: Binding[] = [
    // data-ro-action="toggle-fold" (NESTED YAML block fold): toggle the deeper-indented child
    // lines of a `key:`/`- key:` block in place. Matched BEFORE the section-fold
    // + gutter-anchor handlers (registration order) so a nested-fold click never
    // collapses the whole section or jumps a line anchor. The monolith called
    // preventDefault + stopPropagation + return; we keep stopPropagation (inert
    // for document siblings per the inventory, but preserved 1:1) and stop:true
    // mirrors the early return.
    {
        event: 'click',
        selector: '[data-ro-action="toggle-fold"]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            event.stopPropagation();
            toggleYamlFold(matched as HTMLElement);
            return true;
        },
    },
    // YAML line-number anchors (.linenos a): set the URL hash to the clicked
    // line, re-highlight, and suppress the default anchor jump. In the monolith
    // this branch sits AFTER the section-fold branch; here it shares the leaf
    // list and the section-fold handler (misc-ui) is registered separately. The
    // two never co-match (an anchor in the gutter is not a section title), so
    // relative order is immaterial -- but it keeps its own early-return.
    {
        event: 'click',
        selector: '.linenos a',
        stop: true,
        handler: (event, matched) => {
            const anchor = matched as HTMLAnchorElement;
            location.hash = `#${anchor.href.split('#')[1]}`;
            highlightYamlLine();
            event.preventDefault();
            return true;
        },
    },
];
