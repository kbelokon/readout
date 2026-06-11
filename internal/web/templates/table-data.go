package templates

import "strings"

// table-data.go holds the resolved data contracts the resource-list TABLE engine
// renders, plus the small windowing helpers that branch on table size. The render
// itself is split across the focused table-*.templ files (chrome / cells / cards /
// tools); this file carries no markup, only the structs + Go helpers they share.
//
// The engine renders the body of the `.../{plural}/_table` GET partial route AND
// the inner content of the full list page's #resource-list-content container:
// per-table title row (count chip + TSV / columns / search-this actions -- the
// columns action is the D8 ⊞ popover on single-type pages, the v1 toggle-tools +
// tools form elsewhere) + phase strip + the redesign `.ro-table` (in
// `.ro-table-wrap`), then the footer meta line and the all-cluster partial-failure
// banner.
//
// This is the canonical resource-list ENGINE: it renders Pods (rich cells),
// Nodes/Deployments/Namespaces (rich cells arrive in later waves), and every
// other kind via the generic reskinned k8s-Table cell. It is shared by ALL list
// kinds, so it carries the redesign content marker `ro-rd` (added on the
// #resource-list-content root in resource_list.templ and on the bare partial
// wrapper in the handler), which routes the colliding class names
// (.ro-phase-strip, .ro-phase-chip, .ro-breadcrumb) to the redesign CSS rules.
//
// Every request-derived value (sort hrefs, the TSV/search hrefs, per-row status
// classes, per-cell render branch + class + tone + href, the age cell class +
// tooltip, the phase tally) is resolved in the package-web assembly layer and
// carried in ListData; the renderer touches no request and recomputes nothing.
// Trusted pre-rendered chrome (inline SVG icons, the sort-direction icon) rides
// in as raw strings.
//
// The v2 interaction loop (D6, single-type pages only -- D1): sort headers with
// a non-empty PartialHref are hx-get requests against the `_table` partial,
// morph-swapped into #resource-list-content (the handler pushes the CANONICAL
// page URL via HX-Push-Url -- never the partial URL); rows with a non-empty Key
// carry data-key="cluster/ns/name" plus an id derived from it, so idiomorph
// matches rows by object identity rather than position and readout.js re-keys
// selection/focus state onto the same object after every morph.

// ListData is the resolved resource-list input shared by the full page and the
// `_table` partial. The all-* flags drive the leading Cluster/Namespace columns
// and the footer text; Tables carries the per-table render data.
type ListData struct {
	IsAllNamespaces   bool
	Namespace         string
	Plural            string
	ClusterCount      int
	TableCount        int
	TotalRows         int
	DurationSeconds   float64
	AllNamespacesHref string
	Errors            []string
	Tables            []TableData

	// State is the whole-list forbidden/unreachable state (replaces the tables for
	// a single-cluster list that wholly failed). Empty Kind ("") means no state.
	State ListState

	// Stale (client-side, D11): a hidden `.ro-banner.warn` readout.js reveals on
	// an auto-refresh error (it never blanks the rows). ShowStaleBanner emits the
	// hidden hook; it lives on the FULL page only (the partial fragment is what
	// gets dimmed, it does not re-emit the banner).
	ShowStaleBanner bool

	// FilterBar is the Filters v2 chips editor (D7): rendered in the tools row of
	// the FIRST (only) table on single-type pages, inside the morphed fragment so
	// a chip-committing partial request re-renders the chips and a shareable URL
	// lands with them visible. nil on multi-type pages (D1: `?f=` ignored there).
	FilterBar *FilterBarData
}

// FilterBarData is the chips editor: the plural (placeholder copy), the icon,
// and the active `?f=` chips rendered server-side.
type FilterBarData struct {
	Plural     string
	FilterIcon string
	Chips      []EditorChip
}

// EditorChip is one `.ro-scope-chip` in the editor. Field/Op/Value carry the
// display split of a well-formed chip; a malformed chip (Field == "") renders
// its whole Label instead. RemoveHref drops exactly this raw occurrence.
type EditorChip struct {
	Field      string
	Op         string
	Value      string
	Label      string
	RemoveHref string
}

