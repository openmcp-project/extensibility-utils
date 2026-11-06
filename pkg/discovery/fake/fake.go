package fake

import (
	"context"

	"github.com/openmcp-project/extensibility-utils/pkg/discovery"
	corev1 "k8s.io/api/core/v1"
)

// Resolver is a fake implementation of discovery.Resolver for use in tests.
// Set the Func fields to control what each method returns.
type Resolver struct {
	ApplyFunc        func(ctx context.Context) (bool, error)
	CredentialsFunc  func(ctx context.Context) (*corev1.LocalObjectReference, error)
	ResolveFunc      func(ctx context.Context, discId, version string, componentMatchFunc discovery.ComponentMatchFunc, matchFunc discovery.ResourceMatchFunc) (discovery.Resource, error)
	ListVersionsFunc func(ctx context.Context, discId string, matchFunc discovery.ComponentMatchFunc) ([]string, error)
}

var _ discovery.Resolver = &Resolver{}

func (f *Resolver) Apply(ctx context.Context) (bool, error) {
	if f.ApplyFunc != nil {
		return f.ApplyFunc(ctx)
	}
	return false, nil
}

func (f *Resolver) Credentials(ctx context.Context) (*corev1.LocalObjectReference, error) {
	if f.CredentialsFunc != nil {
		return f.CredentialsFunc(ctx)
	}
	return nil, nil
}

func (f *Resolver) Resolve(ctx context.Context, discId, version string, componentMatchFunc discovery.ComponentMatchFunc, matchFunc discovery.ResourceMatchFunc) (discovery.Resource, error) {
	if f.ResolveFunc != nil {
		return f.ResolveFunc(ctx, discId, version, componentMatchFunc, matchFunc)
	}
	return discovery.Resource{}, nil
}

func (f *Resolver) ListVersions(ctx context.Context, discId string, matchFunc discovery.ComponentMatchFunc) ([]string, error) {
	if f.ListVersionsFunc != nil {
		return f.ListVersionsFunc(ctx, discId, matchFunc)
	}
	return nil, nil
}
