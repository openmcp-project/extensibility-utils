package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	ocirepov1 "ocm.software/open-component-model/bindings/go/oci/spec/repository/v1/oci"

	"github.com/Masterminds/semver/v3"
	"github.com/openmcp-project/controller-utils/pkg/clusters"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	ocmv1alpha1 "ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	ociaccess "ocm.software/open-component-model/bindings/go/oci/spec/access"
	ociaccessv1 "ocm.software/open-component-model/bindings/go/oci/spec/access/v1"
	ocirepospec "ocm.software/open-component-model/bindings/go/oci/spec/repository"
	ocmpath "ocm.software/open-component-model/bindings/go/oci/spec/repository/path"
	"ocm.software/open-component-model/bindings/go/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
)

var (
	ociRepoScheme = func() *runtime.Scheme {
		s := runtime.NewScheme()
		ocirepospec.MustAddToScheme(s)
		ocirepospec.MustAddLegacyToScheme(s)
		return s
	}()

	ocmScheme = func() *runtime.Scheme {
		s := runtime.NewScheme()
		ociaccess.MustAddToScheme(s)
		v2.MustAddToScheme(s)
		return s
	}()
)

// Resolver manages Discovery objects and resolves OCM resources from them.
type Resolver interface {
	// Apply creates or updates the Discovery objects on the platform cluster and returns true if a requeue is needed.
	Apply(ctx context.Context) (bool, error)
	// Credentials returns a reference to the first docker-config secret found in the releasechannels effective OCM config, or nil if none exists.
	Credentials(ctx context.Context) (*corev1.LocalObjectReference, error)
	// Resolve fetches the Discovery for discId, finds the component matching version and componentMatchFunc if not nil, then returns the first resource matching resourceMatchFunc.
	Resolve(ctx context.Context, discId, version string, componentMatchFunc ComponentMatchFunc, resourceMatchFunc ResourceMatchFunc) (Resource, error)
	// ListComponentVersions returns the available component versions for discId, optionally filtered by matchFunc, sorted oldest first.
	ListComponentVersions(ctx context.Context, discId string, componentMatchFunc ComponentMatchFunc) ([]string, error)
}

type resolver struct {
	cluster          *clusters.Cluster
	serviceProvider  string
	namespace        string
	releasechannel   ocmv1alpha1.Component
	discoveryconfigs []DiscoveryConfig
	basePath         string
}

// Config holds the configuration for creating a new Resolver.
type Config struct {
	ServiceProvider  string
	Namespace        string
	Releasechannel   string
	DiscoveryConfigs []DiscoveryConfig
}

// DiscoveryConfig defines a single Discovery object to be managed, with optional selectors to filter components and resources.
type DiscoveryConfig struct {
	ID                string
	ComponentSelector *ocmv1alpha1.Selector
	ResourceSelector  *ocmv1alpha1.Selector
}

// Component is a parsed OCM component descriptor entry with its name, version, resources, and labels.
type Component struct {
	Name      string
	Version   string
	Resources []v2.Resource
	Labels    []v2.Label
}

// Resource is a resolved OCM resource with its name, type, and a fully-qualified OCI access URL.
type Resource struct {
	Name      string
	Type      string
	AccessURL string
}

// NewResolver creates a Resolver by fetching the named releasechannel Component and computing its OCI base path.
func NewResolver(ctx context.Context, cfg Config, c *clusters.Cluster) (Resolver, error) {
	releasechannel, err := fetchReleasechannel(ctx, c.Cluster().GetClient(), cfg.Releasechannel, cfg.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releasechannel component: %w", err)
	}
	basePath, err := computeBasePath(releasechannel)
	if err != nil {
		return nil, fmt.Errorf("failed to compute base path: %w", err)
	}
	return &resolver{
		cluster:          c,
		serviceProvider:  cfg.ServiceProvider,
		namespace:        cfg.Namespace,
		releasechannel:   releasechannel,
		discoveryconfigs: cfg.DiscoveryConfigs,
		basePath:         basePath,
	}, nil
}

func fetchReleasechannel(ctx context.Context, reader client.Reader, name string, namespace string) (ocmv1alpha1.Component, error) {
	component := ocmv1alpha1.Component{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &component)
	if apierrors.IsNotFound(err) {
		return ocmv1alpha1.Component{}, fmt.Errorf("releasechannel component %s/%s was not found", namespace, name)
	}
	if err != nil {
		return ocmv1alpha1.Component{}, err
	}
	return component, nil
}

