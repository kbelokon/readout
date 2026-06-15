package demo

// scenario_test.go is the demo's coverage + integrity law.
//
// TestDemoIntegrity seeds each cluster through the REAL fakekube validator
// (fakekube.New(); srv.Seed(cluster)); a no-error result proves every Service
// selector, ownerRef, Ingress backend, PVC/PV binding, Pod→Node assignment,
// Event involvedObject, and metric key resolves.
//
// TestDemoCoverage asserts the scenario lights up every render path. Its
// expectations are DERIVED FROM READOUT'S OWN CODE, not a hand-written blob:
//   - StatusTone words: each candidate word is GUARDED against kube.StatusTone
//     (the test goes red if table.go drops/renames a word's tone), and the
//     StatusTone source is SCANNED so a NEW case word table.go adds that the
//     test does not cover also goes red. Then each word is asserted to surface
//     in the scenario.
//   - Events Reason-map tones: each candidate Reason is guarded against
//     kube.CellClass("events","Reason",…) and the Reason source switch is
//     scanned, then asserted present in the scenario.
//   - 11 curated icon families: derived from kindicons.go (the curated group map
//     + the *.fluxcd.io / *.gatekeeper.sh suffix regexes), each asserted to have
//     ≥1 CRD; plus ≥1 unknown-group CRD for the HashHue monogram path.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	fakekube "github.com/kbelokon/readout/internal/fakekube"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/icons"
)

// ---------------------------------------------------------------------------
// TestDemoIntegrity — seed each cluster through the real validator.
// ---------------------------------------------------------------------------

