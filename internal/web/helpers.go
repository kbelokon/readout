package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func podColor(name string) string {
	return fmt.Sprintf("log-c%d", crc32.ChecksumIEEE([]byte(name))%8)
}

func isLocalRedirect(target string) bool {
	return strings.HasPrefix(target, "/") &&
		!strings.HasPrefix(target, "//") &&
		!strings.HasPrefix(target, `/\`)
}

// podNameHashSuffix matches a Deployment-style pod name (`<workload>-<rs
// hash>-<pod hash>`) so the workload prefix can render bright and the trailing
// hash segments muted. Mirrors the design reference shell.js podName():
// `^(.*?)(-[a-z0-9]{6,10})(-[a-z0-9]{4,5})$`.
var podNameHashSuffix = regexp.MustCompile(`^(.*?)(-[a-z0-9]{6,10}-[a-z0-9]{4,5})$`)

// replicaSetNameHashSuffix matches a ReplicaSet-style name (`<workload>-<hash>`),
// a single trailing template-hash segment. Used only for the replicasets kind so
// arbitrary names (services, configmaps) are never split.
var replicaSetNameHashSuffix = regexp.MustCompile(`^(.*?)(-[a-z0-9]{6,10})$`)

// splitObjectName splits an identifier into a bright head + a muted hash tail for
// the sticky name cell. Pod and ReplicaSet names carry a generated template hash
// suffix that is noise next to the workload name, so it is rendered muted; for
// every other kind (and any name without a recognisable suffix) the whole name is
// the head and the tail is empty. The invariant the cell relies on is
// head+tail == name for ALL inputs (the tail only ever moves the trailing hash
// out of the head; it never drops or rewrites a character).
func splitObjectName(plural, name string) (head, tail string) {
	switch plural {
	case "pods":
		if m := podNameHashSuffix.FindStringSubmatch(name); len(m) == 3 && m[1] != "" {
			return m[1], m[2]
		}
	case "replicasets":
		if m := replicaSetNameHashSuffix.FindStringSubmatch(name); len(m) == 3 && m[1] != "" {
			return m[1], m[2]
		}
	}
	return name, ""
}

// statusTone maps the existing kube.CellClass Bulma text-color tone onto the
// redesign status-dot tone vocabulary (ok/warn/err/info/mute). An empty Bulma
// class (an unmocked kind, or a value with no recognised status) yields "" so the
// generic fallback cell emits a dot with no tone colour.
func statusTone(bulmaClass string) string {
	switch bulmaClass {
	case "has-text-success":
		return "ok"
	case "has-text-warning":
		return "warn"
	case "has-text-danger":
		return "err"
	case "has-text-info":
		return "info"
	case "has-text-grey":
		return "mute"
	default:
		return ""
	}
}

// transientPodPhase reports whether a pod status phase is an in-flight state that
// should animate (the dot gets .pulse). Per the design rulebook ONLY in-flight
// states pulse; steady states (Running/Completed/CrashLoopBackOff/Error/...)
// never animate.
func transientPodPhase(value string) bool {
	switch strings.TrimSpace(value) {
	case "ContainerCreating", "Terminating", "PodInitializing", "Pending":
		return true
	}
	// Init:N/M progress (e.g. "Init:0/1") is an in-flight state too.
	return strings.HasPrefix(value, "Init:") && !strings.Contains(value, "Error") && !strings.Contains(value, "BackOff")
}

// readyRatioClass classifies a `n/d` ready ratio into the redesign tone
// (full/partial/zero): all-ready -> full (green), some-ready -> partial (amber),
// none-ready -> zero (faint). A value that is not an `n/d` ratio yields "".
func readyRatioClass(value string) string {
	left, right, ok := strings.Cut(value, "/")
	if !ok {
		return ""
	}
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "0" {
		return "zero"
	}
	if left == right {
		return "full"
	}
	return "partial"
}

// splitRestarts splits a kube Table Restarts cell into its count and an optional
// "(… ago)" suffix. The server-side Table renders a restarted pod as
// "2 (38h ago)"; an unrestarted pod is just "0". The count drives the
// .restarts.zero|some tone, the ago string (when present) renders muted after it.
func splitRestarts(value string) (count, ago string) {
	value = strings.TrimSpace(value)
	if idx := strings.Index(value, " ("); idx >= 0 {
		return strings.TrimSpace(value[:idx]), strings.TrimSpace(value[idx+1:])
	}
	return value, ""
}

// restartsTone is the .restarts tone for a count cell: "zero" when the restart
// count is 0/empty/non-numeric-zero, else "some".
func restartsTone(count string) string {
	count = strings.TrimSpace(count)
	if count == "" || count == "0" {
		return "zero"
	}
	return "some"
}

// capacityBucket classifies a usage percentage into the node capacity-bar bucket
// lifted verbatim from the mockup capCell: pct > 80 -> "hi" (red), pct > 55 ->
// "mid" (amber), else "lo" (green). The bucket (and its coloured fill) is meant
// to apply ONLY when a real usage % exists; the no-metrics path never calls this.
func capacityBucket(pct float64) string {
	switch {
	case pct > 80:
		return "hi"
	case pct > 55:
		return "mid"
	default:
		return "lo"
	}
}

// nodeRoles reads a node's role labels (`node-role.kubernetes.io/<role>`) off the
// row object's metadata.labels and returns the role names sorted, with
// "control-plane" first so it leads the chip strip (and earns the .cp accent in
// the renderer). A node with no role label yields nil (the renderer shows nothing
// rather than inventing a "worker" chip the labels did not assert).
func nodeRoles(obj map[string]any) []string {
	labels, _, _ := unstructured.NestedStringMap(obj, "metadata", "labels")
	var roles []string
	for key := range labels {
		if role, ok := strings.CutPrefix(key, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Slice(roles, func(i, j int) bool {
		// control-plane / master lead; the rest stay alphabetical.
		li, lj := roleRank(roles[i]), roleRank(roles[j])
		if li != lj {
			return li < lj
		}
		return roles[i] < roles[j]
	})
	return roles
}

// roleRank ranks a node role for the chip order: control-plane/master lead, then
// everything else alphabetically. Keeps the control-plane chip first regardless of
// label-map iteration order.
func roleRank(role string) int {
	switch role {
	case "control-plane":
		return 0
	case "master":
		return 1
	default:
		return 2
	}
}

// nodeAbnormalConditions returns the node's ABNORMAL condition pills (only the
// conditions not in their healthy state), each with its redesign pill tone. Node
// is a fixed kind, so the object is decoded once into a corev1.Node and the
// conditions are read off the typed Status.Conditions. A clean node yields nil, so
// the renderer shows the muted "—". The abnormality + tone reuse the same semantic
// rule as the detail-page nodeConditionTone (Ready healthy when True; pressure /
// NetworkUnavailable healthy when False), mapped to the list pill tone vocabulary.
func nodeAbnormalConditions(obj map[string]any) []condPill {
	var node corev1.Node
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &node); err != nil {
		return nil
	}
	var pills []condPill
	for _, cond := range node.Status.Conditions {
		typ, value := string(cond.Type), string(cond.Status)
		tone, abnormal := nodeConditionListTone(typ, value)
		if abnormal {
			pills = append(pills, condPill{Name: typ, Tone: tone})
		}
	}
	return pills
}

// nodeConditionListTone maps a node condition (type + status) to the LIST pill
// tone (ok/warn/err) and reports whether the condition is ABNORMAL (worth a pill).
// The healthy polarity matches the detail-page nodeConditionTone: Ready is healthy
// True; MemoryPressure/DiskPressure/PIDPressure are healthy False (warn when set);
// NetworkUnavailable is healthy False (err when set). A condition in its healthy
// state is not abnormal (abnormal=false), so only the off-normal ones surface.
func nodeConditionListTone(typ, status string) (tone string, abnormal bool) {
	switch typ {
	case "Ready":
		if status == "True" {
			return "ok", false
		}
		return "err", true
	case "NetworkUnavailable":
		if status == "True" {
			return "err", true
		}
		return "ok", false
	case "MemoryPressure", "DiskPressure", "PIDPressure":
		if status == "True" {
			return "warn", true
		}
		return "ok", false
	default:
		// An unknown condition type is surfaced only when it is explicitly not True
		// (a True unknown condition is treated as informational/healthy).
		if status == "True" {
			return "warn", false
		}
		return "warn", true
	}
}

// nodeCapacityQuantity reads a node capacity quantity (cpu/memory/pods) off the row
// object's status.capacity and returns it as a resource.Quantity plus whether it
// was present and parseable. Capacity values are canonical k8s quantities
// ("4", "16Gi", "8047476Ki", "110"); resource.Quantity is readout's existing
// quantity handling (the same type the metrics seam parses), so the magnitude math
// is never hand-rolled.
func nodeCapacityQuantity(obj map[string]any, key string) (resource.Quantity, bool) {
	raw := nestedString(obj, "status", "capacity", key)
	if raw == "" {
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, false
	}
	return q, true
}

// capacityCellView resolves a node CPU/Memory capacity cell. `key` is the capacity
// map key ("cpu" or "memory"); `usage` is the joined metrics usage (CPU cores /
// memory bytes) and `haveUsage` reports whether a real usage value exists (metrics
// joined). With a real usage % it sets the bucket + fill width + a "usage/capacity"
// label; without metrics it falls back to the bare capacity value text and leaves
// the bar empty/uncoloured (CapBucket "", CapPct 0).
func capacityCellView(obj map[string]any, key string, usage float64, haveUsage bool) cellView {
	cap, haveCap := nodeCapacityQuantity(obj, key)
	capText := ""
	if haveCap {
		capText = capacityText(key, cap)
	}
	cv := cellView{Kind: cellCapacity}
	if !haveUsage || !haveCap {
		// No-metrics (or missing capacity) default: capacity value text only, no
		// coloured bar.
		cv.Value = capText
		return cv
	}
	capFloat := cap.AsApproximateFloat64()
	if capFloat <= 0 {
		cv.Value = capText
		return cv
	}
	pct := usage / capFloat * 100
	if pct < 0 {
		pct = 0
	}
	clamped := pct
	if clamped > 100 {
		clamped = 100
	}
	cv.CapBucket = capacityBucket(pct)
	cv.CapPct = int(clamped + 0.5)
	cv.CapBar = true
	cv.Value = capacityUsageLabel(key, usage, cap)
	return cv
}

// capacityUsageLabel formats the "usage/capacity" text shown next to a node
// capacity bar (e.g. "2.5/4" for CPU cores, "6/8Gi" for memory). CPU usage is in
// cores; memory usage is in bytes and is rendered as a binary-suffixed quantity so
// it reads in the same unit family as the capacity.
func capacityUsageLabel(key string, usage float64, cap resource.Quantity) string {
	switch key {
	case "cpu":
		return trimFloat(usage) + "/" + cap.String()
	case "memory":
		return humanBytes(int64(usage)) + "/" + humanMemory(cap)
	default:
		return trimFloat(usage) + "/" + cap.String()
	}
}

// capacityText renders a node capacity quantity for display: CPU keeps the plain
// quantity string ("4"); memory is humanised ("8138032Ki" -> "7.8 GiB") because
// resource.Quantity.String() preserves the raw status.capacity binary suffix,
// which is fine for cores but unreadable for memory.
func capacityText(key string, q resource.Quantity) string {
	if key == "memory" {
		return humanMemory(q)
	}
	return q.String()
}

// humanMemory renders a memory quantity in a readable binary unit (the bytes
// value of the quantity passed through humanBytes).
func humanMemory(q resource.Quantity) string {
	return humanBytes(q.Value())
}

// humanBytes formats a byte count with a binary unit suffix (KiB/MiB/GiB/TiB/...),
// one decimal place with a trailing ".0" trimmed, so node memory reads "7.8 GiB"
// instead of the raw "8138032Ki".
func humanBytes(b int64) string {
	if b < 1024 {
		return strconv.FormatInt(b, 10) + " B"
	}
	f := float64(b)
	const units = "KMGTPE"
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	s := strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
	return s + " " + string(units[i-1]) + "iB"
}

// trimFloat renders a CPU-core float with up to one decimal place and no trailing
// zero (1 -> "1", 2.5 -> "2.5", 0.25 -> "0.2"), keeping the capacity label compact.
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s
}

// nodeCapacityKey maps a node capacity COLUMN name onto its status.capacity map
// key: the CPU columns ("CPU", "CPU Usage") read status.capacity.cpu; the memory
// columns read status.capacity.memory.
func nodeCapacityKey(colName string) string {
	if strings.HasPrefix(colName, "Memory") {
		return "memory"
	}
	return "cpu"
}

// replicaTrackCap is the fixed maximum number of `<i>` segments a deployment
// replica track renders, regardless of desired replica count. A deployment with
// hundreds of replicas must not emit hundreds of DOM elements per row, so the
// track caps here and the `.rep-num` ratio text carries the truth beyond the cap.
// With desired <= cap each desired replica gets its own segment; above the cap the
// segment counts are scaled proportionally so the bar still reflects the
// ready/updating/pending split.
const replicaTrackCap = 12

// deploymentReplicas reads a deployment's desired/ready/updated replica counts off
// the row object (spec.replicas + status.{readyReplicas,updatedReplicas}). The
// object is decoded once into a typed appsv1.Deployment so the counts come off the
// typed spec/status, never a hand-rolled map walk. desired defaults to 1 when
// spec.replicas is absent (the kube default); ready/updated default to 0.
func deploymentReplicas(obj map[string]any) (desired, ready, updated int) {
	var dep appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &dep); err != nil {
		return 0, 0, 0
	}
	desired = 1
	if dep.Spec.Replicas != nil {
		desired = int(*dep.Spec.Replicas)
	}
	return desired, int(dep.Status.ReadyReplicas), int(dep.Status.UpdatedReplicas)
}

// replicaTrack builds the deployment replica-track segments + the `ready/desired`
// ratio text for the cellReplicas view. Each of the `ready` filled segments is "",
// the segments in [ready, updated) pulse amber ("updating"), and [updated, desired)
// are hollow ("pending") -- mirroring the design repCell, generalised from its
// single-updating-slot form to the full updated-beyond-ready window. The rendered
// segment count is capped at replicaTrackCap: with desired <= cap one segment per
// desired replica; above the cap the three buckets are scaled proportionally so the
// bar keeps the same ready/updating/pending proportions without a DOM explosion. The
// `repNum` ratio text is ALWAYS the real `ready/desired` (the truth beyond the cap).
func replicaTrack(desired, ready, updated int) (segments []repSegment, repNum string) {
	if desired < 0 {
		desired = 0
	}
	ready = clampInt(ready, 0, desired)
	updated = clampInt(updated, 0, desired)
	if updated < ready {
		updated = ready
	}
	repNum = strconv.Itoa(ready) + "/" + strconv.Itoa(desired)

	filled, updatingSlots, total := ready, updated-ready, desired
	if desired > replicaTrackCap {
		// Scale the three buckets into the cap, preserving their proportions. Filled
		// rounds to nearest; updating takes its proportional share next; pending fills
		// the remainder so the rendered total is exactly the cap.
		total = replicaTrackCap
		filled = scaleToCap(ready, desired)
		updatingSlots = scaleToCap(updated, desired) - filled
		if updatingSlots < 0 {
			updatingSlots = 0
		}
		if filled+updatingSlots > total {
			updatingSlots = total - filled
		}
	}
	for k := 0; k < total; k++ {
		switch {
		case k < filled:
			segments = append(segments, repSegment{State: ""})
		case k < filled+updatingSlots:
			segments = append(segments, repSegment{State: "updating"})
		default:
			segments = append(segments, repSegment{State: "pending"})
		}
	}
	return segments, repNum
}

// scaleToCap maps a count out of `desired` onto the [0, replicaTrackCap] range,
// rounding to the nearest segment. Used only on the over-cap path so the segment
// buckets stay proportional to the real replica counts.
func scaleToCap(count, desired int) int {
	if desired <= 0 {
		return 0
	}
	scaled := int(float64(count)/float64(desired)*float64(replicaTrackCap) + 0.5)
	return clampInt(scaled, 0, replicaTrackCap)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// rolloutState derives a deployment's rollout state (done/prog/paused) + its label
// from the row object's spec/status. A paused deployment (spec.paused) is "paused";
// a deployment whose Progressing condition reports the new ReplicaSet is available
// AND whose ready/updated/available all meet the desired count is "done" (up to
// date); anything mid-flight is "prog" (rolling out). The object is decoded once
// into a typed appsv1.Deployment so the condition reasons + replica counts come off
// the typed status, not a map walk.
func rolloutState(obj map[string]any) (state, label string) {
	var dep appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &dep); err != nil {
		return "prog", "rolling out"
	}
	if dep.Spec.Paused {
		return "paused", "paused"
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	st := dep.Status
	complete := st.UpdatedReplicas == desired &&
		st.ReadyReplicas == desired &&
		st.AvailableReplicas == desired &&
		st.Replicas == desired
	if complete && progressingComplete(&dep) {
		return "done", "up to date"
	}
	return "prog", "rolling out"
}

// progressingComplete reports whether the deployment's Progressing condition (when
// present) signals a finished rollout (reason "NewReplicaSetAvailable"). When the
// deployment carries NO Progressing condition the replica-count check in
// rolloutState already decided completeness, so the absence is treated as
// non-blocking (true).
func progressingComplete(dep *appsv1.Deployment) bool {
	for _, cond := range dep.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing {
			return cond.Reason == "NewReplicaSetAvailable"
		}
	}
	return true
}

// rolloutIconName maps a rollout state onto its icon glyph name (done -> check
// mark, paused -> sliders, rolling -> rotate-cw), matching the design rolloutCell.
func rolloutIconName(state string) string {
	switch state {
	case "done":
		return "check"
	case "paused":
		return "sliders"
	default:
		return "rotate-cw"
	}
}

func humanTitle(value string) string {
	words := strings.Fields(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(value))
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// capitalizeWord upper-cases the first byte and lower-cases the rest, so a
// camelCase section key like "eventTime" renders as the title "Eventtime".
func capitalizeWord(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + strings.ToLower(value[1:])
}

func pluralizeKind(kind string) string {
	if strings.HasSuffix(kind, "s") {
		return kind + "es"
	}
	if strings.HasSuffix(kind, "y") {
		return strings.TrimSuffix(kind, "y") + "ies"
	}
	return kind + "s"
}

func sortIcon(sortValue, column string) string {
	switch sortValue {
	case column:
		// Ascending: the down chevron rotated 180° (.sort-asc) so the arrow points
		// UP -- there is no separate chevron-up glyph.
		return ` <span class="icon sort-ico sort-asc">` + icon("chevron-down") + `</span>`
	case column + ":desc":
		// Descending: the plain down chevron.
		return ` <span class="icon sort-ico">` + icon("chevron-down") + `</span>`
	default:
		return ""
	}
}

func createdSortParam(current string) string {
	if current == "Created" {
		return "Created:desc"
	}
	return "Created"
}

func pluralS(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func namespaceEmptyText(namespace string, allNamespaces bool) string {
	if namespace != "" && !allNamespaces {
		return `in namespace "` + html.EscapeString(namespace) + `" `
	}
	return ""
}

// namespaceLabelChips builds the namespace label-chip view models from the row
// object's metadata.labels (sorted by key for a stable order). Every chip is
// NEUTRAL (D3 colour law: the green app.kubernetes.io/* accent is retired; the
// key/value pair differs by ink weight in the renderer). A namespace with no
// labels yields nil, so the renderer shows the muted "—".
func namespaceLabelChips(obj map[string]any) []chipView {
	labels, _, _ := unstructured.NestedStringMap(obj, "metadata", "labels")
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	chips := make([]chipView, 0, len(keys))
	for _, key := range keys {
		chips = append(chips, chipView{Key: key, Val: labels[key]})
	}
	return chips
}

// namespaceLabelsText is the plain DISPLAY value for the synthetic namespace
// Labels column: the sorted comma-joined "key=value" labels, or "—" when the
// namespace has no labels (so the generic fallback / TSV / sort sees the same "—"
// the rich chips renderer shows for an unlabeled namespace).
func namespaceLabelsText(obj map[string]any) string {
	labels, _, _ := unstructured.NestedStringMap(obj, "metadata", "labels")
	if len(labels) == 0 {
		return "—"
	}
	return formatLabels(labels)
}

func cellClass(table *kube.Table, idx int, cell any) string {
	if idx < 0 || idx >= len(table.Columns) {
		return ""
	}
	return kube.CellClass(table.Resource.Plural, table.Columns[idx].Name, cell)
}

func cpuFormat(cell any) string {
	f, ok := numericCell(cell)
	if !ok {
		return fmt.Sprint(cell)
	}
	return fmt.Sprintf("%.0fm", f*1000)
}

func memoryMiBFormat(cell any) string {
	f, ok := numericCell(cell)
	if !ok {
		return fmt.Sprint(cell)
	}
	return fmt.Sprintf("%.0f", f/(1024*1024))
}

func numericCell(cell any) (float64, bool) {
	switch v := cell.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		return f, err == nil
	}
}

func formatTimestamp(value string) string {
	return strings.TrimSuffix(strings.ReplaceAll(value, "T", " "), "Z")
}

func (s *Server) ageClass(value string) string {
	if value == "" {
		return "age-old"
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "age-old"
	}
	seconds := s.clock().Sub(t).Seconds() - 60
	if seconds < 0 {
		seconds = 0
	}
	fraction := seconds / (24 * time.Hour).Seconds()
	switch {
	case fraction < 0.10:
		return "age-fresh"
	case fraction < 0.35:
		return "age-recent"
	case fraction < 0.65:
		return "age-day"
	case fraction < 1.0:
		return "age-week"
	default:
		return "age-old"
	}
}

func assetHashes(fsys fs.FS) map[string]string {
	result := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		result[name] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return result
}

func theme(r *http.Request, cfg *config.Config) string {
	if cookie, err := r.Cookie("theme"); err == nil && allowedTheme(cookie.Value, cfg) {
		return cookie.Value
	}
	if allowedTheme(cfg.DefaultTheme, cfg) {
		return cfg.DefaultTheme
	}
	options := themeOptions(cfg)
	if len(options) > 0 {
		return options[0]
	}
	return "dark"
}

// apiVersionParam reads the resource-type pin from the request, accepting BOTH
// the camelCase `apiVersion` spelling AND the snake_case `api_version` spelling.
// camelCase wins when both are present, so a request that only sets `apiVersion`
// keeps its behavior; the `api_version` spelling is the kubectl-style fallback.
func apiVersionParam(r *http.Request) string {
	if v := r.URL.Query().Get("apiVersion"); v != "" {
		return v
	}
	return r.URL.Query().Get("api_version")
}

func themeExplicit(r *http.Request) bool {
	if _, err := r.Cookie("theme"); err == nil {
		return true
	}
	return r.URL.Query().Get("theme") != ""
}

func activeClass(active bool) string {
	if active {
		return " is-active"
	}
	return ""
}

func activeAttr(active bool) string {
	if active {
		return ` class="is-active"`
	}
	return ""
}

// truncate shortens value to at most max characters (plus a "..." ellipsis),
// preferring to break on the last space. It counts and slices in CODEPOINT
// (rune) space, so an annotation value with multi-byte runes is never cut
// mid-rune (a byte slice could emit invalid UTF-8). The leeway / word-boundary /
// ellipsis behaviour is unchanged.
func truncate(value string, max int) string {
	const leeway = 5
	runes := []rune(value)
	if len(runes) <= max+leeway {
		return value
	}
	if max <= 3 {
		return string(runes[:max])
	}
	prefix := string(runes[:max-3])
	if idx := strings.LastIndex(prefix, " "); idx >= 0 {
		prefix = prefix[:idx]
	}
	return prefix + "..."
}

func splitOwnerTitle(title string) (string, string) {
	kind, name, ok := strings.Cut(title, "/")
	if !ok {
		return "", ""
	}
	return kind, name
}

func allowedTheme(value string, cfg *config.Config) bool {
	if value == "" {
		return false
	}
	for _, option := range themeOptions(cfg) {
		if value == option {
			return true
		}
	}
	return false
}

func themeOptions(cfg *config.Config) []string {
	if len(cfg.ThemeOptions) > 0 {
		return cfg.ThemeOptions
	}
	return []string{"dark", "light"}
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstSlice[T any](value, fallback []T) []T {
	if len(value) > 0 {
		return value
	}
	return fallback
}

func addQuery(u *url.URL, key, value string) string {
	clone := *u
	q := clone.Query()
	q.Set(key, value)
	clone.RawQuery = queryEncodeKeepParens(q)
	return clone.String()
}

func downloadTSVHref(u *url.URL, plural string) string {
	clone := *resourceListBaseURL(u)
	q := clone.Query()
	q.Set("download", "tsv")
	q.Set("download_table", plural)
	clone.RawQuery = queryEncodeKeepParens(q)
	return clone.String()
}

// delQuery returns u with the named query params removed (a read-only GET),
// used by the empty-filtered state: the per-chip ✕ drops one filter param and
// "Clear filters" drops the whole set. Removing params never changes the verb
// (still a GET to the same list path), so it stays inside the read-only floor.
func delQuery(u *url.URL, keys ...string) string {
	clone := *u
	q := clone.Query()
	for _, key := range keys {
		q.Del(key)
	}
	clone.RawQuery = queryEncodeKeepParens(q)
	return clone.String()
}

// queryEncodeKeepParens URL-encodes the query values but leaves parentheses
// literal, so selector links like `?selector=app(in)(a,b)` stay readable in the
// address bar instead of showing %28/%29.
func queryEncodeKeepParens(values url.Values) string {
	return strings.NewReplacer("%28", "(", "%29", ")").Replace(values.Encode())
}

func resourceListBaseURL(u *url.URL) *url.URL {
	clone := *u
	path := strings.TrimSuffix(strings.TrimRight(clone.Path, "/"), "/_table")
	clone.Path = path
	return &clone
}

func partialResourceListURL(r *http.Request) string {
	clone := *r.URL
	clone.Path = strings.TrimRight(clone.Path, "/") + "/_table"
	return clone.String()
}

func nameColumn(table *kube.Table) int {
	for i, col := range table.Columns {
		if col.Name == "Name" {
			return i
		}
	}
	return 0
}

func cellString(row kube.Row, idx int) string {
	if idx < 0 || idx >= len(row.Cells) {
		return ""
	}
	return cellDisplayString(row.Cells[idx])
}

func cellDisplayString(cell any) string {
	if cell == nil {
		return ""
	}
	return fmt.Sprint(cell)
}

func resourceHref(cluster string, rt *kube.ResourceType, namespace, name string) string {
	if rt.Namespaced {
		return fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(rt.Plural), url.PathEscape(name))
	}
	return fmt.Sprintf("/clusters/%s/%s/%s", url.PathEscape(cluster), url.PathEscape(rt.Plural), url.PathEscape(name))
}

func objectDownloadYAMLHref(cluster, namespace string, object *kube.Object) string {
	if namespace != "" {
		return fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s?download=yaml", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(object.Resource.Endpoint()), url.PathEscape(object.Name()))
	}
	return fmt.Sprintf("/clusters/%s/%s/%s?download=yaml", url.PathEscape(cluster), url.PathEscape(object.Resource.Endpoint()), url.PathEscape(object.Name()))
}

// nestedString reads a string at the given path from a generic browsed-resource
// object map via the apimachinery accessor (empty when absent or non-string). A
// thin wrapper so the row-object reads stay one-liners; the typed accessor does
// the navigation.
func nestedString(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func maskSecret(obj map[string]any) {
	if data, ok := obj["data"].(map[string]any); ok {
		for key := range data {
			data[key] = kube.SecretContentHidden
		}
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["annotations"] = map[string]any{"annotations-hidden": "by-readout"}
}

// containerNames returns a pod's container names (regular then init), for the
// logs tab's container picker. Pod is a fixed kind, so the object is decoded
// once into a corev1.Pod and the names are read off the typed PodSpec.
func containerNames(obj map[string]any) []string {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &pod); err != nil {
		return nil
	}
	var names []string
	for i := range pod.Spec.Containers {
		if name := pod.Spec.Containers[i].Name; name != "" {
			names = append(names, name)
		}
	}
	for i := range pod.Spec.InitContainers {
		if name := pod.Spec.InitContainers[i].Name; name != "" {
			names = append(names, name)
		}
	}
	return names
}
