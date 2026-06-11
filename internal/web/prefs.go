package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// prefs.go is the server READ side of the `ro_prefs` preference cookie (D9):
// one compact cookie carrying column visibility per plural, sort per plural,
// the auto-refresh mode, and a last-used namespace per cluster. The cookie is
// WRITTEN exclusively by readout.js (document.cookie on direct user
// interactions: sort click, column toggle, interval pick, namespace switch;
// attributes Path=/; SameSite=Lax; Max-Age=31536000, Secure on https, never
// HttpOnly) -- the server never sets it, so the read-only edge gains no POST
// route. The Go encoder below exists as the canonical wire-format + eviction
// reference (mirrored by readout.js) and for the test net.
//
// Wire format (pinned): `ro_prefs=v1.<base64url(JSON)>`. Raw JSON in a cookie
// value is unsafe -- column names like "Nominated Node" carry spaces, and JSON
// itself carries quotes/commas that cookie-value octets exclude -- so the JSON
// payload travels base64url-encoded (RawURLEncoding: URL-safe alphabet, no
// padding) behind a `v1.` version tag. Anything that does not decode is
// treated as no preferences at all: a corrupt cookie must never 500 a page.
//
// SPEC §8.4's "last cluster" is deliberately NOT in this schema -- a recorded
// SPEC deviation (D9): no consumer exists for it, and a write-only field
// invites invented redirect semantics. The theme keeps its own `theme` cookie
// (handlers_prefs.go) untouched.
const (
	prefsCookieName    = "ro_prefs"
	prefsVersionPrefix = "v1."
	// prefsMaxEncoded is the cap on the encoded cookie VALUE ("v1." + payload):
	// above it, kind entries evict from the array tail (least recently used --
	// the array is most-recent-first, no timestamps needed). The realistic
	// worst-case payload is ~1.2KB encoded (scan-log accepted-risk-probe
	// prefs-cookie-size), so 3KB leaves headroom under the 4KB browser limit.
	prefsMaxEncoded = 3072
)

// prefs is the decoded ro_prefs payload. The json tags ARE the pinned wire
// contract; readout.js writes exactly this shape.
type prefs struct {
	// Kinds holds the per-resource-type entries, most-recent-first: the JS
	// writer moves an entry to the front on every write, so tail eviction drops
	// the least recently used kind.
	Kinds []kindPrefs `json:"kinds,omitempty"`
	// Refresh is the auto-refresh mode as a STRING: "Off", an interval in
	// seconds ("5"/"10"/"30"/"60"...), or the future "Live" (Unit 27). Stored
	// stringly so Live needs no schema change. "" means no preference.
	Refresh string `json:"refresh,omitempty"`
	// Namespaces maps cluster name -> last-used namespace ("_all" is a valid
	// persistable value). Consumed ONLY for cluster-entry href construction
	// (clusterEntryHref) -- never for redirects, never on direct URL loads.
	Namespaces map[string]string `json:"ns,omitempty"`
}

// kindPrefs is one per-plural entry: the persisted sort param and the hidden
// column set for that resource type's list.
type kindPrefs struct {
	Plural string `json:"k"`
	// Sort is a kube.SortTable param ("Name", "Status:desc", "Created"...).
	Sort string `json:"sort,omitempty"`
	// Hide is the hidden-column-name list. A POINTER so "no column preference"
	// (nil -> the config DefaultHiddenColumns default applies) is distinct from
	// an explicit "hide nothing" ([] -> the user toggled everything visible,
	// which must SUPPRESS the config default -- user override wins, D8). A
	// plain slice could not round-trip that difference through omitempty.
	Hide *[]string `json:"hide,omitempty"`
}

// decodePrefs parses a cookie value. It is deliberately lenient: a missing
// version tag, broken base64, or non-JSON payload yields zero prefs (and
// ok=false), never an error -- the page must render as if no preferences
// existed, exactly like the JS reader.
//
// NOTE the all-or-nothing grain: json.Unmarshal rejects the WHOLE payload when
// any single field is mistyped (e.g. {"kinds":[{"k":"pods","sort":5}]}), so
// one bad field silently disables every preference at SSR. The JS reader
// (readout.js readPrefs) mirrors this by type-checking each inner field and
// DROPPING the mistyped ones -- the next JS write re-encodes a clean cookie,
// which is what keeps such a cookie from staying SSR-invisible forever.
func decodePrefs(value string) (prefs, bool) {
	payload, found := strings.CutPrefix(value, prefsVersionPrefix)
	if !found || payload == "" {
		return prefs{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return prefs{}, false
	}
	var p prefs
	if err := json.Unmarshal(raw, &p); err != nil {
		return prefs{}, false
	}
	return p, true
}

// encodePrefs renders the canonical cookie value, evicting kind entries from
// the array TAIL while the encoded value exceeds prefsMaxEncoded (the entries
// are most-recent-first, so the least recently used kinds drop first --
// deterministic, no timestamps). It never mutates the caller's slice. This is
// the reference implementation of the writer mechanics readout.js mirrors.
func encodePrefs(p prefs) string {
	kinds := p.Kinds
	for {
		clone := p
		clone.Kinds = kinds
		// HTML escaping is DISABLED below: json.Marshal escapes < > & to
		// the < > & forms, but JS JSON.stringify (the writer this
		// mirrors) leaves them literal -- escaping would diverge the two codecs
		// byte-for-byte on CRD column names carrying those characters. No
		// escaping is needed: the JSON is wrapped in base64url and never reaches
		// an HTML context. The Encoder appends a trailing newline that the wire
		// format excludes, so trim it before base64-encoding.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(&clone); err != nil {
			return "" // unreachable for this plain struct; degrade to no cookie
		}
		raw := bytes.TrimRight(buf.Bytes(), "\n")
		value := prefsVersionPrefix + base64.RawURLEncoding.EncodeToString(raw)
		if len(value) <= prefsMaxEncoded || len(kinds) == 0 {
			return value
		}
		kinds = kinds[:len(kinds)-1]
	}
}

