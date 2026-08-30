// filters.ts -- the v2 filter chips editor (client half), migrated from
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
// THE FULL ROW MODEL: every matcher/frequency scan reads the stable facade owned
// by list-projection.ts. That module captures the COMPLETE server-rendered table
// before any client windowing touches it; filters and the virtualizer are peers
// consuming the same canonical snapshot rather than owning overlapping copies.
//
// The PURE grammar + suggestion ranking + the value-frequency scan live in
// filters-parse.ts (unit-tested); this module is the DOM + dispatch around it:
// the row-model capture, the autocomplete mount, the chip-commit requests, and
// the dispatcher bindings (the chip-✕/AC-item/field click branches that headed
// the monolith's big click listener; the #ro-filter-input input branch; the
// editor keydown protocol; the AC outside-click C5).
//
// DISPATCH (the ordered-binding migration): the editor keydown is the
// focus-routed half of compound case 4 (listener-inventory K1 step 2): an Escape
// with focus in #ro-filter-input reaches handleFilterInputKeydown here (a no-op
// with the autocomplete closed), and the migrated palette-open keydown binding
// excludes #ro-filter-input precisely so it never closes the palette first.

import type { Binding } from './events.js';
import {
    type ACItem,
    filterFieldKnown,
    filterSuggestionFields,
    liveNameMatchKeys,
    rankFieldSuggestions,
    rankValueSuggestions,
    splitFilterDraft,
    trimFilterWhitespace,
} from './filters-parse.js';
import {
    adoptListProjection,
    ensureListProjection,
    listProjectionRevision,
    listProjectionRowModel,
    setListProjectionVisibleKeys,
} from './list-projection.js';
import { virtualizeOnFilterChange, virtualizerActive } from './virtualizer.js';

function getHtmx():
    | { ajax(method: string, path: string, opts: object): Promise<unknown> | undefined }
    | undefined {
    return (
        window as unknown as {
            htmx?: {
                ajax(method: string, path: string, opts: object): Promise<unknown> | undefined;
            };
        }
    ).htmx;
}

// Stable full-model facade. list-projection.ts owns and exposes the same object
// as window.roRowModel for the documented debug/e2e seam.
const roRowModel = listProjectionRowModel();

// Compatibility wrapper retained for callers/tests that capture the chips
// editor model directly.  The canonical projection now performs the capture.
export function captureRowModel(root: ParentNode): void {
    adoptListProjection(root);
}

// captureRowModelFromDocument: the first paint is the full server-rendered list,
// so the live DOM IS the complete model here. Must run before the windowing init
// step (windowing) prunes rows -- and must NEVER re-capture once the virtualizer
// is engaged, because the DOM is then a window, not the dataset. The guard keeps
// this exported runInit step safe under any defensive repeat call.
export function captureRowModelFromDocument(): void {
    const content = document.getElementById('resource-list-content');
    if (content && !virtualizerActive()) {
        ensureListProjection(content);
    }
}

// ---- live free-text name match (NO request) --------------------------------
const FILTER_HIDE_CLASS = 'ro-row-filtered';

interface AppliedLiveFilter {
    content: HTMLElement;
    draft: string;
    revision: number;
}

let appliedLiveFilter: AppliedLiveFilter | null = null;

// applyLiveNameFilter narrows the rows to the names containing the draft text,
// entirely client-side. The MATCH (liveNameMatchKeys, filters-parse.ts) runs on
// the full row model; only the application toggles classes on whatever rows are
// rendered. A draft containing an operator is a chip in progress -- no live
// narrowing. The shared post-list-update pipeline re-derives it after changes.
export function applyLiveNameFilter(): void {
    const content = document.getElementById('resource-list-content');
    if (!content) {
        return;
    }
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    const draft = input ? input.value : '';
    const revision = listProjectionRevision();
    if (
        appliedLiveFilter?.content === content &&
        appliedLiveFilter.draft === draft &&
        appliedLiveFilter.revision === revision
    ) {
        return;
    }
    // An EMPTY model is "we know nothing", not "zero matches". Two paths empty
    // it while rendered rows survive: the fail-closed delta reset (its removes
    // already ran, so the projection is dropped) and a history-restored
    // windowed slice awaiting its rebuild. Matching against no rows returns an
    // empty set, which would hide every surviving row (display: none) with no
    // banner and no way back -- the same blanking virtualizeReset already
    // guards on the geometry side. No model -> no narrowing, and the toggle
    // below clears any hide class the rows carried in.
    const visible = roRowModel.rows.length ? liveNameMatchKeys(roRowModel.rows, draft) : null;
    setListProjectionVisibleKeys(visible);
    content
        .querySelectorAll<HTMLElement>('tbody tr[data-key], .ro-cardlist > .ro-pcard[data-key]')
        .forEach((item) => {
            item.classList.toggle(
                FILTER_HIDE_CLASS,
                !!visible && !visible.has(item.dataset.key as string),
            );
        });
    // Virtualization: the class application above only reaches the
    // rendered window -- re-window over the new visible set so a match currently
    // OUTSIDE the window becomes a rendered row.
    virtualizeOnFilterChange();
    appliedLiveFilter = { content, draft, revision };
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
    const partial = `${u.pathname.replace(/\/+$/, '')}/_table${u.search}`;
    const request = htmx.ajax('GET', partial, {
        source: input,
        target: '#resource-list-content',
        swap: 'morph',
    });
    // failures surface through the stale-banner lifecycle; attach a rejection
    // handler when htmx returns its optional request promise.
    void request?.catch(() => {});
}

