package fakekube

// tables.go is the Table-form generator: it fills the per-list Table slot
// (listState.table) the seeder leaves empty for the List form, so a client that
// negotiates `as=Table` gets the same meta.k8s.io Table shape readout's list
// renderer consumes. The 44 embedded-JSON fixtures carried hand-written Table
// columnDefinitions for only ~12 kinds; this registry PORTS those column shapes
// verbatim (the kube printer columns the fixtures already proved against
// readout's curated cells) and AUTHORS the printer columns for the kinds the
// demo adds with no Table fixture — StatefulSet, DaemonSet, ReplicaSet,
// HorizontalPodAutoscaler — plus the default Name/Age table every CRD falls
// back to (no additionalPrinterColumns in the scenario model, matching the
// apiserver's own default for a CRD without printer columns; readout's CRD
// list-render path consumes whatever columns the Table returns, so the
// Name/Age default renders correctly).
//
// The full object rides each row (readout re-reads it for the curated cells —
// deployment replica track, cronjob suspend, secret data chips, …), and the
// Table item set mirrors the List item set one-to-one. The cell VALUES are the
// printer's plain encoding where it is cheap to reconstruct and a stable
// placeholder otherwise; the curated renderers re-read the row object, so the
// column NAMES are the contract this unit owns — not the cell text.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tableColumn is one Table columnDefinition: the displayed name plus the wire
// type/format the meta.k8s.io schema carries (readout's decodeTable reads
// name/type/format). format "name" marks the identity column.
type tableColumn struct {
	name   string
	typ    string
	format string
}

// cellFunc extracts one column's plain printer cell value from a wire object.
type cellFunc func(obj map[string]any) any

// kindTable is the column set + per-column cell extractors for one kind.
type kindTable struct {
	columns []tableColumn
	cells   []cellFunc
}

// tableForKind returns the column set + cell extractors for a kind, falling back
// to the default Name/Age table (the apiserver's own default for a CRD without
// additionalPrinterColumns). ok is false only to signal the fallback was used —
// callers serve a Table either way, so every list gets a Table form.
func tableForKind(kind string) (kindTable, bool) {
	if kt, ok := kindTables[kind]; ok {
		return kt, true
	}
	return defaultKindTable, false
}

// col builds a tableColumn; the empty type defaults to "string" (the printer's
// common case), matching the ported fixtures.
func col(name, typ, format string) tableColumn {
	if typ == "" {
		typ = "string"
	}
	return tableColumn{name: name, typ: typ, format: format}
}

// nameCol is the identity column shared by every kind (format "name").
func nameCol() tableColumn { return col("Name", "string", "name") }

// ageCol is the Age column shared by every kind.
func ageCol() tableColumn { return col("Age", "string", "") }

// defaultKindTable is the Name/Age fallback served for any kind with no
// registered columns — every CRD (the scenario model carries no
// additionalPrinterColumns), matching the apiserver's default CRD table.
var defaultKindTable = kindTable{
	columns: []tableColumn{nameCol(), ageCol()},
	cells:   []cellFunc{cellName, cellAge},
}

