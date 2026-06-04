package icons

// kindicons.go — the server-side icon SYSTEM, a byte-faithful port of
// design/assets/kindicons.js. It resolves a navigable resource's
// {kind, group, isCRD, override} to one of:
//
//   Tier 3 (override)   operator-pinned glyph / emoji / local image
//   Tier 1 (built-in)   a curated Lucide glyph for a known non-CRD kind
//   Tier 2a (CRD family) a semantic glyph tinted with the API-group hue
//   Tier 2b (monogram)  a 2-letter tile whose hue is keyed on the API GROUP
//
// Resolution order is 3 -> 1 -> (CustomResourceDefinition special) -> 2a -> 2b,
// matching kindIcon() in the JS reference so colours match the mockups.
//
// SECURITY. kind and group come from the cluster (anyone able to create a CRD
// controls those strings) and the override comes from operator config. The
// returned markup is emitted RAW (like icons.SVG) and is additionally embedded
// in the command-palette JSON, so every runtime-derived fragment that reaches
// the output is HTML-escaped (label text, the data-group attribute, the emoji,
// the image src) and the image scheme is restricted to local assets. The hue is
// an int from HashHue (safe) and the glyph SVGs are build-time-constant
// (icons.SVG never echoes an unknown name — it falls back to a neutral shape).

import (
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

// kindIconGlyph maps a Kubernetes Kind to a curated glyph name in icons.SVG.
// Tier 1: consulted only when !isCRD. Ported verbatim from KIND_ICON in
// design/assets/kindicons.js (33 entries).
var kindIconGlyph = map[string]string{
	"Pod":                      "pod",
	"Deployment":               "deployment",
	"ReplicaSet":               "replicaset",
	"StatefulSet":              "statefulset",
	"DaemonSet":                "daemonset",
	"Job":                      "job",
	"CronJob":                  "cronjob",
	"Service":                  "service",
	"Ingress":                  "ingress",
	"Endpoints":                "service",
	"EndpointSlice":            "service",
	"NetworkPolicy":            "networkpolicy",
	"IngressClass":             "ingress",
	"ConfigMap":                "configmap",
	"Secret":                   "secret",
	"Namespace":                "namespace",
	"Node":                     "node",
	"PersistentVolume":         "persistentvolume",
	"PersistentVolumeClaim":    "persistentvolumeclaim",
	"StorageClass":             "storageclass",
	"VolumeAttachment":         "persistentvolume",
	"CSIDriver":                "storageclass",
	"ServiceAccount":           "serviceaccount",
	"Role":                     "role",
	"ClusterRole":              "role",
	"RoleBinding":              "rolebinding",
	"ClusterRoleBinding":       "rolebinding",
	"Event":                    "event",
	"HorizontalPodAutoscaler":  "hpa",
	"ResourceQuota":            "resourcequota",
	"LimitRange":               "sliders",
	"PriorityClass":            "priorityclass",
	"PodDisruptionBudget":      "networkpolicy",
	"CustomResourceDefinition": "crd",
}

// kindAbbrevOverride is the small common-kind abbreviation table consulted
// before the generic capitals/first-two rule. Ported verbatim from KIND_ABBREV
// (15 entries).
var kindAbbrevOverride = map[string]string{
	"Pod":         "PO",
	"Service":     "SV",
	"ConfigMap":   "CM",
	"Secret":      "SE",
	"Deployment":  "DE",
	"ReplicaSet":  "RS",
	"StatefulSet": "ST",
	"DaemonSet":   "DS",
	"Namespace":   "NS",
	"Node":        "ND",
	"Ingress":     "IN",
	"CronJob":     "CJ",
	"Cluster":     "CL",
	"Job":         "JO",
	"Event":       "EV",
}

// crdGroupGlyph maps a known operator API group to a semantic glyph name.
// Tier 2a. Ported verbatim from CRD_GROUP_ICON (13 entries); the regex families
// below cover suffix groups (*.fluxcd.io, *.gatekeeper.sh).
var crdGroupGlyph = map[string]string{
	"cert-manager.io":                "cert",
	"trust.cert-manager.io":          "cert",
	"acme.cert-manager.io":           "cert",
	"cilium.io":                      "mesh",
	"argoproj.io":                    "rollout",
	"operator.victoriametrics.com":   "chart",
	"monitoring.coreos.com":          "scope",
	"external-secrets.io":            "vault",
	"generators.external-secrets.io": "vault",
	"keda.sh":                        "hpa",
	"eventing.keda.sh":               "hpa",
	"postgresql.cnpg.io":             "statefulset",
	"gateway.networking.k8s.io":      "ingress",
}

var (
	fluxcdGroup     = regexp.MustCompile(`(^|\.)fluxcd\.io$`)
	gatekeeperGroup = regexp.MustCompile(`(^|\.)gatekeeper\.sh$`)
	capitalLetter   = regexp.MustCompile(`[A-Z]`)
	imageSuffix     = regexp.MustCompile(`(?i)\.(svg|png)$`)
	// A leading URL scheme (`scheme:` before any `/`) or a protocol-relative
	// `//host`. Used to keep only local asset paths in the <img> branch.
	nonLocalImageRef = regexp.MustCompile(`(?i)^([a-z][a-z0-9+.-]*:|//)`)
)

// HashHue returns a deterministic hue in [0,360) for a string (the API group),
// byte-faithful to the JS `hashHue`: h = (h*31 + charCode) >>> 0 over the
// string's UTF-16/Unicode code units, then h % 360. Go's uint32 wraparound
// reproduces the JS `>>> 0` coercion exactly (h*31 < 2^37, within float64's
// exact-integer range, so no rounding diverges).
func HashHue(s string) int {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c)
	}
	return int(h % 360)
}

