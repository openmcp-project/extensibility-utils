package flux

import (
	"context"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openmcp-project/extensibility-utils/pkg/objectmanager"
)

func fluxTestCluster(t *testing.T) objectmanager.Cluster {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, sourcev1.AddToScheme(scheme))
	require.NoError(t, helmv2.AddToScheme(scheme))
	return objectmanager.NewCluster(fake.NewClientBuilder().WithScheme(scheme).Build(), "flux-system", objectmanager.PlatformCluster)
}

func validConfig(t *testing.T) ResourceConfig {
	t.Helper()
	return ResourceConfig{
		Cluster:           fluxTestCluster(t),
		Namespace:         "tenant",
		Interval:          time.Hour,
		KubeConfig:        &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "mcp-kubeconfig", Key: "kubeconfig"}},
		Version:           NewResourceVersion("1.0.0", "oci://registry.example.com/chart", "pull-secret", nil),
		OCIRepositoryName: "chart",
		HelmReleaseName:   "release",
	}
}

func TestManageResources(t *testing.T) {
	config := validConfig(t)
	require.NoError(t, ManageResources(config))

	mgr := objectmanager.NewManager("test")
	mgr.AddCluster(config.Cluster)
	result, err := mgr.Apply(context.Background())
	require.NoError(t, err)
	assert.True(t, result.Requeue, "Flux resources are not yet Ready (no ReadyCondition set by fake client)")

	ociRepo := &sourcev1.OCIRepository{}
	require.NoError(t, config.Cluster.GetClient().Get(context.Background(), client.ObjectKey{Name: "chart", Namespace: "flux-system"}, ociRepo))
	assert.Equal(t, "oci://registry.example.com/chart", ociRepo.Spec.URL)
	assert.Equal(t, "1.0.0", ociRepo.Spec.Reference.Tag)
	assert.Equal(t, time.Hour, ociRepo.Spec.Interval.Duration)
	assert.Equal(t, "pull-secret", ociRepo.Spec.SecretRef.Name)
	require.NotNil(t, ociRepo.Spec.LayerSelector)
	assert.Equal(t, "application/vnd.cncf.helm.chart.content.v1.tar+gzip", ociRepo.Spec.LayerSelector.MediaType)
	assert.Equal(t, "extract", ociRepo.Spec.LayerSelector.Operation)

	helmRelease := &helmv2.HelmRelease{}
	require.NoError(t, config.Cluster.GetClient().Get(context.Background(), client.ObjectKey{Name: "release", Namespace: "flux-system"}, helmRelease))
	assert.Equal(t, "OCIRepository", helmRelease.Spec.ChartRef.Kind)
	assert.Equal(t, "chart", helmRelease.Spec.ChartRef.Name)
	assert.Equal(t, "flux-system", helmRelease.Spec.ChartRef.Namespace)
	assert.Equal(t, time.Hour, helmRelease.Spec.Interval.Duration)
	assert.Equal(t, "mcp-kubeconfig", helmRelease.Spec.KubeConfig.SecretRef.Name)
	assert.Equal(t, "kubeconfig", helmRelease.Spec.KubeConfig.SecretRef.Key)
	assert.Equal(t, "tenant", helmRelease.Spec.TargetNamespace)
	assert.Equal(t, "tenant", helmRelease.Spec.StorageNamespace)
	require.NotNil(t, helmRelease.Spec.Install)
	assert.Equal(t, 3, helmRelease.Spec.Install.Remediation.Retries)
	assert.True(t, helmRelease.Spec.Install.CreateNamespace)
	require.NotNil(t, helmRelease.Spec.Upgrade)
	assert.Equal(t, 3, helmRelease.Spec.Upgrade.Remediation.Retries)
	assert.Equal(t, helmv2.DriftDetectionEnabled, helmRelease.Spec.DriftDetection.Mode)
}

func TestManageResources_NoPullSecret(t *testing.T) {
	config := validConfig(t)
	config.Version = NewResourceVersion("1.0.0", "oci://registry.example.com/chart", "", nil)
	require.NoError(t, ManageResources(config))

	mgr := objectmanager.NewManager("test")
	mgr.AddCluster(config.Cluster)
	_, err := mgr.Apply(context.Background())
	require.NoError(t, err)

	ociRepo := &sourcev1.OCIRepository{}
	require.NoError(t, config.Cluster.GetClient().Get(context.Background(), client.ObjectKey{Name: "chart", Namespace: "flux-system"}, ociRepo))
	assert.Nil(t, ociRepo.Spec.SecretRef, "SecretRef should be absent when no pull secret is configured")
}

func TestManageResourcesValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ResourceConfig)
		wantErr string
	}{
		{
			name:    "nil Cluster",
			mutate:  func(c *ResourceConfig) { c.Cluster = nil },
			wantErr: "Cluster",
		},
		{
			name:    "nil Version",
			mutate:  func(c *ResourceConfig) { c.Version = nil },
			wantErr: "Version",
		},
		{
			name:    "empty ChartURL",
			mutate:  func(c *ResourceConfig) { c.Version = NewResourceVersion("1.0", "", "", nil) },
			wantErr: "GetChartURL",
		},
		{
			name:    "zero Interval",
			mutate:  func(c *ResourceConfig) { c.Interval = 0 },
			wantErr: "Interval",
		},
		{
			name:    "negative Interval",
			mutate:  func(c *ResourceConfig) { c.Interval = -time.Second },
			wantErr: "Interval",
		},
		{
			name:    "nil KubeConfig",
			mutate:  func(c *ResourceConfig) { c.KubeConfig = nil },
			wantErr: "KubeConfig",
		},
		{
			name:    "KubeConfig missing secret name",
			mutate:  func(c *ResourceConfig) { c.KubeConfig.SecretRef.Name = "" },
			wantErr: "KubeConfig",
		},
		{
			name:    "KubeConfig missing secret key",
			mutate:  func(c *ResourceConfig) { c.KubeConfig.SecretRef.Key = "" },
			wantErr: "KubeConfig",
		},
		{
			name:    "empty OCIRepositoryName",
			mutate:  func(c *ResourceConfig) { c.OCIRepositoryName = "" },
			wantErr: "OCIRepositoryName",
		},
		{
			name:    "empty HelmReleaseName",
			mutate:  func(c *ResourceConfig) { c.HelmReleaseName = "" },
			wantErr: "HelmReleaseName",
		},
		{
			name:    "empty Namespace",
			mutate:  func(c *ResourceConfig) { c.Namespace = "" },
			wantErr: "Namespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validConfig(t)
			tt.mutate(&config)
			assert.ErrorContains(t, ManageResources(config), tt.wantErr)
		})
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name        string
		object      client.Object
		wantPhase   string
		wantMessage string
	}{
		{
			name:      "Unknown — object does not implement conditions.Getter",
			object:    &corev1.Secret{},
			wantPhase: objectmanager.StatusPhaseUnknown,
		},
		{
			name: "Terminating — DeletionTimestamp is set",
			object: func() client.Object {
				now := metav1.Now()
				o := &sourcev1.OCIRepository{}
				o.DeletionTimestamp = &now
				return o
			}(),
			wantPhase:   objectmanager.StatusPhaseTerminating,
			wantMessage: "Resource is terminating.",
		},
		{
			name: "Ready — ReadyCondition is True",
			object: func() client.Object {
				o := &sourcev1.OCIRepository{}
				conditions.MarkTrue(o, meta.ReadyCondition, "Reconciled", "applied")
				return o
			}(),
			wantPhase: objectmanager.StatusPhaseReady,
		},
		{
			name: "Progressing — ReadyCondition is False with a message",
			object: func() client.Object {
				o := &sourcev1.OCIRepository{}
				conditions.MarkFalse(o, meta.ReadyCondition, "ChartNotFound", "chart not found in registry")
				return o
			}(),
			wantPhase:   objectmanager.StatusPhaseProgressing,
			wantMessage: "chart not found in registry",
		},
		{
			name:        "Progressing — no conditions set",
			object:      &sourcev1.OCIRepository{},
			wantPhase:   objectmanager.StatusPhaseProgressing,
			wantMessage: "Resource is not ready",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := Status(tt.object)
			assert.Equal(t, tt.wantPhase, status.Phase)
			if tt.wantMessage != "" {
				assert.Equal(t, tt.wantMessage, status.Message)
			}
		})
	}
}
