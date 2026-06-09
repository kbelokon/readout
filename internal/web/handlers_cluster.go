package web

import (
	"net/http"
	"sort"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

func (s *Server) clusters(w http.ResponseWriter, r *http.Request) {
	data := s.buildClustersData(r.URL.Query().Get("selector"), r.URL.Query().Get("filter"))
	s.pageComponent(w, r, "Clusters", templates.Clusters(data))
}

func (s *Server) cluster(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	ctx := r.Context()
	client := s.kubeClient(r, cluster)
	namespaceRT, err := client.FindResource(ctx, "namespaces", false, "")
	if err != nil {
		s.error(w, r, err)
		return
	}
	namespaces, err := client.List(ctx, &namespaceRT, kube.ListOptions{})
	if err != nil {
		s.error(w, r, err)
		return
	}
	clusterTypes, err := client.ClusterResourceTypes(ctx)
	if err != nil {
		s.error(w, r, err)
		return
	}
	data := s.buildClusterData(cluster, &namespaceRT, namespaces, clusterTypes)
	s.pageComponentWithClients(w, r, cluster.Name+" Cluster", requestKubeClients{cluster.Name: client}, templates.Cluster(data))
}

func (s *Server) clusterResourceTypes(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	client := s.kubeClient(r, cluster)
	types, err := client.ClusterResourceTypes(r.Context())
	if err != nil {
		s.error(w, r, err)
		return
	}
	s.renderResourceTypes(w, r, cluster.Name, "", requestKubeClients{cluster.Name: client}, types)
}

func (s *Server) namespacedResourceTypes(w http.ResponseWriter, r *http.Request) {
	cluster, err := s.oneCluster(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	namespace := r.PathValue("namespace")
	if namespace != "" && namespace != kube.AllNamespaces && !s.namespaceAllowed(namespace) {
		s.error(w, r, statusError{status: http.StatusForbidden, message: "namespace is not allowed"})
		return
	}
	client := s.kubeClient(r, cluster)
	types, err := client.NamespacedResourceTypes(r.Context())
	if err != nil {
		s.error(w, r, err)
		return
	}
	s.renderResourceTypes(w, r, cluster.Name, namespace, requestKubeClients{cluster.Name: client}, types)
}

func (s *Server) renderResourceTypes(w http.ResponseWriter, r *http.Request, cluster, namespace string, clients requestKubeClients, types []kube.ResourceType) {
	data := buildResourceTypesData(cluster, namespace, types)
	s.pageComponentWithClients(w, r, "Resource Types", clients, templates.ResourceTypes(data))
}

func sortedResourceTypesForDisplay(types []kube.ResourceType) []kube.ResourceType {
	out := append([]kube.ResourceType(nil), types...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].APIVersion != out[j].APIVersion {
			return out[i].APIVersion < out[j].APIVersion
		}
		if out[i].Plural != out[j].Plural {
			return out[i].Plural < out[j].Plural
		}
		return !out[i].Namespaced && out[j].Namespaced
	})
	return out
}

func uniqueResourceTypesForDisplay(types []kube.ResourceType) []kube.ResourceType {
	seen := map[string]bool{}
	var out []kube.ResourceType
	for i := range types {
		rt := &types[i]
		if seen[rt.Key()] {
			continue
		}
		seen[rt.Key()] = true
		out = append(out, *rt)
	}
	return out
}

func apiGroup(apiVersion string) string {
	group, _, ok := strings.Cut(apiVersion, "/")
	if !ok {
		return ""
	}
	return group
}

func apiVersionVersion(apiVersion string) string {
	_, version, ok := strings.Cut(apiVersion, "/")
	if !ok {
		return apiVersion
	}
	return version
}

func isCRD(apiVersion string) bool {
	group := apiGroup(apiVersion)
	switch group {
	case "", "apps", "batch", "autoscaling", "policy", "extensions":
		return false
	}
	return group != "k8s.io" && !strings.HasSuffix(group, ".k8s.io")
}
