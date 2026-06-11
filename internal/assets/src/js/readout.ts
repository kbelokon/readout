// readout.ts -- strangler entry. The delegated-event dispatcher's ordered
// binding list lives in ./bindings.ts (the single audit point) and is
// registered at the TOP of the legacy bundle's body, BEFORE the monolith
// attaches its remaining hand-rolled document listeners -- so the migrated leaf
// bindings front-run the not-yet-migrated ones. ESM evaluates an imported
// module's body before the importer's own statements, so importing legacy.js
// here runs its body (which does the registration first, then its own
// attachments) as part of bootstrapping the app.
//
// Leaf feature modules with module-load side effects (e.g. theme.ts's matchMedia
// change listener) are imported explicitly so the bundle includes them; their
// init steps and bindings are consumed by legacy.js (runInit) and bindings.ts.
import './theme.js';
import './legacy.js';
