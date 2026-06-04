package web

import (
	"encoding/csv"
	"fmt"
	"html"
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
	ctx, err := s.listContext(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	if r.URL.Query().Get("download") == "tsv" && len(ctx.Tables) > 0 {
		s.downloadTSV(w, r, &ctx.Tables[0])
		return
	}
	view := s.buildListView(r, &ctx)
	partialURL := partialResourceListURL(r)
	s.pageComponent(w, r, view.Title(), templates.ResourceList(toListPageData(&view, partialURL)))
}

func (s *Server) resourceListPartial(w http.ResponseWriter, r *http.Request) {
	ctx, err := s.listContext(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := s.buildListView(r, &ctx)
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

func (s *Server) resourceView(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	view, ok := s.buildDetailView(w, r, cluster)
	if !ok {
		return
	}
	s.pageComponentWithNamespace(w, r, view.Title, &view.Namespace, templates.ResourceView(toDetailData(view)))
}

func (s *Server) resourceLogs(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	name := r.PathValue("name")
	if namespace != "" && !s.namespaceAllowed(namespace) {
		s.error(w, r, statusError{status: http.StatusForbidden, message: "namespace is not allowed"})
		return
	}
	rt, err := s.kubeClient(r, cluster).FindResource(r.Context(), plural, true, "")
	if err != nil {
		s.error(w, r, err)
		return
	}
	obj, err := s.kubeClient(r, cluster).Get(r.Context(), &rt, namespace, name)
	if err != nil {
		s.error(w, r, err)
		return
	}
	object := kube.NewObject(&rt, obj)
	pods := []kube.Object{object}
	if object.Kind() != "Pod" {
		pods = s.podsForSelector(r, cluster, &object, namespace)
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
			logs, err := s.kubeClient(r, cluster).Logs(r.Context(), kube.LogOptions{Namespace: first(pod.Namespace(), namespace), Pod: pod.Name(), Container: container, Timestamps: true, TailLines: tail})
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
		DefaultHref:       base,
		YAMLHref:          base + "?view=yaml",
		ShowContainerLogs: s.cfg.ShowContainerLogs,
		TailLines:         tail,
		PodCount:          len(pods),
		FilterVal:         filterText,
	}
	if s.cfg.ShowContainerLogs {
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
		data.LogPre = logPreHTML(lines, selectedContainer, filterText)
	}
	s.pageComponent(w, r, pageTitle, templates.Logs(data))
}

// logPreHTML builds the trusted <pre class="ro-logpre"> block: a leading
// newline, one line per entry (pod color span + optional container span + the
// level-classed message span, each newline-terminated), and the case-sensitive
// "no matching logs" note. The whitespace here is significant, so it is emitted
// as a raw string and injected via templ.Raw.
func logPreHTML(lines []logLine, selectedContainer, filterText string) string {
	var b strings.Builder
	b.WriteString(`<pre class="ro-logpre">` + "\n")
	for _, l := range lines {
		msgClass := "log-msg"
		if strings.Contains(l.Text, "ERROR") || strings.Contains(l.Text, "error") {
			msgClass += " log-lvl-err"
		} else if strings.Contains(l.Text, "WARN") || strings.Contains(l.Text, "warn") {
			msgClass += " log-lvl-warn"
		}
		fmt.Fprintf(&b, `<span class="log-line %s">%s</span> `, html.EscapeString(podColor(l.Pod)), html.EscapeString(l.Pod))
		if selectedContainer == "" {
			fmt.Fprintf(&b, `<span class="log-ctr">%s</span> `, html.EscapeString(l.Container))
		}
		fmt.Fprintf(&b, `<span class="%s">%s</span>`+"\n", html.EscapeString(msgClass), html.EscapeString(l.Text))
	}
	if filterText != "" && len(lines) == 0 {
		b.WriteString(`<em>No matching logs found. Please note that the filter text is case sensitive!</em>`)
	}
	b.WriteString(`</pre>`)
	return b.String()
}

func (s *Server) downloadYAML(w http.ResponseWriter, r *http.Request, obj map[string]any) {
	filename := strings.Trim(strings.ReplaceAll(r.URL.Path, "/", "_"), "_")
	w.Header().Set("Content-Type", "text/vnd.yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.yaml"`)
	data, _ := yamlview.Marshal(obj)
	_, _ = w.Write(data)
}
