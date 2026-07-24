package controller

// Envtest suite. Requires kube-apiserver/etcd test binaries:
//
//	make envtest
//	bin/setup-envtest use -p path <version>   # e.g. 1.34.x
//	KUBEBUILDER_ASSETS=<printed path> go test ./internal/operator/... -v
//
// If KUBEBUILDER_ASSETS is unset and no assets are found, the suite is
// skipped so plain `go test ./...` stays green on machines without envtest.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
)

const testAgentImage = "example.test/scion-node-agent:test"

var (
	k8sClient client.Client
	ctx       context.Context
	suiteSkip string // non-empty: tests must t.Skip with this reason
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// envtest's built-in default location; if empty too, skip the suite.
		if _, err := os.Stat("/usr/local/kubebuilder/bin/kube-apiserver"); err != nil {
			suiteSkip = "KUBEBUILDER_ASSETS unset and no envtest assets found; run `make envtest && bin/setup-envtest use -p path`"
			os.Exit(m.Run())
		}
	}

	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(context.Background())

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest start:", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "manager:", err)
		os.Exit(1)
	}
	r := &ScionNetworkReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		AgentImage: testAgentImage,
		SCCAvail:   false,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "manager start:", err)
		}
	}()

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

func skipIfNoEnvtest(t *testing.T) {
	t.Helper()
	if suiteSkip != "" {
		t.Skip(suiteSkip)
	}
}
