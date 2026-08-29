// htmx-config.ts -- the FIRST thing the bundle does: set htmx.config BEFORE htmx
// processes the DOM. The entry point (readout.ts) imports this module FIRST, and
// esbuild preserves import order in the emitted IIFE, so this top-level code runs
// ahead of every other module's body -- and the bundle itself loads AFTER
// htmx.min.js but BEFORE htmx walks the document, so the config set here governs
// every boosted navigation and swap.
//
// NATIVE VIEW TRANSITIONS (the only config we touch): keep them disabled.
// Whole-document snapshots made ordinary navigation look like the viewport was
// being kicked, while delayed post-swap enhancements could visibly reflow the
// new page. Readout owns those lifecycle seams directly and always performs a
// plain HTMX swap; no browser animation capability or motion preference can
// silently re-enable a root transition.
//
// Vendor-agnostic: htmx is a classic-script global (loaded before this bundle),
// reached through a typeof guard -- never imported (the bundle has no module for
// the vendored lib). If the vendored lib failed to load, the guard skips the
// config and the app degrades to plain navigation.

declare const htmx: { config: { globalViewTransitions: boolean } } | undefined;

if (typeof htmx !== 'undefined') {
    htmx.config.globalViewTransitions = false;
}

export {};
