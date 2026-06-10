package web

import (
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
)

// view.go holds the plain view-model structs that sit between data assembly
// (build_*.go) and rendering (render.go). The hard contract: these structs
// carry NO *http.Request and NO *kube.Client. Every request-derived value the
// render layer needs — sort hrefs, query flags, base paths, selector hrefs,
// labelcols/hidecols/filter/selector form state — is precomputed in assembly and
// carried here as resolved data. kube.Table / kube.Object remain (plain data
// types backed by no live client) where eliminating them would risk byte
// identity. Rendering reads a struct; it never reaches back into the request.

// listView is the view model for the resource-list page and its htmx partial.
// Cluster/Namespace/Plural and the all-* flags drive breadcrumb + footer text;
// Tables carries the per-table render data with every request-derived href
// already resolved.
type listView struct {
	Cluster         string
	Namespace       string
	Plural          string
	IsAllClusters   bool
	IsAllNamespaces bool
	ClusterCount    int
	Duration        time.Duration
	Errors          []error
	Tables          []tableView

	// SingleType marks a single-resource-type list page -- the D1 surface
	// boundary for the v2 interaction loop (D6). Only single-type pages get the
	// partial sort headers, row identity keys, the location-derived refresh tick,
	// and the bulk-bar mount; multi-type pages (plural=all / CSV / _all) keep the
	// v1 behavior (boosted sort links + the render-time-baked partial URL).
	SingleType bool

	// AllNamespacesHref is the precomputed "Show <plural> across all namespaces"
	// link target (carries the current query string). Empty when the link should
	// not render.
	AllNamespacesHref string

	// State is the resolved whole-list failure state for a SINGLE-cluster list
	// that produced no tables at all (forbidden / unreachable). nil for the happy
	// path, for an all-cluster list (which uses the partial-failure banner, D11),
	// and for a single-cluster list that still produced at least one table (D11:
	// a single-cluster list never reports some-clusters-failed; a partial
	// multi-type list keeps its tables and the per-type errors are dropped here).
	State *listStateView

	// StaleBanner feeds the CLIENT-SIDE stale path (D11): a hidden
	// `.ro-banner.warn` that readout.js reveals (and dims #resource-list-content,
	// the id the JS owns) when an auto-refresh request errors. Pre-rendered so the
	// markup hooks exist in the first server response; the server never decides
	// "stale" (there is no last-good cache) -- only the client does, on a refresh
	// error that keeps the existing rows.
	StaleBanner bool

	// FilterBar is the Filters v2 chips editor (D7): the active `?f=` chips
	// rendered server-side in the tools row (a shareable URL lands with its
	// chips visible) plus the free-text/autocomplete input readout.js drives.
	// nil on multi-type pages (the D1 boundary -- `?f=` is ignored there, so no
	// editor may suggest it works).
	FilterBar *filterBarView
}

// filterBarView is the resolved chips-editor state: the list's plural (the
// input placeholder copy) and the active `?f=` chips. Chips render inside the
// morphed fragment, so a chip-committing partial request re-renders them.
type filterBarView struct {
	Plural string
	Chips  []filterChipView
}

// listKind enumerates the whole-list failure/empty states. emptyState /
// emptyFilterState are per-TABLE (rendered inside a table with zero rows) and
// not carried here; this set is the LIST-level states that replace the tables.
type listStateKind int

const (
	stateForbidden listStateKind = iota
	stateUnreachable
)

// listStateView is the resolved whole-list failure state shown in place of the
// tables when a single-cluster list wholly failed. Forbidden names the
// verb/resource/namespace + 403; unreachable shows the REAL transport error
// string (never a cute message, Principles §11) + a read-only retry GET + a
// "Back to clusters" escape.
type listStateView struct {
	Kind      listStateKind
	Verb      string // "list" (the read-only verb that was denied/attempted)
	Resource  string // the resource plural the request targeted
	Namespace string // the namespace scope ("" / "_all" rendered as a clause)
	Detail    string // forbidden: "403 Forbidden · <reason>"; unreachable: the real error
	RetryHref string // a read-only GET back to this same list URL
	BackHref  string // "/clusters"

	// SourceErr is the underlying kube error that produced this state. The FULL
	// page renders the state card (a first load has no prior rows to keep), but
	// the AUTO-REFRESH `_table` partial must NOT 200-with-state-card -- morph would
	// swap the last-good rows out for the card and defeat the stale path. The
	// partial handler instead surfaces SourceErr via s.error (a non-2xx), so htmx
	// keeps the existing rows and fires htmx:responseError -> the client-side stale
	// banner + dim. Carried as plain data (like listView.Errors), no live client.
	SourceErr error
}

