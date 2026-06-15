package fakekube

// basedata.go is the TYPED base test cluster: the default seed New() builds
// when no Seed(Cluster) is supplied. It replaces the 44 hand-JSON base fixtures
// seedStore() used to read with ONE typed object graph in the same style as
// internal/demo — corev1.Pod, appsv1.ReplicaSet, batchv1.CronJob, … — fed
// through the same buildStore/Seed machinery the demo uses. The Go test suite
// and the Playwright e2e are the faithfulness oracle: every literal those
// assert (pod names, the 600-row big namespace, the 40-line pod log, worker-1's
// External-IP, the configmap key count, the masked secret values) is reproduced
// here, through typed objects plus EXPLICIT literal Table cells where a printer
// column cannot be derived from a typed object (a compressed Age like "17d", a
// restart-with-ago string, the hand-summed ConfigMap key count). No JSON.
//
// The literal-cell mechanism is withCells(obj, cells...): it wraps a typed
// object with the exact meta.k8s.io Table-cell values its row must carry. The
// seeder unwraps it (collectObjects) so discovery / List / integrity see the
// bare typed object, and threads the cells to the Table builder (tables.go).

import (
	"encoding/base64"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ---------------------------------------------------------------------------
// Literal Table-cell override.
// ---------------------------------------------------------------------------

// cellObject wraps a typed object with the EXPLICIT meta.k8s.io Table-cell
// values its row must serve and an optional explicit pod-log payload. It is a
// runtime.Object so it can sit in a Namespace.Objects / Cluster.Nodes slice; the
// seeder unwraps it via unwrapCellObject before any GVK/meta/marshal/integrity
// read, so the override never affects discovery, the List form, or validation.
type cellObject struct {
	inner   runtime.Object
	cells   []any
	logText string
	// skipRefIntegrity exempts this object from the reference-resolution
	// integrity checks (Event involvedObject in particular). The base cluster's
	// events legitimately reference churned pods that are not seeded — a chronic
	// BackOff event about a pod long gone — which a real apiserver never
	// validates. The demo keeps the check (its events reference live pods).
	skipRefIntegrity bool
	// tableOnly / listOnly place this object in only one wire form of its
	// collection route. The nodes route diverges: the worker-1 Node rides the
	// Table (the list page, count, External-IP cell, object route) while the
	// 127.0.0.1 Node rides the List (the pods `join=nodes` custom-column join
	// keys on node.metadata.name). An object route is still served for both.
	tableOnly bool
	listOnly  bool
}

// withCells wraps obj with explicit Table cells.
func withCells(obj runtime.Object, cells ...any) *cellObject {
	return &cellObject{inner: obj, cells: cells}
}

// withCellsAndLog wraps obj with explicit Table cells and an explicit pod-log
// payload (pods whose /log route must serve a specific fixture body).
func withCellsAndLog(obj runtime.Object, logText string, cells ...any) *cellObject {
	return &cellObject{inner: obj, cells: cells, logText: logText}
}

// withEventCells wraps an Event with explicit Table cells and exempts it from
// involvedObject reference-integrity (the event references a pod not seeded in
// the base cluster, as real chronic events do).
func withEventCells(obj runtime.Object, cells ...any) *cellObject {
	return &cellObject{inner: obj, cells: cells, skipRefIntegrity: true}
}

// withCellsNoRef wraps obj with explicit Table cells and exempts it from
// reference-integrity (e.g. the frontend Service whose selector points at pods
// not seeded in the base cluster — a real selector may match zero current pods).
func withCellsNoRef(obj runtime.Object, cells ...any) *cellObject {
	return &cellObject{inner: obj, cells: cells, skipRefIntegrity: true}
}

// GetObjectKind delegates to the inner object so the wrapper resolves its GVK
// identically to the bare object.
func (c *cellObject) GetObjectKind() schema.ObjectKind { return c.inner.GetObjectKind() }

// DeepCopyObject deep-copies the inner object and rewraps it, preserving the
// override (runtime.Object contract).
func (c *cellObject) DeepCopyObject() runtime.Object {
	return &cellObject{inner: c.inner.DeepCopyObject(), cells: c.cells, logText: c.logText}
}

// cellOverride is the unwrapped base-cluster override carried alongside the bare
// inner object: explicit Table cells, an explicit pod-log payload, the
// reference-integrity waiver, and the table/list visibility flags.
type cellOverride struct {
	cells     []any
	logText   string
	tableOnly bool
	listOnly  bool
	inner     runtime.Object
}

// unwrapCell returns an object's override + bare inner object. A non-wrapped
// object returns a zero override carrying the object itself.
func unwrapCell(obj runtime.Object) cellOverride {
	if co, ok := obj.(*cellObject); ok {
		return cellOverride{cells: co.cells, logText: co.logText, tableOnly: co.tableOnly, listOnly: co.listOnly, inner: co.inner}
	}
	return cellOverride{inner: obj}
}

// unwrapCellObject returns an object's explicit cells + log payload and the bare
// inner object (the integrity validator's accessor; flags read via unwrapCell).
func unwrapCellObject(obj runtime.Object) (cells []any, logText string, inner runtime.Object) {
	o := unwrapCell(obj)
	return o.cells, o.logText, o.inner
}

// node builders for the divergent nodes route.
func tableOnlyNode(obj runtime.Object) *cellObject { return &cellObject{inner: obj, tableOnly: true} }
func listOnlyNode(obj runtime.Object) *cellObject  { return &cellObject{inner: obj, listOnly: true} }

// ---------------------------------------------------------------------------
// Base cluster.
// ---------------------------------------------------------------------------

func i32(v int32) *int32    { return &v }
func bptr(v bool) *bool     { return &v }
func sptr(v string) *string { return &v }

// ts builds a metav1.Time from an RFC3339 literal (the fixtures' fixed
// creationTimestamps, reproduced verbatim so age-bucket render is deterministic).
func ts(rfc3339 string) metav1.Time {
	t, err := timeParse(rfc3339)
	if err != nil {
		panic("fakeapi basedata: bad timestamp " + rfc3339 + ": " + err.Error())
	}
	return metav1.NewTime(t)
}

// baseTestCluster is the typed object graph the default New() seed serves. It
// reproduces, route for route, what seedStore() used to serve from the embedded
// base fixtures:
//
//   - default namespace: pods (nginx, my-app), services (frontend, kubernetes),
//     configmaps (app-config + 2), secrets (my-secret, parse-prod, empty),
//     ingresses (3), cronjobs (3), jobs (3), events (4);
//   - states namespace: 4 status-tone pods (Table-only); empty namespace: a
//     zero-row pods list; big namespace: 600 pods + 600 events (bigfixtures.go);
//   - cluster scope: the worker-1 Node (InternalIP 127.0.0.1 + External-IP
//     203.0.113.7), 3 PersistentVolumes;
//   - metrics: nginx PodMetrics, worker-1 NodeMetrics;
//   - discovery-only: Deployment, ReplicaSet (apps/v1), CSINode
//     (storage.k8s.io), plus the Certificate CRD — advertised types whose LIST
//     routes 404 (no objects), matching the resource-types matrix.
func baseTestCluster() Cluster {
	return Cluster{
		Name:               "test",
		Nodes:              baseNodes(),
		NodeMetrics:        []NodeMetric{{Name: "worker-1", CPU: "1", Memory: "256Mi"}},
		ClusterObjects:     basePersistentVolumes(),
		CRDs:               baseCRDs(),
		DiscoveryResources: baseDiscoveryResources(),
		Namespaces: []Namespace{
			baseDefaultNamespace(),
			baseStatesNamespace(),
			baseEmptyNamespace(),
			baseBigNamespace(),
			{Name: "kube-system", Labels: map[string]string{}, Created: "2024-01-02T00:00:00Z"},
			{Name: "my-app", Labels: map[string]string{"app": "my-app"}, Created: "2024-01-03T00:00:00Z"},
		},
	}
}

// baseNodes are the divergent nodes route, reproducing the embedded seed's
// Table-vs-List split:
//
//   - worker-1 (tableOnly): the nodes LIST page row + count + the External-IP
//     203.0.113.7 cell (column-visibility e2e) + the /nodes/worker-1 object
//     route (the name click + kube Get) + the NodeMetrics target.
//   - 127.0.0.1 (listOnly): the node the default pods run on, so the pods
//     `join=nodes` custom-column join (node.metadata.name) surfaces 127.0.0.1.
//
// An object route is served for both; the count reads the Table (worker-1 only
// → 1), the join reads the List (127.0.0.1).
func baseNodes() []runtime.Object {
	worker1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "worker-1",
			CreationTimestamp: ts("2024-02-01T08:00:00Z"),
			Labels: map[string]string{
				"kubernetes.io/hostname": "worker-1",
				"kubernetes.io/os":       "linux",
				"kubernetes.io/arch":     "amd64",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "HasMemory"},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, Reason: "HasDisk"},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, Reason: "HasPID"},
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "Ready"},
			},
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8047476Ki"), corev1.ResourcePods: resource.MustParse("110")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1930m"), corev1.ResourceMemory: resource.MustParse("7393076Ki"), corev1.ResourcePods: resource.MustParse("110")},
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:            "amd64",
				ContainerRuntimeVersion: "containerd://1.7.11",
				KernelVersion:           "6.1.0-18-generic",
				KubeletVersion:          "v1.29.2",
				OperatingSystem:         "linux",
				OSImage:                 "Linux",
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "127.0.0.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.7"},
				{Type: corev1.NodeHostName, Address: "worker-1"},
			},
		},
	}
	joinNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "127.0.0.1",
			CreationTimestamp: ts("2024-01-01T00:00:00Z"),
			Labels:            map[string]string{"kubernetes.io/hostname": "127.0.0.1"},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}},
		},
	}
	return []runtime.Object{
		tableOnlyNode(worker1),
		listOnlyNode(joinNode),
	}
}

