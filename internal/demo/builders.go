package demo

// builders.go holds the small typed object builders the demo scenario
// (scenario.go) composes into story-shaped namespaces. Each builder returns a
// typed Kubernetes object (the fakekube seeder stamps apiVersion/kind from the
// Go type), so the scenario reads as stories — deployment("web", 3, 3),
// crashingPod("checkout", 7) — not as literal struct soup. Referential keys
// (owner refs, selectors, nodeName, claimNames, metric names) are passed in by
// the caller so the integrity validator (fakekube/integrity.go) resolves every
// link; a builder never invents a dangling reference.

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// refTime is the fixed "now" the demo's relative timestamps hang off, so the
// scenario is deterministic across runs (ages render stably).
var refTime = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func i32p(v int32) *int32 { return &v }
func boolp(v bool) *bool  { return &v }

func created(minsAgo int) metav1.Time {
	return metav1.NewTime(refTime.Add(-time.Duration(minsAgo) * time.Minute))
}

// ---- workloads ------------------------------------------------------------

// deployment builds a Deployment with a matching selector label and a
// ready/desired replica status (so the replica-track cell renders).
func deployment(name string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created(120),
			Labels:            map[string]string{"app": name},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32p(desired),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          desired,
			ReadyReplicas:     ready,
			UpdatedReplicas:   ready,
			AvailableReplicas: ready,
		},
	}
}

// replicaSet builds a ReplicaSet owned by a Deployment (the owner chain the
// related-pods sub-table walks).
func replicaSet(name, owningDeployment string, replicas int32, appLabel string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created(115),
			Labels:            map[string]string{"app": appLabel},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: owningDeployment}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: i32p(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
		},
		Status: appsv1.ReplicaSetStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

func statefulSet(name string, desired, ready int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created(300),
			Labels:            map[string]string{"app": name},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: i32p(desired),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.StatefulSetStatus{Replicas: desired, ReadyReplicas: ready},
	}
}

func daemonSet(name string, nodeSelector map[string]string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created(500),
			Labels:            map[string]string{"app": name},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{NodeSelector: nodeSelector},
			},
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 3, CurrentNumberScheduled: 3, NumberReady: 3,
			UpdatedNumberScheduled: 3, NumberAvailable: 3,
		},
	}
}

func hpa(name, targetDeployment string, min, max, current int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created(90),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment", Name: targetDeployment, APIVersion: "apps/v1",
			},
			MinReplicas: i32p(min),
			MaxReplicas: max,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: current},
	}
}

// ---- pods -----------------------------------------------------------------

// podOpts collects the optional knobs a Pod story needs. Zero value yields a
// plain Running pod with a single ready container.
type podOpts struct {
	app          string
	node         string
	phase        corev1.PodPhase
	containers   []corev1.Container
	statuses     []corev1.ContainerStatus
	initStatuses []corev1.ContainerStatus
	ownerRS      string
	claimName    string // a persistentVolumeClaim volume
	createdMins  int
}

func podFrom(name, namespace string, o podOpts) *corev1.Pod {
	if o.phase == "" {
		o.phase = corev1.PodRunning
	}
	if len(o.containers) == 0 {
		o.containers = []corev1.Container{{Name: "app", Image: "registry.example.com/" + name + ":v1"}}
	}
	if o.createdMins == 0 {
		o.createdMins = 100
	}
	meta := metav1.ObjectMeta{
		Name:              name,
		Namespace:         namespace,
		CreationTimestamp: created(o.createdMins),
		Labels:            map[string]string{"app": firstNonEmpty(o.app, name)},
	}
	if o.ownerRS != "" {
		meta.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: o.ownerRS}}
	}
	spec := corev1.PodSpec{Containers: o.containers}
	// The detail-page containers section is built from spec.initContainers (not
	// the status alone — see buildContainersView), so a pod that carries init
	// statuses must declare the matching init containers in its spec or the
	// init-container render branch stays dark. Mirror one spec.initContainers
	// entry per seeded init status so the demo lights up that branch.
	for _, st := range o.initStatuses {
		spec.InitContainers = append(spec.InitContainers, corev1.Container{
			Name:  st.Name,
			Image: "registry.example.com/" + st.Name + ":v1",
		})
	}
	if o.node != "" {
		spec.NodeName = o.node
	}
	if o.claimName != "" {
		spec.Volumes = []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: o.claimName},
			},
		}}
	}
	return &corev1.Pod{
		ObjectMeta: meta,
		Spec:       spec,
		Status: corev1.PodStatus{
			Phase:                 o.phase,
			ContainerStatuses:     o.statuses,
			InitContainerStatuses: o.initStatuses,
		},
	}
}

// readyContainer is a Running, ready container status.
func readyContainer(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:    name,
		Ready:   true,
		Image:   "registry.example.com/" + name + ":v1",
		State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created(95)}},
		Started: boolp(true),
	}
}

// waitingContainer is a not-ready container waiting with a reason (the
// StatusTone err/warn words ride here: CrashLoopBackOff, ImagePullBackOff, ...).
func waitingContainer(name, reason string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		Ready:        false,
		RestartCount: restarts,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	}
}

// terminatedContainer is a terminated container carrying a reason (OOMKilled,
// Error, Completed).
func terminatedContainer(name, reason string, exit int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		Ready: false,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: reason, ExitCode: exit,
		}},
	}
}

