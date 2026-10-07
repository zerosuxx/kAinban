package kube

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestChooseSource(t *testing.T) {
	cases := []struct {
		flag, env, ctx string
		inCluster      bool
		want           Source
	}{
		{"", "", "", true, SourceInCluster},
		{"", "", "", false, SourceKubeconfig},
		{"/k", "", "", true, SourceKubeconfig},
		{"", "/env", "", true, SourceKubeconfig},
		{"", "", "prod", true, SourceKubeconfig},
	}
	for _, c := range cases {
		if got := chooseSource(c.flag, c.env, c.ctx, c.inCluster); got != c.want {
			t.Errorf("chooseSource(%q,%q,%q,%v) = %v, want %v", c.flag, c.env, c.ctx, c.inCluster, got, c.want)
		}
	}
}

func TestPickNamespace(t *testing.T) {
	cases := []struct {
		flag, pod, sa, ctx string
		src                Source
		want               string
	}{
		{"f", "p", "s", "c", SourceInCluster, "f"},
		{"", "p", "s", "c", SourceInCluster, "p"},
		{"", "", "s\n", "c", SourceInCluster, "s"},
		{"", "", "s", "c", SourceKubeconfig, "c"}, // SA file ignored remotely
		{"", "", "", "c", SourceInCluster, "c"},
		{"", "", "", "", SourceKubeconfig, "default"},
	}
	for _, c := range cases {
		if got := pickNamespace(c.flag, c.pod, c.sa, c.ctx, c.src); got != c.want {
			t.Errorf("pickNamespace(%+v) = %q, want %q", c, got, c.want)
		}
	}
}

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- {name: a, cluster: {server: "https://a.example:6443"}}
- {name: b, cluster: {server: "https://b.example:6443"}}
users: [{name: u, user: {token: x}}]
contexts:
- {name: ctx-a, context: {cluster: a, user: u, namespace: ns-a}}
- {name: ctx-b, context: {cluster: b, user: u, namespace: ns-b}}
current-context: ctx-a
`

func setupResolve(t *testing.T, inCluster bool) string {
	t.Helper()
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	if err := os.WriteFile(kc, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	sa := filepath.Join(dir, "namespace")
	if err := os.WriteFile(sa, []byte("ns-sa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldSA, oldIC, oldLR := serviceAccountNamespaceFile, inClusterConfig, loadingRules
	t.Cleanup(func() { serviceAccountNamespaceFile, inClusterConfig, loadingRules = oldSA, oldIC, oldLR })
	loadingRules = func() *clientcmd.ClientConfigLoadingRules {
		r := clientcmd.NewDefaultClientConfigLoadingRules()
		if os.Getenv("KUBECONFIG") == "" { // hide the real ~/.kube/config
			r.Precedence = []string{filepath.Join(dir, "no-such-config")}
		}
		return r
	}
	serviceAccountNamespaceFile = sa
	inClusterConfig = func() (*rest.Config, error) {
		if inCluster {
			return &rest.Config{Host: "https://10.0.0.1:443"}, nil
		}
		return nil, rest.ErrNotInCluster
	}
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("KUBECONFIG", "")
	return kc
}

func TestResolveInCluster(t *testing.T) {
	setupResolve(t, true)
	tg, err := resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tg.Source != SourceInCluster || tg.Namespace != "ns-sa" || tg.Server != "https://10.0.0.1:443" {
		t.Fatalf("got %+v", tg)
	}
}

func TestResolveKubeconfigEnvBeatsInCluster(t *testing.T) {
	kc := setupResolve(t, true)
	t.Setenv("KUBECONFIG", kc)
	tg, err := resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tg.Source != SourceKubeconfig || tg.Context != "ctx-a" || tg.Namespace != "ns-a" || tg.Server != "https://a.example:6443" {
		t.Fatalf("got %+v", tg)
	}
}

func TestResolveExplicitContext(t *testing.T) {
	kc := setupResolve(t, false)
	tg, err := resolve(Options{Kubeconfig: kc, Context: "ctx-b"})
	if err != nil {
		t.Fatal(err)
	}
	if tg.Context != "ctx-b" || tg.Namespace != "ns-b" || tg.Server != "https://b.example:6443" {
		t.Fatalf("got %+v", tg)
	}
	if got := tg.Describe(); got != "context ctx-b (https://b.example:6443), namespace ns-b" {
		t.Errorf("Describe = %q", got)
	}
	tg, _ = resolve(Options{Kubeconfig: kc, Context: "ctx-b", Namespace: "flag"})
	if tg.Namespace != "flag" {
		t.Errorf("namespace flag ignored: %q", tg.Namespace)
	}
}

func TestResolveNothing(t *testing.T) {
	setupResolve(t, false)
	if _, err := Connect(Options{}); err == nil {
		t.Fatal("expected error with no config")
	}
	if got := ResolveNamespace(""); got != "default" {
		t.Errorf("ResolveNamespace = %q", got)
	}
	t.Setenv("POD_NAMESPACE", "pod")
	if got := ResolveNamespace(""); got != "pod" {
		t.Errorf("ResolveNamespace = %q", got)
	}
}
