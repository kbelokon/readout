package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/kbelokon/readout/internal/web/templates"
)

// The v2 server limits deliberately match the strict browser decoder. A delta
// is optional optimization: anything outside these closed limits falls back to
// the already-bounded full snapshot without advancing the sequence twice.
const (
	streamMaxDeltaBytes                  = 256 << 10
	streamMaxFragmentBytes               = 128 << 10
	streamMaxDeltaOperations             = 20_000
	streamMaxDeltaKeyBytes               = 2 << 10
	streamMaxResourceVersionBytes        = 256
	streamMaxDeletedKeys                 = 20_000
	streamMaxSafeSequence         uint64 = 1<<53 - 1
)

var errStreamSequenceExhausted = errors.New("live stream sequence exceeds the JavaScript safe-integer range")

type streamLiveRenderers struct {
	full       func(context.Context, *templates.ListData) (string, error)
	projection liveProjectionRenderers
}

func defaultStreamLiveRenderers() streamLiveRenderers {
	return streamLiveRenderers{
		full: func(ctx context.Context, data *templates.ListData) (string, error) {
			var buf bytes.Buffer
			if err := templates.ResourceTable(*data).Render(ctx, &buf); err != nil {
				return "", err
			}
			return buf.String(), nil
		},
		projection: defaultLiveProjectionRenderers(),
	}
}

type livePreparedKind uint8

const (
	livePreparedNoop livePreparedKind = iota
	livePreparedSnapshot
	livePreparedDelta
)

type livePreparedPush struct {
	kind       livePreparedKind
	payload    []byte
	projection liveProjectionState
	reason     liveSnapshotReason
}

const (
	liveSnapshotForced     liveSnapshotReason = "forced"
	liveSnapshotCheckpoint liveSnapshotReason = "checkpoint"
	liveSnapshotDeltaLimit liveSnapshotReason = "delta-limit"
	liveSnapshotDeltaRatio liveSnapshotReason = "delta-ratio"
)

func (st *streamSession) currentListData() templates.ListData {
	clone := cloneTableForRender(st.rev.table)
	lc := st.srv.streamListContext(st.renderReq, st.client, st.cluster, &clone, st.overlaysForRender())
	view := st.srv.buildListView(st.renderReq, &lc)
	return toListData(&view)
}

func (st *streamSession) liveRenderers() streamLiveRenderers {
	renderers := st.renderers
	defaults := defaultStreamLiveRenderers()
	if renderers.full == nil {
		renderers.full = defaults.full
	}
	if renderers.projection.row == nil {
		renderers.projection.row = defaults.projection.row
	}
	if renderers.projection.card == nil {
		renderers.projection.card = defaults.projection.card
	}
	if renderers.projection.region == nil {
		renderers.projection.region = defaults.projection.region
	}
	return renderers
}

func (st *streamSession) pushLiveV2(ctx context.Context) error {
	prepared, err := st.prepareLiveV2(ctx, time.Now())
	if err != nil {
		return err
	}
	return st.pushPreparedLiveV2(&prepared)
}

// pushPreparedLiveV2 is the transactional write boundary: a prepared state is
// committed only after the complete SSE frame has been written and flushed.
func (st *streamSession) pushPreparedLiveV2(prepared *livePreparedPush) error {
	if prepared.kind == livePreparedNoop {
		st.commitLivePush(prepared, time.Now())
		return nil
	}
	if err := st.writeEncodedEvent("ro-live", prepared.payload); err != nil {
		return err
	}
	st.commitLivePush(prepared, time.Now())
	return nil
}

func (st *streamSession) prepareLiveV2(ctx context.Context, now time.Time) (livePreparedPush, error) {
	data := st.currentListData()
	return st.prepareLiveV2Data(ctx, &data, now)
}