// ---- networking -----------------------------------------------------------

func service(name, namespace, app string, typ corev1.ServiceType, externalIP string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: created(120),
		},
		Spec: corev1.ServiceSpec{
			Type:      typ,
			Selector:  map[string]string{"app": app},
			ClusterIP: "10.96.0.10",
			Ports:     []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}},
		},
	}
	if externalIP != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: externalIP}}
	}
	return svc
}

func ingress(name, namespace, host, svcName string, tls bool) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: created(120),
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: strp("nginx"),
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svcName,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if tls {
		ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: name + "-tls"}}
	}
	return ing
}

func strp(s string) *string { return &s }

// ---- config / secrets -----------------------------------------------------

func configMap(name, namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(120)},
		Data:       data,
	}
}

func secret(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(120)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// ---- batch ----------------------------------------------------------------

func cronJob(name, namespace, schedule string, suspend bool, active int, lastRun bool) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(1000)},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			Suspend:  boolp(suspend),
		},
	}
	for i := 0; i < active; i++ {
		cj.Status.Active = append(cj.Status.Active, corev1.ObjectReference{Kind: "Job", Name: name})
	}
	if lastRun {
		t := created(30)
		cj.Status.LastScheduleTime = &t
	}
	return cj
}

// job builds a Job in one of a few synthetic terminal/in-flight states.
func job(name, namespace string, completions, succeeded int32, condType batchv1.JobConditionType, reason string) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(60)},
		Spec:       batchv1.JobSpec{Completions: i32p(completions)},
		Status:     batchv1.JobStatus{Succeeded: succeeded},
	}
	if condType != "" {
		j.Status.Conditions = []batchv1.JobCondition{{
			Type: condType, Status: corev1.ConditionTrue, Reason: reason,
		}}
	}
	return j
}

// ---- storage --------------------------------------------------------------

func pvc(name, namespace, volumeName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(300)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:       volumeName,
			StorageClassName: strp("fast-ssd"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: mustQty("20Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// persistentVolume builds a cluster-scoped PV in a given phase (Bound/Released).
// A Bound PV carries a claimRef; a Released PV carries a stale claimRef + reason.
func persistentVolume(name, phase, claimNS, claimName, reason string) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created(300)},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: mustQty("20Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "fast-ssd",
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.PersistentVolumePhase(phase), Reason: reason},
	}
	if claimName != "" {
		pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: claimNS, Name: claimName, Kind: "PersistentVolumeClaim"}
	}
	return pv
}

// ---- nodes ----------------------------------------------------------------

// node builds a Node with the given roles + Ready condition; memoryPressure adds
// an abnormal condition pill.
func node(name string, roles []string, ready, memoryPressure bool) *corev1.Node {
	labels := map[string]string{}
	for _, r := range roles {
		labels["node-role.kubernetes.io/"+r] = ""
	}
	readyStatus := corev1.ConditionTrue
	if !ready {
		readyStatus = corev1.ConditionFalse
	}
	conds := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: readyStatus}}
	if memoryPressure {
		conds = append(conds, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, CreationTimestamp: created(5000)},
		Status: corev1.NodeStatus{
			Conditions: conds,
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    mustQty("8"),
				corev1.ResourceMemory: mustQty("32Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    mustQty("8"),
				corev1.ResourceMemory: mustQty("32Gi"),
			},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:          "v1.31.2",
				OSImage:                 "Ubuntu 22.04.4 LTS",
				KernelVersion:           "5.15.0-101-generic",
				ContainerRuntimeVersion: "containerd://1.7.13",
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.5"},
			},
		},
	}
}

// ---- events ---------------------------------------------------------------

// event builds a core/v1 Event referencing an in-namespace object, carrying a
// Type (Normal/Warning) + Reason (the Reason-map vocabulary) and a count.
func event(name, namespace, evType, reason, message, refKind, refName string, count int32) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(10)},
		Type:           evType,
		Reason:         reason,
		Message:        message,
		Count:          count,
		LastTimestamp:  created(5),
		FirstTimestamp: created(40),
		InvolvedObject: corev1.ObjectReference{Kind: refKind, Name: refName, Namespace: namespace},
	}
}

// objs is a tiny variadic helper that flattens builder calls into the
// []runtime.Object a Namespace carries.
func objs(items ...runtime.Object) []runtime.Object { return items }

// mustQty parses a quantity string, panicking on a malformed literal (the
// builders only pass build-time-constant strings, so a panic is a code bug
// caught by the first test run, never runtime data).
func mustQty(s string) resource.Quantity { return resource.MustParse(s) }

// customResource builds an unstructured custom-resource object carrying its
// apiVersion + kind (so the seeder resolves it against the registered CRD) and
// metadata. The matching CRD must be registered on the cluster (platformCRDs),
// or the integrity validator rejects the dangling discovery reference.
func customResource(apiVersion, kind, name, namespace string) runtime.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	u.SetCreationTimestamp(created(200))
	return u
}

// ownerStatefulSet builds the ownerReferences slice pointing a Pod at a
// StatefulSet (the related-pods sub-table walks this owner chain).
func ownerStatefulSet(name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{Kind: "StatefulSet", Name: name}}
}

// bigName renders a zero-padded deterministic name for the big-namespace rows,
// so the sorted list order is stable.
func bigName(i int) string { return fmt.Sprintf("cm-%04d", i) }
