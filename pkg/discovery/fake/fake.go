package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/openmcp-project/extensibility-utils/pkg/discovery"
	corev1 "k8s.io/api/core/v1"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
)

type Resolver struct {
	requeue    bool
	secret     *corev1.LocalObjectReference
	components []Component

	// Sets errors to return for interface functions
	applyErr        error
	credentialsErr  error
	resolveErr      error
	listVersionsErr error

	// Override respective fake resolver behavior if custom behavior is required
	ApplyFunc        func(ctx context.Context) (bool, error)
	CredentialsFunc  func(ctx context.Context) (*corev1.LocalObjectReference, error)
	ResolveFunc      func(ctx context.Context, discId, version string, componentMatchFunc discovery.ComponentMatchFunc, matchFunc discovery.ResourceMatchFunc) (discovery.Resource, error)
	ListVersionsFunc func(ctx context.Context, discId string, matchFunc discovery.ComponentMatchFunc) ([]string, error)
}

type Component struct {
	Name      string
	Version   string
	Labels    map[string]json.RawMessage
	Resources []Resource
}

type Resource struct {
	Name      string
	Type      string
	AccessURL string
	Labels    map[string]json.RawMessage
}

var _ discovery.Resolver = &Resolver{}

func NewResolver() *Resolver {
	return &Resolver{}
}

func (f *Resolver) WithRequeue(requeue bool) *Resolver {
	f.requeue = requeue
	return f
}

func (f *Resolver) WithCredentials(secret *corev1.LocalObjectReference) *Resolver {
	f.secret = secret
	return f
}

func (f *Resolver) WithComponents(components []Component) *Resolver {
	f.components = components
	return f
}

func (f *Resolver) WithApplyErr(err error) *Resolver {
	f.applyErr = err
	return f
}

func (f *Resolver) WithCredentialsError(err error) *Resolver {
	f.credentialsErr = err
	return f
}

func (f *Resolver) WithResolveError(err error) *Resolver {
	f.resolveErr = err
	return f
}

func (f *Resolver) WithListVersionsError(err error) *Resolver {
	f.listVersionsErr = err
	return f
}

func (f *Resolver) Apply(ctx context.Context) (bool, error) {
	if f.applyErr != nil {
		return false, f.applyErr
	}
	if f.ApplyFunc != nil {
		return f.ApplyFunc(ctx)
	}
	return f.requeue, nil
}

func (f *Resolver) Credentials(ctx context.Context) (*corev1.LocalObjectReference, error) {
	if f.credentialsErr != nil {
		return nil, f.credentialsErr
	}
	if f.CredentialsFunc != nil {
		return f.CredentialsFunc(ctx)
	}
	return f.secret, nil
}

func (f *Resolver) Resolve(ctx context.Context, discId, version string, componentMatchFunc discovery.ComponentMatchFunc, resourceMatchFunc discovery.ResourceMatchFunc) (discovery.Resource, error) {
	if f.resolveErr != nil {
		return discovery.Resource{}, f.resolveErr
	}
	if f.ResolveFunc != nil {
		return f.ResolveFunc(ctx, discId, version, componentMatchFunc, resourceMatchFunc)
	}
	if resourceMatchFunc == nil {
		return discovery.Resource{}, fmt.Errorf("matchFunc must not be nil")
	}
	for _, c := range f.components {
		if strings.TrimPrefix(c.Version, "v") != strings.TrimPrefix(version, "v") {
			continue
		}
		if componentMatchFunc != nil && !componentMatchFunc(toComponent(c)) {
			continue
		}
		for _, r := range c.Resources {
			if resourceMatchFunc(toResource(r)) {
				return discovery.Resource{Name: r.Name, Type: r.Type, AccessURL: r.AccessURL}, nil
			}
		}
	}
	return discovery.Resource{}, fmt.Errorf("no resource found for version %q", version)
}

func (f *Resolver) ListComponentVersions(ctx context.Context, discId string, matchFunc discovery.ComponentMatchFunc) ([]string, error) {
	if f.listVersionsErr != nil {
		return nil, f.listVersionsErr
	}
	if f.ListVersionsFunc != nil {
		return f.ListVersionsFunc(ctx, discId, matchFunc)
	}
	var versions []string
	for _, c := range f.components {
		if matchFunc == nil || matchFunc(toComponent(c)) {
			versions = append(versions, c.Version)
		}
	}
	slices.SortFunc(versions, discovery.CompareVersions)
	return versions, nil
}

func toLabels(in map[string]json.RawMessage) []v2.Label {
	labels := make([]v2.Label, 0, len(in))
	for k, v := range in {
		labels = append(labels, v2.Label{Name: k, Value: v})
	}
	return labels
}

func toComponent(c Component) discovery.Component {
	return discovery.Component{
		Name:    c.Name,
		Version: c.Version,
		Labels:  toLabels(c.Labels),
	}
}

func toResource(r Resource) v2.Resource {
	return v2.Resource{
		ElementMeta: v2.ElementMeta{
			ObjectMeta: v2.ObjectMeta{
				Name:   r.Name,
				Labels: toLabels(r.Labels),
			},
		},
		Type: r.Type,
	}
}