func (v *listView) Title() string {
	return v.Plural + " in " + v.Cluster
}

// tableView is one rendered resource table with all request-derived hrefs and
// classes resolved in assembly. Table stays as plain kube.Table so the row/cell
// values, column metadata, and status classes are read directly during render.
type tableView struct {
	Table kube.Table
	Kind  string // pluralizeKind(Resource.Kind)

	DownloadTSVHref string
	SearchHref      string
	ShowMetricsHref string // "" unless pods/nodes and join is unset

	Phase []kube.PhaseCount // kube.PhaseSummary(Table)

	Columns     []columnView
	CreatedHref string
	CreatedIcon string

	// CreatedPartialHref is the synthetic Created header's `_table` partial sort
	// URL (the hx-get target of the v2 loop, D6). Empty on multi-type pages
	// (D1), where the Created header stays a plain boosted link.
	CreatedPartialHref string

	// HideCreated suppresses the synthetic Created header/cells (D8): the
	// Created column is template-rendered, not a kube column, so the hide set
	// reaches it through this flag rather than kube.RemoveColumns. The zero
	// value keeps Created shown.
	HideCreated bool

	// ColumnVis is the column-visibility popover universe (D8): every column of
	// the fully-decorated table + the synthetic Created, with hidden/identity
	// flags. Non-nil only on single-type pages (the D1 gate); nil keeps the v1
	// toggle-tools form chrome.
	ColumnVis []columnVis

	Tools toolsView

	Rows []rowView

	// Empty-state enrichment for a table with zero rows. EmptyAction is the broad
	// next step (e.g. "Show <plural> across all namespaces") offered on a genuinely
	// empty list; EmptyFilters are the removable active-filter chips + ClearHref
	// offered when the emptiness is caused by an active filter/selector (the
	// empty-FILTERED state). When EmptyFilters is non-empty the table is
	// empty-because-filtered; otherwise it is plainly empty.
	EmptyAction  *emptyActionView
	EmptyFilters []filterChipView
	ClearHref    string
}

// emptyActionView is the broad next-step button on a plainly-empty list.
type emptyActionView struct {
	Href  string
	Label string
}

// filterChipView is one removable active-filter chip, shared by the
// empty-filtered state and the chips editor (D7): Label is the human chip text,
// RemoveHref drops just that one filter (a read-only GET) so the ✕ removes it.
// Field/Op/Value carry the editor's display split (`.ck` key, accent operator,
// `.v` value) for a well-formed `?f=` chip; they stay empty for the legacy
// filter/selector/labels chips and for a malformed chip (Label then renders
// whole).
type filterChipView struct {
	Label      string
	RemoveHref string
	Field      string
	Op         string
	Value      string
}

// columnView precomputes a column header's sort link and indicator. SortHref is
// the CANONICAL page URL (history/new-tab/no-JS); PartialHref is the same sort
// against the `_table` partial route -- the header's hx-get in the v2 loop (D6).
// PartialHref is empty on multi-type pages (D1 boundary: v1 boosted links).
type columnView struct {
	SortHref    string
	SortIcon    string
	PartialHref string

	// Hint is the filter-autocomplete type hint for this column (text / number /
	// duration), emitted as the header's data-hint. Its PRESENCE marks the
	// column filterable: the chips editor builds its field-name suggestions from
	// the data-hint headers, so synthetic non-Column headers (Created, the
	// leading Cluster/Namespace columns) never get suggested -- exactly the set
	// resolveFilterColumn can bind. Empty on multi-type pages (no editor).
	Hint string
}

