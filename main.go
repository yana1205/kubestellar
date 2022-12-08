package main

import (
	"context"
	"os"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	kcpinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	"github.com/kcp-dev/logicalcluster/v2"
	kcpkubernetesinformers "k8s.io/client-go/informers"
	kcpkubernetesclientset "k8s.io/client-go/kubernetes"

	edgeindexers "github.com/kcp-dev/edge-mc/pkg/indexers"
	edgeplacement "github.com/kcp-dev/edge-mc/pkg/reconciler/scheduling/placement"
)

func main() {
	const resyncPeriod = 10 * time.Hour

	ctx := context.Background()
	logger := klog.FromContext(ctx)

	// create cfg
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{
		Context: clientcmdapi.Context{
			Cluster:  "base",
			AuthInfo: "shard-admin",
		},
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		logger.Error(err, "failed to make config, if running out of cluster, make sure $KUBECONFIG points to kcp server")
		os.Exit(1)
	}

	// create kubeSharedInformerFactory
	kubernetesConfig := rest.CopyConfig(cfg)
	kubeClusterClient, err := kcpkubernetesclientset.NewClusterForConfig(kubernetesConfig)
	if err != nil {
		logger.Error(err, "failed to create kube cluter client")
		os.Exit(1)
	}
	kubeSharedInformerFactory := kcpkubernetesinformers.NewSharedInformerFactoryWithOptions(
		kubeClusterClient.Cluster(logicalcluster.Wildcard),
		resyncPeriod,
		kcpkubernetesinformers.WithExtraClusterScopedIndexers(edgeindexers.ClusterScoped()),
		kcpkubernetesinformers.WithExtraNamespaceScopedIndexers(edgeindexers.NamespaceScoped()),
	)

	// create kcpSharedInformerFactory
	kcpConfig := rest.CopyConfig(cfg)
	kcpClusterClient, err := kcpclient.NewClusterForConfig(kcpConfig)
	if err != nil {
		logger.Error(err, "failed to create kcp cluster client")
		os.Exit(1)
	}
	kcpSharedInformerFactory := kcpinformers.NewSharedInformerFactoryWithOptions(
		kcpClusterClient.Cluster(logicalcluster.Wildcard),
		resyncPeriod,
		kcpinformers.WithExtraClusterScopedIndexers(edgeindexers.ClusterScoped()),
		kcpinformers.WithExtraNamespaceScopedIndexers(edgeindexers.NamespaceScoped()),
	)

	// create the kcp-scheduling-placement-controller
	controllerConfig := rest.CopyConfig(cfg)
	kcpClientset, err := kcpclient.NewForConfig(controllerConfig)
	if err != nil {
		logger.Error(err, "failed to create kcp clientset for controller")
		os.Exit(1)
	}
	c, err := edgeplacement.NewController(
		kcpClientset,
		kubeSharedInformerFactory.Core().V1().Namespaces(),
		kcpSharedInformerFactory.Scheduling().V1alpha1().Locations(),
		kcpSharedInformerFactory.Scheduling().V1alpha1().Placements(),
	)
	if err != nil {
		logger.Error(err, "Failed to create controller")
		os.Exit(1)
	}

	// run edge placement controller
	kubeSharedInformerFactory.Start(ctx.Done())
	kcpSharedInformerFactory.Start(ctx.Done())
	kubeSharedInformerFactory.WaitForCacheSync(ctx.Done())
	kcpSharedInformerFactory.WaitForCacheSync(ctx.Done())
	c.Start(ctx, 1)
}
