package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// preferences_redesign_test.go pins the recolor-only redesign contract of the
// preferences page (Unit 13). Preferences keeps its retained Bulma `.box`/`.field`
// form (D4: Bulma stays vendored for residual form primitives; D11: borrow rule =
// recolor only, no custom-component rebuild). The facts below are independent of
// any single class the markup happens to emit beyond the two load-bearing ones:
//   - the form must carry the `ro-prefs` recolor MARKER, because readout.css scopes
//     the redesign-token recolor of the label / the <select> control / the dropdown
//     arrow / the leading icon under `.ro-prefs`. Without the marker those rules
//     never apply and the control renders in Bulma's own chrome (the teal arrow +
//     blue focus ring D4 calls "stuck on old Bulma blue"), so the marker IS the
//     "reads as redesign" assertion for a recolor-only screen.
//   - the theme <select> + the Bulma `.box` form shell must still render and the
//     POST /preferences write must still round-trip (behavior unchanged).

// TestPreferencesFormReadsAsRedesign asserts the theme form still renders inside
// the redesign shell and carries the `ro-prefs` recolor marker, so the readout.css
// token recolor (label / select / arrow / icon) actually applies. This is the
// recolor-only "reads as redesign" bar: same Bulma markup, redesign tokens.
func TestPreferencesFormReadsAsRedesign(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/preferences", http.StatusOK)

	// Page renders inside the redesign chrome (the shell the migrated screens use).
	p.wantHas("header.ro-topbar")
	p.wantText("h1.title", "Preferences")

	// The retained Bulma form shell is kept (D4/D11: recolor only, not rebuilt) and
	// carries the redesign recolor marker that activates the token recolor.
	p.wantAttr(`form.ro-prefs[action="/preferences"]`, "method", "post")
	p.wantAttr(`form.ro-prefs[action="/preferences"]`, "hx-boost", "false")
	p.wantHas(`form.box[action="/preferences"]`)
	p.wantHas(`form.ro-prefs .field`)
	p.wantHas(`form.ro-prefs label.label`)

	// The theme <select> (inside Bulma's `.select` wrapper the recolor targets) and
	// the accent Save button still render.
	p.wantHas(`form.ro-prefs .select select[name="theme"]`)
	p.wantHas(`form.ro-prefs button.is-primary`)

	// The select is populated from the resolved options with exactly one selected
	// (the current theme), proving the theme list still renders.
	if n := p.count(`select[name="theme"] option`); n == 0 {
		t.Fatalf("theme select rendered no options: %s", p.rec.Body.String())
	}
	if n := p.count(`select[name="theme"] option[selected]`); n != 1 {
		t.Fatalf("theme select selected-option count = %d, want exactly 1\nbody=%s", n, p.rec.Body.String())
	}
}

// TestPreferencesPostRoundTrips asserts the one allowed write is unchanged: POST
// /preferences persists the chosen theme as the cookie and 303-redirects to the
// resolved next target. Behavior is identical to pre-redesign; this is the
// behavioral half of the recolor-only bar.
func TestPreferencesPostRoundTrips(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	req := httptest.NewRequest(http.MethodPost, "/preferences", strings.NewReader("theme=light&next=/clusters"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /preferences status = %d, want 303\nbody=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/clusters" {
		t.Fatalf("POST /preferences redirect = %q, want /clusters", loc)
	}
	var themeCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "theme" {
			themeCookie = c
		}
	}
	if themeCookie == nil {
		t.Fatalf("POST /preferences set no theme cookie\nheaders=%v", rec.Header())
	}
	if themeCookie.Value != "light" {
		t.Fatalf("theme cookie = %q, want light", themeCookie.Value)
	}
}
