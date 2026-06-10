package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
)

func TestFormattingHelpers(t *testing.T) {
	if humanTitle("app.kubernetes_io-name") != "App Kubernetes Io Name" {
		t.Fatalf("humanTitle mismatch")
	}
	if capitalizeWord("eventTime") != "Eventtime" || capitalizeWord("reportingComponent") != "Reportingcomponent" || capitalizeWord("") != "" {
		t.Fatalf("capitalizeWord mismatch")
	}
	if pluralizeKind("Policy") != "Policies" || pluralizeKind("Ingress") != "Ingresses" || pluralizeKind("Pod") != "Pods" {
		t.Fatalf("pluralizeKind mismatch")
	}
	if createdSortParam("Created") != "Created:desc" || createdSortParam("") != "Created" || pluralS(1) != "" || pluralS(2) != "s" {
		t.Fatalf("sort/plural helpers mismatch")
	}
	if namespaceEmptyText("prod", false) != `in namespace "prod" ` || namespaceEmptyText("prod", true) != "" {
		t.Fatalf("namespaceEmptyText mismatch")
	}
	if truncate("abcdef", 5) != "abcdef" || truncate("abcdefghijkl", 5) != "ab..." || truncate("abcdefgh", 2) != "ab" || truncate("abc", 5) != "abc" {
		t.Fatalf("truncate mismatch")
	}
	if got := truncate("Contains a CA bundle that can be used to verify the kube-apiserver when using internal endpoints.", 40); got != "Contains a CA bundle that can be..." {
		t.Fatalf("truncate should break on the last word boundary: %q", got)
	}
	if cellDisplayString(nil) != "" || cellString(kube.Row{Cells: []any{nil}}, 0) != "" || cellString(kube.Row{Cells: []any{"ok"}}, 0) != "ok" {
		t.Fatalf("cell display string mismatch")
	}
	if !strings.Contains(commandPalette(), `id="ro-palette"`) || !strings.Contains(commandPalette(), `id="ro-palette-list"`) || strings.Contains(commandPalette(), `ro-palette-row-tmpl`) || !strings.Contains(icon("missing"), "<circle") {
		t.Fatalf("command palette/icon fallback mismatch")
	}
}

func TestTableCellFormattingHelpers(t *testing.T) {
	table := kube.Table{Resource: kube.ResourceType{Plural: "pods"}, Columns: []kube.Column{{Name: "Name"}, {Name: "Status"}, {Name: "CPU Usage"}, {Name: "Memory Usage"}}}
	if cellClass(&table, 1, "Running") != "has-text-success" || cellClass(&table, 1, "Completed") != "has-text-info" || cellClass(&table, 1, "ImagePullBackOff") != "has-text-danger" || cellClass(&table, 1, "Pending") != "has-text-warning" {
		t.Fatalf("cellClass mismatch")
	}
	if cellClass(&table, -1, "x") != "" || readyRatioClass("0/2") != "zero" || readyRatioClass("2/2") != "full" || readyRatioClass("1/2") != "partial" || readyRatioClass("ready") != "" {
		t.Fatalf("ready/cell class bounds mismatch")
	}
	if cpuFormat(json.Number("0.25")) != "250m" || cpuFormat("bad") != "bad" {
		t.Fatalf("cpuFormat mismatch")
	}
	if memoryMiBFormat(float64(2*1024*1024)) != "2" || memoryMiBFormat("bad") != "bad" {
		t.Fatalf("memoryMiBFormat mismatch")
	}
	// node memory capacity must read in a binary unit, not the raw "8138032Ki".
	if humanBytes(8333344768) != "7.8 GiB" || humanBytes(2*1024*1024) != "2 MiB" || humanBytes(512) != "512 B" || humanBytes(1024) != "1 KiB" {
		t.Fatalf("humanBytes mismatch: %q %q %q %q", humanBytes(8333344768), humanBytes(2*1024*1024), humanBytes(512), humanBytes(1024))
	}
	if got, ok := numericCell(int64(3)); !ok || got != 3 {
		t.Fatalf("numericCell int64 = %v %v", got, ok)
	}
	if got, ok := numericCell("3.5"); !ok || got != 3.5 {
		t.Fatalf("numericCell string = %v %v", got, ok)
	}
	if got, ok := numericCell(json.Number("bad")); ok || got != 0 {
		t.Fatalf("numericCell bad json.Number = %v %v", got, ok)
	}
	if got, ok := numericCell("bad"); ok || got != 0 {
		t.Fatalf("numericCell bad = %v %v", got, ok)
	}
}

