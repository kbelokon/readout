package fakekube

// tables_test.go pins the Table-form generation: every supported kind serves
// a meta.k8s.io Table whose columnDefinitions contain the column NAMES readout's
// curated list cells actually read. The expected columns below are an independent
// contract, not a parser for internal/web implementation syntax. Columns added by
// per-request decorators (Rollout, Labels, TLS, External-IP, Selector, the jobs
// Status, the events Count, the node CPU/Memory/Pods/Conditions, and metrics usage)
// are intentionally absent: readout guarantees those even when the Table omits
// them, so they are not part of the Table contract this unit owns.
//
// So the test catches "the columns readout's renderer needs from the Table", and
// would FAIL if the registry served a kind List-only (the historical empty-nodes
// bug) or dropped a curated source column (Deployment Ready, CronJob
// Suspend/Last Schedule, …).

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// pluralForKind maps a supported kind to its resource plural (the key
// list_cells.go switches on). Built from the package's own builtinKinds so it
// stays in lockstep with the served paths.
func pluralForKind(kind string) string {
	for _, info := range builtinKinds {
		if info.kind == kind {
			return info.resource
		}
	}
	// Kinds authored but not in builtinKinds (e.g. HorizontalPodAutoscaler):
	// their plural is irrelevant to the oracle because list_cells.go has no
	// curated per-kind branch for them — they assert only Name/Age.
	return ""
}

// consumedColumns is the source-Table side of the web/fakekube contract.
// Name and Age are universal and asserted separately. Keeping this oracle as
// explicit data lets production code be refactored without weakening the test.
func consumedColumns() map[string]map[string]bool {
	return map[string]map[string]bool{
		"configmaps":  {"Data": true},
		"cronjobs":    {"Last Schedule": true, "Suspend": true},
		"deployments": {"Ready": true},
		"events":      {"Last Seen": true, "Message": true, "Object": true, "Type": true},
		"ingresses":   {"Address": true, "Hosts": true},
		"jobs":        {"Completions": true},
		"nodes":       {"Roles": true},
		"secrets":     {"Data": true},
		"services":    {"Port(s)": true},
	}
}

// supportedKinds is one representative object per supported kind, used to seed a
// cluster and read back each kind's served Table. The objects are minimal — the
// test asserts column NAMES, not cell values — but referentially complete enough
// to pass the integrity validator.
func tableColumnNames(t *testing.T, kind string) []string {
	t.Helper()
	kt, _ := tableForKind(kind)
	names := make([]string, 0, len(kt.columns))
	for _, c := range kt.columns {
		names = append(names, c.name)
	}
	return names
}

func TestTableColumns(t *testing.T) {
	consumed := consumedColumns()

	// Every kind the registry serves must carry the columns readout's curated
	// cells read for that kind's plural, plus Name and Age.
	kinds := []string{
		"Pod", "Deployment", "Service", "Node", "Event", "ConfigMap", "Secret",
		"Ingress", "CronJob", "Job", "PersistentVolume", "Namespace",
		// authored kinds with no Table fixture:
		"StatefulSet", "DaemonSet", "ReplicaSet", "HorizontalPodAutoscaler",
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			have := map[string]bool{}
			for _, n := range tableColumnNames(t, kind) {
				have[n] = true
			}

			// Identity + age are required for every kind: the Name cell opens the
			// object and the Age cell assigns its recency bucket.
			// Events are the one printer with no Name column: their identity is
			// the Object column and their age is Last Seen, both curated below.
			if kind != "Event" {
				for _, required := range []string{"Name", "Age"} {
					if !have[required] {
						t.Errorf("%s Table missing universal column %q; columns=%v", kind, required, sortedKeys(have))
					}
				}
			}

			plural := pluralForKind(kind)
			want := consumed[plural]
			// A kind list_cells.go has no curated per-kind branch for (e.g.
			// StatefulSet/ReplicaSet/HPA, or Pod whose cells key on universal
			// names) still must serve Name/Age, asserted above.
			var missing []string
			for col := range want {
				if !have[col] {
					missing = append(missing, col)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s (plural %q) Table missing curated columns %v; columns=%v",
					kind, plural, missing, sortedKeys(have))
			}
		})
	}
}

