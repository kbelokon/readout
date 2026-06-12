// readout.ts -- the bundle ENTRY POINT. legacy.js is gone: the
// monolith is fully dismantled into typed modules, and this file wires them in
// the one order that matters. esbuild bundles from here into a single classic
// IIFE and PRESERVES import order, so the sequence below IS the bundle's
// top-level execution order.
//
// IMPORT ORDER (load-bearing):
//   1. ./htmx-config FIRST -- it sets htmx.config (globalViewTransitions,
//      reduced-motion-aware) at the top of the bundle body. The bundle loads
//      after htmx.min.js but BEFORE htmx walks the DOM, so the config must be set
//      before any other module's body runs (none of them touch htmx.config, but
//      the FIRST-import contract keeps it unambiguous + future-proof).
//   2. ./morph -- registers the idiomorph cell-flash defaults + the CSP-safe
//      ro-morph extension. Defining the extension before init's lifecycle hooks
//      attach is harmless (htmx dispatches swaps later), but morph also has no
//      ordering dependency on the dispatcher, so it sits right after the config.
//   3. ./events + ./bindings -> registerBindings(bindings): the delegated-event
//      dispatcher installs ONE document listener per event type over the ordered
//      binding list. Registered BEFORE init attaches its document-level htmx
//      hooks -- the dispatch contract's "registered first" (the migrated leaf
//      bindings front-run the lifecycle orchestration). Idempotent at load.
//   4. ./init LAST -- the resident htmx-lifecycle orchestration (the sort-write
//      hook, the afterSwap pipeline, the body-swap teardown, the history-restore
//      repaint) + the idempotent runInit chain on DOMContentLoaded / htmx:load /
//      afterSettle / resize. It depends on the dispatcher already being live.
//
// Leaf feature modules with module-load side effects (theme's matchMedia
// listener, stale/skeleton's own document listeners, the window debug seams
// hung off row-selection/filters/virtualizer/palette/live/refresh) are pulled
// in transitively: bindings.ts imports every feature module for its Binding
// arrays, and init.ts imports the orchestrated surfaces, so the bundle includes
// them all with no explicit side-effect import needed here.

import './htmx-config.js';
import './morph.js';
import { bindings } from './bindings.js';
import { registerBindings } from './events.js';

registerBindings(bindings);

import './init.js';