func TestThemeAndURLHelpers(t *testing.T) {
	cfg := config.Config{DefaultTheme: "solarized", ThemeOptions: []string{"light", "dark"}}
	req := httptest.NewRequest("GET", "/x?theme=dark", nil)
	if theme(req, &cfg) != "light" || !themeExplicit(req) {
		t.Fatalf("theme fallback/query explicit mismatch")
	}
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	if theme(req, &cfg) != "dark" || !themeExplicit(req) || allowedTheme("", &cfg) {
		t.Fatalf("theme cookie mismatch")
	}
	if !reflect.DeepEqual(themeOptions(&config.Config{}), []string{"dark", "light"}) {
		t.Fatalf("default theme options mismatch")
	}
	u, _ := url.Parse("/clusters/test/pods?sort=Name")
	if got := addQuery(u, "sort", "Age"); got != "/clusters/test/pods?sort=Age" {
		t.Fatalf("addQuery = %q", got)
	}
	r := httptest.NewRequest("GET", "/clusters/test/pods?sort=Name", nil)
	if got := partialResourceListURL(r); got != "/clusters/test/pods/_table?sort=Name" {
		t.Fatalf("partialResourceListURL = %q", got)
	}
	tableURL, _ := url.Parse("/clusters/test/pods/_table?sort=Name")
	if got := addQuery(resourceListBaseURL(tableURL), "download", "tsv"); got != "/clusters/test/pods?download=tsv&sort=Name" {
		t.Fatalf("resourceListBaseURL download = %q", got)
	}
}

func TestIsLocalRedirect(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"/", true},
		{"/clusters/test", true},
		{"//evil.example", false},
		{`/\evil.example`, false},
		{"https://evil.example", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLocalRedirect(tc.target); got != tc.want {
			t.Fatalf("isLocalRedirect(%q) = %t, want %t", tc.target, got, tc.want)
		}
	}
}

func TestDataExtractionHelpers(t *testing.T) {
	labels := map[string]string{"b": "2", "a": "1"}
	if formatLabels(labels) != "a=1,b=2" || first("", "x", "y") != "x" {
		t.Fatalf("formatLabels/first mismatch")
	}
	if got := truncate(`{"dobs.csi.digitalocean.com":"571970771"}`, 40); !strings.Contains(got, "571970771") {
		t.Fatalf("truncate leeway mismatch: %q", got)
	}
	if got := firstSlice([]int{}, []int{1, 2}); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("firstSlice fallback = %#v", got)
	}
	row := kube.Row{Cells: []any{"nginx", "Running"}}
	table := kube.Table{Columns: []kube.Column{{Name: "Other"}, {Name: "Name"}}}
	if nameColumn(&table) != 1 || cellString(row, 1) != "Running" || cellString(row, 9) != "" {
		t.Fatalf("name/cell helpers mismatch")
	}
	rt := kube.ResourceType{Plural: "pods", Namespaced: true}
	if resourceHref("c", &rt, "ns", "pod/name") != "/clusters/c/namespaces/ns/pods/pod%2Fname" {
		t.Fatalf("resourceHref namespaced mismatch")
	}
	rt.Namespaced = false
	if resourceHref("c", &rt, "", "pod") != "/clusters/c/pods/pod" {
		t.Fatalf("resourceHref cluster mismatch")
	}
	obj := map[string]any{"metadata": map[string]any{"name": "nginx", "labels": map[string]any{"app": "nginx", "bad": 1}}}
	if nestedString(obj, "metadata", "name") != "nginx" || nestedString(obj, "missing") != "" || nestedString(obj, "metadata", "labels", "app") != "nginx" {
		t.Fatalf("nestedString helper mismatch")
	}
}