// commitFilterChip materializes the draft as a `?f=` chip. The raw value is
// encodeURIComponent with the OR-commas RESTORED raw -- typed input treats every
// comma as OR (filter.go parses alternatives on raw commas), and the `?f=` pair
// is appended by STRING CONCATENATION so sibling raw params keep their exact wire
// encoding (never URLSearchParams over the whole query).
function commitFilterChip(draft: string): void {
    const text = trimFilterWhitespace(draft);
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
    const href = `${window.location.pathname + (search ? `${search}&` : '?')}f=${raw}`;
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
    const names = filterSuggestionFields(roRowModel.fields)
        .slice(0, 3)
        .map((f) => f.text);
    // filterSuggestionFields always contributes the virtual `label` field, so
    // the schema-derived list is never empty and needs no fictional fallback.
    el.textContent = `no such field — try ${names.join(', ')}…`;
    (el as HTMLElement).hidden = false;
}

function hideFilterFieldHint(): void {
    const el = document.getElementById('ro-filter-error') as HTMLElement | null;
    if (el) {
        el.hidden = true;
    }
}

// ---- autocomplete -------------------------------------------------------------
// Client-side only: field names (with type hints) while the draft has no
// operator; after `field:` (the equality form, on a known real column) the top 8
// distinct values by frequency from the FULL row model. The operator forms
// (!= > <) autocomplete the field then leave the value free. Tab/⏎ accepts, esc
// dismisses. All nodes are built with createElement/textContent.
let filterACItems: ACItem[] = [];
let filterACActive = 0;

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
    filterACActive = 0;
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
        row.className = `ro-ac-item${idx === 0 ? ' active' : ''}`;
        row.dataset.roAction = 'pick-suggestion';
        row.setAttribute('role', 'option');
        row.setAttribute('aria-selected', idx === 0 ? 'true' : 'false');
        row.dataset.acIndex = String(idx);
        const name = document.createElement('span');
        name.className = 'ac-name';
        name.textContent = item.label; // textContent -> hostile cell values cannot inject
        row.appendChild(name);
        // Every field/value candidate produced by filters-parse carries a
        // meaningful hint (type or frequency), so render the shape uniformly.
        const hint = document.createElement('span');
        hint.className = 'ac-hint';
        hint.textContent = item.hint;
        row.appendChild(hint);
        row.addEventListener('mousemove', () => setFilterACActive(idx));
        ac.appendChild(row);
    });
    ac.hidden = false;
}

function setFilterACActive(index: number): void {
    filterACActive = Math.max(0, Math.min(filterACItems.length - 1, index));
    const ac = document.getElementById('ro-filter-ac');
    if (!ac) {
        return;
    }
    ac.querySelectorAll('[data-ro-action="pick-suggestion"]').forEach((el) => {
        const on = Number((el as HTMLElement).dataset.acIndex) === filterACActive;
        el.classList.toggle('active', on);
        el.setAttribute('aria-selected', on ? 'true' : 'false');
    });
}

function moveFilterACActive(delta: number): void {
    setFilterACActive((filterACActive + delta + filterACItems.length) % filterACItems.length);
}

// updateFilterAC re-derives the dropdown from the current draft. Exported for
// the shared post-list-update pipeline (re-open mid-draft after a change).
export function updateFilterAC(): void {
    const input = document.getElementById('ro-filter-input') as HTMLInputElement | null;
    if (!input) {
        return;
    }
    const draft = input.value;
    if (!trimFilterWhitespace(draft)) {
        closeFilterAC();
        return;
    }
    const parsed = splitFilterDraft(draft);
    if (!parsed) {
        // Field-name suggestions: substring match, prefix matches ranked first.
        openFilterAC(rankFieldSuggestions(roRowModel.fields, draft));
        return;
    }
    if (parsed.op !== ':' || !filterFieldKnown(roRowModel.fields, parsed.field)) {
        // Operator forms leave the value free; `label` values are not in the row
        // model; unknown fields get the ⏎ hint, not suggestions.
        closeFilterAC();
        return;
    }
    // Top 8 distinct values by frequency, computed from the FULL row model.
    // Virtual `label` resolves as a valid filter but has no model-column index;
    // rankValueSuggestions returns before reading rows in that case.
    openFilterAC(rankValueSuggestions(roRowModel.fields, roRowModel.rows, parsed));
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
        if (filterACOpen() && filterACItems.length > 0) {
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
    // Chips editor: a chip's ✕ is a real link (no-JS fallback) whose href is
    // the server-built removal URL; intercept it to ride the v2 partial loop
    // (morph + canonical push) instead of a full navigation.
    {
        event: 'click',
        selector: '#ro-filter-field [data-ro-action="remove-chip"]',
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
        selector: '#ro-filter-ac [data-ro-action="pick-suggestion"]',
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
    // Chips editor: every keystroke re-runs the live name match (model-
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