// basePersistentVolumes are the 3 cluster-scoped PVs (Bound/Released/Failed
// tones, uuid-shaped names). Explicit cells carry the printer's compressed Age
// and the empty Reason column the typed object cannot supply faithfully.
func basePersistentVolumes() []runtime.Object {
	pv := func(name, capacity, reclaim, phase, claimNS, claim, reason, age string) runtime.Object {
		obj := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: ts("2026-05-24T12:00:00Z")},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimPolicy(reclaim),
				StorageClassName:              "do-block-storage",
				ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: claim},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.PersistentVolumePhase(phase), Reason: reason},
		}
		return withCells(obj, name, capacity, "RWO", reclaim, phase, claimNS+"/"+claim, "do-block-storage", reason, age)
	}
	return []runtime.Object{
		pv("pvc-9f2c4e7a-1b3d-4f6a-8c0e-2d5b7a3f9e2c", "100Gi", "Delete", "Bound", "slr-prod", "redis-data-redis-master-0", "", "17d"),
		pv("pvc-7e1d3f9b-4c6a-4d8e-9f2b-0a3c5e7d9b1f", "250Gi", "Retain", "Released", "observability", "vmstorage-0", "", "64d"),
		pv("pvc-1c4f7a9d-2e5b-4c8a-b6d9-3f0e2a4c6b8e", "10Gi", "Delete", "Failed", "staging", "pg-data", "", "1y127d"),
	}
}

