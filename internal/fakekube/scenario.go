package fakekube

// scenario.go is the TYPED object-graph model the engine seeds from (design D3,
// D5). It is the additive alternative to the embedded-JSON seedStore path:
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

	// Objects are the namespaced typed objects (Pods, Deployments, ReplicaSets,
	// Services, Ingresses, Events, Secrets, ConfigMaps, CronJobs, Jobs, PVCs,
	// custom resources, ...). Each object's namespace is taken from this
	// Namespace; an object carrying a conflicting metadata.namespace is a
	// dangling reference the validator rejects.
	Objects []runtime.Object

	// PodMetrics carry per-pod container usage keyed by pod name; every entry's
	// name must resolve to a Pod in this namespace.
	PodMetrics []PodMetric
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
