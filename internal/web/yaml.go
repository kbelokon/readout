package web

import (
	"html"
	"regexp"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/yamlview"
)

// yaml.go is the web-side bridge to the pure internal/yamlview package: it owns
// the cluster/namespace/object + config-derived inputs that yamlview must NOT
// import (yamlview stays free of net/http, kube.Client, and config) and adapts
// them into the plain values yamlview.Highlight accepts. Serialization itself is
// yamlview.Marshal (sigs.k8s.io/yaml); see its docs for the serialization shape.

// iso8601Timestamp matches the canonical k8s RFC3339 instant
// (YYYY-MM-DDThh:mm:ssZ) inside a rendered YAML line, so timestamp values can be
// turned into time-view links.
var iso8601Timestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)

// highlightYAML renders the highlighted YAML HTML for an object's serialized
// form, injecting timestamp links via a per-line callback. It delegates the DOM
// + token classification to yamlview.Highlight (the chroma-lexer custom
// formatter) and supplies only plain inputs, keeping yamlview pure.
//
// anchorPrefix is "" for the full YAML view and "<key>-" for a per-section card.
func (s *Server) highlightYAML(cluster, namespace string, object *kube.Object, anchorPrefix, data string) string {
	return yamlview.Highlight(data, anchorPrefix, func(line string) string {
		return s.linkTimestampsHTML(cluster, namespace, object, line)
	})
}

// linkTimestampsHTML rewrites every ISO-8601 timestamp in a rendered YAML line
// into an <a> link to the configured time-based view for the object's resource
// endpoint. With no configured TimestampLinks it returns the line unchanged.
// Operates on the already-highlighted line HTML (the timestamp digits/colons are
// not HTML-escaped, so the regex still matches inside the token spans) -- the
// behaviour is identical to the previous in-package emitter.
func (s *Server) linkTimestampsHTML(cluster, namespace string, object *kube.Object, out string) string {
	links := s.cfg.TimestampLinks[object.Resource.Endpoint()]
	if len(links) == 0 {
		return out
	}
	link := links[0]
	return iso8601Timestamp.ReplaceAllStringFunc(out, func(timestamp string) string {
		repl := strings.NewReplacer("{cluster}", cluster, "{namespace}", namespace, "{name}", object.Name(), "{timestamp}", timestamp)
		href := repl.Replace(link.Href)
		title := repl.Replace(first(link.Title, timestamp))
		return `<a href="` + html.EscapeString(href) + `" title="` + html.EscapeString(title) + `">` + timestamp + `</a>`
	})
}
