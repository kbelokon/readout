package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// link_scheme_safety_test.go pins Unit 10's link-scheme hardening: the
// config/hook-influenced detail link renders through templ.URL (so a
// javascript:/data: scheme collapses to about:invalid), the hand-built YAML
// timestamp <a href> is scheme-validated, and hook-returned links with a
// disallowed scheme are dropped.

// renderDetailLinkHref renders ResourceView with a single config/hook detail
// link carrying href and returns the rendered HTML.
func renderDetailLinkHref(t *testing.T, href string) string {
	t.Helper()
	d := templates.DetailData{
		Kind:  "Deployment",
		Links: []templates.DetailLink{{Href: href, Title: "go", Icon: "x"}},
	}
	var sb strings.Builder
	if err := templates.ResourceView(d).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestDetailLinkSanitizesScheme proves the config/hook detail link href is run
// through templ.URL: executable schemes become about:invalid while http(s) and
// rooted paths pass through unchanged.
func TestDetailLinkSanitizesScheme(t *testing.T) {
	cases := []struct {
		name string
		href string
		want string // substring expected in the rendered href
		bad  bool   // the original href must NOT appear when true
	}{
		{"javascript", "javascript:alert(1)", "about:invalid", true},
		{"data", "data:text/html,<script>", "about:invalid", true},
		{"vbscript", "vbscript:msgbox", "about:invalid", true},
		{"https", "https://ok.example/path", "https://ok.example/path", false},
		{"rooted", "/clusters/x/pods", "/clusters/x/pods", false},
		{"protocol-relative", "//evil.example", "//evil.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			html := renderDetailLinkHref(t, tc.href)
			if !strings.Contains(html, tc.want) {
				t.Fatalf("href %q: rendered HTML missing %q\n%s", tc.href, tc.want, html)
			}
			if tc.bad && strings.Contains(html, `href="`+tc.href) {
				t.Fatalf("href %q rendered verbatim (not sanitized)\n%s", tc.href, html)
			}
		})
	}
}

// timestampLinkObject builds a kube.Object whose serialized YAML carries an
// ISO-8601 creationTimestamp the timestamp linker can rewrite.
func timestampLinkObject() *kube.Object {
	rt := kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}
	obj := kube.NewObject(&rt, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":              "pod-0",
			"namespace":         "default",
			"creationTimestamp": "2024-03-01T08:00:00Z",
		},
	}})
	return &obj
}

// serverWithTimestampLink returns a Server whose TimestampLinks for pods uses
// the given href template.
func serverWithTimestampLink(href string) *Server {
	return &Server{cfg: config.Config{
		TimestampLinks: map[string][]config.Link{
			"pods": {{Href: href, Title: "at {timestamp}"}},
		},
	}}
}

// TestTimestampLinkScheme proves linkTimestampsHTML validates the scheme of the
// hand-built timestamp <a href>: a javascript: href is skipped (timestamp left
// as plain text) while http(s) and rooted hrefs are linked.
func TestTimestampLinkScheme(t *testing.T) {
	obj := timestampLinkObject()
	const ts = "2024-03-01T08:00:00Z"
	line := `<span>` + ts + `</span>`

	cases := []struct {
		name   string
		href   string
		linked bool
	}{
		{"javascript", "javascript:alert(1)", false},
		{"data", "data:text/html", false},
		{"https", "https://logs.example/{namespace}/{name}?t={timestamp}", true},
		{"rooted", "/timeview/{name}?t={timestamp}", true},
		{"mailto", "mailto:ops@example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := serverWithTimestampLink(tc.href)
			out := s.linkTimestampsHTML("c", "default", obj, line)
			if got := strings.Contains(out, "<a href="); got != tc.linked {
				t.Fatalf("href %q: linked=%v, want %v\n%s", tc.href, got, tc.linked, out)
			}
			if !strings.Contains(out, ts) {
				t.Fatalf("timestamp text lost for href %q: %s", tc.href, out)
			}
		})
	}
}

// TestPrerenderHookLinkDropsBadScheme proves hook-returned links with a
// disallowed scheme are dropped from the link list while safe ones are kept.
func TestPrerenderHookLinkDropsBadScheme(t *testing.T) {
	app := newTestServer(t)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"links":[` +
			`{"href":"javascript:alert(1)","title":"evil","icon":"external-link"},` +
			`{"href":"https://ok.example","title":"ok","icon":"external-link"},` +
			`{"href":"data:text/html,x","title":"evil2","icon":"external-link"},` +
			`{"href":"/rooted","title":"rooted","icon":"external-link"}` +
			`]}`))
	}))
	defer hook.Close()
	app.cfg.ResourcePrerenderHookURL = hook.URL

	seed := []config.Link{{Href: "/base", Title: "Base"}}
	got, _, err := app.resourcePrerenderHook(context.Background(), "test", "default", "pods", timestampLinkObject(), seed)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	var hrefs []string
	for _, l := range got {
		hrefs = append(hrefs, l.Href)
	}
	want := []string{"/base", "https://ok.example", "/rooted"}
	if len(hrefs) != len(want) {
		t.Fatalf("hook links = %v, want %v", hrefs, want)
	}
	for i, h := range want {
		if hrefs[i] != h {
			t.Fatalf("hook links = %v, want %v", hrefs, want)
		}
	}
}
