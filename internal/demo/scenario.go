package demo

// scenario.go is the curated demo object graph: two in-process fake clusters,
// `prod` and `staging`, modelled on a believable commerce platform (Acme Shop)
// rather than a coverage checklist. Namespaces map to how a real team organises
// a cluster — product services (storefront, checkout, payments, search,
// databases) and the platform/operator namespaces (argocd, monitoring,
// cert-manager, flux-system, ingress-nginx, kube-system, …). Each carries a
// coherent, healthy-looking workload, with a single believable incident (a bad
// checkout deploy) so a visitor instantly sees the product spot a real problem.
//
// Referential integrity is law (the validator runs inside Seed): every Service
// selector, ownerRef, Ingress backend, PVC/PV binding, Pod→Node assignment,
// Event involvedObject, and metric key resolves, or Seed errors. The builders
// (builders.go) take the linking keys explicitly so a story never invents a
// dangling reference.

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	fakekube "github.com/kbelokon/readout/internal/fakekube"
)

// longAnnotation is the >120-char annotation the detail page must collapse.
const longAnnotation = "Managed by GitOps (Argo CD) from github.com/acme/platform — do not edit live; changes must land through the infra pull-request flow and are reconciled automatically within five minutes of merge."

// storefrontFleet is the storefront web tier size, large enough to cross the
// list-virtualization threshold so the big-list render path is exercised by a
// realistic fleet (not a dump of empty objects).
const storefrontFleet = 540

// prodWorkers is the worker pool the schedulable workloads spread across (the
// cordoned worker-5 is deliberately left out so nothing new lands on it).
var prodWorkers = []string{"worker-1", "worker-2", "worker-3", "worker-4"}

// DemoScenario returns the whole two-cluster demo description; the wiring starts
// one fakekube.Server per Cluster and calls Server.Seed(cluster).
func DemoScenario() fakekube.Scenario {
	return fakekube.Scenario{
		Clusters: []fakekube.Cluster{prodCluster(), stagingCluster()},
	}
}

func prodCluster() fakekube.Cluster {
	return fakekube.Cluster{
		Name:           "prod",
		Nodes:          prodNodes(),
		NodeMetrics:    prodNodeMetrics(),
		ClusterObjects: append(prodPVs(), clusterScopedObjects()...),
		CRDs:           append(demoCRDs(), clusterScopedKinds()...),
		FillEmptyLists: true, // a served kind answers an empty 200 in any namespace, never a 404
		Namespaces: []fakekube.Namespace{
			storefrontNamespace(),
			checkoutNamespace(),
			paymentsNamespace(),
			searchNamespace(),
			databasesNamespace(),
			argocdNamespace(),
			monitoringNamespace(),
			certManagerNamespace(),
			fluxNamespace(),
			externalSecretsNamespace(),
			gatekeeperNamespace(),
			ingressNginxNamespace(),
			kubeSystemNamespace(),
			defaultNamespace(),
		},
	}
}

// ---- nodes -----------------------------------------------------------------

func prodNodes() []runtime.Object {
	return objs(
		node("cp-1", []string{"control-plane"}, true, false),
		node("cp-2", []string{"control-plane"}, true, false),
		node("cp-3", []string{"control-plane"}, true, false),
		node("worker-1", []string{"worker"}, true, false),
		node("worker-2", []string{"worker"}, true, false),
		node("worker-3", []string{"worker"}, true, false),
		node("worker-4", []string{"worker"}, true, true),   // MemoryPressure pill
		node("worker-5", []string{"worker"}, false, false), // NotReady (cordoned/down)
	)
}

func prodNodeMetrics() []fakekube.NodeMetric {
	return []fakekube.NodeMetric{
		{Name: "cp-1", CPU: "1100m", Memory: "5Gi"},
		{Name: "cp-2", CPU: "980m", Memory: "5Gi"},
		{Name: "cp-3", CPU: "1040m", Memory: "5Gi"},
		{Name: "worker-1", CPU: "6800m", Memory: "27Gi"}, // hot (>80% → red)
		{Name: "worker-2", CPU: "5200m", Memory: "21Gi"}, // amber
		{Name: "worker-3", CPU: "3600m", Memory: "16Gi"},
		{Name: "worker-4", CPU: "7400m", Memory: "30Gi"}, // hot + MemoryPressure
		{Name: "worker-5", CPU: "120m", Memory: "1Gi"},   // drained
	}
}

// prodPVs are the cluster-scoped volumes the stateful namespaces bind to, plus a
// Released one from a decommissioned replica (the warn tone).
func prodPVs() []runtime.Object {
	return objs(
		persistentVolume("pvc-orders-db-0", "Bound", "databases", "data-orders-db-0", ""),
		persistentVolume("pvc-orders-db-1", "Bound", "databases", "data-orders-db-1", ""),
		persistentVolume("pvc-search-0", "Bound", "search", "data-opensearch-0", ""),
		persistentVolume("pvc-search-1", "Bound", "search", "data-opensearch-1", ""),
		persistentVolume("pvc-search-2", "Bound", "search", "data-opensearch-2", ""),
		persistentVolume("pvc-orders-db-old", "Released", "databases", "data-orders-db-2", "node drained, replica retired"),
	)
}

