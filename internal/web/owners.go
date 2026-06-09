package web

import (
	"fmt"
	"net/http"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
)

func (s *Server) ownerLinks(r *http.Request, client *kube.Client, cluster *kube.Cluster, object *kube.Object) []config.Link {
	refs := object.OwnerReferences()
	if len(refs) == 0 {
		return nil
	}
	var links []config.Link
	for i := range refs {
		ref := &refs[i]
		if ref.Kind == "" || ref.Name == "" {
			continue
		}
		rt, err := client.FindResourceByKind(r.Context(), ref.APIVersion, ref.Kind, object.Resource.Namespaced)
		if err != nil {
			rt, err = client.FindResourceByKind(r.Context(), ref.APIVersion, ref.Kind, false)
		}
		if err != nil {
			continue
		}
		namespace := ""
		if rt.Namespaced {
			namespace = object.Namespace()
		}
		links = append(links, config.Link{
			Href:  resourceHref(cluster.Name, &rt, namespace, ref.Name),
			Title: fmt.Sprintf("%s/%s", ref.Kind, ref.Name),
		})
	}
	return links
}
