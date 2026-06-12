package kube

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Typing boundary for cluster data — the one rule this package follows:
// use the strongest typing the data allows.
//
//   - Where the kind is FIXED (pod/node metrics, Node, Pod, Secret,
//     ownerReferences, the cluster-registry response), decode once into a full
//     struct via runtime.DefaultUnstructuredConverter.FromUnstructured at a named
//     seam, then read typed fields.
//   - Where the kind is NOT known at compile time (the generic browsed resource:
//     list / detail / YAML / Table), stay on map[string]any but navigate it with
//     apimachinery's typed accessors — u.GetName()/GetLabels() and
//     unstructured.NestedString/NestedSlice/NestedMap — never hand-rolled
//     string-map walking.
//   - NEVER type a CRD-specific shape: an arbitrary custom-resource body has no
//     compile-time struct, so generic browsed-resource bodies and Table cell
//     VALUES stay dynamic by design.
//
// The point is a single visible seam (FromUnstructured) for known-kind side-data
// and zero reinvented accessors for the generic path.

const (
	AllNamespaces       = "_all"
	AllClusters         = "_all"
	SecretContentHidden = "**SECRET-CONTENT-HIDDEN-BY-READOUT**"
)

type ResourceType struct {
	Group       string
	Version     string
	APIVersion  string
	Kind        string
	Plural      string
	Singular    string
	Namespaced  bool
	ShortNames  []string
	Categories  []string
	Verbs       []string
	LastRefresh time.Time
}

func (rt *ResourceType) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: rt.Group, Version: rt.Version, Resource: rt.Plural}
}

func (rt *ResourceType) GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: rt.Group, Version: rt.Version, Kind: rt.Kind}
}

func (rt *ResourceType) Endpoint() string {
	return rt.Plural
}

func (rt *ResourceType) Key() string {
	return fmt.Sprintf("%s/%s/%s/%t", rt.Group, rt.Version, rt.Plural, rt.Namespaced)
}

type Cluster struct {
	Name   string
	URL    string
	Source Source
	Labels map[string]string
	Spec   map[string]any
	Client *Client
}

type Column struct {
	Name        string
	Type        string
	Format      string
	Description string
	Class       string
	Label       string
}

type Row struct {
	Cells   []any
	Object  map[string]any
	Cluster string
}

type Table struct {
	Resource ResourceType
	Columns  []Column
	Rows     []Row
	Clusters []string

	// RemainingItemCount mirrors the response's metadata.remainingItemCount:
	// with a ListOptions.Limit the apiserver returns the first chunk plus an
	// ESTIMATE of how many items it left out (nil when the list was complete or
	// the server does not paginate). The sidebar counts consume it as
	// len(Rows) + RemainingItemCount.
	RemainingItemCount *int64

	// ResourceVersion mirrors the response's list metadata.resourceVersion —
	// the consistency point of the list. A Live stream captures it from
	// the initial Table list and starts its watch there; for a decoded watch
	// event this is the EVENT's resourceVersion instead.
	ResourceVersion string
}

type ListOptions struct {
	Namespace     string
	LabelSelector string
	FieldSelector string

	// Limit caps the number of items the apiserver returns (the chunked-list
	// `?limit=N` parameter); 0 means no limit. A limited response carries
	// metadata.remainingItemCount + continue, which Table surfaces.
	Limit int64
}

// WatchOptions scope a Table watch (Client.WatchTable). ResourceVersion is
// the captured list resourceVersion the watch resumes from
// (Table.ResourceVersion of the initial list); empty starts at the server's
// current state with no replay. No FieldSelector: the one watch consumer (the
// Live stream) scopes by namespace + label selector only.
type WatchOptions struct {
	Namespace       string
	LabelSelector   string
	ResourceVersion string
}

// WatchEventType is the Kubernetes watch wire vocabulary. ERROR frames never
// surface as events: TableWatch.Next folds them into typed errors
// (ErrWatchGone for 410/Expired, a StatusError otherwise).
type WatchEventType string

const (
	WatchAdded    WatchEventType = "ADDED"
	WatchModified WatchEventType = "MODIFIED"
	WatchDeleted  WatchEventType = "DELETED"
	WatchBookmark WatchEventType = "BOOKMARK"
	WatchError    WatchEventType = "ERROR"
)

// WatchEvent is one decoded Table watch event. Data events carry a 1-row
// Table whose columnDefinitions are populated only in the stream's FIRST
// event — the consumer caches those columns for the rest of the stream.
// Bookmarks carry no rows; they exist to advance ResourceVersion.
type WatchEvent struct {
	Type  WatchEventType
	Table Table

	// ResourceVersion is the event's resourceVersion (the payload Table's
	// list metadata) — the consumer's last-seen RV for re-watching after a
	// clean EOF. Every event type carries it; BOOKMARK events carry nothing
	// else.
	ResourceVersion string
}

type LogOptions struct {
	Namespace  string
	Pod        string
	Container  string
	Timestamps bool
	TailLines  int64
}