// baseCRDs registers the Certificate CRD (cert-manager.io) so the resource-types
// matrix shows it WITH the CRD badge (its list route 404s, no objects seeded).
func baseCRDs() []CRD {
	return []CRD{
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Plural: "certificates", Namespaced: true},
	}
}

// baseDiscoveryResources advertise the built-in kinds the resource-types matrix
// shows that carry no seeded objects: Deployment + ReplicaSet (apps/v1, so the
// apiVersion=apps/v1 pin resolves two namespaced kinds) and CSINode
// (storage.k8s.io, cluster-scoped). Their LIST routes 404 (counts test).
func baseDiscoveryResources() []DiscoveryResource {
	return []DiscoveryResource{
		{Group: "apps", Version: "v1", Kind: "Deployment", Plural: "deployments", Singular: "deployment", Namespaced: true},
		{Group: "apps", Version: "v1", Kind: "ReplicaSet", Plural: "replicasets", Singular: "replicaset", Namespaced: true},
		{Group: "storage.k8s.io", Version: "v1", Kind: "CSINode", Plural: "csinodes", Singular: "csinode", Namespaced: false},
	}
}

// baseDefaultNamespace is the rich "default" namespace: every kind the base
// fixtures served, with explicit literal Table cells reproducing the printer
// columns. Labels {team: core} ride the namespace object.
func baseDefaultNamespace() Namespace {
	return Namespace{
		Name:    "default",
		Labels:  map[string]string{"team": "core"},
		Created: "2024-01-01T00:00:00Z",
		Objects: baseDefaultObjects(),
		PodMetrics: []PodMetric{
			{Name: "nginx", Containers: []ContainerMetric{{Name: "nginx", CPU: "250m", Memory: "128Mi"}}},
		},
	}
}

func baseDefaultObjects() []runtime.Object {
	var all []runtime.Object
	all = append(all, baseDefaultPods()...)
	all = append(all, baseDefaultServices()...)
	all = append(all, baseDefaultConfigMaps()...)
	all = append(all, baseDefaultSecrets()...)
	all = append(all, baseDefaultIngresses()...)
	all = append(all, baseDefaultCronJobs()...)
	all = append(all, baseDefaultJobs()...)
	all = append(all, baseDefaultEvents()...)
	return all
}

