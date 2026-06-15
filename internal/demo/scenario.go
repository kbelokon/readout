package demo

// scenario.go is the curated demo object graph (design D5/D6/D7): two
// in-process fake clusters, `prod` and `staging`, whose namespaces are
// STORIES, not a flat one-of-each dump. `prod` carries the rich healthy +
// failing narratives that light up every render path; `staging` carries a
// smaller, healthier variant so the cross-cluster overview / search / `_all`
// views differ meaningfully.
//
// Referential integrity is law (fakekube/integrity.go runs inside Seed): every
// Service selector, ownerRef, Ingress backend, PVC/PV binding, Pod→Node
// assignment, Event involvedObject, and metric key in here resolves, or Seed
// errors. The builders (builders.go) take the linking keys explicitly so a
// story never invents a dangling reference.
//
// DemoScenario() is the single entry point a later wiring unit consumes; the
// per-cluster builders (prodCluster / stagingCluster) are exported-for-test via
// the package's coverage test.

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	fakekube "github.com/kbelokon/readout/internal/fakekube"
)

// longAnnotation is the >120-char annotation value the detail page must collapse
// (the annotations-chip render path). Kept here so the coverage test can assert
// its length and its presence in the scenario.
const longAnnotation = "This deployment is managed by the platform team via GitOps; changes must go through the infra repo pull-request flow and are reconciled by Flux every five minutes — do not edit live."

// bigNamespaceRows is the row count of the big namespace, chosen to cross
// readout's list-virtualization threshold (~500). Exported via a const so the
// coverage test asserts it without a magic number drift.
const bigNamespaceRows = 600

// DemoScenario returns the whole two-cluster demo description. A later unit
// starts one fakekube.Server per Cluster and calls Server.Seed(cluster).
func DemoScenario() fakekube.Scenario {
	return fakekube.Scenario{
		Clusters: []fakekube.Cluster{prodCluster(), stagingCluster()},
	}
}

// prodCluster is the rich cluster: the full status-tone vocabulary, the CRD
// icon-family zoo, pod + node metrics, empty + big namespaces, the long
// annotation, and the curated per-kind cells.
func prodCluster() fakekube.Cluster {
	nodes := []runtime.Object{
		node("cp-1", []string{"control-plane"}, true, false),
		node("worker-1", []string{"worker"}, true, false),
		node("worker-2", []string{"worker"}, false, true), // NotReady + MemoryPressure pill
	}
	nodeMetrics := []fakekube.NodeMetric{
		{Name: "cp-1", CPU: "1200m", Memory: "6Gi"},
		{Name: "worker-1", CPU: "6800m", Memory: "28Gi"}, // heavy (>80% → red bar)
		{Name: "worker-2", CPU: "4400m", Memory: "20Gi"}, // amber band
	}

	return fakekube.Cluster{
		Name:           "prod",
		Nodes:          nodes,
		NodeMetrics:    nodeMetrics,
		ClusterObjects: prodClusterObjects(),
		CRDs:           platformCRDs(),
		Namespaces: []fakekube.Namespace{
			shopNamespace(),
			paymentsNamespace(),
			dataNamespace(),
			platformNamespace(),
			batchNamespace(),
			kubeSystemNamespace(),
			emptyNamespace(),
			bigNamespace(),
		},
	}
}

// prodClusterObjects are the cluster-scoped PersistentVolumes the `data`
// namespace's PVCs bind to, plus a Released PV (the warn tone) and a Failed PV.
func prodClusterObjects() []runtime.Object {
	return objs(
		persistentVolume("pv-data-bound", "Bound", "data", "pgdata-postgres-0", ""),
		persistentVolume("pv-data-released", "Released", "data", "old-claim", "stale bind"),
	)
}

