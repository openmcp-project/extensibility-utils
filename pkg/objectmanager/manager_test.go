package objectmanager

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func testCluster(t *testing.T, objects ...runtime.Object) Cluster {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	return NewCluster(fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(), "default", PlatformCluster)
}

func TestManagerApplyAndDelete(t *testing.T) {
	cluster := testCluster(t)
	cluster.AddObject(NewObject(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "default"}}, ObjectConfig{StatusFunc: SimpleStatus}))
	manager := NewManager("test")
	manager.AddCluster(cluster)

	applyResult, err := manager.Apply(context.Background())
	require.NoError(t, err)
	assert.True(t, applyResult.Requeue)
	require.Len(t, applyResult.Results, 1)
	managedObjects := applyResult.ManagedObjects()
	require.Len(t, managedObjects, 1)
	managedObj := managedObjects[0]
	assert.Equal(t, "Secret", managedObj.Kind)
	assert.Equal(t, "managed", managedObj.Name)
	assert.Equal(t, string(PlatformCluster), managedObj.Location)
	assert.Equal(t, controllerutil.OperationResultCreated, applyResult.Results[0].OperationResult)

	deleteResult, err := manager.Delete(context.Background())
	require.NoError(t, err)
	assert.True(t, deleteResult.Requeue)
	require.Len(t, deleteResult.Results, 1)
	assert.Equal(t, OperationResultDeletionRequested, deleteResult.Results[0].OperationResult)

	deleteResult, err = manager.Delete(context.Background())
	require.NoError(t, err)
	assert.False(t, deleteResult.Requeue)
}

func TestManagedObjectJSON(t *testing.T) {
	encoded, err := json.Marshal(ManagedObject{APIGroup: "apps", Kind: "Deployment", Name: "app", Status: ManagedObjectStatus{Phase: StatusPhaseReady}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"apiGroup":"apps","kind":"Deployment","name":"app","status":{"phase":"Ready"}}`, string(encoded))
}
