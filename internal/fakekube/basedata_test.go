package fakekube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCellObjectDeepCopyPreservesOverrides pins the runtime.Object contract for
// the base-cluster override wrapper: a deep copy must carry every override
// field. A copy that dropped skipRefIntegrity/tableOnly/listOnly would silently
// re-arm the waived integrity checks or collapse the divergent node Table/List
// split if any future seed path clones the object graph.
func TestCellObjectDeepCopyPreservesOverrides(t *testing.T) {
	base := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	cases := map[string]*cellObject{
		"withCellsNoRef": withCellsNoRef(base, "c", 1, "10m"),
		"tableOnlyNode":  tableOnlyNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}),
		"listOnlyNode":   listOnlyNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "127.0.0.1"}}),
	}

	for name, orig := range cases {
		t.Run(name, func(t *testing.T) {
			cp, ok := orig.DeepCopyObject().(*cellObject)
			if !ok {
				t.Fatalf("DeepCopyObject did not return *cellObject")
			}
			if cp.skipRefIntegrity != orig.skipRefIntegrity {
				t.Errorf("skipRefIntegrity not preserved: got %v want %v", cp.skipRefIntegrity, orig.skipRefIntegrity)
			}
			if cp.tableOnly != orig.tableOnly {
				t.Errorf("tableOnly not preserved: got %v want %v", cp.tableOnly, orig.tableOnly)
			}
			if cp.listOnly != orig.listOnly {
				t.Errorf("listOnly not preserved: got %v want %v", cp.listOnly, orig.listOnly)
			}
			if len(cp.cells) != len(orig.cells) {
				t.Errorf("cells not preserved: got %v want %v", cp.cells, orig.cells)
			}
		})
	}
}
