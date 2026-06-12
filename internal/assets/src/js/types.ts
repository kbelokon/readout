// types.ts -- the hand-authored wire contracts the client shares with the Go
// server, plus the global declarations for the debug/e2e seams the modules hang
// off `window`. NOTHING here is code-generated: the SERVER is the source of
// truth for every wire shape, so each type below carries a pointer to its Go
// origin and is kept in sync BY HAND. A drift shows up as a typecheck failure at
// the consumer (the JSON.parse boundary stays defensive -- these types describe
// the AUTHORITATIVE shape, not a runtime guarantee), never as a silent
// re-render of a stale contract.
//
// Two kinds of declarations live here:
//   1. wire contracts (Go <-> JS): the ⌘K palette feed, the ro_prefs cookie
//      payload, the Live SSE frames. The field NAMES + JSON tags are the pinned
//      seam; the Go side pins the same tags (the cited files).
//   2. the `window` debug/e2e seams: every roRowState / roVirtual / roLive /
//      roToast / roFuzzy / roOpenPalette / roRowModel / requestListRefresh
//      signature the e2e suite drives, declared ONCE on the global Window so the
//      assignment sites are compiler-checked against the frozen shape (the e2e
//      contracts in tests/e2e/*.spec.ts depend on these names + signatures).
//
// This is a module (it exports the wire types), so the `declare global` block
// needs the trailing `export {}` to keep TS treating the file as a module under
// isolatedModules + verbatimModuleSyntax.

// The chips-editor row model + autocomplete shapes are CLIENT-side (no Go wire
// seam): the model is captured from the server fragment but lives only in JS.
// Their canonical definitions are filters-parse.ts (the node-tested pure half);
// re-exported here so the Window.roRowModel seam below references the SAME shape
// the runtime builds, and so the "row model (data-key) + filter autocomplete"
// contracts have one documented home alongside the Go wire types.
import type { ACItem, ModelField, ModelRow } from './filters-parse.js';

export type { ACItem, ModelField, ModelRow };

// ---------------------------------------------------------------------------
// ⌘K palette feed -- internal/web/layout_view.go (paletteFeedView, ~145-192).
// ---------------------------------------------------------------------------
// Serialized verbatim into the #ro-palette-data JSON blob by the templ shell;
// palette.ts reads it on every open. The JSON tags ARE the pinned wire contract
// (the Go struct comment says so). palette-rank.ts consumes the four group
// arrays through the looser `Record<string, unknown>[]` PaletteFeed shape (the
// ranker is field-agnostic); these interfaces document the AUTHORITATIVE per-
// entry fields the row builder + the Go feed builder agree on.

// paletteLinkFeed: a {name, href} jump target (a cluster or a namespace).
// Display carries the middle-truncated form only when the name
// overruns the 42-rune budget (server-side truncation, omitempty).
export interface PaletteLinkFeed {
    name: string;
    href: string;
    display?: string;
}

// paletteKindFeed: a resource-type jump target -- the kind label + plural + API
// group + the namespaced/cluster scope + the list href + the pre-rendered
// (server-escaped) icon markup. Display is the truncated label, like the link.
export interface PaletteKindFeed {
    kind: string;
    plural: string;
    group: string;
    namespaced: boolean;
    href: string;
    icon: string;
    display?: string;
}

// paletteActionFeed: a labelled action -- an href (navigate) OR a named client
// action the palette JS interprets. Both keys are emitted; only one is
// populated per entry (omitempty on each).
export interface PaletteActionFeed {
    label: string;
    href?: string;
    action?: string;
}

// paletteFeedView: the whole #ro-palette-data blob. CurrentCluster/Namespace are
// nullable pointers on the Go side (*string -> null when unscoped).
export interface PaletteFeedWire {
    currentCluster: string | null;
    currentNamespace: string | null;
    clusters: PaletteLinkFeed[];
    namespaces: PaletteLinkFeed[];
    kinds: PaletteKindFeed[];
    actions: PaletteActionFeed[];
}

// ---------------------------------------------------------------------------
// ro_prefs cookie payload -- internal/web/prefs.go (prefs / kindPrefs).
// ---------------------------------------------------------------------------
// The cookie is JS-written, server-read (the codec lives in prefs.ts, pinned
// from both sides by the golden fixtures prefs.test.ts shares with the Go
// codec). These types mirror the Go JSON tags: `k`/`sort`/`hide` per kind, the
// top-level `refresh` string and the `ns` cluster->namespace map. Go's
// decodePrefs is all-or-nothing on a mistyped field, so the JS reader keeps each
// field only when its type matches (the self-healing field guards prefs.ts pins).

