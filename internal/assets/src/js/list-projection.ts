// list-projection.ts -- the canonical client-side projection of a resource list.
//
// A list has two DOM representations (table rows and mobile cards) plus the
// plain-data row model used by filtering/autocomplete.  They must always be
// captured from the SAME complete server snapshot.  In particular, a windowed
// table keeps most of its rows detached, so neither the live tbody nor the card
// mount can be treated as the complete dataset after virtualization engages.
//
// This module is the single owner of that complete snapshot.  The virtualizer
// consumes its rows and identity index, but owns only viewport geometry and the
// rendered slice.  filters.ts consumes the stable row-model facade.  During a
// morph, the incoming snapshot is prepared before any rows are detached and is
// committed after the live DOM lands: windowed lists retain the prepared,
// off-DOM nodes; small lists re-adopt the connected nodes Idiomorph retained.

import {
    fieldSuggestionText,
    type ModelField,
    type ModelRow,
    normalizeFieldWhitespace,
    trimFilterWhitespace,
} from './filters-parse.js';
import type { RowModelWire } from './types.js';

interface ProjectionSnapshot {
    rows: HTMLElement[];
    byKey: Map<string, HTMLElement>;
    order: string[];
    cardsByKey: Map<string, HTMLElement>;
    fields: ModelField[];
    modelRows: ModelRow[];
    windowed: boolean;
}

interface PreparedProjection {
    root: ParentNode;
    snapshot: ProjectionSnapshot;
    previousByKey: Map<string, HTMLElement>;
}

export interface PreparedListProjection {
    readonly rows: readonly HTMLElement[];
    readonly windowed: boolean;
}

const rowModel: RowModelWire = {
    fields: [],
    rows: [],
    visibleKeys: null,
};

function emptySnapshot(): ProjectionSnapshot {
    return {
        rows: [],
        byKey: new Map(),
        order: [],
        cardsByKey: new Map(),
        fields: [],
        modelRows: [],
        windowed: false,
    };
}

let projection = emptySnapshot();
let prepared: PreparedProjection | null = null;
let projectionRoot: ParentNode | null = null;
let projectionRevision = 0;

// Stable, documented debug/e2e seam.  Captures mutate its fields rather than
// replacing the object so readers may safely retain the facade across swaps.
window.roRowModel = rowModel;

function captureFields(table: HTMLTableElement): ModelField[] {
    return Array.from(table.querySelectorAll('thead th')).map((th) => {
        const label = normalizeFieldWhitespace(th.textContent || '');
        return {
            label,
            name: fieldSuggestionText(label),
            hint: (th as HTMLElement).dataset.hint || '',
        };
    });
}

function captureModelRows(rows: readonly HTMLElement[]): ModelRow[] {
    return rows.map((tr) => {
        const cells = Array.from(tr.querySelectorAll('td')).map((td) =>
            trimFilterWhitespace(td.textContent || ''),
        );
        const nameLink = tr.querySelector('td.cell-name a');
        return {
            key: tr.dataset.key as string,
            name: nameLink ? trimFilterWhitespace(nameLink.textContent || '') : cells[0] || '',
            cells,
        };
    });
}

function captureCards(root: ParentNode, order: readonly string[]): Map<string, HTMLElement> {
    const cards = Array.from(root.querySelectorAll<HTMLElement>('.ro-cardlist > .ro-pcard'));
    const cardsByKey = new Map<string, HTMLElement>();
    cards.forEach((card) => {
        const key = card.dataset.key;
        if (key) {
            cardsByKey.set(key, card);
        }
    });
    // Older snapshots did not carry data-key on cards.  Their server order is
    // the table order, so a complete 1:1 card list can still be indexed safely.
    if (cardsByKey.size === 0 && cards.length === order.length) {
        cards.forEach((card, index) => {
            cardsByKey.set(order[index] as string, card);
        });
    }
    return cardsByKey;
}

function snapshotFrom(root: ParentNode): ProjectionSnapshot {
    const table = root.querySelector<HTMLTableElement>('table.ro-table');
    if (!table) {
        return emptySnapshot();
    }
    const tbody = table.tBodies.item(0);
    // A history-restored virtualized tbody is only a viewport slice.  Treat it
    // as no snapshot at all; virtualizer.ts will request one complete rebuild.
    if (!tbody || tbody.querySelector(':scope > tr.ro-vspacer')) {
        return emptySnapshot();
    }
    const rows = Array.from(tbody.querySelectorAll<HTMLElement>(':scope > tr[data-key]'));
    const byKey = new Map(rows.map((row) => [row.dataset.key as string, row]));
    const order = rows.map((row) => row.dataset.key as string);
    return {
        rows,
        byKey,
        order,
        cardsByKey: captureCards(root, order),
        fields: captureFields(table),
        modelRows: captureModelRows(rows),
        windowed: table.closest('.ro-table-wrap.ro-windowed') !== null,
    };
}

