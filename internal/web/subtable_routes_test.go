package web

import (
	"net/http/httptest"
	"testing"

	"github.com/kbelokon/readout/internal/kube"
)

func TestSubtableLinksUseCanonicalResourceScopeAndEscaping(t *testing.T) {
	request := httptest.NewRequest("GET", "/clusters/test/pods", nil)
	app := &Server{}
	row := kube.Row{
		Cluster: "prod/eu",
		Cells:   []any{"display-name", "worker/a"},
		Object: map[string]any{"metadata": map[string]any{
			"name":      "object/name",
			"namespace": "team/a",
		}},
	}

	namespaced := kube.Table{
		Resource: kube.ResourceType{Plural: "pods/example", Namespaced: true},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Node"}},
		Rows:     []kube.Row{row},
	}
	view := app.buildSubtableView(request, &namespaced, "team/a")
	if got, want := view.Rows[0].Cells[0].Href, "/clusters/prod%2Feu/namespaces/team%2Fa/pods%2Fexample/object%2Fname"; got != want {
		t.Fatalf("namespaced object href = %q, want %q", got, want)
	}
	if got, want := view.Rows[0].Cells[1].Href, "/clusters/prod%2Feu/nodes/worker%2Fa"; got != want {
		t.Fatalf("Node href = %q, want %q", got, want)
	}

	// Namespace-like metadata on a cluster-scoped row must not change the
	// discovered resource's route scope.
	clusterScoped := kube.Table{
		Resource: kube.ResourceType{Plural: "widgets/example", Namespaced: false},
		Columns:  []kube.Column{{Name: "Name"}},
		Rows:     []kube.Row{row},
	}
	view = app.buildSubtableView(request, &clusterScoped, "")
	if got, want := view.Rows[0].Cells[0].Href, "/clusters/prod%2Feu/widgets%2Fexample/object%2Fname"; got != want {
		t.Fatalf("cluster-scoped object href = %q, want %q", got, want)
	}
}