// kindTables maps kind -> its Table column set + cell extractors. PORTED kinds
// (Pod, Deployment, Service, Node, Event, ConfigMap, Secret, Ingress, CronJob,
// Job, PersistentVolume, Namespace) carry the exact column shapes lifted from
// the embedded-JSON Table fixtures. AUTHORED kinds (StatefulSet, DaemonSet,
// ReplicaSet, HorizontalPodAutoscaler) carry the kube printer columns.
var kindTables = map[string]kindTable{
	// ---- PORTED (from internal/fakekube/fixtures/data/*_table.json) ----
	"Pod": {
		columns: []tableColumn{nameCol(), col("Ready", "", ""), col("Status", "", ""), col("Restarts", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellPodReady,
			cellPodStatus,
			cellPodRestarts,
			cellAge,
		},
	},
	"Deployment": {
		columns: []tableColumn{nameCol(), col("Ready", "", ""), col("Up-to-date", "", ""), col("Available", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellWorkloadReady("readyReplicas", "replicas"),
			cellStatusInt("updatedReplicas"),
			cellStatusInt("availableReplicas"),
			cellAge,
		},
	},
	"Service": {
		columns: []tableColumn{nameCol(), col("Type", "", ""), col("Cluster-IP", "", ""), col("Port(s)", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellSpec("type"),
			cellServiceClusterIP,
			cellServicePorts,
			cellAge,
		},
	},
	"Node": {
		columns: []tableColumn{
			nameCol(), col("Status", "", ""), col("Roles", "", ""), ageCol(),
			col("Version", "", ""), col("Internal-IP", "", ""), col("External-IP", "", ""),
			col("OS-Image", "", ""), col("Kernel-Version", "", ""), col("Container-Runtime", "", ""),
		},
		cells: []cellFunc{
			cellName,
			cellNodeStatus,
			cellNodeRoles,
			cellAge,
			cellNodeInfo("kubeletVersion"),
			cellNodeAddress("InternalIP"),
			cellNodeAddress("ExternalIP"),
			cellNodeInfo("osImage"),
			cellNodeInfo("kernelVersion"),
			cellNodeInfo("containerRuntimeVersion"),
		},
	},
	"Event": {
		columns: []tableColumn{col("Last Seen", "", ""), col("Type", "", ""), col("Reason", "", ""), col("Object", "", ""), col("Message", "", "")},
		cells: []cellFunc{
			cellEventLastSeen,
			cellOneOf("type"),
			cellOneOf("reason"),
			cellEventObject,
			cellEventMessage,
		},
	},
	"ConfigMap": {
		columns: []tableColumn{nameCol(), col("Data", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellMapCount("data"),
			cellAge,
		},
	},
	"Secret": {
		columns: []tableColumn{nameCol(), col("Type", "", ""), col("Data", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellTopLevel("type"),
			cellMapCount("data"),
			cellAge,
		},
	},
	"Ingress": {
		columns: []tableColumn{nameCol(), col("Class", "", ""), col("Hosts", "", ""), col("Address", "", ""), col("Ports", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellIngressClass,
			cellIngressHosts,
			cellIngressAddress,
			cellIngressPorts,
			cellAge,
		},
	},
	"CronJob": {
		columns: []tableColumn{nameCol(), col("Schedule", "", ""), col("Suspend", "boolean", ""), col("Active", "integer", ""), col("Last Schedule", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellSpec("schedule"),
			cellCronSuspend,
			cellSliceLen("status", "active"),
			cellCronLastSchedule,
			cellAge,
		},
	},
	"Job": {
		columns: []tableColumn{nameCol(), col("Status", "", ""), col("Completions", "", ""), col("Duration", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellJobStatus,
			cellJobCompletions,
			cellPlaceholder("<unknown>"),
			cellAge,
		},
	},
	"PersistentVolume": {
		columns: []tableColumn{
			nameCol(), col("Capacity", "", ""), col("Access Modes", "", ""), col("Reclaim Policy", "", ""),
			col("Status", "", ""), col("Claim", "", ""), col("StorageClass", "", ""), col("Reason", "", ""), ageCol(),
		},
		cells: []cellFunc{
			cellName,
			cellPVCapacity,
			cellPVAccessModes,
			cellSpec("persistentVolumeReclaimPolicy"),
			cellPVStatus,
			cellPVClaim,
			cellSpec("storageClassName"),
			cellPVReason,
			cellAge,
		},
	},
	"Namespace": {
		columns: []tableColumn{nameCol(), col("Status", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellNamespaceStatus,
			cellAge,
		},
	},

	// ---- AUTHORED (kube printer columns; no Table fixture existed) ----
	"StatefulSet": {
		columns: []tableColumn{nameCol(), col("Ready", "", ""), ageCol()},
		cells: []cellFunc{
			cellName,
			cellWorkloadReady("readyReplicas", "replicas"),
			cellAge,
		},
	},
	"DaemonSet": {
		columns: []tableColumn{
			nameCol(), col("Desired", "integer", ""), col("Current", "integer", ""), col("Ready", "integer", ""),
			col("Up-to-date", "integer", ""), col("Available", "integer", ""), col("Node Selector", "", ""), ageCol(),
		},
		cells: []cellFunc{
			cellName,
			cellStatusInt("desiredNumberScheduled"),
			cellStatusInt("currentNumberScheduled"),
			cellStatusInt("numberReady"),
			cellStatusInt("updatedNumberScheduled"),
			cellStatusInt("numberAvailable"),
			cellDaemonNodeSelector,
			cellAge,
		},
	},
	"ReplicaSet": {
		columns: []tableColumn{
			nameCol(), col("Desired", "integer", ""), col("Current", "integer", ""), col("Ready", "integer", ""), ageCol(),
		},
		cells: []cellFunc{
			cellName,
			cellSpecInt("replicas"),
			cellStatusInt("replicas"),
			cellStatusInt("readyReplicas"),
			cellAge,
		},
	},
	"HorizontalPodAutoscaler": {
		columns: []tableColumn{
			nameCol(), col("Reference", "", ""), col("Targets", "", ""), col("MinPods", "integer", ""),
			col("MaxPods", "integer", ""), col("Replicas", "integer", ""), ageCol(),
		},
		cells: []cellFunc{
			cellName,
			cellHPAReference,
			cellPlaceholder("<unknown>/<unknown>"),
			cellSpecInt("minReplicas"),
			cellSpecInt("maxReplicas"),
			cellStatusInt("currentReplicas"),
			cellAge,
		},
	},
}

// buildTableDoc renders one collection's Table wire document from its bucket:
// the meta.k8s.io/v1 Table envelope, the kind's columnDefinitions, and one row
// per item (its plain cells + the FULL object) in INSERTION order (the seeder
// controls it, so the order is deterministic AND mirrors the List form). A row
// whose object carries EXPLICIT cells (the base test cluster's literal printer
// cells) serves those verbatim; otherwise the kind's tableForKind extractors
// derive them (the demo path).
func buildTableDoc(b *listBucket) map[string]any {
	kt, _ := tableForKind(b.info.kind)

	columns := make([]any, 0, len(kt.columns))
	for _, c := range kt.columns {
		columns = append(columns, map[string]any{
			"name":   c.name,
			"type":   c.typ,
			"format": c.format,
		})
	}

	rows := make([]any, 0, len(b.items))
	for i, obj := range b.items {
		if i < len(b.listOnly) && b.listOnly[i] {
			continue // List-only item: omitted from the Table wire form.
		}
		var explicit []any
		if i < len(b.cells) {
			explicit = b.cells[i]
		}
		var cells []any
		if explicit != nil {
			cells = explicit
		} else {
			cells = make([]any, 0, len(kt.cells))
			for _, fn := range kt.cells {
				cells = append(cells, fn(obj))
			}
		}
		rows = append(rows, map[string]any{
			"cells":  cells,
			"object": obj, // the FULL object rides the row (curated cells re-read it)
		})
	}

	return map[string]any{
		"kind":              "Table",
		"apiVersion":        "meta.k8s.io/v1",
		"metadata":          map[string]any{"resourceVersion": "100000"},
		"columnDefinitions": columns,
		"rows":              rows,
	}
}

// ---------------------------------------------------------------------------
// Cell extractors. Each reads the wire object (a decoded JSON map) and returns
// the column's plain printer value. The curated renderers re-read the object,
// so these stay simple — a sensible plain value the generic fallback / sort /
// TSV can use, never the rich presentation.
// ---------------------------------------------------------------------------

func cellName(obj map[string]any) any { return nestedString(obj, "metadata", "name") }

// cellAgeReference is the fixed "now" the derived Age cell measures
// creationTimestamp against, so a generated Age is deterministic (no wall clock)
// and stable across runs/snapshots. It matches the demo scenario's reference
// instant; the base test cluster never reaches this derivation (its rows carry
// EXPLICIT age cells via withCells), so the constant does not skew base ages.
var cellAgeReference = mustTime("2026-06-15T12:00:00Z")

// mustTime parses an RFC3339 literal, panicking on a malformed constant (a code
// bug caught by the first test run).
func mustTime(rfc3339 string) time.Time {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic("fakeapi tables: bad reference timestamp " + rfc3339 + ": " + err.Error())
	}
	return t
}

// cellAge renders the printer's compressed-duration Age from the object's
// creationTimestamp measured against the fixed cellAgeReference instant — the
// kube printer's HumanDuration shape (years/days/hours/minutes/seconds, the two
// most-significant units). The full timestamp also rides the object, which
// readout's rich age cell re-reads; this value backs the generic/sort/TSV path
// and the demo's visible Age column. An absent or unparseable timestamp renders
// "<unknown>".
func cellAge(obj map[string]any) any {
	created := nestedString(obj, "metadata", "creationTimestamp")
	if created == "" {
		return "<unknown>"
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return "<unknown>"
	}
	return humanAge(cellAgeReference.Sub(t))
}

// humanAge renders a duration as the kube printer's compressed Age token (e.g.
// "12d", "3h", "45m", "5s", "1y127d"), showing the two most-significant nonzero
// units the way kubectl's HumanDuration does. A non-positive duration renders
// "0s".
func humanAge(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	seconds := int64(d.Seconds())
	const (
		minute = 60
		hour   = 60 * minute
		day    = 24 * hour
		year   = 365 * day
	)
	switch {
	case seconds < minute:
		return fmt.Sprintf("%ds", seconds)
	case seconds < hour:
		return fmt.Sprintf("%dm", seconds/minute)
	case seconds < day:
		h := seconds / hour
		m := (seconds % hour) / minute
		if h < 8 && m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	case seconds < year:
		dys := seconds / day
		h := (seconds % day) / hour
		if dys < 8 && h > 0 {
			return fmt.Sprintf("%dd%dh", dys, h)
		}
		return fmt.Sprintf("%dd", dys)
	default:
		yrs := seconds / year
		dys := (seconds % year) / day
		return fmt.Sprintf("%dy%dd", yrs, dys)
	}
}

// nestedString reads a dotted-path string from a decoded JSON map, "" when
// absent or not a string. A local mirror of the web layer's helper.
func nestedString(obj map[string]any, path ...string) string {
	cur := any(obj)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}

// nestedMap reads a dotted-path object, nil when absent.
func nestedMap(obj map[string]any, path ...string) map[string]any {
	cur := any(obj)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	if m, ok := cur.(map[string]any); ok {
		return m
	}
	return nil
}

// nestedSlice reads a dotted-path array, nil when absent.
func nestedSlice(obj map[string]any, path ...string) []any {
	cur := any(obj)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	if s, ok := cur.([]any); ok {
		return s
	}
	return nil
}

// numString renders a JSON number (decoded as float64) or numeric string as a
// plain integer string; "0" when absent.
func numString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case int:
		return strconv.Itoa(n)
	case string:
		if n == "" {
			return "0"
		}
		return n
	default:
		return "0"
	}
}

// cellTopLevel reads a top-level object field as a string.
func cellTopLevel(key string) cellFunc {
	return func(obj map[string]any) any { return nestedString(obj, key) }
}

// cellSpec reads spec.<key> as a string.
func cellSpec(key string) cellFunc {
	return func(obj map[string]any) any { return nestedString(obj, "spec", key) }
}

// cellOneOf reads the first present of the given top-level string keys.
func cellOneOf(keys ...string) cellFunc {
	return func(obj map[string]any) any {
		for _, k := range keys {
			if v := nestedString(obj, k); v != "" {
				return v
			}
		}
		return ""
	}
}

// cellStatusInt renders status.<key> as an integer string.
func cellStatusInt(key string) cellFunc {
	return func(obj map[string]any) any {
		m := nestedMap(obj, "status")
		if m == nil {
			return int64(0)
		}
		return asInt64(m[key])
	}
}

// cellSpecInt renders spec.<key> as an integer.
func cellSpecInt(key string) cellFunc {
	return func(obj map[string]any) any {
		m := nestedMap(obj, "spec")
		if m == nil {
			return int64(0)
		}
		return asInt64(m[key])
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

// cellPlaceholder always returns a fixed string (for columns whose value the
// scenario model does not carry, e.g. Job Duration / HPA Targets).
func cellPlaceholder(s string) cellFunc {
	return func(map[string]any) any { return s }
}

// cellMapCount renders the key count of a top-level string map (configmap /
// secret Data column the curated chips re-read).
func cellMapCount(key string) cellFunc {
	return func(obj map[string]any) any {
		return int64(len(nestedMap(obj, key)))
	}
}

// cellSliceLen renders the length of a dotted-path slice as an integer.
func cellSliceLen(path ...string) cellFunc {
	return func(obj map[string]any) any {
		return int64(len(nestedSlice(obj, path...)))
	}
}

// cellWorkloadReady renders the "<ready>/<desired>" ratio readout's replica
// track / ready cell consumes (status.<readyKey>/spec.<desiredKey>).
func cellWorkloadReady(readyKey, desiredKey string) cellFunc {
	return func(obj map[string]any) any {
		ready := asInt64(valueAt(obj, "status", readyKey))
		desired := asInt64(valueAt(obj, "spec", desiredKey))
		return fmt.Sprintf("%d/%d", ready, desired)
	}
}

func valueAt(obj map[string]any, parent, key string) any {
	m := nestedMap(obj, parent)
	if m == nil {
		return nil
	}
	return m[key]
}

// ---- Pod ----

func cellPodStatus(obj map[string]any) any {
	if phase := nestedString(obj, "status", "phase"); phase != "" {
		return phase
	}
	return "Pending"
}

func cellPodReady(obj map[string]any) any {
	statuses := nestedSlice(obj, "status", "containerStatuses")
	total := len(nestedSlice(obj, "spec", "containers"))
	ready := 0
	for _, s := range statuses {
		if cs, ok := s.(map[string]any); ok {
			if r, ok := cs["ready"].(bool); ok && r {
				ready++
			}
		}
	}
	if total == 0 {
		total = len(statuses)
	}
	return fmt.Sprintf("%d/%d", ready, total)
}

func cellPodRestarts(obj map[string]any) any {
	var restarts int64
	for _, s := range nestedSlice(obj, "status", "containerStatuses") {
		if cs, ok := s.(map[string]any); ok {
			restarts += asInt64(cs["restartCount"])
		}
	}
	return strconv.FormatInt(restarts, 10)
}

// ---- Service ----

func cellServiceClusterIP(obj map[string]any) any {
	if ip := nestedString(obj, "spec", "clusterIP"); ip != "" {
		return ip
	}
	return "<none>"
}

func cellServicePorts(obj map[string]any) any {
	ports := nestedSlice(obj, "spec", "ports")
	if len(ports) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		proto := nestedString(pm, "protocol")
		if proto == "" {
			proto = "TCP"
		}
		port := numString(pm["port"])
		if np := numString(pm["nodePort"]); np != "0" && np != "" {
			parts = append(parts, fmt.Sprintf("%s:%s/%s", port, np, proto))
		} else {
			parts = append(parts, fmt.Sprintf("%s/%s", port, proto))
		}
	}
	if len(parts) == 0 {
		return "<none>"
	}
	return strings.Join(parts, ",")
}

// ---- Node ----

func cellNodeStatus(obj map[string]any) any {
	for _, c := range nestedSlice(obj, "status", "conditions") {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if nestedString(cm, "type") == "Ready" {
			if nestedString(cm, "status") == "True" {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

func cellNodeRoles(obj map[string]any) any {
	labels := nestedMap(obj, "metadata", "labels")
	var roles []string
	for k := range labels {
		const prefix = "node-role.kubernetes.io/"
		if strings.HasPrefix(k, prefix) {
			if role := strings.TrimPrefix(k, prefix); role != "" {
				roles = append(roles, role)
			}
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func cellNodeInfo(key string) cellFunc {
	return func(obj map[string]any) any { return nestedString(obj, "status", "nodeInfo", key) }
}

func cellNodeAddress(addrType string) cellFunc {
	return func(obj map[string]any) any {
		for _, a := range nestedSlice(obj, "status", "addresses") {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if nestedString(am, "type") == addrType {
				return nestedString(am, "address")
			}
		}
		return "<none>"
	}
}

// ---- Event ----

func cellEventLastSeen(obj map[string]any) any {
	if v := nestedString(obj, "lastTimestamp"); v != "" {
		return "5m"
	}
	if v := nestedString(obj, "eventTime"); v != "" {
		return "5m"
	}
	return "<unknown>"
}

func cellEventObject(obj map[string]any) any {
	ref := nestedMap(obj, "involvedObject")
	if ref == nil {
		ref = nestedMap(obj, "regarding")
	}
	if ref == nil {
		return ""
	}
	kind := strings.ToLower(nestedString(ref, "kind"))
	name := nestedString(ref, "name")
	if kind == "" || name == "" {
		return name
	}
	return kind + "/" + name
}

func cellEventMessage(obj map[string]any) any {
	if m := nestedString(obj, "message"); m != "" {
		return m
	}
	return nestedString(obj, "note")
}

// ---- Ingress ----

func cellIngressClass(obj map[string]any) any {
	if c := nestedString(obj, "spec", "ingressClassName"); c != "" {
		return c
	}
	return "<none>"
}

func cellIngressHosts(obj map[string]any) any {
	var hosts []string
	for _, r := range nestedSlice(obj, "spec", "rules") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if h := nestedString(rm, "host"); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return "*"
	}
	return strings.Join(hosts, ",")
}

func cellIngressAddress(obj map[string]any) any {
	for _, i := range nestedSlice(obj, "status", "loadBalancer", "ingress") {
		im, ok := i.(map[string]any)
		if !ok {
			continue
		}
		if a := firstNonEmpty(nestedString(im, "ip"), nestedString(im, "hostname")); a != "" {
			return a
		}
	}
	return ""
}

func cellIngressPorts(obj map[string]any) any {
	if len(nestedSlice(obj, "spec", "tls")) > 0 {
		return "80, 443"
	}
	return "80"
}

// ---- CronJob ----

func cellCronSuspend(obj map[string]any) any {
	if s, ok := valueAt(obj, "spec", "suspend").(bool); ok {
		return s
	}
	return false
}

func cellCronLastSchedule(obj map[string]any) any {
	if t := nestedString(obj, "status", "lastScheduleTime"); t != "" {
		return "5m"
	}
	return "<none>"
}

// ---- Job ----

func cellJobStatus(obj map[string]any) any {
	for _, c := range nestedSlice(obj, "status", "conditions") {
		cm, ok := c.(map[string]any)
		if !ok || nestedString(cm, "status") != "True" {
			continue
		}
		typ := nestedString(cm, "type")
		if typ == "Failed" {
			if reason := nestedString(cm, "reason"); reason != "" {
				return reason
			}
		}
		if typ != "" {
			return typ
		}
	}
	return "Running"
}

func cellJobCompletions(obj map[string]any) any {
	succeeded := asInt64(valueAt(obj, "status", "succeeded"))
	completions := asInt64(valueAt(obj, "spec", "completions"))
	if completions == 0 {
		// Parallel job without a fixed completion count.
		return fmt.Sprintf("%d/1", succeeded)
	}
	return fmt.Sprintf("%d/%d", succeeded, completions)
}

// ---- PersistentVolume ----

func cellPVCapacity(obj map[string]any) any {
	if c := nestedString(obj, "spec", "capacity", "storage"); c != "" {
		return c
	}
	return ""
}

func cellPVAccessModes(obj map[string]any) any {
	short := map[string]string{
		"ReadWriteOnce":    "RWO",
		"ReadOnlyMany":     "ROX",
		"ReadWriteMany":    "RWX",
		"ReadWriteOncePod": "RWOP",
	}
	var modes []string
	for _, m := range nestedSlice(obj, "spec", "accessModes") {
		if s, ok := m.(string); ok {
			if sh, ok := short[s]; ok {
				modes = append(modes, sh)
			} else {
				modes = append(modes, s)
			}
		}
	}
	return strings.Join(modes, ",")
}

func cellPVStatus(obj map[string]any) any {
	if p := nestedString(obj, "status", "phase"); p != "" {
		return p
	}
	return "Available"
}

func cellPVClaim(obj map[string]any) any {
	ns := nestedString(obj, "spec", "claimRef", "namespace")
	name := nestedString(obj, "spec", "claimRef", "name")
	if name == "" {
		return ""
	}
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func cellPVReason(obj map[string]any) any {
	return nestedString(obj, "status", "reason")
}

// ---- Namespace ----

func cellNamespaceStatus(obj map[string]any) any {
	if p := nestedString(obj, "status", "phase"); p != "" {
		return p
	}
	return "Active"
}

// ---- DaemonSet / HPA ----

func cellDaemonNodeSelector(obj map[string]any) any {
	sel := nestedMap(obj, "spec", "template", "spec", "nodeSelector")
	if len(sel) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fmt.Sprint(sel[k]))
	}
	return strings.Join(parts, ",")
}

func cellHPAReference(obj map[string]any) any {
	ref := nestedMap(obj, "spec", "scaleTargetRef")
	if ref == nil {
		return "<unknown>"
	}
	kind := nestedString(ref, "kind")
	name := nestedString(ref, "name")
	if kind == "" || name == "" {
		return name
	}
	return kind + "/" + name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