// TestCRDDefaultTable pins the CRD fallback: a kind with no registered columns
// (every CRD — the scenario model carries no additionalPrinterColumns) gets the
// apiserver's default Name/Age Table, which is exactly what readout's CRD
// list-render path consumes.
func TestCRDDefaultTable(t *testing.T) {
	kt, ok := tableForKind("WidgetThatHasNoRegistry")
	if ok {
		t.Fatal("unknown kind must report the fallback (ok=false), not a registered table")
	}
	names := make([]string, 0, len(kt.columns))
	for _, c := range kt.columns {
		names = append(names, c.name)
	}
	want := []string{"Name", "Age"}
	if len(names) != len(want) {
		t.Fatalf("CRD default table columns = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("CRD default table columns = %v, want %v", names, want)
		}
	}
	// The Name column must be the identity column (format "name") so readout's
	// nameColumn/identity rules find it.
	if kt.columns[0].format != "name" {
		t.Errorf("CRD default Name column format = %q, want \"name\"", kt.columns[0].format)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTableColumnsServed is the wiring proof: it SEEDS one object per supported
// built-in kind into a real Server, fetches each kind's collection with the
// `as=Table` content negotiation, and asserts the SERVED Table carries the
// curated columns. This catches the historical empty-table bug — a kind served
// List-only would return the List form (no columnDefinitions) when readout
// negotiates Table, and the per-kind assertion would fail.
func TestTableColumnsServed(t *testing.T) {
	consumed := consumedColumns()

	c := allKindsCluster()
	srv, err := New(WithoutControl())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	if err := srv.Seed(&c); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

	// kind -> served collection path (the all-namespaces alias for namespaced
	// kinds, the cluster path otherwise).
	paths := map[string]string{
		"Pod":              "/api/v1/pods",
		"Service":          "/api/v1/services",
		"Secret":           "/api/v1/secrets",
		"ConfigMap":        "/api/v1/configmaps",
		"Event":            "/api/v1/events",
		"Node":             "/api/v1/nodes",
		"PersistentVolume": "/api/v1/persistentvolumes",
		"Namespace":        "/api/v1/namespaces",
		"Deployment":       "/apis/apps/v1/deployments",
		"ReplicaSet":       "/apis/apps/v1/replicasets",
		"StatefulSet":      "/apis/apps/v1/statefulsets",
		"DaemonSet":        "/apis/apps/v1/daemonsets",
		"Job":              "/apis/batch/v1/jobs",
		"CronJob":          "/apis/batch/v1/cronjobs",
		"Ingress":          "/apis/networking.k8s.io/v1/ingresses",
	}

	for kind, path := range paths {
		t.Run(kind, func(t *testing.T) {
			doc := getTable(t, srv.URL+path, tableAccept)
			if got, _ := doc["kind"].(string); got != "Table" {
				t.Fatalf("%s negotiated as=Table but served kind=%q (a List-only route serves the empty table)", kind, got)
			}
			have := tableDocColumnNames(t, doc)
			if len(have) == 0 {
				t.Fatalf("%s served Table has no columnDefinitions", kind)
			}
			set := map[string]bool{}
			for _, n := range have {
				set[n] = true
			}
			if kind != "Event" {
				for _, required := range []string{"Name", "Age"} {
					if !set[required] {
						t.Errorf("%s served Table missing universal column %q; columns=%v", kind, required, have)
					}
				}
			}
			plural := pluralForKind(kind)
			var missing []string
			for col := range consumed[plural] {
				if !set[col] {
					missing = append(missing, col)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s (plural %q) served Table missing curated columns %v; columns=%v", kind, plural, missing, have)
			}
			// The served row count must match the seeded objects (the Table item
			// set mirrors the List set); at least one row proves the rows render.
			if rows, _ := doc["rows"].([]any); len(rows) == 0 {
				t.Errorf("%s served Table has zero rows; want the seeded object", kind)
			}
		})
	}
}

func getTable(t *testing.T, url, accept string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s => %d: %s", url, res.StatusCode, body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	return doc
}

func tableDocColumnNames(t *testing.T, doc map[string]any) []string {
	t.Helper()
	cols, _ := doc["columnDefinitions"].([]any)
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := cm["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// allKindsCluster is a referentially-complete cluster with one object per
// supported built-in kind (HPA is not a builtin and is covered by the registry
// oracle in TestTableColumns). The graph satisfies the integrity validator:
// owner chains resolve, the Service selector matches the Pod, the Ingress
// backend names the Service, the Event involvedObject names the Pod, the PVC
// binds the PV and the Pod mounts the PVC.
func allKindsCluster() Cluster {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-1"}}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}},
	}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc-123", Namespace: "default",
			Labels:          map[string]string{"app": "web"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-1",
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
			}},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}},
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "import", Namespace: "default"}}
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "default"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"}}
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: "web.example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: "web",
							Port: networkingv1.ServiceBackendPort{Number: 80},
						}},
					}},
				}},
			}},
		},
	}
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-abc-123", Namespace: "default"},
		Type:           "Normal",
		Reason:         "Scheduled",
		Message:        "Successfully assigned default/web-abc-123 to worker-1",
	}

	return Cluster{
		Name:           "prod",
		Nodes:          []runtime.Object{node},
		ClusterObjects: []runtime.Object{pv},
		Namespaces: []Namespace{{
			Name:    "default",
			Objects: []runtime.Object{deploy, rs, pvc, pod, svc, sts, ds, job, cron, cm, secret, ing, event},
		}},
	}
}

