package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	ocmv1alpha1 "ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	ociaccess "ocm.software/open-component-model/bindings/go/oci/spec/access"
	ociaccessv1 "ocm.software/open-component-model/bindings/go/oci/spec/access/v1"
	ocirepov1 "ocm.software/open-component-model/bindings/go/oci/spec/repository/v1/oci"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Resolver interface {
	Apply(ctx context.Context) (bool, error)
	Resolve(ctx context.Context, discId, version string, componentMatchFunc ComponentMatchFunc, matchFunc ResourceMatchFunc) (Resource, error)
	Credentials(ctx context.Context, discId string) (*corev1.LocalObjectReference, error)
	ListVersions(ctx context.Context, discId string, matchFunc ComponentMatchFunc) ([]string, error)
}

type resolver struct {
	cluster          *clusters.Cluster
	serviceProvider  string
	namespace        string
	discoveryconfigs []DiscoveryConfig
}

type Config struct {
	ServiceProvider  string
	Namespace        string
	DiscoveryConfigs []DiscoveryConfig
}

type DiscoveryConfig struct {
	ID                string
	Releasechannel    string
	ComponentSelector *ocmv1alpha1.Selector
	ResourceSelector  *ocmv1alpha1.Selector
}

type Component struct {
	Name      string
	Version   string
	Resources []v2.Resource
	Labels    []v2.Label
}

type Resource struct {
	Name      string
	Type      string
	AccessURL string
}

func NewResolver(cfg Config, c *clusters.Cluster) Resolver {
	return &resolver{
		cluster:          c,
		serviceProvider:  cfg.ServiceProvider,
		namespace:        cfg.Namespace,
		discoveryconfigs: cfg.DiscoveryConfigs,
	}
}

func (r *resolver) Apply(ctx context.Context) (bool, error) {
	platformCluster := objectmanager.NewCluster(r.cluster, r.namespace, objectmanager.PlatformCluster)
	manageDiscoveries(r.discoveryconfigs, r.namespace, platformCluster, r.serviceProvider)
	mgr := objectmanager.NewManager(r.serviceProvider)
	mgr.AddCluster(platformCluster)
	rr, err := mgr.Apply(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to apply discovery objects: %w", err)
	}
	return rr.Requeue, nil
}

func manageDiscoveries(discoveryConfigs []DiscoveryConfig, namespace string, cluster objectmanager.Cluster, serviceProvider string) {
	for _, discConfig := range discoveryConfigs {
		discovery := objectmanager.NewObject(
			&ocmv1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryObjectName(discConfig.ID, serviceProvider),
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

func (r *resolver) Credentials(ctx context.Context, discId string) (*corev1.LocalObjectReference, error) {
	disc, err := r.fetchDiscovery(ctx, discId)
	reader := r.cluster.Cluster().GetClient()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch discovery %s: %w", discId, err)
	}
	configs := disc.GetEffectiveOCMConfig()
	for _, config := range configs {
		if config.Kind != "Secret" {
			continue
		}
		secret := v1.Secret{}
		reader.Get(ctx, client.ObjectKey{Name: config.Name, Namespace: config.Namespace}, &secret)
		if secret.Type != corev1.SecretTypeDockerConfigJson {
			continue
		}
		return &corev1.LocalObjectReference{Name: config.Name}, nil
	}
	return nil, nil
}

func (r *resolver) ListVersions(ctx context.Context, discId string, matchFunc ComponentMatchFunc) ([]string, error) {
	discovery, err := r.fetchDiscovery(ctx, discId)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch discovery %s: %w", discId, err)
	}
	components, err := parseDiscovery(discovery)
	if err != nil {
		return nil, fmt.Errorf("failed to parse discovery %s: %w", discId, err)
	}
	var availableVersions []string
	for _, component := range components {
		if matchFunc == nil || matchFunc(component) {
			availableVersions = append(availableVersions, component.Version)
		}
	}
	slices.Sort(availableVersions)
	slices.Reverse(availableVersions)
	return availableVersions, nil
}

type ResourceMatchFunc func(v2.Resource) bool

func ResourceByName(resourceName string) func(v2.Resource) bool {
	return func(r v2.Resource) bool { return r.Name == resourceName }
}

func ResourceByType(resourceType string) func(v2.Resource) bool {
	return func(r v2.Resource) bool { return r.Type == string(resourceType) }
}
func ResourceByLabel(key string, value json.RawMessage) func(v2.Resource) bool {
	return func(r v2.Resource) bool {
		for _, l := range r.Labels {
			if l.Name == key && bytes.Equal(l.Value, value) {
				return true
			}
		}
		return false
	}
}

type ComponentMatchFunc func(Component) bool

func ComponentByName(componentName string) func(Component) bool {
	return func(c Component) bool { return c.Name == componentName }
}

func ComponentByShortName(shortName string) func(Component) bool {
	return func(c Component) bool { return path.Base(c.Name) == shortName }
}
func ComponentByLabel(key string, value json.RawMessage) func(Component) bool {
	return func(c Component) bool {
		for _, l := range c.Labels {
			if l.Name == key && bytes.Equal(l.Value, value) {
				return true
			}
		}
		return false
	}
}

func (r *resolver) Resolve(ctx context.Context, discId, version string, componentMatchFunc ComponentMatchFunc, resourceMatchFunc ResourceMatchFunc) (Resource, error) {
	discovery, err := r.fetchDiscovery(ctx, discId)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to fetch discovery %q: %w", discId, err)
	}
	baseURL, err := r.fetchBaseURL(ctx, "testcomponent")
	if err != nil {
		return Resource{}, fmt.Errorf("failed to fetch base URL %q: %w", discId, err)
	}
	components, err := parseDiscovery(discovery)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to parse discovery %q: %w", discId, err)
	}
	component, ok := findComponent(components, componentMatchFunc, version)
	if !ok {
		return Resource{}, fmt.Errorf("failed to find component for version %q", version)
	}
	resource, err := findResource(component, baseURL, resourceMatchFunc)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to find resource in discovery %q: %w", discId, err)
	}
	return resource, nil
}

