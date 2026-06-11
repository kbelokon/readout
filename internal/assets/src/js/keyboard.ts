// keyboard.ts -- keyboard row navigation + the "?" keyboard-map overlay (Unit 10,
// migrated from legacy.js / Unit 18 + D10/D23). j/k move a single keyboard row
// focus through the VISIBLE identity rows (kfocus, keyed by data-key through
// window.roRowState so it survives every morph), ⏎ opens the focused row's
// detail href, "?" toggles the keyboard-map card.
//
// THE inter-surface decoupling rides DOM GUARDS, not registration order
// (compound case 2): the gesture keys are INERT while any text-entry surface or
// overlay owns the keyboard -- a focused input/textarea/select (the chips
// editor's ⏎-commits-a-chip protocol above all), the ⌘K palette, the row
// context menu, the namespace dropdown, the ⊞ columns popover, and the kbd
// overlay itself. keyboardSurfaceBusy() reads those live -- so while the palette
// is open, j/k/Enter NEVER move row focus (the migrated palette keydown owns the
// keys; here we return inert). These guards are transcribed LITERALLY; the
// dispatch order vs the palette keydown is belt-and-suspenders.
//
// The Unit-24 virtualizer and the Unit-12 columns popover are NOT migrated yet,
// so the windowed j/k walk and the colsPopOpen guard reach them through the
// window.roClusterBridge seam (cluster-bridge.ts).

import type { Binding } from './events.js';
import { clusterBridge } from './cluster-bridge.js';

const PALETTE_ID = 'ro-palette';

// roRowState focus seam (owned by row-selection.ts), read at call time.
function roRowState(): { setFocus(key: string): void; focusedKey(): string | null } {
    return (window as unknown as {
        roRowState: { setFocus(key: string): void; focusedKey(): string | null };
    }).roRowState;
}

// keyboardTargetIsTextEntry: the focused element owns typed characters, so the
// gesture keys pass through untouched (j in the filter editor is the letter j).
function keyboardTargetIsTextEntry(target: EventTarget | null): boolean {
    const el = target as (Element & { isContentEditable?: boolean }) | null;
    if (!el || el.nodeType !== 1) {
        return false;
    }
    const tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || !!el.isContentEditable;
}

// keyboardSurfaceBusy: an open palette / context menu / namespace dropdown /
// columns popover owns the keys (SPEC §8.6: menus and overlays are modal to the
// keyboard). The first three are read from the live DOM; the columns popover
// (Unit 12, still in legacy.js) is read through the bridge.
function keyboardSurfaceBusy(): boolean {
    const palette = document.getElementById(PALETTE_ID);
    if (palette && palette.classList.contains('open')) {
        return true;
    }
    const menu = document.getElementById('ro-ctxmenu');
    if (menu && menu.classList.contains('is-open')) {
        return true;
    }
    const nsDropdown = document.getElementById('namespace-dropdown');
    if (nsDropdown && nsDropdown.classList.contains('is-active')) {
        return true;
    }
    return clusterBridge().colsPopOpen();
}

// visibleKeyRows: the identity rows j/k walk, in DOM order, with rows the live
// free-text filter is hiding excluded (focus lands only on rows the user can
// see). Reads the DOM (not the row model) deliberately.
function visibleKeyRows(): HTMLElement[] {
    return Array.from(
        document.querySelectorAll('#resource-list-content tbody tr[data-key]'),
    ).filter((tr) => !tr.classList.contains('ro-row-filtered')) as HTMLElement[];
}

// moveRowFocus steps the focus key by delta through the visible rows, clamping
// at both ends. Returns true when a row took focus (the caller preventDefaults
// only then). While the Unit-24 virtualizer is engaged the walker is fed from
// the virtualizer's full visible list (via the bridge; D20).
function moveRowFocus(delta: number): boolean {
    const bridge = clusterBridge();
    if (bridge.virtualizerActive()) {
        return bridge.virtMoveFocus(delta);
    }
    const rows = visibleKeyRows();
    if (rows.length === 0) {
        return false;
    }
    const focusKey = roRowState().focusedKey();
    const current = rows.findIndex((tr) => tr.dataset.key === focusKey);
    const next = Math.max(0, Math.min(rows.length - 1, current + delta));
    roRowState().setFocus(rows[next].dataset.key as string);
    rows[next].scrollIntoView({ block: 'nearest' });
    return true;
}

