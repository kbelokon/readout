package web

import (
	"strings"
	"testing"

	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/client-go/rest"
)

func testHubClient(t *testing.T, host string) *kube.Client {
	t.Helper()
	client, err := kube.NewClient(&rest.Config{Host: host}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func hubDeployments() *kube.ResourceType {
	return &kube.ResourceType{
		Group:      "apps",
		Version:    "v1",
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Plural:     "deployments",
		Namespaced: true,
		Verbs:      []string{"get", "list", "watch"},
	}
}

func mustHubKey(t *testing.T, client *kube.Client, cluster string, rt *kube.ResourceType, namespace, selector string) watchHubKey {
	t.Helper()
	key, err := newWatchHubKey(client, cluster, rt, namespace, selector)
	if err != nil {
		t.Fatalf("newWatchHubKey(%q, %q): %v", namespace, selector, err)
	}
	return key
}

// Two spellings of one selector are one upstream watch, not two.
func TestWatchHubKeyCanonicalizesSelectorOrder(t *testing.T) {
	client := testHubClient(t, "https://one.example")
	rt := hubDeployments()

	forward := mustHubKey(t, client, "prod", rt, "default", "a=b,c=d")
	reversed := mustHubKey(t, client, "prod", rt, "default", "c=d,a=b")
	if forward != reversed {
		t.Fatalf("selector order should canonicalize: %+v != %+v", forward, reversed)
	}
	spaced := mustHubKey(t, client, "prod", rt, "default", "a = b, c = d")
	if spaced != forward {
		t.Fatalf("selector whitespace should canonicalize: %+v != %+v", spaced, forward)
	}
	if different := mustHubKey(t, client, "prod", rt, "default", "a=b"); different == forward {
		t.Fatal("a narrower selector is a different upstream watch")
	}
	if unselected := mustHubKey(t, client, "prod", rt, "default", ""); unselected.selector != "" {
		t.Fatalf("empty selector = %q, want empty", unselected.selector)
	}
}

// Every dimension that changes the upstream request or the identity it is
// evaluated as must split the key.
func TestWatchHubKeySeparatesScopes(t *testing.T) {
	base := testHubClient(t, "https://one.example")
	otherCluster := testHubClient(t, "https://two.example")
	rt := hubDeployments()
	reference := mustHubKey(t, base, "prod", rt, "default", "a=b")

	viewerOne, err := base.WithBearer("viewer-one")
	if err != nil {
		t.Fatal(err)
	}
	viewerTwo, err := base.WithBearer("viewer-two")
	if err != nil {
		t.Fatal(err)
	}
	viewerOneAgain, err := base.WithBearer("viewer-one")
	if err != nil {
		t.Fatal(err)
	}

	betaVersion := hubDeployments()
	betaVersion.Version = "v1beta1"
	betaVersion.APIVersion = "apps/v1beta1"

	for _, tc := range []struct {
		name string
		key  watchHubKey
	}{
		{"token", mustHubKey(t, viewerOne, "prod", rt, "default", "a=b")},
		{"other token", mustHubKey(t, viewerTwo, "prod", rt, "default", "a=b")},
		{"apiVersion", mustHubKey(t, base, "prod", betaVersion, "default", "a=b")},
		{"namespace", mustHubKey(t, base, "prod", rt, "kube-system", "a=b")},
		{"cluster name", mustHubKey(t, base, "staging", rt, "default", "a=b")},
		{"cluster client", mustHubKey(t, otherCluster, "prod", rt, "default", "a=b")},
		{"selector", mustHubKey(t, base, "prod", rt, "default", "a=c")},
	} {
		if tc.key == reference {
			t.Fatalf("%s should not share a source with the reference scope: %+v", tc.name, tc.key)
		}
	}

	// The same viewer token is the same source: that is the whole point of
	// deriving identity from the token rather than from the client pointer.
	if one, again := mustHubKey(t, viewerOne, "prod", rt, "default", "a=b"), mustHubKey(t, viewerOneAgain, "prod", rt, "default", "a=b"); one != again {
		t.Fatalf("the same viewer token should share one source: %+v != %+v", one, again)
	}
	if strings.Contains(mustHubKey(t, viewerOne, "prod", rt, "default", "").identity, "viewer-one") {
		t.Fatal("key identity leaks the raw viewer token")
	}
}

// The namespace collapses exactly like the request URL does: cluster-scoped
// types and the all-namespaces pseudo-namespace both address the unscoped
// collection path, so they must not fork the source.
func TestWatchHubKeyCanonicalizesNamespace(t *testing.T) {
	client := testHubClient(t, "https://one.example")
	rt := hubDeployments()

	allNamespaces := mustHubKey(t, client, "prod", rt, kube.AllNamespaces, "")
	unscoped := mustHubKey(t, client, "prod", rt, "", "")
	if allNamespaces != unscoped {
		t.Fatalf("all-namespaces and the empty namespace address one collection: %+v != %+v", allNamespaces, unscoped)
	}

	nodes := &kube.ResourceType{Version: "v1", APIVersion: "v1", Kind: "Node", Plural: "nodes", Verbs: []string{"list", "watch"}}
	ignored := mustHubKey(t, client, "prod", nodes, "default", "")
	clusterScoped := mustHubKey(t, client, "prod", nodes, "", "")
	if ignored != clusterScoped {
		t.Fatalf("a cluster-scoped type ignores the namespace: %+v != %+v", ignored, clusterScoped)
	}
	if ignored == unscoped {
		t.Fatal("different resources must not share a source")
	}
}

func TestWatchHubKeyRejectsUnparsableSelector(t *testing.T) {
	client := testHubClient(t, "https://one.example")
	key, err := newWatchHubKey(client, "prod", hubDeployments(), "default", "a=!b")
	if err == nil {
		t.Fatalf("unparsable selector should be rejected, got key %+v", key)
	}
	if key != (watchHubKey{}) {
		t.Fatalf("rejected selector should yield the zero key, got %+v", key)
	}
}
