// row-selection.ts -- identity-keyed row state (D6) + the row-click selection
// gesture (Unit 10, migrated from legacy.js). Single-type list rows carry
// data-key="cluster/ns/name" (and an id derived from it, which idiomorph uses to
// match rows by OBJECT identity, never by position). Row-level client state
// lives here, keyed by that identity:
//   - rowSelection: the multi-select Map, key -> { name } -- the bulk-action
//     payload (the full untruncated object name) captured from the row at
//     selection time, so a bulk action can act on a selected object even after a
//     server-side filter dropped its row from the DOM.
//   - rowFocusKey:  the single j/k keyboard-focus row.
// A morph syncs the server's class attribute over any client-added class, so the
// classes are RE-APPLIED from this store on htmx:afterSwap (legacy.js). Because
// the keys are object identities, a re-sorted/filtered fragment re-decorates the
// SAME objects wherever their rows land. window.roRowState is the deliberate
// seam the gesture layer + e2e suite drive; everything is pure DOM classList
// writes (CSP-clean). The needle contract pins reapplyRowState / roRowState /
// tr[data-key] surviving in the bundle (internal/web/list_redesign_test.go).

import type { Binding } from './events.js';

const rowSelection = new Map<string, { name: string }>();
let rowFocusKey: string | null = null;

export function reapplyRowState(): void {
    const content = document.getElementById('resource-list-content');
    if (!content) {
        return;
    }
    let focusedRow: HTMLElement | null = null;
    content.querySelectorAll('tr[data-key]').forEach((tr) => {
        const row = tr as HTMLElement;
        row.classList.toggle('is-selected', rowSelection.has(row.dataset.key as string));
        const focused = row.dataset.key === rowFocusKey;
        row.classList.toggle('kfocus', focused);
        if (focused) {
            focusedRow = row;
        }
    });
    // a11y (D23/SPEC §8.6): the table wrap mirrors the focused row's id as
    // aria-activedescendant. Synced HERE -- the single place row state lands in
    // the DOM -- so it survives every morph exactly like kfocus, and clears when
    // the focused row left the fragment.
    content.querySelectorAll('.ro-table-wrap').forEach((wrap) => {
        const fr = focusedRow as HTMLElement | null;
        if (fr?.id && wrap.contains(fr)) {
            wrap.setAttribute('aria-activedescendant', fr.id);
        } else {
            wrap.removeAttribute('aria-activedescendant');
        }
    });
}

// lastKeySegment falls back to the key's trailing segment as the object name
// (k8s names cannot contain "/") when a caller selects a key whose row is not in
// the DOM (the e2e seam); server-rendered rows always carry data-name.
export function lastKeySegment(key: string): string {
    const parts = (key || '').split('/');
    return parts[parts.length - 1] || '';
}

// rowSelectionEntry captures the bulk-action payload for key from its row: the
// object NAME (the bulk download derives its names list from key/name).
function rowSelectionEntry(key: string): { name: string } {
    const content = document.getElementById('resource-list-content');
    let entry: { name: string } | null = null;
    if (content) {
        content.querySelectorAll('tr[data-key]').forEach((tr) => {
            const row = tr as HTMLElement;
            if (row.dataset.key === key) {
                entry = { name: row.dataset.name || lastKeySegment(key) };
            }
        });
    }
    return entry || { name: lastKeySegment(key) };
}

function setRowSelected(key: string, on: boolean): void {
    if (on) {
        rowSelection.set(key, rowSelectionEntry(key));
    } else {
        rowSelection.delete(key);
    }
    reapplyRowState();
    updateBulkBar();
}

export function clearRowState(): void {
    rowSelection.clear();
    rowFocusKey = null;
    reapplyRowState();
    updateBulkBar();
}

// window.roRowState -- the e2e + gesture seam (selection store + j/k focus).
(
    window as unknown as {
        roRowState: {
            setSelected(key: string, on: boolean): void;
            setFocus(key: string): void;
            focusedKey(): string | null;
            clear(): void;
            selectedKeys(): string[];
            selectedEntries(): { key: string; name: string }[];
        };
    }
).roRowState = {
    setSelected: setRowSelected,
    setFocus(key: string) {
        rowFocusKey = key || null;
        reapplyRowState();
    },
    // focusedKey is the j/k focus seam the windowed walker (virtualizeMoveFocus,
    // still in legacy.js) reads across the module boundary -- the focused row can
    // be detached off-window, so the store (not the DOM kfocus class) is the
    // truth there. Also a debug sim the console can poll.
    focusedKey() {
        return rowFocusKey;
    },
    clear: clearRowState,
    selectedKeys() {
        return Array.from(rowSelection.keys());
    },
    // selectedEntries feeds the bulk actions: Copy names reads .name, and the
    // bulk Download-YAML builds its names list from .key/.name.
    selectedEntries() {
        return Array.from(rowSelection, ([key, entry]) => ({ key: key, name: entry.name }));
    },
};