// openFocusedRow (⏎): navigate to the focused row's server-resolved open href.
// Only acts when the focused row is still present AND visible.
function openFocusedRow(): boolean {
    const key = roRowState().focusedKey();
    if (!key) {
        return false;
    }
    const bridge = clusterBridge();
    let row: HTMLElement | null = visibleKeyRows().find((tr) => tr.dataset.key === key) || null;
    if (!row && bridge.virtualizerActive()) {
        // Windowed (Unit 24): the focused row may have scrolled out of the
        // rendered window -- it is still logically visible.
        const tr = bridge.virtRowByKey(key) as HTMLElement | null;
        if (tr && bridge.virtVisible().indexOf(tr) !== -1) {
            row = tr;
        }
    }
    if (!row || !row.dataset.href) {
        return false;
    }
    window.location.assign(row.dataset.href);
    return true;
}

// --- the "?" keyboard-map overlay (layout chrome, kbdOverlayC) --------------
// Open/close follow the palette pattern: the `open` class + aria-hidden, with
// the prior focus remembered so esc lands the keyboard user back where they
// were. The card (tabindex="-1") takes focus on open; it is the dialog's only
// focus stop, so the Tab trap in the keydown binding completes the focus trap.
let kbdPriorFocus: Element | null = null;

function kbdOverlayEl(): HTMLElement | null {
    return document.getElementById('ro-kbd-overlay');
}

function kbdOverlayOpen(): boolean {
    const overlay = kbdOverlayEl();
    return !!overlay && overlay.classList.contains('open');
}

function openKbdOverlay(): void {
    const overlay = kbdOverlayEl();
    if (!overlay) {
        return;
    }
    kbdPriorFocus = document.activeElement;
    overlay.classList.add('open');
    overlay.setAttribute('aria-hidden', 'false');
    const card = overlay.querySelector('.kbd-card') as HTMLElement | null;
    if (card) {
        card.focus();
    }
}

// closeKbdOverlay is imported by palette.ts (the ⌘K chord closes it FIRST so one
// Esc later closes exactly one surface).
export function closeKbdOverlay(): void {
    const overlay = kbdOverlayEl();
    if (!overlay) {
        return;
    }
    overlay.classList.remove('open');
    overlay.setAttribute('aria-hidden', 'true');
    const prior = kbdPriorFocus as HTMLElement | null;
    if (prior && document.contains(prior) && typeof prior.focus === 'function') {
        prior.focus();
    }
    kbdPriorFocus = null;
}

// --- dispatcher bindings ----------------------------------------------------
export const keyboardBindings: Binding[] = [
    // C3: a click on the overlay backdrop ITSELF (outside the card) closes it --
    // the palette's backdrop contract. Independent.
    {
        event: 'click',
        handler: (event) => {
            if ((event.target as Element).id === 'ro-kbd-overlay') {
                closeKbdOverlay();
            }
        },
    },
    // K3: THE gesture keydown. The DOM guards (kbd overlay open, modifier chord,
    // text-entry, surface-busy) keep it disjoint from the palette/filter keys --
    // registration after the palette keydown is incidental; the busy guard does
    // the real work (compound case 2). No selector (it keys off focus/state).
    {
        event: 'keydown',
        handler: (event) => {
            const e = event as KeyboardEvent;
            // The kbd overlay is modal: esc and "?" close it, Tab is trapped on
            // the card (its only focus stop), everything else inert while open.
            if (kbdOverlayOpen()) {
                if (e.key === 'Escape' || e.key === '?') {
                    e.preventDefault();
                    closeKbdOverlay();
                } else if (e.key === 'Tab') {
                    e.preventDefault();
                }
                return;
            }
            if (e.metaKey || e.ctrlKey || e.altKey) {
                return; // never hijack a chorded shortcut
            }
            if (keyboardTargetIsTextEntry(e.target) || keyboardSurfaceBusy()) {
                return;
            }
            if (e.key === '?') {
                e.preventDefault();
                openKbdOverlay();
                return;
            }
            if (e.key === 'j' || e.key === 'k') {
                if (moveRowFocus(e.key === 'j' ? 1 : -1)) {
                    e.preventDefault();
                }
                return;
            }
            if (e.key === 'Enter') {
                // ⏎ opens the focused row -- but never steals the key from a real
                // control (a focused sort-header link, button, or summary keeps
                // its native activation; the focusable table wrap is intentionally
                // NOT excluded -- ⏎ there is the aria-activedescendant pattern).
                const target = e.target as Element | null;
                if (target && target.closest && target.closest('a, button, summary')) {
                    return;
                }
                if (openFocusedRow()) {
                    e.preventDefault();
                }
            }
        },
    },
];
