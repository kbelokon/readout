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
import type {
    LiveV2DeltaRegion,
    LiveV2DeltaRemove,
    LiveV2DeltaUpsert,
    LiveV2Error,
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

interface ListProjectionDeltaOptions {
    readonly morph?: (current: HTMLElement, incoming: HTMLElement) => unknown;
    readonly reconcile: () => void;
    readonly restoreExternalState: () => void;
}

export type ListProjectionDeltaResult =
    | {
          ok: true;
          summary: ListProjectionDeltaSummary;
      }
    | { ok: false; error: LiveV2Error };

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
        indexByKey: new Map(order.map((key, index) => [key, index])),
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

// ---------------------------------------------------------------------------
// Live v2 delta transaction (internal cross-module surface).
// ---------------------------------------------------------------------------

declare const Idiomorph:
    | {
          morph(
              target: Element,
              content: Element,
              config: { morphStyle: 'outerHTML'; ignoreActiveValue: true },
          ): unknown;
      }
    | undefined;

type ProjectionMode = 'cards' | 'windowed';

const LIVE_FRAGMENT_BYTES = 128 * 1024;
const LIVE_DELTA_BYTES = 256 * 1024;
const LIVE_FRAGMENT_NODES = 4096;
const LIVE_FRAGMENT_DEPTH = 64;
const LIVE_FRAGMENT_ATTRIBUTES = 8192;
const liveTextEncoder = new TextEncoder();

interface ParsedDelta {
    candidate: ProjectionSnapshot;
    fastPath: boolean;
    modelUpdates: Map<number, ModelRow>;
    parsedRows: Map<string, HTMLElement>;
    parsedCards: Map<string, HTMLElement>;
    parsedRegions: Map<LiveV2RegionName, { current: HTMLElement; incoming: HTMLElement }>;
    summary: ListProjectionDeltaSummary;
    tbody: HTMLTableSectionElement;
    cardMount: HTMLElement | null;
}

interface ParentJournalEntry {
    parent: ParentNode;
    children: Node[];
}

interface ElementJournalEntry {
    state: DOMNodeState;
}

interface PlacementJournalEntry {
    node: Node;
    parent: ParentNode;
    nextSibling: Node | null;
}

interface DOMNodeState {
    node: Node;
    nodeValue: string | null;
    attributes: { name: string; value: string }[] | null;
    children: DOMNodeState[];
}

interface AttributeJournalEntry {
    element: HTMLElement;
    name: string;
    value: string | null;
}

interface DeltaDOMJournal {
    parents: ParentJournalEntry[];
    placements: PlacementJournalEntry[];
    elements: ElementJournalEntry[];
    attributes: AttributeJournalEntry[];
}

function projectionError(code: LiveV2Error['code'], message: string, fatal = false): LiveV2Error {
    return { code, message, fatal };
}

function oneElementRoot(parent: ParentNode): HTMLElement | null {
    let root: HTMLElement | null = null;
    for (const node of parent.childNodes) {
        if (node.nodeType === Node.TEXT_NODE && !node.textContent?.trim()) {
            continue;
        }
        if (node.nodeType !== Node.ELEMENT_NODE || root) {
            return null;
        }
        root = node as HTMLElement;
    }
    return root;
}

// Byte-for-byte JS port of web.rowDomID. Kubernetes identity keys are UTF-8
// text; only the ASCII bytes unsafe in Idiomorph's quoted id selector are
// escaped, so iterating Unicode code points preserves every non-ASCII rune.
function liveRowDOMID(key: string): string {
    let result = 'row-';
    for (const character of key) {
        const code = character.codePointAt(0) as number;
        if (
            code <= 0x20 ||
            character === '"' ||
            character === '\\' ||
            character === '%' ||
            code === 0x7f
        ) {
            result += `%${code.toString(16).toUpperCase().padStart(2, '0')}`;
        } else {
            result += character;
        }
    }
    return result;
}

function fragmentIntroducesIdentity(root: HTMLElement): boolean {
    return root.querySelector('[id], [data-ro-live-region]') !== null;
}

function fragmentIsCSPClean(root: HTMLElement): boolean {
    const forbiddenElements = 'script, style, link, iframe, object, embed, base, meta[http-equiv]';
    let nodes = 0;
    let attributes = 0;
    const pending: { node: Node; depth: number }[] = [{ node: root, depth: 1 }];
    while (pending.length > 0) {
        const current = pending.pop() as { node: Node; depth: number };
        nodes += 1;
        if (nodes > LIVE_FRAGMENT_NODES || current.depth > LIVE_FRAGMENT_DEPTH) {
            return false;
        }
        if (!(current.node instanceof Element)) continue;
        attributes += current.node.attributes.length;
        if (attributes > LIVE_FRAGMENT_ATTRIBUTES || current.node.matches(forbiddenElements)) {
            return false;
        }
        for (const attribute of Array.from(current.node.attributes)) {
            const name = attribute.name.toLowerCase();
            if (name === 'style') {
                if (
                    current.node.tagName !== 'I' ||
                    current.node.parentElement?.classList.contains('cap-bar') !== true ||
                    !/^width\s*:\s*(?:100|[0-9]{1,2})%\s*;?$/u.test(attribute.value)
                ) {
                    return false;
                }
                continue;
            }
            if (name === 'srcdoc' || name.startsWith('on')) {
                return false;
            }
            if (!['href', 'src', 'xlink:href', 'action', 'formaction'].includes(name)) {
                continue;
            }
            let normalizedURL = '';
            for (const character of attribute.value) {
                const code = character.codePointAt(0) as number;
                if (code > 0x20 && !(code >= 0x7f && code <= 0x9f)) {
                    normalizedURL += character;
                }
            }
            if (/^(?:(?:javascript|vbscript):|data:text\/html)/iu.test(normalizedURL)) {
                return false;
            }
        }
        for (const child of Array.from(current.node.childNodes)) {
            pending.push({ node: child, depth: current.depth + 1 });
        }
    }
    return true;
}

function parseRowFragment(html: string, key: string): HTMLElement | LiveV2Error {
    try {
        const table = document.createElement('table');
        const tbody = document.createElement('tbody');
        table.append(tbody);
        tbody.innerHTML = html;
        const row = oneElementRoot(tbody);
        if (
            row?.tagName !== 'TR' ||
            row.dataset.key !== key ||
            row.id !== liveRowDOMID(key) ||
            row.hasAttribute('data-ro-live-region') ||
            row.classList.contains('ro-vspacer') ||
            fragmentIntroducesIdentity(row) ||
            !fragmentIsCSPClean(row)
        ) {
            return projectionError(
                'fragment-invalid',
                `row fragment for ${key} is not one canonical keyed tr`,
            );
        }
        return row;
    } catch {
        return projectionError('fragment-invalid', `row fragment for ${key} cannot be parsed`);
    }
}

function parseCardFragment(html: string, key: string): HTMLElement | LiveV2Error {
    try {
        const template = document.createElement('template');
        template.innerHTML = html;
        const card = oneElementRoot(template.content);
        if (
            card?.tagName !== 'DIV' ||
            !card.matches('.ro-pcard[data-key]') ||
            card.dataset.key !== key ||
            card.hasAttribute('id') ||
            card.hasAttribute('data-ro-live-region') ||
            fragmentIntroducesIdentity(card) ||
            !fragmentIsCSPClean(card)
        ) {
            return projectionError(
                'fragment-invalid',
                `card fragment for ${key} is not one canonical keyed card`,
            );
        }
        return card;
    } catch {
        return projectionError('fragment-invalid', `card fragment for ${key} cannot be parsed`);
    }
}

function parseRegionFragment(
    update: LiveV2DeltaRegion,
): { current: HTMLElement; incoming: HTMLElement } | LiveV2Error {
    try {
        const mounts = document.querySelectorAll<HTMLElement>(
            `[data-ro-live-region="${update.region}"]`,
        );
        if (mounts.length !== 1) {
            return projectionError(
                'projection-mismatch',
                `region ${update.region} does not have exactly one fixed mount`,
            );
        }
        const template = document.createElement('template');
        template.innerHTML = update.html;
        const incoming = oneElementRoot(template.content);
        const expectedTag = update.region === 'phase' ? 'DIV' : 'SPAN';
        const expectedClass =
            update.region === 'count'
                ? 'ro-count'
                : update.region === 'phase'
                  ? 'ro-phase-strip'
                  : 'ro-foundline';
        if (
            !incoming ||
            incoming.tagName !== expectedTag ||
            !incoming.classList.contains(expectedClass) ||
            incoming.dataset.roLiveRegion !== update.region ||
            incoming.hasAttribute('id') ||
            mounts[0]?.tagName !== expectedTag ||
            !mounts[0]?.classList.contains(expectedClass) ||
            incoming.querySelector('[id], [data-ro-live-region]') !== null ||
            !fragmentIsCSPClean(incoming)
        ) {
            return projectionError(
                'fragment-invalid',
                `region ${update.region} is not one canonical fixed-region root`,
            );
        }
        return { current: mounts[0] as HTMLElement, incoming };
    } catch {
        return projectionError('fragment-invalid', `region ${update.region} cannot be parsed`);
    }
}

function arraysEqual(left: readonly string[], right: readonly string[]): boolean {
    return left.length === right.length && left.every((value, index) => value === right[index]);
}

function validateDeltaHTMLBounds(plan: ListProjectionDeltaPlan): LiveV2Error | null {
    let aggregate = 0;
    const fragments = [
        ...plan.upsert.flatMap((operation) =>
            operation.card === undefined ? [operation.row] : [operation.row, operation.card],
        ),
        ...plan.regions.map((operation) => operation.html),
    ];
    for (const html of fragments) {
        if (html.length > LIVE_FRAGMENT_BYTES) {
            return projectionError('limit-exceeded', 'Live delta fragment exceeds its limit');
        }
        const bytes = liveTextEncoder.encode(html).byteLength;
        if (bytes > LIVE_FRAGMENT_BYTES) {
            return projectionError('limit-exceeded', 'Live delta fragment exceeds its limit');
        }
        aggregate += bytes;
        if (aggregate > LIVE_DELTA_BYTES) {
            return projectionError(
                'limit-exceeded',
                'Live delta fragments exceed their aggregate limit',
            );
        }
    }
    return null;
}

function currentProjectionMode(): ProjectionMode | LiveV2Error {
    if (projection.rows.length === 0) {
        return projectionError('projection-mismatch', 'empty projections require a snapshot');
    }
    if (projection.windowed) {
        return projection.cardsByKey.size === 0
            ? 'windowed'
            : projectionError('projection-mismatch', 'windowed projection unexpectedly has cards');
    }
    if (projection.cardsByKey.size === projection.rows.length) {
        return 'cards';
    }
    return projectionError('projection-mismatch', 'projection mode is not delta-capable');
}

function validateCurrentProjection(
    content: HTMLElement,
    fastPath: boolean,
):
    | { mode: ProjectionMode; tbody: HTMLTableSectionElement; cardMount: HTMLElement | null }
    | LiveV2Error {
    if (prepared || projectionRoot !== content || !content.isConnected) {
        return projectionError('projection-mismatch', 'canonical projection is not stably mounted');
    }
    const mode = currentProjectionMode();
    if (typeof mode !== 'string') return mode;
    const tables = content.querySelectorAll<HTMLTableElement>('table.ro-table');
    const tbody = tables.length === 1 ? tables[0]?.tBodies.item(0) : null;
    if (!tbody) {
        return projectionError('projection-mismatch', 'projection table mount is ambiguous');
    }
    if (!fastPath) {
        const orderSet = new Set(projection.order);
        if (
            orderSet.size !== projection.order.length ||
            projection.rows.length !== projection.order.length ||
            projection.byKey.size !== projection.order.length ||
            projection.indexByKey.size !== projection.order.length ||
            projection.modelRows.length !== projection.order.length ||
            projection.order.some(
                (key, index) =>
                    projection.byKey.get(key) !== projection.rows[index] ||
                    projection.indexByKey.get(key) !== index ||
                    projection.rows[index]?.dataset.key !== key ||
                    projection.rows[index]?.id !== liveRowDOMID(key) ||
                    projection.modelRows[index]?.key !== key,
            )
        ) {
            return projectionError(
                'projection-mismatch',
                'canonical projection invariants are broken',
            );
        }
    }
    let cardMount: HTMLElement | null = null;
    if (mode === 'cards') {
        const mounts = content.querySelectorAll<HTMLElement>('.ro-cardlist');
        if (mounts.length !== 1) {
            return projectionError('projection-mismatch', 'card mount is ambiguous');
        }
        cardMount = mounts[0] as HTMLElement;
        if (
            !fastPath &&
            projection.order.some((key) => {
                const card = projection.cardsByKey.get(key);
                return !card || card.dataset.key !== key || card.parentElement !== cardMount;
            })
        ) {
            return projectionError(
                'projection-mismatch',
                'canonical keyed-card invariants are broken',
            );
        }
        if (!fastPath && projection.rows.some((row) => row.parentElement !== tbody)) {
            return projectionError('projection-mismatch', 'small-list rows are not fully mounted');
        }
    } else if (
        !fastPath &&
        projection.rows.some((row) => row.isConnected && row.parentElement !== tbody)
    ) {
        return projectionError('projection-mismatch', 'windowed rows are mounted outside tbody');
    }
    return { mode, tbody, cardMount };
}

function prepareDelta(plan: ListProjectionDeltaPlan): ParsedDelta | LiveV2Error {
    const boundsError = validateDeltaHTMLBounds(plan);
    if (boundsError) return boundsError;
    const fastPath =
        plan.remove.length === 0 &&
        plan.order === undefined &&
        plan.upsert.every((operation) => projection.byKey.has(operation.key));
    const content = document.getElementById('resource-list-content');
    if (!content) {
        return projectionError('projection-mismatch', 'resource list content is missing');
    }
    const current = validateCurrentProjection(content, fastPath);
    if ('code' in current) return current;

    const removed = new Set<string>();
    let deleted = 0;
    let projected = 0;
    for (const operation of plan.remove) {
        if (removed.has(operation.key)) {
            return projectionError(
                'projection-mismatch',
                `remove key ${operation.key} is duplicate`,
            );
        }
        if (!projection.byKey.has(operation.key)) {
            return projectionError('projection-mismatch', `remove key ${operation.key} is absent`);
        }
        removed.add(operation.key);
        if (operation.cause === 'delete') deleted += 1;
        else projected += 1;
    }

    const parsedRows = new Map<string, HTMLElement>();
    const parsedCards = new Map<string, HTMLElement>();
    let inserted = 0;
    let updated = 0;
    for (const operation of plan.upsert) {
        if (parsedRows.has(operation.key) || removed.has(operation.key)) {
            return projectionError(
                'projection-mismatch',
                `upsert key ${operation.key} is duplicate or also removed`,
            );
        }
        const row = parseRowFragment(operation.row, operation.key);
        if (!('dataset' in row)) return row;
        const existingRow = projection.byKey.get(operation.key);
        if (existingRow && existingRow.id !== row.id) {
            return projectionError(
                'fragment-invalid',
                `row fragment for ${operation.key} changed its canonical id`,
            );
        }
        if (existingRow) {
            const index = projection.indexByKey.get(operation.key);
            if (
                index === undefined ||
                projection.rows[index] !== existingRow ||
                projection.modelRows[index]?.key !== operation.key ||
                (existingRow.isConnected && existingRow.parentElement !== current.tbody)
            ) {
                return projectionError(
                    'projection-mismatch',
                    `row ${operation.key} is not at its canonical index`,
                );
            }
        }
        const globalMatches = document.querySelectorAll<HTMLElement>(`[id="${row.id}"]`);
        if (
            (existingRow?.isConnected &&
                (globalMatches.length !== 1 || globalMatches[0] !== existingRow)) ||
            (existingRow && !existingRow.isConnected && globalMatches.length !== 0) ||
            (!existingRow && globalMatches.length !== 0)
        ) {
            return projectionError(
                'fragment-invalid',
                `row fragment for ${operation.key} collides with a document id`,
            );
        }
        parsedRows.set(operation.key, row);
        if (current.mode === 'cards') {
            if (operation.card === undefined) {
                return projectionError(
                    'fragment-invalid',
                    `card-mode upsert ${operation.key} is missing its card`,
                );
            }
            const card = parseCardFragment(operation.card, operation.key);
            if (!('dataset' in card)) return card;
            const existingCard = projection.cardsByKey.get(operation.key);
            if (
                existingRow &&
                (!existingCard ||
                    existingCard.dataset.key !== operation.key ||
                    existingCard.parentElement !== current.cardMount)
            ) {
                return projectionError(
                    'projection-mismatch',
                    `card ${operation.key} is not canonically mounted`,
                );
            }
            parsedCards.set(operation.key, card);
        } else if (operation.card !== undefined) {
            return projectionError(
                'fragment-invalid',
                `windowed upsert ${operation.key} unexpectedly carries a card`,
            );
        }
        if (projection.byKey.has(operation.key)) updated += 1;
        else inserted += 1;
    }

    const parsedRegions = new Map<
        LiveV2RegionName,
        { current: HTMLElement; incoming: HTMLElement }
    >();
    for (const update of plan.regions) {
        if (parsedRegions.has(update.region)) {
            return projectionError('projection-mismatch', `region ${update.region} is duplicate`);
        }
        const parsed = parseRegionFragment(update);
        if ('code' in parsed) return parsed;
        parsedRegions.set(update.region, parsed);
    }

    if (fastPath) {
        const modelUpdates = new Map<number, ModelRow>();
        for (const [key, incoming] of parsedRows) {
            modelUpdates.set(projection.indexByKey.get(key) as number, captureModelRow(incoming));
        }
        return {
            candidate: { ...projection },
            fastPath: true,
            modelUpdates,
            parsedRows,
            parsedCards,
            parsedRegions,
            summary: {
                inserted: 0,
                updated,
                deleted: 0,
                projected: 0,
                reordered: false,
                regions: [...parsedRegions.keys()],
            },
            tbody: current.tbody,
            cardMount: current.cardMount,
        };
    }

    const finalKeys = new Set(projection.order.filter((key) => !removed.has(key)));
    for (const key of parsedRows.keys()) finalKeys.add(key);
    if (finalKeys.size === 0) {
        return projectionError(
            'projection-mismatch',
            'empty projection boundary requires snapshot',
        );
    }
    const topologyChanged = removed.size > 0 || inserted > 0;
    if (topologyChanged && plan.order === undefined) {
        return projectionError(
            'projection-mismatch',
            'topology-changing delta requires full order',
        );
    }
    const finalOrder = plan.order ? [...plan.order] : [...projection.order];
    if (
        finalOrder.length !== finalKeys.size ||
        new Set(finalOrder).size !== finalOrder.length ||
        finalOrder.some((key) => !finalKeys.has(key))
    ) {
        return projectionError('projection-mismatch', 'delta order is not the exact final key set');
    }
    if (plan.order && !topologyChanged && arraysEqual(plan.order, projection.order)) {
        return projectionError('projection-mismatch', 'redundant unchanged order is not allowed');
    }

    const candidateByKey = new Map(projection.byKey);
    const candidateCards = new Map(projection.cardsByKey);
    for (const key of removed) {
        candidateByKey.delete(key);
        candidateCards.delete(key);
    }
    for (const [key, incoming] of parsedRows) {
        const old = projection.byKey.get(key);
        candidateByKey.set(key, old?.isConnected ? old : incoming);
    }
    for (const [key, incoming] of parsedCards) {
        const old = projection.cardsByKey.get(key);
        candidateCards.set(key, old?.isConnected ? old : incoming);
    }
    const rows = finalOrder.map((key) => candidateByKey.get(key) as HTMLElement);
    const modelByKey = new Map(projection.modelRows.map((model) => [model.key, model]));
    for (const [key, incoming] of parsedRows) modelByKey.set(key, captureModelRow(incoming));
    for (const key of removed) modelByKey.delete(key);
    const modelRows = finalOrder.map((key) => modelByKey.get(key) as ModelRow);
    if (rows.some((row) => !row) || modelRows.some((row) => !row)) {
        return projectionError('projection-mismatch', 'delta candidate is incomplete');
    }

    const ids = new Set<string>();
    for (const key of finalOrder) {
        // Audit the node that will actually own this key after commit. For an
        // updated connected row that is the existing identity-preserved root,
        // not the detached incoming parse tree.
        const row = candidateByKey.get(key);
        if (!row || row.id !== liveRowDOMID(key) || ids.has(row.id)) {
            return projectionError('fragment-invalid', 'row ids are missing or duplicate');
        }
        // Structural/reorder preflight is intentionally O(final rows): unlike
        // the existing-upsert fast path, it must audit every final identity so
        // an external duplicate on an untouched row cannot poison Idiomorph's
        // document-wide id matching.
        const globalMatches = document.querySelectorAll<HTMLElement>(`[id="${row.id}"]`);
        if (
            (row.isConnected && (globalMatches.length !== 1 || globalMatches[0] !== row)) ||
            (!row.isConnected && globalMatches.length !== 0)
        ) {
            return projectionError(
                'fragment-invalid',
                `final row ${key} collides with a document id`,
            );
        }
        ids.add(row.id);
    }
    return {
        candidate: {
            rows,
            byKey: candidateByKey,
            indexByKey: new Map(finalOrder.map((key, index) => [key, index])),
            order: finalOrder,
            cardsByKey: candidateCards,
            fields: projection.fields.map((field) => ({ ...field })),
            modelRows,
            windowed: projection.windowed,
        },
        fastPath: false,
        modelUpdates: new Map(),
        parsedRows,
        parsedCards,
        parsedRegions,
        summary: {
            inserted,
            updated,
            deleted,
            projected,
            reordered: !arraysEqual(finalOrder, projection.order),
            regions: [...parsedRegions.keys()],
        },
        tbody: current.tbody,
        cardMount: current.cardMount,
    };
}

function addParentJournal(
    entries: ParentJournalEntry[],
    seen: Set<ParentNode>,
    parent: ParentNode | null,
): void {
    if (parent && !seen.has(parent)) {
        seen.add(parent);
        entries.push({ parent, children: Array.from(parent.childNodes) });
    }
}

function addElementJournal(
    entries: ElementJournalEntry[],
    seen: Set<HTMLElement>,
    element: HTMLElement,
): void {
    if (!seen.has(element)) {
        seen.add(element);
        entries.push({ state: captureDOMNode(element) });
    }
}

function addPlacementJournal(entries: PlacementJournalEntry[], seen: Set<Node>, node: Node): void {
    const parent = node.parentNode;
    if (parent && !seen.has(node)) {
        seen.add(node);
        entries.push({ node, parent, nextSibling: node.nextSibling });
    }
}

function captureDOMNode(node: Node): DOMNodeState {
    return {
        node,
        nodeValue: node.nodeValue,
        attributes:
            node instanceof Element
                ? Array.from(node.attributes, (attribute) => ({
                      name: attribute.name,
                      value: attribute.value,
                  }))
                : null,
        children: Array.from(node.childNodes, captureDOMNode),
    };
}

function addAttributeJournal(
    entries: AttributeJournalEntry[],
    seen: Map<HTMLElement, Set<string>>,
    element: HTMLElement,
    name: string,
): void {
    const names = seen.get(element) || new Set<string>();
    if (names.has(name)) return;
    names.add(name);
    seen.set(element, names);
    entries.push({ element, name, value: element.getAttribute(name) });
}

function createDOMJournal(parsed: ParsedDelta): DeltaDOMJournal {
    const parents: ParentJournalEntry[] = [];
    const placements: PlacementJournalEntry[] = [];
    const elements: ElementJournalEntry[] = [];
    const attributes: AttributeJournalEntry[] = [];
    const seenParents = new Set<ParentNode>();
    const seenPlacements = new Set<Node>();
    const seenElements = new Set<HTMLElement>();
    const seenAttributes = new Map<HTMLElement, Set<string>>();
    // Structural deltas replace the full canonical order. A fast small-list
    // upsert morphs roots in place, so targeted parent/nextSibling checkpoints
    // below avoid copying every tbody/card child merely to update one row.
    if (!parsed.fastPath || projection.windowed) {
        addParentJournal(parents, seenParents, parsed.tbody);
    }
    parsed.tbody.querySelectorAll<HTMLElement>(':scope > tr.ro-vspacer').forEach((spacer) => {
        addElementJournal(elements, seenElements, spacer);
    });
    if (parsed.cardMount && !parsed.fastPath) {
        addParentJournal(parents, seenParents, parsed.cardMount);
    }

    for (const key of parsed.parsedRows.keys()) {
        const current = projection.byKey.get(key);
        if (current) {
            if (parsed.fastPath) {
                addPlacementJournal(placements, seenPlacements, current);
            }
            addElementJournal(elements, seenElements, current);
        }
    }
    for (const key of parsed.parsedCards.keys()) {
        const current = projection.cardsByKey.get(key);
        if (current?.isConnected) {
            if (parsed.fastPath) {
                addPlacementJournal(placements, seenPlacements, current);
            }
            addElementJournal(elements, seenElements, current);
        }
    }
    for (const { current } of parsed.parsedRegions.values()) {
        addParentJournal(parents, seenParents, current.parentNode);
        addElementJournal(elements, seenElements, current);
    }
    if (!parsed.fastPath) {
        for (const element of [...projection.rows, ...projection.cardsByKey.values()]) {
            addAttributeJournal(attributes, seenAttributes, element, 'class');
        }
    }
    document.querySelectorAll<HTMLElement>('.ro-table-wrap').forEach((wrap) => {
        addAttributeJournal(attributes, seenAttributes, wrap, 'aria-activedescendant');
    });
    const status = document.getElementById('ro-live-status');
    if (status) {
        addParentJournal(parents, seenParents, status.parentNode);
        addElementJournal(elements, seenElements, status);
    }
    const bulk = document.getElementById('ro-bulkbar');
    if (bulk) {
        addParentJournal(parents, seenParents, bulk.parentNode);
        addElementJournal(elements, seenElements, bulk);
    }
    return { parents, placements, elements, attributes };
}

function restoreDOMNode(state: DOMNodeState): void {
    const { node } = state;
    if (node instanceof Element && state.attributes) {
        for (const attribute of Array.from(node.attributes)) node.removeAttribute(attribute.name);
        for (const attribute of state.attributes) {
            node.setAttribute(attribute.name, attribute.value);
        }
    } else if (node.nodeValue !== state.nodeValue) {
        node.nodeValue = state.nodeValue;
    }
    if (node instanceof Element) {
        node.replaceChildren(...state.children.map((child) => child.node));
    }
    for (const child of state.children) restoreDOMNode(child);
}

function verifyDOMNode(state: DOMNodeState): boolean {
    const { node } = state;
    if (node.nodeValue !== state.nodeValue) return false;
    if (node instanceof Element && state.attributes) {
        if (node.attributes.length !== state.attributes.length) return false;
        for (const attribute of state.attributes) {
            if (node.getAttribute(attribute.name) !== attribute.value) return false;
        }
    }
    const children = Array.from(node.childNodes);
    return (
        children.length === state.children.length &&
        children.every((child, index) => child === state.children[index]?.node) &&
        state.children.every(verifyDOMNode)
    );
}

function restorePlacementJournal(entries: readonly PlacementJournalEntry[]): void {
    const byNode = new Map(entries.map((entry) => [entry.node, entry]));
    const restored = new Set<Node>();
    const restoring = new Set<Node>();
    const restore = (entry: PlacementJournalEntry): void => {
        if (restored.has(entry.node)) return;
        if (restoring.has(entry.node)) throw new Error('original sibling order is cyclic');
        if (entry.parent instanceof Element && !entry.parent.isConnected) {
            throw new Error('an original parent mount disappeared');
        }
        restoring.add(entry.node);
        if (entry.nextSibling) {
            const dependency = byNode.get(entry.nextSibling);
            if (dependency) restore(dependency);
            else if (entry.nextSibling.parentNode !== entry.parent) {
                throw new Error('an original sibling disappeared');
            }
        }
        entry.parent.insertBefore(entry.node, entry.nextSibling);
        restoring.delete(entry.node);
        restored.add(entry.node);
    };
    for (const entry of entries) restore(entry);
    if (
        entries.some(
            ({ node, parent, nextSibling }) =>
                node.parentNode !== parent || node.nextSibling !== nextSibling,
        )
    ) {
        throw new Error('original root placement could not be restored');
    }
}

function restoreDOMJournal(journal: DeltaDOMJournal): void {
    if (
        journal.parents.some(
            ({ parent }) => parent instanceof Element && parent.isConnected === false,
        )
    ) {
        throw new Error('an original parent mount disappeared');
    }
    for (const { parent, children } of journal.parents) parent.replaceChildren(...children);
    restorePlacementJournal(journal.placements);
    for (const { state } of journal.elements) restoreDOMNode(state);
    for (const { element, name, value } of journal.attributes) {
        if (value === null) element.removeAttribute(name);
        else element.setAttribute(name, value);
    }
    for (const { parent, children } of journal.parents) {
        const restored = Array.from(parent.childNodes);
        if (
            restored.length !== children.length ||
            restored.some((child, index) => child !== children[index])
        ) {
            throw new Error('original child order could not be restored');
        }
    }
    if (journal.elements.some(({ state }) => !verifyDOMNode(state))) {
        throw new Error('original descendant identity could not be restored');
    }
}

function resolveMorph(
    override: ListProjectionDeltaOptions['morph'],
): ((current: HTMLElement, incoming: HTMLElement) => void) | null {
    if (override) return override;
    if (typeof Idiomorph === 'undefined' || typeof Idiomorph.morph !== 'function') return null;
    return (current, incoming) => {
        Idiomorph.morph(current, incoming, {
            morphStyle: 'outerHTML',
            ignoreActiveValue: true,
        });
    };
}

const MORPH_TRANSIENT_CLASSES = ['is-selected', 'kfocus', 'ro-row-filtered', 'ro-cell-changed'];

function canonicalMorphClone(element: HTMLElement): HTMLElement {
    const clone = element.cloneNode(true) as HTMLElement;
    for (const current of [clone, ...Array.from(clone.querySelectorAll<HTMLElement>('*'))]) {
        current.classList.remove(...MORPH_TRANSIENT_CLASSES);
        if (current.getAttribute('class') === '') current.removeAttribute('class');
    }
    return clone;
}

function morphLandedCanonical(current: HTMLElement, incoming: HTMLElement): boolean {
    return canonicalMorphClone(current).isEqualNode(canonicalMorphClone(incoming));
}

function runScopedMorph(
    morph: (current: HTMLElement, incoming: HTMLElement) => unknown,
    current: HTMLElement,
    incoming: HTMLElement,
): void {
    const parent = current.parentNode;
    const next = current.nextSibling;
    const connected = current.isConnected;
    const outcome = morph(current, incoming);
    if (!connected) {
        if (parent) {
            parent.insertBefore(current, next?.parentNode === parent ? next : null);
        } else {
            current.remove();
        }
    }
    if (
        current.isConnected !== connected ||
        current.parentNode !== parent ||
        current.nextSibling !== next ||
        outcome === false ||
        !morphLandedCanonical(current, incoming)
    ) {
        throw new Error('scoped morph did not land canonical content in place');
    }
}

function updateLiveStatus(summary: ListProjectionDeltaSummary): void {
    const status = document.getElementById('ro-live-status');
    if (!status) return;
    const changed = summary.inserted + summary.updated + summary.deleted + summary.projected;
    const regionCount = summary.regions.length;
    const parts: string[] = [];
    if (changed > 0) parts.push(`${changed} row${changed === 1 ? '' : 's'}`);
    if (summary.reordered) parts.push('order changed');
    if (regionCount > 0) {
        parts.push(`${regionCount} region${regionCount === 1 ? '' : 's'}`);
    }
    status.textContent = `Live update: ${parts.join(', ')}`;
}

// @internal The transport-facing public boundary is applyLiveV2Delta in
// live-protocol.ts.  Keeping the callback inside this function makes projection
// publication + all derived-view reconciliation one synchronous transaction.
export function applyListProjectionDeltaTransaction(
    plan: ListProjectionDeltaPlan,
    options: ListProjectionDeltaOptions,
): ListProjectionDeltaResult {
    let parsed: ParsedDelta | LiveV2Error;
    try {
        parsed = prepareDelta(plan);
    } catch {
        return {
            ok: false,
            error: projectionError('projection-mismatch', 'Live delta preflight failed'),
        };
    }
    if ('code' in parsed) return { ok: false, error: parsed };

    const morphNeeded =
        [...parsed.parsedRows.keys()].some(
            (key) => parsed.candidate.byKey.get(key) === projection.byKey.get(key),
        ) ||
        [...parsed.parsedCards.keys()].some(
            (key) => parsed.candidate.cardsByKey.get(key) === projection.cardsByKey.get(key),
        ) ||
        parsed.parsedRegions.size > 0;
    const morph = resolveMorph(options.morph);
    if (morphNeeded && !morph) {
        return {
            ok: false,
            error: projectionError('morph-failed', 'Idiomorph is unavailable'),
        };
    }

    const oldProjection = projection;
    const oldPrepared = prepared;
    const oldRoot = projectionRoot;
    const oldRevision = projectionRevision;
    const oldFields = rowModel.fields;
    const oldModelRows = rowModel.rows;
    const oldVisibleKeys = rowModel.visibleKeys;
    const modelPatchJournal: { index: number; model: ModelRow }[] = [];
    let journal: DeltaDOMJournal;
    try {
        journal = createDOMJournal(parsed);
    } catch {
        return {
            ok: false,
            error: projectionError('projection-mismatch', 'Live delta journal could not be built'),
        };
    }
    let mutationPhase: 'morph' | 'reconcile' = 'morph';
    try {
        for (const [key, incoming] of parsed.parsedRows) {
            const current = oldProjection.byKey.get(key);
            if (current && parsed.candidate.byKey.get(key) === current) {
                runScopedMorph(morph as NonNullable<typeof morph>, current, incoming);
                if (current.dataset.key !== key) {
                    throw new Error(`row morph did not preserve ${key}`);
                }
            }
        }
        for (const [key, incoming] of parsed.parsedCards) {
            const current = oldProjection.cardsByKey.get(key);
            if (current && parsed.candidate.cardsByKey.get(key) === current) {
                runScopedMorph(morph as NonNullable<typeof morph>, current, incoming);
                if (current.dataset.key !== key) {
                    throw new Error(`card morph did not preserve ${key}`);
                }
            }
        }
        for (const { current, incoming } of parsed.parsedRegions.values()) {
            runScopedMorph(morph as NonNullable<typeof morph>, current, incoming);
            if (current.dataset.roLiveRegion !== incoming.dataset.roLiveRegion) {
                throw new Error('region morph did not preserve its fixed mount');
            }
        }

        if (!oldProjection.windowed) {
            parsed.tbody.replaceChildren(...parsed.candidate.rows);
            (parsed.cardMount as HTMLElement).replaceChildren(
                ...parsed.candidate.order.map(
                    (key) => parsed.candidate.cardsByKey.get(key) as HTMLElement,
                ),
            );
        }
        for (const [index, model] of parsed.modelUpdates) {
            modelPatchJournal.push({ index, model: parsed.candidate.modelRows[index] as ModelRow });
            parsed.candidate.modelRows[index] = model;
        }
        projection = parsed.candidate;
        projectionRoot = document.getElementById('resource-list-content');
        prepared = null;
        projectionRevision = oldRevision + 1;
        publishModel(projection);

        mutationPhase = 'reconcile';
        options.reconcile();
        updateLiveStatus(parsed.summary);
        return {
            ok: true,
            summary: parsed.summary,
        };
    } catch {
        let rollbackFailed = false;
        try {
            restoreDOMJournal(journal);
        } catch {
            rollbackFailed = true;
        }
        projection = oldProjection;
        for (const patch of modelPatchJournal) oldProjection.modelRows[patch.index] = patch.model;
        prepared = oldPrepared;
        projectionRoot = oldRoot;
        projectionRevision = oldRevision;
        rowModel.fields = oldFields;
        rowModel.rows = oldModelRows;
        rowModel.visibleKeys = oldVisibleKeys;
        try {
            options.restoreExternalState();
        } catch {
            rollbackFailed = true;
        }
        if (rollbackFailed) {
            return {
                ok: false,
                error: projectionError(
                    'rollback-failed',
                    'Live delta rollback could not restore the original mounts',
                    true,
                ),
            };
        }
        return {
            ok: false,
            error: projectionError(
                mutationPhase === 'morph' ? 'morph-failed' : 'reconcile-failed',
                mutationPhase === 'morph'
                    ? 'Live delta DOM morph failed and was rolled back'
                    : 'Live delta reconcile failed and was rolled back',
            ),
        };
    }
}
