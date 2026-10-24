// Package kube reads TopoLVM state from a cluster. It only ever gets and lists.
package kube

import (
	"context"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/EsDmitrii/kubectl-topolvm/internal/version"
)

// Clients bundles the API clients the plugin uses.
type Clients struct {
	Core    kubernetes.Interface
	Dynamic dynamic.Interface
	Meta    metadata.Interface
	// Raw GETs an API server path; --metrics and --usage read pod and node proxies through it.
	Raw func(ctx context.Context, path string) ([]byte, error)
}

// RESTConfig resolves the kubeconfig the way kubectl does, honouring the override flags.
func RESTConfig(rules *clientcmd.ClientConfigLoadingRules, o *clientcmd.ConfigOverrides) (*rest.Config, error) {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, o).ClientConfig()
}

// NewClients builds the typed, dynamic and metadata clients from one config.
func NewClients(cfg *rest.Config) (*Clients, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.UserAgent = "kubectl-topolvm/" + version.Version
	// Per-node fan-out (--usage, --metrics) would otherwise queue behind the default 5 QPS limiter.
	cfg.QPS, cfg.Burst = 50, 100
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	// Result.Error decodes an error Status into its message; DoRaw and Raw report it as "unknown".
	raw := func(ctx context.Context, path string) ([]byte, error) {
		res := core.CoreV1().RESTClient().Get().AbsPath(path).Do(ctx)
		if err := res.Error(); err != nil {
			return nil, err
		}
		return res.Raw()
	}
	return &Clients{Core: core, Dynamic: dyn, Meta: meta, Raw: raw}, nil
}