// ---- storefront: the healthy hero + the large web fleet --------------------

func storefrontNamespace() fakekube.Namespace {
	const rs = "storefront-web-6f8d94c7"
	web := deployment("storefront-web", storefrontFleet, storefrontFleet)
	web.Annotations = map[string]string{
		"kubernetes.io/change-cause": "rollout v4.2.1 (perf: cache warm on boot)",
		"readout.dev/runbook":        longAnnotation, // >120-char annotation
	}
	rsObj := replicaSet(rs, "storefront-web", storefrontFleet, "storefront-web")
	pods := fleetPods("storefront", "storefront-web", rs, storefrontFleet, prodWorkers)

	svc := service("storefront-web", "storefront", "storefront-web", corev1.ServiceTypeLoadBalancer, "203.0.113.40")
	ing := ingress("storefront", "storefront", "shop.acme.example", "storefront-web", true)
	route := customResource("gateway.networking.k8s.io/v1", "HTTPRoute", "storefront", "storefront")
	scaler := customResource("keda.sh/v1alpha1", "ScaledObject", "storefront-web", "storefront")
	cm := configMap("storefront-config", "storefront", map[string]string{
		"FEATURE_NEW_CART": "true",
		"CDN_BASE":         "https://cdn.acme.example",
		"CHECKOUT_URL":     "https://shop.acme.example/checkout",
		"LOCALE_DEFAULT":   "en-US",
	})

	all := objs(web, rsObj, svc, ing, route, scaler, cm)
	all = append(all, pods...)

	return fakekube.Namespace{
		Name:    "storefront",
		Created: createdAgo(74 * 24 * time.Hour),
		Labels:  map[string]string{"app.kubernetes.io/name": "storefront", "team": "web", "tier": "frontend"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: rs + "-" + fleetHash(0), Containers: []fakekube.ContainerMetric{{Name: "storefront-web", CPU: "240m", Memory: "320Mi"}}},
			{Name: rs + "-" + fleetHash(1), Containers: []fakekube.ContainerMetric{{Name: "storefront-web", CPU: "180m", Memory: "280Mi"}}},
			{Name: rs + "-" + fleetHash(2), Containers: []fakekube.ContainerMetric{{Name: "storefront-web", CPU: "320m", Memory: "360Mi"}}},
		},
	}
}