// --- bulk bar paint (the selection store's view) ---------------------------
// updateBulkBar paints the pill from the selection store: at >=1 selected it
// reveals (`is-open`) with "N selected"; at 0 it fades out AND goes `inert`, so
// the invisible buttons can never take focus or clicks. On bulk-capable bars
// (data-bulk-href) it also enforces the client half of the download cap: above
// BULK_NAMES_MAX the Download button disables and ONE toast announces the
// refusal per cap crossing (re-armed once the selection drops back under).
export const BULK_NAMES_MAX = 100;
let bulkOverCapToasted = false;

export function updateBulkBar(): void {
    const bar = document.getElementById('ro-bulkbar');
    if (!bar) {
        return;
    }
    const count = rowSelection.size;
    const label = document.getElementById('ro-bulk-count');
    if (label && count > 0) {
        label.textContent = `${count} selected`;
    }
    bar.classList.toggle('is-open', count > 0);
    bar.toggleAttribute('inert', count === 0);
    const download = document.getElementById('ro-bulk-download') as HTMLButtonElement | null;
    if (download && (bar as HTMLElement).dataset.bulkHref) {
        const over = count > BULK_NAMES_MAX;
        download.disabled = over;
        download.title = over ? `Over the ${BULK_NAMES_MAX}-object bulk download cap` : '';
        if (over && !bulkOverCapToasted) {
            roToast(`Download refused: ${count} selected (max ${BULK_NAMES_MAX})`);
        }
        bulkOverCapToasted = over;
    }
}

// roToast bridges the window.roToast seam (toasts.ts is wired through legacy via
// window.roToast = showToast; calling through the seam keeps this module free of
// a toasts.ts import and matches the over-cap path the monolith used).
function roToast(message: string): void {
    const fn = (window as unknown as { roToast?: (m: string) => void }).roToast;
    if (typeof fn === 'function') {
        fn(message);
    }
}

// roCopyText copies text via the async clipboard API with a hidden-textarea
// execCommand fallback (navigator.clipboard exists only in secure contexts, and
// a plain-HTTP LAN deployment is a real readout topology). done(ok) runs after
// the attempt either way. Shared by the bulk Copy-names and the context menu's
// Copy item.
export function roCopyText(text: string, done: (ok: boolean) => void): void {
    const fallback = (): boolean => {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.setAttribute('readonly', '');
        ta.style.position = 'fixed';
        ta.style.top = '-1000px';
        document.body.appendChild(ta);
        ta.select();
        let ok = false;
        try {
            ok = document.execCommand('copy');
        } catch {
            ok = false;
        }
        ta.remove();
        return ok;
    };
    if (navigator.clipboard?.writeText) {
        navigator.clipboard.writeText(text).then(
            () => done(true),
            () => done(fallback()),
        );
        return;
    }
    done(fallback());
}

// toggleRowSelection is the row-click gesture: flip this row's key in the store
// and repaint. The payload (the object name) is captured from the clicked row.
function toggleRowSelection(tr: HTMLElement): void {
    const key = tr.dataset.key;
    if (!key) {
        return;
    }
    if (rowSelection.has(key)) {
        rowSelection.delete(key);
    } else {
        rowSelection.set(key, { name: tr.dataset.name || lastKeySegment(key) });
    }
    reapplyRowState();
    updateBulkBar();
}

// --- dispatcher bindings ----------------------------------------------------
// The row-select step of the monolith's row-gesture click listener (C2 step 4).
// It is the LAST step and falls THROUGH from the unconditional closeRowMenu
// (context-menu.ts) with NO stop between them -- so a click on a different row
// while a menu is open dismisses the menu AND selects that row in one pass
// (compound case 1). Registered AFTER context-menu's + bulk's click bindings in
// bindings.ts, reproducing the monolith step order. NO stop (it is terminal).
export const rowSelectionBindings: Binding[] = [
    {
        event: 'click',
        selector: '#resource-list-content tr[data-key]',
        handler: (event, matched) => {
            // Interactive descendants keep their own gesture (the NAME anchor
            // opens, label chips filter, +N overflow expands; SPEC §5).
            const target = event.target as Element | null;
            if (target?.closest('a, button, input, select, textarea, label')) {
                return;
            }
            toggleRowSelection(matched as HTMLElement);
        },
    },
];
