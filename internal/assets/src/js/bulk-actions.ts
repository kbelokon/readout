// bulk-actions.ts -- the bottom-center bulk bar actions (Unit 10, migrated from
// legacy.js): Copy names, Download YAML, Clear. The bar's PAINT (is-open / "N
// selected" / the download cap) lives in row-selection.ts (updateBulkBar); this
// file owns the three action handlers + their dispatcher click bindings, which
// were the bulk steps of the monolith's row-gesture click listener (C2 step 3).
//
// They register AFTER context-menu's click bindings and BEFORE the row-select
// binding (bindings.ts), reproducing the monolith order: the unconditional
// closeRowMenu (context-menu) runs first (dismiss any menu), then these bulk
// branches return on a match (stop:true), else the click falls through to the
// row-select binding. A disabled #ro-bulk-download never dispatches a click, so
// reaching the handler implies the action is allowed.

import type { Binding } from './events.js';
import { roCopyText, BULK_NAMES_MAX, clearRowState } from './row-selection.js';

// bulkCopyNames copies the newline-joined FULL names of every selected row --
// PINNED: including rows the live free-text filter is hiding and rows a
// server-side filter dropped from the DOM (selection is explicit user intent;
// the store, not the DOM, is the source). Feedback is the inline "Copied" flip
// on the button itself -- deliberately NO toast (D10).
let bulkCopyResetTimer = 0;
function bulkCopyNames(button: HTMLElement): void {
    const entries = roRowState().selectedEntries();
    const names = entries.map((entry) => entry.name).join('\n');
    roCopyText(names, (ok) => {
        if (!ok) {
            return; // clipboard refused: no false "Copied"
        }
        const label = button.querySelector('span:last-child');
        if (!label) {
            return;
        }
        window.clearTimeout(bulkCopyResetTimer);
        label.textContent = 'Copied';
        bulkCopyResetTimer = window.setTimeout(() => {
            label.textContent = 'Copy names';
        }, 1100);
    });
}

// bulkDownloadYAML navigates to the bulk GET (D11): the CLEAN server-baked base
// href (data-bulk-href, no filter/sort params -- the server looks names up in
// the UNFILTERED table, so selected-but-filtered rows are included) plus the
// comma-joined selected names. Grammar follows the list scope: bare object names
// on single-namespace / cluster-scoped lists; ns/name on _all-namespaces lists,
// derived by stripping the list's cluster prefix (data-bulk-cluster) off each
// selection key. The URL serves a Content-Disposition attachment, so
// location.assign downloads WITHOUT leaving the page and the selection survives.
function bulkDownloadYAML(bar: HTMLElement | null): void {
    if (!bar || !bar.dataset.bulkHref) {
        return;
    }
    const entries = roRowState().selectedEntries();
    if (entries.length === 0 || entries.length > BULK_NAMES_MAX) {
        return; // the button is disabled in both states; belt for direct calls
    }
    const clusterPrefix = (bar.dataset.bulkCluster || '') + '/';
    const names = entries.map((entry) => {
        if (bar.dataset.bulkAllns === 'true' && entry.key.indexOf(clusterPrefix) === 0) {
            return entry.key.slice(clusterPrefix.length);
        }
        return entry.name;
    });
    window.location.assign(bar.dataset.bulkHref + '&names=' + encodeURIComponent(names.join(',')));
}

// roRowState reads the selection seam (owned by row-selection.ts) at call time.
function roRowState(): { selectedEntries(): { key: string; name: string }[] } {
    return (window as unknown as {
        roRowState: { selectedEntries(): { key: string; name: string }[] };
    }).roRowState;
}

// --- dispatcher bindings ----------------------------------------------------
// C2 step 3: the bulk-bar buttons. Each returned in the monolith -> stop:true.
export const bulkBindings: Binding[] = [
    {
        event: 'click',
        selector: '#ro-bulk-download',
        stop: true,
        handler: (_event, matched) => {
            bulkDownloadYAML((matched as HTMLElement).closest('#ro-bulkbar'));
            return true;
        },
    },
    {
        event: 'click',
        selector: '#ro-bulk-copy',
        stop: true,
        handler: (_event, matched) => {
            bulkCopyNames(matched as HTMLElement);
            return true;
        },
    },
    {
        event: 'click',
        selector: '#ro-bulk-clear',
        stop: true,
        handler: () => {
            clearRowState();
            return true;
        },
    },
];
