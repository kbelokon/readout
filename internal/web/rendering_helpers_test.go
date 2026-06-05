package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRenderingHelpersCoverBranches(t *testing.T) {
	cfg := config.Config{DefaultTheme: "bogus", ThemeOptions: []string{"light", "dark"}}
	req := httptest.NewRequest(http.MethodGet, "/?theme=dark", nil)
	if got := theme(req, &cfg); got != "light" || !themeExplicit(req) {
		t.Fatalf("theme/query = %q explicit=%t", got, themeExplicit(req))
	}
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	if got := theme(req, &cfg); got != "dark" {
		t.Fatalf("theme cookie = %q", got)
	}
	if allowedTheme("", &cfg) || !allowedTheme("light", &cfg) {
		t.Fatal("allowedTheme mismatch")
	}
	if activeClass(true) != " is-active" || activeClass(false) != "" || activeAttr(true) == "" || activeAttr(false) != "" {
		t.Fatal("active helper mismatch")
	}
	if truncate("abcdef", 3) != "abcdef" || truncate("abcdefghijkl", 5) != "ab..." || truncate("abc", 5) != "abc" {
		t.Fatal("truncate mismatch")
	}
	if !strings.Contains(icon("missing"), "<circle") {
		t.Fatal("default icon mismatch")
	}
	if pluralizeKind("Policy") != "Policies" || pluralizeKind("Ingress") != "Ingresses" || pluralizeKind("Pod") != "Pods" {
		t.Fatal("pluralizeKind mismatch")
	}
	if sortIcon("Name", "Name") == "" || sortIcon("Name:desc", "Name") == "" || sortIcon("Other", "Name") != "" {
		t.Fatal("sortIcon mismatch")
	}
	// ascending and descending must render DIFFERENT arrows: ascending carries the
	// sort-asc rotate class, descending does not.
	if !strings.Contains(sortIcon("Name", "Name"), "sort-asc") || strings.Contains(sortIcon("Name:desc", "Name"), "sort-asc") {
		t.Fatal("sortIcon direction: ascending must carry sort-asc, descending must not")
	}
	if createdSortParam("Created") != "Created:desc" || createdSortParam("") != "Created" || pluralS(1) != "" || pluralS(2) != "s" {
		t.Fatal("sort/plural helper mismatch")
	}
	if namespaceEmptyText("default", false) == "" || namespaceEmptyText("default", true) != "" {
		t.Fatal("namespaceEmptyText mismatch")
	}
	if appLabelClass("app.kubernetes.io/name") == "" || appLabelClass("team") != "" {
		t.Fatal("appLabelClass mismatch")
	}
	if cpuFormat(0.25) != "250m" || memoryMiBFormat(float64(2*1024*1024)) != "2" || cpuFormat("bad") != "bad" {
		t.Fatal("resource format mismatch")
	}
	if readyRatioClass("0/2") != "zero" || readyRatioClass("2/2") != "full" || readyRatioClass("1/2") != "partial" || readyRatioClass("x") != "" {
		t.Fatal("readyRatioClass mismatch")
	}
	if _, ok := numericCell(json.Number("3.5")); !ok {
		t.Fatal("json.Number should be numeric")
	}
	ageNow := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ageSrv := &Server{now: func() time.Time { return ageNow }}
	if ageSrv.ageClass("") != "age-old" || ageSrv.ageClass("not-time") != "age-old" || ageSrv.ageClass(ageNow.Add(time.Minute).Format(time.RFC3339)) != "age-fresh" {
		t.Fatal("ageClass mismatch")
	}
	if first("", "x") != "x" || firstSlice([]string{"x"}, []string{"y"})[0] != "x" || firstSlice([]string{}, []string{"y"})[0] != "y" {
		t.Fatal("first helpers mismatch")
	}
	u, _ := url.Parse("/clusters/test/namespaces/default/services")
	if got := addQuery(u, "sort", "Port(s)"); got != "/clusters/test/namespaces/default/services?sort=Port(s)" {
		t.Fatalf("addQuery parentheses = %q", got)
	}
}

