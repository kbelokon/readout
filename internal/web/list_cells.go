package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
)

// list_cells.go builds the resolved cellView for one body cell: buildCellView
// dispatches each recognized column to its rich per-kind presentation, and the
// cell-cookbook constructors below assemble the corner-case cell types (pending
// addresses, ports, hosts, TLS locks, data-key chips, event counts/objects/ages/
// messages) over plain data. The kube.Table cell keeps its raw value for
// sort/filter/TSV; everything here is display-only.

// buildCellView resolves one body cell: its render branch, value, classes, and
// any request-derived href. The rich per-kind presentation (pod-name split,
// status-dot tone + transient pulse, ready/restart tones, secondary-text
// truncation tooltip) is resolved here too so the templ renderer reads plain data
// and emits the redesign vocabulary directly. Recognized columns of the existing
// k8s Table schema are ADAPTED in place -- a user-added label/custom column falls
// through to the generic (plain/truncated) cell, so hidecols/labelcols/customcols/
// sort/TSV are untouched.
func (s *Server) buildCellView(r *http.Request, table *kube.Table, row kube.Row, i int, cell any, ns, name string) cellView {
	value := cellDisplayString(cell)
	if i >= len(table.Columns) {
		return cellView{Kind: cellPlain, Value: value}
	}
	colName := table.Columns[i].Name
	cls := cellClass(table, i, cell)
	if colName == "Age" || colName == "First Seen" {
		cls = strings.TrimSpace(cls + " " + s.ageClass(nestedString(row.Object, "metadata", "creationTimestamp")))
	}
	cv := cellView{Value: value, Class: cls, ColClass: table.Columns[i].Class}
	switch {
	case colName == "Name":
		cv.Kind = cellName
		cv.NameHead, cv.NameTail = splitObjectName(table.Resource.Plural, value)
		// Middle truncation: a head longer than 42 chars displays as
		// `first26…last12` with the FULL name in the tooltip. The hash tail is
		// never touched, so a cron pod's job/pod suffix stays unique on screen.
		if display, truncated := MiddleTruncate(cv.NameHead, nameHeadMax, nameHeadLead, nameHeadTrail); truncated {
			cv.NameHead = display
			cv.Title = value
		}
		href := resourceHref(row.Cluster, &table.Resource, ns, name)
		if table.Resource.Plural == "namespaces" {
			href = fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(row.Cluster), url.PathEscape(name))
		}
		cv.Href = href
	case table.Columns[i].Label != "" && table.Columns[i].Label != "*":
		// A user-added single label column: still a selector link, but the label
		// VALUE is secondary free-text, so it truncates with a tooltip.
		cv.Kind = cellLabel
		cv.Href = addQuery(r.URL, "selector", table.Columns[i].Label+"="+value)
		cv.Trunc, cv.Title = true, value
	case colName == "Node":
		cv.Kind = cellNode
		cv.Href = "/clusters/" + url.PathEscape(row.Cluster) + "/nodes/" + url.PathEscape(value)
	case table.Resource.Plural == "nodes" && (colName == "CPU Usage" || colName == "Memory Usage"):
		// Nodes reskin the joined metrics usage column as a capacity bar: the cell
		// carries the usage (cores/bytes from applyMetricsUsage), the node's
		// capacity comes from status.capacity, and the bucket + fill come from
		// usage/capacity.
		usage, haveUsage := numericCell(cell)
		cv = capacityCellView(row.Object, nodeCapacityKey(colName), usage, haveUsage)
	case table.Resource.Plural == "nodes" && (colName == "CPU" || colName == "Memory"):
		// No-metrics node capacity column: capacity value text, no usage overlay.
		cv = capacityCellView(row.Object, nodeCapacityKey(colName), 0, false)
	case table.Resource.Plural == "nodes" && colName == "Roles":
		cv.Kind = cellRoles
		cv.Roles = nodeRoles(row.Object)
	case table.Resource.Plural == "nodes" && colName == "Conditions":
		cv.Kind = cellConditions
		cv.Conds = nodeAbnormalConditions(row.Object)
	case table.Resource.Plural == "deployments" && colName == "Ready":
		// Deployments reskin the Ready column as the replica track: the segment
		// states + the ready/desired ratio come from the deployment status/spec
		// (readyReplicas / updatedReplicas / spec.replicas), capped at
		// replicaTrackCap so a high-replica deployment never explodes the DOM.
		cv.Kind = cellReplicas
		desired, ready, updated := deploymentReplicas(row.Object)
		cv.RepSegments, cv.RepNum = replicaTrack(desired, ready, updated)
		cv.Ratio = readyRatioClass(cv.RepNum)
	case table.Resource.Plural == "deployments" && colName == "Rollout":
		// The synthetic Rollout column (added by decorateDeploymentColumns) renders
		// the rollout status pill; the state + label come from the deployment
		// status/conditions/spec.paused.
		cv.Kind = cellRollout
		cv.RolloutState, cv.Value = rolloutState(row.Object)
	case table.Resource.Plural == "namespaces" && colName == "Labels" && table.Columns[i].Label == "":
		// The synthetic Labels column (added by decorateNamespaceColumns) renders the
		// namespace label chips read from metadata.labels (the .app accent for
		// app.kubernetes.io/*). The Label=="" guard keeps a user-added labelcols
		// "Labels" column (which carries a Label tag) on the generic path instead.
		cv.Kind = cellChips
		cv.Chips = namespaceLabelChips(row.Object)
		// Label-chip click-to-filter: on a single-type page each
		// chip links to THIS list with the `label:key=value` chip appended to its
		// `?f=` set (the same gate the filter engine applies under -- a multi-type
		// page ignores `f`, so its chips stay inert spans).
		if isSingleListType(r.PathValue("plural")) {
			for ci := range cv.Chips {
				cv.Chips[ci].Href = addFilterChipHref(r.URL, "label:"+cv.Chips[ci].Key+"="+cv.Chips[ci].Val)
			}
		}
	case (table.Resource.Plural == "services" && colName == "External-IP") ||
		(table.Resource.Plural == "ingresses" && colName == "Address"):
		// Pending cell: the printer's `<none>` (or an empty address) is
		// the faint none; the literal `<pending>` of an unprovisioned LB/ingress is
		// the amber pulsing in-flight state; an ExternalName target / provisioned
		// address renders verbatim.
		cv = pendingCellView(value)
	case table.Resource.Plural == "services" && colName == "Port(s)":
		// Ports cell over the printer's comma-joined list: first 2 +
		// faint "+N", the full list in the tooltip; portless (`<none>`) -> "—".
		cv = portsCellView(commaListValues(value))
	case table.Resource.Plural == "ingresses" && colName == "Hosts":
		// Hosts cell: the first host + faint "+N hosts" with the full
		// newline-joined list in the tooltip.
		cv = hostsCellView(commaListValues(value))
	case table.Resource.Plural == "services" && colName == "Selector" && table.Columns[i].Label == "":
		// The services Selector column renders neutral chips read from
		// spec.selector. Deliberately NO click-to-filter href (see
		// selectorChips); the Label=="" guard keeps a user-added labelcols
		// "Selector" column on the label path.
		cv.Kind = cellChips
		cv.Chips = selectorChips(row.Object)
	case table.Resource.Plural == "ingresses" && colName == "TLS":
		// The synthetic TLS column (added by decorateIngressColumns) renders the
		// earned-green lock only when spec.tls terminates, else "—".
		cv = tlsCellView(ingressTLSTerminated(row.Object))
	case table.Resource.Plural == "configmaps" && colName == "Data":
		// The configmap Data column renders `name · size` key chips decoded from
		// the row object's data/binaryData; the server's count cell
		// stays in the kube.Table for sort/TSV/filter.
		cv = keysCellView(configMapKeyChips(row.Object))
	case table.Resource.Plural == "secrets" && colName == "Data":
		// The secret Data column renders key chips with DECODED byte sizes; the
		// VALUE bytes never reach the view model (secretKeyChips).
		cv = keysCellView(secretKeyChips(row.Object))
	case table.Resource.Plural == "cronjobs" && colName == "Suspend":
		// The cronjob Suspend cell renders the prototype's status vocabulary:
		// the printer's boolean maps false→Active (ok, live health) /
		// true→Suspended (mute) with the tone owned by kube.StatusTone
		// via CellClass — display-only; the kube.Table cell keeps the printer
		// boolean for sort/TSV/filter. Neither word is transient, so no pulse.
		label := "Active"
		if strings.EqualFold(value, "true") {
			label = "Suspended"
		}
		cv.Kind = cellStatus
		cv.Value = label
		cv.Tone = statusTone(kube.CellClass(table.Resource.Plural, "Status", label))
	case table.Resource.Plural == "cronjobs" && colName == "Last Schedule":
		// Lastrun cell: the printer's compressed duration gains the
		// age-bucket colour + " ago"; a cronjob that never ran prints the
		// literal <none> on the wire — that IS the empty case → faint <never>.
		if value == "<none>" {
			value = ""
		}
		cv = lastRunCellView(value)
	case table.Resource.Plural == "jobs" && colName == "Completions" && strings.Contains(value, "/"):
		// Completions share the ready-ratio grammar (full green when
		// n==m, partial amber, zero faint).
		cv.Kind = cellReady
		cv.Ratio = readyRatioClass(value)
	case table.Resource.Plural == "events" && colName == "Type":
		// The events Type cell is a status cell whose vocabulary IS the status table
		// (Normal→mute, Warning→warn — never an invented stronger severity);
		// CellClass("events","Type",…) delegates to kube.StatusTone. Neither
		// word is transient, so no pulse.
		cv.Kind = cellStatus
		cv.Tone = statusTone(cls)
	case table.Resource.Plural == "events" && colName == "Object":
		// Events Object cell: kind icon + faint "Kind/" + the 20…8 middle-truncated
		// name, decoded from involvedObject (core/v1) or regarding
		// (events.k8s.io/v1). An undecodable ref keeps the printer's plain
		// "kind/name" cell.
		if item, ok := decodeEventItem(row.Object); ok && item.refName() != "" {
			cv = evObjCellView(item.refKind(), item.refName())
		} else {
			cv.Kind = cellPlain
		}
	case table.Resource.Plural == "events" && colName == "Count":
		// The ×N cell over the dual-API count decode (≥20 amber, 1
		// faint). Re-decoded from the row object so a server-provided Count
		// column shows the same pinned-precedence truth as the decorated one.
		n := 1
		if item, ok := decodeEventItem(row.Object); ok {
			n = int(item.eventCount())
		}
		cv = countCellView(n)
	case table.Resource.Plural == "events" && colName == "Last Seen":
		// Events Age cell: the two-layer age built from the timestamp decode
		// (last-seen lead token bucket-coloured; "(first <dur> ago)" faint when
		// count > 1 and the spread exceeds 60s). When no timestamp decodes the
		// printer's own Last Seen duration stays as the single layer.
		text := value
		if item, ok := decodeEventItem(row.Object); ok {
			if t := eventAgeText(item, s.clock()); t != "" {
				text = t
			}
		}
		cv = evAgeCellView(text)
	case table.Resource.Plural == "events" && colName == "Message":
		// Message cell: THE only wrapping column in the system (the 520px
		// clamp lives in CSS on td.ro-event-msg).
		cv = msgCellView(value)
	case colName == "CPU Usage":
		cv.Kind = cellCPU
		cv.Value = cpuFormat(cell)
	case colName == "Memory Usage":
		cv.Kind = cellMemory
		cv.Value = memoryMiBFormat(cell)
	case colName == "Status":
		cv.Kind = cellStatus
		// cls is kube.CellClass's encoding of kube.StatusTone (the single
		// value->tone owner), so the dot tone always exists (fallback mute).
		cv.Tone = statusTone(cls)
		// Pulse the transient set for ANY kind's status cell (only in-flight states animate) -- the set
		// itself gates (steady and err states never pulse), so a Terminating
		// namespace pulses exactly like a Terminating pod.
		cv.Pulse = transientStatus(value)
	case colName == "Ready" && strings.Contains(value, "/"):
		cv.Kind = cellReady
		cv.Ratio = readyRatioClass(value)
	case colName == "Restarts":
		cv.Kind = cellRestarts
		cv.Value, cv.Ago = splitRestarts(value)
		cv.Tone = restartsTone(cv.Value)
		// The restart count gets a thousands separator (1047 ->
		// 1,047). Applied after the tone (which keys on the raw "0") and safe
		// for any non-numeric cell (groupThousands passes those through).
		cv.Value = groupThousands(cv.Value)
	default:
		cv.Kind = cellPlain
		if isSecondaryTextColumn(colName) {
			cv.Trunc, cv.Title = true, value
		}
	}
	if colName == "Age" || colName == "First Seen" {
		// The age cell carries the short bucketed value; the full timestamp moves
		// into the tooltip (no redundant full-timestamp column).
		if ts := formatTimestamp(nestedString(row.Object, "metadata", "creationTimestamp")); ts != "" {
			cv.Title = "created " + ts
		}
	}
	return cv
}

