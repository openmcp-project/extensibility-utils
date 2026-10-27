package discovery

import (
	"context"
	"fmt"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ocmv1alpha1 "ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Resolver interface {
	Apply(ctx context.Context) (bool, error)
}

type resolver struct {
	cluster         *clusters.Cluster
	serviceProvider string
	namespace       string
	providerconfig  ProviderConfig
}

type ProviderConfig interface {
	client.Object
	DiscoveryConfigs() []DiscoveryConfig
}

type Config struct {
	ServiceProvider string
	Namespace       string
	ProviderConfig  ProviderConfig
}

type DiscoveryConfig struct {
	Name              string
	Releasechannel    string
	ComponentSelector *ocmv1alpha1.Selector
	ResourceSelector  *ocmv1alpha1.Selector
}

func NewResolver(cfg Config, c *clusters.Cluster) Resolver {
	return &resolver{
		cluster:         c,
		serviceProvider: cfg.ServiceProvider,
		namespace:       cfg.Namespace,
		providerconfig:  cfg.ProviderConfig,
	}
}

func (r *resolver) Apply(ctx context.Context) (bool, error) {
	platformCluster := objectmanager.NewCluster(r.cluster, r.namespace, objectmanager.PlatformCluster)
	manageDiscoveries(r.providerconfig.DiscoveryConfigs(), r.namespace, platformCluster)
	mgr := objectmanager.NewManager(r.serviceProvider)
	mgr.AddCluster(platformCluster)
	rr, err := mgr.Apply(ctx)
	if err != nil {
		return false, fmt.Errorf("applying the discoveries failed: %w", err)
	}
	return rr.Requeue, nil
}

func manageDiscoveries(discoveryConfigs []DiscoveryConfig, namespace string, cluster objectmanager.Cluster) {
	for _, discConfig := range discoveryConfigs {
		discovery := objectmanager.NewObject(
			&ocmv1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discConfig.Name,
					Namespace: namespace,
				},
				Spec: ocmv1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: discConfig.Releasechannel},
				},
			},
			objectmanager.ObjectConfig{
				ReconcileFunc: func(_ context.Context, o client.Object) error {
					discovery, ok := o.(*ocmv1alpha1.Discovery)
					if !ok {
						return fmt.Errorf("expected *ocmv1alpha1.Discovery, got %T", o)
					}
					discovery.Spec = ocmv1alpha1.DiscoverySpec{
						ComponentRef:      corev1.LocalObjectReference{Name: discConfig.Releasechannel},
						ComponentSelector: discConfig.ComponentSelector,
						ResourceSelector:  discConfig.ResourceSelector,
					}
					return nil
				},
				DeletionPolicy: objectmanager.Delete,
				StatusFunc:     discoveryStatus,
			},
		)
		cluster.AddObject(discovery)
	}
}

func discoveryStatus(object client.Object) objectmanager.ManagedObjectStatus {
	discovery, ok := object.(*ocmv1alpha1.Discovery)
	if !ok {
		return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseProgressing, Message: "Unexpected object type."}
	}
	if !discovery.GetDeletionTimestamp().IsZero() {
		return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseTerminating, Message: "Resource is terminating."}
	}
	if discovery.GetUID() == "" {
		return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseProgressing, Message: "Resource has not been created yet."}
	}
	if discovery.GetGeneration() != discovery.Status.ObservedGeneration {
		return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseProgressing, Message: "Resource has not reconciled yet."}
	}
	return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseReady, Message: "Resource exists."}
}
