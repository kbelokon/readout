// Canonical resource-list projection: table rows, mobile cards and the filter
// row model always come from one complete snapshot. The virtualizer owns only
// viewport geometry; this module retains the complete keyed dataset.

import {
    fieldSuggestionText,
    type ModelField,
    type ModelRow,
    normalizeFieldWhitespace,
    trimFilterWhitespace,
} from './filters-parse.js';
import type {
    LiveV2DeltaRegion,
    LiveV2DeltaRemove,
    LiveV2DeltaUpsert,
    LiveV2RegionName,
} from './live-protocol.js';
import type { RowModelWire } from './types.js';

interface ProjectionSnapshot {
    rows: HTMLElement[];
    byKey: Map<string, HTMLElement>;
    indexByKey: Map<string, number>;
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
    previousRevision: number;
}

export interface ListProjectionDeltaPlan {
    readonly remove: readonly LiveV2DeltaRemove[];
    readonly upsert: readonly LiveV2DeltaUpsert[];
    readonly order?: readonly string[];
    readonly regions: readonly LiveV2DeltaRegion[];
}

export interface ListProjectionDeltaSummary {
    readonly inserted: number;
    readonly updated: number;
    readonly deleted: number;
    readonly projected: number;
    readonly reordered: boolean;
    readonly regions: readonly LiveV2RegionName[];
}

export type ListProjectionDeltaResult =
    | {
          ok: true;
          focusKey: string | null;
          previousByKey: ReadonlyMap<string, HTMLElement>;
          summary: ListProjectionDeltaSummary;
          restoreFocus: () => void;
      }
    | { ok: false };

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
        indexByKey: new Map(),
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

function captureModelRow(tr: HTMLElement): ModelRow {
    const cells = Array.from(tr.querySelectorAll('td')).map((td) =>
        trimFilterWhitespace(td.textContent || ''),
    );
    const nameLink = tr.querySelector('td.cell-name a');
    return {
        key: tr.dataset.key as string,
        name: nameLink ? trimFilterWhitespace(nameLink.textContent || '') : cells[0] || '',
        cells,
    };
}

function captureModelRows(rows: readonly HTMLElement[]): ModelRow[] {
    return rows.map(captureModelRow);
}

