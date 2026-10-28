package discovery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	ocmv1alpha1 "ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	oci "ocm.software/open-component-model/bindings/go/oci/spec/access"
	ociv1 "ocm.software/open-component-model/bindings/go/oci/spec/access/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Resolver interface {
	Apply(ctx context.Context) (bool, error)
	ResolveByName(ctx context.Context, discId, version, componentName string, resourceName string) (Resource, error)
	ResolveByType(ctx context.Context, discId, version, componentName string, resourceType string) (Resource, error)
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
	ID                string
	Releasechannel    string
	ComponentSelector *ocmv1alpha1.Selector
	ResourceSelector  *ocmv1alpha1.Selector
}

type Component struct {
	Name       string
	Version    string
	PullSecret string
	Resources  []v2.Resource
}

type Resource struct {
	Name       string
	Type       string
	AccessURL  string
	PullSecret string
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
	manageDiscoveries(r.providerconfig.DiscoveryConfigs(), r.namespace, platformCluster, r.serviceProvider)
	mgr := objectmanager.NewManager(r.serviceProvider)
	mgr.AddCluster(platformCluster)
	rr, err := mgr.Apply(ctx)
	if err != nil {
		return false, fmt.Errorf("applying discovery objects failed: %w", err)
	}
	return rr.Requeue, nil
}

func manageDiscoveries(discoveryConfigs []DiscoveryConfig, namespace string, cluster objectmanager.Cluster, serviceProvider string) {
	for _, discConfig := range discoveryConfigs {
		discovery := objectmanager.NewObject(
			&ocmv1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      prefixDiscoveryName(discConfig.ID, serviceProvider),
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
		return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseProgressing, Message: "Resource has not been reconciled yet."}
	}
	return objectmanager.ManagedObjectStatus{Phase: objectmanager.StatusPhaseReady, Message: "Resource exists."}
}

func (r *resolver) ResolveByName(ctx context.Context, discId, version, componentName string, resourceName string) (Resource, error) {
	discovery, err := r.fetchDiscovery(ctx, discId)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to fetch discovery %s: %w", discId, err)
	}
	components, err := parseDiscovery(discovery)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to parse discovery %s: %w", discId, err)
	}
	for _, component := range components {
		if component.Name == componentName && component.Version == version {
			for _, resource := range component.Resources {
				if resource.Name == resourceName {
					access, err := decodeOCIAccess(resource)
					if err != nil {
						return Resource{}, fmt.Errorf("could not decode OCI access of resource %s: %w", resource.Name, err)
					}
					return Resource{
						Name:      resource.Name,
						Type:      resource.Type,
						AccessURL: access.ImageReference,
					}, nil
				}
			}
			return Resource{}, fmt.Errorf("resource %s not found", resourceName)
		}
	}
	return Resource{}, fmt.Errorf("component %s not found", componentName)
}

func (r *resolver) ResolveByType(ctx context.Context, discId, version, componentName string, resourceType string) (Resource, error) {
	return Resource{}, nil
}

func (r *resolver) fetchDiscovery(ctx context.Context, id string) (ocmv1alpha1.Discovery, error) {
	discovery := ocmv1alpha1.Discovery{}
	cluster := r.cluster.Cluster()
	reader := cluster.GetClient()
	err := reader.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: prefixDiscoveryName(id, r.serviceProvider)}, &discovery)
	if apierrors.IsNotFound(err) {
		return ocmv1alpha1.Discovery{}, fmt.Errorf("discovery %s was not found", id)
	}
	if err != nil {
		return ocmv1alpha1.Discovery{}, fmt.Errorf("error fetching discovery %s: %w", id, err)
	}
	if discovery.Generation != discovery.Status.ObservedGeneration {
		return ocmv1alpha1.Discovery{}, fmt.Errorf("discovery %s has not been observed for its current spec", id)
	}
	return discovery, nil
}

func parseDiscovery(discovery ocmv1alpha1.Discovery) ([]Component, error) {
	components := make([]Component, 0, len(discovery.Status.Components))
	for _, raw := range discovery.Status.Components {
		var desc v2.Descriptor
		if err := json.Unmarshal(raw.Raw, &desc); err != nil {
			return nil, fmt.Errorf("discovery %q: parse component descriptor: %w", discovery.Name, err)
		}
		component := Component{
			Name:      desc.Component.Name,
			Version:   desc.Component.Version,
			Resources: desc.Component.Resources,
		}
		components = append(components, component)
	}
	return components, nil
}

func decodeOCIAccess(resource v2.Resource) (ociv1.OCIImage, error) {
	var img ociv1.OCIImage
	err := oci.Scheme.Convert(resource.Access, &img)
	if err != nil {
		return ociv1.OCIImage{}, err
	}
	return img, nil
}

func prefixDiscoveryName(discoveryName string, providerName string) string {
	return fmt.Sprintf("sp-%s-%s", providerName, discoveryName)
}
