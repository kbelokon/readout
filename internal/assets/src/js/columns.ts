// columns.ts -- the column-visibility popover (D8, client half), migrated from
// legacy.js. The ⊞ title-row popover on single-type list pages. The popover
// itself is SERVER-rendered inside the morphed fragment (one checkbox per column
// of the full universe, hidden columns included; the identity row disabled; the
// absorbed labelcols/selector form); this module owns only the open state, the
// toggle gesture, the checkbox commit, and the popover form submit.
//
// A toggle is cookie-state, not URL-state (D9): it writes the COMPLETE hidden
// set through roPrefsSetHiddenColumns (an empty array is the explicit "hide
// nothing" that suppresses the config default) and re-renders by riding the
// container's own programmatic path (requestListRefresh -> source
// #resource-list-content -> RO-No-Push), so the server never answers with
// HX-Push-Url -- zero history entries, and the URL never changes. The morph
// re-renders the popover from server truth (checkbox states included) and wipes
// the client-added `.is-open`, so colsPopOpen re-applies it after every list
// swap; runInit re-derives the flag from the DOM so a boosted body swap (which
// renders the popover closed) can never leave a stale-open flag.
//
// The popover-submit form MERGES into the live query (the labelcols/selector
// apply): popFormMergedHref keeps every un-owned `?f=` chip byte-exact -- the
// pure merge lives in filters-parse.ts (mergeColParams, node-tested), this is
// the thin DOM read of the form fields around it. The Go needle contract
// (internal/web/colvis_test.go, TestColsPopoverSubmitMergeJSContract) pins the
// FORMS preserved here: 'popFormMergedHref', 'form.ro-pop-form', and the wiring
// 'issueFilterNavigation(popFormMergedHref(popForm))'.
//
// DISPATCH (the Unit 12 ordered-binding migration): the popover's click branches
// were branches of the monolith's big click listener (the [data-cols-toggle]
// toggle C1, the .col-toggle commit) plus its own outside-click listener (C4).
// The [data-cols-toggle] branch flips colsPopOpen and does NOT stop the chain --
// C4's own [data-cols-toggle] guard prevents the double-close, NOT propagation
// (listener-inventory C1/C4). So neither binding is stop:true; the guard does
// the work. colsPopOpen() is read DIRECTLY by keyboard.ts's keyboardSurfaceBusy
// (the Unit-12 dismantling of the window.roClusterBridge colsPopOpen member).

import type { Binding } from './events.js';
import { roPrefsSetHiddenColumns } from './prefs.js';
import { requestListRefresh } from './refresh.js';
import { mergeColParams } from './filters-parse.js';
import { issueFilterNavigation } from './filters.js';

function getHtmx(): { trigger(el: Element, name: string): void } | undefined {
    return (window as unknown as { htmx?: { trigger(el: Element, name: string): void } }).htmx;
}

// The popover open flag. Derived from the DOM at gesture/init time (a boosted
// body swap renders it closed, so a stale flag can never invert the gesture);
// the flag only re-applies `.is-open` after fragment morphs.
let colsPopOpenFlag = false;

// colsPopOpen is read DIRECTLY by keyboard.ts (keyboardSurfaceBusy) -- the
// columns popover owns the keys while open, so the gesture keys stay inert.
export function colsPopOpen(): boolean {
    return colsPopOpenFlag;
}

export function setColsPopOpen(open: boolean): void {
    colsPopOpenFlag = open;
    const pop = document.getElementById('ro-cols-pop');
    if (pop) {
        pop.classList.toggle('is-open', open);
    }
    const btn = document.getElementById('ro-cols-btn');
    if (btn) {
        btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    }
}

// syncColsPopState re-derives the open flag from the freshly-rendered DOM (init +
// boosted swaps render the popover closed; no popover -> closed). The runInit
// step + the afterSwap re-open both call into it / setColsPopOpen.
export function syncColsPopState(): void {
    const pop = document.getElementById('ro-cols-pop');
    colsPopOpenFlag = !!pop && pop.classList.contains('is-open');
}

// commitColumnVisibility reads the popover's checkbox state into the complete
// hidden-column list, persists it, and re-renders the fragment. The identity row
// (disabled) never contributes; an in-flight container request (a tick or a
// rapid prior toggle) is aborted first so a stale response can never land over
// the newer cookie state.
function commitColumnVisibility(pop: Element | null): void {
    if (!pop) {
        return;
    }
    const plural = (pop as HTMLElement).dataset.plural || '';
    if (!plural) {
        return;
    }
    const hidden: string[] = [];
    pop.querySelectorAll('.col-toggle').forEach((toggle) => {
        const check = toggle.querySelector('.ro-check') as HTMLInputElement | null;
        if (!(toggle as HTMLButtonElement).disabled && check && !check.checked
            && (toggle as HTMLElement).dataset.col) {
            hidden.push((toggle as HTMLElement).dataset.col as string);
        }
    });
    roPrefsSetHiddenColumns(plural, hidden);
    const content = document.getElementById('resource-list-content');
    const htmx = getHtmx();
    if (content && htmx) {
        htmx.trigger(content, 'htmx:abort');
    }
    requestListRefresh();
}

