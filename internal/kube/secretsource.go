package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// argoClusterSecretLabelSelector selects Argo CD cluster Secrets. Argo labels
// every cluster Secret it manages with this, so a List scoped to it returns the
// cluster set (and only it) in a host namespace (D6).
const argoClusterSecretLabelSelector = "argocd.argoproj.io/secret-type=cluster"

// argoTLSClientConfig is the nested TLS block inside an Argo cluster Secret's
// `config` JSON blob. caData/certData/keyData are base64 strings INSIDE the JSON
// (decoded a second time here -- the Secret API already base64-decoded the outer
// `config` value into raw bytes). ServerName carries SNI when set.
type argoTLSClientConfig struct {
	Insecure   bool   `json:"insecure"`
	ServerName string `json:"serverName"`
	CAData     string `json:"caData"`
	CertData   string `json:"certData"`
	KeyData    string `json:"keyData"`
}

// argoExecEnvVar mirrors one entry of Argo's execProviderConfig.env map. Argo
// serialises env as a JSON object (name -> value); client-go's ExecConfig wants
// an ordered []ExecEnvVar, so the parser converts.
//
// argoExecProviderConfig is the nested exec credential plugin block. It maps onto
// clientcmdapi.ExecConfig. InteractiveMode is forced to Never at map time:
// clientcmd rejects an ExecConfig with an unset InteractiveMode
// ("interactiveMode must be specified"), so an Argo Secret naming an exec plugin
// would otherwise fail RESTConfig() with a validation error rather than connect.
type argoExecProviderConfig struct {
	APIVersion  string            `json:"apiVersion"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	InstallHint string            `json:"installHint"`
}

// argoAWSAuthConfig is Argo's awsAuthConfig block (the legacy EKS IAM path). Its
// only role here is DETECTION: readout does not ship the aws binary, so an Argo
// Secret authenticating via awsAuthConfig cannot be honored and must be surfaced
// as a skip-with-error rather than silently producing a credential-less (anonymous)
// connection. Operators should configure execProviderConfig instead.
type argoAWSAuthConfig struct {
	ClusterName string `json:"clusterName"`
	RoleARN     string `json:"roleARN"`
	Profile     string `json:"profile"`
}

// argoClusterConfig is the parsed form of an Argo cluster Secret's `config` JSON
// blob: the credential + TLS material that does NOT fit the flat top-level
// name/server. This is the nested shape D6 pins (NOT a flat {server,CA,creds}).
type argoClusterConfig struct {
	BearerToken        string                  `json:"bearerToken"`
	TLSClientConfig    argoTLSClientConfig     `json:"tlsClientConfig"`
	ExecProviderConfig *argoExecProviderConfig `json:"execProviderConfig"`
	AWSAuthConfig      *argoAWSAuthConfig      `json:"awsAuthConfig"`
	Username           string                  `json:"username"`
	Password           string                  `json:"password"`
}

// parseArgoClusterSecret is the source-agnostic Secret->connection core (D6),
// specialised to the Argo CD cluster-Secret schema. Input is a Secret's Data map
// (raw bytes -- the Secret API has already base64-decoded stringData into it), so
// data["name"]/data["server"] are plain strings and data["config"] is a JSON
// blob. It reads the flat top-level name/server, unmarshals the nested config,
// base64-decodes the inner caData/certData/keyData, and maps everything onto the
// canonical Connection triple (Cluster TLS + AuthInfo credentials), Source =
// SourceSecret. No rest.Config field is set by hand: the produced Connection
// defers entirely to clientcmd via RESTConfig (D1).
//
// Errors (skip-with-error fuel for the caller, D3): a missing/empty server, an
// unparseable config blob, or undecodable base64 in the TLS material.
func parseArgoClusterSecret(data map[string][]byte) (*Connection, error) {
	name := string(data["name"])
	server := string(data["server"])
	if server == "" {
		return nil, fmt.Errorf("argo cluster secret %q: empty server", name)
	}
	// name is the ONLY discovery path where the cluster identifier comes from
	// untrusted Secret content (static enforces non-empty; kubeconfig uses a map
	// key). Guard it symmetrically with server: an empty name would key the cluster
	// as "" and collide with any sibling missing a name, hiding a real cluster.
	if name == "" {
		return nil, fmt.Errorf("argo cluster secret (server %q): empty name", server)
	}

	var conf argoClusterConfig
	rawConfig := data["config"]
	if len(rawConfig) == 0 {
		return nil, fmt.Errorf("argo cluster secret %q: missing config blob", name)
	}
	if err := json.Unmarshal(rawConfig, &conf); err != nil {
		return nil, fmt.Errorf("argo cluster secret %q: parse config: %w", name, err)
	}
	// awsAuthConfig is the legacy EKS IAM path; readout does not ship the aws
	// binary (D1 boundary / Accepted Risk), so honoring it would mean building a
	// credential-less connection that silently runs anonymous -- exactly the class
	// D8 exists to kill. When awsAuthConfig is the only credential, skip-with-error.
	if conf.AWSAuthConfig != nil && conf.BearerToken == "" && conf.ExecProviderConfig == nil &&
		conf.TLSClientConfig.CertData == "" {
		return nil, fmt.Errorf("argo cluster secret %q: awsAuthConfig is not supported "+
			"(readout does not ship the aws binary); configure execProviderConfig instead", name)
	}

	caData, err := decodeArgoB64("caData", name, conf.TLSClientConfig.CAData)
	if err != nil {
		return nil, err
	}
	certData, err := decodeArgoB64("certData", name, conf.TLSClientConfig.CertData)
	if err != nil {
		return nil, err
	}
	keyData, err := decodeArgoB64("keyData", name, conf.TLSClientConfig.KeyData)
	if err != nil {
		return nil, err
	}

	cluster := &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: caData,
		InsecureSkipTLSVerify:    conf.TLSClientConfig.Insecure,
		TLSServerName:            conf.TLSClientConfig.ServerName,
	}

	auth := &clientcmdapi.AuthInfo{
		Token:                 conf.BearerToken,
		ClientCertificateData: certData,
		ClientKeyData:         keyData,
		Username:              conf.Username,
		Password:              conf.Password,
	}
	if conf.ExecProviderConfig != nil {
		auth.Exec = argoExecToClientcmd(conf.ExecProviderConfig)
	}
	if isZeroAuthInfo(auth) && auth.Username == "" && auth.Password == "" && auth.Exec == nil {
		auth = nil
	}

	return &Connection{
		Name:     name,
		Source:   SourceSecret,
		Cluster:  cluster,
		AuthInfo: auth,
	}, nil
}

// decodeArgoB64 base64-decodes one TLS field from inside the Argo config JSON.
// An empty value is legitimately absent (returns nil, no error); a non-empty but
// undecodable value is a typed parse error so the Secret is skipped-with-error.
func decodeArgoB64(field, secretName, value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("argo cluster secret %q: decode %s: %w", secretName, field, err)
	}
	return decoded, nil
}

// argoExecToClientcmd converts Argo's execProviderConfig onto client-go's
// ExecConfig. Argo's env is an unordered JSON object; client-go wants an ordered
// slice, so the keys are sorted for a deterministic ExecConfig. InteractiveMode
// is forced to Never (see argoExecProviderConfig doc): clientcmd validation
// rejects an unset mode, so leaving it empty would break RESTConfig().
func argoExecToClientcmd(e *argoExecProviderConfig) *clientcmdapi.ExecConfig {
	exec := &clientcmdapi.ExecConfig{
		APIVersion:      e.APIVersion,
		Command:         e.Command,
		Args:            e.Args,
		InstallHint:     e.InstallHint,
		InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
	}
	if len(e.Env) > 0 {
		keys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		exec.Env = make([]clientcmdapi.ExecEnvVar, 0, len(keys))
		for _, k := range keys {
			exec.Env = append(exec.Env, clientcmdapi.ExecEnvVar{Name: k, Value: e.Env[k]})
		}
	}
	return exec
}

// discoverArgoSecrets is the Argo CD consumer of the Secret->connection primitive
// (D6). It lists cluster Secrets in `namespace` of the HOST cluster (the cluster
// reachable through `client`) and turns each into a discoveredCluster.
//
// Error model (D3):
//   - A failed LIST -- the host is down or the host SA is RBAC-forbidden to read
//     Secrets -- is a SOURCE-level failure: it returns (nil, error). The caller
//     surfaces it, but it does not blank the other sources.
//   - A single Secret that does not parse is skipped-with-error: a discoveredCluster
//     with Err set (Config nil), non-fatal to its sibling Secrets.
//
// `client` is a kubernetes.Interface parameter so tests inject a fake clientset
// (the LIST + parse is what this exercises; the connection transport itself is
// already proven by Unit 1's RESTConfig tests).
func discoverArgoSecrets(ctx context.Context, client kubernetes.Interface, namespace string) ([]discoveredCluster, error) {
	list, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: argoClusterSecretLabelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list argo cluster secrets in %q: %w", namespace, err)
	}

	var result []discoveredCluster
	for i := range list.Items {
		secret := &list.Items[i]
		conn, perr := parseArgoClusterSecret(secret.Data)
		if perr != nil {
			result = append(result, discoveredCluster{
				Name:   secret.Name,
				Source: SourceSecret,
				Err:    &ContextLoadError{Name: secret.Name, Source: SourceSecret, Err: perr},
			})
			continue
		}
		restCfg, rerr := conn.RESTConfig()
		if rerr != nil {
			result = append(result, discoveredCluster{
				Name:   secret.Name,
				Source: SourceSecret,
				Err:    &ContextLoadError{Name: secret.Name, Source: SourceSecret, Err: rerr},
			})
			continue
		}
		result = append(result, discoveredCluster{
			Name:   conn.Name,
			Config: restCfg,
			Source: SourceSecret,
			Labels: map[string]string{},
			Spec:   map[string]any{"argo_secret": secret.Name},
		})
	}
	return result, nil
}