// ListState is the resolved whole-list failure state. Kind is "" (no state),
// "forbidden", or "unreachable". Hint is the ONE plain-language line under the
// headline; Detail carries the VERBATIM apiserver/transport string for the
// mono `.errdetail` block (SPEC §1.5). Cluster names the failing cluster in
// the unreachable headline ("Can’t reach <cluster>").
type ListState struct {
	Kind      string
	Cluster   string
	Verb      string
	Resource  string
	Namespace string
	Hint      string
	Detail    string
	GlyphIcon string // pre-rendered state glyph (raw SVG)
	RetryHref string
	BackHref  string
}

// TableData is one rendered resource table. MultiCluster gates the leading
// Cluster column; Icons are pre-rendered inline SVG; Tools is the tools form.
type TableData struct {
	Kind            string
	Count           int // visible row count -> the .ro-count title chip
	DownloadTSVHref string
	SearchHref      string
	DownloadIcon    string
	ToolsIcon       string
	SearchIcon      string

	Tools TableTools

	// Cols is the ⊞ column-visibility popover (D8): non-nil on single-type
	// pages, where it REPLACES the v1 toggle-tools button + tools form (the
	// filter input's new home is the chips editor, labelcols + selector move
	// into the popover). nil (multi-type pages, D1) keeps the v1 chrome.
	Cols *ColsPopover

	// HideCreated suppresses the synthetic Created header and cells (D8): the
	// Created column is rendered by this template, not carried by the kube
	// Table, so the hide set reaches it through this flag.
	HideCreated bool

	ShowMetricsHref string

	Phase     []PhaseChip
	PhaseRows int

	MultiCluster bool

	Columns       []TableColumn
	ColumnCount   int // kube.Table column count (drives the empty-row colspan)
	CreatedHref   string
	CreatedIcon   string
	CreatedSorted bool // active sort is the synthetic Created column

	// CreatedPartialHref is the Created header's `_table` partial sort URL: when
	// non-empty (single-type pages, D6) the header becomes an hx-get morph swap
	// of #resource-list-content; empty keeps the v1 boosted link (D1).
	CreatedPartialHref string

	Rows []TableRow

	EmptyGlyph string // pre-rendered empty-state glyph (raw SVG, the inbox icon)

	// Empty-state enrichment (rendered only when Rows is empty). EmptyActionHref/
	// Label is the broad next step on a plainly-empty list; EmptyFilters +
	// ClearHref are the removable filter chips + Clear on the empty-FILTERED state
	// (non-empty EmptyFilters => the emptiness is filter-caused).
	EmptyActionHref  string
	EmptyActionLabel string
	EmptyFilters     []FilterChip
	ClearHref        string
}

// FilterChip is one removable active-filter chip on the empty-filtered state.
type FilterChip struct {
	Label      string
	RemoveHref string
}

// PhaseChip is one phase-tally chip (status dot tone + label + count). Tone is
// the redesign dot tone (ok/warn/err/info/mute).
type PhaseChip struct {
	Tone  string
	Label string
	Count string
}

// TableColumn is one column header: its title/class + sort link + the
// pre-rendered sort-direction icon (raw HTML, empty when not the sort column).
// Sorted marks the active sort column (the redesign `th.sorted` highlight).
// PartialHref, when non-empty (single-type pages, D6), turns the header link
// into an hx-get of the `_table` partial morph-swapped into
// #resource-list-content; SortHref stays the canonical page URL for
// history/new-tab/no-JS. Empty PartialHref keeps the v1 boosted link (D1).
type TableColumn struct {
	Description string
	Class       string
	SortHref    string
	Name        string
	SortIcon    string
	Sorted      bool
	PartialHref string

	// Hint is the chips-editor autocomplete type hint (text/number/duration),
	// emitted as data-hint on the <th>. Its presence marks the column
	// FILTERABLE: the editor builds field suggestions only from data-hint
	// headers, so the synthetic Created / leading Cluster/Namespace headers
	// (which the server-side filter cannot bind) are never suggested.
	Hint string
}