// ---- checkout: the incident (a bad v2 rollout) -----------------------------
//
// The new ReplicaSet is crash-looping while the old one still serves traffic:
// the deployment reads 4/6, two pods CrashLoopBackOff with rising restarts, one
// Pending for capacity, and a burst of Warning events. This is the "spot the
// problem instantly" story.
func checkoutNamespace() fakekube.Namespace {
	const oldRS = "checkout-api-7b4f"
	const newRS = "checkout-api-9f2a"
	dep := deployment("checkout-api", 6, 4)
	dep.Annotations = map[string]string{"kubernetes.io/change-cause": "rollout v2.0.0 (new tax engine)"}
	rsOld := replicaSet(oldRS, "checkout-api", 4, "checkout-api")
	rsNew := replicaSet(newRS, "checkout-api", 2, "checkout-api")

	// Old RS: four healthy pods still serving.
	healthy := fleetPods("checkout", "checkout-api", oldRS, 4, prodWorkers)

	// New RS: two crash-looping (rising restarts) + one Pending (no capacity).
	crash1 := podFrom(newRS+"-c4d2x", "checkout", &podOpts{
		app: "checkout-api", node: "worker-1", ownerRS: newRS, createdMins: 38,
		containers: []corev1.Container{{Name: "checkout-api", Image: "registry.example.com/checkout-api:v2.0.0"}},
		statuses:   []corev1.ContainerStatus{waitingContainer("checkout-api", "CrashLoopBackOff", 11)},
	})
	// A second new-RS pod can't even pull the new image (a registry hiccup) — a
	// realistic mixed failure in one bad rollout.
	imgpull := podFrom(newRS+"-h8k7p", "checkout", &podOpts{
		app: "checkout-api", node: "worker-2", ownerRS: newRS, createdMins: 38,
		containers: []corev1.Container{{Name: "checkout-api", Image: "registry.example.com/checkout-api:v2.0.0"}},
		statuses:   []corev1.ContainerStatus{waitingContainer("checkout-api", "ImagePullBackOff", 0)},
	})
	pending := podFrom(newRS+"-q2w9z", "checkout", &podOpts{
		app: "checkout-api", phase: corev1.PodPending, ownerRS: newRS, createdMins: 36,
		statuses: []corev1.ContainerStatus{waitingContainer("checkout-api", "ContainerCreating", 0)},
	})

	svc := service("checkout-api", "checkout", "checkout-api", corev1.ServiceTypeClusterIP, "")
	cm := configMap("checkout-config", "checkout", map[string]string{
		"TAX_PROVIDER": "avalara",
		"RETRY_LIMIT":  "3",
	})
	sec := secret("checkout-signing-key", "checkout", map[string][]byte{
		"hmac.key": []byte("d34db33f-never-rendered-in-the-ui-0000"),
	})

	events := objs(
		event("ev-backoff", "checkout", "Warning", "BackOff", "Back-off restarting failed container checkout-api in pod "+newRS+"-c4d2x", "Pod", newRS+"-c4d2x", 23),
		event("ev-unhealthy", "checkout", "Warning", "Unhealthy", "Readiness probe failed: HTTP probe returned statuscode 500", "Pod", newRS+"-c4d2x", 18),
		event("ev-sched", "checkout", "Warning", "FailedScheduling", "0/8 nodes are available: 4 Insufficient cpu, 4 node(s) had untolerated taint.", "Pod", newRS+"-q2w9z", 1),
		event("ev-pulled", "checkout", "Normal", "Pulled", "Successfully pulled image checkout-api:v2.0.0 in 2.1s", "Pod", newRS+"-c4d2x", 3),
	)

	all := objs(dep, rsOld, rsNew, svc, cm, sec, crash1, imgpull, pending)
	all = append(all, healthy...)
	all = append(all, events...)

	return fakekube.Namespace{
		Name:    "checkout",
		Created: createdAgo(61 * 24 * time.Hour),
		Labels:  map[string]string{"app.kubernetes.io/name": "checkout", "team": "payments"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: oldRS + "-" + fleetHash(0), Containers: []fakekube.ContainerMetric{{Name: "checkout-api", CPU: "310m", Memory: "420Mi"}}},
			{Name: newRS + "-c4d2x", Containers: []fakekube.ContainerMetric{{Name: "checkout-api", CPU: "15m", Memory: "90Mi"}}},
		},
	}
}

// ---- payments: healthy api + nightly batch + one memory incident -----------
func paymentsNamespace() fakekube.Namespace {
	apiObjs, apiRS := app("payments", "payments-api", "registry.example.com/payments-api:v3.4.0", 4, prodWorkers, 5*24*60)
	svc := service("payments-api", "payments", "payments-api", corev1.ServiceTypeClusterIP, "")

	// One pod hit a memory spike and got OOMKilled (a believable single incident).
	oom := podFrom(apiRS+"-m3r9k", "payments", &podOpts{
		app: "payments-api", node: "worker-4", ownerRS: apiRS, phase: corev1.PodRunning, createdMins: 5 * 24 * 60,
		containers: []corev1.Container{{Name: "payments-api", Image: "registry.example.com/payments-api:v3.4.0"}},
		statuses: []corev1.ContainerStatus{{
			Name: "payments-api", Ready: true, RestartCount: 2,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created(40)}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}},
	})

	stripeSecret := secret("stripe-api-key", "payments", map[string][]byte{
		"STRIPE_SECRET_KEY":     []byte("sk_live_never-rendered-in-the-ui-000000"),
		"STRIPE_WEBHOOK_SECRET": []byte("whsec_never-rendered-000000000000000000"),
	})
	tlsSecret := secret("payments-tls", "payments", map[string][]byte{
		"tls.crt": []byte("-----BEGIN CERTIFICATE-----"),
		"tls.key": []byte("-----BEGIN PRIVATE KEY-----"),
	})

	// Nightly settlement batch: an active cron, a suspended legacy export, a
	// completed run, and a failed reconcile.
	cronSettle := cronJob("settlement", "payments", "0 2 * * *", false, 1, true)
	cronExport := cronJob("legacy-export", "payments", "*/15 * * * *", true, 0, true)
	jobOK := job("settlement-28291", "payments", 1, 1, batchv1.JobComplete, "")
	jobFail := job("reconcile-28290", "payments", 1, 0, batchv1.JobFailed, "BackoffLimitExceeded")

	events := objs(
		event("ev-oom", "payments", "Warning", "SystemOOM", "System OOM encountered, victim process: payments-api", "Pod", oom.Name, 1),
		event("ev-sawjob", "payments", "Normal", "SawCompletedJob", "Saw completed job: settlement-28291", "CronJob", "settlement", 1),
		event("ev-screate", "payments", "Normal", "SuccessfulCreate", "Created pod: settlement-28291-abcde", "Job", "settlement-28291", 1),
		event("ev-blexceed", "payments", "Warning", "BackoffLimitExceeded", "Job has reached the specified backoff limit", "Job", "reconcile-28290", 1),
	)

	scaler := hpa("payments-api", "payments-api", 4, 16, 4)
	all := apiObjs
	all = append(all, svc, scaler, oom, stripeSecret, tlsSecret, cronSettle, cronExport, jobOK, jobFail)
	all = append(all, events...)

	return fakekube.Namespace{
		Name:    "payments",
		Created: createdAgo(61 * 24 * time.Hour),
		Labels:  map[string]string{"app.kubernetes.io/name": "payments", "team": "payments", "pci": "in-scope"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: apiRS + "-" + fleetHash(0*97+nameHash("payments-api")), Containers: []fakekube.ContainerMetric{{Name: "payments-api", CPU: "220m", Memory: "300Mi"}}},
			{Name: oom.Name, Containers: []fakekube.ContainerMetric{{Name: "payments-api", CPU: "650m", Memory: "980Mi"}}},
		},
	}
}

