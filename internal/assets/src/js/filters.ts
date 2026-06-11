// filters.ts -- the v2 filter chips editor (D7, client half), migrated from
// legacy.js: free-text live match, operator chips, autocomplete, ⌫ pop,
// unknown-field hint. CSP-clean, GET-only.
//
// The editor lives INSIDE the morphed fragment (server renders the chips + the
// #ro-filter-input with a stable id), so a shareable URL lands with chips
// visible and the ignoreActiveValue morph (still configured in legacy.js's
// ro-morph handleSwap) keeps a focused draft + caret across refresh ticks. The
// client owns: the live name match (NO request until an operator chip commits),
// the chip-commit/pop requests (riding the v2 loop -- user-initiated `_table`
// GETs the server answers with the canonical HX-Push-Url), and the schema/value
// autocomplete.
//
// THE FULL ROW MODEL (D20): every matcher/frequency scan reads roRowModel, a
// capture of the COMPLETE server-rendered table -- taken from the incoming
// server fragment in the ro-morph handleSwap (before the morph, before any
// client windowing touches the DOM) and from the full document at init. The
// virtualizer (virtualizer.ts) CONSUMES roRowModel.visibleKeys to decide which
// rows are renderable; it reads that through the window.roRowModel seam, so
// THIS module depends on the virtualizer (applyLiveNameFilter calls
// virtualizeOnFilterChange), not the reverse -- no import cycle.
//
// The PURE grammar + suggestion ranking + the value-frequency scan live in
// filters-parse.ts (node-tested); this module is the DOM + dispatch around it:
// the row-model capture, the autocomplete mount, the chip-commit requests, and
// the dispatcher bindings (the chip-✕/AC-item/field click branches that headed
// the monolith's big click listener; the #ro-filter-input input branch; the
// editor keydown protocol; the AC outside-click C5).
//
// DISPATCH (the Unit 12 ordered-binding migration): the editor keydown is the
// focus-routed half of compound case 4 (listener-inventory K1 step 2): an Escape
// with focus in #ro-filter-input reaches handleFilterInputKeydown here (a no-op
// with the autocomplete closed), and the migrated palette-open keydown binding
// excludes #ro-filter-input precisely so it never closes the palette first.

import type { Binding } from './events.js';
import {
    normalizeFieldName,
    fieldSuggestionText,
    splitFilterDraft,
    filterSuggestionFields,
    filterFieldKnown,
    rankFieldSuggestions,
    rankValueSuggestions,
    liveNameMatchKeys,
    type ModelField,
    type ModelRow,
    type ACItem,
} from './filters-parse.js';
import { virtualizeOnFilterChange, virtualizerActive } from './virtualizer.js';

function getHtmx(): { ajax(method: string, path: string, opts: object): Promise<unknown> | undefined } | undefined {
    return (window as unknown as {
        htmx?: { ajax(method: string, path: string, opts: object): Promise<unknown> | undefined };
    }).htmx;
}

// roRowModel is the full server-render capture. EXPOSED as window.roRowModel
// (the seam the virtualizer reads visibleKeys through, and a documented debug
// seam) -- legacy.js's ro-morph handleSwap also reaches captureRowModel by name.
interface RowModel {
    fields: ModelField[];
    rows: ModelRow[];
    visibleKeys: Set<string> | null;
}
const roRowModel: RowModel = {
    fields: [],
    rows: [],
    visibleKeys: null,
};
(window as unknown as { roRowModel: RowModel }).roRowModel = roRowModel;

// captureRowModel reads the chips-editor model from `root` -- the incoming
// server fragment (a DocumentFragment) or the live container at init. The header
// cells carry data-hint ONLY on filterable columns (server-resolved Table
// columns), so synthetic headers (Created, Cluster, Namespace) are captured as
// alignment-only fields with hint ''. Exported for legacy.js's ro-morph
// handleSwap (the fragment capture before the morph).
export function captureRowModel(root: ParentNode): void {
    const table = root.querySelector('table.ro-table');
    if (!table) {
        roRowModel.fields = [];
        roRowModel.rows = [];
        return;
    }
    const fields: ModelField[] = [];
    table.querySelectorAll('thead th').forEach((th) => {
        const label = (th.textContent || '').trim();
        fields.push({ label, name: fieldSuggestionText(label), hint: (th as HTMLElement).dataset.hint || '' });
    });
    const rows: ModelRow[] = [];
    table.querySelectorAll('tbody tr[data-key]').forEach((tr) => {
        const cells: string[] = [];
        tr.querySelectorAll('td').forEach((td) => {
            cells.push((td.textContent || '').trim());
        });
        const nameLink = tr.querySelector('td.cell-name a');
        rows.push({
            key: (tr as HTMLElement).dataset.key as string,
            name: nameLink ? (nameLink.textContent || '').trim() : (cells[0] || ''),
            cells,
        });
    });
    roRowModel.fields = fields;
    roRowModel.rows = rows;
}