func TestSecretSearchAndSelectorHelpers(t *testing.T) {
	secret := map[string]any{"data": map[string]any{"password": "plain"}}
	maskSecret(secret)
	if secret["data"].(map[string]any)["password"] != kube.SecretContentHidden || secret["metadata"] == nil {
		t.Fatalf("maskSecret = %#v", secret)
	}
	pod := map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}, "initContainers": []any{map[string]any{"name": "init"}}}}
	if got := containerNames(pod); !reflect.DeepEqual(got, []string{"app", "init"}) {
		t.Fatalf("containerNames = %#v", got)
	}
	selector, filter := splitSearchQuery("app=api prod text")
	if selector != "app=api" || filter != "prod text" {
		t.Fatalf("splitSearchQuery = %q %q", selector, filter)
	}
	results := []searchResult{
		{Title: "beta", Link: "/b", Labels: map[string]string{"app": "api"}},
		{Title: "api", Link: "/a"},
		{Title: "alpha", Link: "/c"},
	}
	sortResults(results, "api")
	if results[0].Title != "api" || searchScore("xxapi", nil, "api") != 2 || searchScore("x", map[string]string{"app": "api"}, "api") != 1 {
		t.Fatalf("search ranking mismatch: %#v", results)
	}
	deploy := map[string]any{"spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "api"}}}}
	if matchLabels(deploy)["app"] != "api" || selectorString(map[string]string{"b": "2", "a": "1"}) != "a=1,b=2" {
		t.Fatalf("selector helpers mismatch")
	}
}

// TestTruncateRuneSafety pins the rune-safe annotation-value truncation: a value
// of multi-byte runes longer than max must be cut on a rune boundary (never
// mid-rune), so the result stays valid UTF-8.
func TestTruncateRuneSafety(t *testing.T) {
	value := strings.Repeat("漢", 60) // 60 CJK runes, 180 bytes
	got := truncate(value, 40)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncate(%d-rune value, 40) = %q, want a ... ellipsis", utf8.RuneCountInString(value), got)
	}
	// The kept prefix (sans the "...") is <= max runes and is a prefix of value.
	prefix := strings.TrimSuffix(got, "...")
	if n := utf8.RuneCountInString(prefix); n > 40 {
		t.Fatalf("truncate kept %d runes, want <= 40", n)
	}
	if !strings.HasPrefix(value, prefix) {
		t.Fatalf("truncate prefix %q is not a prefix of the original", prefix)
	}
	// A short multi-byte value is returned unchanged.
	if got := truncate("漢字", 40); got != "漢字" {
		t.Fatalf("truncate(short) = %q, want 漢字", got)
	}
}

// TestAgeClassThresholds pins the clock and walks one representative age per
// bucket plus the boundary edges, so every one of the five age-* classes is
// genuinely exercised. The bucket structure (boundaries 0.10/0.35/0.65/1.0,
// strict less-than, and the -60s "last minute counts as zero" floor) is pinned
// here so a render change that miscolors an age has to break one of these rows.
//
// Window: every template call site (resource-view, cluster,
// resource-list-content, events) renders with a 1-day window, matching this
// code's 86400s denominator. Do NOT change the denominator.
func TestAgeClassThresholds(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	srv := &Server{now: func() time.Time { return now }}

	at := func(age time.Duration) string {
		return now.Add(-age).UTC().Format(time.RFC3339)
	}

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty is old", "", "age-old"},
		{"unparseable is old", "not-time", "age-old"},
		// -60s floor: anything within the last minute counts as age zero -> fresh.
		{"30s ago hits the -60s floor (fresh)", at(30 * time.Second), "age-fresh"},
		{"exactly 60s ago is the -60s floor (fresh)", at(60 * time.Second), "age-fresh"},
		// One representative strictly inside each bucket.
		{"1h -> fresh (frac 0.041)", at(time.Hour), "age-fresh"},
		{"5h -> recent (frac 0.208)", at(5 * time.Hour), "age-recent"},
		{"12h -> day (frac 0.499)", at(12 * time.Hour), "age-day"},
		{"20h -> week (frac 0.833)", at(20 * time.Hour), "age-week"},
		{"48h -> old (frac clamps >= 1.0)", at(48 * time.Hour), "age-old"},
		// Exact boundary behaviour: strict `<` means the boundary value falls
		// into the NEXT (older) bucket.
		{"fraction exactly 0.10 -> recent not fresh", at(8700 * time.Second), "age-recent"},
		{"fraction just under 1.0 -> week not old", at(86459 * time.Second), "age-week"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.ageClass(tc.value); got != tc.want {
				t.Fatalf("ageClass(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
