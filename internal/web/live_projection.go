package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"slices"

	"github.com/a-h/templ"
	"github.com/kbelokon/readout/internal/web/templates"
)

// Live v2 deliberately keeps transport concerns out of this file. A projection
// is an immutable, compact description of the last successfully-written list;
// diffLiveList returns the candidate state alongside a typed delta and leaves it
// to the stream writer to commit that state only after the event was written.
// In particular, neither a prior ListData nor prior rendered HTML is retained.

const liveProjectionHashDomain = "readout.live-projection.v2\x00"

type liveProjectionMode uint8

const (
	liveProjectionUnsupported liveProjectionMode = iota
	liveProjectionEmpty
	liveProjectionCards
	liveProjectionWindowed
)

type liveSnapshotReason string

const (
	liveSnapshotUninitialized  liveSnapshotReason = "uninitialized"
	liveSnapshotListState      liveSnapshotReason = "list-state"
	liveSnapshotMultiTable     liveSnapshotReason = "multi-table"
	liveSnapshotIdentity       liveSnapshotReason = "identity"
	liveSnapshotSchema         liveSnapshotReason = "schema"
	liveSnapshotEmptyBoundary  liveSnapshotReason = "empty-boundary"
	liveSnapshotWindowBoundary liveSnapshotReason = "window-boundary"
	liveSnapshotRevision       liveSnapshotReason = "unmapped-revision"
)

type liveProjectionState struct {
	initialized bool
	revision    string
	schema      [sha256.Size]byte
	mode        liveProjectionMode
	boundary    liveSnapshotReason
	order       []string
	rows        map[string][sha256.Size]byte
	chrome      liveProjectionChrome
}

type liveProjectionChrome struct {
	count int
	phase liveProjectionPhase
	found liveProjectionFound
}

type liveProjectionPhase struct {
	semantic [sha256.Size]byte
	duration string
}

type liveProjectionFound struct {
	totalRows    int
	tableCount   int
	clusterCount int
	duration     string
}

type liveRemovalCause string

const (
	liveRemovalDelete  liveRemovalCause = "delete"
	liveRemovalProject liveRemovalCause = "project"
)

type liveProjectionRegion string

const (
	liveRegionCount liveProjectionRegion = "count"
	liveRegionPhase liveProjectionRegion = "phase"
	liveRegionFound liveProjectionRegion = "found"
)

// liveProjectionDelta is the transport-neutral semantic patch. Its HTML values
// are closed template components (one row, one optional mobile card, or one
// stable chrome region), never arbitrary selectors or a full ResourceTable.
type liveProjectionDelta struct {
	Base     string                      `json:"base"`
	Revision string                      `json:"rev"`
	Removals []liveProjectionRemoval     `json:"remove,omitempty"`
	Upserts  []liveProjectionUpsert      `json:"upsert,omitempty"`
	Order    []string                    `json:"order,omitempty"`
	Regions  []liveProjectionRegionPatch `json:"regions,omitempty"`
}

type liveProjectionRemoval struct {
	Key   string           `json:"key"`
	Cause liveRemovalCause `json:"cause"`
}

type liveProjectionUpsert struct {
	Key      string `json:"key"`
	RowHTML  string `json:"row"`
	CardHTML string `json:"card,omitempty"`
}

type liveProjectionRegionPatch struct {
	Region liveProjectionRegion `json:"region"`
	HTML   string               `json:"html"`
}

// liveProjectionResult is transactional: Projection is the candidate for the
// current ListData, but the caller must not replace its committed state until a
// delta or required snapshot has been written successfully.
type liveProjectionResult struct {
	Projection      liveProjectionState
	Delta           *liveProjectionDelta
	RequireSnapshot bool
	SnapshotReason  liveSnapshotReason
}

// liveProjectionRenderers are injectable both to isolate the pure diff in tests
// and to make render-count assertions exact. There is intentionally no full-list
// renderer here: the diff stops at the wire budget, but choosing and rendering
// the snapshot that replaces an over-budget delta belongs to the handler.
type liveProjectionRenderers struct {
	row    func(context.Context, *templates.ListData, *templates.TableData, *templates.TableRow) (string, error)
	card   func(context.Context, *templates.ListData, *templates.TableData, *templates.TableRow) (string, error)
	region func(context.Context, liveProjectionRegion, *templates.ListData, *templates.TableData) (string, error)
}

