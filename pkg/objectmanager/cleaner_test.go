package objectmanager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCleanerDeletesUnwantedManagedObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "obsolete",
			Namespace: "default",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "test"},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	cluster := NewCluster(fakeClient, "default", PlatformCluster)
	cleaner := NewCleaner(cluster, "test", "default", CleanerConfig[*corev1.SecretList]{
		EmptyList: func() *corev1.SecretList { return &corev1.SecretList{} },
	})

	results, err := cleaner.Cleanup(context.Background())
	require.NoError(t, err)
	require.Empty(t, results)
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "obsolete", Namespace: "default"}, &corev1.Secret{})
	require.True(t, apierrors.IsNotFound(err))
}