// toolsView precomputes the resource-list tools form (label columns, selector,
// filter) state. All values are resolved request inputs; render emits them.
type toolsView struct {
	Active       bool
	HiddenInputs []hiddenInput
	LabelColsVal string
	SelectorVal  string
	FilterVal    string
}

type hiddenInput struct {
	Name  string
	Value string
}

// rowView precomputes the per-row status class and per-cell render data so the
// table body render needs no request access.
type rowView struct {
	StatusClass  string
	Cluster      string
	Namespace    string
	ClusterHref  string // cell link for the Cluster column (multi-cluster)
	NsHref       string // cell link for the Namespace column (all-namespaces)
	Cells        []cellView
	CreatedClass string
	CreatedText  string

	// Key is the row's stable object identity "cluster/ns/name" (empty segments
	// collapsed) -- the D6 row-identity contract. The renderer emits it as
	// data-key plus an id derived from it, so idiomorph matches rows by identity
	// (never position) and client row state (selection, j/k focus) re-keys onto
	// the same object across morphs. Empty on multi-type pages (D1), where rows
	// stay identity-less v1 markup.
	Key string
}

// cellView precomputes a single body cell. Kind selects the render branch; the
// resolved href (when the branch needs one) is carried so render never calls
// addQuery live. The redesign fields (Tone/Ratio/Pulse/NameHead/NameTail/Ago/
// Trunc/Title) carry the resolved rich-cell presentation (status dot tone, ready
// ratio tone, pod-name split, restart "ago" suffix, secondary-text truncation
// tooltip) so the templ renderer emits the new vocabulary without recomputing.
type cellView struct {
	Kind     cellKind
	Value    string
	Class    string // augmented cellClass (incl. age) for the <td>
	ColClass string // table.Columns[i].Class
	Href     string // resolved link target for name/label/node branches

	// Tone is the redesign status-dot/cell-status tone (ok/warn/err/info/mute),
	// mapped from the Bulma cellClass; "" means no tone colour (generic fallback).
	Tone string
	// Ratio is the ready/replica ratio tone (full/partial/zero) for cellReady.
	Ratio string
	// Pulse marks a transient status whose dot animates (.pulse).
	Pulse bool
	// NameHead/NameTail split an identifier into a bright workload prefix + a
	// muted hash suffix for the sticky name cell. NameHead+NameTail == Value,
	// EXCEPT when the head exceeds the SPEC §4.2 threshold (42 chars): the head
	// is then middle-truncated for display (26…12) and Title carries the FULL
	// name. The tail/hash is NEVER truncated.
	NameHead string
	NameTail string
	// Ago is the optional "(… ago)" suffix on a restarts cell (muted).
	Ago string
	// Trunc marks a secondary free-text cell (image/label/selector/message) that
	// truncates with a Title tooltip; identifiers never set it.
	Trunc bool
	// Title is the full-value tooltip carried on a truncated or age cell.
	Title string

	// CapBucket is the node capacity-bar bucket (lo/mid/hi -> green/amber/red) for
	// cellCapacity. It is set ONLY when a real usage % exists (metrics joined); the
	// no-metrics state leaves it "" so the bar renders empty and uncoloured.
	CapBucket string
	// CapPct is the capacity-bar fill width as a percentage (0..100), set ONLY with
	// a real usage %. The no-metrics state leaves it 0 (empty/0-width bar).
	CapPct int
	// CapBar is true only when metrics are joined (a real usage %); the no-metrics
	// state leaves it false so the renderer shows the capacity value WITHOUT an
	// empty bar.
	CapBar bool
	// Roles are the node role chips for cellRoles (e.g. "control-plane", "worker");
	// the control-plane role earns the `.cp` accent in the renderer.
	Roles []string
	// Conds are the abnormal node condition pills for cellConditions. Empty means a
	// clean node, rendered as a muted "—". Each pill carries its tone + name.
	Conds []condPill

	// RepSegments are the deployment replica-track segments for cellReplicas: one
	// per rendered slot (capped at replicaTrackCap), each carrying its state
	// (filled/updating/pending). Beyond the cap NO segments render -- RepNum is the
	// source of truth. Ratio (full/partial/zero) tones the .rep-num.
	RepSegments []repSegment
	// RepNum is the `ready/desired` ratio text shown in the .rep-num span; it is the
	// truth beyond the segment cap. Empty for a non-replica cell.
	RepNum string
	// RolloutState is the deployment rollout state for cellRollout
	// (done/prog/paused); the renderer maps it to the .rollout.<state> class + icon.
	// Value carries the rollout label ("up to date"/"rolling out"/"paused").
	RolloutState string

	// Chips are the namespace label chips for cellChips: one per metadata.labels
	// entry (sorted), each carrying its key/value pair. Empty means a namespace
	// with no labels, rendered as a muted "—". In tables the renderer shows the
	// first chipsCellMax chips; the rest carry `.xtra` (hidden) behind the `+N`
	// in-cell expand button (SPEC §4.9).
	Chips []chipView

	// More is the faint overflow suffix on a ports/hosts cell ("+N" / "+N
	// hosts"); the full list rides in Title. Empty when nothing overflows.
	More string
	// Keys are the configmap/secret data chips for cellKeys (`name · size`).
	// Secret VALUES never reach this view model -- a key chip carries ONLY the
	// key name and its byte size (SPEC §4.10). Past keysCellMax the renderer
	// hides chips behind the `+N keys` in-cell expand, same `.xtra` machinery
	// as the label chips.
	Keys []keyChipView
	// EvKind/EvName are the events Object cell split for cellEvObj: EvKind
	// renders faint with a trailing slash next to its kind icon; EvName is the
	// 20…8 middle-truncated object name (Title carries the full name when
	// truncated, SPEC §4.2).
	EvKind string
	EvName string
	// EvAgeRest is the faint second layer of an events Age cell for cellEvAge
	// (e.g. "(first 41h ago)"); Value keeps the leading age token, which is the
	// only part the age bucket colours.
	EvAgeRest string
}

