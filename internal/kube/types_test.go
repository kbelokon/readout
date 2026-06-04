package kube

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResourceTypeHelpers(t *testing.T) {
	rt := ResourceType{Group: "apps", Version: "v1", APIVersion: "apps/v1", Kind: "Deployment", Plural: "deployments", Namespaced: true}
	if got := rt.GVR().String(); got != "apps/v1, Resource=deployments" {
		t.Fatalf("GVR = %q", got)
	}
	if got := rt.GVK().String(); got != "apps/v1, Kind=Deployment" {
		t.Fatalf("GVK = %q", got)
	}
	if rt.Endpoint() != "deployments" || rt.Key() != "apps/v1/deployments/true" {
		t.Fatalf("endpoint/key mismatch: %q %q", rt.Endpoint(), rt.Key())
	}
	if NormalizeAPIVersion("", "v1") != "v1" || NormalizeAPIVersion("apps", "v1") != "apps/v1" {
		t.Fatal("NormalizeAPIVersion mismatch")
	}
	if group, version := SplitAPIVersion("batch/v1"); group != "batch" || version != "v1" {
		t.Fatalf("SplitAPIVersion grouped = %q %q", group, version)
	}
	if group, version := SplitAPIVersion("v1"); group != "" || version != "v1" {
		t.Fatalf("SplitAPIVersion core = %q %q", group, version)
	}
}

func TestObjectHelpers(t *testing.T) {
	raw := map[string]any{
		"kind": "Pod",
		"metadata": map[string]any{
			"name":              "nginx",
			"namespace":         "default",
			"uid":               "uid-1",
			"creationTimestamp": "2026-01-02T03:04:05Z",
			"labels":            map[string]any{"app": "nginx", "tier": "web"},
			"annotations":       map[string]any{"note": "yes"},
		},
	}
	obj := NewObject(&ResourceType{Kind: "Fallback"}, &unstructured.Unstructured{Object: raw})
	if obj.Name() != "nginx" || obj.Namespace() != "default" || obj.UID() != "uid-1" || obj.Kind() != "Pod" {
		t.Fatalf("object identity mismatch: %#v", obj)
	}
	if obj.CreationTimestamp() != "2026-01-02T03:04:05Z" || obj.Labels()["app"] != "nginx" || obj.Annotations()["note"] != "yes" {
		t.Fatalf("metadata mismatch: %#v", obj)
	}
	// The accessors now go through apimachinery's GetLabels/NestedStringMap,
	// which is stricter than the retired hand-rolled walk: a labels map with a
	// non-string value yields an empty map (all-or-nothing) rather than silently
	// dropping just the bad entry. Real label/annotation maps are all strings, so
	// this only changes behavior on malformed input — the intended stricter read.
	mixed := NewObject(&ResourceType{}, &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app": "nginx", "ignored": 3}},
	}})
	if len(mixed.Labels()) != 0 {
		t.Fatalf("mixed-type labels = %#v, want empty (apimachinery NestedStringMap is all-or-nothing)", mixed.Labels())
	}
	if ToObjectMap(nil) == nil {
		t.Fatal("ToObjectMap(nil) should return an empty map")
	}
	if got := ToObjectMap(&unstructured.Unstructured{Object: raw}); got["kind"] != "Pod" {
		t.Fatalf("ToObjectMap(non-nil) = %#v", got)
	}
	fallback := NewObject(&ResourceType{Kind: "Fallback"}, &unstructured.Unstructured{Object: map[string]any{}})
	if got := fallback.Kind(); got != "Fallback" {
		t.Fatalf("fallback kind = %q", got)
	}
}