// baseDefaultPods are nginx + my-app: full Pod objects (the join reads podIP
// 127.0.0.1 + nodeName worker-1; the detail page reads nginx's annotations).
// Explicit cells reproduce the pods Table row exactly (["nginx","1/1","Running",
// "0","10m"] / ["my-app",...,"5m"]); nginx carries the 40-line fixture log.
func baseDefaultPods() []runtime.Object {
	nginx := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nginx",
			Namespace:         "default",
			CreationTimestamp: ts("2024-03-01T10:00:00Z"),
			Labels:            map[string]string{"app": "nginx"},
			Annotations: map[string]string{
				"example.com/note": "generic-annotation",
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Pod","metadata":{"labels":{"app":"nginx"},"name":"nginx","namespace":"default"},"spec":{"containers":[{"image":"nginx:1.27","name":"nginx","ports":[{"containerPort":80,"protocol":"TCP"}]}]}}` + "\n",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "127.0.0.1",
			Containers: []corev1.Container{{
				Name:  "nginx",
				Image: "nginx:1.27",
				Ports: []corev1.ContainerPort{{ContainerPort: 80, Protocol: corev1.ProtocolTCP}},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "127.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "nginx",
				Ready:        true,
				RestartCount: 0,
				Image:        "nginx:1.27",
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ts("2024-03-01T10:00:05Z")}},
			}},
		},
	}
	myApp := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app",
			Namespace:         "default",
			CreationTimestamp: ts("2024-03-02T11:30:00Z"),
			Labels:            map[string]string{"app": "my-app"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "127.0.0.1",
			Containers: []corev1.Container{{Name: "my-app", Image: "my-app:latest"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "127.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "my-app", Ready: true, RestartCount: 0, Image: "my-app:latest",
			}},
		},
	}
	return []runtime.Object{
		withCellsAndLog(nginx, basePodLog, "nginx", "1/1", "Running", "0", "10m"),
		withCells(myApp, "my-app", "1/1", "Running", "0", "5m"),
	}
}

// baseDefaultServices are frontend (with an app:frontend selector → the
// Selector-column chip) + kubernetes (selectorless → the muted "—"). frontend's
// selector points at no seeded pod (a real selector may match zero current
// pods), so it rides withCellsNoRef to waive the selector-match integrity.
func baseDefaultServices() []runtime.Object {
	frontend := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "default", CreationTimestamp: ts("2026-05-23T12:00:00Z"), Labels: map[string]string{"app": "frontend"}},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.10",
			Selector:  map[string]string{"app": "frontend"},
			Ports:     []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstrFromInt(8080)}},
		},
	}
	kubernetes := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", CreationTimestamp: ts("2026-05-05T12:00:00Z"), Labels: map[string]string{"component": "apiserver"}},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.1",
			Ports:     []corev1.ServicePort{{Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstrFromInt(6443)}},
		},
	}
	return []runtime.Object{
		withCellsNoRef(frontend, "frontend", "ClusterIP", "10.96.0.10", "80/TCP", "12d"),
		withCells(kubernetes, "kubernetes", "ClusterIP", "10.96.0.1", "443/TCP", "30d"),
	}
}

// baseDefaultConfigMaps are app-config (4 data + 1 binaryData = the e2e's 5-key
// chip cell), kube-root-ca.crt (1 key), pending-cleanup-marker (0 keys). The
// FULL ConfigMap data rides each row so the keys-cell chips + the bulk YAML
// download (port: 1337) read real key material. The Data cell is the literal
// fixture count (5/1/0), hand-summed across data + binaryData.
func baseDefaultConfigMaps() []runtime.Object {
	appConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "default", CreationTimestamp: ts("2026-05-24T12:00:00Z"), Labels: map[string]string{"app": "frontend"}},
		Data: map[string]string{
			"application.yaml":   "server:\n  port: 1337\n  mode: production\n",
			"logging.conf":       "level=info\n",
			"feature-gates.json": `{"newNav":true,"betaSearch":false}`,
			"cors.yaml":          "origins:\n  - https://example.com\n",
		},
		BinaryData: map[string][]byte{
			"ca.der": mustBase64("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+Pw=="),
		},
	}
	kubeRootCA := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "default", CreationTimestamp: ts("2026-05-05T12:00:00Z")},
		Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\nMIIBfakefixturecertbytesforreadouttests\n-----END CERTIFICATE-----\n"},
	}
	pending := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-cleanup-marker", Namespace: "default", CreationTimestamp: ts("2026-03-14T12:00:00Z")},
	}
	return []runtime.Object{
		withCells(appConfig, "app-config", 5, "17d"),
		withCells(kubeRootCA, "kube-root-ca.crt", 1, "30d"),
		withCells(pending, "pending-cleanup-marker", 0, "88d"),
	}
}