// keyChipView is one configmap/secret data chip for cellKeys: the key name and
// its HUMAN byte size ("4.2 KiB"). By construction no value field exists --
// secret values must never be serialized into a view model (SPEC §4.10).
type keyChipView struct {
	Name string
	Size string
}

// chipView is one namespace label chip: the label key and value, rendered as a
// NEUTRAL `.ro-chip` with the `.ck`/`.cs`/`.cv` ink-weight split (D3 colour law:
// every label chip is neutral; the green `.app` accent is retired). Href, when
// non-empty (single-type pages, D7/SPEC §8.1), is the click-to-filter target:
// the SAME list URL with the `label:key=value` chip appended to `?f=`.
type chipView struct {
	Key  string
	Val  string
	Href string
}

// repSegment is one deployment replica-track segment. State is "" for a filled
// (ready) segment, "updating" for an amber pulsing segment (updated beyond
// ready), "pending" for a hollow not-yet-updated segment. The renderer maps the
// state straight onto the `<i class="<state>">` segment class.
type repSegment struct {
	State string
}

// condPill is one abnormal node condition pill: Name is the condition type (e.g.
// "MemoryPressure"), Tone is the redesign pill tone (warn/err/ok). Only abnormal
// conditions are surfaced, so a clean node has no pills.
type condPill struct {
	Name string
	Tone string
}

type cellKind int

const (
	cellPlain cellKind = iota
	cellName
	cellLabel
	cellNode
	cellCPU
	cellMemory
	cellStatus
	cellReady
	cellRestarts
	cellCapacity
	cellRoles
	cellConditions
	cellReplicas
	cellRollout
	cellChips
	// The SPEC §4 cookbook corner-case kinds (Unit 10). The kind-specific
	// schema decorators that EMIT most of them land with the services/ingress/
	// configmap/secret/cronjob/job (Unit 11) and events (Unit 12) columns; the
	// constructors live in build_list.go and the renderers in
	// resource_table.templ.
	cellPending // empty -> faint <none>; literal <pending> -> amber pulsing dot + word
	cellPorts   // first 2 ports + faint +N, full list in title
	cellHosts   // first host + faint "+N hosts", full list in title
	cellTLS     // green lock + "tls" ONLY when terminated, else "—"
	cellLastRun // age-scale colour + " ago"; never ran -> faint <never>
	cellKeys    // data chips `name · size`, 3 + "+N keys" in-cell expand
	cellCount   // events ×N, ≥20 amber, 1 faint, thousands separator
	cellEvObj   // kind icon + faint "Kind/" + 20…8 middle-truncated name
	cellEvAge   // two-layer age: bucket-coloured first token + faint remainder
	cellMsg     // the ONLY wrapping cell in the system (max-width 520px)
)

