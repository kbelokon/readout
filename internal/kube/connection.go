package kube

import (
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Source identifies where a cluster connection was discovered. It rides the
// Connection (and the runtime Cluster) so the loader can prune/reload per origin
// (D3) and so a cluster's provenance is visible without re-deriving it from the
// config shape.
type Source int

const (
	SourceStatic Source = iota
	SourceKubeconfig
	SourceInCluster
	SourceSecret
)

func (s Source) String() string {
	switch s {
	case SourceStatic:
		return "static"
	case SourceKubeconfig:
		return "kubeconfig"
	case SourceInCluster:
		return "in-cluster"
	case SourceSecret:
		return "secret"
	default:
		return "unknown"
	}
}

// Connection is readout's canonical cluster-connection model: the in-memory
// kubeconfig triple (Context + Cluster + AuthInfo, all from client-go's
// clientcmd/api package) plus a Source tag. Its *rest.Config is produced solely
// by clientcmd (RESTConfig) -- readout sets ZERO rest.Config TLS/auth fields by
// hand. Every TLS and auth method clientcmd maps for free therefore rides the
// model: custom CA (file/data), insecure-skip, server name, bearer token (inline
// or file-with-rotation), client cert/key (file/data), exec credential plugin,
// OIDC auth-provider, and impersonation. One model, every source, one builder.
//
// A nil AuthInfo is valid: it is the static no-auth case where identity is
// supplied per request (WithBearer) rather than baked into the stored model.
type Connection struct {
	Name     string
	Source   Source
	Cluster  *clientcmdapi.Cluster
	AuthInfo *clientcmdapi.AuthInfo
	Context  *clientcmdapi.Context
}

// RESTConfig assembles a single-context api.Config from the triple and hands it
// to clientcmd, which populates the entire *rest.Config. No rest.Config field is
// set here by hand: clientcmd's getServerIdentificationPartialConfig and
// getUserIdentificationPartialConfig copy every TLS/auth field from the
// api.Cluster/api.AuthInfo. The context references the cluster (and the auth info
// only when one is present, mirroring Headlamp's static processManualConfig,
// which leaves the context's auth empty so an authInfo-less connection stays
// valid).
func (c *Connection) RESTConfig() (*rest.Config, error) {
	name := c.Name
	cluster := c.Cluster
	if cluster == nil {
		cluster = &clientcmdapi.Cluster{}
	}

	apiCtx := &clientcmdapi.Context{Cluster: name}
	if c.Context != nil && c.Context.Namespace != "" {
		apiCtx.Namespace = c.Context.Namespace
	}

	conf := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{name: cluster},
		Contexts: map[string]*clientcmdapi.Context{name: apiCtx},
	}
	if c.AuthInfo != nil {
		apiCtx.AuthInfo = name
		conf.AuthInfos = map[string]*clientcmdapi.AuthInfo{name: c.AuthInfo}
	}

	return clientcmd.NewNonInteractiveClientConfig(conf, name, nil, nil).ClientConfig()
}