// ---- shop: healthy serving story ------------------------------------------
//
// deployments 3/3, keda HPA, ingress + TLS, LoadBalancer w/ external IP.
func shopNamespace() fakekube.Namespace {
	web := deployment("web", 3, 3)
	web.Annotations = map[string]string{"readout.dev/notes": longAnnotation} // >120-char annotation
	rs := replicaSet("web-7c9", "web", 3, "web")

	pods := objs(
		podFrom("web-7c9-aaa", "shop", podOpts{app: "web", node: "worker-1", ownerRS: "web-7c9",
			statuses: []corev1.ContainerStatus{readyContainer("web")}}),
		podFrom("web-7c9-bbb", "shop", podOpts{app: "web", node: "worker-1", ownerRS: "web-7c9",
			statuses: []corev1.ContainerStatus{readyContainer("web")}}),
		podFrom("web-7c9-ccc", "shop", podOpts{app: "web", node: "worker-2", ownerRS: "web-7c9",
			statuses: []corev1.ContainerStatus{readyContainer("web")}}),
	)

	svc := service("web", "shop", "web", corev1.ServiceTypeLoadBalancer, "203.0.113.40")
	ing := ingress("web", "shop", "shop.example.com", "web", true)
	scaler := hpa("web", "web", 3, 12, 3) // keda-managed target

	all := objs(web, rs, svc, ing, scaler)
	all = append(all, pods...)

	return fakekube.Namespace{
		Name:    "shop",
		Labels:  map[string]string{"app.kubernetes.io/name": "shop", "team": "storefront"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: "web-7c9-aaa", Containers: []fakekube.ContainerMetric{{Name: "web", CPU: "180m", Memory: "256Mi"}}},
			{Name: "web-7c9-bbb", Containers: []fakekube.ContainerMetric{{Name: "web", CPU: "210m", Memory: "300Mi"}}},
			{Name: "web-7c9-ccc", Containers: []fakekube.ContainerMetric{{Name: "web", CPU: "0", Memory: "0"}}}, // zero usage (faint)
		},
	}
}

// ---- payments: failing story ----------------------------------------------
//
// CrashLoopBackOff w/ rising restarts, job BackoffLimitExceeded, Warning events
// incl. a count>1 burst, Pending pod.
func paymentsNamespace() fakekube.Namespace {
	dep := deployment("checkout", 3, 1)
	rs := replicaSet("checkout-5d", "checkout", 3, "checkout")

	crashing := podFrom("checkout-5d-crash", "payments", podOpts{app: "checkout", node: "worker-1", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "CrashLoopBackOff", 14)}}) // rising restarts
	pending := podFrom("checkout-5d-pend", "payments", podOpts{app: "checkout", phase: corev1.PodPending,
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "ContainerCreating", 0)}})
	imgpull := podFrom("checkout-5d-img", "payments", podOpts{app: "checkout", node: "worker-2", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "ImagePullBackOff", 0)}})
	errpull := podFrom("checkout-5d-errpull", "payments", podOpts{app: "checkout", node: "worker-2", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "ErrImagePull", 0)}})
	oom := podFrom("checkout-5d-oom", "payments", podOpts{app: "checkout", node: "worker-1", ownerRS: "checkout-5d",
		phase:    corev1.PodFailed, // pod-phase Failed
		statuses: []corev1.ContainerStatus{terminatedContainer("checkout", "OOMKilled", 137)}})
	cfgerr := podFrom("checkout-5d-cfg", "payments", podOpts{app: "checkout", node: "worker-1", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "CreateContainerConfigError", 0)}})
	badimg := podFrom("checkout-5d-badimg", "payments", podOpts{app: "checkout", node: "worker-1", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "InvalidImageName", 0)}})
	evicted := podFrom("checkout-5d-evict", "payments", podOpts{app: "checkout", node: "worker-2", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{terminatedContainer("checkout", "Evicted", 1)}})
	errc := podFrom("checkout-5d-err", "payments", podOpts{app: "checkout", node: "worker-1", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{terminatedContainer("checkout", "Error", 1)}})
	outofcpu := podFrom("checkout-5d-cpu", "payments", podOpts{app: "checkout", node: "worker-1", ownerRS: "checkout-5d",
		statuses: []corev1.ContainerStatus{waitingContainer("checkout", "OutOfcpu", 0)}})

	svc := service("checkout", "payments", "checkout", corev1.ServiceTypeClusterIP, "")

	failedJob := job("settle-batch", "payments", 1, 0, batchv1.JobFailed, "BackoffLimitExceeded")

	events := objs(
		event("ev-fail-sched", "payments", "Warning", "FailedScheduling", "0/3 nodes are available: insufficient cpu.", "Pod", "checkout-5d-pend", 1),
		event("ev-oom", "payments", "Warning", "SystemOOM", "System OOM encountered, victim process: checkout", "Pod", "checkout-5d-oom", 1),
		event("ev-backoff", "payments", "Warning", "BackOff", "Back-off restarting failed container checkout", "Pod", "checkout-5d-crash", 23), // count>1 burst
		event("ev-unhealthy", "payments", "Warning", "Unhealthy", "Readiness probe failed: connection refused", "Pod", "checkout-5d-crash", 6),
		event("ev-deadline", "payments", "Warning", "DeadlineExceeded", "Job was active longer than specified deadline", "Job", "settle-batch", 1),
		event("ev-preempt", "payments", "Warning", "Preempted", "Preempted by another pod", "Pod", "checkout-5d-pend", 1),
		event("ev-killing", "payments", "Normal", "Killing", "Stopping container checkout", "Pod", "checkout-5d-crash", 2),
		event("ev-pulling", "payments", "Normal", "Pulling", "Pulling image checkout:v2", "Pod", "checkout-5d-img", 1),
		event("ev-blexceeded", "payments", "Warning", "BackoffLimitExceeded", "Job has reached the specified backoff limit", "Job", "settle-batch", 1),
	)

	all := objs(dep, rs, svc, failedJob)
	all = append(all, crashing, pending, imgpull, errpull, oom, cfgerr, badimg, evicted, errc, outofcpu)
	all = append(all, events...)

	return fakekube.Namespace{
		Name:    "payments",
		Labels:  map[string]string{"app.kubernetes.io/name": "payments", "team": "money"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: "checkout-5d-crash", Containers: []fakekube.ContainerMetric{{Name: "checkout", CPU: "50m", Memory: "64Mi"}}},
		},
	}
}

