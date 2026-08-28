package web

import (
	"bytes"
	"maps"
	"testing"
	"testing/fstest"
)

func TestResourceListRendererFingerprintEmbeddedSet(t *testing.T) {
	t.Parallel()
	first := resourceListRendererFingerprint()
	second := resourceListRendererFingerprint()
	if first != second {
		t.Fatalf("renderer fingerprint is not deterministic: %x != %x", first, second)
	}
	if bytes.Equal(first[:], make([]byte, len(first))) {
		t.Fatal("embedded renderer fingerprint is zero")
	}

	computed, paths, err := fingerprintResourceListRendererSources(resourceListRendererSources)
	if err != nil {
		t.Fatalf("fingerprint embedded renderer sources: %v", err)
	}
	if computed != first {
		t.Fatalf("cached fingerprint = %x, direct computation = %x", first, computed)
	}
	for _, required := range []string{
		"templates/table-chrome.templ",
		"templates/table-chrome_templ.go",
		"templates/helpers.go",
	} {
		if !rendererPathsContain(paths, required) {
			t.Fatalf("embedded renderer paths omit %q: %v", required, paths)
		}
	}
}

func TestFingerprintResourceListRendererSourcesAutomaticFileSet(t *testing.T) {
	t.Parallel()
	sources := fstest.MapFS{
		"templates/z.go":           {Data: []byte("package templates\n")},
		"templates/a.templ":        {Data: []byte("package templates\n\ntempl A() {}\n")},
		"templates/ignored.txt":    {Data: []byte("not renderer source")},
		"templates/nested/skip.go": {Data: []byte("package nested\n")},
	}

	base, paths, err := fingerprintResourceListRendererSources(sources)
	if err != nil {
		t.Fatalf("fingerprint map sources: %v", err)
	}
	wantPaths := []string{"templates/a.templ", "templates/z.go"}
	if len(paths) != len(wantPaths) {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
	for i := range wantPaths {
		if paths[i] != wantPaths[i] {
			t.Fatalf("paths = %v, want sorted %v", paths, wantPaths)
		}
	}

	changed := maps.Clone(sources)
	changed["templates/a.templ"] = &fstest.MapFile{Data: []byte("package templates\n\ntempl A() { changed }\n")}
	changedDigest, changedPaths, err := fingerprintResourceListRendererSources(changed)
	if err != nil {
		t.Fatalf("fingerprint changed map sources: %v", err)
	}
	if changedDigest == base {
		t.Fatalf("renderer byte change kept fingerprint %x", base)
	}
	if len(changedPaths) != len(paths) {
		t.Fatalf("renderer byte change altered file set: %v -> %v", paths, changedPaths)
	}

	added := maps.Clone(sources)
	added["templates/new.go"] = &fstest.MapFile{Data: []byte("package templates\n")}
	addedDigest, addedPaths, err := fingerprintResourceListRendererSources(added)
	if err != nil {
		t.Fatalf("fingerprint added map source: %v", err)
	}
	if addedDigest == base {
		t.Fatalf("automatic matching file addition kept fingerprint %x", base)
	}
	if !rendererPathsContain(addedPaths, "templates/new.go") {
		t.Fatalf("automatic file set omitted new matching source: %v", addedPaths)
	}
}

func rendererPathsContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
