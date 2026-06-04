package web

// templ_customization_test.go pins that the templ page shell injects the
// operator's custom extrahead/footer partials from cfg.TemplatesPath (via
// loadPartials -> s.partials, emitted with templ.Raw because they are trusted
// operator HTML). It builds a real Server through New with a TemplatesPath temp
// dir and asserts BOTH custom markups survive the templ layout render.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTemplLayoutPreservesTemplatesPathPartials(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "partials"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Custom head + footer markup the operator drops into TemplatesPath. Distinct,
	// addressable hooks so the assertion is exact (an id/name a default page never
	// emits).
	if err := os.WriteFile(filepath.Join(dir, "partials", "extrahead.html"), []byte(`<meta name="x-custom-extrahead" content="present">`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "partials", "footer.html"), []byte(`<footer id="x-custom-footer">custom footer body</footer>`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig(t)
	cfg.TemplatesPath = dir
	app := newServer(t, cfg, time.Now())

	// Any page exercises the shell; the clusters page needs no list fixtures.
	p := get(t, app, "/clusters", http.StatusOK)

	// The custom extrahead survived into <head>.
	p.wantHas(`head meta[name="x-custom-extrahead"]`)
	p.wantAttr(`meta[name="x-custom-extrahead"]`, "content", "present")

	// The custom footer REPLACED the default footer (the codeberg link is gone).
	p.wantHas(`#x-custom-footer`)
	p.wantText(`#x-custom-footer`, "custom footer body")
	if p.has(`footer.ro-footer`) {
		t.Fatalf("default footer should be replaced by the custom TemplatesPath footer")
	}
}