// baseDefaultSecrets are my-secret (2 keys), parse-prod (4 keys),
// rotation-marker-empty (0). The FULL data (base64 wire values) rides each row
// so the bulk download masks real material; my-secret carries the
// last-applied-configuration annotation the detail/object route surfaces.
func baseDefaultSecrets() []runtime.Object {
	mySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-secret", Namespace: "default", CreationTimestamp: ts("2024-03-01T10:00:00Z"), Labels: map[string]string{},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"c3VwZXItc2VjcmV0LXZhbHVl"}}`,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password":  []byte("super-secret-value"),
			"api-token": []byte("token"),
		},
	}
	parseProd := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "parse-prod", Namespace: "default", CreationTimestamp: ts("2024-02-13T10:00:00Z"), Labels: map[string]string{}},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"MONGODB_HOST":     []byte("mongodb.internal.example"),
			"MONGODB_PASSWORD": []byte("hunter2-export-grade"),
			"PARSE_MASTER_KEY": []byte("master-key-sentinel-0xCAFE"),
			"S3_ACCESS_KEY":    []byte("AKIAFAKEACCESSKEY"),
		},
	}
	emptySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rotation-marker-empty", Namespace: "default", CreationTimestamp: ts("2024-03-01T07:00:00Z"), Labels: map[string]string{}},
		Type:       corev1.SecretTypeOpaque,
	}
	return []runtime.Object{
		withCells(mySecret, "my-secret", "Opaque", 2, "5m"),
		withCells(parseProd, "parse-prod", "Opaque", 4, "17d"),
		withCells(emptySecret, "rotation-marker-empty", "Opaque", 0, "3h"),
	}
}

// baseDefaultIngresses are slr-www (multi-host + TLS), admin-internal,
// preview-env (<pending> address). Each Ingress backend references the frontend
// service (which exists), so the integrity validator's Ingress->Service hop
// resolves. Explicit cells carry the printer's hosts join / address / ports.
func baseDefaultIngresses() []runtime.Object {
	ing := func(name, class string, hosts []string, address, ports, age string, tls bool) runtime.Object {
		rules := make([]networkingv1.IngressRule, 0, len(hosts))
		pathType := networkingv1.PathTypePrefix
		for _, h := range hosts {
			rules = append(rules, networkingv1.IngressRule{
				Host: h,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "frontend", Port: networkingv1.ServiceBackendPort{Number: 80}}},
				}}}},
			})
		}
		obj := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: ts("2026-05-24T12:00:00Z")},
			Spec:       networkingv1.IngressSpec{IngressClassName: sptr(class), Rules: rules},
		}
		if tls {
			obj.Spec.TLS = []networkingv1.IngressTLS{{Hosts: hosts, SecretName: "tls-" + name}}
		}
		if address != "<pending>" {
			obj.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: address}}
		}
		hostJoin := joinComma(hosts)
		return withCells(obj, name, class, hostJoin, address, ports, age)
	}
	return []runtime.Object{
		ing("slr-www", "nginx", []string{"sexlikereal.com", "www.sexlikereal.com", "m.sexlikereal.com", "cdn.sexlikereal.com"}, "45.55.107.21", "80, 443", "17d", true),
		ing("admin-internal", "nginx-internal", []string{"admin.internal.slr"}, "10.245.0.7", "80", "17d", false),
		ing("preview-env", "nginx", []string{"preview-8842.dev.slr"}, "<pending>", "80", "6m", false),
	}
}

// baseDefaultCronJobs are an Active, a Suspended, and a never-ran CronJob.
// Explicit cells carry the printer's schedule / suspend / active / lastrun.
func baseDefaultCronJobs() []runtime.Object {
	active := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-billing-queues", Namespace: "default", CreationTimestamp: ts("2026-05-24T12:00:00Z")},
		Spec:       batchv1.CronJobSpec{Schedule: "*/1 * * * *", Suspend: bptr(false)},
		Status:     batchv1.CronJobStatus{Active: []corev1.ObjectReference{{Kind: "Job", Name: "cron-billing-queues-29678683", Namespace: "default"}}, LastScheduleTime: tptr(ts("2026-06-10T11:59:01Z"))},
	}
	suspended := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-legacy-cleanup", Namespace: "default", CreationTimestamp: ts("2025-02-03T12:00:00Z")},
		Spec:       batchv1.CronJobSpec{Schedule: "0 4 * * 0", Suspend: bptr(true)},
		Status:     batchv1.CronJobStatus{LastScheduleTime: tptr(ts("2026-04-30T04:00:00Z"))},
	}
	never := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-new-feature-warmup", Namespace: "default", CreationTimestamp: ts("2026-06-10T09:00:00Z")},
		Spec:       batchv1.CronJobSpec{Schedule: "30 6 * * *", Suspend: bptr(false)},
	}
	return []runtime.Object{
		withCells(active, "cron-billing-queues", "*/1 * * * *", false, 1, "59s", "17d"),
		withCells(suspended, "cron-legacy-cleanup", "0 4 * * 0", true, 0, "41d", "1y127d"),
		withCells(never, "cron-new-feature-warmup", "30 6 * * *", false, 0, "<none>", "3h"),
	}
}