// detailView is the view model for the resource-view (object detail) page. The
// request-derived pieces — the active-tab flags, the YAML/Logs hrefs, the
// download href — are resolved in assembly; Object stays plain so render reads
// labels/annotations/spec sections directly.
type detailView struct {
	Cluster   string
	Namespace string
	Object    kube.Object
	Title     string

	// NameHead/NameTail split the detail H1 into the bright workload prefix +
	// the muted hash tail (SPEC §6.6: the hash tail stays faint even in the
	// title), via the same splitObjectName/MiddleTruncate pair the table name
	// cells use (D14). NameTitle carries the FULL name for the title= tooltip
	// when the head was middle-truncated; "" otherwise.
	NameHead  string
	NameTail  string
	NameTitle string

	DownloadHref string
	Links        []config.Link

	IsYAMLView   bool
	IsEventsView bool
	DefaultTab   bool   // active flag for the Default tab
	YAMLTab      bool   // active flag for the YAML tab
	EventsTab    bool   // active flag for the Events tab
	LogsHref     string // "" unless a Logs tab should render

	HighlightedYAML string // precomputed when IsYAMLView (else "")

	Owners []config.Link

	ShowNamespaceLinks bool   // Namespace-kind extra links
	AllObjectsHref     string // "Show all objects in this namespace"
	ResourceTypesHref  string // "Show Resource Types in this namespace"

	RelatedPods *subtableView // nil when absent
	Events      []eventView

	// Resolved render data (assembled in buildDetailView so the templ
	// resource-view component reads plain data, never the raw object). These
	// carry the iteration/sort/escape done once at assembly time.
	CreatedMeta string // formatTimestamp(creationTimestamp) for the meta line
	Version     string // metadata.resourceVersion

	Labels      []labelChipView
	Annotations []annotationChipView
	// AnnotationsLong are the >120-char annotation values (SPEC §7.15 /
	// D14): each renders as a collapsed `key · size` toggle whose payload
	// expands into a scrollable <pre>, never as a chip.
	AnnotationsLong []annotationLongView
	Node            *nodeSummaryView // non-nil only for Kind == Node

	// Containers is the pod containers table (D14): init containers first
	// (badged), then regular, each joining status/spec/metrics. nil for
	// non-pod kinds and for a pod whose spec decodes no containers.
	Containers *containersSectionView

	Secret    *secretDataView // non-nil only for masked Secret with data
	YAMLCards []yamlCardView

	// State is the resolved detail-page failure state (forbidden / unreachable).
	// When non-nil the detail body renders the `.ro-empty-lg` state instead of the
	// object detail; the handler still returns 200 (the page chrome is intact and
	// the user gets an actionable state, not a bare error panel). NotFound stays a
	// real 404 via s.error (a missing object is not a cluster failure).
	State *detailStateView
}

// detailStateView is the resolved detail-page failure state, mirroring
// listStateView: forbidden names the verb/resource/namespace + 403; unreachable
// shows the REAL transport error string + a read-only retry GET + Back to
// clusters.
type detailStateView struct {
	Kind      listStateKind
	Verb      string
	Resource  string
	Name      string
	Namespace string
	Detail    string
	RetryHref string
	BackHref  string
}

// labelChipView is one resolved label chip: the selector href and the
// key/value. Every label chip is a NEUTRAL `.ro-chip` (D3 colour law -- the
// green app.kubernetes.io/* accent is retired; key/value differ by ink weight).
type labelChipView struct {
	Href string
	Key  string
	Val  string
}

