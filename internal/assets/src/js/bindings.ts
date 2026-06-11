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
import { foldBindings } from './yaml-folds.js';

// Leaf feature modules contribute their bindings here. theme.ts + toasts.ts are
// leaves with NO delegated document binding (theme's only hook is a matchMedia
// change listener; toasts is a pure function), so they do not appear in this
// list -- they are wired through the runInit step / direct calls in legacy.js.
//
// REGISTRATION ORDER (read top-to-bottom): yaml-folds' two click branches
// (.ro-fold-toggle, .linenos a) front-run the monolith's big click listener,
// reproducing their position ahead of the section-fold / gutter-anchor branches.
export const bindings: Binding[] = [
    ...foldBindings,
];
