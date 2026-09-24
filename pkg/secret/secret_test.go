package secret

import (
	"context"
	"fmt"
	"strings"
	"testing"

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

const (
	secretName      = "privateregcred"
	sourceNamespace = "source"
	targetNamespace = "target"
)

func TestManagePullSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	sourceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: sourceNamespace},
		Data:       map[string][]byte{"test": []byte("testdata")},
		// Deliberately Opaque to verify the mutator always sets DockerConfigJson on the target.
		Type: corev1.SecretTypeOpaque,
	}
	existingTargetSecret := &corev1.Secret{
		// A pre-existing secret in the target namespace must not be altered.
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: targetNamespace},
		Data:       map[string][]byte{"existing-secret-data": []byte("must-not-be-altered")},
		Type:       corev1.SecretTypeDockerConfigJson,
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sourceSecret, existingTargetSecret).Build()

	tests := []struct {
		name    string
		config  CopyConfig
		wantErr bool
	}{
		{
			name: "copy secret from source to target namespace",
			config: CopyConfig{
				SourceClient:    fakeClient,
				SourceName:      secretName,
				SourceNamespace: sourceNamespace,
				TargetNamespace: targetNamespace,
				TargetName:      fmt.Sprintf("prefix-%s", secretName),
			},
		},
		{
			name: "source secret not found — wrong name",
			config: CopyConfig{
				SourceClient:    fakeClient,
				SourceName:      "wrongname",
				SourceNamespace: sourceNamespace,
				TargetNamespace: targetNamespace,
				TargetName:      fmt.Sprintf("prefix-%s", secretName),
			},
			wantErr: true,
		},
		{
			name: "source secret not found — wrong namespace",
			config: CopyConfig{
				SourceClient:    fakeClient,
				SourceName:      secretName,
				SourceNamespace: "wrongnamespace",
				TargetNamespace: targetNamespace,
				TargetName:      fmt.Sprintf("prefix-%s", secretName),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := objectmanager.NewCluster(fakeClient, sourceNamespace, objectmanager.PlatformCluster)
			err := ManagePullSecret(cluster, tt.config)
			require.NoError(t, err)

			mgr := objectmanager.NewManager("test")
			mgr.AddCluster(cluster)
			result, err := mgr.Apply(context.Background())

			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, objectmanager.ErrReconcileManagedObjects)
				require.Len(t, result.Results, 1)
				assert.Error(t, result.Results[0].Error)
				return
			}

			require.NoError(t, err)

			target := &corev1.Secret{}
			require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Name: tt.config.TargetName, Namespace: tt.config.TargetNamespace}, target))
			assert.Equal(t, sourceSecret.Data, target.Data)
			assert.Equal(t, corev1.SecretTypeDockerConfigJson, target.Type)

			// The pre-existing secret in the target namespace must be untouched.
			existing := &corev1.Secret{}
			require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Name: secretName, Namespace: targetNamespace}, existing))
			assert.Equal(t, map[string][]byte{"existing-secret-data": []byte("must-not-be-altered")}, existing.Data)
		})
	}
}

func TestPrefixName(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"short name", "privateregcred"},
		{"long name is truncated to 63 characters", strings.Repeat("a", 60)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PrefixName(tt.input, "prefix-")
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(got, "prefix-"))
			assert.LessOrEqual(t, len(got), 63)
		})
	}
}
