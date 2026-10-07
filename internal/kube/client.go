package kube

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// serviceAccountNamespaceFile is where the in-cluster namespace is mounted.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// inClusterConfig and loadingRules are replaceable in tests.
var (
	inClusterConfig = rest.InClusterConfig
	loadingRules    = clientcmd.NewDefaultClientConfigLoadingRules
)

// Source says where the cluster connection settings came from.
type Source int

const (
	// SourceKubeconfig: clientcmd loading rules (explicit path, KUBECONFIG or
	// ~/.kube/config), honouring the selected context.
	SourceKubeconfig Source = iota
	// SourceInCluster: the pod's service account (rest.InClusterConfig).
	SourceInCluster
)

func (s Source) String() string {
	if s == SourceInCluster {
		return "in-cluster"
	}
	return "kubeconfig"
}

// Options select the cluster and namespace. Zero values mean "auto".
type Options struct {
	Kubeconfig string // explicit kubeconfig path (--kubeconfig)
	Context    string // kubeconfig context (--context)
	Namespace  string // --namespace
}

// Target is a resolved cluster connection.
type Target struct {
	Client    kubernetes.Interface
	Config    *rest.Config
	Source    Source
	Context   string // kubeconfig context; empty in-cluster
	Server    string // API server URL
	Namespace string
}

// Describe returns a one-line summary such as
// "context prod (https://1.2.3.4:6443), namespace kainban".
func (t *Target) Describe() string {
	where := "in-cluster"
	if t.Source == SourceKubeconfig {
		where = "context " + t.Context
		if t.Context == "" {
			where = "kubeconfig"
		}
	}
	if t.Server != "" {
		where += " (" + t.Server + ")"
	}
	return where + ", namespace " + t.Namespace
}

// chooseSource decides where the connection settings come from:
//  1. an explicit kubeconfig, a KUBECONFIG env var or an explicit context
//     selects the kubeconfig;
//  2. otherwise the in-cluster config when available;
//  3. otherwise the default kubeconfig (~/.kube/config).
func chooseSource(kubeconfigFlag, kubeconfigEnv, contextFlag string, inClusterAvailable bool) Source {
	if kubeconfigFlag != "" || kubeconfigEnv != "" || contextFlag != "" {
		return SourceKubeconfig
	}
	if inClusterAvailable {
		return SourceInCluster
	}
	return SourceKubeconfig
}

// pickNamespace applies the namespace precedence: flag > POD_NAMESPACE >
// service account namespace (in-cluster only) > kubeconfig context
// namespace > "default".
func pickNamespace(flag, podNamespace, saNamespace, contextNamespace string, src Source) string {
	for _, ns := range []string{flag, podNamespace} {
		if ns = strings.TrimSpace(ns); ns != "" {
			return ns
		}
	}
	if src == SourceInCluster {
		if ns := strings.TrimSpace(saNamespace); ns != "" {
			return ns
		}
	}
	if ns := strings.TrimSpace(contextNamespace); ns != "" {
		return ns
	}
	return "default"
}

func clientConfig(o Options) clientcmd.ClientConfig {
	rules := loadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: o.Context})
}

// resolve builds the rest.Config and namespace without creating a client.
func resolve(o Options) (*Target, error) {
	ic, icErr := inClusterConfig()
	src := chooseSource(o.Kubeconfig, os.Getenv("KUBECONFIG"), o.Context, icErr == nil)

	t := &Target{Source: src}
	var ctxNS, saNS string
	if src == SourceInCluster {
		t.Config = ic
		if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
			saNS = string(b)
		}
	} else {
		cc := clientConfig(o)
		raw, err := cc.RawConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
		t.Context = raw.CurrentContext
		if o.Context != "" {
			t.Context = o.Context
		}
		if ns, _, err := cc.Namespace(); err == nil {
			ctxNS = ns
		}
		cfg, err := cc.ClientConfig()
		if err != nil {
			if clientcmd.IsEmptyConfig(err) {
				err = errors.New("no kubeconfig found and not running in a cluster (set --kubeconfig or KUBECONFIG)")
			}
			// Keep the namespace usable even without a working config.
			t.Namespace = pickNamespace(o.Namespace, os.Getenv("POD_NAMESPACE"), "", ctxNS, src)
			return t, fmt.Errorf("load kubeconfig: %w", err)
		}
		t.Config = cfg
	}
	t.Server = t.Config.Host
	t.Namespace = pickNamespace(o.Namespace, os.Getenv("POD_NAMESPACE"), saNS, ctxNS, src)
	return t, nil
}

// Connect resolves the cluster connection and namespace and creates a
// clientset.
func Connect(o Options) (*Target, error) {
	t, err := resolve(o)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(t.Config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	t.Client = cs
	return t, nil
}

// NewClient returns a clientset: kubeconfig when a path is given or
// KUBECONFIG is set, else in-cluster, else ~/.kube/config.
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	t, err := Connect(Options{Kubeconfig: kubeconfig})
	if err != nil {
		return nil, err
	}
	return t.Client, nil
}

// ResolveNamespace returns flag if set, else POD_NAMESPACE, else the service
// account namespace (in-cluster only), else the current kubeconfig context's
// namespace, else "default".
func ResolveNamespace(flag string) string {
	t, _ := resolve(Options{Namespace: flag})
	if t == nil || t.Namespace == "" {
		return pickNamespace(flag, os.Getenv("POD_NAMESPACE"), "", "", SourceKubeconfig)
	}
	return t.Namespace
}
