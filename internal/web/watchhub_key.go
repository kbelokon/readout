package web

import (
	"fmt"

	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/labels"
)

// watchHubKey identifies ONE shared upstream list+watch. Two Live streams may
// read from the same source only when every field matches, so the key carries
// exactly what changes the upstream request or the identity the apiserver
// evaluates it as: the credential (kube.Client.IdentityKey), the cluster, the
// resolved resource (group/version/plural/scope), the collection path's
// namespace, and the label selector.
//
// The identity field commits to the viewer's bearer token for a passthrough
// client. The key is a map key only: never log it, render it, or put it in a
// metric label.
type watchHubKey struct {
	identity  string
	cluster   string
	resource  string
	namespace string
	selector  string
}

// newWatchHubKey builds the key for one stream's scope, canonicalizing the two
// inputs whose spelling varies for an identical upstream request:
//
//   - Namespace: a cluster-scoped type and the all-namespaces pseudo-namespace
//     both address the unscoped collection path, the same collapse the request
//     URL itself makes, so both canonicalize to the empty namespace.
//   - Selector: parsed and re-serialized, so `a=b,c=d` and `c=d,a=b` are one
//     source instead of two identical watches.
//
// An unparsable selector is an error rather than a distinct key: the apiserver
// would reject the list anyway, and a rejected source would be created once per
// spelling of the same broken selector.
func newWatchHubKey(client *kube.Client, cluster string, rt *kube.ResourceType, namespace, selector string) (watchHubKey, error) {
	parsed, err := labels.Parse(selector)
	if err != nil {
		return watchHubKey{}, fmt.Errorf("parse label selector %q: %w", selector, err)
	}
	if !rt.Namespaced || namespace == kube.AllNamespaces {
		namespace = ""
	}
	return watchHubKey{
		identity:  client.IdentityKey(),
		cluster:   cluster,
		resource:  rt.Key(),
		namespace: namespace,
		selector:  parsed.String(),
	}, nil
}