// KindAbbrev returns the 2-letter monogram label for a Kind: the override table
// first, else the first two capital letters when there are >=2, else the first
// two letters uppercased. Byte-faithful to the JS `kindAbbrev`, including the
// `slice(0,2)` clamp on short strings (a 1-rune Kind yields a 1-char label
// rather than panicking).
func KindAbbrev(kind string) string {
	if a, ok := kindAbbrevOverride[kind]; ok {
		return a
	}
	caps := capitalLetter.FindAllString(kind, -1)
	if len(caps) >= 2 {
		return caps[0] + caps[1]
	}
	runes := []rune(kind)
	if len(runes) > 2 {
		runes = runes[:2]
	}
	return strings.ToUpper(string(runes))
}

// groupGlyph returns the curated Tier-2a glyph name for an API group, or "" if
// the group is not a known operator family. Byte-faithful to the JS
// `groupIcon`, including the fluxcd/gatekeeper suffix regex fallback.
func groupGlyph(group string) string {
	if group == "" {
		return ""
	}
	if g, ok := crdGroupGlyph[group]; ok {
		return g
	}
	if fluxcdGroup.MatchString(group) {
		return "gitops"
	}
	if gatekeeperGroup.MatchString(group) {
		return "role"
	}
	return ""
}

// KindIcon resolves {kind, group, isCRD, override} to trusted-shape markup for
// the entry's icon. The markup is emitted raw by callers (like icons.SVG); all
// runtime-derived parts are escaped per the package SECURITY note. Returns
// template.HTML so the templ call sites can @templ.Raw it directly.
func KindIcon(kind, group string, isCRD bool, override string) template.HTML {
	// Tier 3 — operator override. A rejected remote-image override returns
	// handled=false so resolution falls through to the built-in tiers.
	if markup, handled := overrideIcon(override); handled {
		return markup
	}
	// Tier 1 — built-in kind glyph (only for non-CRDs).
	if !isCRD {
		if g, ok := kindIconGlyph[kind]; ok {
			return glyphSpan("ico sm", g)
		}
	}
	// CustomResourceDefinition itself keeps the puzzle glyph even though it is
	// served from a CRD-bearing group.
	if kind == "CustomResourceDefinition" {
		return glyphSpan("ico sm", "crd")
	}
	// Tier 2a — curated CRD-family glyph, tinted with the group hue.
	if g := groupGlyph(group); g != "" {
		return curatedGlyphSpan(g, HashHue(group))
	}
	// Tier 2b — monogram tile keyed on the API group (kind only when group is
	// empty), so a CRD family shares a hue.
	return MonogramTile(kind, group)
}

// MonogramTile renders the Tier-2b monogram: a 2-letter tile whose hue is
// HashHue(group) (or HashHue(kind) when group is empty). The label text and the
// data-group attribute are HTML-escaped; --kh is an int. Byte-faithful to the
// JS `monogramTile` (sans the large-variant opt, which callers don't use here).
func MonogramTile(kind, group string) template.HTML {
	hue := HashHue(firstNonEmpty(group, kind))
	label := html.EscapeString(KindAbbrev(kind))
	dataGroup := html.EscapeString(group)
	return template.HTML(`<span class="kind-tile" style="--kh:` + itoa(hue) +
		`" data-group="` + dataGroup + `">` + label + `</span>`)
}

