package fakekube

// scenario.go is the TYPED object-graph model the engine seeds from. It is the
// additive alternative to the embedded-JSON seedStore path:
// instead of //go:embed'd fixtures, a Scenario describes clusters -> namespaces
// / nodes -> typed Kubernetes objects, and Seed() (seed.go) turns one Cluster
// into the discovery + List + object + log + metrics responses the server
// serves. The referential-integrity validator (integrity.go) runs inside Seed
// so a forgotten link fails a test, never ships as a dead click.
//
// Shape decision (resolved per the unit's design-shape guidance): ONE
// fakekube.Server is ONE apiserver = ONE cluster (it is an httptest.Server),
// so Seed takes a single Cluster. A Scenario carries the WHOLE multi-cluster
// description (Clusters), and the demo (a later unit) starts one Server per
// Cluster, each fed its own Cluster value. Seed(Scenario) is intentionally not
// offered: one server must not serve two clusters' merged data.

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// Scenario is the whole multi-cluster description: the typed object graph for
// every cluster the demo serves. A later unit's scenario builder returns one
// Scenario; the demo runner starts one Server per Cluster and calls
// Server.Seed(cluster) on each.
type Scenario struct {
	Clusters []Cluster
}

// Cluster is ONE apiserver's typed object graph: its nodes (cluster-scoped),
// its namespaces (each carrying namespaced objects), other cluster-scoped
// objects (e.g. PersistentVolumes), the custom resource definitions whose
// discovery group-versions must be registered for their list routes to
// resolve, and the node-level metrics keyed to real node names.
//
// One Server.Seed(Cluster) builds the entire served surface for this cluster.
type Cluster struct {
	// Name is the cluster's display name (used by the demo's multi-cluster
	// surface; it does not affect the served API paths).
	Name string

	// Nodes are the cluster-scoped Node objects. Their names are the keys
	// NodeMetrics and Pod.spec.nodeName must resolve against.
	Nodes []runtime.Object

	// Namespaces hold the namespaced objects, grouped by namespace.
	Namespaces []Namespace

	// ClusterObjects are cluster-scoped objects other than Nodes (e.g.
	// PersistentVolume). They are served on cluster-scoped list/object routes.
	ClusterObjects []runtime.Object

	// CRDs registers custom-resource group-versions in discovery so the list
	// routes for their objects resolve instead of 404ing. A CR object placed
	// in a namespace (or ClusterObjects) whose GroupVersionKind is not covered
	// by a registered CRD is a dangling-discovery reference the validator
	// rejects.
	CRDs []CRD

	// NodeMetrics carry per-node usage keyed by node name. metrics.k8s.io is
	// not a module dependency, so metrics ride the JSON wire shape the existing
	// fixtures use (see seed.go); every NodeMetrics name must resolve to a Node.
	NodeMetrics []NodeMetric

	// DiscoveryResources advertise kinds in discovery WITHOUT seeding objects or
	// list routes for them — a real apiserver lists every registered type even
	// when a namespace holds none (the resource-types matrix shows Deployment /
	// ReplicaSet / CSINode, whose LIST routes 404 because nothing is seeded). A
	// CRD already advertises its own kind, so these are for BUILT-IN groups only.
	DiscoveryResources []DiscoveryResource

	// FillEmptyLists, when true, registers a zero-row list route (List + Table
	// forms) for EVERY namespace in the cluster crossed with every NAMESPACED
	// kind this cluster advertises in discovery, skipping pairs that already
	// carry objects. This mirrors a real apiserver, which answers an empty 200
	// list for any served kind in any namespace rather than a 404. The demo opts
	// in so the sidebar's per-namespace kind links never 404 on a kind that
	// namespace happens to hold none of. The base test cluster leaves it off:
	// its tests deliberately assert that a kind with no objects in a namespace
	// 404s (selective-404).
	FillEmptyLists bool
}

// DiscoveryResource advertises one built-in kind in discovery without a seeded
// object or list route. Group is "" for the core group ("" => /api/v1); a
// non-empty Group rides /apis/<Group>/<Version>. The resource LIST route is
// intentionally absent (it 404s), matching an apiserver that registers a type a
// namespace has zero objects of.
type DiscoveryResource struct {
	Group      string
	Version    string
	Kind       string
	Plural     string
	Singular   string
	Namespaced bool
}

// Namespace is one namespace and the objects living in it. Objects are typed
// Kubernetes values (corev1.Pod, appsv1.Deployment, ...) plus optional
// per-pod metrics keyed by pod name.
type Namespace struct {
	// Name is the namespace name. An empty name is invalid (the validator
	// rejects it).
	Name string

	// Labels ride the served Namespace object's metadata.labels.
	Labels map[string]string

	// Created, when set (RFC3339), rides the served Namespace object's
	// metadata.creationTimestamp (the cluster-overview age-bucket cell reads it).
	Created string

	// Unlisted, when true, serves this namespace's per-namespace object routes
	// (its pods etc.) but does NOT emit a synthetic Namespace object — so the
	// namespace stays OUT of the /api/v1/namespaces list and has no
	// /api/v1/namespaces/<name> object route. Used for list-render scaffolding
	// namespaces (the states / empty / big test namespaces) that must serve a
	// list route without appearing in the cluster's namespaces roster.
	Unlisted bool

	// Objects are the namespaced typed objects (Pods, Deployments, ReplicaSets,
	// Services, Ingresses, Events, Secrets, ConfigMaps, CronJobs, Jobs, PVCs,
	// custom resources, ...). Each object's namespace is taken from this
	// Namespace; an object carrying a conflicting metadata.namespace is a
	// dangling reference the validator rejects.
	Objects []runtime.Object

	// PodMetrics carry per-pod container usage keyed by pod name; every entry's
	// name must resolve to a Pod in this namespace.
	PodMetrics []PodMetric

	// EmptyLists names resource plurals this namespace must serve as a
	// REGISTERED but zero-row list (e.g. "pods" in the "empty" namespace), so a
	// list returns a 0-row Table/List instead of a 404. A kind with no objects
	// has no route otherwise; this declares the genuinely-empty list state.
	EmptyLists []string
}

// CRD registers one custom-resource group-version-kind in discovery so its
// list/object routes resolve. The plural is the resource segment in the served
// path (/apis/<Group>/<Version>/.../<Plural>).
type CRD struct {
	Group      string
	Version    string
	Kind       string
	Plural     string
	Namespaced bool
}

// PodMetric is one pod's container usage, the JSON-shape metrics.k8s.io
// PodMetrics the existing fixtures serve (metrics.k8s.io is not a module
// dependency). Name must resolve to a real Pod in the same namespace.
type PodMetric struct {
	Name       string
	Containers []ContainerMetric
}

// ContainerMetric is one container's CPU/memory usage (quantity strings such as
// "250m" / "128Mi"), matching the served PodMetrics container usage shape.
type ContainerMetric struct {
	Name   string
	CPU    string
	Memory string
}

// NodeMetric is one node's usage, the JSON-shape metrics.k8s.io NodeMetrics.
// Name must resolve to a real Node in the cluster.
type NodeMetric struct {
	Name   string
	CPU    string
	Memory string
}