// popFormMergedHref builds the D8 popover form's submit URL by MERGING its
// user-editable fields into the LIVE query instead of replacing it wholesale
// (which is what a native GET submit does). Every location.search pair whose key
// the form does not own survives BYTE-EXACT -- above all the `?f=` chips, whose
// raw OR-commas are wire-significant (the server splits alternatives on raw
// commas BEFORE percent-decoding; filter.go). The form's hidden round-trip
// inputs are NOT owned: their values snapshot the very pairs the merge already
// keeps byte-exact (they exist for the no-JS fallback); only the visible inputs
// (labelcols / selector) replace their pairs -- a cleared visible input drops
// its pair, exactly like the native path's blank-empty-names trick. The pure
// string-concat merge is filters-parse.ts's mergeColParams (node-tested).
function popFormMergedHref(form: HTMLFormElement): string {
    const owned = new Set<string>();
    const fields: string[] = [];
    Array.prototype.slice.call(form.elements).forEach((el: HTMLInputElement) => {
        if (el.tagName !== 'INPUT' || el.type === 'hidden' || !el.name) {
            return;
        }
        owned.add(el.name);
        if (el.value) {
            fields.push(el.name + '=' + encodeURIComponent(el.value));
        }
    });
    return mergeColParams(window.location.pathname, window.location.search, owned, fields);
}

// --- dispatcher bindings ----------------------------------------------------
export const columnsBindings: Binding[] = [
    // Column-visibility popover (D8): the ⊞ title-row button toggles the popover
    // open/closed. Open state is derived from the DOM (a boosted body swap
    // renders it closed). NOT stop:true -- C4's own [data-cols-toggle] guard
    // (the outside-click binding below) keeps the double-fire single, not a stop
    // signal (listener-inventory C1/C4: both see the same click, no propagation
    // stop between them).
    {
        event: 'click',
        selector: '[data-cols-toggle]',
        handler: (event) => {
            event.preventDefault();
            const pop = document.getElementById('ro-cols-pop');
            setColsPopOpen(!!pop && !pop.classList.contains('is-open'));
        },
    },
    // A column checkbox row: flip the checkbox optimistically, then commit the
    // COMPLETE hidden set (as the user now sees it) to the ro_prefs cookie and
    // re-render through the container's own programmatic path -- cookie-state,
    // not URL-state: RO-No-Push, zero history entries (D6/D9). The identity row
    // is a disabled <button>, so its clicks never fire.
    {
        event: 'click',
        selector: '.col-toggle',
        handler: (event, matched) => {
            event.preventDefault();
            const toggle = matched as HTMLElement;
            const check = toggle.querySelector('.ro-check') as HTMLInputElement | null;
            if (check) {
                check.checked = !check.checked;
            }
            commitColumnVisibility(toggle.closest('.ro-pop'));
            return true;
        },
        stop: true,
    },
    // C4: a click outside the popover (and not on its ⊞ opener) closes it -- the
    // same dismissal contract the autocomplete dropdown uses. The
    // [data-cols-toggle] escape: when the ⊞ toggle is clicked WHILE open, the
    // toggle binding above already set colsPopOpen=false (closed), and this
    // guard makes this binding a no-op so it does NOT re-toggle (no double-fire /
    // no reopen). No selector (it keys off the flag + the closest() escapes).
    {
        event: 'click',
        handler: (event) => {
            if (!colsPopOpenFlag) {
                return;
            }
            const t = event.target as Element;
            if (t.closest('#ro-cols-pop') || t.closest('[data-cols-toggle]')) {
                return;
            }
            setColsPopOpen(false);
        },
    },
    // form.ro-pop-form (the D8 popover's labelcols/selector form): intercept and
    // MERGE into the live query, riding the v2 loop exactly like a chip commit
    // (issueFilterNavigation falls back to a plain navigation when the loop is
    // unavailable). The native submit would rebuild the query from the round-trip
    // hidden inputs alone and wipe every `?f=` chip.
    {
        event: 'submit',
        selector: 'form.ro-pop-form',
        handler: (event, matched) => {
            event.preventDefault();
            const popForm = matched as HTMLFormElement;
            issueFilterNavigation(popFormMergedHref(popForm));
            return true;
        },
        stop: true,
    },
];
