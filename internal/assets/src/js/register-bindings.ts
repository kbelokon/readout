// register-bindings.ts -- the dispatcher-registration side-effect boundary.
// Keeping the call in its own module makes ESM dependency evaluation enforce
// the bundle contract: when readout.ts imports this module before init.ts, the
// delegated listeners are installed before init.ts attaches lifecycle hooks.

import { bindings } from './bindings.js';
import { registerBindings } from './events.js';

registerBindings(bindings);