// YAML serialization is now handled by internal/yamlview (sigs.k8s.io/yaml); the
// value-level intent -- top-level keys present, deterministic sorted-key output,
// and parse-equivalence -- is pinned hermetically by
// internal/yamlview/yaml_test.go.

// The Node summary DOM is produced by the templ resource-view +
// buildNodeSummaryView, and its contract (the Conditions / Capacity·Allocatable
// / System Info section labels, the Ready=True ro-st-ok pill bound to its
// condition, the "allocatable cpu" KV row, the kubeletVersion value) is pinned
// by named goquery facts in TestBehaviorNodeDetailFacts.

// The list/table render is exercised through templates.ResourceTable on the live
// render path:
//   - the per-cell table render (Cluster/Namespace cols, CPU/Memory, Node, empty
//     "No <Kind> objects" state, partial-results note) -> the goquery fact net
//     (TestBehaviorPodListFacts and the resource_table fanout facts);
//   - all five age-* buckets on a rendered table -> TestBehaviorClusterOverviewAgeBuckets
//     (live templ render) plus TestAgeClassThresholds (the
//     helper, with boundary fractions) in helpers_test.go;
//   - partialResourceListURL / cellString / nameColumn -> helpers_test.go.

func TestObjectRenderingLinksAndSearchHelpers(t *testing.T) {
	s := &Server{cfg: config.Config{
		ObjectLinks: map[string][]config.Link{"pods": {{Href: "https://obj/{cluster}/{namespace}/{name}", Title: "Obj {name}", Icon: "box"}}},
		LabelLinks:  map[string][]config.Link{"app": {{Href: "https://label/{label}/{label_value}", Title: "{labelValue}", Icon: "tag"}}},
		TimestampLinks: map[string][]config.Link{
			"pods": {{Href: "https://time/{cluster}/{namespace}/{name}/{timestamp}", Title: "At {timestamp}"}},
		},
	}}
	obj := kube.NewObject(&kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":              "nginx",
			"namespace":         "default",
			"creationTimestamp": "2026-01-02T03:04:05Z",
			"labels":            map[string]any{"app": "web"},
			"annotations":       map[string]any{"long": "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"},
		},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}, "startedAt": "2026-01-02T03:04:05Z"},
	}})
	// The resource-view DOM is produced by templ; its contract is pinned as named
	// goquery facts: the label chips + the Labels/Annotations sections in
	// TestBehaviorDetailLabelChips, the object breadcrumb in
	// TestBehaviorDetailBreadcrumb, the YAML-card sections + id scheme in
	// TestBehaviorPodDetailFacts / TestBehaviorPodYAMLViewIDScheme, the masked-Secret
	// block in TestBehaviorSecretBarrierMaskedOn, and the timestamp-link YAML
	// decoration in TestTimestampLinksDecorateYAML. What stays HERE is the
	// behavioral helper coverage (objectLinks/splitSearchQuery/
	// sortResults/matchLabels/selectorString), which is not render markup.
	links := s.objectLinks("test", "default", &obj)
	if len(links) != 2 || !strings.Contains(links[0].Href, "/test/default/nginx") || !strings.Contains(links[1].Href, "app/web") {
		t.Fatalf("object links = %#v", links)
	}

	selector, filter := splitSearchQuery("app=web nginx tier=api")
	if selector != "app=web,tier=api" || filter != "nginx" {
		t.Fatalf("splitSearchQuery selector=%q filter=%q", selector, filter)
	}
	results := []searchResult{{Title: "b", Link: "/b"}, {Title: "nginx", Link: "/a"}, {Title: "a", Link: "/z", Labels: map[string]string{"app": "nginx"}}}
	sortResults(results, "nginx")
	if results[0].Title != "nginx" || results[1].Title != "a" {
		t.Fatalf("sortResults = %#v", results)
	}
	results = []searchResult{{Title: "same", Link: "/b"}, {Title: "same", Link: "/a"}}
	sortResults(results, "")
	if results[0].Link != "/a" {
		t.Fatalf("sortResults link tie = %#v", results)
	}
	labels := matchLabels(map[string]any{"spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}}}})
	if selectorString(labels) != "app=web" {
		t.Fatalf("selectorString = %q labels=%#v", selectorString(labels), labels)
	}
	labels = matchLabels(map[string]any{"spec": map[string]any{"selector": map[string]any{"app": "api"}}})
	if selectorString(labels) != "app=api" {
		t.Fatalf("direct selectorString = %q labels=%#v", selectorString(labels), labels)
	}
}

// TestSortResultsKindTiebreak pins the result sort key: score DESC, then title,
// kind, link ASC. For three equal-name/equal-score hits (the "redpanda"
// Namespace/Service/StatefulSet case), Kind breaks the tie ASCENDING --
// Namespace < Service < StatefulSet -- BETWEEN title and link. A missing Kind
// tiebreak (a score->title->link order) would leave them in insertion order;
// this test fails without the Kind comparator.
func TestSortResultsKindTiebreak(t *testing.T) {
	// Deliberately shuffled input order; all share title "redpanda" and score 10.
	// The Links are chosen so that sorting by LINK alone would give a DIFFERENT
	// order than sorting by KIND (link order here is Service, StatefulSet,
	// Namespace -- "/a" < "/b" < "/z"). Only the Kind tiebreak (Namespace <
	// Service < StatefulSet) yields this order, so this fails if the Kind
	// comparator is dropped and the sort falls through to Link.
	results := []searchResult{
		{Title: "redpanda", Kind: "Service", Link: "/a-service"},
		{Title: "redpanda", Kind: "StatefulSet", Link: "/b-statefulset"},
		{Title: "redpanda", Kind: "Namespace", Link: "/z-namespace"},
	}
	sortResults(results, "redpanda")
	kinds := []string{results[0].Kind, results[1].Kind, results[2].Kind}
	if strings.Join(kinds, ",") != "Namespace,Service,StatefulSet" {
		t.Fatalf("kind-tiebreak order = %v, want [Namespace Service StatefulSet]", kinds)
	}

	// Same title AND same kind -> Link breaks the final tie ascending.
	tie := []searchResult{
		{Title: "x", Kind: "Pod", Link: "/z"},
		{Title: "x", Kind: "Pod", Link: "/a"},
	}
	sortResults(tie, "x")
	if tie[0].Link != "/a" {
		t.Fatalf("link final tiebreak = %#v", tie)
	}
}

// TestSearchScoreLabelValueCountedOnce pins the label scoring: +1 at most ONCE
// if a label value equals the (lowercased) query, never once-per-matching-label.
// Two label values equal to the query still add only 1.
func TestSearchScoreLabelValueCountedOnce(t *testing.T) {
	// Title does not contain the query, so the only points come from labels.
	two := map[string]string{"app": "redpanda", "team": "redpanda"}
	if got := searchScore("other", two, "redpanda"); got != 1 {
		t.Fatalf("searchScore with two matching label values = %d, want 1 (counted once)", got)
	}
	// No label value matches -> 0; the value check is exact (raw value vs the
	// lowercased query).
	if got := searchScore("other", map[string]string{"app": "Redpanda"}, "redpanda"); got != 0 {
		t.Fatalf("searchScore with case-differing label value = %d, want 0 (exact match)", got)
	}
	// Exact title match still scores 10 and the single label +1 stacks to 11.
	if got := searchScore("redpanda", map[string]string{"app": "redpanda"}, "redpanda"); got != 11 {
		t.Fatalf("searchScore exact title + label = %d, want 11", got)
	}
}

func TestAssetHashesSkipsUnreadableAndMissingAssets(t *testing.T) {
	fsys := fstest.MapFS{
		"app.css": {Data: []byte("body{}")},
		"dir":     {Mode: fs.ModeDir},
	}
	hashes := assetHashes(fsys)
	if hashes["app.css"] == "" {
		t.Fatalf("asset hashes = %#v", hashes)
	}
	s := &Server{assets: hashes}
	if s.assetURL("missing.css") != "" || !strings.HasPrefix(s.assetURL("app.css"), "/assets/app.css?v=") {
		t.Fatalf("asset URLs missing=%q app=%q", s.assetURL("missing.css"), s.assetURL("app.css"))
	}
}