// ---------------------------------------------------------------------------
// Cell-cookbook cell constructors. Each builds the resolved
// cellView for one corner-case cell type over plain data; the kind-specific
// schema decorators that read row objects and CALL these land with the
// services/ingress/configmap/secret/cronjob/job columns and the
// events columns. Display-only: the kube.Table cell keeps its raw
// value for sort/filter/TSV.
// ---------------------------------------------------------------------------

// portsCellMax / hostsCellMax / keysCellMax / chipsCellMax are the
// in-cell overflow thresholds: 2 ports, 1 host, 3 data keys, 2 label/selector
// chips shown before the faint +N (ports/hosts) or the +N expand button
// (keys/chips).
const (
	portsCellMax = 2
	hostsCellMax = 1
	keysCellMax  = 3
	chipsCellMax = 2
)

// pendingCellView resolves a service External-IP / ingress Address cell:
// empty -- including the printer's literal `<none>`, which IS the
// empty case on the wire -- -> the faint `<none>`, the literal `<pending>` ->
// an amber PULSING dot + the word "pending" (an in-flight state, the motion law),
// anything else -> the plain address.
func pendingCellView(value string) cellView {
	cv := cellView{Kind: cellPending, Value: value}
	switch value {
	case "", "<none>":
		cv.Value = ""
	case "<pending>":
		cv.Value = "pending"
		cv.Tone = "warn"
		cv.Pulse = true
	}
	return cv
}