func TestCellPodStatus(t *testing.T) {
	waiting := func(reason string) map[string]any {
		return map[string]any{"state": map[string]any{"waiting": map[string]any{"reason": reason}}}
	}
	terminated := func(reason string, code int64) map[string]any {
		return map[string]any{"state": map[string]any{"terminated": map[string]any{"reason": reason, "exitCode": code}}}
	}
	pod := func(phase string, cs ...map[string]any) map[string]any {
		items := make([]any, len(cs))
		for i, c := range cs {
			items[i] = c
		}
		return map[string]any{"status": map[string]any{"phase": phase, "containerStatuses": items}}
	}

	cases := map[string]struct {
		obj  map[string]any
		want string
	}{
		"running healthy":     {pod("Running", map[string]any{"state": map[string]any{"running": map[string]any{}}}), "Running"},
		"crashloop overrides": {pod("Running", waiting("CrashLoopBackOff")), "CrashLoopBackOff"},
		"imagepull overrides": {pod("Running", waiting("ImagePullBackOff")), "ImagePullBackOff"},
		"terminated reason":   {pod("Failed", terminated("OOMKilled", 137)), "OOMKilled"},
		"terminated exitcode": {pod("Failed", terminated("", 2)), "ExitCode:2"},
		"deletion terminating": {map[string]any{
			"metadata": map[string]any{"deletionTimestamp": "2026-06-15T00:00:00Z"},
			"status":   map[string]any{"phase": "Running"},
		}, "Terminating"},
		"init in progress": {map[string]any{"status": map[string]any{
			"phase":                 "Pending",
			"initContainerStatuses": []any{waiting("PodInitializing")},
		}}, "Init:0/1"},
		"init failed": {map[string]any{"status": map[string]any{
			"phase":                 "Pending",
			"initContainerStatuses": []any{terminated("Error", 1)},
		}}, "Init:Error"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := cellPodStatus(tc.obj); got != tc.want {
				t.Errorf("cellPodStatus = %v, want %v", got, tc.want)
			}
		})
	}
}
