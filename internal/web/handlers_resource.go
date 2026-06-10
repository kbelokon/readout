package web

import (
	"encoding/csv"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
	"github.com/kbelokon/readout/internal/yamlview"
)

// logLine is one rendered log entry (a timestamp-grouped block): the message
// text and the source pod/container. It feeds logPreHTML, which derives the
// level class from the text at render time.
type logLine struct{ Text, Pod, Container string }

// hasLogTimestamp reports whether a container-log line begins with an RFC3339
// timestamp (the prefix added by the Timestamps:true fetch). Lines are split on
// the first space and the leading token is parsed as a time; a line that does
// not start with a parseable timestamp is treated as a continuation of the
// previous entry by the grouping loop.
func hasLogTimestamp(line string) bool {
	ts, _, ok := strings.Cut(line, " ")
	if !ok {
		return false
	}
	if _, err := time.Parse(time.RFC3339, ts); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339Nano, ts)
	return err == nil
}

func (s *Server) resourceList(w http.ResponseWriter, r *http.Request) {
	// Bulk YAML download (D11) branches BEFORE the table fan-out: its name
	// bounds are validated first, so a rejected request (101+ names) never
	// pays for a cluster round-trip.
	if r.URL.Query().Get("download") == "yaml" {
		s.downloadBulkYAML(w, r)
		return
	}
	ctx, err := s.listContext(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	if r.URL.Query().Get("download") == "tsv" && len(ctx.Tables) > 0 {
		s.downloadTSV(w, r, selectDownloadTable(ctx.Tables, r.URL.Query().Get("download_table")))
		return
	}
	view := s.buildListView(r, &ctx)
	partialURL := partialResourceListURL(r)
	s.pageComponentWithClients(w, r, view.Title(), ctx.Clients, templates.ResourceList(toListPageData(&view, partialURL)))
}

func selectDownloadTable(tables []kube.Table, plural string) *kube.Table {
	if plural != "" {
		for i := range tables {
			if tables[i].Resource.Plural == plural {
				return &tables[i]
			}
		}
	}
	return &tables[0]
}

func (s *Server) resourceListPartial(w http.ResponseWriter, r *http.Request) {
	ctx, err := s.listContext(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	view := s.buildListView(r, &ctx)
	// AUTO-REFRESH stale invariant (D11): the full page renders a whole-list
	// forbidden/unreachable state card (a first load has no rows to keep), but this
	// `_table` partial is the ro:refresh target morphed in place. Returning a
	// 200-with-state-card here would swap the last-good rows OUT for the card and
	// then htmx:afterSwap would clear the stale dim -- defeating the stale path. So
	// when buildListView produced a whole-list failure state, surface its source
	// error as a NON-2xx instead: htmx does not swap on error (the existing rows
	// stay), and htmx:responseError fires the client-side stale banner + dim. A
	// partial EMPTY / empty-filtered (zero rows, no error -> no State) still renders
	// normally; only the unreachable/forbidden whole-list error goes non-2xx.
	if view.State != nil {
		s.error(w, r, view.State.SourceErr)
		return
	}
	// Canonical-URL history push (D6): a USER-initiated sort/filter request gets
	// the CANONICAL list URL (path minus `/_table`, current query) pushed into
	// history -- never the partial URL (hx-push-url="true" would push
	// `…/_table?…`; a reload of that entry renders a bare fragment). The header
	// is deliberately conditional: htmx pushes one history entry per HX-Push-Url
	// occurrence with NO same-URL dedupe, so an unconditional header would turn a
	// 5s refresh interval into one junk entry per tick. Ticks and every other
	// programmatic re-fetch mark themselves with RO-No-Push (readout.js sets it
	// on requests issued by #resource-list-content itself; column toggles and
	// later programmatic surfaces ride the same header), preload warm-ups carry
	// HX-Preloaded, non-htmx requests have no HX-Request, and the loop is
	// single-type-only (D1) -- none of those push.
	if isSingleListType(r.PathValue("plural")) &&
		r.Header.Get("HX-Request") == "true" &&
		r.Header.Get("RO-No-Push") == "" &&
		r.Header.Get("HX-Preloaded") != "true" {
		w.Header().Set("HX-Push-Url", resourceListBaseURL(r.URL).String())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.ResourceTable(toListData(&view)).Render(r.Context(), w)
}

func (s *Server) downloadTSV(w http.ResponseWriter, r *http.Request, table *kube.Table) {
	filename := strings.Trim(strings.ReplaceAll(r.URL.Path, "/", "_"), "_")
	w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.tsv"`)
	writer := csv.NewWriter(w)
	writer.Comma = '\t'
	var cols []string
	if len(table.Clusters) > 1 {
		cols = append(cols, "Cluster")
	}
	if table.Resource.Namespaced {
		cols = append(cols, "Namespace")
	}
	for _, col := range table.Columns {
		cols = append(cols, col.Name)
	}
	_ = writer.Write(cols)
	for _, row := range table.Rows {
		var rec []string
		if len(table.Clusters) > 1 {
			rec = append(rec, row.Cluster)
		}
		if table.Resource.Namespaced {
			rec = append(rec, nestedString(row.Object, "metadata", "namespace"))
		}
		for _, cell := range row.Cells {
			rec = append(rec, cellDisplayString(cell))
		}
		_ = writer.Write(rec)
	}
	writer.Flush()
}

// bulkNamesMax is the double-sided bulk-download bound (D11): the client
// disables the bulk button above this many selected objects, and the server
// rejects a larger `names` list with 400 -- so a hand-built GET URL stays
// bounded exactly like a button-built one.
const bulkNamesMax = 100

// parseBulkNames splits every `names` query occurrence on commas and drops
// empty segments. Kubernetes object names and namespaces are DNS-shaped (no
// commas), so the comma join is unambiguous for both the bare-name and the
// ns/name grammar.
func parseBulkNames(query url.Values) []string {
	var names []string
	for _, raw := range query["names"] {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// downloadBulkYAML serves the list-level `?download=yaml&names=…` bulk export
// (D11): ONE multi-document YAML, `---`-separated, one document per requested
// name in request order. It extends the existing list download surface -- the
// objects come from the same Table fan-out the page render uses (full objects
// ride the rows via includeObject=Object), so the config namespace allow/deny
// filtering applies identically and no per-name GET fan-out is introduced.
//
// Names grammar: bare `name` on single-namespace (and cluster-scoped) lists,
// `ns/name` on _all-namespaces lists. A name absent from the table renders a
// `# not found: <name>` comment document -- never a whole-download failure.
// Bounds: >bulkNamesMax names, an empty names list, a multi-type plural, and
// multi-cluster scope (the bulk button is single-cluster only) all reject
// with 400.
func (s *Server) downloadBulkYAML(w http.ResponseWriter, r *http.Request) {
	if !isSingleListType(r.PathValue("plural")) {
		s.error(w, r, statusError{status: http.StatusBadRequest, message: "bulk YAML download needs a single resource type"})
		return
	}
	names := parseBulkNames(r.URL.Query())
	if len(names) == 0 {
		s.error(w, r, statusError{status: http.StatusBadRequest, message: "bulk YAML download needs a names parameter"})
		return
	}
	if len(names) > bulkNamesMax {
		s.error(w, r, statusError{status: http.StatusBadRequest, message: fmt.Sprintf("bulk YAML download is limited to %d names, got %d", bulkNamesMax, len(names))})
		return
	}
	ctx, err := s.listContext(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	if ctx.IsAllClusters || ctx.ClusterCount > 1 {
		s.error(w, r, statusError{status: http.StatusBadRequest, message: "bulk YAML download works on single-cluster lists only"})
		return
	}
	// A whole-list failure (unreachable / forbidden cluster) surfaces as the
	// fetch error: rendering every name as not-found would misreport objects
	// that merely could not be listed.
	if len(ctx.Tables) == 0 && len(ctx.Errors) > 0 {
		s.error(w, r, ctx.Errors[0])
		return
	}

	// Index the rows by the grammar key: ns/name on _all-namespaces lists
	// (a cluster-scoped row there keeps its bare name -- it has no namespace
	// segment), bare name everywhere else.
	objects := map[string]map[string]any{}
	for ti := range ctx.Tables {
		for _, row := range ctx.Tables[ti].Rows {
			key := nestedString(row.Object, "metadata", "name")
			if ns := nestedString(row.Object, "metadata", "namespace"); ctx.IsAllNamespaces && ns != "" {
				key = ns + "/" + key
			}
			objects[key] = row.Object
		}
	}

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("---\n")
		}
		obj, ok := objects[name]
		if !ok {
			// The echoed name is collapsed to one line so a crafted %0A can
			// never break out of the YAML comment.
			b.WriteString("# not found: " + strings.Join(strings.Fields(name), " ") + "\n")
			continue
		}
		data, _ := yamlview.Marshal(obj)
		b.Write(data)
	}

	filename := make([]string, 0, 3)
	for _, part := range []string{ctx.Cluster, ctx.Namespace, ctx.Plural} {
		if part != "" {
			filename = append(filename, part)
		}
	}
	w.Header().Set("Content-Type", "text/vnd.yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.Join(filename, "_")+`_bulk.yaml"`)
	_, _ = io.WriteString(w, b.String())
}

func (s *Server) resourceView(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	client := s.kubeClient(r, cluster)
	view, ok := s.buildDetailView(w, r, client, cluster)
	if !ok {
		return
	}
	s.pageComponentWithNamespaceAndClients(w, r, view.Title, &view.Namespace, requestKubeClients{cluster.Name: client}, templates.ResourceView(toDetailData(view)))
}

func (s *Server) resourceLogs(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	client := s.kubeClient(r, cluster)
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	name := r.PathValue("name")
	if namespace != "" && !s.namespaceAllowed(namespace) {
		s.error(w, r, statusError{status: http.StatusForbidden, message: "namespace is not allowed"})
		return
	}
	rt, err := client.FindResource(r.Context(), plural, true, "")
	if err != nil {
		s.error(w, r, err)
		return
	}
	obj, err := client.Get(r.Context(), &rt, namespace, name)
	if err != nil {
		s.error(w, r, err)
		return
	}
	object := kube.NewObject(&rt, obj)
	pods := []kube.Object{object}
	if object.Kind() != "Pod" {
		pods = s.podsForSelector(r, client, &object, namespace)
	}
	if len(pods) == 0 {
		s.error(w, r, fmt.Errorf("resource has no logs"))
		return
	}
	tail := int64(200)
	if raw := r.URL.Query().Get("tail_lines"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			tail = n
		}
	}
	if tail < 1 {
		tail = 1
	} else if tail > 100000 {
		tail = 100000
	}
	filterText := r.URL.Query().Get("filter")
	selectedContainer := r.URL.Query().Get("container")
	var lines []logLine
	containerSet := map[string]bool{"": true}
	for pi := range pods {
		pod := &pods[pi]
		names := containerNames(pod.Raw)
		for _, name := range names {
			containerSet[name] = true
		}
		for _, container := range names {
			if selectedContainer != "" && selectedContainer != container {
				continue
			}
			if !s.cfg.ShowContainerLogs {
				continue
			}
			logs, err := client.Logs(r.Context(), kube.LogOptions{Namespace: first(pod.Namespace(), namespace), Pod: pod.Name(), Container: container, Timestamps: true, TailLines: tail})
			if err != nil {
				continue
			}
			var containerLines []logLine
			for _, text := range strings.Split(logs, "\n") {
				if filterText != "" && !strings.Contains(text, filterText) {
					continue
				}
				// Logs are fetched with Timestamps:true, so a fresh entry begins
				// at every line whose first space-delimited token parses as an
				// RFC3339 timestamp; a line without a parseable timestamp prefix
				// is a wrapped continuation of the previous entry (e.g. a stack
				// trace) and is folded into it. This replaces the old "starts
				// with 20" year heuristic, which broke outside years 20xx.
				if hasLogTimestamp(text) || len(containerLines) == 0 {
					containerLines = append(containerLines, logLine{Text: text, Pod: pod.Name(), Container: container})
				} else {
					prev := &containerLines[len(containerLines)-1]
					prev.Text += "\n" + text
				}
			}
			lines = append(lines, containerLines...)
		}
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Text != lines[j].Text {
			return lines[i].Text < lines[j].Text
		}
		if lines[i].Pod != lines[j].Pod {
			return lines[i].Pod < lines[j].Pod
		}
		return lines[i].Container < lines[j].Container
	})
	// Download-logs (D25): a plain GET over the SAME assembled view -- the
	// container/tail/filter params shape `lines` exactly like the on-screen
	// stream. Branches before the page render; gated on showContainerLogs so a
	// disabled deployment never serves log bytes through the download spelling.
	if s.cfg.ShowContainerLogs && r.URL.Query().Get("download") == "txt" {
		s.downloadLogs(w, r, lines)
		return
	}
	allContainers := make([]string, 0, len(containerSet))
	for name := range containerSet {
		allContainers = append(allContainers, name)
	}
	sort.Strings(allContainers)
	pageTitle := object.Name() + " (" + object.Kind()
	if namespace != "" {
		pageTitle += " in " + namespace
	}
	pageTitle += ")"

	base := fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s", url.PathEscape(cluster.Name), url.PathEscape(namespace), url.PathEscape(object.Resource.Plural), url.PathEscape(object.Name()))
	data := templates.LogsData{
		Breadcrumb:        objectBreadcrumb(cluster.Name, namespace, &object),
		Name:              object.Name(),
		Kind:              object.Kind(),
		DefaultHref:       base,
		YAMLHref:          base + "?view=yaml",
		EventsHref:        base + "?view=events",
		ShowContainerLogs: s.cfg.ShowContainerLogs,
		TailLines:         tail,
		PodCount:          len(pods),
		FilterVal:         filterText,
		ContainerVal:      selectedContainer,
	}
	// The logs H1 carries the same pn-head/pn-tail split as the detail title
	// (Unit 13 flagged the plain-name gap here).
	data.NameHead, data.NameTail, data.NameTitle = detailNameParts(&object)
	if s.cfg.ShowContainerLogs {
		// The Download-logs title action mirrors the on-screen view: same
		// container/tail/filter params, download=txt spelling.
		dq := url.Values{}
		dq.Set("download", "txt")
		if selectedContainer != "" {
			dq.Set("container", selectedContainer)
		}
		dq.Set("tail_lines", strconv.FormatInt(tail, 10))
		if filterText != "" {
			dq.Set("filter", filterText)
		}
		data.DownloadHref = base + "/logs?" + dq.Encode()
		data.DownloadIcon = icon("download")
		data.FollowIcon = icon("rotate-cw")

		if len(allContainers) > 2 {
			for _, container := range allContainers {
				text := container
				if text == "" {
					text = "all"
				}
				tab := templates.LogContainerTab{Active: container == selectedContainer, Label: text}
				if !tab.Active {
					q := "container=" + url.QueryEscape(container) + "&tail_lines=" + url.QueryEscape(strconv.FormatInt(tail, 10)) + "&filter=" + url.QueryEscape(filterText)
					tab.Href = base + "/logs?" + q
				}
				data.Containers = append(data.Containers, tab)
			}
		}
		data.LogPre = logPreHTML(lines, filterText)
	}
	s.pageComponentWithClients(w, r, pageTitle, requestKubeClients{cluster.Name: client}, templates.Logs(data))
}

// logPreHTML builds the trusted <pre class="ro-logpre"> block: a leading
// newline, then one .log-line block span per entry. Each line follows the
// redesign structure (D13 / mockup): the .log-src source pod, the colored
// .log-cN container name (palette index = podColor(container), the CRC32 mod-8
// hash so every container keeps a stable identity colour), the .log-ts
// timestamp split off the entry's first line, then the bare message. A
// continuation entry (a wrapped line with no timestamp prefix, folded into the
// previous block with a '\n') has no .log-ts and renders its whole text as the
// message. The case-sensitive "no matching logs" note closes a filtered-empty
// stream. The whitespace here is significant, so the block is emitted as a raw
// string and injected via templ.Raw.
func logPreHTML(lines []logLine, filterText string) string {
	var b strings.Builder
	b.WriteString(`<pre class="ro-logpre">` + "\n")
	for _, l := range lines {
		ts, msg := splitLogTimestamp(l.Text)
		b.WriteString(`<span class="log-line">`)
		fmt.Fprintf(&b, `<span class="log-src">%s</span> `, html.EscapeString(l.Pod))
		fmt.Fprintf(&b, `<span class="%s">%s</span> `, html.EscapeString(podColor(l.Container)), html.EscapeString(l.Container))
		if ts != "" {
			fmt.Fprintf(&b, `<span class="log-ts">%s</span> `, html.EscapeString(ts))
		}
		b.WriteString(html.EscapeString(msg))
		b.WriteString("</span>\n")
	}
	if filterText != "" && len(lines) == 0 {
		b.WriteString(`<em>No matching logs found. Please note that the filter text is case sensitive!</em>`)
	}
	b.WriteString(`</pre>`)
	return b.String()
}

// splitLogTimestamp splits a log entry's leading RFC3339 timestamp token off the
// rest of the text, so the renderer can wrap it in a faint .log-ts span and leave
// the message bare (matching the redesign mockup). Logs are fetched with
// Timestamps:true, so an entry's first line begins with a parseable timestamp;
// when it does, ts is that token and msg is the remainder. A line without a
// parseable timestamp prefix (a folded continuation, e.g. a wrapped stack frame)
// yields an empty ts and the full text as msg.
func splitLogTimestamp(text string) (ts, msg string) {
	if !hasLogTimestamp(text) {
		return "", text
	}
	ts, msg, _ = strings.Cut(text, " ")
	return ts, msg
}

// downloadLogs serves the assembled log stream as a plain-text attachment
// (D25: the Download-logs title action is a plain GET). One line per entry in
// display order -- source pod, container, then the raw entry text (timestamp
// prefix + message, folded continuation lines kept inline) -- so the file
// mirrors the on-screen stream without markup. The filename derives from the
// request path exactly like downloadYAML (slashes to underscores).
func (s *Server) downloadLogs(w http.ResponseWriter, r *http.Request, lines []logLine) {
	filename := strings.Trim(strings.ReplaceAll(r.URL.Path, "/", "_"), "_")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.txt"`)
	for _, l := range lines {
		_, _ = fmt.Fprintf(w, "%s %s %s\n", l.Pod, l.Container, l.Text)
	}
}

func (s *Server) downloadYAML(w http.ResponseWriter, r *http.Request, obj map[string]any) {
	filename := strings.Trim(strings.ReplaceAll(r.URL.Path, "/", "_"), "_")
	w.Header().Set("Content-Type", "text/vnd.yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.yaml"`)
	data, _ := yamlview.Marshal(obj)
	_, _ = w.Write(data)
}