// ---- search: a stateful OpenSearch cluster (PVCs, heavy metrics) -----------
func searchNamespace() fakekube.Namespace {
	ss := statefulSet("opensearch", 3, 3)
	svc := service("opensearch", "search", "opensearch", corev1.ServiceTypeClusterIP, "")

	var pods []runtime.Object
	var metrics []fakekube.PodMetric
	for i, nodeName := range []string{"worker-1", "worker-2", "worker-3"} {
		name := "opensearch-" + itoa(i)
		p := podFrom(name, "search", &podOpts{
			app: "opensearch", node: nodeName, claimName: "data-opensearch-" + itoa(i), createdMins: 21 * 24 * 60,
			containers: []corev1.Container{
				{Name: "opensearch", Image: "opensearchproject/opensearch:2.13.0"},
				{Name: "exporter", Image: "quay.io/prometheuscommunity/elasticsearch-exporter:v1.7.0"},
			},
			statuses: []corev1.ContainerStatus{readyContainer("opensearch"), readyContainer("exporter")},
		})
		p.OwnerReferences = ownerStatefulSet("opensearch")
		pods = append(pods, p)
		pvcObj := pvc("data-opensearch-"+itoa(i), "search", "pvc-search-"+itoa(i))
		pods = append(pods, pvcObj)
		metrics = append(metrics, fakekube.PodMetric{Name: name, Containers: []fakekube.ContainerMetric{
			{Name: "opensearch", CPU: "3100m", Memory: "14Gi"},
			{Name: "exporter", CPU: "12m", Memory: "24Mi"},
		}})
	}

	all := append(objs(ss, svc), pods...)
	return fakekube.Namespace{
		Name:    "search",
		Created: createdAgo(40 * 24 * time.Hour),
		Labels:  map[string]string{"app.kubernetes.io/name": "search", "team": "discovery"},
		Objects: all, PodMetrics: metrics,
	}
}

