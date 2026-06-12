// events.ts -- the delegated-event dispatcher. ONE DOM listener per
// event TYPE on `document`, fronting an ORDERED list of bindings. This is the
// target architecture the strangler migrates the monolith's hand-rolled
// `document.addEventListener` listeners into, one feature cluster at a time.
//
// THE DISPATCH CONTRACT (pinned by the adversarial review + the original listener
// inventory, whose durable projection is tests/e2e/compound-gestures.spec.ts):
//
//   1. NOT first-match-wins. EVERY binding whose `selector` matches the event
//      fires, in REGISTRATION ORDER. This mirrors the monolith, where separate
//      `document.addEventListener('click', ...)` listeners ALL see the same
//      click (the browser dispatches to every listener on the node) -- e.g. the
//      cols toggle (C1) and the cols outside-click guard (C4) both run on one
//      click with no propagation stop between them.
//
//   2. The chain only halts on an EXPLICIT stop signal that MIRRORS an existing
//      early `return` in the monolith: a binding declared `stop: true` whose
//      handler returns a truthy value ends the chain FOR THAT EVENT INSTANCE
//      (the remaining bindings of the SAME event type do not run). This is the
//      delegated equivalent of a matched branch's `return` inside the monolith's
//      one big click listener. No stop signal that does not already exist in the
//      monolith may be introduced.
//
//   3. A throw in one binding NEVER prevents the others from running: each
//      binding is invoked in its own try/catch, exactly as the browser isolates
//      sibling `addEventListener` listeners (a throw in one does not abort the
//      dispatch to the next). A failing binding is logged and skipped.
//
//   4. Registration order is fixed by ONE explicit list in readout.ts -- the
//      single place the ordering lives, so it is auditable in one read.
//
// A binding may omit `selector` (it then matches every event of its type and
// decides for itself inside the handler -- used for the listeners that key off
// focus/state, not a delegated closest() target). When `selector` IS present
// the handler receives the matched element (the `event.target.closest(selector)`
// result) so it never re-queries.
//
// Vendor globals (htmx, Idiomorph) are reached through `typeof` guards by the
// feature modules, NOT imported here -- the dispatcher is vendor-agnostic.

// The matched element handed to a selector-bound handler. `null` only for a
// binding with NO selector (it opted out of delegated matching).
export type BindingHandler = (event: Event, matched: Element | null) => boolean | undefined;

export interface Binding {
    // The DOM event type ('click', 'keydown', 'change', 'input', 'keyup', ...).
    event: string;
    // A CSS selector resolved with `event.target.closest(selector)`. Omit to
    // match every event of this type (the handler guards itself).
    selector?: string;
    // Invoked with the event and the matched element (or null when selector is
    // omitted). Returns truthy to request a stop (only honoured when stop:true).
    handler: BindingHandler;
    // When true, a truthy handler return HALTS the remaining bindings for this
    // event instance -- the delegated mirror of a monolith branch's `return`.
    // Defaults to false: the handler runs but the chain always continues.
    stop?: boolean;
}

// closestElement resolves the delegated target. event.target can be a non-
// Element node (a text node from a click on a text run); we walk to the nearest
// Element first so closest() is always defined.
function closestElement(event: Event, selector: string): Element | null {
    let node: Node | null = event.target as Node | null;
    while (node && node.nodeType !== 1) {
        node = node.parentNode;
    }
    return node ? (node as Element).closest(selector) : null;
}

// dispatch runs the bindings for ONE event type against ONE event instance, in
// order, honouring per-binding match/stop/isolation per the contract above.
function dispatch(bindings: Binding[], event: Event): void {
    for (let i = 0; i < bindings.length; i++) {
        const binding = bindings[i];
        let matched: Element | null = null;
        if (binding.selector !== undefined) {
            matched = closestElement(event, binding.selector);
            if (!matched) {
                continue; // selector miss -> this binding does not apply
            }
        }
        let result: boolean | undefined;
        try {
            result = binding.handler(event, matched);
        } catch (e) {
            // Isolation: a throwing binding must not abort the dispatch to the
            // rest (the browser isolates sibling listeners the same way).
            console.warn('readout event binding failed', binding.event, binding.selector, e);
            continue;
        }
        if (binding.stop && result) {
            return; // explicit stop -> the monolith's early `return` mirror
        }
    }
}

// registerBindings installs ONE document listener per distinct event type and
// routes each event through dispatch() over the bindings of that type, in the
// order they appear in `bindings`. Idempotent guard is the caller's job: this
// is called ONCE at module load (the listeners live on `document`, which is
// never replaced, so they survive every htmx swap with no re-init -- exactly
// the monolith's model).
export function registerBindings(bindings: Binding[]): void {
    const byType = new Map<string, Binding[]>();
    for (const binding of bindings) {
        const list = byType.get(binding.event);
        if (list) {
            list.push(binding);
        } else {
            byType.set(binding.event, [binding]);
        }
    }
    byType.forEach((list, type) => {
        document.addEventListener(type, (event) => dispatch(list, event));
    });
}
