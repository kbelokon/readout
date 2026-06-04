package icons

import (
	"html/template"
	"strings"
	"testing"
)

// The resolver's hues / abbrevs / tier selection have no external oracle but
// determinism, so every expectation below is computed INDEPENDENTLY (by hand /
// a second implementation) rather than by re-reading the rendered --kh. The
// hashHue integers were cross-checked with a Python reimplementation of the JS
// `h = (h*31 + charCode) >>> 0; return h % 360`.
func TestKindIcon(t *testing.T) {
	t.Run("hashHue matches the JS hash byte-for-byte", func(t *testing.T) {
		// Independently computed (uint32 wrap, then % 360).
		cases := map[string]int{
			"cilium.io":          81,
			"redpanda.com":       196,
			"cert-manager.io":    24,
			"postgresql.cnpg.io": 88,
			"argoproj.io":        134,
			"":                   0, // empty string hashes to 0
		}
		for in, want := range cases {
			if got := HashHue(in); got != want {
				t.Errorf("HashHue(%q) = %d, want %d", in, got, want)
			}
			if got := HashHue(in); got < 0 || got >= 360 {
				t.Errorf("HashHue(%q) = %d, out of [0,360)", in, got)
			}
		}
	})

	t.Run("kindAbbrev override table then caps/first-two rule", func(t *testing.T) {
		cases := map[string]string{
			"CiliumNetworkPolicy": "CN", // >=2 capitals -> first two capitals
			"Pod":                 "PO", // override table
			"Service":             "SV", // override table
			"Topic":               "TO", // one capital -> first two letters upper
			"kustomization":       "KU", // no capitals -> first two letters upper
			"x":                   "X",  // single char -> single upper (no panic)
			"Certificate":         "CE", // one capital -> first two letters upper
		}
		for in, want := range cases {
			if got := KindAbbrev(in); got != want {
				t.Errorf("KindAbbrev(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("tier 1 — curated glyph for a built-in non-CRD kind", func(t *testing.T) {
		got := string(KindIcon("Pod", "", false, ""))
		// curated glyph rides in a `.ico sm` span and is NOT a tile/img/emoji.
		if !strings.Contains(got, `class="ico sm"`) {
			t.Fatalf("Pod (tier1) should be an `ico sm` span: %s", got)
		}
		if strings.Contains(got, "kind-tile") || strings.Contains(got, "kind-curated") ||
			strings.Contains(got, "kind-img") || strings.Contains(got, "kind-emoji") {
			t.Fatalf("Pod (tier1) leaked a tier2/3 class: %s", got)
		}
		if !strings.Contains(got, "<svg") {
			t.Fatalf("Pod (tier1) should embed an inline SVG glyph: %s", got)
		}
		// The Pod glyph must be the curated `pod` SVG specifically — not a
		// different-but-valid glyph and not the neutral fallback circle. SVG("pod")
		// is the independent expectation for the claim KIND_ICON["Pod"]="pod"; a
		// mis-mapping (e.g. Pod->"service") embeds SVG("service") and fails here.
		if !strings.Contains(got, SVG("pod")) {
			t.Fatalf("Pod (tier1) must embed the curated `pod` glyph, got: %s", got)
		}
		if strings.Contains(got, `r="10"`) {
			t.Fatalf("Pod (tier1) resolved to the neutral fallback circle: %s", got)
		}
	})

	t.Run("tier 1 — CustomResourceDefinition keeps the crd glyph even as a CRD", func(t *testing.T) {
		got := string(KindIcon("CustomResourceDefinition", "apiextensions.k8s.io", true, ""))
		if !strings.Contains(got, `class="ico sm"`) || strings.Contains(got, "kind-tile") {
			t.Fatalf("CRD kind should keep the curated puzzle glyph: %s", got)
		}
	})

	t.Run("tier 2a — group-tinted curated glyph keyed on the group hue", func(t *testing.T) {
		got := string(KindIcon("Certificate", "cert-manager.io", true, ""))
		if !strings.Contains(got, "kind-curated") {
			t.Fatalf("cert-manager.io CRD should be a tinted curated glyph: %s", got)
		}
		// hue is the GROUP hash (24), never the kind hash. Match the CLOSED value
		// (`--kh:24"`) so a regression producing 240..249 cannot prefix-match.
		if !strings.Contains(got, `--kh:24"`) {
			t.Fatalf("tier2a hue must be hashHue(group)=24: %s", got)
		}
		if strings.Contains(got, "kind-tile") {
			t.Fatalf("tier2a must not fall through to a monogram tile: %s", got)
		}
	})

	t.Run("tier 2a — fluxcd/gatekeeper suffix-regex group families resolve to tinted glyphs", func(t *testing.T) {
		// The regex fallback (*.fluxcd.io->gitops, *.gatekeeper.sh->role) is a
		// distinct branch from the exact-match crdGroupGlyph map. Hues computed
		// independently: HashHue("kustomize.toolkit.fluxcd.io")=263,
		// HashHue("config.gatekeeper.sh")=162.
		flux := string(KindIcon("Kustomization", "kustomize.toolkit.fluxcd.io", true, ""))
		if !strings.Contains(flux, "kind-curated") || !strings.Contains(flux, SVG("gitops")) {
			t.Fatalf("*.fluxcd.io must resolve to the tinted `gitops` glyph: %s", flux)
		}
		if !strings.Contains(flux, `--kh:263"`) {
			t.Fatalf("fluxcd family hue must be hashHue(group)=263: %s", flux)
		}
		gk := string(KindIcon("Config", "config.gatekeeper.sh", true, ""))
		if !strings.Contains(gk, "kind-curated") || !strings.Contains(gk, SVG("role")) {
			t.Fatalf("*.gatekeeper.sh must resolve to the tinted `role` glyph: %s", gk)
		}
		if !strings.Contains(gk, `--kh:162"`) {
			t.Fatalf("gatekeeper family hue must be hashHue(group)=162: %s", gk)
		}
	})

	t.Run("tier 2b — group-keyed monogram for an unmapped CRD family", func(t *testing.T) {
		got := string(KindIcon("Topic", "redpanda.com", true, ""))
		if !strings.Contains(got, `class="kind-tile"`) {
			t.Fatalf("redpanda.com CRD should be a monogram tile: %s", got)
		}
		// keyed on the API GROUP hue (196), NOT the kind — the whole point.
		if !strings.Contains(got, "--kh:196") {
			t.Fatalf("monogram hue must be hashHue(group)=196 (group-keyed, not kind): %s", got)
		}
		if !strings.Contains(got, ">TO<") {
			t.Fatalf("monogram label must be kindAbbrev(Topic)=TO: %s", got)
		}
	})

	t.Run("tier 2b — two kinds in one group share a hue (family colour)", func(t *testing.T) {
		a := string(KindIcon("Topic", "redpanda.com", true, ""))
		b := string(KindIcon("User", "redpanda.com", true, ""))
		if !strings.Contains(a, "--kh:196") || !strings.Contains(b, "--kh:196") {
			t.Fatalf("same-group CRDs must share the group hue: a=%s b=%s", a, b)
		}
	})

	t.Run("tier 3 — lucide: prefix renders a bundled glyph span", func(t *testing.T) {
		got := string(KindIcon("Topic", "redpanda.com", true, "lucide:rotate-cw"))
		if !strings.Contains(got, `class="ico sm"`) || !strings.Contains(got, "<svg") {
			t.Fatalf("lucide: override should be an ico glyph span: %s", got)
		}
		if strings.Contains(got, "kind-tile") {
			t.Fatalf("override must win over the tier2 monogram: %s", got)
		}
	})

	t.Run("tier 3 — bare name renders a bundled glyph span", func(t *testing.T) {
		got := string(KindIcon("Topic", "redpanda.com", true, "rotate-cw"))
		if !strings.Contains(got, `class="ico sm"`) || !strings.Contains(got, "<svg") {
			t.Fatalf("bare-name override should be an ico glyph span: %s", got)
		}
	})

	t.Run("tier 3 — emoji: renders an escaped kind-emoji span", func(t *testing.T) {
		got := string(KindIcon("Topic", "redpanda.com", true, "emoji:🐙"))
		if !strings.Contains(got, `class="kind-emoji"`) || !strings.Contains(got, "🐙") {
			t.Fatalf("emoji override should be a kind-emoji span: %s", got)
		}
	})

	t.Run("tier 3 — local .svg path renders a kind-img", func(t *testing.T) {
		got := string(KindIcon("Cluster", "postgresql.cnpg.io", true, "/icons/pg.svg"))
		if !strings.Contains(got, `class="kind-img"`) || !strings.Contains(got, `src="/icons/pg.svg"`) {
			t.Fatalf("local image override should be a kind-img: %s", got)
		}
	})

	t.Run("tier 3 — relative .png path renders a kind-img", func(t *testing.T) {
		got := string(KindIcon("Cluster", "postgresql.cnpg.io", true, "logo.png"))
		if !strings.Contains(got, `class="kind-img"`) || !strings.Contains(got, `src="logo.png"`) {
			t.Fatalf("relative image override should be a kind-img: %s", got)
		}
	})

	t.Run("tier 3 — remote http(s) image is REJECTED and falls back to the glyph", func(t *testing.T) {
		// CSP img-src 'self' data: would silently fail a remote <img>; the
		// resolver must treat a remote URL as an invalid override and fall back.
		for _, remote := range []string{
			"https://evil.example/x.svg",
			"http://evil.example/x.png",
			"//evil.example/x.png",       // protocol-relative
			"HTTPS://evil.example/x.svg", // case-insensitive scheme
			"javascript:alert(1)//x.svg", // scheme that ends .svg
			"data:image/svg+xml,<svg/>",  // data: scheme is not a local asset
		} {
			got := string(KindIcon("Certificate", "cert-manager.io", true, remote))
			if strings.Contains(got, "<img") || strings.Contains(got, remote) {
				t.Fatalf("remote image override %q must NOT emit an <img>: %s", remote, got)
			}
			// falls back to the resolved tier2a glyph for cert-manager.io.
			if !strings.Contains(got, "kind-curated") {
				t.Fatalf("rejected remote override must fall back to the resolved glyph: %s", got)
			}
		}
	})

	t.Run("SECURITY — malicious kind/group/override is escaped or rejected", func(t *testing.T) {
		// kind controls the monogram label; group controls the (escaped) data
		// attribute; both come from the cluster (anyone who can create a CRD).
		evilKind := `<script>x` // monogram would echo kindAbbrev, but the raw kind must never appear
		evilGroup := `"></span><script>alert(1)</script>`
		got := string(KindIcon(evilKind, evilGroup, true, ""))
		assertNoInjection(t, "tile kind/group", got)

		// emoji override carrying a tag must be escaped, not echoed raw.
		gotEmoji := string(KindIcon("Topic", "redpanda.com", true, `emoji:</span><script>alert(1)</script>`))
		assertNoInjection(t, "emoji override", gotEmoji)

		// image override that tries to break out of the src attribute.
		gotImg := string(KindIcon("Topic", "redpanda.com", true, `/x.svg" onerror="alert(1)`))
		assertNoInjection(t, "image override", gotImg)
		if strings.Contains(gotImg, `onerror="alert(1)"`) {
			t.Fatalf("image override broke out of the src attribute: %s", gotImg)
		}

		// javascript: scheme is not a local image (no leading / and no .svg/.png
		// suffix), so it must NOT become an <img>; it degrades to a glyph lookup
		// (icons.SVG never echoes an unknown name).
		gotJS := string(KindIcon("Topic", "redpanda.com", true, "javascript:alert(1)"))
		if strings.Contains(gotJS, "<img") || strings.Contains(gotJS, "javascript:") {
			t.Fatalf("javascript: override must not produce an <img> or echo the scheme: %s", gotJS)
		}
	})
}

// assertNoInjection fails if a runtime-derived raw markup fragment leaked an
// unescaped tag opener that could break the surrounding document (the resolver
// output is emitted raw AND embedded in Unit 3's palette JSON).
func assertNoInjection(t *testing.T, label, markup string) {
	t.Helper()
	for _, bad := range []string{"<script", "</span><script", `onerror="alert`} {
		if strings.Contains(markup, bad) {
			t.Fatalf("%s leaked unescaped %q: %s", label, bad, markup)
		}
	}
}

// compile-time assertion that the documented public shape exists.
var (
	_ func(kind, group string, isCRD bool, override string) template.HTML = KindIcon
	_ func(string) int                                                    = HashHue
	_ func(string) string                                                 = KindAbbrev
)