// MetaGlyph returns the sidebar icon-slot markup for a non-resource Meta nav
// entry (the Resource Types / Events links the layout adds under the "Meta"
// group). It maps the known Meta labels to a curated chrome glyph and falls
// through to a neutral glyph for anything else, wrapped in the same `.ico sm`
// span the curated kind glyphs use so the sidebar row shape is uniform. The
// label is a build-time-constant layout string (not user data) and the glyph
// name is looked up in icons.SVG (a constant switch), so the markup is
// trusted-shape and may be emitted raw.
func MetaGlyph(label string) template.HTML {
	switch label {
	case "Resource Types":
		return glyphSpan("ico sm", "table")
	case "Events":
		return glyphSpan("ico sm", "event")
	default:
		return glyphSpan("ico sm", "")
	}
}

// PluralMonogram renders a deterministic monogram keyed on a resource-type
// plural, for the no-discovery sidebar fallback: when the cluster manager is
// absent or the cluster is unknown, sidebarResourceLink returns only a plural
// (no kube.ResourceType), so the resolver has no kind/group. Keying hue + label
// on the plural keeps the slot deterministic and non-empty (never a blank icon).
func PluralMonogram(plural string) template.HTML {
	hue := HashHue(plural)
	label := html.EscapeString(KindAbbrev(plural))
	dataGroup := html.EscapeString(plural)
	return template.HTML(`<span class="kind-tile" style="--kh:` + itoa(hue) +
		`" data-group="` + dataGroup + `">` + label + `</span>`)
}

// overrideIcon parses a Tier-3 override string into markup, returning
// handled=false when there is no usable override (empty, or a remote-image URL
// that the server CSP `img-src 'self' data:` would silently block — treated as
// invalid so the caller falls back to the resolved glyph rather than emitting a
// dead <img>). Resolution order mirrors the JS `kindIcon` override branch:
// lucide: prefix, emoji: prefix, local image (path/suffix), else bare glyph
// name. Remote http(s):// is the one deliberate divergence from the JS (which
// renders it as an <img>): readout drops it.
func overrideIcon(override string) (template.HTML, bool) {
	if override == "" {
		return "", false
	}
	if name, ok := strings.CutPrefix(override, "lucide:"); ok {
		return glyphSpan("ico sm", name), true
	}
	if ch, ok := strings.CutPrefix(override, "emoji:"); ok {
		return template.HTML(`<span class="kind-emoji">` + html.EscapeString(ch) + `</span>`), true
	}
	if isNonLocalImageRef(override) {
		// Anything carrying a URL scheme (http:, https:, javascript:, data:, …)
		// or protocol-relative (//host): not a local asset. The server CSP is
		// img-src 'self' data:, so a remote ref is dead markup — reject it and
		// fall back to the glyph rather than emit a broken/contract-violating <img>.
		return "", false
	}
	if imageSuffix.MatchString(override) || strings.HasPrefix(override, "/") {
		return template.HTML(`<img class="kind-img" src="` + html.EscapeString(override) + `" alt="">`), true
	}
	// Bare name = bundled glyph. icons.SVG never echoes an unknown name, so an
	// arbitrary string here degrades to a neutral shape, not injected markup.
	return glyphSpan("ico sm", override), true
}

// isNonLocalImageRef reports whether s is anything other than a local asset
// path: it carries a URL scheme (http:, https:, javascript:, data:, vbscript:,
// any `scheme:` before the first slash, case-insensitive) or is protocol-relative
// (`//host`). Only local paths may become an <img>, because the server CSP is
// img-src 'self' data: — a remote/schemed ref would be blocked and render as a
// dead image, so the resolver drops it and falls back to the glyph.
func isNonLocalImageRef(s string) bool {
	return nonLocalImageRef.MatchString(s)
}

// glyphSpan wraps a build-time-constant icons.SVG glyph in a span with the
// given class. name is looked up in icons.SVG (a constant switch), so even an
// attacker-chosen name cannot inject markup — it falls back to a neutral shape.
func glyphSpan(class, name string) template.HTML {
	return template.HTML(`<span class="` + class + `">` + SVG(name) + `</span>`)
}

// curatedGlyphSpan wraps a Tier-2a glyph in a `.kind-curated` span tinted by the
// group hue. hue is an int (safe); name is a constant glyph key.
func curatedGlyphSpan(name string, hue int) template.HTML {
	return template.HTML(`<span class="ico sm kind-curated" style="--kh:` + itoa(hue) + `">` + SVG(name) + `</span>`)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// itoa renders the hue int for the --kh custom property. The value always comes
// from HashHue (0..359), so it is digits-only and safe in attribute context.
func itoa(i int) string {
	return strconv.Itoa(i)
}
