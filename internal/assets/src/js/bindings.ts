// bindings.ts -- THE single ordered registration list for the delegated-event
// dispatcher (events.ts). This is the one auditable place the registration
// ORDER lives, per the Unit 9 dispatch contract: read this file top-to-bottom
// to know exactly which migrated binding sees an event first.
//
// The list is registered FIRST (at the very top of legacy.js's body, before the
// monolith attaches its own remaining document listeners), so every migrated
// leaf binding here runs ahead of the not-yet-migrated monolith listeners. That
// is safe today because every entry is a LEAF feature with no inter-listener
// dependency (per docs/forge/frontend-refactor/raw/listener-inventory.md); the
// risky clusters (palette / rows / columns) still live in legacy.js and join
// this list in later units in the order the inventory pins.
//
// Each entry is a `{ event, selector?, handler, stop? }` Binding (see events.ts
// for the full contract). `stop: true` mirrors a matched branch's early
// `return` in the monolith's one big listener -- it ends the chain for that
// event instance. A binding WITHOUT a selector matches every event of its type
// and guards itself in the handler.

import { bulkBindings } from './bulk-actions.js';
import { columnsBindings } from './columns.js';
import { contextMenuBindings } from './context-menu.js';
import type { Binding } from './events.js';
import { filtersBindings } from './filters.js';
import { keyboardBindings } from './keyboard.js';
import { logsBindings } from './logs.js';
import { miscBindings } from './misc-ui.js';
import { paletteBindings } from './palette.js';
import { refreshBindings } from './refresh.js';
import { rowSelectionBindings } from './row-selection.js';
import { foldBindings } from './yaml-folds.js';

// Leaf feature modules contribute their bindings here. theme.ts + toasts.ts are
// leaves with NO delegated document binding (theme's only hook is a matchMedia
// change listener; toasts is a pure function), so they do not appear in this
// list -- they are wired through the runInit step / direct calls in legacy.js.
//
// REGISTRATION ORDER (read top-to-bottom). The Unit-10 cluster reproduces the
// monolith's inter-listener decoupling (docs/forge/frontend-refactor/raw/
// listener-inventory.md, durable projection tests/e2e/compound-gestures.spec.ts):
//
//   - context-menu FIRST: its UNCONDITIONAL dismiss (C2 step 2) runs before any
//     stop:true leaf below, so clicking ANYWHERE (incl. a stop:true namespace
//     item) still dismisses an open row menu -- exactly the monolith, where C1's
//     early-returns never stopped the separate C2 listener. ctx-item (C2 step 1)
//     leads its own array so a menu-item click acts BEFORE the dismiss, mirroring
//     the monolith step order;
//   - bulk then row-selection: the dismiss FALLS THROUGH (no stop) to the bulk
//     branches (C2 step 3) and finally the row-select branch (C2 step 4, no
//     stop), so a click on a different row dismisses the menu AND selects that
//     row in one pass (compound case 1 -- a positive selection outcome, never a
//     stray stop after the dismiss);
//   - palette next: its click branches were the HEAD of the monolith's big click
//     listener (palette item/opener/refine/backdrop) and never co-match a row or
//     bulk selector, so their order vs the row cluster is observationally free;
//     each early-returned -> stop:true. Its keydown bindings (⌘K chord +
//     palette-open keys) own the palette keys; the palette-open binding excludes
//     #ro-filter-input so an Escape there routes to the still-resident filter
//     keydown listener (compound case 4 -- focus-routed, not topmost-first);
//   - keyboard last of the cluster: the gesture keydown (K3) stays INERT under
//     the palette/menu via keyboardSurfaceBusy() -- the DOM guard is the real
//     decoupler, so it neither stops nor double-acts regardless of order
//     (compound case 2). The kbd-overlay backdrop click (C3) is independent;
//
// The Unit-12 cluster (columns + filters) registers AFTER the row cluster but
// BEFORE palette, for ONE inter-listener reason: the columns outside-click (C4)
// and the filter-AC outside-click (C5) were SEPARATE always-running monolith
// listeners with NO selector (they match every click and guard themselves), so
// they must front-run the stop:true leaves below (a palette-item / copy click
// still dismisses an open columns popover or filter-AC, exactly as the separate
// monolith listeners did). They register AFTER the row cluster so the
// context-menu UNCONDITIONAL dismiss (C2 step 2) and the row-select fall-through
// still run on a chip-✕ / column-toggle click -- a chip-✕ click while a row menu
// is open dismisses the menu (C1 returned, C2 still ran in the monolith). Their
// own click branches (chip-✕/AC-item/field; cols-toggle/col-toggle) never
// co-match a row/palette selector, so the relative order within is free; the
// cols-toggle binding precedes C4 so the toggle-click-while-open guard sees the
// freshly-flipped flag (single close, never a reopen -- listener-inventory
// C1/C4). columns precedes filters only because the popover-submit (form) and
// the column toggles share the popover surface; neither stops the other.
//
//   - the Unit-9 leaves (folds/logs/misc) keep their existing relative order.
export const bindings: Binding[] = [
    ...contextMenuBindings,
    ...bulkBindings,
    ...rowSelectionBindings,
    ...columnsBindings,
    ...filtersBindings,
    ...paletteBindings,
    ...keyboardBindings,
    ...foldBindings,
    ...logsBindings,
    // misc-ui's click bindings keep their relative monolith order: copy is
    // registered before the section-fold binding (copy stop:true short-circuits
    // a copy click), so a copy click never folds its section. misc-ui now also
    // carries the trailing presentation toggles ([data-ro-more] / [data-ro-annolong] /
    // [data-ro-action="toggle-tools"]) and the v1 form glue (the data-ro-toggle-button
    // change + the tools-form submit) lifted out of the dismantled legacy.js.
    ...miscBindings,
    // refresh-domain tails LAST: the retry + set-refresh hooks were
    // the monolith big click listener's own trailing branches, so registering
    // them after the migrated leaves preserves the C1 order -- every leaf
    // front-ran the monolith, and these ran at its end. Neither co-matches any
    // selector above, so the position is observationally free; LAST documents
    // their monolith origin.
    ...refreshBindings,
];
