package web

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

// build_cluster.go assembles the templ view models for the clusters-list,
// cluster-overview, and resource-types pages from the request + kube data. It is
// the handler-side seam: every request-derived value (filtered rows, chip hrefs
// with their url.QueryEscape'd selector, age cell classes, the kind links) is
// resolved here so the templ components render plain data. The hrefs/classes/
// escaping are pinned by the behavior-fact net.

// anchorChips builds the /clusters?selector= label pills (anchor style) used on
// the clusters list and the cluster-overview meta line. The href uses
// url.QueryEscape on key and value (matching the prior render); the class is the
// full ro-chip class incl. the app.kubernetes.io accent.
func anchorChips(labels map[string]string) []templates.Chip {
	keys := sortedKeys(labels)
	chips := make([]templates.Chip, 0, len(keys))
	for _, key := range keys {
		val := labels[key]
		chips = append(chips, templates.Chip{
			Href:  "/clusters?selector=" + url.QueryEscape(key) + "=" + url.QueryEscape(val),
			Class: chipClass(key),
			Key:   key,
			Val:   val,
		})
	}
	return chips
}

// labelChips builds the non-link namespace-row label pills (a <span> with an
// inner <span class="tag">), matching the cluster.html namespace chip markup.
func labelChips(labels map[string]string) []templates.LabelChip {
	keys := sortedKeys(labels)
	chips := make([]templates.LabelChip, 0, len(keys))
	for _, key := range keys {
		chips = append(chips, templates.LabelChip{Class: chipClass(key), Key: key, Val: labels[key]})
	}
	return chips
}

// chipClass is the full ro-chip class string with the app.kubernetes.io accent
// appended, matching appLabelClass(key) used inline in the prior render.
func chipClass(key string) string {
	if strings.HasPrefix(key, "app.kubernetes.io/") {
		return "ro-chip ro-label-app"
	}
	return "ro-chip"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// buildClustersData assembles the clusters-list view model: the count, the form
// round-trip values, the filtered in-cluster rows, and the external-cluster
// rows. The filter matches the prior handler (case-insensitive over
// name+url+labels).
func (s *Server) buildClustersData(selector, filter string) templates.ClustersData {
	clusters := s.manager.Clusters()
	data := templates.ClustersData{
		Count:         len(clusters) + len(s.cfg.ExternalClusters),
		SelectorValue: selector,
		FilterValue:   filter,
	}
	filterText := strings.ToLower(filter)
	for _, cluster := range clusters {
		if filterText != "" && !strings.Contains(strings.ToLower(cluster.Name+" "+cluster.URL+" "+formatLabels(cluster.Labels)), filterText) {
			continue
		}
		data.Rows = append(data.Rows, templates.ClusterRow{
			Name:  cluster.Name,
			URL:   cluster.URL,
			Chips: anchorChips(cluster.Labels),
		})
	}
	for name, href := range s.cfg.ExternalClusters {
		data.ExternalRows = append(data.ExternalRows, templates.ExternalClusterRow{Name: name, URL: href})
	}
	return data
}

// buildClusterData assembles the cluster-overview view model: the meta chips,
// the allowed namespace rows (with their pods link, label chips, and the
// precomputed age cell class + created text), and the cluster resource-type
// rows.
func (s *Server) buildClusterData(cluster *kube.Cluster, namespaceRT *kube.ResourceType, namespaces *unstructured.UnstructuredList, clusterTypes []kube.ResourceType) templates.ClusterData {
	data := templates.ClusterData{
		Name:         cluster.Name,
		URL:          cluster.URL,
		ClusterChips: anchorChips(cluster.Labels),
	}
	for i := range namespaces.Items {
		object := kube.NewObject(namespaceRT, &namespaces.Items[i])
		if !s.namespaceAllowed(object.Name()) {
			continue
		}
		data.Namespaces = append(data.Namespaces, templates.NamespaceRow{
			Name:     object.Name(),
			PodsHref: fmt.Sprintf("/clusters/%s/namespaces/%s/pods", url.PathEscape(cluster.Name), url.PathEscape(object.Name())),
			Chips:    labelChips(object.Labels()),
			AgeClass: s.ageClass(object.CreationTimestamp()),
			Created:  formatTimestamp(object.CreationTimestamp()),
		})
	}
	sortedTypes := sortedResourceTypesForDisplay(clusterTypes)
	for i := range sortedTypes {
		rt := &sortedTypes[i]
		data.ResourceTypes = append(data.ResourceTypes, templates.ClusterResourceTypeRow{
			Href:    fmt.Sprintf("/clusters/%s/%s", url.PathEscape(cluster.Name), url.PathEscape(rt.Plural)),
			Kind:    rt.Kind,
			IsCRD:   isCRD(rt.APIVersion),
			Group:   first(apiGroup(rt.APIVersion), "core"),
			Version: apiVersionVersion(rt.APIVersion),
		})
	}
	return data
}

// buildResourceTypesData assembles the resource-types view model (cluster +
// namespaced). namespace=="" => the cluster-scoped page. It resolves the row
// href, the CRD flag, the boolean, and the tab hrefs.
func buildResourceTypesData(cluster, namespace string, types []kube.ResourceType) templates.ResourceTypesData {
	types = sortedResourceTypesForDisplay(uniqueResourceTypesForDisplay(types))
	nsForLink := namespace
	if nsForLink == "" {
		nsForLink = "default"
	}
	data := templates.ResourceTypesData{
		Cluster:       cluster,
		NamespaceShow: namespace != "",
		Namespace:     namespace,
		Count:         len(types),
		ClusterActive: namespace == "",
		ClusterTab:    fmt.Sprintf("/clusters/%s/_resource-types", url.PathEscape(cluster)),
		NamespacedTab: fmt.Sprintf("/clusters/%s/namespaces/%s/_resource-types", url.PathEscape(cluster), url.PathEscape(nsForLink)),
	}
	for i := range types {
		rt := &types[i]
		href := fmt.Sprintf("/clusters/%s/%s", cluster, rt.Plural)
		if rt.Namespaced {
			ns := namespace
			if ns == "" {
				ns = kube.AllNamespaces
			}
			href = fmt.Sprintf("/clusters/%s/namespaces/%s/%s", cluster, ns, rt.Plural)
		}
		data.Rows = append(data.Rows, templates.ResourceTypeRow{
			Href:       href,
			Kind:       rt.Kind,
			IsCRD:      isCRD(rt.APIVersion),
			Group:      first(apiGroup(rt.APIVersion), "core"),
			Version:    apiVersionVersion(rt.APIVersion),
			Namespaced: rt.Namespaced,
		})
	}
	return data
}