// ColsPopover is the ⊞ column-visibility popover (D8): the per-column
// checkbox entries (the full universe -- hidden columns included, so they stay
// re-offerable) plus the labelcols / selector inputs absorbed from the tools
// form (Tools carries their values and the hidden param round-trip; its
// FilterVal is unused here -- the chips editor owns filtering).
type ColsPopover struct {
	Plural  string
	Icon    string // pre-rendered ⊞ glyph (raw SVG)
	Entries []ColsEntry
	Tools   TableTools
}

// ColsEntry is one popover checkbox: the column name, whether the current
// render hides it, and whether it is the protected identity column (rendered
// checked + disabled -- the server ignores it in hidecols too).
type ColsEntry struct {
	Name     string
	Hidden   bool
	Identity bool
}

// TableTools is the per-table tools form (label columns / selector / filter).
type TableTools struct {
	Active       bool
	HiddenInputs []HiddenInput
	LabelColsVal string
	SelectorVal  string
	FilterVal    string
	TableIcon    string
	TagsIcon     string
	FilterIcon   string
}

// HiddenInput is one hidden form input round-tripping a query param.
type HiddenInput struct {
	Name  string
	Value string
}

// TableRow is one body row: its status class, the optional Cluster/Namespace
// cells, the per-column cells, and the Created cell. Key/DomID carry the D6
// row identity ("cluster/ns/name" + the id derived from it): when non-empty
// (single-type pages) the <tr> emits data-key + id so idiomorph matches rows
// by identity (never position) and client row state re-keys across morphs.
type TableRow struct {
	StatusClass  string
	ClusterHref  string
	Cluster      string
	NsHref       string
	Namespace    string
	Cells        []TableCell
	CreatedClass string
	CreatedText  string
	CreatedTitle string
	Key          string
	DomID        string

	// Per-row gesture targets (Unit 16 / D10), set together with Key on
	// single-type pages: the full untruncated object name plus the
	// server-resolved open / ?view=yaml / /logs (pods-only) / ?download=yaml
	// hrefs, emitted as data-name/data-href/data-yaml/data-logs/data-download
	// for the context menu + bulk actions in readout.js.
	Name         string
	OpenHref     string
	YAMLHref     string
	LogsHref     string
	DownloadHref string
}

// TableCell is one body cell. Kind selects the render branch; Class is the full
// <td> class, Href the resolved link target where the branch needs one. The
// redesign fields carry the rich-cell presentation resolved in assembly.
type TableCell struct {
	Kind     CellKind
	Value    string
	Class    string
	ColClass string
	Href     string

	Tone     string // status-dot / cell-status tone (ok/warn/err/info/mute)
	Ratio    string // ready ratio tone (full/partial/zero)
	Pulse    bool   // transient status -> .ro-dot.pulse
	NameHead string // bright workload prefix (sticky name cell); middle-truncated past 42 chars (full name then in Title)
	NameTail string // muted hash suffix -- NEVER truncated
	Ago      string // optional "(… ago)" suffix on a restarts cell
	Trunc    bool   // secondary free-text cell: truncate with Title tooltip
	Title    string // full-value tooltip (truncated, age, ports/hosts, evobj cells)

	CapBucket string   // node capacity bar bucket (lo/mid/hi); "" -> no colour fill
	CapPct    int      // node capacity bar fill width %; 0 -> empty bar
	CapBar    bool     // true only with metrics joined; false -> value text, no bar
	Roles     []string // node role chips (control-plane earns .cp)
	Conds     []Cond   // node abnormal condition pills (empty -> muted "—")

	RepSegments  []RepSegment // deployment replica-track segments (capped; empty beyond cap)
	RepNum       string       // deployment ready/desired ratio text (.rep-num truth)
	RolloutState string       // deployment rollout state (done/prog/paused)
	RolloutIcon  string       // pre-rendered rollout glyph (raw SVG, themed by state colour)

	Chips []RowChip // label/selector chips (empty -> muted "—"); 2 + the +N in-cell expand

	More      string    // faint overflow suffix on a ports/hosts cell ("+N" / "+N hosts")
	Keys      []KeyChip // configmap/secret data chips (name · size); 3 + the +N keys in-cell expand
	EvKind    string    // events object kind (faint "Kind/" prefix)
	EvName    string    // events object name, 20…8 middle-truncated (full name in Title)
	EvAgeRest string    // faint 11px second layer of an events age cell
	CellIcon  string    // pre-rendered glyph for the tls lock / evobj kind icon (raw trusted markup)
}

