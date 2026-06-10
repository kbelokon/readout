package web

// colvis_test.go pins the D8 column-visibility contract introduced with the ⊞
// popover:
//
//   - the hide spec is applied AFTER the label/synthetic/joined columns land,
//     so SYNTHETIC columns (node Pods/Conditions, namespace Labels) hide too --
//     including through the ro_prefs cookie (the Unit-8 caveat: removal used
//     to run before the decorations, which made these unhideable);
//   - the identity/name column is NEVER removed: ?hidecols=Name is ignored
//     server-side, and `*` keeps the identity column standing;
//   - the synthetic Created column (template-rendered, not a kube column)
//     hides through the same spec;
//   - the popover renders the FULL column universe (hidden entries stay
//     re-offerable, unchecked; the identity entry checked + disabled) on
//     single-type pages only -- multi-type pages keep the v1 tools form.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestColvisSyntheticColumnsHide pins the moved removal point: synthetic
// columns added by the decorations are hideable via ?hidecols= -- node
// Conditions/Pods (decorateNodeColumns) and the namespace Labels chips column
// (decorateNamespaceColumns).
func TestColvisSyntheticColumnsHide(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	control := get(t, app, "/clusters/test/nodes", http.StatusOK)
	assertContainsAll(t, "control node headers", control.texts("thead th"), "Conditions", "Pods")

	p := get(t, app, "/clusters/test/nodes?hidecols=Conditions,Pods", http.StatusOK)
	headers := p.texts("thead th")
	if contains(headers, "Conditions") || contains(headers, "Pods") {
		t.Fatalf("hidecols left synthetic node columns standing: %v", headers)
	}

	ns := get(t, app, "/clusters/test/namespaces?hidecols=Labels", http.StatusOK)
	if contains(ns.texts("thead th"), "Labels") {
		t.Fatalf("hidecols=Labels left the synthetic namespace Labels column: %v", ns.texts("thead th"))
	}
}

// TestColvisCookieHidesSyntheticColumns pins the same fact through the D9
// cookie write surface the popover uses: a persisted hide list naming a
// synthetic column render-hides it (the exact gap the pre-v2 removal order
// left open).
func TestColvisCookieHidesSyntheticColumns(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	hide := []string{"Conditions"}
	cookie := encodePrefs(prefs{Kinds: []kindPrefs{{Plural: "nodes", Hide: &hide}}})

	p := prefsGet(t, app, "/clusters/test/nodes", cookie, nil)
	if contains(p.texts("thead th"), "Conditions") {
		t.Fatalf("cookie hide left the synthetic Conditions column: %v", p.texts("thead th"))
	}
	// The popover still offers it back, unchecked.
	p.wantHas(`#ro-cols-pop .col-toggle[data-col="Conditions"]`)
	if p.has(`#ro-cols-pop .col-toggle[data-col="Conditions"] .ro-check[checked]`) {
		t.Fatalf("hidden Conditions entry renders checked")
	}
}

// TestColvisIdentityColumnProtected pins the D8 identity rule: the name column
// is not hideable -- a forced ?hidecols=Name is ignored server-side, and a
// wildcard hide keeps the identity column (plus nothing else) standing.
func TestColvisIdentityColumnProtected(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	p := get(t, app, "/clusters/test/namespaces/default/pods?hidecols=Name", http.StatusOK)
	if !contains(p.texts("thead th"), "Name") {
		t.Fatalf("hidecols=Name removed the identity column: %v", p.texts("thead th"))
	}
	if got := p.texts("td.cell-name"); strings.Join(got, "|") != "nginx|my-app" {
		t.Fatalf("hidecols=Name broke the name cells: %v", got)
	}
	// The popover renders the identity entry checked + disabled.
	p.wantHas(`#ro-cols-pop .col-toggle[data-col="Name"][disabled] .ro-check[checked][disabled]`)

	star := get(t, app, "/clusters/test/namespaces/default/pods?hidecols=*", http.StatusOK)
	if got := strings.Join(star.texts("thead th"), "|"); got != "Name" {
		t.Fatalf("hidecols=* headers = %q, want exactly the protected Name column", got)
	}
}

// TestColvisCreatedColumnHides pins the synthetic Created column's hide path:
// it is rendered by the template (not a kube column), so the spec reaches it
// via the render flag -- header gone, per-row created cell gone, and the
// popover still lists it as a re-offerable entry.
func TestColvisCreatedColumnHides(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	control := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	if !contains(control.texts("thead th"), "Created") {
		t.Fatalf("control render lost the Created header: %v", control.texts("thead th"))
	}
	controlCells := control.count("table.ro-table tbody tr:first-child td")

	p := get(t, app, "/clusters/test/namespaces/default/pods?hidecols=Created", http.StatusOK)
	if contains(p.texts("thead th"), "Created") {
		t.Fatalf("hidecols=Created left the Created header: %v", p.texts("thead th"))
	}
	if got := p.count("table.ro-table tbody tr:first-child td"); got != controlCells-1 {
		t.Fatalf("hidden Created row cells = %d, want %d (one fewer than control)", got, controlCells-1)
	}
	p.wantHas(`#ro-cols-pop .col-toggle[data-col="Created"]`)
	if p.has(`#ro-cols-pop .col-toggle[data-col="Created"] .ro-check[checked]`) {
		t.Fatalf("hidden Created entry renders checked")
	}
}

// TestColvisConfigDefaultRendersHiddenButOffered pins the config-default leg
// the v2 shipped defaults ride: a DefaultHiddenColumns entry hides the column
// at render while the popover keeps offering it (unchecked), and an explicit
// empty cookie hide set resurfaces it (user override wins, D8).
func TestColvisConfigDefaultRendersHiddenButOffered(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DefaultHiddenColumns = map[string]string{"nodes": "External-IP,Created"}
	app := newServer(t, cfg, time.Now())

	p := get(t, app, "/clusters/test/nodes", http.StatusOK)
	headers := p.texts("thead th")
	if contains(headers, "External-IP") || contains(headers, "Created") {
		t.Fatalf("config default did not hide External-IP/Created: %v", headers)
	}
	p.wantHas(`#ro-cols-pop .col-toggle[data-col="External-IP"]`)

	empty := []string{}
	cookie := encodePrefs(prefs{Kinds: []kindPrefs{{Plural: "nodes", Hide: &empty}}})
	shown := prefsGet(t, app, "/clusters/test/nodes", cookie, nil)
	assertContainsAll(t, "explicit-empty headers", shown.texts("thead th"), "External-IP", "Created")
}

// TestColsPopoverSingleTypeGate pins the D1 boundary of the popover chrome:
// single-type pages render the ⊞ popover (and no v1 tools form); multi-type
// pages keep the v1 toggle-tools + tools form and get no popover.
func TestColsPopoverSingleTypeGate(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	single := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	single.wantHas("#ro-cols-pop")
	single.wantHas(`#ro-cols-pop[data-plural="pods"]`)
	single.wantAbsent("form.tools-form")
	single.wantAbsent("a.toggle-tools")

	multi := get(t, app, "/clusters/test/namespaces/default/pods,services", http.StatusOK)
	multi.wantAbsent("#ro-cols-pop")
	multi.wantHas("form.tools-form")
	multi.wantAttr("a.toggle-tools", "data-target", "tools-table-1")
}