// baseDefaultJobs are a Complete, a Failed (BackoffLimitExceeded), and a Running
// (8/10) Job. Explicit cells carry the printer's status / completions / duration.
func baseDefaultJobs() []runtime.Object {
	complete := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-billing-queues-29678683", Namespace: "default", CreationTimestamp: ts("2026-06-10T11:58:00Z")},
		Spec:       batchv1.JobSpec{Completions: i32(1), BackoffLimit: i32(6)},
		Status:     batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached"}}},
	}
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-export-once-20260605", Namespace: "default", CreationTimestamp: ts("2026-06-05T12:00:00Z")},
		Spec:       batchv1.JobSpec{Completions: i32(1), BackoffLimit: i32(6)},
		Status: batchv1.JobStatus{Failed: 7, Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}},
	}
	running := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-embeddings-reindex-29677500", Namespace: "default", CreationTimestamp: ts("2026-06-10T11:22:00Z")},
		Spec:       batchv1.JobSpec{Completions: i32(10), Parallelism: i32(2), BackoffLimit: i32(6)},
		Status:     batchv1.JobStatus{Active: 2, Succeeded: 8},
	}
	return []runtime.Object{
		withCells(complete, "cron-billing-queues-29678683", "Complete", "1/1", "42s", "2m"),
		withCells(failed, "legacy-export-once-20260605", "Failed", "0/1", "46m", "5d"),
		withCells(running, "ml-embeddings-reindex-29677500", "Running", "8/10", "38m", "38m"),
	}
}

// baseDefaultEvents are the 4 events the events-list screen + the events count
// (4) read: a count-aggregate Warning, a series-shape FailedScheduling, a Normal
// Scheduled (which references nginx — the one seeded pod — so the nginx detail
// Events tab's involvedObject.name=nginx field-selector List resolves to exactly
// it), and a tight-burst Unhealthy. The three Warning events reference churned
// pods not seeded here (a real chronic event); they ride withEventCells so the
// involvedObject reference-integrity is waived for them. The list renderer
// re-decodes each event object (involvedObject / count / timestamps) for its
// cells, so the explicit cells matter only for sort/TSV fallback.
func baseDefaultEvents() []runtime.Object {
	ev := func(name, evType, reason, message, refName, age string, count int32, lastTS, firstTS string, series bool) runtime.Object {
		e := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: ts(firstTS)},
			Type:           evType,
			Reason:         reason,
			Message:        message,
			Count:          count,
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: refName, Namespace: "default"},
		}
		if series {
			e.EventTime = metav1.NewMicroTime(ts(firstTS).Time)
			e.Series = &corev1.EventSeries{Count: 12, LastObservedTime: metav1.NewMicroTime(ts(lastTS).Time)}
		} else {
			e.FirstTimestamp = ts(firstTS)
			e.LastTimestamp = ts(lastTS)
		}
		return withEventCells(e, age, evType, reason, "pod/"+refName, message)
	}
	return []runtime.Object{
		ev("ugc-backend-8b9fc9d44-nxxz9.0001", "Warning", "BackOff",
			"Back-off restarting failed container ugc-backend in pod ugc-backend-8b9fc9d44-nxxz9_default(7f3c9a1e-8b2d-4f6a-9c0e-1d5b7a3f9e2c)",
			"ugc-backend-8b9fc9d44-nxxz9", "3m", 141, "2026-06-10T11:57:00Z", "2026-06-08T19:00:00Z", false),
		ev("ml-batch-inference-8d9f7c6b4-p4q5r.0002", "Warning", "FailedScheduling",
			"0/20 nodes are available: 3 Insufficient cpu, 5 Insufficient memory, 12 node(s) didn't match Pod's node affinity/selector.",
			"ml-batch-inference-8d9f7c6b4-p4q5r", "24s", 12, "2026-06-10T11:59:36Z", "2026-06-10T11:56:00Z", true),
		// The Scheduled event references nginx so the nginx detail Events tab
		// (involvedObject.name=nginx) resolves to exactly this row.
		ev("nginx.0003", "Normal", "Scheduled",
			"Successfully assigned default/nginx to 127.0.0.1",
			"nginx", "59s", 1, "2026-06-10T11:59:01Z", "2026-06-10T11:59:01Z", false),
		ev("slr-frontend-f87f7c686-44xxr.0004", "Warning", "Unhealthy",
			`Liveness probe failed: Get "http://10.151.21.38:3000/healthz": context deadline exceeded`,
			"slr-frontend-f87f7c686-44xxr", "45s", 5, "2026-06-10T11:59:15Z", "2026-06-10T11:58:30Z", false),
	}
}