function captureCards(root: ParentNode): Map<string, HTMLElement> {
    const cards = Array.from(root.querySelectorAll<HTMLElement>('.ro-cardlist > .ro-pcard'));
    const cardsByKey = new Map<string, HTMLElement>();
    cards.forEach((card) => {
        const key = card.dataset.key;
        if (key) {
            cardsByKey.set(key, card);
        }
    });
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
        indexByKey: new Map(order.map((key, index) => [key, index])),
        order,
        cardsByKey: captureCards(root),
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

// Repeated init on the same mount must not erase freshly derived visibility.
export function ensureListProjection(root: ParentNode): boolean {
    if (projectionRoot === root) {
        return false;
    }
    adoptListProjection(root);
    return true;
}

// morph.ts and the virtualizer may both prepare the same fragment.
export function prepareListProjectionSwap(root: ParentNode): PreparedListProjection {
    if (prepared?.root !== root) {
        const snapshot = snapshotFrom(root);
        const previousRevision = projectionRevision;
        projectionRevision += 1;
        prepared = {
            root,
            snapshot,
            previousRevision,
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

// A synchronous morph failure leaves the connected DOM untouched, so retire
// only the matching prepared snapshot and republish the still-authoritative
// projection. The root identity prevents an older failing attempt from
// cancelling a reentrant replacement.
export function cancelListProjectionSwap(root: ParentNode): void {
    const incoming = prepared;
    if (!incoming || incoming.root !== root) return;
    prepared = null;
    projectionRevision = incoming.previousRevision;
    publishModel(projection);
}

// Windowed rows remain detached; small lists re-adopt Idiomorph's connected DOM.
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

// Monotonic identity across completed model changes. A synchronous prepared
// swap cancellation restores its prior value before control returns to HTMX.
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

declare const Idiomorph:
    | {
          morph(
              target: Element,
              content: Element,
              config: { morphStyle: 'outerHTML'; ignoreActiveValue: true },
          ): unknown;
      }
    | undefined;

interface ParsedRegion {
    current: HTMLElement;
    incoming: HTMLElement;
}

interface FocusBookmark {
    active: HTMLElement;
    cellIndex: number;
    focusableIndex: number;
    key: string;
}

interface ListMounts {
    cardMount: HTMLElement | null;
    content: HTMLElement;
    tbody: HTMLTableSectionElement;
}

const focusableSelector = 'a[href], button, input, select, textarea, [tabindex]';

function oneElementRoot(parent: ParentNode): HTMLElement | null {
    let root: HTMLElement | null = null;
    for (const node of parent.childNodes) {
        if (node instanceof Text && !node.data.trim()) continue;
        if (root || !(node instanceof HTMLElement)) return null;
        root = node;
    }
    return root;
}

function parseRowFragment(html: string, key: string): HTMLElement | null {
    const tbody = document.createElement('tbody');
    tbody.innerHTML = html;
    const row = oneElementRoot(tbody);
    return row?.dataset.key === key ? row : null;
}

function parseCardFragment(html: string, key: string): HTMLElement | null {
    const template = document.createElement('template');
    template.innerHTML = html;
    const card = oneElementRoot(template.content);
    return card?.dataset.key === key ? card : null;
}

function parseRegionFragment(content: HTMLElement, update: LiveV2DeltaRegion): ParsedRegion | null {
    const selector = `[data-ro-live-region="${update.region}"]`;
    const current = content.querySelector<HTMLElement>(selector);
    const template = document.createElement('template');
    template.innerHTML = update.html;
    const incoming = oneElementRoot(template.content);
    if (!current || !incoming || incoming.dataset.roLiveRegion !== update.region) return null;
    return { current, incoming };
}

function currentListMounts(): ListMounts | null {
    const content = document.getElementById('resource-list-content');
    if (!content || projectionRoot !== content || prepared) return null;
    const table = content.querySelector<HTMLTableElement>('table.ro-table');
    const tbody = table?.tBodies.item(0) || null;
    if (!tbody) return null;
    return {
        cardMount: content.querySelector<HTMLElement>('.ro-cardlist'),
        content,
        tbody,
    };
}

function arraysEqual(left: readonly string[], right: readonly string[]): boolean {
    return left.length === right.length && left.every((value, index) => value === right[index]);
}

function morphElement(current: HTMLElement, incoming: HTMLElement): HTMLElement {
    // Availability is a transaction precondition; applyListProjectionDelta owns
    // the single fail-closed catch for both a missing implementation and a morph failure.
    const implementation = Idiomorph as Required<NonNullable<typeof Idiomorph>>;
    const outcome = implementation.morph(current, incoming, {
        morphStyle: 'outerHTML',
        ignoreActiveValue: true,
    });
    if (Array.isArray(outcome)) {
        const landed = outcome.find((node) => node instanceof HTMLElement);
        if (landed instanceof HTMLElement) return landed;
    }
    return current;
}

function placeElementsInOrder(parent: HTMLElement, elements: readonly HTMLElement[]): void {
    let cursor = parent.firstElementChild;
    for (const element of elements) {
        if (element === cursor) {
            cursor = cursor.nextElementSibling;
        } else {
            parent.insertBefore(element, cursor);
        }
    }
}

function captureFocusBookmark(): FocusBookmark | null {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement)) return null;
    const row = active.closest<HTMLTableRowElement>('tr[data-key]');
    const key = row?.dataset.key;
    const cell = active.closest<HTMLTableCellElement>('td, th');
    if (!row || !key || !cell || cell.parentElement !== row) return null;
    const focusables = cellFocusTargets(cell);
    const focusableIndex = focusables.indexOf(active);
    if (focusableIndex === -1) return null;
    return {
        active,
        cellIndex: Array.from(row.cells).indexOf(cell),
        focusableIndex,
        key,
    };
}

function cellFocusTargets(cell: HTMLElement): HTMLElement[] {
    return [cell, ...cell.querySelectorAll<HTMLElement>(focusableSelector)];
}

function focusRestorer(bookmark: FocusBookmark | null): () => void {
    return () => {
        if (!bookmark) return;
        const row = projection.byKey.get(bookmark.key) as HTMLTableRowElement | undefined;
        if (!row?.isConnected) return;
        const cell = row.cells.item(bookmark.cellIndex);
        if (!(cell instanceof HTMLElement)) return;
        const focusables = cellFocusTargets(cell);
        focusables[bookmark.focusableIndex]?.focus({ preventScroll: true });
    };
}

function updateLiveStatus(summary: ListProjectionDeltaSummary): void {
    const status = document.getElementById('ro-live-status');
    if (!status) return;
    const changed = summary.inserted + summary.updated + summary.deleted + summary.projected;
    const parts: string[] = [];
    if (changed > 0) parts.push(`${changed} row${changed === 1 ? '' : 's'}`);
    if (summary.reordered) parts.push('order changed');
    if (summary.regions.length > 0) {
        parts.push(`${summary.regions.length} region${summary.regions.length === 1 ? '' : 's'}`);
    }
    status.textContent = `Live update: ${parts.join(', ')}`;
}

// Apply one already-decoded server delta. The browser validates only the wire
// schema/cursor plus the one invariant needed to mount an upsert: each row/card
// fragment is one root carrying the operation key. Server templates remain the
// sole authority for tags, classes, ids, styles and nested markup.
export function applyListProjectionDelta(plan: ListProjectionDeltaPlan): ListProjectionDeltaResult {
    const mounts = currentListMounts();
    if (!mounts) return { ok: false };

    const parsedRows = new Map<string, HTMLElement>();
    const parsedCards = new Map<string, HTMLElement>();
    const parsedRegions: ParsedRegion[] = [];
    for (const operation of plan.upsert) {
        const row = parseRowFragment(operation.row, operation.key);
        if (!row) return { ok: false };
        parsedRows.set(operation.key, row);
        if (operation.card !== undefined) {
            const card = parseCardFragment(operation.card, operation.key);
            if (!card || !mounts.cardMount) return { ok: false };
            parsedCards.set(operation.key, card);
        }
    }
    for (const operation of plan.regions) {
        const region = parseRegionFragment(mounts.content, operation);
        if (!region) return { ok: false };
        parsedRegions.push(region);
    }

    const removedKeys = new Set(plan.remove.map((operation) => operation.key));
    const nextByKey = new Map(projection.byKey);
    const nextCards = new Map(projection.cardsByKey);
    for (const key of removedKeys) {
        nextByKey.delete(key);
    }
    for (const [key, incoming] of parsedRows) {
        nextByKey.set(key, projection.byKey.get(key) || incoming);
    }
    for (const [key, incoming] of parsedCards) {
        nextCards.set(key, projection.cardsByKey.get(key) || incoming);
    }

    const implicitOrder = projection.order.filter((key) => !removedKeys.has(key));
    for (const key of parsedRows.keys()) {
        if (!projection.byKey.has(key)) implicitOrder.push(key);
    }
    const nextOrder = plan.order ? [...plan.order] : implicitOrder;
    if (nextOrder.length !== nextByKey.size || nextOrder.some((key) => !nextByKey.has(key))) {
        return { ok: false };
    }
    const nextRowSet = new Set(nextOrder);

    const focus = captureFocusBookmark();
    const restoreFocus = focusRestorer(focus);
    const previousByKey = new Map<string, HTMLElement>();
    for (const { key } of plan.upsert) {
        const current = projection.byKey.get(key);
        if (current) previousByKey.set(key, current.cloneNode(true) as HTMLElement);
    }
    const summary: ListProjectionDeltaSummary = {
        inserted: plan.upsert.filter((operation) => !projection.byKey.has(operation.key)).length,
        updated: plan.upsert.filter((operation) => projection.byKey.has(operation.key)).length,
        deleted: plan.remove.filter((operation) => operation.cause === 'delete').length,
        projected: plan.remove.filter((operation) => operation.cause === 'project').length,
        reordered: !arraysEqual(nextOrder, projection.order),
        regions: plan.regions.map((operation) => operation.region),
    };

    try {
        for (const operation of plan.remove) {
            projection.byKey.get(operation.key)?.remove();
            projection.cardsByKey.get(operation.key)?.remove();
        }
        for (const [key, incoming] of parsedRows) {
            const current = projection.byKey.get(key);
            if (current) nextByKey.set(key, morphElement(current, incoming));
        }
        for (const [key, incoming] of parsedCards) {
            const current = projection.cardsByKey.get(key);
            if (current) nextCards.set(key, morphElement(current, incoming));
        }
        for (const region of parsedRegions) morphElement(region.current, region.incoming);

        const nextRows = nextOrder.map((key) => nextByKey.get(key) as HTMLElement);
        const nextCardEntries = nextOrder.flatMap((key) => {
            const card = nextCards.get(key);
            return card ? [[key, card] as const] : [];
        });

        if (!projection.windowed) placeElementsInOrder(mounts.tbody, nextRows);
        if (mounts.cardMount) {
            placeElementsInOrder(
                mounts.cardMount,
                nextCardEntries.map(([, card]) => card),
            );
        }

        const byKey = new Map(nextOrder.map((key, index) => [key, nextRows[index] as HTMLElement]));
        projection = {
            rows: nextRows,
            byKey,
            indexByKey: new Map(nextOrder.map((key, index) => [key, index])),
            order: nextOrder,
            cardsByKey: new Map(nextCardEntries),
            fields: projection.fields,
            modelRows: captureModelRows(nextRows),
            windowed: projection.windowed,
        };
        projectionRoot = mounts.content;
        prepared = null;
        projectionRevision += 1;
        publishModel(projection);
        if (rowModel.visibleKeys) {
            rowModel.visibleKeys = new Set(
                Array.from(rowModel.visibleKeys).filter((key) => nextRowSet.has(key)),
            );
        }
        updateLiveStatus(summary);
        return { ok: true, focusKey: focus?.key || null, previousByKey, summary, restoreFocus };
    } catch {
        restoreFocus();
        return { ok: false };
    }
}
