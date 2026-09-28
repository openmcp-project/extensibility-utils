// Package secret provides object-manager helpers for copying Kubernetes secrets.
package secret

import (
	"context"
	"fmt"

	ctrlutils "github.com/openmcp-project/controller-utils/pkg/controller"
	openmcpresources "github.com/openmcp-project/controller-utils/pkg/resources"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
)

// CopyConfig holds the configuration for copying a secret between clusters or namespaces.
type CopyConfig struct {
	SourceClient    client.Client
	SourceName      string
	SourceNamespace string
	TargetNamespace string
	TargetName      string
}

func validateConfig(config CopyConfig) error {
	if config.SourceClient == nil {
		return fmt.Errorf("CopyConfig.SourceClient must not be nil")
	}
	if config.SourceName == "" {
		return fmt.Errorf("CopyConfig.SourceName must not be empty")
	}
	if config.SourceNamespace == "" {
		return fmt.Errorf("CopyConfig.SourceNamespace must not be empty")
	}
	if config.TargetNamespace == "" {
		return fmt.Errorf("CopyConfig.TargetNamespace must not be empty")
	}
	if config.TargetName == "" {
		return fmt.Errorf("CopyConfig.TargetName must not be empty")
	}
	return nil
}

// ManagePullSecret registers an image-pull secret copy on a target cluster.
func ManagePullSecret(targetCluster objectmanager.Cluster, config CopyConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	targetCluster.AddObject(createSecret(config))
	return nil
}

func createSecret(config CopyConfig) objectmanager.Object {
	return objectmanager.NewObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.TargetName,
			Namespace: config.TargetNamespace,
		},
	}, objectmanager.ObjectConfig{
		ReconcileFunc: func(ctx context.Context, object client.Object) error {
			targetSecret, ok := object.(*corev1.Secret)
			if !ok {
				return fmt.Errorf("expected *corev1.Secret, got %T", object)
			}
			sourceSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SourceName,
					Namespace: config.SourceNamespace,
				},
			}
			if err := config.SourceClient.Get(ctx, client.ObjectKeyFromObject(sourceSecret), sourceSecret); err != nil {
				return fmt.Errorf("get source secret: %w", err)
			}
			return openmcpresources.NewSecretMutator(config.TargetName, config.TargetNamespace, sourceSecret.Data, corev1.SecretTypeDockerConfigJson).Mutate(targetSecret)
		},
		StatusFunc: objectmanager.SimpleStatus,
	})
}

// PrefixName prefixes a secret name and limits it to the Kubernetes name length.
func PrefixName(name, prefix string) (string, error) {
	return ctrlutils.ShortenToXCharacters(fmt.Sprintf("%s%s", prefix, name), ctrlutils.K8sMaxNameLength)
}

// NewCleaner removes managed pull secrets not included in secretsToKeep.
func NewCleaner(cluster objectmanager.Cluster, serviceProvider, namespace string, secretsToKeep []corev1.LocalObjectReference) objectmanager.Cleaner {
	return objectmanager.NewCleaner(
		cluster,
		serviceProvider,
		namespace,
		objectmanager.CleanerConfig[*corev1.SecretList]{
			ObjectsToKeep: secretsToKeep,
			EmptyList:     func() *corev1.SecretList { return &corev1.SecretList{} },
		})
}
