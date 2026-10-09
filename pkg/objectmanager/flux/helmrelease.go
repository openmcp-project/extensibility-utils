// Package flux provides helpers for deploying Helm charts through Flux.
package flux

import (
	"context"
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
)

// OCIRepository abstraction to create a Flux OCI repository
type OCIRepository struct {
	// mandatory fields
	Name         string
	ChartURL     string
	ChartVersion string

	// optional fields
	chartPullSecret string
	// interval defaults to 5 minutes based on https://fluxcd.io/flux/components/helm/helmreleases/#recommended-settings
	interval time.Duration
}

// HelmRelease abstraction to create a Flux helm release
type HelmRelease struct {
	// mandatory fields
	Name string
	// Namespace defines the target/storage namespace of the flux HelmRelease
	Namespace string
	// ChartSource contains the Helm chart and has to be an OCI repository in OpenControlPlane
	ChartSource OCIRepository

	// optional fields
	values *apiextensionsv1.JSON
	// interval defaults to 30 minutes based on https://fluxcd.io/flux/components/helm/helmreleases/#recommended-settings
	interval time.Duration
	// kubeconfig allows to target a remote cluster
	kubeconfig *meta.KubeConfigReference
}

func (release *HelmRelease) defaultOptions() {
	// based on https://fluxcd.io/flux/components/helm/helmreleases/#recommended-settings
	release.interval = time.Minute * 30
	release.ChartSource.interval = time.Minute * 5
}

type Option func(*HelmRelease)

func WithChartPullSecret(secret string) Option {
	return func(r *HelmRelease) {
		r.ChartSource.chartPullSecret = secret
	}
}

func WithKubeConfig(kubeconfig meta.KubeConfigReference) Option {
	return func(r *HelmRelease) {
		r.kubeconfig = &kubeconfig
	}
}

func WithOCIRepoInterval(interval time.Duration) Option {
	return func(r *HelmRelease) {
		r.ChartSource.interval = interval
	}
}

func WithValues(values *apiextensionsv1.JSON) Option {
	return func(r *HelmRelease) {
		r.values = values
	}
}

func WithHelmReleaseInterval(interval time.Duration) Option {
	return func(r *HelmRelease) {
		r.interval = interval
	}
}

// ManageResources manages flux resources in a cluster.
func ManageHelmRelease(cluster objectmanager.Cluster, helmRelease *HelmRelease, opts ...Option) error {
	if cluster == nil {
		return fmt.Errorf("cluster must not be nil")
	}
	if helmRelease == nil {
		return fmt.Errorf("helm release must not be nil")
	}
	helmRelease.defaultOptions()
	for _, o := range opts {
		o(helmRelease)
	}
	if err := helmRelease.validate(); err != nil {
		return err
	}
	repoObj := ociRepoObject(cluster, helmRelease.ChartSource)
	cluster.AddObject(repoObj)
	cluster.AddObject(helmRelease.object(cluster, repoObj))
	return nil
}

func (r *HelmRelease) validate() error {
	if r.Name == "" {
		return fmt.Errorf("helm release name must not be empty")
	}
	if r.Namespace == "" {
		return fmt.Errorf("helm release namespace must not be empty")
	}
	ociRepo := r.ChartSource
	if ociRepo.Name == "" {
		return fmt.Errorf("oci repository name must not be empty")
	}
	if ociRepo.ChartVersion == "" {
		return fmt.Errorf("oci repository chart version must not be empty")
	}
	if ociRepo.ChartURL == "" {
		return fmt.Errorf("oci repository chart URL must not be empty")
	}
	return nil
}

func ociRepoObject(cluster objectmanager.Cluster, config OCIRepository) objectmanager.Object {
	return objectmanager.NewObject(&sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Name,
			Namespace: cluster.DefaultNamespace(),
		},
	}, objectmanager.ObjectConfig{
		ReconcileFunc: func(_ context.Context, object client.Object) error {
			ociRepository, ok := object.(*sourcev1.OCIRepository)
			if !ok {
				return fmt.Errorf("expected *sourcev1.OCIRepository, got %T", object)
			}
			spec := sourcev1.OCIRepositorySpec{
				Interval:  metav1.Duration{Duration: config.interval},
				URL:       config.ChartURL,
				Reference: &sourcev1.OCIRepositoryRef{Tag: config.ChartVersion},
				LayerSelector: &sourcev1.OCILayerSelector{
					MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
					Operation: "extract",
				},
			}
			if secret := config.chartPullSecret; secret != "" {
				spec.SecretRef = &meta.LocalObjectReference{Name: secret}
			}
			ociRepository.Spec = spec
			return nil
		},
		DeletionPolicy: objectmanager.Delete,
		StatusFunc:     Status,
	})
}

func (r *HelmRelease) object(cluster objectmanager.Cluster, ociRepo objectmanager.Object) objectmanager.Object {
	return objectmanager.NewObject(&helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.Name,
			Namespace: cluster.DefaultNamespace(),
		},
	}, objectmanager.ObjectConfig{
		ReconcileFunc: func(_ context.Context, object client.Object) error {
			helmRelease, ok := object.(*helmv2.HelmRelease)
			if !ok {
				return fmt.Errorf("expected *helmv2.HelmRelease, got %T", object)
			}
			helmRelease.Spec = helmv2.HelmReleaseSpec{
				Interval: metav1.Duration{Duration: r.interval},
				ChartRef: &helmv2.CrossNamespaceSourceReference{
					Kind:      "OCIRepository",
					Name:      ociRepo.ClientObject().GetName(),
					Namespace: cluster.DefaultNamespace(),
				},
				KubeConfig: r.kubeconfig,
				Install: &helmv2.Install{
					Remediation:     &helmv2.InstallRemediation{Retries: 3},
					CreateNamespace: true,
				},
				Upgrade:          &helmv2.Upgrade{Remediation: &helmv2.UpgradeRemediation{Retries: 3}},
				DriftDetection:   &helmv2.DriftDetection{Mode: helmv2.DriftDetectionEnabled},
				Values:           r.values,
				TargetNamespace:  r.Namespace,
				StorageNamespace: r.Namespace,
			}
			return nil
		},
		DependsOn:      []objectmanager.Object{ociRepo},
		DeletionPolicy: objectmanager.Delete,
		StatusFunc:     Status,
	})
}

// Status reports whether a Flux resource is terminating, ready, or progressing.
func Status(object client.Object) objectmanager.ManagedObjectStatus {
	fluxObject, ok := object.(conditions.Getter)
	if !ok {
		return objectmanager.ManagedObjectStatus{
			Phase:   objectmanager.StatusPhaseUnknown,
			Message: fmt.Sprintf("object %T does not implement conditions.Getter", object),
		}
	}
	if !object.GetDeletionTimestamp().IsZero() {
		return objectmanager.ManagedObjectStatus{
			Phase:   objectmanager.StatusPhaseTerminating,
			Message: "Resource is terminating.",
		}
	}
	if conditions.IsTrue(fluxObject, meta.ReadyCondition) {
		return objectmanager.ManagedObjectStatus{
			Phase:   objectmanager.StatusPhaseReady,
			Message: "Resource is ready",
		}
	}
	message := "Resource is not ready"
	if condition := conditions.Get(fluxObject, meta.ReadyCondition); condition != nil && condition.Message != "" {
		message = condition.Message
	}
	return objectmanager.ManagedObjectStatus{
		Phase:   objectmanager.StatusPhaseProgressing,
		Message: message,
	}
}