func defaultLiveProjectionRenderers() liveProjectionRenderers {
	return liveProjectionRenderers{
		row: func(ctx context.Context, data *templates.ListData, table *templates.TableData, row *templates.TableRow) (string, error) {
			return renderLiveProjectionComponent(ctx, templates.LiveTableRow(*data, *table, *row))
		},
		card: func(ctx context.Context, data *templates.ListData, table *templates.TableData, row *templates.TableRow) (string, error) {
			return renderLiveProjectionComponent(ctx, templates.LiveResourceCard(*data, *table, *row))
		},
		region: func(ctx context.Context, region liveProjectionRegion, data *templates.ListData, table *templates.TableData) (string, error) {
			switch region {
			case liveRegionCount:
				return renderLiveProjectionComponent(ctx, templates.LiveCountRegion(*table))
			case liveRegionPhase:
				return renderLiveProjectionComponent(ctx, templates.LivePhaseRegion(*data, *table))
			case liveRegionFound:
				return renderLiveProjectionComponent(ctx, templates.LiveFoundRegion(*data))
			default:
				return "", fmt.Errorf("unknown Live projection region %q", region)
			}
		},
	}
}

func renderLiveProjectionComponent(ctx context.Context, component templ.Component) (string, error) {
	var buffer bytes.Buffer
	if err := component.Render(ctx, &buffer); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// projectLiveList performs the sole O(rows) semantic scan. It accepts unsupported
// shapes as candidates but marks them fail-closed: diffLiveList will require a
// snapshot and will never attempt a row delta for a list state, multiple tables,
// or missing/duplicate row identities.
func projectLiveList(data *templates.ListData) (liveProjectionState, error) {
	if data == nil {
		return liveProjectionState{}, errors.New("project Live list: nil data")
	}

	schema, err := liveProjectionSchema(data)
	if err != nil {
		return liveProjectionState{}, fmt.Errorf("project Live list schema: %w", err)
	}

	projection := liveProjectionState{
		initialized: true,
		schema:      schema,
		mode:        liveProjectionUnsupported,
	}
	switch {
	case data.State.Kind != "":
		projection.boundary = liveSnapshotListState
		fallback, fallbackErr := liveProjectionUnsupportedDigest(data)
		if fallbackErr != nil {
			return liveProjectionState{}, fmt.Errorf("project Live list-state fallback: %w", fallbackErr)
		}
		projection.revision = liveProjectionRevision(&projection, &fallback)
		return projection, nil
	case len(data.Tables) != 1:
		projection.boundary = liveSnapshotMultiTable
		fallback, fallbackErr := liveProjectionUnsupportedDigest(data)
		if fallbackErr != nil {
			return liveProjectionState{}, fmt.Errorf("project Live multi-table fallback: %w", fallbackErr)
		}
		projection.revision = liveProjectionRevision(&projection, &fallback)
		return projection, nil
	}

	table := &data.Tables[0]
	projection.mode = liveProjectionCards
	if len(table.Rows) == 0 {
		projection.mode = liveProjectionEmpty
	} else if templates.TableWindowed(table) {
		projection.mode = liveProjectionWindowed
	}
	projection.order = make([]string, 0, len(table.Rows))
	projection.rows = make(map[string][sha256.Size]byte, len(table.Rows))
	identitiesValid := true
	for i := range table.Rows {
		row := &table.Rows[i]
		digest, hashErr := liveProjectionRowDigest(row)
		if hashErr != nil {
			return liveProjectionState{}, fmt.Errorf("project Live row %q: %w", row.Key, hashErr)
		}
		if row.Key == "" {
			identitiesValid = false
			continue
		}
		if _, duplicate := projection.rows[row.Key]; duplicate {
			identitiesValid = false
			continue
		}
		projection.order = append(projection.order, row.Key)
		projection.rows[row.Key] = digest
	}

	phase, err := liveProjectionPhaseFor(data, table)
	if err != nil {
		return liveProjectionState{}, fmt.Errorf("project Live phase: %w", err)
	}
	projection.chrome = liveProjectionChrome{
		count: table.Count,
		phase: phase,
		found: liveProjectionFound{
			totalRows:    data.TotalRows,
			tableCount:   data.TableCount,
			clusterCount: data.ClusterCount,
			duration:     templates.FormatListDuration(data.DurationSeconds),
		},
	}
	if !identitiesValid {
		projection.mode = liveProjectionUnsupported
		projection.boundary = liveSnapshotIdentity
		projection.order = nil
		projection.rows = nil
		fallback, fallbackErr := liveProjectionUnsupportedDigest(data)
		if fallbackErr != nil {
			return liveProjectionState{}, fmt.Errorf("project Live identity fallback: %w", fallbackErr)
		}
		projection.revision = liveProjectionRevision(&projection, &fallback)
		return projection, nil
	}
	projection.revision = liveProjectionRevision(&projection, nil)
	return projection, nil
}

// diffLiveList renders only changed/new rows and changed closed regions, and
// stops as soon as the wire budget is spent so an over-budget delta never pays
// for fragments the handler would throw away. It does not mutate previous or
// current. deletedKeys classifies a missing prior key as an actual watch
// deletion; all other exits are projection changes (for example a row no longer
// matching the active filter).
func diffLiveList(
	ctx context.Context,
	previous *liveProjectionState,
	current *liveProjectionState,
	data *templates.ListData,
	deletedKeys map[string]struct{},
	renderers liveProjectionRenderers,
) (liveProjectionResult, error) {
	if previous == nil || current == nil {
		return liveProjectionResult{}, errors.New("diff Live list: nil projection")
	}
	result := liveProjectionResult{Projection: *current}
	if data == nil {
		return liveProjectionResult{}, errors.New("diff Live list: nil data")
	}
	if !current.initialized {
		return liveProjectionResult{}, errors.New("diff Live list: uninitialized candidate")
	}
	if !previous.initialized {
		return liveProjectionSnapshot(&result, liveSnapshotUninitialized), nil
	}
	if previous.boundary != "" || current.boundary != "" {
		reason := current.boundary
		if reason == "" {
			reason = previous.boundary
		}
		return liveProjectionSnapshot(&result, reason), nil
	}
	if previous.schema != current.schema {
		return liveProjectionSnapshot(&result, liveSnapshotSchema), nil
	}
	if previous.mode != current.mode {
		reason := liveSnapshotWindowBoundary
		if previous.mode == liveProjectionEmpty || current.mode == liveProjectionEmpty {
			reason = liveSnapshotEmptyBoundary
		}
		return liveProjectionSnapshot(&result, reason), nil
	}
	if previous.revision == current.revision {
		// Duration is intentionally not part of the semantic revision: a timing-
		// only watch tick emits no frame. Keep the committed projection (and thus
		// its last-rendered timing tokens), so the next real delta can repair both
		// timing mounts if their formatted display has moved meanwhile.
		result.Projection = *previous
		return result, nil
	}
	if len(data.Tables) != 1 || len(data.Tables[0].Rows) != len(current.order) {
		return liveProjectionResult{}, errors.New("diff Live list: candidate shape does not match current data")
	}

	delta := &liveProjectionDelta{Base: previous.revision, Revision: current.revision}
	for _, key := range previous.order {
		if _, present := current.rows[key]; present {
			continue
		}
		cause := liveRemovalProject
		if _, deleted := deletedKeys[key]; deleted {
			cause = liveRemovalDelete
		}
		delta.Removals = append(delta.Removals, liveProjectionRemoval{Key: key, Cause: cause})
	}

	// The wire budget is spent while the diff renders, not audited after it.
	// An over-budget delta is discarded and the whole table is re-rendered as a
	// snapshot, so every fragment produced past the limit is work paid twice.
	// These checks mirror liveDeltaWireSafe exactly, so the fallback a caller
	// sees is unchanged -- only the wasted rendering is gone.
	if len(delta.Removals) > streamMaxDeltaOperations {
		return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
	}
	orderChanged := !slices.Equal(previous.order, current.order)
	if orderChanged && len(current.order) > streamMaxDeltaOperations {
		return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
	}
	fragmentBytes := 0

	table := &data.Tables[0]
	for i := range table.Rows {
		row := &table.Rows[i]
		if row.Key != current.order[i] {
			return liveProjectionResult{}, errors.New("diff Live list: candidate order does not match current data")
		}
		priorHash, existed := previous.rows[row.Key]
		if existed && priorHash == current.rows[row.Key] {
			continue
		}
		if renderers.row == nil {
			return liveProjectionResult{}, errors.New("diff Live list: nil row renderer")
		}
		rowHTML, renderErr := renderers.row(ctx, data, table, row)
		if renderErr != nil {
			return liveProjectionResult{}, fmt.Errorf("render Live row %q: %w", row.Key, renderErr)
		}
		if rowHTML == "" {
			return liveProjectionResult{}, fmt.Errorf("render Live row %q: empty fragment", row.Key)
		}
		if len(rowHTML) > streamMaxFragmentBytes {
			return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
		}
		upsert := liveProjectionUpsert{Key: row.Key, RowHTML: rowHTML}
		if current.mode == liveProjectionCards {
			if renderers.card == nil {
				return liveProjectionResult{}, errors.New("diff Live list: nil card renderer")
			}
			cardHTML, cardErr := renderers.card(ctx, data, table, row)
			if cardErr != nil {
				return liveProjectionResult{}, fmt.Errorf("render Live card %q: %w", row.Key, cardErr)
			}
			if cardHTML == "" {
				return liveProjectionResult{}, fmt.Errorf("render Live card %q: empty fragment", row.Key)
			}
			if len(cardHTML) > streamMaxFragmentBytes {
				return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
			}
			upsert.CardHTML = cardHTML
		}
		delta.Upserts = append(delta.Upserts, upsert)
		fragmentBytes += len(upsert.RowHTML) + len(upsert.CardHTML)
		if fragmentBytes > streamMaxDeltaBytes || len(delta.Removals)+len(delta.Upserts) > streamMaxDeltaOperations {
			return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
		}
	}
	if orderChanged {
		delta.Order = slices.Clone(current.order)
	}

	regions := [...]struct {
		name    liveProjectionRegion
		changed bool
	}{
		{name: liveRegionCount, changed: previous.chrome.count != current.chrome.count},
		{name: liveRegionPhase, changed: previous.chrome.phase != current.chrome.phase},
		{name: liveRegionFound, changed: previous.chrome.found != current.chrome.found},
	}
	for _, region := range regions {
		if !region.changed {
			continue
		}
		if renderers.region == nil {
			return liveProjectionResult{}, errors.New("diff Live list: nil region renderer")
		}
		html, regionErr := renderers.region(ctx, region.name, data, table)
		if regionErr != nil {
			return liveProjectionResult{}, fmt.Errorf("render Live %s region: %w", region.name, regionErr)
		}
		if html == "" {
			return liveProjectionResult{}, fmt.Errorf("render Live %s region: empty fragment", region.name)
		}
		if len(html) > streamMaxFragmentBytes {
			return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
		}
		delta.Regions = append(delta.Regions, liveProjectionRegionPatch{Region: region.name, HTML: html})
		fragmentBytes += len(html)
		if fragmentBytes > streamMaxDeltaBytes {
			return liveProjectionSnapshot(&result, liveSnapshotDeltaLimit), nil
		}
	}

	if len(delta.Removals) == 0 && len(delta.Upserts) == 0 && len(delta.Order) == 0 && len(delta.Regions) == 0 {
		// A revision change that cannot be represented by a known closed patch is
		// a normalization bug or a newly-added ListData field. Never acknowledge
		// it with an empty delta: a snapshot is the safe automatic fallback.
		return liveProjectionSnapshot(&result, liveSnapshotRevision), nil
	}
	result.Delta = delta
	return result, nil
}

func liveProjectionSnapshot(result *liveProjectionResult, reason liveSnapshotReason) liveProjectionResult {
	result.RequireSnapshot = true
	result.SnapshotReason = reason
	return *result
}

func liveProjectionSchema(data *templates.ListData) ([sha256.Size]byte, error) {
	semantic := *data
	semantic.DurationSeconds = 0
	semantic.ShowStaleBanner = false
	semantic.TotalRows = 0
	semantic.TableCount = 0
	semantic.ClusterCount = 0
	semantic.Tables = slices.Clone(data.Tables)
	for i := range semantic.Tables {
		semantic.Tables[i].Count = 0
		semantic.Tables[i].Phase = nil
		semantic.Tables[i].PhaseRows = 0
		semantic.Tables[i].Rows = nil
	}
	semanticDigest, err := liveProjectionDigest("schema", &semantic)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	// Renderer identity belongs to the schema boundary: a new binary must force
	// one authoritative snapshot, but it must not be repeated in every row hash.
	digest := sha256.New()
	liveProjectionWriteString(digest, "schema-renderer")
	liveProjectionWriteBytes(digest, semanticDigest[:])
	renderer := resourceListRendererFingerprint()
	liveProjectionWriteBytes(digest, renderer[:])
	return [sha256.Size]byte(digest.Sum(nil)), nil
}

func liveProjectionPhaseFor(data *templates.ListData, table *templates.TableData) (liveProjectionPhase, error) {
	semantic, err := liveProjectionPhaseDigest(table)
	if err != nil {
		return liveProjectionPhase{}, err
	}
	phase := liveProjectionPhase{semantic: semantic}
	if len(table.Phase) > 0 {
		phase.duration = templates.FormatListDuration(data.DurationSeconds)
	}
	return phase, nil
}

func liveProjectionPhaseDigest(table *templates.TableData) ([sha256.Size]byte, error) {
	if len(table.Phase) == 0 {
		// The hidden mount renders neither PhaseRows nor timing. Canonicalizing it
		// avoids a same-markup delta for nil/empty tallies or a stray total.
		return liveProjectionDigest("phase", struct{ Visible bool }{Visible: false})
	}
	return liveProjectionDigest("phase", struct {
		Visible   bool
		Phase     []templates.PhaseChip
		PhaseRows int
	}{Visible: true, Phase: table.Phase, PhaseRows: table.PhaseRows})
}

// liveProjectionRevision composes the already-partitioned semantic hashes. It
// never marshals the full ListData and intentionally excludes the formatted
// duration tokens: timing-only ticks do not consume sequence numbers or wire.
func liveProjectionRevision(projection *liveProjectionState, fallback *[sha256.Size]byte) string {
	digest := sha256.New()
	liveProjectionWriteString(digest, "revision")
	liveProjectionWriteBytes(digest, projection.schema[:])
	liveProjectionWriteInt(digest, int(projection.mode))
	liveProjectionWriteString(digest, string(projection.boundary))
	liveProjectionWriteInt(digest, projection.chrome.count)
	liveProjectionWriteBytes(digest, projection.chrome.phase.semantic[:])
	liveProjectionWriteInt(digest, projection.chrome.found.totalRows)
	liveProjectionWriteInt(digest, projection.chrome.found.tableCount)
	liveProjectionWriteInt(digest, projection.chrome.found.clusterCount)
	if fallback != nil {
		liveProjectionWriteBytes(digest, fallback[:])
	} else {
		liveProjectionWriteInt(digest, len(projection.order))
		for _, key := range projection.order {
			liveProjectionWriteString(digest, key)
			row := projection.rows[key]
			liveProjectionWriteBytes(digest, row[:])
		}
	}
	return "ro-live-v2-" + base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

// liveProjectionUnsupportedDigest is the rare fail-closed path for shapes that
// cannot carry deltas. It streams every dynamic partition into a fixed-size hash
// rather than allocating a full JSON representation, so snapshot revisions still
// change safely for multi-table and invalid-identity content.
func liveProjectionUnsupportedDigest(data *templates.ListData) ([sha256.Size]byte, error) {
	digest := sha256.New()
	liveProjectionWriteString(digest, "unsupported")
	liveProjectionWriteInt(digest, data.TotalRows)
	liveProjectionWriteInt(digest, data.TableCount)
	liveProjectionWriteInt(digest, data.ClusterCount)
	liveProjectionWriteInt(digest, len(data.Tables))
	for tableIndex := range data.Tables {
		table := &data.Tables[tableIndex]
		liveProjectionWriteInt(digest, tableIndex)
		liveProjectionWriteInt(digest, table.Count)
		phase, err := liveProjectionPhaseDigest(table)
		if err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("table %d phase: %w", tableIndex, err)
		}
		liveProjectionWriteBytes(digest, phase[:])
		liveProjectionWriteInt(digest, len(table.Rows))
		for rowIndex := range table.Rows {
			row, rowErr := liveProjectionRowDigest(&table.Rows[rowIndex])
			if rowErr != nil {
				return [sha256.Size]byte{}, fmt.Errorf("table %d row %d: %w", tableIndex, rowIndex, rowErr)
			}
			liveProjectionWriteInt(digest, rowIndex)
			liveProjectionWriteBytes(digest, row[:])
		}
	}
	return [sha256.Size]byte(digest.Sum(nil)), nil
}

func liveProjectionWriteString(digest hash.Hash, value string) {
	liveProjectionWriteBytes(digest, []byte(value))
}

func liveProjectionWriteBytes(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}

func liveProjectionWriteInt(digest hash.Hash, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(int64(value)))
	_, _ = digest.Write(encoded[:])
}

func liveProjectionDigest(domain string, value any) ([sha256.Size]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(liveProjectionHashDomain))
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
	return [sha256.Size]byte(hash.Sum(nil)), nil
}

// liveProjectionRowDigest excludes presentation that changes merely because
// the server clock advanced. ResourceVersion remains in the row, so a real
// Kubernetes update still changes the digest even if its only visible change
// is a volatile Event age.
func liveProjectionRowDigest(row *templates.TableRow) ([sha256.Size]byte, error) {
	semantic := resourceStateTableRow(row)
	return liveProjectionDigest("row", &semantic)
}
