// cluster-bridge.ts -- the TYPE of the seam legacy.js exposes to the migrated
// cluster modules (palette.ts, keyboard.ts) for the few pieces that still live
// in the monolith: the Unit-24 virtualizer internals and the Unit-12 columns
// popover open flag. Both clusters are deliberately NOT migrated yet (per the
// Unit 10 boundary), so the migrated keyboard/palette code reaches them through
// the same `window.ro*` seam pattern the file already uses (roRowState, roFuzzy,
// roVirtual): legacy.js populates `window.roClusterBridge` near its virtualizer,
// and the modules read it at call time (never at module-eval time, so the
// bundle's evaluation order is irrelevant).
//
// Only palette.ts / keyboard.ts import this (both carry runtime imports
// already, so they are never node-tested); the pure rank/grouping logic stays
// in palette-rank.ts with no bridge dependency.

// RowEl is a table row the virtualizer tracks (a plain Element in practice; the
// modules only read dataset/classList/scrollIntoView through it).
export interface ClusterBridge {
    // The Unit-24 virtualizer is engaged (windowing the rows).
    virtualizerActive(): boolean;
    // The FULL identity row set in server order (harvest reads this so ⌘K
    // filters every object on the page, not just the rendered window).
    virtRows(): Element[];
    // The rows passing the live free-text filter, in order (the j/k walk set
    // while windowed).
    virtVisible(): Element[];
    // The tracked row for a key (rendered or detached), or null.
    virtRowByKey(key: string): Element | null;
    // Step the windowed focus by delta (scrolls the window, sets roRowState
    // focus); returns true when a row took focus.
    virtMoveFocus(delta: number): boolean;
    // The ⊞ columns popover is open (read by keyboardSurfaceBusy so the gesture
    // keys stay inert while it owns the keyboard).
    colsPopOpen(): boolean;
}

// The window seam. Always present at runtime: legacy.js assigns it before its
// own listeners attach, and the dispatcher (where the migrated bindings run)
// only fires inside a user event, long after module load.
export function clusterBridge(): ClusterBridge {
    return (window as unknown as { roClusterBridge: ClusterBridge }).roClusterBridge;
}