// prefsFromRequest reads the ro_prefs cookie off the request; no cookie or an
// undecodable one yields zero prefs.
func prefsFromRequest(r *http.Request) prefs {
	cookie, err := r.Cookie(prefsCookieName)
	if err != nil {
		return prefs{}
	}
	p, _ := decodePrefs(cookie.Value)
	return p
}

// kind returns the entry for a plural, or nil when the cookie carries none.
func (p *prefs) kind(plural string) *kindPrefs {
	for i := range p.Kinds {
		if p.Kinds[i].Plural == plural {
			return &p.Kinds[i]
		}
	}
	return nil
}

// isHistoryRestoreRequest reports whether this request is htmx re-fetching a
// page for a history (back/forward) restore after a cache miss. Those renders
// must be URL-explicit for URL-REPRESENTABLE state (D9): the back button is
// not defeated by a freshly written sort pref.
func isHistoryRestoreRequest(r *http.Request) bool {
	return r.Header.Get("HX-History-Restore-Request") == "true"
}

// prefsFill is the resolved D9 cookie fill for one list request: the values
// that stand in for ABSENT ?sort= / ?hidecols= URL params at SSR time. URL
// params always win; the fill is RENDER-ONLY state -- it never materializes
// into r.URL, so every rebuilt href (sort headers, metrics join, TSV) and the
// HX-Push-Url header keep carrying only what the user explicitly chose.
type prefsFill struct {
	// Sort fills an absent ?sort= ("" = nothing to fill). Empty on a history
	// restore: sort is URL-representable, so a back-render honours the URL.
	Sort string
	// Hide fills an absent ?hidecols= as a comma-joined list (the
	// kube.RemoveColumns spec format; k8s printer-column names never contain
	// commas). HasHide distinguishes "no column preference" from an explicit
	// empty hide set: the latter must SUPPRESS the DefaultHiddenColumns config
	// default (user override wins, D8). Column visibility has NO URL form, so
	// it stays filled even on a history restore -- stripping it would make a
	// back-render differ from a hard reload of the same URL.
	Hide    string
	HasHide bool
}

// prefsListFill resolves the cookie fill for a list request. Single-type pages
// only (the D1 surface boundary, the same gate the interaction loop and `?f=`
// use): the write surfaces only exist there, and a page-wide sort fill on a
// multi-type page could not be reflected per-table.
func prefsListFill(r *http.Request) prefsFill {
	plural := r.PathValue("plural")
	if !isSingleListType(plural) {
		return prefsFill{}
	}
	p := prefsFromRequest(r)
	kp := p.kind(plural)
	if kp == nil {
		return prefsFill{}
	}
	var fill prefsFill
	if kp.Hide != nil {
		fill.HasHide = true
		fill.Hide = strings.Join(*kp.Hide, ",")
	}
	if !isHistoryRestoreRequest(r) {
		fill.Sort = kp.Sort
	}
	return fill
}

// clusterEntryHref is THE namespace-per-cluster consumer (D9, pinned): a
// cluster-entry link (the clusters page's rows, the palette's topbar cluster
// nav) points into the persisted namespace's pods list when one is recorded
// ("_all" included), else at the cluster overview. Pure link construction --
// never a redirect, never applied on direct URL loads, so deep links stay
// canonical, cluster-scoped kinds are unaffected, and a stale/deleted
// persisted namespace simply renders that namespace's normal empty list.
func clusterEntryHref(cluster, namespace string) string {
	if namespace == "" {
		return "/clusters/" + url.PathEscape(cluster)
	}
	return "/clusters/" + url.PathEscape(cluster) + "/namespaces/" + url.PathEscape(namespace) + "/pods"
}
