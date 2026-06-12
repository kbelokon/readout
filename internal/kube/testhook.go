package kube

// SetAllowLoopbackClusterURLForTest flips the loopback exception in the
// cluster-server-URL validator and returns a restore func. It exists SOLELY so
// other packages' tests (e.g. internal/web) can run their in-process fake
// apiservers on 127.0.0.1 httptest ports without the loopback guard rejecting
// them. Production never calls it: the guard defaults to rejecting loopback, and
// link-local/metadata stays rejected even while this exception is set, so the
// metadata-SSRF guard is never weakened. Pair it with t.Cleanup:
//
//	defer kube.SetAllowLoopbackClusterURLForTest(true)()
func SetAllowLoopbackClusterURLForTest(allow bool) func() {
	prev := clusterURLAllowLoopback
	clusterURLAllowLoopback = allow
	return func() { clusterURLAllowLoopback = prev }
}