function publishModel(snapshot: ProjectionSnapshot): void {
    rowModel.fields = snapshot.fields;
    rowModel.rows = snapshot.modelRows;
}

// Adopt a complete DOM snapshot immediately.  Initial paint and fresh full-body
// renders use this path before the virtualizer removes anything from the tbody.
export function adoptListProjection(root: ParentNode): void {
    projection = snapshotFrom(root);
    projectionRoot = root;
    prepared = null;
    projectionRevision += 1;
    publishModel(projection);
    rowModel.visibleKeys = null;
}

// Adopt only when `root` is a genuinely new live projection. runInit and the
// virtualizer both visit the same persistent content node; the second visit must
// not traverse/model-capture it again or clear the draft visibility just
// re-derived between those steps. A prepared morph targets that same persistent
// root and is left untouched until afterSwap; a genuinely new root supersedes
// an abandoned pending snapshot (for example, cross-page navigation).
export function ensureListProjection(root: ParentNode): boolean {
    if (projectionRoot === root) {
        return false;
    }
    adoptListProjection(root);
    return true;
}

// Prepare an incoming list fragment exactly once.  morph.ts and the
// virtualizer both call this boundary deliberately; the root identity makes the
// second call idempotent after the virtualizer has replaced rows with spacers.
export function prepareListProjectionSwap(root: ParentNode): PreparedListProjection {
    if (prepared?.root !== root) {
        const snapshot = snapshotFrom(root);
        projectionRevision += 1;
        prepared = {
            root,
            snapshot,
            // Snapshot maps are created once and never mutated or exposed. Keep
            // the immutable prior index by reference for the windowed cell diff.
            previousByKey: projection.byKey,
        };
        // Filtering runs after the morph but before virtualizer adoption.  Make
        // the new full model available immediately while keeping active DOM
        // node ownership unchanged until commit.
        publishModel(snapshot);
    }
    return {
        rows: prepared.snapshot.rows,
        windowed: prepared.snapshot.windowed,
    };
}

// Commit the prepared fragment after Idiomorph lands it.  Windowed rows were
// intentionally detached and remain authoritative.  Small lists re-capture the
// connected DOM so consumers never retain the throwaway fragment nodes when
// Idiomorph preserved existing identities.
export function commitListProjectionSwap(): ReadonlyMap<string, HTMLElement> | null {
    if (!prepared) {
        return null;
    }
    const incoming = prepared;
    prepared = null;
    const content = document.getElementById('resource-list-content');
    if (incoming.snapshot.windowed) {
        projection = incoming.snapshot;
        // The full rows stay detached, but this is now the canonical projection
        // for the connected content mount. Identity-aware init must not inspect
        // its spacer-only tbody as though it were another snapshot.
        projectionRoot = content || incoming.root;
    } else {
        projection = content ? snapshotFrom(content) : emptySnapshot();
        projectionRoot = content;
        publishModel(projection);
    }
    return incoming.previousByKey;
}

export function resetListProjection(): void {
    projection = emptySnapshot();
    projectionRoot = null;
    prepared = null;
    projectionRevision += 1;
    publishModel(projection);
    rowModel.visibleKeys = null;
}

export function listProjectionSwapPending(): boolean {
    return prepared !== null;
}

// Monotonic model identity for cheap derived-view memoization. Preparing a new
// server snapshot advances it; committing that same snapshot to connected DOM
// does not. Future delta application can advance this at its atomic commit.
export function listProjectionRevision(): number {
    return projectionRevision;
}

export function listProjectionWindowed(): boolean {
    return projection.windowed;
}

export function listProjectionRows(): readonly HTMLElement[] {
    return projection.rows;
}

export function listProjectionOrder(): readonly string[] {
    return projection.order;
}

export function listProjectionRowByKey(key: string): HTMLElement | null {
    return projection.byKey.get(key) || null;
}

export function listProjectionCardByKey(key: string): HTMLElement | null {
    return projection.cardsByKey.get(key) || null;
}

export function listProjectionRowModel(): RowModelWire {
    return rowModel;
}

export function setListProjectionVisibleKeys(keys: Set<string> | null): void {
    rowModel.visibleKeys = keys;
}

export function listProjectionVisibleRows(): HTMLElement[] {
    const keys = rowModel.visibleKeys;
    return keys
        ? projection.rows.filter((row) => keys.has(row.dataset.key as string))
        : projection.rows;
}
