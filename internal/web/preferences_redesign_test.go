package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// preferences_redesign_test.go pins the owned-control preferences contract. The
// page keeps the POST /preferences behavior but renders native controls through
// `.ro-*` classes instead of a framework form shell.

// TestPreferencesFormReadsAsRedesign asserts the theme form still renders inside
// the redesign shell using the owned preferences form vocabulary.
func TestPreferencesFormReadsAsRedesign(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/preferences", http.StatusOK)

	// Page renders inside the redesign chrome (the shell the migrated screens use).
	p.wantHas("header.ro-topbar")
	p.wantText("h1.title", "Preferences")

	p.wantAttr(`form.ro-prefs[action="/preferences"]`, "method", "post")
	p.wantAttr(`form.ro-prefs[action="/preferences"]`, "hx-boost", "false")
	p.wantHas(`form.ro-prefs .ro-form-row`)
	p.wantHas(`form.ro-prefs label.ro-label[for="theme-select"]`)

	// The native theme <select> and the accent Save button still render.
	p.wantHas(`form.ro-prefs select.ro-select#theme-select[name="theme"]`)
	p.wantHas(`form.ro-prefs button.ro-btn.ro-btn-primary[type="submit"]`)

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