// ---- data: stateful story --------------------------------------------------
//
// CNPG postgres CRD, StatefulSet, PV Bound/Released, heavy metrics,
// pod-containers + related-pods.
func dataNamespace() fakekube.Namespace {
	ss := statefulSet("postgres", 2, 2)
	pgPVC := pvc("pgdata-postgres-0", "data", "pv-data-bound")

	// Two pods owned by the StatefulSet (related-pods sub-table), one mounting
	// the PVC; multi-container so the per-container metrics section renders.
	pg0 := podFrom("postgres-0", "data", podOpts{app: "postgres", node: "worker-1", claimName: "pgdata-postgres-0",
		containers: []corev1.Container{
			{Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:16"},
			{Name: "metrics", Image: "prometheuscommunity/postgres-exporter:v0.15"},
		},
		statuses: []corev1.ContainerStatus{readyContainer("postgres"), readyContainer("metrics")}})
	pg0.OwnerReferences = ownerStatefulSet("postgres")
	pg1 := podFrom("postgres-1", "data", podOpts{app: "postgres", node: "worker-2",
		containers: []corev1.Container{
			{Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:16"},
			{Name: "metrics", Image: "prometheuscommunity/postgres-exporter:v0.15"},
		},
		statuses: []corev1.ContainerStatus{readyContainer("postgres"), readyContainer("metrics")}})
	pg1.OwnerReferences = ownerStatefulSet("postgres")

	// A CNPG Cluster custom resource (postgresql.cnpg.io family glyph).
	cnpg := customResource("postgresql.cnpg.io/v1", "Cluster", "postgres", "data")

	svc := service("postgres", "data", "postgres", corev1.ServiceTypeClusterIP, "")

	all := objs(ss, pgPVC, cnpg, svc, pg0, pg1)

	return fakekube.Namespace{
		Name:    "data",
		Labels:  map[string]string{"app.kubernetes.io/name": "data", "team": "platform"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: "postgres-0", Containers: []fakekube.ContainerMetric{
				{Name: "postgres", CPU: "2400m", Memory: "12Gi"}, // heavy
				{Name: "metrics", CPU: "20m", Memory: "32Mi"},
			}},
			{Name: "postgres-1", Containers: []fakekube.ContainerMetric{
				{Name: "postgres", CPU: "2100m", Memory: "11Gi"},
				{Name: "metrics", CPU: "18m", Memory: "30Mi"},
			}},
		},
	}
}

// ---- platform: the CRD icon-family zoo -------------------------------------
//
// CRDs covering ALL 11 curated icon families + ≥2 unknown-group CRDs for the
// monogram path. The CRD list (platformCRDs) registers the discovery
// group-versions; each family also gets one custom-resource OBJECT here so the
// list route serves a row carrying that group's icon.
func platformNamespace() fakekube.Namespace {
	all := objs(
		customResource("cert-manager.io/v1", "Certificate", "shop-tls", "platform"),
		customResource("cilium.io/v2", "CiliumNetworkPolicy", "default-deny", "platform"),
		customResource("argoproj.io/v1alpha1", "Rollout", "web", "platform"),
		customResource("operator.victoriametrics.com/v1beta1", "VMAgent", "vmagent", "platform"),
		customResource("monitoring.coreos.com/v1", "ServiceMonitor", "web", "platform"),
		customResource("external-secrets.io/v1beta1", "ExternalSecret", "db-creds", "platform"),
		customResource("keda.sh/v1alpha1", "ScaledObject", "web", "platform"),
		customResource("gateway.networking.k8s.io/v1", "HTTPRoute", "web", "platform"),
		customResource("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "apps", "platform"),            // *.fluxcd.io suffix
		customResource("templates.gatekeeper.sh/v1", "ConstraintTemplate", "require-labels", "platform"), // *.gatekeeper.sh suffix
		// postgresql.cnpg.io family object lives in `data` (the CNPG Cluster);
		// register it here too so the family is present cluster-wide.
		customResource("postgresql.cnpg.io/v1", "Backup", "nightly", "platform"),
		// ≥2 unknown-group CRDs → HashHue monogram tiles.
		customResource("widgets.acme.example", "Widget", "blue-widget", "platform"),
		customResource("gizmos.contoso.example", "Gizmo", "left-gizmo", "platform"),
	)
	return fakekube.Namespace{
		Name:    "platform",
		Labels:  map[string]string{"app.kubernetes.io/name": "platform", "team": "platform"},
		Objects: all,
	}
}

// platformCRDs registers every custom-resource group-version-kind the scenario
// serves (across all namespaces), so the list routes resolve and the discovery
// documents carry the groups.
func platformCRDs() []fakekube.CRD {
	return []fakekube.CRD{
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Plural: "certificates", Namespaced: true},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy", Plural: "ciliumnetworkpolicies", Namespaced: true},
		{Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout", Plural: "rollouts", Namespaced: true},
		{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMAgent", Plural: "vmagents", Namespaced: true},
		{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor", Plural: "servicemonitors", Namespaced: true},
		{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret", Plural: "externalsecrets", Namespaced: true},
		{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject", Plural: "scaledobjects", Namespaced: true},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute", Plural: "httproutes", Namespaced: true},
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization", Plural: "kustomizations", Namespaced: true},
		{Group: "templates.gatekeeper.sh", Version: "v1", Kind: "ConstraintTemplate", Plural: "constrainttemplates", Namespaced: true},
		{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster", Plural: "clusters", Namespaced: true},
		{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup", Plural: "backups", Namespaced: true},
		{Group: "widgets.acme.example", Version: "v1", Kind: "Widget", Plural: "widgets", Namespaced: true},
		{Group: "gizmos.contoso.example", Version: "v1", Kind: "Gizmo", Plural: "gizmos", Namespaced: true},
	}
}

// ---- batch: CronJobs + Jobs + the event Reason-map tones --------------------
func batchNamespace() fakekube.Namespace {
	cronActive := cronJob("nightly-report", "batch", "0 2 * * *", false, 1, true)    // Active
	cronSuspended := cronJob("legacy-export", "batch", "*/5 * * * *", true, 0, true) // Suspended
	cronNever := cronJob("first-run", "batch", "@weekly", false, 0, false)           // never-ran (<never>)

	jobComplete := job("backup-ok", "batch", 1, 1, batchv1.JobComplete, "") // Complete
	jobRunning := job("reindex", "batch", 5, 2, "", "")                     // in-flight
	jobFailed := job("export-fail", "batch", 1, 0, batchv1.JobFailed, "DeadlineExceeded")

	// Events carrying the Reason-map tones (success Reasons + the unique info
	// Reasons), so the events list exercises every Reason→tone branch.
	events := objs(
		event("ev-created", "batch", "Normal", "Created", "Created container report", "Job", "backup-ok", 1),
		event("ev-pulled", "batch", "Normal", "Pulled", "Successfully pulled image", "Job", "backup-ok", 1),
		event("ev-scheduled", "batch", "Normal", "Scheduled", "Successfully assigned batch/backup-ok to worker-1", "Job", "backup-ok", 1),
		event("ev-started", "batch", "Normal", "Started", "Started container report", "Job", "backup-ok", 1),
		event("ev-succreate", "batch", "Normal", "SuccessfulCreate", "Created pod: backup-ok-xyz", "Job", "backup-ok", 1),
		event("ev-sawjob", "batch", "Normal", "SawCompletedJob", "Saw completed job: backup-ok", "CronJob", "nightly-report", 1),   // info tone
		event("ev-scaleup", "batch", "Normal", "TriggeredScaleUp", "pod triggered scale-up: 1->2", "CronJob", "nightly-report", 1), // info tone
		event("ev-getmetric", "batch", "Warning", "FailedGetResourceMetric", "unable to get metric cpu", "CronJob", "legacy-export", 1),
		event("ev-computemetric", "batch", "Warning", "FailedComputeMetricsReplicas", "invalid metrics", "CronJob", "legacy-export", 1),
		event("ev-failed", "batch", "Warning", "Failed", "Error: failed to start container", "Job", "export-fail", 3),
	)

	all := objs(cronActive, cronSuspended, cronNever, jobComplete, jobRunning, jobFailed)
	all = append(all, events...)

	return fakekube.Namespace{
		Name:    "batch",
		Labels:  map[string]string{"app.kubernetes.io/name": "batch", "team": "data"},
		Objects: all,
	}
}

// ---- kube-system: DaemonSets, init containers, multi-key secrets -----------
func kubeSystemNamespace() fakekube.Namespace {
	ds := daemonSet("kube-proxy", map[string]string{"kubernetes.io/os": "linux"})

	// A pod with init containers in progress (Init:1/2 → warn) and one with an
	// errored init container (Init:CrashLoopBackOff → err). These exercise the
	// StatusTone Init:* branches.
	initProgress := podFrom("installer-progress", "kube-system", podOpts{app: "installer", node: "cp-1",
		initStatuses: []corev1.ContainerStatus{
			terminatedContainer("step-1", "Completed", 0), // 1 complete
			waitingContainer("step-2", "PodInitializing", 0),
		},
		statuses: []corev1.ContainerStatus{waitingContainer("app", "PodInitializing", 0)}})
	initErrored := podFrom("installer-failed", "kube-system", podOpts{app: "installer", node: "cp-1",
		initStatuses: []corev1.ContainerStatus{
			waitingContainer("step-1", "CrashLoopBackOff", 5),
		},
		statuses: []corev1.ContainerStatus{waitingContainer("app", "PodInitializing", 0)}})

	// A succeeded one-shot pod (phase Succeeded → mute) and a Terminating pod.
	doneOnce := podFrom("migrate-once", "kube-system", podOpts{app: "migrate", node: "cp-1",
		phase:    corev1.PodSucceeded,
		statuses: []corev1.ContainerStatus{terminatedContainer("migrate", "Completed", 0)}})
	terminating := podFrom("kube-proxy-term", "kube-system", podOpts{app: "kube-proxy", node: "worker-1",
		statuses: []corev1.ContainerStatus{waitingContainer("kube-proxy", "Terminating", 0)}})

	multiSecret := secret("registry-creds", "kube-system", map[string][]byte{
		"username":   []byte("admin"),
		"password":   []byte("s3cr3t-value-never-rendered"),
		"ca.crt":     []byte("-----BEGIN CERTIFICATE-----"),
		".dockercfg": []byte("{\"auths\":{}}"),
	})
	cm := configMap("kube-proxy-config", "kube-system", map[string]string{
		"config.conf": "mode: iptables",
		"kubeconfig":  "apiVersion: v1",
	})

	all := objs(ds, multiSecret, cm, initProgress, initErrored, doneOnce, terminating)

	return fakekube.Namespace{
		Name:    "kube-system",
		Labels:  map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		Objects: all,
	}
}

// ---- empty + big namespaces ------------------------------------------------

// emptyNamespace is a real namespace with no objects (the empty-list render
// path).
func emptyNamespace() fakekube.Namespace {
	return fakekube.Namespace{
		Name:   "empty",
		Labels: map[string]string{"app.kubernetes.io/name": "empty"},
	}
}

// bigNamespace holds ~bigNamespaceRows ConfigMaps so the list crosses readout's
// virtualization threshold (~500 rows).
func bigNamespace() fakekube.Namespace {
	items := make([]runtime.Object, 0, bigNamespaceRows)
	for i := 0; i < bigNamespaceRows; i++ {
		items = append(items, configMap(bigName(i), "big", map[string]string{"k": "v"}))
	}
	return fakekube.Namespace{
		Name:    "big",
		Labels:  map[string]string{"app.kubernetes.io/name": "big"},
		Objects: items,
	}
}

// stagingCluster is the smaller, healthier variant: a single healthy app, one
// node, node + pod metrics, a couple of CRDs — enough that cross-cluster
// overview/search/`_all` views differ from prod.
func stagingCluster() fakekube.Cluster {
	nodes := []runtime.Object{node("staging-1", []string{"control-plane", "worker"}, true, false)}

	web := deployment("web", 2, 2)
	rs := replicaSet("web-aa1", "web", 2, "web")
	pods := objs(
		podFrom("web-aa1-x", "apps", podOpts{app: "web", node: "staging-1", ownerRS: "web-aa1",
			statuses: []corev1.ContainerStatus{readyContainer("web")}}),
		podFrom("web-aa1-y", "apps", podOpts{app: "web", node: "staging-1", ownerRS: "web-aa1",
			statuses: []corev1.ContainerStatus{readyContainer("web")}}),
	)
	svc := service("web", "apps", "web", corev1.ServiceTypeClusterIP, "")
	all := objs(web, rs, svc)
	all = append(all, pods...)

	return fakekube.Cluster{
		Name:        "staging",
		Nodes:       nodes,
		NodeMetrics: []fakekube.NodeMetric{{Name: "staging-1", CPU: "900m", Memory: "4Gi"}},
		CRDs: []fakekube.CRD{
			{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Plural: "certificates", Namespaced: true},
		},
		Namespaces: []fakekube.Namespace{{
			Name:    "apps",
			Labels:  map[string]string{"app.kubernetes.io/name": "apps", "env": "staging"},
			Objects: append(all, customResource("cert-manager.io/v1", "Certificate", "web-tls", "apps")),
			PodMetrics: []fakekube.PodMetric{
				{Name: "web-aa1-x", Containers: []fakekube.ContainerMetric{{Name: "web", CPU: "120m", Memory: "180Mi"}}},
				{Name: "web-aa1-y", Containers: []fakekube.ContainerMetric{{Name: "web", CPU: "140m", Memory: "200Mi"}}},
			},
		}},
	}
}