// kindPrefs: one per-plural entry. Sort is a kube.SortTable param
// ("Name", "Status:desc", ...). Hide is the hidden-column list -- a POINTER on
// the Go side, so an ABSENT hide (DefaultHiddenColumns applies) is distinct from
// an explicit empty list (suppress the defaults); here that is `hide?: string[]`.
export interface KindPrefsWire {
    k: string;
    sort?: string;
    hide?: string[];
}

// prefs: the cookie envelope. Refresh is the auto-refresh MODE as a string
// ("Off", an interval in seconds, or "Live"). Namespaces maps cluster name ->
// last-used namespace ("_all" is valid).
export interface PrefsWire {
    kinds?: KindPrefsWire[];
    refresh?: string;
    ns?: Record<string, string>;
}

// ---------------------------------------------------------------------------
// Live SSE frames -- internal/web/handlers_stream.go
// (streamTablePayload / streamTerminalPayload).
// ---------------------------------------------------------------------------
// The `GET …/{plural}/_stream` endpoint pushes `event: ro-table` and
// `event: ro-terminal` frames, each a single `data:` line of JSON. live.ts
// parses them defensively (the fields arrive as `unknown` until checked); these
// document the pinned `g`/`html` and `g`/`reason` tags.

// ro-table: the client-minted generation echoed back (`g`) + the rendered
// `_table` partial HTML on one line (`html`).
export interface StreamTablePayloadWire {
    g: string;
    html: string;
}

// ro-terminal: the echoed generation (`g`) + the close reason -- one of
// "idle" | "auth" | "watch-failed" | "shutdown". The client drops to polling
// without reconnecting.
export type StreamTerminalReason = 'idle' | 'auth' | 'watch-failed' | 'shutdown';
export interface StreamTerminalPayloadWire {
    g: string;
    reason: StreamTerminalReason;
}

// ---------------------------------------------------------------------------
// Row model + filter autocomplete -- CLIENT-side, see filters-parse.ts.
// ---------------------------------------------------------------------------
// The row model is captured from the FULL server fragment (never the windowed
// DOM) in filters.ts; rows are identified by their `data-key` (the same identity
// the idiomorph morph + the selection store key off). ModelField/ModelRow/ACItem
// are re-exported above from filters-parse.ts (their canonical, node-tested
// home) -- the in-memory shapes filters.ts / virtualizer.ts share. There is NO
// Go wire seam here: the server emits the <tr data-key> + per-column data-hint
// markup the capture reads, but the model itself never crosses the wire.

// RowModelWire: the assembled in-memory model (the Window.roRowModel seam shape).
export interface RowModelWire {
    fields: ModelField[];
    rows: ModelRow[];
    visibleKeys: Set<string> | null;
}

// ---------------------------------------------------------------------------
// window debug/e2e seams -- the frozen signatures the e2e suite drives.
// ---------------------------------------------------------------------------
// Declared on the global Window so the module assignment sites (window.roX = …)
// are checked against these shapes instead of each casting `window as unknown`.
// The vendor classic-script globals (htmx, Idiomorph) stay OFF Window -- the
// modules reach them through their own typeof-guarded getHtmx()/typeof checks so
// the dispatcher + features remain vendor-agnostic; only the readout-owned seams
// live here.
declare global {
    interface Window {
        // row-selection.ts: the selection store + j/k focus seam.
        roRowState: {
            setSelected(key: string, on: boolean): void;
            setFocus(key: string): void;
            focusedKey(): string | null;
            clear(): void;
            selectedKeys(): string[];
            selectedEntries(): { key: string; name: string }[];
        };
        // virtualizer.ts: the windowing inspection + scroll-to-identity seam.
        roVirtual: {
            active(): boolean;
            renderedBounds(): { start: number; end: number; total: number };
            scrollToKey(key: string): boolean;
        };
        // live.ts: the discard observability counter (await "push discarded").
        roLive: {
            discards(): number;
        };
        // toasts.ts (bridged through init.ts): the detached-result notifier.
        roToast?: (message: string) => void;
        // palette-rank.ts (re-exposed by palette.ts): the pure fuzzy ranker.
        roFuzzy: (query: string, text: string) => number;
        // palette.ts: open the ⌘K palette programmatically.
        roOpenPalette: () => void;
        // filters.ts: the full server-render row model capture.
        roRowModel: RowModelWire;
        // refresh.ts: re-fire the read-only list refresh (e2e + stale Retry).
        requestListRefresh: () => void;
    }
}