// captureRowModelFromDocument: the first paint is the full server-rendered list,
// so the live DOM IS the complete model here. Must run before the windowing init
// step (Unit 24) prunes rows -- and must NEVER re-capture once the virtualizer is
// engaged: runInit re-runs on htmx:load, and by then the DOM is a window, not the
// dataset. A runInit step (exported for legacy.js's runInit chain).
export function captureRowModelFromDocument(): void {
    const content = document.getElementById('resource-list-content');
    if (content && document.getElementById('ro-filter-input') && !virtualizerActive()) {
        captureRowModel(content);
    }
}

// ---- live free-text name match (NO request, D7) ----------------------------
const FILTER_HIDE_CLASS = 'ro-row-filtered';

// applyLiveNameFilter narrows the rows to the names containing the draft text,
// entirely client-side. The MATCH (liveNameMatchKeys, filters-parse.ts) runs on
// the full row model; only the application toggles classes on whatever rows are
// rendered. A draft containing an operator is a chip in progress -- no live
// narrowing. Exported for legacy.js's htmx:afterSwap pipeline (re-derive from the
// surviving draft after a morph).
export function applyLiveNameFilter(): void {
    const content = document.getElementById('resource-list-content');
    if (!content) {
        return;
    }
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    const draft = input ? input.value : '';
    const visible = liveNameMatchKeys(roRowModel.rows, draft);
    roRowModel.visibleKeys = visible;
    content.querySelectorAll('tbody tr[data-key]').forEach((tr) => {
        tr.classList.toggle(FILTER_HIDE_CLASS, !!visible && !visible.has((tr as HTMLElement).dataset.key as string));
    });
    // Virtualization (Unit 24/D20): the class application above only reaches the
    // rendered window -- re-window over the new visible set so a match currently
    // OUTSIDE the window becomes a rendered row.
    virtualizeOnFilterChange();
}

// ---- chip commit / pop: ride the v2 loop ------------------------------------
// issueFilterNavigation GETs the `_table` partial for a CANONICAL list href,
// sourced from the editor input -- a USER-initiated request (no RO-No-Push), so
// the in-flight guard counts it, an in-flight tick is aborted, and the server
// answers with the canonical HX-Push-Url. Falls back to a plain navigation when
// the loop is unavailable. EXPORTED: columns.ts's popover-submit binding rides
// it for the merged labelcols/selector href (the Go needle pins
// 'issueFilterNavigation(popFormMergedHref(popForm))').
export function issueFilterNavigation(href: string): void {
    const content = document.getElementById('resource-list-content');
    const input = document.getElementById('ro-filter-input');
    const htmx = getHtmx();
    if (!content || !input || !htmx) {
        window.location.assign(href);
        return;
    }
    const u = new URL(href, window.location.href);
    const partial = u.pathname.replace(/\/+$/, '') + '/_table' + u.search;
    const request = htmx.ajax('GET', partial, {
        source: input,
        target: '#resource-list-content',
        swap: 'morph',
    });
    if (request && typeof request.catch === 'function') {
        request.catch(() => {}); // failures surface via the stale banner path
    }
}

// commitFilterChip materializes the draft as a `?f=` chip. The raw value is
// encodeURIComponent with the OR-commas RESTORED raw -- typed input treats every
// comma as OR (filter.go parses alternatives on raw commas), and the `?f=` pair
// is appended by STRING CONCATENATION so sibling raw params keep their exact wire
// encoding (never URLSearchParams over the whole query).
function commitFilterChip(draft: string): void {
    const text = draft.trim();
    const parsed = splitFilterDraft(text);
    if (!parsed) {
        return; // free text never commits -- it live-matches only
    }
    if (!filterFieldKnown(roRowModel.fields, parsed.field)) {
        showFilterFieldHint();
        return;
    }
    const raw = encodeURIComponent(text).replace(/%2C/gi, ',');
    const search = window.location.search;
    const href = window.location.pathname + (search ? search + '&' : '?') + 'f=' + raw;
    clearFilterDraft();
    issueFilterNavigation(href);
}

// popLastFilterChip (⌫ on empty input) removes the LAST chip by riding its
// server-built removal href (delQueryRawValue keeps sibling chips byte-exact).
function popLastFilterChip(): void {
    const removers = document.querySelectorAll('#ro-filter-field .ro-scope-chip .chip-x');
    if (removers.length === 0) {
        return;
    }
    const href = removers[removers.length - 1].getAttribute('href');
    if (href) {
        issueFilterNavigation(href);
    }
}

