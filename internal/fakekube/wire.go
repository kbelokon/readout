package fakekube

// wire.go exposes the engine's served WIRE BYTES for a typed Cluster WITHOUT
// starting a Server, so the hand-built-mux tests (the bespoke multi-cluster
// fan-out mux, the node/state/deep muxes) can serve the exact discovery docs +
// Table/List forms the engine would serve for a route, built from a typed
// Cluster, through their own delay/failure/search wrappers — instead of reading
// embedded JSON fixtures. It reuses the SAME validateCluster + buildStore
// pipeline New()/Seed() run; it does not duplicate discovery/table/list logic.
//
// Each bespoke mux owns a small focused typed Cluster for exactly what it
// asserts; WireResponses turns it into the route-keyed bytes the mux hands out.

import (
	"encoding/json"
	"fmt"
)

// WireList is the two served wire forms of one collection route: the
// meta.k8s.io Table form (negotiated via `Accept: ...as=Table`) and the plain
// List form. Either may be nil if the route serves only one form.
type WireList struct {
	Table []byte
	List  []byte
}

// Wire is the full set of served wire bytes for a typed Cluster: the discovery
// documents keyed by their served path (/api, /api/v1, /apis, each
// /apis/<group>/<version>, plus the metrics group docs) and every collection
// route's Table/List forms keyed by the served list path. A hand-mux serves
// Discovery[path] for discovery routes and Lists[path].Table / .List for
// collection routes through its own wrappers.
type Wire struct {
	Discovery map[string][]byte
	Lists     map[string]WireList
}

// WireResponses runs the same typed pipeline New()/Seed() run (validateCluster +
// buildStore) over one Cluster and returns the served discovery + List/Table
// wire bytes per route, so a bespoke test mux can reproduce the engine's bytes
// for a route from a typed Cluster without an embedded JSON fixture or a running
// Server. The returned Table/List bytes are exactly what the engine's list
// handler serves for an `as=Table` vs plain Accept.
func WireResponses(c *Cluster) (*Wire, error) {
	reg := kindRegistry(c.CRDs)
	if err := validateCluster(c, reg); err != nil {
		return nil, err
	}
	st, err := buildStore(c, reg)
	if err != nil {
		return nil, err
	}

	w := &Wire{
		Discovery: make(map[string][]byte, len(st.discovery)),
		Lists:     make(map[string]WireList, len(st.lists)),
	}
	for path, data := range st.discovery {
		w.Discovery[path] = data
	}
	for path, ls := range st.lists {
		wl := WireList{}
		if ls.table != nil {
			data, err := json.Marshal(ls.table)
			if err != nil {
				return nil, fmt.Errorf("fakeapi: marshal table for %s: %w", path, err)
			}
			wl.Table = data
		}
		if ls.list != nil {
			data, err := json.Marshal(ls.list)
			if err != nil {
				return nil, fmt.Errorf("fakeapi: marshal list for %s: %w", path, err)
			}
			wl.List = data
		}
		w.Lists[path] = wl
	}
	return w, nil
}