func (st *streamSession) prepareLiveV2Data(ctx context.Context, data *templates.ListData, now time.Time) (livePreparedPush, error) {
	candidate, err := projectLiveList(data)
	if err != nil {
		return livePreparedPush{}, err
	}
	renderers := st.liveRenderers()

	if !st.projection.initialized {
		return st.prepareLiveSnapshot(ctx, data, &candidate, liveSnapshotUninitialized, renderers)
	}
	if st.forceSnapshot {
		return st.prepareLiveSnapshot(ctx, data, &candidate, liveSnapshotForced, renderers)
	}
	if st.tuning.checkpointInterval > 0 && !st.lastSnapshotAt.IsZero() &&
		!now.Before(st.lastSnapshotAt.Add(st.tuning.checkpointInterval)) {
		return st.prepareLiveSnapshot(ctx, data, &candidate, liveSnapshotCheckpoint, renderers)
	}

	result, err := diffLiveList(ctx, &st.projection, &candidate, data, st.deletedKeys, renderers.projection)
	if err != nil {
		return livePreparedPush{}, err
	}
	if result.RequireSnapshot {
		return st.prepareLiveSnapshot(ctx, data, &result.Projection, result.SnapshotReason, renderers)
	}
	if result.Delta == nil {
		return livePreparedPush{kind: livePreparedNoop, projection: result.Projection}, nil
	}
	if st.tuning.checkpointDeltas > 0 && st.deltasSinceSnapshot+1 >= st.tuning.checkpointDeltas {
		return st.prepareLiveSnapshot(ctx, data, &result.Projection, liveSnapshotCheckpoint, renderers)
	}
	if !liveDeltaWireSafe(result.Delta, &result.Projection) {
		return st.prepareLiveSnapshot(ctx, data, &result.Projection, liveSnapshotDeltaLimit, renderers)
	}

	next, err := st.nextLiveSequence()
	if err != nil {
		return livePreparedPush{}, err
	}
	payload, err := encodeStreamPayload(streamLiveEnvelope{
		V:      2,
		Kind:   "delta",
		G:      st.gen,
		Seq:    next,
		Rev:    result.Projection.revision,
		RV:     liveWireResourceVersion(st.lastRV),
		Schema: liveProjectionSchemaToken(&result.Projection),
		Delta:  result.Delta,
	}, streamMaxDeltaBytes)
	if err != nil {
		if errors.Is(err, errStreamEventTooLarge) {
			return st.prepareLiveSnapshot(ctx, data, &result.Projection, liveSnapshotDeltaLimit, renderers)
		}
		return livePreparedPush{}, err
	}
	if !liveDeltaWorthSending(len(payload), st.lastSnapshotBytes) {
		return st.prepareLiveSnapshot(ctx, data, &result.Projection, liveSnapshotDeltaRatio, renderers)
	}
	return livePreparedPush{
		kind:       livePreparedDelta,
		payload:    payload,
		projection: result.Projection,
	}, nil
}

func (st *streamSession) prepareLiveSnapshot(
	ctx context.Context,
	data *templates.ListData,
	candidate *liveProjectionState,
	reason liveSnapshotReason,
	renderers streamLiveRenderers,
) (livePreparedPush, error) {
	html, err := renderers.full(ctx, data)
	if err != nil {
		return livePreparedPush{}, err
	}
	if html == "" || len(html) > streamMaxEventBytes || !utf8.ValidString(html) {
		return livePreparedPush{}, errStreamEventTooLarge
	}
	next, err := st.nextLiveSequence()
	if err != nil {
		return livePreparedPush{}, err
	}
	payload, err := encodeStreamPayload(streamLiveEnvelope{
		V:      2,
		Kind:   "snapshot",
		G:      st.gen,
		Seq:    next,
		Rev:    candidate.revision,
		RV:     liveWireResourceVersion(st.lastRV),
		Schema: liveProjectionSchemaToken(candidate),
		Snapshot: &streamLiveSnapshot{
			HTML: html,
		},
	}, streamMaxEventBytes)
	if err != nil {
		return livePreparedPush{}, err
	}
	return livePreparedPush{
		kind:       livePreparedSnapshot,
		payload:    payload,
		projection: *candidate,
		reason:     reason,
	}, nil
}

func (st *streamSession) commitLivePush(prepared *livePreparedPush, now time.Time) {
	st.projection = prepared.projection
	st.dirty = false
	st.deletedKeys = nil
	st.forceSnapshot = false
	// The rendered revision is now the client-visible base, so the next push
	// classifies deletions against exactly what the browser holds.
	st.base = st.rev
	st.lastPush = now
	if prepared.kind == livePreparedNoop {
		return
	}
	st.seq++
	if prepared.kind == livePreparedSnapshot {
		st.lastSnapshotAt = now
		st.lastSnapshotBytes = len(prepared.payload)
		st.deltasSinceSnapshot = 0
		return
	}
	st.deltasSinceSnapshot++
}