func (r *resolver) fetchBaseURL(ctx context.Context, releasechannel string) (string, error) {
	component := ocmv1alpha1.Component{}
	reader := r.cluster.Cluster().GetClient()
	err := reader.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: releasechannel}, &component)
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("releasechannel component %s/%s was not found", r.namespace, releasechannel)
	}
	if err != nil {
		return "", err
	}
	repository := ocmv1alpha1.Repository{}
	err = reader.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: component.Spec.RepositoryRef.Name}, &repository)
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("releasechannel repository %s/%s was not found", r.namespace, component.Spec.RepositoryRef.Name)
	}
	if err != nil {
		return "", err
	}
	var repo ocirepov1.Repository
	err = json.Unmarshal(repository.Spec.RepositorySpec.Raw, &repo)
	if err != nil {
		return "", err
	}
	// Check repo type correctly
	if repo.Type.String() != "OCIRegistry" {
		return "", fmt.Errorf("repo needs to be of type OCIRegistry")
	}
	if repo.SubPath != "" {
		return fmt.Sprintf("%s/%s", repo.BaseUrl, repo.SubPath), nil
	}
	return repo.BaseUrl, nil

}

func (r *resolver) fetchDiscovery(ctx context.Context, id string) (ocmv1alpha1.Discovery, error) {
	discovery := ocmv1alpha1.Discovery{}
	reader := r.cluster.Cluster().GetClient()
	err := reader.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: id}, &discovery)
	if apierrors.IsNotFound(err) {
		return ocmv1alpha1.Discovery{}, fmt.Errorf("discovery %q was not found", id)
	}
	if err != nil {
		return ocmv1alpha1.Discovery{}, err
	}
	if discovery.Generation != discovery.Status.ObservedGeneration {
		return ocmv1alpha1.Discovery{}, fmt.Errorf("discovery %q has not been observed for its current spec", id)
	}
	return discovery, nil
}

func discoveryObjectName(providerName string, discoveryID string) string {
	return fmt.Sprintf("sp-%s-%s", providerName, discoveryID)
}

func parseDiscovery(discovery ocmv1alpha1.Discovery) ([]Component, error) {
	components := make([]Component, 0, len(discovery.Status.Components))
	for _, raw := range discovery.Status.Components {
		var desc v2.Descriptor
		err := json.Unmarshal(raw.Raw, &desc)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal components of discovery %s: %w", discovery.Name, err)
		}
		component := Component{
			Name:      desc.Component.Name,
			Version:   desc.Component.Version,
			Resources: desc.Component.Resources,
			Labels:    desc.Component.Labels,
		}
		components = append(components, component)
	}
	return components, nil
}

func findComponent(components []Component, matchFunc ComponentMatchFunc, version string) (Component, bool) {
	for _, component := range components {
		if (matchFunc == nil || matchFunc(component)) && component.Version == version {
			return component, true
		}
	}
	return Component{}, false
}

func findResource(component Component, baseURL string, matchFunc ResourceMatchFunc) (Resource, error) {
	if matchFunc == nil {
		return Resource{}, fmt.Errorf("matchFunc must not be nil")
	}
	for _, resource := range component.Resources {
		if matchFunc(resource) {
			accessURL, err := decodeAccess(resource, baseURL, component.Name)
			fmt.Println(accessURL)
			if err != nil {
				return Resource{}, fmt.Errorf("no matching resource was found")
			}
			return Resource{
				Name:      resource.Name,
				Type:      resource.Type,
				AccessURL: accessURL,
			}, nil
		}
	}
	return Resource{}, fmt.Errorf("no matching resource was found")
}

func decodeAccess(resource v2.Resource, baseURL string, componentName string) (string, error) {
	if resource.Access == nil {
		return "", fmt.Errorf("resource has no access")
	}

	t := resource.Access.GetType()
	switch {
	case v2.Scheme.IsRegistered(t):
		var blob v2.LocalBlob
		err := v2.Scheme.Convert(resource.Access, &blob)
		if err != nil {
			return "", err
		}
		if blob.GlobalAccess != nil {
			var img ociaccessv1.OCIImage
			err := ociaccess.Scheme.Convert(blob.GlobalAccess, &img)
			if err != nil {
				return "", err
			}
			return img.ImageReference, nil
		}
		return fmt.Sprintf("%s/component-descriptors/%s@%s", baseURL, componentName, blob.LocalReference), nil

	case ociaccess.Scheme.IsRegistered(t):
		var img ociaccessv1.OCIImage
		if err := ociaccess.Scheme.Convert(resource.Access, &img); err != nil {
			return "", err
		}
		return img.ImageReference, nil

	default:
		return "", fmt.Errorf("unhandled access type: %s", t)
	}
}
