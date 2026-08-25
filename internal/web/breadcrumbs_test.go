package web

import (
	"testing"

	"github.com/kbelokon/readout/internal/kube"
)

func TestResourceListHrefScopeAndEscaping(t *testing.T) {
	const cluster = "prod/eu"
	const namespace = "team/a"
	const plural = "widgets/example"

	if got := resourceListHref(cluster, namespace, plural, true); got != "/clusters/prod%2Feu/namespaces/team%2Fa/widgets%2Fexample" {
		t.Fatalf("namespaced list href = %q", got)
	}
	if got := resourceListHref(cluster, namespace, plural, false); got != "/clusters/prod%2Feu/widgets%2Fexample" {
		t.Fatalf("cluster-scoped list href = %q", got)
	}
	rt := kube.ResourceType{Plural: plural, Namespaced: true}
	if got := resourceHref(cluster, &rt, namespace, "object/name"); got != "/clusters/prod%2Feu/namespaces/team%2Fa/widgets%2Fexample/object%2Fname" {
		t.Fatalf("object href = %q", got)
	}
}

func TestDetailBreadcrumbsShareResourceListRoutes(t *testing.T) {
	namespacedObject := &kube.Object{
		Resource: kube.ResourceType{Plural: "pods/example", Kind: "Pod", Namespaced: true},
		Raw:      map[string]any{"metadata": map[string]any{"name": "pod/name"}},
	}
	objectCrumb := objectBreadcrumb("prod/eu", "team/a", namespacedObject)
	stateCrumb := detailStateBreadcrumb(&detailView{
		Cluster: "prod/eu",
		State: &detailStateView{
			Resource:  "pods/example",
			Namespace: "team/a",
			Name:      "pod/name",
		},
	})
	const namespacedList = "/clusters/prod%2Feu/namespaces/team%2Fa/pods%2Fexample"
	for label, crumb := range map[string]struct {
		showNamespace bool
		namespaceHref string
		pluralHref    string
	}{
		"object": {objectCrumb.ShowNamespace, objectCrumb.NamespaceHref, objectCrumb.PluralHref},
		"state":  {stateCrumb.ShowNamespace, stateCrumb.NamespaceHref, stateCrumb.PluralHref},
	} {
		if !crumb.showNamespace || crumb.namespaceHref != "/clusters/prod%2Feu/namespaces/team%2Fa" || crumb.pluralHref != namespacedList {
			t.Fatalf("%s breadcrumb scope = %+v", label, crumb)
		}
	}

	clusterScopedObject := &kube.Object{
		Resource: kube.ResourceType{Plural: "nodes/example", Kind: "Node", Namespaced: false},
		Raw:      map[string]any{"metadata": map[string]any{"name": "node/name"}},
	}
	clusterCrumb := objectBreadcrumb("prod/eu", "ignored/ns", clusterScopedObject)
	if clusterCrumb.ShowNamespace || clusterCrumb.NamespaceHref != "" || clusterCrumb.PluralHref != "/clusters/prod%2Feu/nodes%2Fexample" {
		t.Fatalf("cluster-scoped object breadcrumb = %+v", clusterCrumb)
	}

	namespaceState := detailStateBreadcrumb(&detailView{
		Cluster: "prod/eu",
		State: &detailStateView{
			Resource:  "namespaces",
			Namespace: "ignored/ns",
			Name:      "team/a",
		},
	})
	if namespaceState.ShowNamespace || namespaceState.NamespaceHref != "" || namespaceState.PluralHref != "/clusters/prod%2Feu/namespaces" {
		t.Fatalf("Namespace failure breadcrumb = %+v", namespaceState)
	}
}
