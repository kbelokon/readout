package kube

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// recordedObservation is one captured observer call: the operation, whether the
// request errored, and that the elapsed time was a real (non-negative) duration.
type recordedObservation struct {
	operation string
	failed    bool
	elapsed   time.Duration
}

// observerRecorder collects observer callbacks. It is synchronized because
// discovery runs on a concurrent client-go goroutine under -race.
type observerRecorder struct {
	mu   sync.Mutex
	hits []recordedObservation
}

func (o *observerRecorder) observer() RequestObserver {
	return func(operation string, err error, elapsed time.Duration) {
		o.mu.Lock()
		o.hits = append(o.hits, recordedObservation{operation: operation, failed: err != nil, elapsed: elapsed})
		o.mu.Unlock()
	}
}

func (o *observerRecorder) operations() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var ops []string
	for _, h := range o.hits {
		ops = append(ops, h.operation)
	}
	return ops
}

func containsOp(ops []string, want string) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}

// TestNilObserverRequestsDoNotPanic pins the nil-safety contract: a Client built
// without an observer (every kube test path) performs its request methods
// without panicking. The observe helper is the single guard, so exercising a
// representative request and a denied short-circuit covers the lane.
func TestNilObserverRequestsDoNotPanic(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	if client.observe != nil {
		t.Fatal("freshly built Client must have no observer")
	}

	rt, err := client.FindResource(context.Background(), "pods", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background(), &rt, ListOptions{Namespace: "default"}); err != nil {
		t.Fatalf("List with nil observer: %v", err)
	}
	if _, err := client.Table(context.Background(), &rt, ListOptions{Namespace: "default"}); err != nil {
		t.Fatalf("Table with nil observer: %v", err)
	}
	// A denied clone with no observer must also short-circuit without panic.
	if _, err := client.Denied().List(context.Background(), &rt, ListOptions{Namespace: "default"}); err == nil {
		t.Fatal("denied List should refuse")
	}
}

// TestObserverFiresOnRequest pins that an installed observer records each
// observed operation (here List and a setup-only WatchTable) with a real
// elapsed time.
func TestObserverFiresOnRequest(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	rec := &observerRecorder{}
	client.SetObserver(rec.observer())

	rt, err := client.FindResource(context.Background(), "pods", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background(), &rt, ListOptions{Namespace: "default"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	ops := rec.operations()
	if !containsOp(ops, OpList) {
		t.Fatalf("observer never recorded a list operation: %v", ops)
	}
	rec.mu.Lock()
	for _, h := range rec.hits {
		if h.elapsed < 0 {
			t.Fatalf("observed a negative elapsed time: %v", h)
		}
	}
	rec.mu.Unlock()
}

// TestWithBearerCloneCarriesObserver pins the clone-propagation contract for the
// passthrough path: a WithBearer clone keeps the SAME observer the base carries,
// so passthrough requests stay attributed to the cluster the closure baked in. A
// single shared recorder proves both base and clone funnel to the same sink.
func TestWithBearerCloneCarriesObserver(t *testing.T) {
	f := newFakeAPIServer(t)
	base := f.client(t, false)
	rec := &observerRecorder{}
	base.SetObserver(rec.observer())

	clone, err := base.WithBearer("viewer-token")
	if err != nil {
		t.Fatalf("WithBearer: %v", err)
	}
	rt, err := clone.FindResource(context.Background(), "pods", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clone.List(context.Background(), &rt, ListOptions{Namespace: "default"}); err != nil {
		t.Fatalf("clone List: %v", err)
	}
	if !containsOp(rec.operations(), OpList) {
		t.Fatalf("WithBearer clone did not propagate the observer: %v", rec.operations())
	}
}

// TestManagerObserverFactoryBakesClusterName pins that the Manager's observer
// factory closes over each cluster's name: a factory that captures the name it
// is called with installs a distinct observer per Client, so the cluster the
// metric is attributed to comes from the closure, not from any call parameter.
func TestManagerObserverFactoryBakesClusterName(t *testing.T) {
	clientA := &Client{}
	clientB := &Client{}
	m := &Manager{clusters: map[string]*Cluster{
		"alpha": {Name: "alpha", Client: clientA},
		"beta":  {Name: "beta", Client: clientB},
	}}

	var mu sync.Mutex
	seen := map[string]string{} // operation-tagged sink -> cluster baked in
	m.SetRequestObserverFactory(func(cluster string) RequestObserver {
		return func(operation string, _ error, _ time.Duration) {
			mu.Lock()
			seen[cluster] = operation
			mu.Unlock()
		}
	})

	if clientA.observe == nil || clientB.observe == nil {
		t.Fatal("factory did not install observers on the cluster Clients")
	}
	clientA.observe(OpList, nil, 0)
	clientB.observe(OpGet, nil, 0)

	mu.Lock()
	defer mu.Unlock()
	if seen["alpha"] != OpList || seen["beta"] != OpGet {
		t.Fatalf("observers did not carry the baked cluster name: %#v", seen)
	}
}

// TestDeniedCloneCarriesObserver pins the same clone-propagation contract for the
// Denied path: the denied clone still observes, recording its short-circuit as a
// forbidden request rather than dropping the metric.
func TestDeniedCloneCarriesObserver(t *testing.T) {
	base, err := NewClient(&rest.Config{Host: "https://x"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rec := &observerRecorder{}
	base.SetObserver(rec.observer())

	rt := &ResourceType{Plural: "pods", Namespaced: true, Version: "v1", APIVersion: "v1", Kind: "Pod"}
	if _, err := base.Denied().List(context.Background(), rt, ListOptions{Namespace: "ns"}); !IsForbidden(err) {
		t.Fatalf("denied List should be forbidden, got %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.hits) != 1 || rec.hits[0].operation != OpList || !rec.hits[0].failed {
		t.Fatalf("denied clone did not observe a failed list: %#v", rec.hits)
	}
}