// baseStatesNamespace is the status-tone pods namespace (Table-only). The 4 pods
// exercise ContainerCreating / Terminating / degraded / steady tones; explicit
// cells reproduce the printer rows verbatim (incl. the "5 (2m ago)" restart and
// the "2/3" partial-ready). No List form is needed (the resource-list page
// always negotiates as=Table), so a minimal Pod object rides each row.
func baseStatesNamespace() Namespace {
	pod := func(name, ready, status, restarts, age, created string) runtime.Object {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "states", CreationTimestamp: ts(created), Labels: map[string]string{"app": "web"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "web:v1"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		return withCells(p, name, ready, status, restarts, age)
	}
	return Namespace{
		Name:     "states",
		Labels:   map[string]string{"app": "web"},
		Unlisted: true,
		Objects: []runtime.Object{
			pod("web-creating-7c9f7cd495-6fff6", "0/1", "ContainerCreating", "0", "3s", "2026-06-04T11:59:57Z"),
			pod("web-terminating-7c9f7cd495-aaaaa", "1/1", "Terminating", "0", "2d", "2026-06-02T12:00:00Z"),
			pod("web-degraded-7c9f7cd495-bbbbb", "2/3", "Running", "5 (2m ago)", "9h", "2026-06-04T03:00:00Z"),
			pod("web-steady-7c9f7cd495-ccccc", "1/1", "Running", "0", "5h", "2026-06-04T07:00:00Z"),
		},
	}
}

// baseEmptyNamespace serves a registered zero-row pods list (the genuinely-empty
// list state) and nothing else — its services list stays absent (404).
func baseEmptyNamespace() Namespace {
	return Namespace{
		Name:       "empty",
		Labels:     map[string]string{"app.kubernetes.io/name": "empty"},
		Unlisted:   true,
		EmptyLists: []string{"pods"},
	}
}

// baseBigNamespace holds 600 pods + 600 events so the list crosses readout's
// virtualization threshold. The rows reuse bigfixtures.go's exact identities and
// long-message material via explicit cells, so the windowing e2e (big-pod-NNNN
// names, the '600' count, the clamp-message rows) reads the same data the
// hand-built Table produced — now from typed objects.
func baseBigNamespace() Namespace {
	objects := make([]runtime.Object, 0, 2*bigRowCount)
	for i := 1; i <= bigRowCount; i++ {
		name := bigPodName(i)
		status, ready, restarts := "Running", "1/1", "0"
		if i%7 == 0 {
			status, ready, restarts = "Pending", "0/1", "0"
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "big", CreationTimestamp: ts("2026-06-08T12:00:00Z"), Labels: map[string]string{"app": "big"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:v1"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodPhase(status)},
		}
		objects = append(objects, withCells(pod, name, ready, status, restarts, "10m"))
	}
	for i := 1; i <= bigRowCount; i++ {
		pod := bigPodName(i)
		evType, reason := "Normal", "Scheduled"
		message := bigEventMessageNormal(pod, i)
		if i%5 == 0 {
			evType, reason = "Warning", "BackOff"
			message = bigEventMessageWarning(pod, i)
		}
		e := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: pod + ".ev1", Namespace: "big", CreationTimestamp: ts("2026-06-08T12:00:00Z")},
			Type:           evType,
			Reason:         reason,
			Message:        message,
			Count:          1,
			FirstTimestamp: ts("2026-06-08T12:00:00Z"),
			LastTimestamp:  ts("2026-06-08T12:05:00Z"),
			Source:         corev1.EventSource{Component: "default-scheduler"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod, Namespace: "big"},
		}
		objects = append(objects, withCells(e, "5m", evType, reason, "pod/"+pod, message))
	}
	return Namespace{
		Name:     "big",
		Labels:   map[string]string{"app.kubernetes.io/name": "big"},
		Unlisted: true,
		Objects:  objects,
	}
}

// joinComma joins host lists for the Ingress Hosts printer cell.
func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

// intstrFromInt builds an IntOrString from an int (service targetPort).
func intstrFromInt(v int) intstr.IntOrString { return intstr.FromInt(v) }

func tptr(t metav1.Time) *metav1.Time { return &t }

// timeParse parses an RFC3339 literal (the fixtures' fixed timestamps).
func timeParse(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// mustBase64 decodes a base64 literal into the bytes a ConfigMap binaryData /
// Secret data value carries; the input is build-time-constant so a decode
// failure is a code bug caught by the first test run.
func mustBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic("fakeapi basedata: bad base64 literal: " + err.Error())
	}
	return b
}