// portsCellView resolves a service Ports cell: the first 2 ports
// joined ", ", a faint "+N" for the rest, and the FULL comma-joined list in the
// tooltip. No ports -> the muted "—" (empty Value).
func portsCellView(ports []string) cellView {
	cv := cellView{Kind: cellPorts}
	if len(ports) == 0 {
		return cv
	}
	shown := ports
	if len(ports) > portsCellMax {
		shown = ports[:portsCellMax]
		cv.More = "+" + strconv.Itoa(len(ports)-portsCellMax)
	}
	cv.Value = strings.Join(shown, ", ")
	cv.Title = strings.Join(ports, ", ")
	return cv
}

// hostsCellView resolves an ingress Hosts cell: the first host +
// a faint "+N hosts", with the full newline-joined list in the tooltip. No
// hosts -> the muted "—" (empty Value).
func hostsCellView(hosts []string) cellView {
	cv := cellView{Kind: cellHosts}
	if len(hosts) == 0 {
		return cv
	}
	cv.Value = hosts[0]
	if len(hosts) > hostsCellMax {
		cv.More = "+" + strconv.Itoa(len(hosts)-hostsCellMax) + " hosts"
		cv.Title = strings.Join(hosts, "\n")
	}
	return cv
}

// tlsCellView resolves an ingress TLS cell: the green lock +
// "tls" ONLY when TLS is terminated (an EARNED green under the colour law: live protection),
// else the muted "—".
func tlsCellView(terminated bool) cellView {
	cv := cellView{Kind: cellTLS}
	if terminated {
		cv.Value = "tls"
		cv.Tone = "ok"
	}
	return cv
}