// annotationChipView is one resolved annotation chip. Val is the truncated
// display value (clipped in the chip body); Full is the complete "key: value"
// string the chip carries in its title= tooltip, so the full value stays
// readable even though the body is clipped.
type annotationChipView struct {
	Key  string
	Val  string
	Full string
}

// annotationLongView is one >120-char annotation (SPEC §7.15): the key, the
// humanBytes payload size shown on the collapsed toggle, and the full value
// rendered (escaped) inside the hidden scrollable <pre>.
type annotationLongView struct {
	Key   string
	Size  string
	Value string
}

// containersSectionView is the pod containers table (D14): Count/InitCount
// drive the `Containers · N + M init` section label; Rows are ordered init
// containers first (badged), then regular, both in spec declaration order.
type containersSectionView struct {
	Count     int
	InitCount int
	Rows      []containerRowView
}

// containerRowView is one container row. State/Ready/Restarts/Ago come from
// the status.containerStatuses / status.initContainerStatuses entry joined by
// name; Ports/Image come from the spec.containers entry; CPU/Mem come from the
// PodMetrics containers[] join when live ("" renders the faint "—"). The
// StateTone is kube.StatusTone(State) (D4 — the single value->tone owner, the
// waiting/terminated reason IS the state word); StatePulse marks the transient
// set (law §1.3).
type containerRowView struct {
	Name string
	Init bool

	State      string
	StateTone  string
	StatePulse bool

	Ready      string // "ready" / "not ready"; "" (init or no status) renders "—"
	ReadyClass string // full / partial

	Restarts     string
	RestartsTone string // zero / some
	Ago          string // "(6d ago)" suffix, "" when never restarted

	Ports string
	CPU   string
	Mem   string
	Image string
}

// nodeSummaryView holds the resolved Node-kind summary blocks.
type nodeSummaryView struct {
	Conditions  []nodeConditionView
	HasCapAlloc bool
	Capacity    *kvListView
	Allocatable *kvListView
	NodeInfo    *kvListView
}

type nodeConditionView struct {
	Tone  string
	Title string
	Type  string
	Value string
}

type kvListView struct {
	Rows []kvRowView
}

type kvRowView struct {
	Key string
	Val string
}

// secretDataView is the resolved masked-Secret data block (key names only).
type secretDataView struct {
	KeyCount int
	Keys     []string
}

// yamlCardView is one resolved per-section YAML card. Content is the trusted
// highlighted-YAML HTML produced by the YAML highlighter (injected raw).
// Collapsed marks a card that starts folded (SPEC §7.15: the Status card is
// collapsed by default) — the same is-collapsed class the readout.js section
// fold toggles.
type yamlCardView struct {
	Name      string
	Title     string
	Content   string
	Collapsed bool
}

// subtableView is the related-pods subtable on the detail page, with column sort
// hrefs and per-row cell data resolved in assembly.
type subtableView struct {
	Table       kube.Table
	Namespace   string
	Columns     []subtableColumn
	CreatedHref string
	Rows        []subtableRow
}

type subtableColumn struct {
	Description string
	SortHref    string
	Name        string
}

type subtableRow struct {
	StatusClass  string
	ShowNs       bool
	NsHref       string
	Namespace    string
	Cells        []subtableCell
	CreatedClass string
	CreatedText  string
}

type subtableCell struct {
	Kind  cellKind
	Value string
	Class string
	Href  string
	Tone  string
}

// eventView is one rendered event row (already flattened from the raw object).
// Tone is the redesign status tone for the Type cell (mute for Normal/unknown,
// warn for Warning), mapped from the events/Type cell class via statusTone.
// Count/CountClass are the ×N dedupe cell (D15: ≥20 amber "restarts some", 1
// faint); Age is the leading compressed-duration token with AgeClass its
// .age-* bucket, AgeRest the faint "(first 41h ago)" second layer (empty for
// a single occurrence or a ≤60s spread), and AgeTitle the full last-seen
// timestamp tooltip. The Reason cell renders plain and From faint in the
// redesign; the Message <td>'s ro-event-msg class is static (emitted in the
// templ).
type eventView struct {
	Type       string
	Tone       string
	Reason     string
	Count      string
	CountClass string
	Age        string
	AgeClass   string
	AgeRest    string
	AgeTitle   string
	From       string
	Message    string
}