function clearFilterDraft(): void {
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    if (input) {
        input.value = '';
    }
    closeFilterAC();
    applyLiveNameFilter();
}

// ---- unknown-field hint ------------------------------------------------------
// "no such field — try status, node, age…" -- the suggestion list is built from
// the ACTUAL schema (first three filterable fields) so the hint is never a lie.
function showFilterFieldHint(): void {
    const el = document.getElementById('ro-filter-error');
    if (!el) {
        return;
    }
    const names = filterSuggestionFields(roRowModel.fields).slice(0, 3).map((f) => f.text);
    el.textContent = 'no such field — try ' + (names.length ? names.join(', ') : 'status, node, age') + '…';
    (el as HTMLElement).hidden = false;
}

function hideFilterFieldHint(): void {
    const el = document.getElementById('ro-filter-error') as HTMLElement | null;
    if (el) {
        el.hidden = true;
    }
}

// ---- autocomplete -------------------------------------------------------------
// Client-side only (D7): field names (with type hints) while the draft has no
// operator; after `field:` (the equality form, on a known real column) the top 8
// distinct values by frequency from the FULL row model. The operator forms
// (!= > <) autocomplete the field then leave the value free. Tab/⏎ accepts, esc
// dismisses. All nodes are built with createElement/textContent.
let filterACItems: ACItem[] = [];
let filterACActive = -1;

function filterACOpen(): boolean {
    const ac = document.getElementById('ro-filter-ac') as HTMLElement | null;
    return !!ac && !ac.hidden;
}

function closeFilterAC(): void {
    const ac = document.getElementById('ro-filter-ac') as HTMLElement | null;
    if (ac) {
        ac.hidden = true;
        ac.textContent = '';
    }
    filterACItems = [];
    filterACActive = -1;
}

function openFilterAC(items: ACItem[]): void {
    const ac = document.getElementById('ro-filter-ac') as HTMLElement | null;
    if (!ac || items.length === 0) {
        closeFilterAC();
        return;
    }
    ac.textContent = '';
    ac.setAttribute('role', 'listbox');
    filterACItems = items;
    filterACActive = 0;
    items.forEach((item, idx) => {
        const row = document.createElement('div');
        row.className = 'ro-ac-item' + (idx === 0 ? ' active' : '');
        row.setAttribute('role', 'option');
        row.setAttribute('aria-selected', idx === 0 ? 'true' : 'false');
        row.dataset.acIndex = String(idx);
        const name = document.createElement('span');
        name.className = 'ac-name';
        name.textContent = item.label; // textContent -> hostile cell values cannot inject
        row.appendChild(name);
        if (item.hint) {
            const hint = document.createElement('span');
            hint.className = 'ac-hint';
            hint.textContent = item.hint;
            row.appendChild(hint);
        }
        row.addEventListener('mousemove', () => setFilterACActive(idx));
        ac.appendChild(row);
    });
    ac.hidden = false;
}

function setFilterACActive(index: number): void {
    if (filterACItems.length === 0) {
        return;
    }
    filterACActive = Math.max(0, Math.min(filterACItems.length - 1, index));
    const ac = document.getElementById('ro-filter-ac');
    if (!ac) {
        return;
    }
    ac.querySelectorAll('.ro-ac-item').forEach((el) => {
        const on = Number((el as HTMLElement).dataset.acIndex) === filterACActive;
        el.classList.toggle('active', on);
        el.setAttribute('aria-selected', on ? 'true' : 'false');
    });
}

function moveFilterACActive(delta: number): void {
    if (filterACItems.length === 0) {
        return;
    }
    setFilterACActive((filterACActive + delta + filterACItems.length) % filterACItems.length);
}

// updateFilterAC re-derives the dropdown from the current draft. Exported for
// legacy.js's htmx:afterSwap pipeline (re-open mid-draft after a morph).
export function updateFilterAC(): void {
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    if (!input) {
        return;
    }
    const draft = input.value;
    if (!draft.trim()) {
        closeFilterAC();
        return;
    }
    const parsed = splitFilterDraft(draft);
    if (!parsed) {
        // Field-name suggestions: substring match, prefix matches ranked first.
        openFilterAC(rankFieldSuggestions(roRowModel.fields, draft));
        return;
    }
    const isLabel = normalizeFieldName(parsed.field) === 'label';
    if (parsed.op !== ':' || isLabel || !filterFieldKnown(roRowModel.fields, parsed.field)) {
        // Operator forms leave the value free; `label` values are not in the row
        // model (metadata.labels never renders for most kinds); unknown fields
        // get the ⏎ hint, not suggestions.
        closeFilterAC();
        return;
    }
    // Top 8 distinct values by frequency, computed from the FULL row model.
    const items = rankValueSuggestions(roRowModel.fields, roRowModel.rows, parsed);
    if (items.length === 0) {
        closeFilterAC();
        return;
    }
    openFilterAC(items);
}

