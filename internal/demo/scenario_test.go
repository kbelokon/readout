package demo

// scenario_test.go is the demo's coverage + integrity law.
//
// TestDemoIntegrity seeds each cluster through the REAL fakekube validator
// (fakekube.New(); srv.Seed(cluster)); a no-error result proves every Service
// selector, ownerRef, Ingress backend, PVC/PV binding, Pod→Node assignment,
// Event involvedObject, and metric key resolves.
//
// TestDemoCoverage asserts the scenario lights up the render paths that matter
// for a believable, polished demo. It deliberately checks a REALISTIC set of
// impactful states (a healthy fleet, a crash-looping rollout, an image-pull
// failure, a pending pod, completed/failed jobs, init/terminating lifecycles,
// node + PV states), not every enum value — a real cluster does not exhibit
// every obscure status at once. The status/reason words it does require are
// guarded against readout's own kube.StatusTone / kube.CellClass, so the test
// goes red if table.go renames a tone. The 11 curated CRD icon families are
// derived from kindicons.go and each asserted present (the glyphs are a visible
// selling point), plus a monogram fallback.

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
			if err := srv.Seed(&c); err != nil {
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

// statusToneWords is the realistic set of impactful status words the demo must
// surface, each with the tone kube.StatusTone is expected to assign. It is a
// guard (assertStatusToneCoverage re-checks kube.StatusTone still maps each to
// the listed tone), and every word is asserted to surface on a real scenario
// object — so each tone class has at least one row to render.
var statusToneWords = map[string]string{
	// ok / mute — the healthy and terminal-success vocabulary.
	"Running": "ok", "Ready": "ok", "Active": "ok", "Bound": "ok", "Complete": "ok",
	"Completed": "mute", "Succeeded": "mute", "Suspended": "mute", "Normal": "mute",
	// warn — in-flight / lifecycle.
	"Pending": "warn", "ContainerCreating": "warn", "PodInitializing": "warn",
	"Terminating": "warn", "Warning": "warn", "Released": "warn",
	// err — the incident vocabulary a visitor should spot at a glance.
	"CrashLoopBackOff": "err", "ImagePullBackOff": "err", "NotReady": "err",
	"Failed": "err", "BackoffLimitExceeded": "err",
}

func assertStatusToneCoverage(t *testing.T, s fakekube.Scenario) {
	t.Helper()

	// Guard: kube.StatusTone still maps each word to the expected tone.
	for word, want := range statusToneWords {
		if got := kube.StatusTone(word); got != want {
			t.Errorf("kube.StatusTone(%q) = %q, want %q — table.go changed; update statusToneWords", word, got, want)
		}
	}

	// The Init:* branch (warn + err).
	if got := kube.StatusTone("Init:1/2"); got != "warn" {
		t.Errorf("StatusTone(Init:1/2) = %q, want warn", got)
	}
	if got := kube.StatusTone("Init:CrashLoopBackOff"); got != "err" {
		t.Errorf("StatusTone(Init:CrashLoopBackOff) = %q, want err", got)
	}

	// Presence: every word surfaces in the scenario where readout renders it.
	present := collectStatusWords(s)
	for word := range statusToneWords {
		if !present[word] {
			t.Errorf("StatusTone word %q surfaces in no scenario object (no render branch exercises it)", word)
		}
	}
	if !present["Init:warn"] {
		t.Errorf("no pod surfaces an in-flight Init:N/M state (StatusTone Init:* warn branch untested)")
	}
	if !present["Init:err"] {
		t.Errorf("no pod surfaces an errored Init:* state (StatusTone Init:* err branch untested)")
	}
}

// --- Events Reason-map coverage --------------------------------------------

// reasonToneClass is the realistic set of event Reasons the demo must carry,
// each with the Bulma class kube.CellClass returns. It spans all four event
// tones — danger, warning-via-class, success, and the unique info tone
// (SawCompletedJob) — without forcing every reason in the map.
var reasonToneClass = map[string]string{
	// danger
	"BackOff": "has-text-danger", "Unhealthy": "has-text-danger",
	"FailedScheduling": "has-text-danger", "SystemOOM": "has-text-danger",
	"BackoffLimitExceeded": "has-text-danger",
	// success
	"Pulled": "has-text-success", "SuccessfulCreate": "has-text-success",
	// info (the unique tone)
	"SawCompletedJob": "has-text-info",
}

func assertReasonCoverage(t *testing.T, s fakekube.Scenario) {
	t.Helper()

	// Guard against kube.CellClass.
	for reason, want := range reasonToneClass {
		if got := kube.CellClass("events", "Reason", reason); got != want {
			t.Errorf("kube.CellClass(events,Reason,%q) = %q, want %q — table.go changed; update reasonToneClass", reason, got, want)
		}
	}

	// The unique info tone must be exercised.
	if reasonToneClass["SawCompletedJob"] != "has-text-info" {
		t.Fatal("the unique events info tone (SawCompletedJob) is not classified has-text-info")
	}

	// Presence: every Reason surfaces on a scenario Event.
	present := collectEventReasons(s)
	for reason := range reasonToneClass {
		if !present[reason] {
			t.Errorf("events Reason %q is carried by no scenario Event (its Reason→tone branch is untested)", reason)
		}
	}
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
	for i := range s.Clusters {
		c := &s.Clusters[i]
		for _, crd := range c.CRDs {
			groups[crd.Group] = true
		}
	}

	for _, fam := range families {
		if !familyHit(fam, groups) {
			t.Errorf("no CRD group matches curated icon family %q — that glyph is unexercised", fam.name)
		}
	}

	// Monogram path: ≥1 CRD whose group hits NO curated family.
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

// curatedIconFamilies derives the 11 curated icon families from kindicons.go:
// nine exact groups plus the two suffix families. Each is asserted still present
// in the source so the family set cannot drift unnoticed.
func curatedIconFamilies(t *testing.T) []iconFamily {
	t.Helper()
	src := readSource(t, "../web/icons/kindicons.go")

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
	"Job", "CronJob", "Service", "Ingress",
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
	for i := range s.Clusters {
		c := &s.Clusters[i]
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
	for i := range s.Clusters {
		c := &s.Clusters[i]
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
	for i := range s.Clusters {
		c := &s.Clusters[i]
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
// readout reads, mirroring the production status-vocabulary derivations.
// ---------------------------------------------------------------------------

func eachObject(s fakekube.Scenario, fn func(o runtime.Object, ns string)) {
	for i := range s.Clusters {
		c := &s.Clusters[i]
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

// collectInitMarkers mirrors the kube list printer's Init:N/M synthesis.
func collectInitMarkers(p *corev1.Pod, present map[string]bool) {
	inits := p.Status.InitContainerStatuses
	if len(inits) == 0 {
		return
	}
	for i := range inits {
		st := &inits[i]
		if w := st.State.Waiting; w != nil {
			if strings.Contains(w.Reason, "Error") || strings.Contains(w.Reason, "BackOff") {
				if kube.StatusTone("Init:"+w.Reason) == "err" {
					present["Init:err"] = true
				}
			} else {
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
// Tiny source + wire helpers (test-local).
// ---------------------------------------------------------------------------

func readSource(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
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

// icons import kept meaningful: assert HashHue stays linked (the monogram path
// the scenario's unknown groups key on).
var _ = icons.HashHue
