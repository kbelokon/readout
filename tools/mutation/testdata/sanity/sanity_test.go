package sanity

import "testing"

func TestMutationCanary(t *testing.T) {
	if !KilledBoundary(10) {
		t.Fatal("KilledBoundary(10) = false, want true")
	}
	_ = LivedBoundary(10)
	if got := CompileBoundary("read", "out"); got != "readout" {
		t.Fatalf("CompileBoundary() = %q, want readout", got)
	}
}