// acceptFilterAC fills the input with the active suggestion. Accepting a FIELD
// readies the value (`field:` + value suggestions open); accepting a complete
// VALUE is a finished chip -- ⏎ commits it directly (Tab only fills).
function acceptFilterAC(commitValues: boolean): void {
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    const item = filterACItems[filterACActive];
    if (!input || !item) {
        return;
    }
    input.value = item.insert;
    closeFilterAC();
    if (item.kind === 'value' && commitValues) {
        commitFilterChip(input.value);
    } else {
        applyLiveNameFilter();
        updateFilterAC();
    }
}

// handleFilterInputKeydown is the editor's keyboard protocol, dispatched from the
// editor keydown binding below.
function handleFilterInputKeydown(event: KeyboardEvent): void {
    const input = event.target as HTMLInputElement;
    if (event.key === 'Enter') {
        event.preventDefault();
        if (filterACOpen() && filterACActive >= 0) {
            acceptFilterAC(true);
            return;
        }
        commitFilterChip(input.value);
        return;
    }
    if (event.key === 'Tab' && filterACOpen()) {
        event.preventDefault();
        acceptFilterAC(false);
        return;
    }
    if (event.key === 'Escape' && filterACOpen()) {
        event.preventDefault();
        closeFilterAC();
        return;
    }
    if (event.key === 'ArrowDown' && filterACOpen()) {
        event.preventDefault();
        moveFilterACActive(1);
        return;
    }
    if (event.key === 'ArrowUp' && filterACOpen()) {
        event.preventDefault();
        moveFilterACActive(-1);
        return;
    }
    if (event.key === 'Backspace' && input.value === '') {
        event.preventDefault();
        popLastFilterChip();
    }
}

// --- dispatcher bindings ----------------------------------------------------
export const filtersBindings: Binding[] = [
    // Chips editor (D7): a chip's ✕ is a real link (no-JS fallback) whose href is
    // the server-built removal URL; intercept it to ride the v2 partial loop
    // (morph + canonical push) instead of a full navigation.
    {
        event: 'click',
        selector: '#ro-filter-field .chip-x',
        handler: (event, matched) => {
            event.preventDefault();
            const href = (matched as HTMLElement).getAttribute('href');
            if (href) {
                issueFilterNavigation(href);
            }
            return true;
        },
        stop: true,
    },
    // Autocomplete row: clicking accepts it (a complete value commits the chip, a
    // field fills `field:` and opens the value suggestions).
    {
        event: 'click',
        selector: '#ro-filter-ac .ro-ac-item',
        handler: (event, matched) => {
            event.preventDefault();
            setFilterACActive(Number((matched as HTMLElement).dataset.acIndex) || 0);
            acceptFilterAC(true);
            const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
            if (input) {
                input.focus();
            }
            return true;
        },
        stop: true,
    },
    // Clicking the editor field anywhere (the padding, a chip's text) lands the
    // caret in the input -- the whole field reads as one input.
    {
        event: 'click',
        selector: '#ro-filter-field',
        handler: (event, matched) => {
            const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
            if (input && event.target !== input) {
                input.focus();
            }
            // The matched #ro-filter-field already excludes the chip-✕ / AC-item
            // branches above (they stop first); returning truthy mirrors the
            // monolith's `return` after the field-focus branch.
            void matched;
            return true;
        },
        stop: true,
    },
    // C5: a click anywhere outside the editor dismisses the dropdown
    // (esc-equivalent). Independent of the others (listener-inventory C5). No
    // selector (it keys off the closest() escape).
    {
        event: 'click',
        handler: (event) => {
            if (!(event.target as Element).closest('#ro-filter-field')) {
                closeFilterAC();
            }
        },
    },
    // Chips editor (D7): every keystroke re-runs the live name match (model-
    // driven, NO request) and the autocomplete; a fresh draft clears any
    // unknown-field hint.
    {
        event: 'input',
        selector: '#ro-filter-input',
        handler: () => {
            hideFilterFieldHint();
            applyLiveNameFilter();
            updateFilterAC();
            return true;
        },
        stop: true,
    },
    // The editor keydown protocol (the focus-routed half of compound case 4):
    // #ro-filter-input owns ⏎ commit/accept, Tab accept, esc dismiss, arrows, and
    // ⌫-on-empty pop. No selector -- it keys off the focused target id, exactly
    // like the still-resident monolith keydown listener it replaces.
    {
        event: 'keydown',
        handler: (event) => {
            if ((event.target as Element).id === 'ro-filter-input') {
                handleFilterInputKeydown(event as KeyboardEvent);
            }
        },
    },
];