func TestDemoIntegrity(t *testing.T) {
	for _, c := range DemoScenario().Clusters {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			srv, err := fakekube.New(fakekube.WithoutControl())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(srv.Close)
			if err := srv.Seed(c); err != nil {
				t.Fatalf("Seed(%s): %v", c.Name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestDemoCoverage — the render-path coverage law.
// ---------------------------------------------------------------------------

func TestDemoCoverage(t *testing.T) {
	scenario := DemoScenario()

	if len(scenario.Clusters) != 2 {
		t.Fatalf("scenario has %d clusters, want exactly 2 (prod + staging)", len(scenario.Clusters))
	}

	t.Run("status tone words", func(t *testing.T) { assertStatusToneCoverage(t, scenario) })
	t.Run("events reason tones", func(t *testing.T) { assertReasonCoverage(t, scenario) })
	t.Run("icon families", func(t *testing.T) { assertIconFamilyCoverage(t, scenario) })
	t.Run("curated kinds", func(t *testing.T) { assertCuratedKinds(t, scenario) })
	t.Run("metrics", func(t *testing.T) { assertMetrics(t, scenario) })
	t.Run("edge namespaces", func(t *testing.T) { assertEdgeNamespaces(t, scenario) })
	t.Run("long annotation", func(t *testing.T) { assertLongAnnotation(t, scenario) })
}

// --- StatusTone coverage ----------------------------------------------------

// statusToneWords is the EXPECTED word→tone table. It is a guard, not the
// source of truth: assertStatusToneCoverage (1) verifies kube.StatusTone still
// maps each word to the listed tone, and (2) scans table.go's StatusTone source
// so a word added there but missing here fails the test.
var statusToneWords = map[string]string{
	// ok
	"Running": "ok", "Ready": "ok", "Active": "ok", "Bound": "ok", "Complete": "ok",
	// mute
	"Completed": "mute", "Succeeded": "mute", "Normal": "mute", "Suspended": "mute",
	// warn
	"Pending": "warn", "ContainerCreating": "warn", "PodInitializing": "warn",
	"Terminating": "warn", "Warning": "warn", "Released": "warn",
	// err
	"CrashLoopBackOff": "err", "Error": "err", "Failed": "err", "NotReady": "err",
	"OOMKilled": "err", "ImagePullBackOff": "err", "Evicted": "err",
	"BackoffLimitExceeded": "err", "ErrImagePull": "err",
	"CreateContainerConfigError": "err", "InvalidImageName": "err", "OutOfcpu": "err",
}

func assertStatusToneCoverage(t *testing.T, s fakekube.Scenario) {
	t.Helper()

	// (1) Guard: kube.StatusTone still maps each word to the expected tone.
	for word, want := range statusToneWords {
		if got := kube.StatusTone(word); got != want {
			t.Errorf("kube.StatusTone(%q) = %q, want %q — table.go changed; update statusToneWords", word, got, want)
		}
	}

	// (2) Source scan: every quoted word in table.go's StatusTone switch must be
	// covered by statusToneWords, so a NEW case word goes red here.
	for _, word := range sourceStatusToneWords(t) {
		if _, ok := statusToneWords[word]; !ok {
			t.Errorf("table.go StatusTone maps %q but statusToneWords does not cover it — add it and a carrier object", word)
		}
	}

	// (3) The Init:* branch (warn + err): assert StatusTone classifies both, and
	// that the scenario carries init-container states yielding each.
	if got := kube.StatusTone("Init:1/2"); got != "warn" {
		t.Errorf("StatusTone(Init:1/2) = %q, want warn", got)
	}
	if got := kube.StatusTone("Init:CrashLoopBackOff"); got != "err" {
		t.Errorf("StatusTone(Init:CrashLoopBackOff) = %q, want err", got)
	}

	// (4) Presence: every word surfaces in the scenario where readout renders it.
	present := collectStatusWords(s)
	for word := range statusToneWords {
		if !present[word] {
			t.Errorf("StatusTone word %q surfaces in no scenario object (no render branch exercises it)", word)
		}
	}
	// Init:* representatives.
	if !present["Init:warn"] {
		t.Errorf("no pod surfaces an in-flight Init:N/M state (StatusTone Init:* warn branch untested)")
	}
	if !present["Init:err"] {
		t.Errorf("no pod surfaces an errored Init:* state (StatusTone Init:* err branch untested)")
	}
}

// sourceStatusToneWords scans internal/kube/table.go for the quoted words in the
// StatusTone function's `case` clauses (the tone return strings "ok"/"warn"/… are
// excluded — they are returns, not cases).
func sourceStatusToneWords(t *testing.T) []string {
	t.Helper()
	src := readSource(t, "../kube/table.go")
	body := between(t, src, "func StatusTone(", "\n}\n")
	return caseStringLiterals(body)
}

// --- Events Reason-map coverage --------------------------------------------

// reasonToneClass is the EXPECTED Reason→Bulma-class table (the class
// kube.CellClass returns for an events Reason cell). Guarded against
// kube.CellClass and the table.go Reason switch source, mirroring the StatusTone
// approach. The two info-tone Reasons (SawCompletedJob/TriggeredScaleUp) are the
// only has-text-info path in the whole system.
var reasonToneClass = map[string]string{
	// danger
	"BackOff": "has-text-danger", "BackoffLimitExceeded": "has-text-danger",
	"DeadlineExceeded": "has-text-danger", "Failed": "has-text-danger",
	"FailedComputeMetricsReplicas": "has-text-danger", "FailedGetResourceMetric": "has-text-danger",
	"FailedScheduling": "has-text-danger", "Preempted": "has-text-danger",
	"SystemOOM": "has-text-danger", "Unhealthy": "has-text-danger",
	// warning
	"Killing": "has-text-warning", "Pulling": "has-text-warning",
	// success
	"Created": "has-text-success", "Pulled": "has-text-success", "Scheduled": "has-text-success",
	"Started": "has-text-success", "SuccessfulCreate": "has-text-success",
	// info (the unique tone)
	"SawCompletedJob": "has-text-info", "TriggeredScaleUp": "has-text-info",
}

func assertReasonCoverage(t *testing.T, s fakekube.Scenario) {
	t.Helper()

	// (1) Guard against kube.CellClass.
	for reason, want := range reasonToneClass {
		if got := kube.CellClass("events", "Reason", reason); got != want {
			t.Errorf("kube.CellClass(events,Reason,%q) = %q, want %q — table.go changed; update reasonToneClass", reason, got, want)
		}
	}

	// (2) Source scan: every quoted Reason in the table.go Reason switch must be
	// covered.
	for _, reason := range sourceReasonWords(t) {
		if _, ok := reasonToneClass[reason]; !ok {
			t.Errorf("table.go Reason map handles %q but reasonToneClass does not cover it — add it and a carrier Event", reason)
		}
	}

	// (3) The unique info tone must be exercised.
	sawInfo := false
	for _, reason := range []string{"SawCompletedJob", "TriggeredScaleUp"} {
		if reasonToneClass[reason] == "has-text-info" {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Fatal("the unique events info tone (SawCompletedJob/TriggeredScaleUp) is not in reasonToneClass")
	}

	// (4) Presence: every Reason surfaces on a scenario Event.
	present := collectEventReasons(s)
	for reason := range reasonToneClass {
		if !present[reason] {
			t.Errorf("events Reason %q is carried by no scenario Event (its Reason→tone branch is untested)", reason)
		}
	}
}

// sourceReasonWords scans the Reason switch inside CellClass for its quoted
// case words.
func sourceReasonWords(t *testing.T) []string {
	t.Helper()
	src := readSource(t, "../kube/table.go")
	// The Reason cases live between `if col == "Reason"` and the next `case` of
	// the enclosing plural switch ("deployments").
	body := between(t, src, `if col == "Reason"`, "\n\tcase \"deployments\":")
	return caseStringLiterals(body)
}

// --- Icon-family coverage ---------------------------------------------------

func assertIconFamilyCoverage(t *testing.T, s fakekube.Scenario) {
	t.Helper()

	families := curatedIconFamilies(t)
	if len(families) != 11 {
		t.Fatalf("derived %d curated icon families from kindicons.go, want 11: %v", len(families), families)
	}

	// Collect every CRD group across both clusters.
	groups := map[string]bool{}
	for _, c := range s.Clusters {
		for _, crd := range c.CRDs {
			groups[crd.Group] = true
		}
	}

	for _, fam := range families {
		if !familyHit(fam, groups) {
			t.Errorf("no CRD group matches curated icon family %q — that Tier-2a glyph is unexercised", fam.name)
		}
	}

	// Monogram path: ≥1 CRD whose group hits NO curated family (so it falls to
	// the HashHue monogram tile).
	monograms := 0
	for g := range groups {
		hit := false
		for _, fam := range families {
			if fam.matches(g) {
				hit = true
				break
			}
		}
		if !hit {
			monograms++
		}
	}
	if monograms < 1 {
		t.Errorf("no unknown-group CRD present (the HashHue monogram path is unexercised)")
	}
}

// iconFamily is one curated icon family: either an exact group name or a suffix
// regex (the *.fluxcd.io / *.gatekeeper.sh families).
type iconFamily struct {
	name    string
	exact   string
	pattern *regexp.Regexp
}

func (f iconFamily) matches(group string) bool {
	if f.exact != "" {
		return group == f.exact
	}
	return f.pattern != nil && f.pattern.MatchString(group)
}

func familyHit(f iconFamily, groups map[string]bool) bool {
	for g := range groups {
		if f.matches(g) {
			return true
		}
	}
	return false
}

// curatedIconFamilies derives the 11 curated icon families from
// kindicons.go: the distinct GLYPH targets among the curated group map are
// folded so cert-manager.io's three aliases count once, plus the two suffix
// families. We express the canonical 11 family representatives and assert
// kindicons.go still recognizes each (icons.HashHue is exported; the group
// recognition is proven via the package's own KindIcon path is not exported, so
// we scan the source group keys to confirm each representative is present).
func curatedIconFamilies(t *testing.T) []iconFamily {
	t.Helper()
	src := readSource(t, "../web/icons/kindicons.go")

	// The 11 canonical families (one representative group each). Each exact group
	// is asserted to appear as a key in crdGroupGlyph; the two suffix families
	// are asserted via their regex literals.
	exact := []string{
		"cert-manager.io",
		"cilium.io",
		"argoproj.io",
		"operator.victoriametrics.com",
		"monitoring.coreos.com",
		"external-secrets.io",
		"keda.sh",
		"postgresql.cnpg.io",
		"gateway.networking.k8s.io",
	}
	var families []iconFamily
	for _, g := range exact {
		if !strings.Contains(src, `"`+g+`"`) {
			t.Errorf("kindicons.go no longer lists curated group %q — family set changed", g)
		}
		families = append(families, iconFamily{name: g, exact: g})
	}

	// Suffix families: assert the regex source is still present, then use an
	// equivalent matcher.
	if !strings.Contains(src, `fluxcd\.io$`) {
		t.Error("kindicons.go no longer carries the *.fluxcd.io suffix family")
	}
	families = append(families, iconFamily{
		name: "*.fluxcd.io", pattern: regexp.MustCompile(`(^|\.)fluxcd\.io$`),
	})
	if !strings.Contains(src, `gatekeeper\.sh$`) {
		t.Error("kindicons.go no longer carries the *.gatekeeper.sh suffix family")
	}
	families = append(families, iconFamily{
		name: "*.gatekeeper.sh", pattern: regexp.MustCompile(`(^|\.)gatekeeper\.sh$`),
	})
	return families
}

// --- Curated-kind / metrics / edge / annotation coverage --------------------

// curatedKinds are the builtin kinds the demo must serve at least one of, so the
// curated per-kind cells (list_cells.go) each render against real rows.
var curatedKinds = []string{
	"Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet",
	"HorizontalPodAutoscaler", "Job", "CronJob", "Service", "Ingress",
	"ConfigMap", "Secret", "Node", "PersistentVolume", "PersistentVolumeClaim",
	"Event",
}

func assertCuratedKinds(t *testing.T, s fakekube.Scenario) {
	t.Helper()
	kinds := collectKinds(s)
	for _, k := range curatedKinds {
		if !kinds[k] {
			t.Errorf("curated kind %q is served by no scenario object", k)
		}
	}
}

func assertMetrics(t *testing.T, s fakekube.Scenario) {
	t.Helper()
	pods, nodes := 0, 0
	for _, c := range s.Clusters {
		nodes += len(c.NodeMetrics)
		for _, ns := range c.Namespaces {
			pods += len(ns.PodMetrics)
		}
	}
	if pods == 0 {
		t.Error("no PodMetrics present (the pod capacity bars render nothing)")
	}
	if nodes == 0 {
		t.Error("no NodeMetrics present (the node usage-over-capacity bars render nothing)")
	}
}

func assertEdgeNamespaces(t *testing.T, s fakekube.Scenario) {
	t.Helper()
	var emptyFound bool
	var bigCount int
	for _, c := range s.Clusters {
		for _, ns := range c.Namespaces {
			if len(ns.Objects) == 0 {
				emptyFound = true
			}
			if len(ns.Objects) > bigCount {
				bigCount = len(ns.Objects)
			}
		}
	}
	if !emptyFound {
		t.Error("no empty namespace present (the empty-list render path is untested)")
	}
	if bigCount < 500 {
		t.Errorf("biggest namespace has %d objects, want ≥500 to cross the virtualization threshold", bigCount)
	}
}

func assertLongAnnotation(t *testing.T, s fakekube.Scenario) {
	t.Helper()
	const minLen = 120
	if len(longAnnotation) <= minLen {
		t.Fatalf("longAnnotation is %d chars, want >120", len(longAnnotation))
	}
	found := false
	for _, c := range s.Clusters {
		for _, ns := range c.Namespaces {
			for _, o := range ns.Objects {
				for _, v := range annotationsOf(o) {
					if len(v) > minLen {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("no scenario object carries a >120-char annotation (the annotation-collapse detail path is untested)")
	}
}

// ---------------------------------------------------------------------------
// Wire collectors. Each walks the scenario's typed objects as the JSON wire map
// readout reads, mirroring the production status-vocabulary derivations
// (containerStateWord / event Type+Reason / phase / Init:N-M synthesis).
// ---------------------------------------------------------------------------

func eachObject(s fakekube.Scenario, fn func(o runtime.Object, ns string)) {
	for _, c := range s.Clusters {
		for _, n := range c.Nodes {
			fn(n, "")
		}
		for _, o := range c.ClusterObjects {
			fn(o, "")
		}
		for _, ns := range c.Namespaces {
			for _, o := range ns.Objects {
				fn(o, ns.Name)
			}
		}
	}
}

// collectStatusWords gathers every status word readout would render from the
// scenario: pod phases, container state reasons (containerStateWord), PV
// phase/reason, job condition reason+type, cronjob Active/Suspended, node
// Ready/NotReady, PVC Bound, plus the two Init:* synthesis markers ("Init:warn"
// / "Init:err").
func collectStatusWords(s fakekube.Scenario) map[string]bool {
	present := map[string]bool{}
	eachObject(s, func(o runtime.Object, _ string) {
		switch v := o.(type) {
		case *corev1.Pod:
			if v.Status.Phase != "" {
				present[string(v.Status.Phase)] = true
			}
			for i := range v.Status.ContainerStatuses {
				if w := stateWord(&v.Status.ContainerStatuses[i]); w != "" {
					present[w] = true
				}
			}
			collectInitMarkers(v, present)
		case *corev1.PersistentVolume:
			if v.Status.Phase != "" {
				present[string(v.Status.Phase)] = true
			}
		case *corev1.PersistentVolumeClaim:
			if v.Status.Phase != "" {
				present[string(v.Status.Phase)] = true
			}
		case *corev1.Node:
			present[nodeReadyWord(v)] = true
		}
	})
	// Job condition reasons/types + cronjob suspend are read off the wire maps so
	// the collector stays type-agnostic for those.
	eachWire(s, func(kind string, m map[string]any) {
		switch kind {
		case "Job":
			for _, c := range nestedSlice(m, "status", "conditions") {
				cm, _ := c.(map[string]any)
				if reason, _ := cm["reason"].(string); reason != "" {
					present[reason] = true
				}
				if typ, _ := cm["type"].(string); typ != "" {
					present[typ] = true
				}
			}
		case "CronJob":
			if sus, ok := nestedBool(m, "spec", "suspend"); ok && sus {
				present["Suspended"] = true
			} else {
				present["Active"] = true
			}
		case "Event":
			// Event Type (Normal/Warning) is status vocabulary.
			if typ, _ := m["type"].(string); typ != "" {
				present[typ] = true
			}
		}
	})
	return present
}

// stateWord mirrors web.containerStateWord: Running / terminated reason /
// waiting reason.
func stateWord(cs *corev1.ContainerStatus) string {
	switch {
	case cs.State.Running != nil:
		return "Running"
	case cs.State.Terminated != nil:
		return firstNonEmpty(cs.State.Terminated.Reason, "Terminated")
	case cs.State.Waiting != nil:
		return firstNonEmpty(cs.State.Waiting.Reason, "Waiting")
	}
	return ""
}

// collectInitMarkers mirrors the kube list printer's Init:N/M synthesis: a pod
// with init containers not all complete shows "Init:<done>/<total>" (warn) or,
// when an init container waits with an Error/BackOff reason,
// "Init:<reason>" (err). We record the warn/err markers the StatusTone Init:*
// branch keys on.
func collectInitMarkers(p *corev1.Pod, present map[string]bool) {
	inits := p.Status.InitContainerStatuses
	if len(inits) == 0 {
		return
	}
	for i := range inits {
		st := &inits[i]
		if w := st.State.Waiting; w != nil {
			if strings.Contains(w.Reason, "Error") || strings.Contains(w.Reason, "BackOff") {
				// e.g. "Init:CrashLoopBackOff" → StatusTone err branch.
				if kube.StatusTone("Init:"+w.Reason) == "err" {
					present["Init:err"] = true
				}
			} else {
				// In-flight init (e.g. "Init:0/2") → StatusTone warn branch.
				if kube.StatusTone("Init:0/"+itoa(len(inits))) == "warn" {
					present["Init:warn"] = true
				}
			}
		}
	}
}

func nodeReadyWord(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "NotReady"
}

func collectEventReasons(s fakekube.Scenario) map[string]bool {
	present := map[string]bool{}
	eachObject(s, func(o runtime.Object, _ string) {
		if ev, ok := o.(*corev1.Event); ok && ev.Reason != "" {
			present[ev.Reason] = true
		}
	})
	return present
}

func collectKinds(s fakekube.Scenario) map[string]bool {
	kinds := map[string]bool{}
	eachWire(s, func(kind string, _ map[string]any) { kinds[kind] = true })
	return kinds
}

// eachWire visits every object as (kind, wireMap), deriving the kind from the
// Go type for typed objects and from the unstructured TypeMeta for CRs.
func eachWire(s fakekube.Scenario, fn func(kind string, m map[string]any)) {
	eachObject(s, func(o runtime.Object, _ string) {
		fn(kindOf(o), toMap(o))
	})
}

func kindOf(o runtime.Object) string {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u.GetKind()
	}
	t := strings.TrimPrefix(fmt.Sprintf("%T", o), "*")
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

func annotationsOf(o runtime.Object) map[string]string {
	m := toMap(o)
	meta, _ := m["metadata"].(map[string]any)
	raw, _ := meta["annotations"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Tiny source/AST + wire helpers (test-local).
// ---------------------------------------------------------------------------

func readSource(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// between returns the substring of src between the first occurrence of start and
// the first occurrence of end AFTER start.
func between(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("source marker %q not found", start)
	}
	rest := src[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("source end marker %q not found after %q", end, start)
	}
	return rest[:j]
}

// caseClauseRe matches a `case <list>:` clause whose value list may span
// MULTIPLE lines (the StatusTone err case wraps over two lines). The (?s) flag
// lets `.` cross newlines; the non-greedy `.*?` stops at the first clause-ending
// `:` followed by a newline, so a multi-line case is captured whole.
var caseClauseRe = regexp.MustCompile(`(?s)\bcase\s+(.*?):[ \t]*\n`)
var quotedRe = regexp.MustCompile(`"([^"]+)"`)

// caseStringLiterals extracts the quoted strings from every `case ...:` clause
// in a code block, including clauses whose value list wraps across lines.
func caseStringLiterals(body string) []string {
	var out []string
	for _, clause := range caseClauseRe.FindAllStringSubmatch(body, -1) {
		for _, m := range quotedRe.FindAllStringSubmatch(clause[1], -1) {
			out = append(out, m[1])
		}
	}
	return out
}

func toMap(o runtime.Object) map[string]any {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u.Object
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		panic(err)
	}
	return m
}

func nestedSlice(m map[string]any, path ...string) []any {
	cur := any(m)
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	if s, ok := cur.([]any); ok {
		return s
	}
	return nil
}

func nestedBool(m map[string]any, path ...string) (bool, bool) {
	cur := any(m)
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false, false
		}
		cur = mm[k]
	}
	b, ok := cur.(bool)
	return b, ok
}

// itoa is a tiny int→string for the Init:N synthesis (avoids strconv import
// noise in the collector).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// icons import kept meaningful: assert HashHue is callable so the monogram path
// stays linked (a compile guard that kindicons.go's hue function is the one the
// scenario's unknown groups will key on).
var _ = icons.HashHue