// ---- databases: a CNPG postgres cluster -----------------------------------
func databasesNamespace() fakekube.Namespace {
	ss := statefulSet("orders-db", 2, 2)
	cnpg := customResource("postgresql.cnpg.io/v1", "Cluster", "orders-db", "databases")
	backup := customResource("postgresql.cnpg.io/v1", "Backup", "orders-db-nightly", "databases")
	svc := service("orders-db-rw", "databases", "orders-db", corev1.ServiceTypeClusterIP, "")
	sec := secret("orders-db-app", "databases", map[string][]byte{
		"username": []byte("orders"),
		"password": []byte("never-rendered-in-the-ui-000000"),
		"pgpass":   []byte("never-rendered-000000000000000"),
	})

	var pods []runtime.Object
	var metrics []fakekube.PodMetric
	for i, nodeName := range []string{"worker-1", "worker-3"} {
		name := "orders-db-" + itoa(i)
		p := podFrom(name, "databases", &podOpts{
			app: "orders-db", node: nodeName, claimName: "data-orders-db-" + itoa(i), createdMins: 33 * 24 * 60,
			containers: []corev1.Container{
				{Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:16.2"},
				{Name: "metrics", Image: "prometheuscommunity/postgres-exporter:v0.15.0"},
			},
			statuses: []corev1.ContainerStatus{readyContainer("postgres"), readyContainer("metrics")},
		})
		p.OwnerReferences = ownerStatefulSet("orders-db")
		pods = append(pods, p, pvc("data-orders-db-"+itoa(i), "databases", "pvc-orders-db-"+itoa(i)))
		metrics = append(metrics, fakekube.PodMetric{Name: name, Containers: []fakekube.ContainerMetric{
			{Name: "postgres", CPU: "2200m", Memory: "11Gi"},
			{Name: "metrics", CPU: "18m", Memory: "30Mi"},
		}})
	}

	all := append(objs(ss, cnpg, backup, svc, sec), pods...)
	return fakekube.Namespace{
		Name:    "databases",
		Created: createdAgo(120 * 24 * time.Hour),
		Labels:  map[string]string{"app.kubernetes.io/name": "databases", "team": "platform"},
		Objects: all, PodMetrics: metrics,
	}
}

// ---- platform / operator namespaces ----------------------------------------

func argocdNamespace() fakekube.Namespace {
	server, _ := app("argocd", "argocd-server", "quay.io/argoproj/argocd:v2.11.0", 2, prodWorkers, 90*24*60)
	repo, _ := app("argocd", "argocd-repo-server", "quay.io/argoproj/argocd:v2.11.0", 2, prodWorkers, 90*24*60)
	ctrl := statefulSet("argocd-application-controller", 1, 1)
	svc := service("argocd-server", "argocd", "argocd-server", corev1.ServiceTypeClusterIP, "")
	apps := objs(
		customResource("argoproj.io/v1alpha1", "Application", "storefront", "argocd"),
		customResource("argoproj.io/v1alpha1", "Application", "checkout", "argocd"),
		customResource("argoproj.io/v1alpha1", "Application", "payments", "argocd"),
		customResource("argoproj.io/v1alpha1", "Rollout", "storefront-web", "argocd"),
	)
	all := append(append(server, repo...), append(objs(ctrl, svc), apps...)...)
	return fakekube.Namespace{
		Name: "argocd", Created: createdAgo(150 * 24 * time.Hour),
		Labels: map[string]string{"app.kubernetes.io/name": "argocd", "team": "platform"}, Objects: all,
	}
}

func monitoringNamespace() fakekube.Namespace {
	graf, _ := app("monitoring", "grafana", "grafana/grafana:10.4.2", 1, prodWorkers, 60*24*60)
	prom := statefulSet("prometheus-k8s", 2, 2)
	am := statefulSet("alertmanager-main", 3, 3)
	crs := objs(
		customResource("monitoring.coreos.com/v1", "ServiceMonitor", "storefront-web", "monitoring"),
		customResource("monitoring.coreos.com/v1", "ServiceMonitor", "checkout-api", "monitoring"),
		customResource("monitoring.coreos.com/v1", "ServiceMonitor", "orders-db", "monitoring"),
		customResource("operator.victoriametrics.com/v1beta1", "VMAgent", "vmagent", "monitoring"),
	)
	all := graf
	all = append(all, prom, am)
	all = append(all, crs...)
	return fakekube.Namespace{
		Name: "monitoring", Created: createdAgo(150 * 24 * time.Hour),
		Labels: map[string]string{"app.kubernetes.io/name": "monitoring", "team": "platform"}, Objects: all,
	}
}

func certManagerNamespace() fakekube.Namespace {
	cm, _ := app("cert-manager", "cert-manager", "quay.io/jetstack/cert-manager-controller:v1.14.4", 1, prodWorkers, 150*24*60)
	wh, _ := app("cert-manager", "cert-manager-webhook", "quay.io/jetstack/cert-manager-webhook:v1.14.4", 1, prodWorkers, 150*24*60)
	ca, _ := app("cert-manager", "cert-manager-cainjector", "quay.io/jetstack/cert-manager-cainjector:v1.14.4", 1, prodWorkers, 150*24*60)
	certs := objs(
		customResource("cert-manager.io/v1", "Certificate", "storefront-tls", "cert-manager"),
		customResource("cert-manager.io/v1", "Certificate", "payments-tls", "cert-manager"),
	)
	all := append(append(cm, wh...), append(ca, certs...)...)
	return fakekube.Namespace{
		Name: "cert-manager", Created: createdAgo(150 * 24 * time.Hour),
		Labels: map[string]string{"app.kubernetes.io/name": "cert-manager", "team": "platform"}, Objects: all,
	}
}

func fluxNamespace() fakekube.Namespace {
	src, _ := app("flux-system", "source-controller", "ghcr.io/fluxcd/source-controller:v1.2.4", 1, prodWorkers, 150*24*60)
	kus, _ := app("flux-system", "kustomize-controller", "ghcr.io/fluxcd/kustomize-controller:v1.2.2", 1, prodWorkers, 150*24*60)
	crs := objs(
		customResource("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "apps", "flux-system"),
		customResource("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "infrastructure", "flux-system"),
	)
	all := src
	all = append(all, kus...)
	all = append(all, crs...)
	return fakekube.Namespace{
		Name: "flux-system", Created: createdAgo(150 * 24 * time.Hour),
		Labels: map[string]string{"app.kubernetes.io/name": "flux", "team": "platform"}, Objects: all,
	}
}

func externalSecretsNamespace() fakekube.Namespace {
	es, _ := app("external-secrets", "external-secrets", "ghcr.io/external-secrets/external-secrets:v0.9.13", 2, prodWorkers, 120*24*60)
	crs := objs(
		customResource("external-secrets.io/v1beta1", "ExternalSecret", "stripe-api-key", "external-secrets"),
		customResource("external-secrets.io/v1beta1", "ExternalSecret", "orders-db-app", "external-secrets"),
		// A real but uncurated operator → HashHue monogram tile.
		customResource("bitnami.com/v1alpha1", "SealedSecret", "registry-creds", "external-secrets"),
	)
	all := es
	all = append(all, crs...)
	return fakekube.Namespace{
		Name: "external-secrets", Created: createdAgo(120 * 24 * time.Hour),
		Labels: map[string]string{"app.kubernetes.io/name": "external-secrets", "team": "platform"}, Objects: all,
	}
}

func gatekeeperNamespace() fakekube.Namespace {
	ctrl, _ := app("gatekeeper-system", "gatekeeper-controller-manager", "openpolicyagent/gatekeeper:v3.15.1", 3, prodWorkers, 130*24*60)
	audit, _ := app("gatekeeper-system", "gatekeeper-audit", "openpolicyagent/gatekeeper:v3.15.1", 1, prodWorkers, 130*24*60)
	crs := objs(
		customResource("templates.gatekeeper.sh/v1", "ConstraintTemplate", "k8srequiredlabels", "gatekeeper-system"),
		customResource("templates.gatekeeper.sh/v1", "ConstraintTemplate", "k8sallowedrepos", "gatekeeper-system"),
	)
	all := append(append(ctrl, audit...), crs...)
	return fakekube.Namespace{
		Name: "gatekeeper-system", Created: createdAgo(130 * 24 * time.Hour),
		Labels: map[string]string{"app.kubernetes.io/name": "gatekeeper", "team": "platform"}, Objects: all,
	}
}

func ingressNginxNamespace() fakekube.Namespace {
	ctrl, ctrlRS := app("ingress-nginx", "ingress-nginx-controller", "registry.k8s.io/ingress-nginx/controller:v1.10.0", 3, prodWorkers, 150*24*60)
	svc := service("ingress-nginx-controller", "ingress-nginx", "ingress-nginx-controller", corev1.ServiceTypeLoadBalancer, "203.0.113.10")
	gw := customResource("gateway.networking.k8s.io/v1", "Gateway", "acme-gateway", "ingress-nginx")
	all := ctrl
	all = append(all, svc, gw)
	return fakekube.Namespace{
		Name: "ingress-nginx", Created: createdAgo(150 * 24 * time.Hour),
		Labels:  map[string]string{"app.kubernetes.io/name": "ingress-nginx", "team": "platform"},
		Objects: all,
		PodMetrics: []fakekube.PodMetric{
			{Name: ctrlRS + "-" + fleetHash(0*97+nameHash("ingress-nginx-controller")), Containers: []fakekube.ContainerMetric{{Name: "ingress-nginx-controller", CPU: "420m", Memory: "260Mi"}}},
		},
	}
}

// ---- kube-system: the cluster's own plumbing -------------------------------
func kubeSystemNamespace() fakekube.Namespace {
	coredns, _ := app("kube-system", "coredns", "registry.k8s.io/coredns/coredns:v1.11.1", 2, prodWorkers, 417*24*60)
	metricsServer, _ := app("kube-system", "metrics-server", "registry.k8s.io/metrics-server/metrics-server:v0.7.1", 1, prodWorkers, 417*24*60)
	ciliumOp, _ := app("kube-system", "cilium-operator", "quay.io/cilium/operator-generic:v1.15.3", 1, prodWorkers, 417*24*60)
	kubeProxy := daemonSet("kube-proxy", map[string]string{"kubernetes.io/os": "linux"})
	cilium := daemonSet("cilium", map[string]string{"kubernetes.io/os": "linux"})
	netpol := customResource("cilium.io/v2", "CiliumNetworkPolicy", "default-deny-egress", "kube-system")

	// A node-bootstrap pod still running its init containers, and one whose init
	// step is failing (the Init:* status branches).
	initProgress := podFrom("node-setup-fb12-zx", "kube-system", &podOpts{
		app: "node-setup", node: "worker-2",
		initStatuses: []corev1.ContainerStatus{
			terminatedContainer("sysctl", "Completed", 0),
			waitingContainer("install-cni", "PodInitializing", 0),
		},
		statuses: []corev1.ContainerStatus{waitingContainer("agent", "PodInitializing", 0)},
	})
	initFailed := podFrom("node-setup-fb12-q7", "kube-system", &podOpts{
		app: "node-setup", node: "worker-5",
		initStatuses: []corev1.ContainerStatus{waitingContainer("install-cni", "CrashLoopBackOff", 7)},
		statuses:     []corev1.ContainerStatus{waitingContainer("agent", "PodInitializing", 0)},
	})
	// A one-shot migration that completed, and a pod being torn down.
	migrated := podFrom("schema-migrate-1-abc", "kube-system", &podOpts{
		app: "schema-migrate", node: "worker-1", phase: corev1.PodSucceeded,
		statuses: []corev1.ContainerStatus{terminatedContainer("migrate", "Completed", 0)},
	})
	terminating := podFrom("cilium-zq4t9", "kube-system", &podOpts{
		app: "cilium", node: "worker-5",
		statuses: []corev1.ContainerStatus{waitingContainer("cilium-agent", "Terminating", 0)},
	})
	terminating.DeletionTimestamp = ptrTime(created(2))

	regSecret := secret("registry-creds", "kube-system", map[string][]byte{
		"username":          []byte("acme-ci"),
		"password":          []byte("never-rendered-in-the-ui-000000"),
		".dockerconfigjson": []byte("{\"auths\":{}}"),
		"ca.crt":            []byte("-----BEGIN CERTIFICATE-----"),
	})

	all := coredns
	all = append(all, metricsServer...)
	all = append(all, ciliumOp...)
	all = append(all, kubeProxy, cilium, netpol, initProgress, initFailed, migrated, terminating, regSecret)
	return fakekube.Namespace{
		Name: "kube-system", Created: createdAgo(417 * 24 * time.Hour),
		Labels:  map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		Objects: all,
	}
}

// defaultNamespace is the empty `default` namespace every cluster has (the
// empty-list render path), realistic rather than a synthetic "empty".
func defaultNamespace() fakekube.Namespace {
	return fakekube.Namespace{
		Name:    "default",
		Created: createdAgo(417 * 24 * time.Hour),
		Labels:  map[string]string{"kubernetes.io/metadata.name": "default"},
	}
}

// clusterScopedKinds registers the built-in cluster-scoped resource types a real
// cluster serves beyond Namespaces/Nodes/PersistentVolumes, so the
// cluster-resources sidebar reads like a real cluster instead of a bare three
// entries. They ride the same kind-registration path as CRDs (the engine treats
// "register a kind + serve its objects" uniformly).
func clusterScopedKinds() []fakekube.CRD {
	return []fakekube.CRD{
		{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass", Plural: "storageclasses", Namespaced: false},
		{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClass", Plural: "ingressclasses", Namespaced: false},
		{Group: "scheduling.k8s.io", Version: "v1", Kind: "PriorityClass", Plural: "priorityclasses", Namespaced: false},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Plural: "clusterroles", Namespaced: false},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding", Plural: "clusterrolebindings", Namespaced: false},
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition", Plural: "customresourcedefinitions", Namespaced: false},
	}
}

// clusterScopedObjects seeds a believable set of cluster-scoped objects: storage
// classes, the ingress class, priority classes, the common RBAC cluster roles +
// bindings, and one CustomResourceDefinition per installed operator CRD (so the
// CRD list mirrors the operators on the cluster).
func clusterScopedObjects() []runtime.Object {
	const bootstrap = 417 * 24 * 60 // cluster-bootstrap age (minutes)
	out := []runtime.Object{
		clusterCR("storage.k8s.io/v1", "StorageClass", "fast-ssd", bootstrap),
		clusterCR("storage.k8s.io/v1", "StorageClass", "standard", bootstrap),
		clusterCR("storage.k8s.io/v1", "StorageClass", "gp3", 120*24*60),
		clusterCR("networking.k8s.io/v1", "IngressClass", "nginx", bootstrap),
		clusterCR("scheduling.k8s.io/v1", "PriorityClass", "system-cluster-critical", bootstrap),
		clusterCR("scheduling.k8s.io/v1", "PriorityClass", "system-node-critical", bootstrap),
		clusterCR("scheduling.k8s.io/v1", "PriorityClass", "high-priority", 90*24*60),
	}
	for _, name := range []string{
		"cluster-admin", "admin", "edit", "view",
		"cert-manager-controller", "argocd-application-controller", "prometheus-k8s", "gatekeeper-manager",
	} {
		out = append(out, clusterCR("rbac.authorization.k8s.io/v1", "ClusterRole", name, bootstrap))
	}
	for _, name := range []string{"cluster-admin", "cert-manager-controller", "argocd-application-controller"} {
		out = append(out, clusterCR("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", name, bootstrap))
	}
	for _, crd := range demoCRDs() {
		out = append(out, clusterCR("apiextensions.k8s.io/v1", "CustomResourceDefinition", crd.Plural+"."+crd.Group, 150*24*60))
	}
	return out
}

// demoCRDs registers every custom-resource group-version-kind the scenario
// serves, so the list routes resolve and the discovery documents carry the
// groups (and the curated icon families + monogram fallbacks render).
func demoCRDs() []fakekube.CRD {
	return []fakekube.CRD{
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Plural: "certificates", Namespaced: true},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy", Plural: "ciliumnetworkpolicies", Namespaced: true},
		{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application", Plural: "applications", Namespaced: true},
		{Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout", Plural: "rollouts", Namespaced: true},
		{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMAgent", Plural: "vmagents", Namespaced: true},
		{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor", Plural: "servicemonitors", Namespaced: true},
		{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret", Plural: "externalsecrets", Namespaced: true},
		{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject", Plural: "scaledobjects", Namespaced: true},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute", Plural: "httproutes", Namespaced: true},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway", Plural: "gateways", Namespaced: true},
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization", Plural: "kustomizations", Namespaced: true},
		{Group: "templates.gatekeeper.sh", Version: "v1", Kind: "ConstraintTemplate", Plural: "constrainttemplates", Namespaced: true},
		{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster", Plural: "clusters", Namespaced: true},
		{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup", Plural: "backups", Namespaced: true},
		{Group: "bitnami.com", Version: "v1alpha1", Kind: "SealedSecret", Plural: "sealedsecrets", Namespaced: true},
	}
}

// ---- staging: a smaller, all-healthy mirror --------------------------------
func stagingCluster() fakekube.Cluster {
	nodes := objs(
		node("staging-cp", []string{"control-plane"}, true, false),
		node("staging-1", []string{"worker"}, true, false),
		node("staging-2", []string{"worker"}, true, false),
	)
	workers := []string{"staging-1", "staging-2"}

	store, storeRS := app("storefront", "storefront-web", "registry.example.com/storefront-web:v4.3.0-rc1", 3, workers, 6*60)
	storeSvc := service("storefront-web", "storefront", "storefront-web", corev1.ServiceTypeClusterIP, "")
	storeIng := ingress("storefront", "storefront", "staging.acme.example", "storefront-web", true)
	cert := customResource("cert-manager.io/v1", "Certificate", "storefront-tls", "storefront")

	checkout, _ := app("checkout", "checkout-api", "registry.example.com/checkout-api:v2.0.0", 2, workers, 3*60)
	checkoutSvc := service("checkout-api", "checkout", "checkout-api", corev1.ServiceTypeClusterIP, "")

	storeAll := store
	storeAll = append(storeAll, storeSvc, storeIng, cert)
	checkoutAll := checkout
	checkoutAll = append(checkoutAll, checkoutSvc)

	const bootstrap = 200 * 24 * 60
	clusterObjs := []runtime.Object{
		clusterCR("storage.k8s.io/v1", "StorageClass", "fast-ssd", bootstrap),
		clusterCR("storage.k8s.io/v1", "StorageClass", "standard", bootstrap),
		clusterCR("networking.k8s.io/v1", "IngressClass", "nginx", bootstrap),
		clusterCR("scheduling.k8s.io/v1", "PriorityClass", "system-cluster-critical", bootstrap),
		clusterCR("rbac.authorization.k8s.io/v1", "ClusterRole", "cluster-admin", bootstrap),
		clusterCR("rbac.authorization.k8s.io/v1", "ClusterRole", "view", bootstrap),
		clusterCR("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "cluster-admin", bootstrap),
		clusterCR("apiextensions.k8s.io/v1", "CustomResourceDefinition", "certificates.cert-manager.io", 150*24*60),
	}

	stagingCRDs := []fakekube.CRD{
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Plural: "certificates", Namespaced: true},
	}
	stagingCRDs = append(stagingCRDs, clusterScopedKinds()...)

	return fakekube.Cluster{
		Name:           "staging",
		Nodes:          nodes,
		ClusterObjects: clusterObjs,
		FillEmptyLists: true,
		NodeMetrics: []fakekube.NodeMetric{
			{Name: "staging-cp", CPU: "600m", Memory: "3Gi"},
			{Name: "staging-1", CPU: "1400m", Memory: "6Gi"},
			{Name: "staging-2", CPU: "1100m", Memory: "5Gi"},
		},
		CRDs: stagingCRDs,
		Namespaces: []fakekube.Namespace{
			{
				Name: "storefront", Created: createdAgo(20 * 24 * time.Hour),
				Labels:  map[string]string{"app.kubernetes.io/name": "storefront", "env": "staging"},
				Objects: storeAll,
				PodMetrics: []fakekube.PodMetric{
					{Name: storeRS + "-" + fleetHash(0*97+nameHash("storefront-web")), Containers: []fakekube.ContainerMetric{{Name: "storefront-web", CPU: "90m", Memory: "160Mi"}}},
				},
			},
			{
				Name: "checkout", Created: createdAgo(20 * 24 * time.Hour),
				Labels:  map[string]string{"app.kubernetes.io/name": "checkout", "env": "staging"},
				Objects: checkoutAll,
			},
			defaultNamespace(),
		},
	}
}
