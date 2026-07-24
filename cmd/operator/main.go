// Command operator runs the SCION operator: it reconciles the ScionNetwork
// singleton into the node-agent DaemonSet and supporting objects.
package main

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/controller"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	log := ctrl.Log.WithName("setup")

	agentImage := os.Getenv("AGENT_IMAGE")
	if agentImage == "" {
		log.Error(nil, "AGENT_IMAGE environment variable is required")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "add client-go scheme")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "add v1alpha1 scheme")
		os.Exit(1)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "get kubeconfig")
		os.Exit(1)
	}

	// SCC availability: OpenShift exposes security.openshift.io/v1; on
	// vanilla Kubernetes the group is absent and the SCC is not applied.
	// Note: a transient discovery failure here also yields sccAvail=false,
	// meaning the SCC is skipped until the operator restarts.
	sccAvail := false
	if dc, err := discovery.NewDiscoveryClientForConfig(cfg); err == nil {
		if _, err := dc.ServerResourcesForGroupVersion("security.openshift.io/v1"); err == nil {
			sccAvail = true
		}
	} else {
		log.Error(err, "discovery client; assuming no SCC API")
	}
	log.Info("SCC API discovery", "available", sccAvail)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: ":8080"},
		HealthProbeBindAddress: ":8081",
		LeaderElection:         true,
		LeaderElectionID:       "scion-operator-leader",
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "add healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "add readyz")
		os.Exit(1)
	}

	r := &controller.ScionNetworkReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		AgentImage: agentImage,
		SCCAvail:   sccAvail,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "setup ScionNetwork controller")
		os.Exit(1)
	}

	log.Info("starting operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