type ResourceRef struct {
	Cluster    string
	Namespace  string
	Plural     string
	Name       string
	APIVersion string
}

type Object struct {
	Resource ResourceType
	Raw      map[string]any
}

func NewObject(rt *ResourceType, u *unstructured.Unstructured) Object {
	return Object{Resource: *rt, Raw: u.Object}
}

// u wraps the raw object map in an *unstructured.Unstructured so the accessors
// below read it through apimachinery's typed metadata getters instead of
// hand-rolled string-map navigation (the generic browsed-resource path).
func (o *Object) u() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: o.Raw}
}

func (o *Object) Name() string {
	return o.u().GetName()
}

func (o *Object) Namespace() string {
	return o.u().GetNamespace()
}

func (o *Object) UID() string {
	return string(o.u().GetUID())
}

func (o *Object) Kind() string {
	if k := o.u().GetKind(); k != "" {
		return k
	}
	return o.Resource.Kind
}

func (o *Object) Labels() map[string]string {
	return o.u().GetLabels()
}

func (o *Object) Annotations() map[string]string {
	return o.u().GetAnnotations()
}

func (o *Object) CreationTimestamp() string {
	ts, _, _ := unstructured.NestedString(o.Raw, "metadata", "creationTimestamp")
	return ts
}

// OwnerReferences returns the object's metadata.ownerReferences as typed
// metav1.OwnerReference values (via the apimachinery accessor) — the single
// typed read of ownerReferences anywhere; ownerLinks consumes it.
func (o *Object) OwnerReferences() []metav1.OwnerReference {
	return o.u().GetOwnerReferences()
}

func ToObjectMap(obj *unstructured.Unstructured) map[string]any {
	if obj == nil {
		return map[string]any{}
	}
	return obj.Object
}

func NormalizeAPIVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

func SplitAPIVersion(apiVersion string) (string, string) {
	group, version, ok := strings.Cut(apiVersion, "/")
	if !ok {
		return "", apiVersion
	}
	return group, version
}

// metricsItem is the local typed view of a metrics.k8s.io PodMetrics/NodeMetrics
// object — a known, fixed shape, so it is decoded once via FromUnstructured at
// the MetricsUsage seam rather than navigated as a string-map. It carries only
// the two fields the join needs; resource.Quantity parses the cpu/memory values
// (strictly more correct than the retired hand-rolled parser: it handles
// Pi/Ei/exponent suffixes too). PodMetrics has a per-container usage list;
// NodeMetrics has a single top-level usage map.
type metricsItem struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Usage      resourceUsage    `json:"usage"`
	Containers []containerUsage `json:"containers"`
}

type containerUsage struct {
	Name  string        `json:"name"`
	Usage resourceUsage `json:"usage"`
}

type resourceUsage struct {
	CPU    resource.Quantity `json:"cpu"`
	Memory resource.Quantity `json:"memory"`
}

// MetricsUsage decodes a metrics.k8s.io item (Pod or Node) and returns its
// namespace/name key plus the summed CPU (cores) and memory (bytes) usage as
// approximate float64s. This is the named seam that replaced the hand-rolled
// ParseResourceQuantity: the only conversion of a metrics object into typed
// values lives here.
func MetricsUsage(obj map[string]any) (key string, cpu, mem float64) {
	var item metricsItem
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &item); err != nil {
		return "", 0, 0
	}
	key = item.Metadata.Namespace + "/" + item.Metadata.Name
	if len(item.Containers) > 0 {
		for i := range item.Containers {
			cpu += item.Containers[i].Usage.CPU.AsApproximateFloat64()
			mem += item.Containers[i].Usage.Memory.AsApproximateFloat64()
		}
		return key, cpu, mem
	}
	return key, item.Usage.CPU.AsApproximateFloat64(), item.Usage.Memory.AsApproximateFloat64()
}

// ContainerUsage is one container's resolved usage from a PodMetrics
// `containers[]` entry: CPU in cores, memory in bytes.
type ContainerUsage struct {
	CPU    float64
	Memory float64
}

// PodContainerUsage decodes a PodMetrics item into per-container usage keyed
// by container name (the pod-detail containers table joins on it). It
// lives next to MetricsUsage so the metrics-object -> typed-values conversion
// stays at this one seam. An undecodable object or one without a containers
// list yields nil — the caller renders the no-metrics placeholder.
func PodContainerUsage(obj map[string]any) map[string]ContainerUsage {
	var item metricsItem
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &item); err != nil {
		return nil
	}
	if len(item.Containers) == 0 {
		return nil
	}
	out := make(map[string]ContainerUsage, len(item.Containers))
	for i := range item.Containers {
		c := &item.Containers[i]
		out[c.Name] = ContainerUsage{
			CPU:    c.Usage.CPU.AsApproximateFloat64(),
			Memory: c.Usage.Memory.AsApproximateFloat64(),
		}
	}
	return out
}