// basePodLog is the 40-line timestamped pod-log payload the nginx pod's /log
// route serves (the logs e2e + the kube log tests read "GET / 200" and the
// long upstream-timeout line). Reproduced verbatim from the historical
// data/pod_log.txt fixture so the log-render surface is unchanged.
const basePodLog = `2026-01-01T00:00:00Z Starting nginx
2026-01-01T00:00:01Z nginx ready, listening on 127.0.0.1:8080
2026-01-01T00:00:02Z GET / 200
2026-01-01T00:00:03Z GET /healthz 200 0.003ms upstream=10.0.0.4:8080
2026-01-01T00:00:04Z GET /healthz 200 0.004ms upstream=10.0.0.5:8080
2026-01-01T00:00:05Z GET /healthz 200 0.005ms upstream=10.0.0.6:8080
2026-01-01T00:00:06Z GET /healthz 200 0.006ms upstream=10.0.0.7:8080
2026-01-01T00:00:07Z GET /healthz 200 0.007ms upstream=10.0.0.8:8080
2026-01-01T00:00:08Z GET /healthz 200 0.008ms upstream=10.0.0.9:8080
2026-01-01T00:00:09Z GET /healthz 200 0.009ms upstream=10.0.0.1:8080
2026-01-01T00:00:10Z GET /healthz 200 0.010ms upstream=10.0.0.2:8080
2026-01-01T00:00:11Z GET /healthz 200 0.011ms upstream=10.0.0.3:8080
2026-01-01T00:00:12Z GET /healthz 200 0.012ms upstream=10.0.0.4:8080
2026-01-01T00:00:13Z GET /healthz 200 0.013ms upstream=10.0.0.5:8080
2026-01-01T00:00:14Z GET /healthz 200 0.014ms upstream=10.0.0.6:8080
2026-01-01T00:00:15Z GET /healthz 200 0.015ms upstream=10.0.0.7:8080
2026-01-01T00:00:16Z GET /healthz 200 0.016ms upstream=10.0.0.8:8080
2026-01-01T00:00:17Z GET /healthz 200 0.017ms upstream=10.0.0.9:8080
2026-01-01T00:00:18Z GET /healthz 200 0.018ms upstream=10.0.0.1:8080
2026-01-01T00:00:19Z GET /healthz 200 0.019ms upstream=10.0.0.2:8080
2026-01-01T00:00:20Z GET /healthz 200 0.020ms upstream=10.0.0.3:8080
2026-01-01T00:00:21Z GET /healthz 200 0.021ms upstream=10.0.0.4:8080
2026-01-01T00:00:22Z GET /healthz 200 0.022ms upstream=10.0.0.5:8080
2026-01-01T00:00:23Z GET /healthz 200 0.023ms upstream=10.0.0.6:8080
2026-01-01T00:00:24Z GET /healthz 200 0.024ms upstream=10.0.0.7:8080
2026-01-01T00:00:25Z GET /healthz 200 0.025ms upstream=10.0.0.8:8080
2026-01-01T00:00:26Z GET /healthz 200 0.026ms upstream=10.0.0.9:8080
2026-01-01T00:00:27Z GET /healthz 200 0.027ms upstream=10.0.0.1:8080
2026-01-01T00:00:28Z GET /healthz 200 0.028ms upstream=10.0.0.2:8080
2026-01-01T00:00:29Z GET /healthz 200 0.029ms upstream=10.0.0.3:8080
2026-01-01T00:00:30Z GET /healthz 200 0.030ms upstream=10.0.0.4:8080
2026-01-01T00:00:31Z GET /healthz 200 0.031ms upstream=10.0.0.5:8080
2026-01-01T00:00:32Z GET /healthz 200 0.032ms upstream=10.0.0.6:8080
2026-01-01T00:00:33Z GET /healthz 200 0.033ms upstream=10.0.0.7:8080
2026-01-01T00:00:34Z GET /healthz 200 0.034ms upstream=10.0.0.8:8080
2026-01-01T00:00:35Z GET /healthz 200 0.035ms upstream=10.0.0.9:8080
2026-01-01T00:00:36Z GET /healthz 200 0.036ms upstream=10.0.0.1:8080
2026-01-01T00:00:37Z GET /healthz 200 0.037ms upstream=10.0.0.2:8080
2026-01-01T00:00:38Z upstream timeout while proxying request method=GET path=/api/v1/orders/2026-06-10/export request_id=9f7c2b1a-44d0-4f4e-9d2a-7c1b2f3a4d5e client=10.42.7.13 upstream=orders-backend.svc.cluster.local:8443 attempt=3 backoff=1.6s headers={accept: application/json, x-trace: 00-9f7c2b1a44d04f4e-01} hint=increase proxy_read_timeout or check the backend readiness probe
2026-01-01T00:00:39Z shutting down idle worker after 38s of warmup traffic
`