// searchView is the view model for the search page. Every value the
// redesign search render needs -- the form round-trip, the offered resource-type
// checkboxes (with checked state), the per-cluster scope chips, the result cards,
// and the count footer -- is resolved in buildSearchView and carried here as
// plain data. The renderer (toSearchData) derives the partial-failure banner +
// footer entirely from ScopeClusters (each chip carries its own Failed/Reason/
// RetryHref), so no separate error-record map is needed. No *http.Request and no
// kube.Client cross this boundary.
type searchView struct {
	Query     string
	Cluster   string
	Namespace string
	// ShellCluster is the real single-cluster scope passed to the page shell.
	// All-cluster and CSV multi-cluster searches keep Cluster for form
	// round-trip but leave the shell unscoped so sidebar/palette links never
	// point at a synthetic cluster scope.
	ShellCluster string
	// ShellNamespace is the real single-namespace scope passed to the page
	// shell. Multi-namespace search keeps Namespace as CSV for form round-trip,
	// but leaves the shell at cluster scope so sidebar/palette links never point
	// at a fake "a,b" namespace.
	ShellNamespace string

	IsAllClusters   bool
	IsAllNamespaces bool

	// OfferedTypes are the resource-type checkboxes, sorted by plural; Checked
	// marks the types in the current ?type= selection.
	OfferedTypes      []searchTypeOption
	SelectedTypeCount int // len(resource_types) -- drives the "type N" chip

	// SelectedTypes is the raw ?type= plural set the search ran with (or the
	// configured default when the request carried none). The redesign search
	// drops the in-body checkbox UI, so these round-trip as hidden form inputs to
	// preserve the type scope when the query box is re-submitted.
	SelectedTypes []string

	// ScopeClusters are the chips in the scope strip (search_clusters). When
	// IsAllClusters and ScopeClusters is empty, the strip shows "all clusters".
	// Each chip carries its own Failed/Reason/RetryHref, so the partial-failure
	// banner + foundline are derived from this slice (no separate error map).
	ScopeClusters []searchScopeCluster

	Results  []searchResult
	Duration time.Duration

	// RetryFailedHref is the read-only GET the partial-failure banner's "Retry
	// failed" action points at: the SAME search re-scoped to the comma-joined set
	// of clusters that failed to answer (cluster=<f1>,<f2>). Empty when no cluster
	// failed (the banner is then hidden).
	RetryFailedHref string
}

// searchTypeOption is one resource-type checkbox: the plural is the input value,
// the Kind is the label text, Checked marks a type in the current selection.
type searchTypeOption struct {
	Plural  string
	Kind    string
	Checked bool
}

// searchScopeCluster is one per-cluster scope chip in the redesign search
// `.ro-scope` strip (D11): the cluster Name, whether it Failed to answer (any
// per-cluster error record), the number of result cards it contributed
// (ResultCount, shown on an `.ok` chip), the short failure Reason (shown on an
// `.err` chip), and the read-only RetryHref that re-runs the SAME search scoped
// to just this cluster (cluster=<name>) so a failed cluster can be retried
// without leaving the GET surface.
type searchScopeCluster struct {
	Name        string
	Failed      bool
	ResultCount int
	Reason      string
	RetryHref   string
}

// searchResult is one search hit row in the redesign results table
// (Cluster/Namespace/Kind/Name/Age). Cluster + Namespace populate their own
// cells; Kind + Group + IsCRD drive the kind-icon resolver (icons.KindIcon) in
// the Kind cell; Created feeds the Age cell's bucket class. Labels feeds the
// sort score (searchScore ranks on Title + Labels). The redesign table drops the
// per-card snippet UI, so no match snippets are retained on the result row.
type searchResult struct {
	Title     string
	Kind      string
	Group     string
	IsCRD     bool
	Link      string
	Cluster   string
	Namespace string
	Created   string // formatTimestamp(creationTimestamp); "" => no age cell text
	AgeClass  string // the num + age-* bucket class for the Age cell
	Labels    map[string]string
}