func (st *streamSession) nextLiveSequence() (uint64, error) {
	if st.seq >= streamMaxSafeSequence {
		return 0, errStreamSequenceExhausted
	}
	return st.seq + 1, nil
}

func liveProjectionSchemaToken(projection *liveProjectionState) string {
	if projection == nil || !projection.initialized {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(projection.schema[:])
}

func liveDeltaWorthSending(deltaBytes, snapshotBytes int) bool {
	return deltaBytes >= 0 && snapshotBytes > 0 && deltaBytes*5 < snapshotBytes*3
}

func liveDeltaWireSafe(delta *liveProjectionDelta, candidate *liveProjectionState) bool {
	if delta == nil || candidate == nil || !candidate.initialized || delta.Base == "" || delta.Revision == "" || delta.Base == delta.Revision || delta.Revision != candidate.revision {
		return false
	}
	if len(delta.Removals)+len(delta.Upserts) > streamMaxDeltaOperations || len(delta.Order) > streamMaxDeltaOperations || len(delta.Regions) > 3 {
		return false
	}
	if len(delta.Removals)+len(delta.Upserts)+len(delta.Order)+len(delta.Regions) == 0 {
		return false
	}
	if delta.Order != nil && !slices.Equal(delta.Order, candidate.order) {
		return false
	}
	keys := make(map[string]uint8, len(delta.Removals)+len(delta.Upserts)+len(delta.Order))
	for _, removal := range delta.Removals {
		if !liveWireKey(removal.Key) || (removal.Cause != liveRemovalDelete && removal.Cause != liveRemovalProject) || keys[removal.Key]&1 != 0 {
			return false
		}
		keys[removal.Key] |= 1
	}
	fragmentBytes := 0
	for _, upsert := range delta.Upserts {
		if !liveWireKey(upsert.Key) || keys[upsert.Key]&2 != 0 || keys[upsert.Key]&1 != 0 ||
			!liveWireHTML(upsert.RowHTML) || (candidate.mode == liveProjectionCards && !liveWireHTML(upsert.CardHTML)) ||
			(candidate.mode != liveProjectionCards && upsert.CardHTML != "") {
			return false
		}
		keys[upsert.Key] |= 2
		fragmentBytes += len(upsert.RowHTML) + len(upsert.CardHTML)
		if fragmentBytes > streamMaxDeltaBytes {
			return false
		}
	}
	orderKeys := make(map[string]struct{}, len(delta.Order))
	for _, key := range delta.Order {
		if !liveWireKey(key) {
			return false
		}
		if _, duplicate := orderKeys[key]; duplicate {
			return false
		}
		orderKeys[key] = struct{}{}
	}
	regions := make(map[liveProjectionRegion]struct{}, len(delta.Regions))
	for _, patch := range delta.Regions {
		if patch.Region != liveRegionCount && patch.Region != liveRegionPhase && patch.Region != liveRegionFound {
			return false
		}
		if _, duplicate := regions[patch.Region]; duplicate {
			return false
		}
		regions[patch.Region] = struct{}{}
		if !liveWireHTML(patch.HTML) {
			return false
		}
		fragmentBytes += len(patch.HTML)
		if fragmentBytes > streamMaxDeltaBytes {
			return false
		}
	}
	return true
}

func liveWireKey(key string) bool {
	return key != "" && len(key) <= streamMaxDeltaKeyBytes && utf8.ValidString(key) && !liveHasControls(key)
}

func liveWireHTML(html string) bool {
	return html != "" && len(html) <= streamMaxFragmentBytes && utf8.ValidString(html)
}

func liveWireResourceVersion(rv string) string {
	if rv == "" || len(rv) > streamMaxResourceVersionBytes || !utf8.ValidString(rv) || liveHasControls(rv) {
		return ""
	}
	return rv
}

func liveHasControls(value string) bool {
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

func (st *streamSession) terminalLiveV2(reason string) {
	next, err := st.nextLiveSequence()
	if err != nil {
		return
	}
	envelope := streamLiveEnvelope{
		V:      2,
		Kind:   "terminal",
		G:      st.gen,
		Seq:    next,
		Reason: reason,
	}
	if st.projection.initialized {
		envelope.Rev = st.projection.revision
		envelope.Schema = liveProjectionSchemaToken(&st.projection)
	}
	envelope.RV = liveWireResourceVersion(st.lastRV)
	if st.writeEvent("ro-live", envelope) == nil {
		st.seq = next
	}
}
