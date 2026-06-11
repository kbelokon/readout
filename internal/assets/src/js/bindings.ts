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
import type { Binding } from './events.js';
import { contextMenuBindings } from './context-menu.js';
import { bulkBindings } from './bulk-actions.js';
import { rowSelectionBindings } from './row-selection.js';
import { paletteBindings } from './palette.js';
import { keyboardBindings } from './keyboard.js';
import { foldBindings } from './yaml-folds.js';
import { logsBindings } from './logs.js';
import { miscBindings } from './misc-ui.js';

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
//   - the Unit-9 leaves (folds/logs/misc) keep their existing relative order.
export const bindings: Binding[] = [
    ...contextMenuBindings,
    ...bulkBindings,
    ...rowSelectionBindings,
    ...paletteBindings,
    ...keyboardBindings,
    ...foldBindings,
    ...logsBindings,
    // misc-ui's click bindings keep their relative monolith order: copy is
    // registered before the section-fold binding (copy stop:true short-circuits
    // a copy click), so a copy click never folds its section.
    ...miscBindings,
];
