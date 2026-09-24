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

// ResourceVersion describes the version information required for a Flux-managed chart.
type ResourceVersion interface {
	GetChartVersion() string
	GetChartURL() string
	GetChartPullSecret() string
	GetHelmValues() *apiextensionsv1.JSON
}

type resourceVersion struct {
	chartVersion    string
	chartURL        string
	chartPullSecret string
	helmValues      *apiextensionsv1.JSON
}

// NewResourceVersion creates a ResourceVersion for callers without a CRD-backed version type.
func NewResourceVersion(chartVersion, chartURL, chartPullSecret string, helmValues *apiextensionsv1.JSON) ResourceVersion {
	return resourceVersion{
		chartVersion:    chartVersion,
		chartURL:        chartURL,
		chartPullSecret: chartPullSecret,
		helmValues:      helmValues,
	}
}

func (v resourceVersion) GetChartVersion() string              { return v.chartVersion }
func (v resourceVersion) GetChartURL() string                  { return v.chartURL }
func (v resourceVersion) GetChartPullSecret() string           { return v.chartPullSecret }
func (v resourceVersion) GetHelmValues() *apiextensionsv1.JSON { return v.helmValues }

// ResourceConfig configures the Flux resources for a Helm chart.
type ResourceConfig struct {
	Cluster           objectmanager.Cluster
	Namespace         string
	Interval          time.Duration
	KubeConfig        *meta.KubeConfigReference
	Version           ResourceVersion
	OCIRepositoryName string
	HelmReleaseName   string
}

// ManageResources registers an OCIRepository and dependent HelmRelease on a cluster.
func ManageResources(config ResourceConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	ociRepository := newOCIRepository(config)
	config.Cluster.AddObject(ociRepository)
	config.Cluster.AddObject(newHelmRelease(config, []objectmanager.Object{ociRepository}))
	return nil
}

func validateConfig(config ResourceConfig) error {
	if config.Cluster == nil {
		return fmt.Errorf("ResourceConfig.Cluster must not be nil")
	}
	if config.Version == nil {
		return fmt.Errorf("ResourceConfig.Version must not be nil")
	}
	if config.Version.GetChartURL() == "" {
		return fmt.Errorf("ResourceConfig.Version.GetChartURL() must not be empty")
	}
	if config.Interval <= 0 {
		return fmt.Errorf("ResourceConfig.Interval must be greater than zero, got %v", config.Interval)
	}
	if config.KubeConfig == nil || config.KubeConfig.SecretRef == nil || config.KubeConfig.SecretRef.Name == "" || config.KubeConfig.SecretRef.Key == "" {
		return fmt.Errorf("ResourceConfig.KubeConfig must reference a secret name and key")
	}
	if config.OCIRepositoryName == "" {
		return fmt.Errorf("ResourceConfig.OCIRepositoryName must not be empty")
	}
	if config.HelmReleaseName == "" {
		return fmt.Errorf("ResourceConfig.HelmReleaseName must not be empty")
	}
	if config.Namespace == "" {
		return fmt.Errorf("ResourceConfig.Namespace must not be empty")
	}
	return nil
}

func newOCIRepository(config ResourceConfig) objectmanager.Object {
	return objectmanager.NewObject(&sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.OCIRepositoryName,
			Namespace: config.Cluster.GetDefaultNamespace(),
		},
	}, objectmanager.ObjectConfig{
		ReconcileFunc: func(_ context.Context, object client.Object) error {
			ociRepository, ok := object.(*sourcev1.OCIRepository)
			if !ok {
				return fmt.Errorf("expected *sourcev1.OCIRepository, got %T", object)
			}
			spec := sourcev1.OCIRepositorySpec{
				Interval:  metav1.Duration{Duration: config.Interval},
				URL:       config.Version.GetChartURL(),
				Reference: &sourcev1.OCIRepositoryRef{Tag: config.Version.GetChartVersion()},
				LayerSelector: &sourcev1.OCILayerSelector{
					MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
					Operation: "extract",
				},
			}
			if secret := config.Version.GetChartPullSecret(); secret != "" {
				spec.SecretRef = &meta.LocalObjectReference{Name: secret}
			}
			ociRepository.Spec = spec
			return nil
		},
		DeletionPolicy: objectmanager.Delete,
		StatusFunc:     Status,
	})
}

func newHelmRelease(config ResourceConfig, dependencies []objectmanager.Object) objectmanager.Object {
	return objectmanager.NewObject(&helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.HelmReleaseName,
			Namespace: config.Cluster.GetDefaultNamespace(),
		},
	}, objectmanager.ObjectConfig{
		ReconcileFunc: func(_ context.Context, object client.Object) error {
			helmRelease, ok := object.(*helmv2.HelmRelease)
			if !ok {
				return fmt.Errorf("expected *helmv2.HelmRelease, got %T", object)
			}
			helmRelease.Spec = helmv2.HelmReleaseSpec{
				Interval: metav1.Duration{Duration: config.Interval},
				ChartRef: &helmv2.CrossNamespaceSourceReference{
					Kind:      "OCIRepository",
					Name:      config.OCIRepositoryName,
					Namespace: config.Cluster.GetDefaultNamespace(),
				},
				KubeConfig: config.KubeConfig,
				Install: &helmv2.Install{
					Remediation:     &helmv2.InstallRemediation{Retries: 3},
					CreateNamespace: true,
				},
				Upgrade:          &helmv2.Upgrade{Remediation: &helmv2.UpgradeRemediation{Retries: 3}},
				DriftDetection:   &helmv2.DriftDetection{Mode: helmv2.DriftDetectionEnabled},
				Values:           config.Version.GetHelmValues(),
				TargetNamespace:  config.Namespace,
				StorageNamespace: config.Namespace,
			}
			return nil
		},
		DependsOn:      dependencies,
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