// lastRunCellView resolves a cronjob Last Schedule cell: the
// age-bucket colour (the value is already a kubectl compressed duration) +
// a " ago" suffix; a cronjob that never ran -> the faint `<never>` (empty
// Value).
func lastRunCellView(value string) cellView {
	cv := cellView{Kind: cellLastRun}
	if value == "" {
		return cv
	}
	cv.Value = value + " ago"
	cv.Class = durationAgeClass(value)
	return cv
}

// keysCellView resolves a configmap/secret Data cell: one
// `name · size` chip per key, the first keysCellMax shown, the rest behind the
// `+N keys` in-cell expand. The keyChipView carries ONLY the key name + byte
// size -- secret values are structurally absent from the view model. Empty
// data -> the muted "—".
func keysCellView(keys []keyChipView) cellView {
	return cellView{Kind: cellKeys, Keys: keys}
}

// countCellView resolves an events Count cell: `×N` with a
// thousands separator; ≥20 reads chronic (the amber .restarts.some ink), a
// 0/1 count fades. The class strings are final span classes lifted from the
// reference countCell.
func countCellView(n int) cellView {
	cv := cellView{Kind: cellCount, Value: groupThousands(strconv.Itoa(n))}
	switch {
	case n >= 20:
		cv.Class = "restarts some"
	case n > 1:
		cv.Class = ""
	default:
		cv.Class = "faint"
	}
	return cv
}