// KeyChip is one configmap/secret data chip (`name · size`): the key name and
// its human byte size. No value field exists by construction -- secret VALUES
// never enter the DOM (SPEC §4.10).
type KeyChip struct {
	Name string
	Size string
}

// RowChip is one namespace label chip: the label key and value, rendered as a
// NEUTRAL `.ro-chip` with the `.ck`/`.cs`/`.cv` ink-weight split (D3 colour
// law: every label chip is neutral; the green `.app` accent is retired).
// A non-empty Href (single-type pages, D7/SPEC §8.1) renders the chip as a
// click-to-filter anchor: the same list with `label:key=value` appended to
// `?f=`.
type RowChip struct {
	Key  string
	Val  string
	Href string
}

// Cond is one abnormal node condition pill (name + redesign tone warn/err/ok).
type Cond struct {
	Name string
	Tone string
}

// RepSegment is one deployment replica-track segment: State is "" (filled/ready),
// "updating" (amber pulse), or "pending" (hollow), mapped straight onto the
// `<i class="<State>">` segment class.
type RepSegment struct {
	State string
}

// CellKind selects a body-cell render branch (matches package-web cellKind).
type CellKind int

const (
	CellPlain CellKind = iota
	CellName
	CellLabel
	CellNode
	CellCPU
	CellMemory
	CellStatus
	CellReady
	CellRestarts
	CellCapacity
	CellRoles
	CellConditions
	CellReplicas
	CellRollout
	CellChips
	// SPEC §4 cookbook corner-case kinds -- the order MUST mirror the
	// package-web cellKind enum (the bridge casts between them).
	CellPending
	CellPorts
	CellHosts
	CellTLS
	CellLastRun
	CellKeys
	CellCount
	CellEvObj
	CellEvAge
	CellMsg
)

// chipsCellMax / keysCellMax are the SPEC §4.9/§4.10 in-cell overflow
// thresholds: 2 label/selector chips and 3 data-key chips shown before the
// extras hide behind `.xtra` + the `+N` expand button.
const (
	chipsCellMax = 2
	keysCellMax  = 3
)

// virtualizeThreshold is the D20 windowing boundary (~500 rows), owned HERE:
// readout.js engages its virtualizer off the `ro-windowed` wrap class this
// template emits, so the threshold has exactly one owner. Above it (Unit 24):
//   - the wrap swaps `has-cards` for `ro-windowed` and the mobile card
//     projection is NOT emitted (600 hidden .ro-pcard subtrees must not ride
//     every morph);
//   - the events msg cell clamps to one line in CSS and carries the full text
//     in its title (recorded SPEC deviation, the fixed-height law);
//   - in-cell chip/keys expansion is disabled: the +N renders as a static
//     chip whose title carries the full list, and the `.xtra` overflow chips
//     are not emitted at all.
const virtualizeThreshold = 500

// tableWindowed reports whether a table crosses the windowing threshold. The
// table is taken by pointer (the TableData struct is heavy -- gocritic
// hugeParam, matching nameCell/statusCell/metaKey in helpers.go).
func tableWindowed(table *TableData) bool { return len(table.Rows) > virtualizeThreshold }

// tableWrapClass picks the wrap modifier: the mobile-card projection marker
// below the threshold, the D20 `ro-windowed` marker above it.
func tableWrapClass(windowed bool) string {
	if windowed {
		return "ro-table-wrap ro-windowed"
	}
	return "ro-table-wrap has-cards"
}

// chipsTitle / keysTitle render the FULL chip list for the windowed +N's
// title (D20: expansion disabled while windowed; the title recovers the data).
func chipsTitle(chips []RowChip) string {
	parts := make([]string, len(chips))
	for i, chip := range chips {
		parts[i] = chip.Key + ":" + chip.Val
	}
	return strings.Join(parts, ", ")
}

func keysTitle(keys []KeyChip) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key.Name + " · " + key.Size
	}
	return strings.Join(parts, ", ")
}