func (r *resolver) Apply(ctx context.Context) (bool, error) {
	platformCluster := objectmanager.NewCluster(r.cluster, r.namespace, objectmanager.PlatformCluster)
	manageDiscoveries(r.discoveryconfigs, r.namespace, platformCluster, r.serviceProvider, r.releasechannel.Name)
	mgr := objectmanager.NewManager(r.serviceProvider)
	mgr.AddCluster(platformCluster)
	rr, err := mgr.Apply(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to apply discovery objects: %w", err)
	}
	return rr.Requeue, nil
}

func manageDiscoveries(discoveryConfigs []DiscoveryConfig, namespace string, cluster objectmanager.Cluster, serviceProvider string, releasechannel string) {
	for _, discConfig := range discoveryConfigs {
		discovery := objectmanager.NewObject(
			&ocmv1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryObjectName(serviceProvider, discConfig.ID),
					Namespace: namespace,
				},
				Spec: ocmv1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: releasechannel},
				},
			},
			objectmanager.ObjectConfig{
				ReconcileFunc: func(_ context.Context, o client.Object) error {
					discovery, ok := o.(*ocmv1alpha1.Discovery)
					if !ok {
						return fmt.Errorf("expected *ocmv1alpha1.Discovery, got %T", o)
					}
					discovery.Spec = ocmv1alpha1.DiscoverySpec{
						ComponentRef:      corev1.LocalObjectReference{Name: releasechannel},
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

func (r *resolver) Credentials(ctx context.Context) (*corev1.LocalObjectReference, error) {
	reader := r.cluster.Cluster().GetClient()
	configs := r.releasechannel.GetEffectiveOCMConfig()
	var fetchErrs []error
	for _, config := range configs {
		if config.Kind != "Secret" {
			continue
		}
		ns := config.Namespace
		if ns == "" {
			ns = r.namespace
		}
		secret := corev1.Secret{}
		err := reader.Get(ctx, client.ObjectKey{Name: config.Name, Namespace: ns}, &secret)
		if err != nil {
			fetchErrs = append(fetchErrs, fmt.Errorf("failed to fetch secret %s/%s: %w", ns, config.Name, err))
			continue
		}
		if secret.Type != corev1.SecretTypeDockerConfigJson {
			continue
		}
		return &corev1.LocalObjectReference{Name: config.Name}, nil
	}
	if len(fetchErrs) > 0 {
		return nil, errors.Join(fetchErrs...)
	}
	return nil, nil
}

func (r *resolver) ListComponentVersions(ctx context.Context, discId string, componentMatchFunc ComponentMatchFunc) ([]string, error) {
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
		if componentMatchFunc == nil || componentMatchFunc(component) {
			availableVersions = append(availableVersions, component.Version)
		}
	}
	slices.SortFunc(availableVersions, CompareVersions)
	return availableVersions, nil
}

// CompareVersions orders two component versions oldest first.
// Versions are compared semantically and versions that do not parse as semver are lexicographically sorted after valid ones.
func CompareVersions(a, b string) int {
	va, errA := semver.NewVersion(a)
	vb, errB := semver.NewVersion(b)
	switch {
	case errA == nil && errB == nil:
		return va.Compare(vb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// ResourceMatchFunc returns true if the given OCM resource should be selected.
type ResourceMatchFunc func(v2.Resource) bool

// ComponentMatchFunc returns true if the given Component should be selected.
type ComponentMatchFunc func(Component) bool

func ResourceByName(name string) ResourceMatchFunc {
	return func(r v2.Resource) bool { return r.Name == name }
}
func ResourceByShortName(name string) ResourceMatchFunc {
	return func(r v2.Resource) bool { return path.Base(r.Name) == name }
}
func ResourceByType(resourceType string) ResourceMatchFunc {
	return func(r v2.Resource) bool { return r.Type == resourceType }
}
func ResourceByLabel(key string, value json.RawMessage) ResourceMatchFunc {
	return func(r v2.Resource) bool {
		for _, l := range r.Labels {
			if l.Name == key && bytes.Equal(l.Value, value) {
				return true
			}
		}
		return false
	}
}

func ComponentByName(name string) ComponentMatchFunc {
	return func(c Component) bool { return c.Name == name }
}
func ComponentByShortName(name string) ComponentMatchFunc {
	return func(c Component) bool { return path.Base(c.Name) == name }
}
func ComponentByLabel(key string, value json.RawMessage) ComponentMatchFunc {
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
	components, err := parseDiscovery(discovery)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to parse discovery %q: %w", discId, err)
	}
	component, ok := findComponent(components, componentMatchFunc, version)
	if !ok {
		return Resource{}, fmt.Errorf("failed to find component for version %q", version)
	}
	resource, err := findResource(component, r.basePath, resourceMatchFunc)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to find resource in discovery %q: %w", discId, err)
	}
	return resource, nil
}

func computeBasePath(releasechannel ocmv1alpha1.Component) (string, error) {
	ociRepo, err := ociRepoFromComponent(&releasechannel)
	if err != nil {
		return "", err
	}

	basePath, err := componentDescriptorBase(ociRepo)
	if err != nil {
		return "", err
	}
	return basePath, nil
}

func (r *resolver) fetchDiscovery(ctx context.Context, id string) (ocmv1alpha1.Discovery, error) {
	discovery := ocmv1alpha1.Discovery{}
	reader := r.cluster.Cluster().GetClient()
	err := reader.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: discoveryObjectName(r.serviceProvider, id)}, &discovery)
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

func findComponent(components []Component, componentMatchFunc ComponentMatchFunc, version string) (Component, bool) {
	for _, component := range components {
		if (componentMatchFunc == nil || componentMatchFunc(component)) && normalizeVersion(component.Version) == normalizeVersion(version) {
			return component, true
		}
	}
	return Component{}, false
}

func normalizeVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

func findResource(component Component, basePath string, resourceMatchFunc ResourceMatchFunc) (Resource, error) {
	if resourceMatchFunc == nil {
		return Resource{}, fmt.Errorf("matchFunc must not be nil")
	}
	for _, resource := range component.Resources {
		if resourceMatchFunc(resource) {
			accessURL, err := decodeAccess(resource, basePath, component.Name)
			if err != nil {
				return Resource{}, fmt.Errorf("failed to decode access for resource %q: %w", resource.Name, err)
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

func decodeAccess(resource v2.Resource, basePath string, componentName string) (string, error) {
	if resource.Access == nil {
		return "", fmt.Errorf("resource has no access")
	}

	obj, err := ocmScheme.NewObject(resource.Access.GetType())
	if err != nil {
		return "", fmt.Errorf("unhandled access type %q: %w", resource.Access.GetType(), err)
	}
	if err := ocmScheme.Convert(resource.Access, obj); err != nil {
		return "", fmt.Errorf("failed to convert access: %w", err)
	}

	switch a := obj.(type) {
	case *v2.LocalBlob:
		if a.GlobalAccess != nil {
			img, err := as[*ociaccessv1.OCIImage](ocmScheme, a.GlobalAccess)
			if err != nil {
				return "", err
			}
			return img.ImageReference, nil
		}
		return fmt.Sprintf("%s/%s@%s", basePath, componentName, a.LocalReference), nil

	case *ociaccessv1.OCIImage:
		return a.ImageReference, nil

	default:
		return "", fmt.Errorf("unhandled access type: %s", resource.Access.GetType())
	}
}

func ociRepoFromComponent(component *ocmv1alpha1.Component) (*ocirepov1.Repository, error) {
	componentInfo := component.Status.Component
	if componentInfo.RepositorySpec == nil {
		return nil, fmt.Errorf("component %q has no resolved repositorySpec", component.Name)
	}

	raw := &runtime.Raw{}
	if err := json.Unmarshal(componentInfo.RepositorySpec.Raw, raw); err != nil {
		return nil, fmt.Errorf("failed to decode repositorySpec: %w", err)
	}

	ociRepo, err := as[*ocirepov1.Repository](ociRepoScheme, raw)
	if err != nil {
		return nil, err
	}

	return ociRepo, nil
}

func componentDescriptorBase(repo *ocirepov1.Repository) (string, error) {
	baseURL := strings.TrimSuffix(repo.BaseUrl, "/")
	purl, err := runtime.ParseURLAndAllowNoScheme(baseURL)
	if err != nil {
		return "", fmt.Errorf("could not parse OCI repository URL %q: %w", repo.BaseUrl, err)
	}

	subPath := repo.SubPath
	if subPath == "" && purl.Path != "" && purl.Path != "/" {
		subPath = strings.TrimPrefix(purl.Path, "/")
		baseURL = purl.Host
	}
	if purl.Scheme != "" {
		baseURL = strings.TrimPrefix(baseURL, purl.Scheme+"://")
	}

	basePath := baseURL
	if subPath != "" {
		basePath += "/" + subPath
	}
	return basePath + "/" + ocmpath.DefaultComponentDescriptorPath, nil
}

func as[T runtime.Typed](scheme *runtime.Scheme, raw *runtime.Raw) (T, error) {
	var zero T
	obj, err := scheme.NewObject(raw.GetType())
	if err != nil {
		return zero, fmt.Errorf("unknown type %q: %w", raw.GetType(), err)
	}
	if err := scheme.Convert(raw, obj); err != nil {
		return zero, err
	}
	typed, ok := obj.(T)
	if !ok {
		return zero, fmt.Errorf("expected %T but got type %q", zero, raw.GetType())
	}
	return typed, nil
}