// evObjCellView resolves an events Object cell: the kind icon
// (pre-rendered in the bridge) + the faint "Kind/" prefix + the 20…8
// middle-truncated object name, full name in the tooltip when truncated
// (the truncation rule beats the reference DOM, which dropped the
// tooltip).
func evObjCellView(kind, name string) cellView {
	cv := cellView{Kind: cellEvObj, Value: name, EvKind: kind, EvName: name}
	if display, truncated := MiddleTruncate(name, evObjNameMax, evObjLead, evObjTrail); truncated {
		cv.EvName = display
		cv.Title = name
	}
	return cv
}

// evAgeCellView resolves an events Age cell: the leading age
// token carries the age-bucket colour; any remainder ("(first 41h ago)")
// renders as the faint 11px second layer.
func evAgeCellView(value string) cellView {
	cv := cellView{Kind: cellEvAge}
	first, rest, _ := strings.Cut(strings.TrimSpace(value), " ")
	cv.Value = first
	cv.EvAgeRest = strings.TrimSpace(rest)
	if first != "" {
		cv.Class = durationAgeClass(first)
	}
	return cv
}

// msgCellView resolves an events Message cell: the ONLY wrapping
// column in the system (td.ro-event-msg, max-width 520px in CSS). The value is
// plain text; templ escapes it at render.
func msgCellView(value string) cellView {
	return cellView{Kind: cellMsg, Value: value}
}

// secondaryTextColumns are the recognized k8s Table columns whose value is long
// free-text rather than an identifier -- they truncate with a `title=` tooltip
// (e.g. images, labels, selectors, node selectors, messages). The design keeps an ALLOW-LIST
// here, not the inverse, because identifiers are sacred: an unlisted column stays
// FULL and the table wrapper scrolls horizontally under the pinned name column
// (the design's escape valve), which is always safe -- whereas truncating by
// default would clip a short identifier/enum (Type, Cluster-IP, Port(s)) that must
// stay readable. Identifier columns (Name, Node, IP, Namespace, Ports, container
// names, counts) are deliberately never listed here.
var secondaryTextColumns = map[string]bool{
	"Image":         true,
	"Images":        true,
	"Selector":      true,
	"Node Selector": true,
	"Labels":        true,
	"Message":       true,
	"Reason":        true,
	"Data":          true,
	"Provider":      true,
	"Resources":     true,
}

func isSecondaryTextColumn(colName string) bool {
	return secondaryTextColumns[colName]
}

// buildToolsView resolves the resource-list tools form state from the request.
func (s *Server) buildToolsView(r *http.Request, table *kube.Table) toolsView {
	q := r.URL.Query()
	active := q.Get("labelcols") != "" || q.Get("selector") != "" || q.Get("filter") != ""
	if !active {
		active = q.Get("label-columns") != "" || q.Get("custom-columns") != "" || q.Get("hide-columns") != ""
	}
	tv := toolsView{
		Active:       active,
		LabelColsVal: first(q.Get("labelcols"), q.Get("label-columns"), s.cfg.DefaultLabelColumns[table.Resource.Plural]),
		SelectorVal:  q.Get("selector"),
		FilterVal:    q.Get("filter"),
	}
	for _, key := range []string{"join", "sort", "customcols", "custom-columns", "hidecols", "hide-columns", "apiVersion", "api_version", "limit", "label-columns"} {
		if value := q.Get(key); value != "" {
			tv.HiddenInputs = append(tv.HiddenInputs, hiddenInput{Name: key, Value: value})
		}
	}
	return tv
}
