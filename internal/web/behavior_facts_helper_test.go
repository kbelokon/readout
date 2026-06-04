package web

// Hermetic behavior-fact helpers.
//
// These helpers parse a rendered page into a goquery document and expose a
// small, named-assertion vocabulary so the behavior-fact tests can pin the
// STRUCTURAL + BEHAVIORAL contract of every page by exact selector+value --
// not by raw substring `strings.Contains`. A named goquery assertion survives
// attribute reordering and whitespace changes while still failing loudly when
// the contract it guards (a cell value, a `?sort=` href, an age-bucket class,
// an `hx-*` wire, a palette `<template>` id) moves.
//
// Every expected value here is read off the server's own output today.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/config"
)

// page is a parsed response: the HTTP recorder plus a goquery document over its
// body. It carries the request path for readable failure messages.
type page struct {
	t    *testing.T
	path string
	rec  *httptest.ResponseRecorder
	doc  *goquery.Document
}

// get drives one GET through the full handler chain and parses the HTML body
// into a goquery document. It fails the test if the status is not the want, so
// every fact test starts from a known-good render.
func get(t *testing.T, app *Server, path string, wantStatus int) *page {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status = %d, want %d\nbody=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("GET %s: parse HTML: %v", path, err)
	}
	return &page{t: t, path: path, rec: rec, doc: doc}
}

// text returns the normalized (whitespace-collapsed) text of the FIRST match of
// selector, failing if nothing matches.
func (p *page) text(selector string) string {
	p.t.Helper()
	sel := p.doc.Find(selector)
	if sel.Length() == 0 {
		p.t.Fatalf("GET %s: selector %q matched nothing\nbody=%s", p.path, selector, p.rec.Body.String())
	}
	return normSpace(sel.First().Text())
}

// texts returns the normalized text of EVERY match of selector, in document
// order. Empty results are allowed (the caller asserts on the slice).
func (p *page) texts(selector string) []string {
	p.t.Helper()
	var out []string
	p.doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		out = append(out, normSpace(s.Text()))
	})
	return out
}

// attr returns the attribute value of the FIRST match of selector, failing if
// the element is absent or the attribute is unset. This is the core named
// JS-contract assertion: exact selector -> exact attribute value.
func (p *page) attr(selector, name string) string {
	p.t.Helper()
	sel := p.doc.Find(selector)
	if sel.Length() == 0 {
		p.t.Fatalf("GET %s: selector %q matched nothing (wanted attr %q)\nbody=%s", p.path, selector, name, p.rec.Body.String())
	}
	value, ok := sel.First().Attr(name)
	if !ok {
		p.t.Fatalf("GET %s: selector %q has no attribute %q\nbody=%s", p.path, selector, name, p.rec.Body.String())
	}
	return value
}

// attrs returns the attribute value of every match of selector (only matches
// that actually carry the attribute), in document order.
func (p *page) attrs(selector, name string) []string {
	p.t.Helper()
	var out []string
	p.doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		if value, ok := s.Attr(name); ok {
			out = append(out, value)
		}
	})
	return out
}

// has reports whether at least one element matches selector.
func (p *page) has(selector string) bool {
	return p.doc.Find(selector).Length() > 0
}

// count returns how many elements match selector.
func (p *page) count(selector string) int {
	return p.doc.Find(selector).Length()
}

// wantAttr asserts the first match of selector carries name=want exactly.
func (p *page) wantAttr(selector, name, want string) {
	p.t.Helper()
	if got := p.attr(selector, name); got != want {
		p.t.Fatalf("GET %s: %s[%s] = %q, want %q", p.path, selector, name, got, want)
	}
}

// wantText asserts the first match of selector has the given normalized text.
func (p *page) wantText(selector, want string) {
	p.t.Helper()
	if got := p.text(selector); got != want {
		p.t.Fatalf("GET %s: %s text = %q, want %q", p.path, selector, got, want)
	}
}

// wantHas asserts at least one element matches selector.
func (p *page) wantHas(selector string) {
	p.t.Helper()
	if !p.has(selector) {
		p.t.Fatalf("GET %s: expected an element matching %q, found none\nbody=%s", p.path, selector, p.rec.Body.String())
	}
}

// wantAbsent asserts NO element matches selector. Used for the secret barrier
// (no Secret row under default config) and other negative facts.
func (p *page) wantAbsent(selector string) {
	p.t.Helper()
	if n := p.count(selector); n != 0 {
		p.t.Fatalf("GET %s: expected NO element matching %q, found %d\nbody=%s", p.path, selector, n, p.rec.Body.String())
	}
}

// wantBodyContains is the escape hatch for raw-text facts that are not cleanly
// addressable by a selector (e.g. a base64 secret byte that must NOT leak
// anywhere in the rendered output). Kept deliberately rare; the structural
// facts go through the selector vocabulary above.
func (p *page) wantBodyContains(substr string) {
	p.t.Helper()
	if !strings.Contains(p.rec.Body.String(), substr) {
		p.t.Fatalf("GET %s: body missing %q\nbody=%s", p.path, substr, p.rec.Body.String())
	}
}

// wantBodyExcludes asserts a substring appears NOWHERE in the rendered output.
func (p *page) wantBodyExcludes(substr string) {
	p.t.Helper()
	if strings.Contains(p.rec.Body.String(), substr) {
		p.t.Fatalf("GET %s: body unexpectedly contains %q\nbody=%s", p.path, substr, p.rec.Body.String())
	}
}

// containsHref reports whether any element matched by selector has an href
// equal to want. Used to assert a specific `?sort=` permalink exists among the
// column-header links without depending on header order.
func (p *page) containsHref(selector, want string) bool {
	for _, h := range p.attrs(selector, "href") {
		if h == want {
			return true
		}
	}
	return false
}

// normSpace collapses runs of whitespace to single spaces and trims, so text
// facts compare on visible content rather than incidental whitespace.
func normSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fixedClock returns a clock function pinned to t, for deterministic age-bucket
// rendering. The render path reads s.now via s.clock(); injecting a fixed
// instant makes every age-* cell class deterministic.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// baseConfig is the default hermetic server config: one fake cluster named
// "test", dark theme, access logs suppressed (the access-log test builds its
// own server so it can observe logging).
func baseConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Port:         8080,
		Clusters:     map[string]string{"test": newServerFakeAPI(t).URL},
		DefaultTheme: "dark",
		NoAccessLogs: true,
	}
}

// withSecrets is baseConfig plus IncludeSecrets=true (the masked-on half of the
// secret barrier).
func withSecrets(t *testing.T) *config.Config {
	cfg := baseConfig(t)
	cfg.IncludeSecrets = true
	return cfg
}

// newServer builds a Server straight from New (NOT via newTestServerWithConfig,
// which force-sets NoAccessLogs); callers control every field, including the
// access-log knob. The clock is pinned to a fixed instant so age facts are
// deterministic; callers that do not care still get reproducible output.
func newServer(t *testing.T, cfg *config.Config, clock time.Time) *Server {
	t.Helper()
	app := newTestServerWithConfig(t, cfg)
	app.now = fixedClock(clock)
	return app
}
