// morph.ts -- the idiomorph integration: the auto-refresh changed-cell flash
// (Idiomorph.defaults.callbacks, set ONCE here) and the CSP-safe `ro-morph`
// extension that delivers the v2 list-loop morph config as a JS OBJECT (no
// attribute-spec eval). Migrated from legacy.js; the Go needle contract
// (internal/web/list_redesign_test.go) pins the extension name, the
// ignoreActiveValue config, and the morphStyle in the BUNDLE, so the forms below
// are byte-preserved.
//
// Vendor globals: htmx + Idiomorph are classic-script globals (the vendored
// idiomorph extension exposes Idiomorph; htmx loads before this bundle), reached
// through typeof guards -- never imported (no module exists for the vendored
// libs). Cross-module surfaces this module's handleSwap calls (captureRowModel /
// virtualizePrepareSwap) ARE imported -- they live in the typed modules.

import { captureRowModel } from './filters.js';
import { virtualizePrepareSwap } from './virtualizer.js';

// Minimal vendor typings (classic-script globals). Only the surfaces this module
// touches are declared; the bundle stays vendor-agnostic otherwise.
declare const htmx:
    | {
          defineExtension(
              name: string,
              ext: {
                  isInlineSwap(swapStyle: string): boolean;
                  handleSwap(
                      swapStyle: string,
                      target: Element,
                      fragment: DocumentFragment,
                  ): boolean;
              },
          ): void;
      }
    | undefined;

interface IdiomorphCallbacks {
    beforeNodeMorphed?: (oldNode: Node) => boolean | undefined;
    afterNodeMorphed?: (oldNode: Node) => void;
}
declare const Idiomorph:
    | {
          defaults?: { callbacks?: IdiomorphCallbacks };
          morph(
              target: Element,
              content: HTMLCollection,
              config: { morphStyle: string; ignoreActiveValue: boolean },
          ): boolean;
      }
    | undefined;

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
if (
    Idiomorph?.defaults?.callbacks &&
    !window.matchMedia('(prefers-reduced-motion: reduce)').matches
) {
    const PRIOR = new WeakMap<Element, string | null>();
    Idiomorph.defaults.callbacks.beforeNodeMorphed = (oldNode: Node) => {
        if (oldNode && oldNode.nodeType === 1 && (oldNode as Element).tagName === 'TD') {
            PRIOR.set(oldNode as Element, oldNode.textContent);
        }
        // return undefined -> idiomorph proceeds with the morph (false would skip it)
    };
    Idiomorph.defaults.callbacks.afterNodeMorphed = (oldNode: Node) => {
        if (oldNode?.nodeType !== 1 || (oldNode as Element).tagName !== 'TD') {
            return;
        }
        const el = oldNode as HTMLElement;
        if (!PRIOR.has(el)) {
            return;
        }
        const before = PRIOR.get(el);
        PRIOR.delete(el);
        if (before !== el.textContent) {
            el.classList.remove('ro-cell-changed');
            // force a reflow so re-adding the class restarts the animation if the
            // same cell changes again within the fade window
            void el.offsetWidth;
            el.classList.add('ro-cell-changed');
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
        isInlineSwap: (swapStyle: string) => swapStyle === 'morph',
        handleSwap: (swapStyle: string, target: Element, fragment: DocumentFragment) => {
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
            return (Idiomorph as NonNullable<typeof Idiomorph>).morph(target, fragment.children, {
                morphStyle: 'innerHTML',
                ignoreActiveValue: true,
            });
        },
    });
}
