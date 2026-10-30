package discovery

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"ocm.software/open-component-model/bindings/go/credentials"
	credcfgruntime "ocm.software/open-component-model/bindings/go/credentials/spec/config/runtime"
	ocmv1alpha1 "ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/pkg/configuration"
	ocicredentials "ocm.software/open-component-model/bindings/go/oci/credentials"
	"ocm.software/open-component-model/bindings/go/oci/looseref"
	ociidentityv1 "ocm.software/open-component-model/bindings/go/oci/spec/identity/v1"
	credentialrepository "ocm.software/open-component-model/bindings/go/plugin/manager/registries/credentialrepository"
	"ocm.software/open-component-model/bindings/go/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrNoCredentialSecret = errors.New("no credential secret found for identity")

func getResourceCredentials(ctx context.Context, reader client.Reader, defaultNS string, imageRef string, ocmConfigs []ocmv1alpha1.OCMConfiguration) (string, error) {
	ref, err := looseref.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("failed to parse image reference %q: %w", imageRef, err)
	}
	identity := runtime.Identity{
		runtime.IdentityAttributeType:     ociidentityv1.Type.String(),
		runtime.IdentityAttributeHostname: ref.Registry,
	}

	registry := credentialrepository.NewCredentialRepositoryRegistry(ctx)
	if err = registry.RegisterInternalCredentialRepositoryPlugin(
		&ocicredentials.OCICredentialRepository{},
		[]runtime.Type{ociidentityv1.Type, ociidentityv1.VersionedType},
	); err != nil {
		return "", fmt.Errorf("failed to register repository plugin: %w", err)
	}

	return findSecretForIdentity(ctx, reader, defaultNS, ocmConfigs, identity, registry)
}

func findSecretForIdentity(ctx context.Context, reader client.Reader, defaultNS string, ocmConfigs []ocmv1alpha1.OCMConfiguration, identity runtime.Identity, registry *credentialrepository.RepositoryRegistry) (string, error) {
	for _, ocmCfg := range ocmConfigs {
		ns := ocmCfg.Namespace
		if ns == "" {
			ns = defaultNS
		}

		var obj client.Object
		switch ocmCfg.Kind {
		case "Secret":
			obj = &corev1.Secret{}
		case "ConfigMap":
			obj = &corev1.ConfigMap{}
		default:
			continue
		}

		if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ocmCfg.Name}, obj); err != nil {
			continue
		}

		genericCfg, err := configuration.GetConfigFromObject(obj)
		if err != nil || genericCfg == nil {
			continue
		}

		credCfg, err := credcfgruntime.LookupCredentialConfig(genericCfg)
		if err != nil || credCfg == nil {
			continue
		}

		graph, err := credentials.ToGraph(ctx, credCfg, credentials.Options{
			RepositoryPluginProvider:       registry,
			CredentialRepositoryTypeScheme: registry.RepositoryScheme(),
		})
		if err != nil {
			continue
		}

		if _, err := graph.Resolve(ctx, identity); err == nil {
			return ocmCfg.Name, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoCredentialSecret, identity.String())
}
