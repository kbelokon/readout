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

	// AllNamespacesHref is the precomputed "Show <plural> across all namespaces"
	// link target (carries the current query string). Empty when the link should
	// not render.
	AllNamespacesHref string
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

	Tools toolsView

	Rows []rowView
}

// columnView precomputes a column header's sort link and indicator.
type columnView struct {
	SortHref string
	SortIcon string
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
	// muted hash suffix for the sticky name cell. NameHead+NameTail == Value.
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
	// entry (sorted), each carrying its chip class (the .app accent for
	// app.kubernetes.io/* labels) + its "key: value" text. Empty means a namespace
	// with no labels, rendered as a muted "—".
	Chips []chipView
}

// chipView is one namespace label chip: Class is the FULL redesign chip class
// (the canonical "ro-chip" + the " app" accent token for app.kubernetes.io/*
// labels, scoped under the list shell's .ro-rd marker), Text is the "key: value"
// label shown in the pill.
type chipView struct {
	Class string
	Text  string
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
	Node        *nodeSummaryView // non-nil only for Kind == Node

	Secret    *secretDataView // non-nil only for masked Secret with data
	YAMLCards []yamlCardView
}

// labelChipView is one resolved label chip: the selector href, the full chip
// class (incl. the app accent), and the key/value.
type labelChipView struct {
	Href  string
	Class string
	Key   string
	Val   string
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
type yamlCardView struct {
	Name    string
	Title   string
	Content string
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
}

// eventView is one rendered event row (already flattened from the raw object).
// Tone is the redesign status tone for the Type cell (mute for Normal/unknown,
// warn for Warning), mapped from the events/Type cell class via statusTone.
// AgeClass is the age bucket for lastTimestamp at days=1 (the .age-* token on the
// Age <td>). The Reason cell renders plain and From faint in the redesign; the
// Message <td>'s ro-event-msg class is static (emitted in the templ).
type eventView struct {
	Type     string
	Tone     string
	Reason   string
	Age      string
	AgeClass string
	From     string
	Message  string
}

// searchView is the view model for the search page. Every value the
// rich search render needs -- the form round-trip, the offered resource-type
// checkboxes (with checked state), the scope chips, the result cards (with
// snippet tuples + label chips + meta), the count footer, and the per-cluster
// error records -- is resolved in buildSearchView and carried here as plain
// data. No *http.Request and no kube.Client cross this boundary.
type searchView struct {
	Query     string
	Cluster   string
	Namespace string

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
	ScopeClusters []searchScopeCluster

	Results  []searchResult
	Duration time.Duration

	// ClusterErrors are the per-cluster error records (partial-failure):
	// failures are collected as records and surfaced as `message is-danger`
	// articles, not raised as hard errors. ErrorClusterOrder fixes the render
	// order (first-seen) so output is deterministic.
	ClusterErrors     map[string][]searchClusterError
	ErrorClusterOrder []string

	// AllNamespacesHref is the precomputed "Repeat search across all namespaces"
	// target (rel_url with namespace=''). Empty when already all-namespaces.
	AllNamespacesHref string

	// RetryFailedHref is the read-only GET the partial-failure banner's "Retry
	// failed" action points at: the SAME search re-scoped to the comma-joined set
	// of clusters that failed to answer (cluster=<f1>,<f2>). Empty when no cluster
	// failed (the banner is then hidden).
	RetryFailedHref string

	SearchedClusterCount int // len(search_clusters) -- footer
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

// searchClusterError is one per-cluster error record: ResourceType is the plural
// that failed and Message is the error text shown in the danger article.
type searchClusterError struct {
	ResourceType string
	Message      string
}

// searchResult is one search hit row in the redesign results table
// (Cluster/Namespace/Kind/Name/Age). Cluster + Namespace populate their own
// cells; Kind + Group + IsCRD drive the kind-icon resolver (icons.KindIcon) in
// the Kind cell; Created feeds the Age cell's bucket class. Labels still feeds
// the sort score. Matches/LabelChips are retained for the score/relevance ranking
// (the redesign table drops the per-card snippet + label-chip UI).
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
	Matches   []snippet
}

// snippet is one match context window: the text before the match, the matched
// text (wrapped in <em>), and the text after.
type snippet struct {
	Pre   string
	Match string
	Post  string
}
