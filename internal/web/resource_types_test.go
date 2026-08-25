package web

import (
	"reflect"
	"testing"

	"github.com/kbelokon/readout/internal/kube"
)

// TestSortedResourceTypesForDisplayContract pins the complete display order,
// not the sorting implementation: Kind, API version, plural, then scope, with
// equivalent discovery records stable. The virtual metrics API is omitted and
// the discovery slice remains reusable by other request builders.
func TestSortedResourceTypesForDisplayContract(t *testing.T) {
	types := []kube.ResourceType{
		{Kind: "Widget", APIVersion: "z.example/v1", Plural: "widgets", Namespaced: true, Singular: "z-version"},
		{Group: "metrics.k8s.io", Kind: "WidgetMetrics", APIVersion: "metrics.k8s.io/v1beta1", Plural: "widgets", Namespaced: true, Singular: "virtual"},
		{Kind: "Widget", APIVersion: "a.example/v1", Plural: "widgets", Namespaced: true, Singular: "namespaced"},
		{Kind: "Alpha", APIVersion: "z.example/v1", Plural: "alphas", Namespaced: true, Singular: "kind-first"},
		{Kind: "Widget", APIVersion: "a.example/v1", Plural: "gadgets", Namespaced: true, Singular: "plural-first"},
		{Kind: "Widget", APIVersion: "a.example/v1", Plural: "widgets", Singular: "stable-first"},
		{Kind: "Widget", APIVersion: "a.example/v1", Plural: "widgets", Singular: "stable-second"},
	}
	original := append([]kube.ResourceType(nil), types...)

	got := sortedResourceTypesForDisplay(types)
	wantOrder := []string{
		"kind-first",
		"plural-first",
		"stable-first",
		"stable-second",
		"namespaced",
		"z-version",
	}
	gotOrder := make([]string, 0, len(got))
	for i := range got {
		gotOrder = append(gotOrder, got[i].Singular)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("resource-type order = %q, want %q", gotOrder, wantOrder)
	}
	if !reflect.DeepEqual(types, original) {
		t.Fatalf("input discovery slice mutated:\n got %#v\nwant %#v", types, original)
	}
}
