package tokenrefresh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
)

const (
	serviceProvider = "test"
	newToken        = "new-token"
	oldToken        = "old-token"
)

var (
	caBundle = []byte("ca-bundle")
	ownerKey = types.NamespacedName{Name: "owner", Namespace: "onboarding"}
	saKey    = types.NamespacedName{Name: "sa", Namespace: "mcp-ns"}
	secKey   = types.NamespacedName{Name: "kube-api-access-sa", Namespace: "workload-ns"}
)

type testEnv struct {
	reconciler    *Reconciler[*corev1.ConfigMap]
	workload      client.Client
	tokenRequests int
}

type testConfig struct {
	owner        bool
	bindingNotOK bool
	clustersErr  error
	clustersNil  bool
	sa           bool
	secret       *corev1.Secret
	tokenTTL     time.Duration // lifetime returned by the fake API server, 0 = requested lifetime
	tokenLifeReq time.Duration
}

func newScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	return scheme
}

func newTestEnv(t *testing.T, cfg testConfig) *testEnv {
	scheme := newScheme(t)
	env := &testEnv{}

	onboardingBuilder := fake.NewClientBuilder().WithScheme(scheme)
	if cfg.owner {
		onboardingBuilder = onboardingBuilder.WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ownerKey.Name, Namespace: ownerKey.Namespace}})
	}

	mcpBuilder := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, subResourceObj client.Object, opts ...client.SubResourceCreateOption) error {
			tr, ok := subResourceObj.(*authenticationv1.TokenRequest)
			if subResource != "token" || !ok {
				return c.SubResource(subResource).Create(ctx, obj, subResourceObj, opts...)
			}
			env.tokenRequests++
			ttl := cfg.tokenTTL
			if ttl == 0 {
				ttl = time.Duration(*tr.Spec.ExpirationSeconds) * time.Second
			}
			tr.Status = authenticationv1.TokenRequestStatus{
				Token:               newToken,
				ExpirationTimestamp: metav1.NewTime(time.Now().Add(ttl)),
			}
			return nil
		},
	})
	if cfg.sa {
		mcpBuilder = mcpBuilder.WithObjects(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saKey.Name, Namespace: saKey.Namespace}})
	}

	workloadBuilder := fake.NewClientBuilder().WithScheme(scheme)
	if cfg.secret != nil {
		workloadBuilder = workloadBuilder.WithObjects(cfg.secret)
	}
	env.workload = workloadBuilder.Build()

	mcp := clusters.NewTestClusterFromClient("mcp", mcpBuilder.Build()).WithRESTConfig(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: caBundle}})
	workload := clusters.NewTestClusterFromClient("workload", env.workload)

	env.reconciler = &Reconciler[*corev1.ConfigMap]{
		Client:              onboardingBuilder.Build(),
		ServiceProviderName: serviceProvider,
		NewObject:           func() *corev1.ConfigMap { return &corev1.ConfigMap{} },
		Clusters: func(_ context.Context, _ reconcile.Request) (*clusters.Cluster, *clusters.Cluster, error) {
			if cfg.clustersErr != nil {
				return nil, nil, cfg.clustersErr
			}
			if cfg.clustersNil {
				return nil, nil, nil
			}
			return mcp, workload, nil
		},
		Binding: func(_ *corev1.ConfigMap) (Binding, bool) {
			return Binding{ServiceAccount: saKey, Secret: secKey}, !cfg.bindingNotOK
		},
		TokenLifetime: cfg.tokenLifeReq,
	}
	return env
}

func secretWithToken(token string, creation, expiration time.Time) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secKey.Name, Namespace: secKey.Namespace}}
	if token != "" {
		s.Data = map[string][]byte{SecretKeyToken: []byte(token)}
	}
	ann := map[string]string{}
	if !creation.IsZero() {
		ann[CreationAnnotation(serviceProvider)] = creation.Format(time.RFC3339)
	}
	if !expiration.IsZero() {
		ann[ExpirationAnnotation(serviceProvider)] = expiration.Format(time.RFC3339)
	}
	if len(ann) > 0 {
		s.Annotations = ann
	}
	return s
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name string
		cfg  testConfig
		// reconciles is how many times Reconcile is called (default 1); only the last result is asserted.
		reconciles int
		// wantRequeueMin/Max bound the expected RequeueAfter of the last reconcile.
		wantRequeueMin time.Duration
		wantRequeueMax time.Duration
		wantRefresh    bool
		// wantTokenRequests is the expected number of issued tokens (default: 1 if wantRefresh else 0).
		wantTokenRequests int
		// wantErr expects Reconcile to return an error; no requeue or token assertions are made.
		wantErr bool
	}{
		{
			name: "object not found stops reconciling",
			cfg:  testConfig{sa: true, secret: secretWithToken("", time.Time{}, time.Time{})},
		},
		{
			name:           "binding not ready requeues",
			cfg:            testConfig{owner: true, bindingNotOK: true, sa: true, secret: secretWithToken("", time.Time{}, time.Time{})},
			wantRequeueMin: DefaultRetryInterval,
			wantRequeueMax: DefaultRetryInterval,
		},
		{
			name:           "clusters not ready requeues",
			cfg:            testConfig{owner: true, clustersNil: true, sa: true, secret: secretWithToken("", time.Time{}, time.Time{})},
			wantRequeueMin: DefaultRetryInterval,
			wantRequeueMax: DefaultRetryInterval,
		},
		{
			name:    "unavailable clusters return an error",
			cfg:     testConfig{owner: true, clustersErr: errors.New("access request not granted"), sa: true},
			wantErr: true,
		},
		{
			name:    "missing secret returns an error",
			cfg:     testConfig{owner: true, sa: true},
			wantErr: true,
		},
		{
			name:    "missing service account returns an error",
			cfg:     testConfig{owner: true, secret: secretWithToken("", time.Time{}, time.Time{})},
			wantErr: true,
		},
		{
			name:           "empty secret gets a token",
			cfg:            testConfig{owner: true, sa: true, secret: secretWithToken("", time.Time{}, time.Time{})},
			wantRequeueMin: 47 * time.Minute,
			wantRequeueMax: 48 * time.Minute,
			wantRefresh:    true,
		},
		{
			name:           "valid token is not refreshed",
			cfg:            testConfig{owner: true, sa: true, secret: secretWithToken(oldToken, time.Now().Add(-30*time.Minute), time.Now().Add(30*time.Minute))},
			wantRequeueMin: 17 * time.Minute,
			wantRequeueMax: 18 * time.Minute,
		},
		{
			// A token with a missing/unparseable creation annotation (e.g. written by an older
			// provider version) must be refreshed, not treated as an error.
			name:           "token without creation annotation is refreshed",
			cfg:            testConfig{owner: true, sa: true, secret: secretWithToken(oldToken, time.Time{}, time.Now().Add(30*time.Minute))},
			wantRequeueMin: 47 * time.Minute,
			wantRequeueMax: 48 * time.Minute,
			wantRefresh:    true,
		},
		{
			name:           "token past refresh time is refreshed",
			cfg:            testConfig{owner: true, sa: true, secret: secretWithToken(oldToken, time.Now().Add(-55*time.Minute), time.Now().Add(5*time.Minute))},
			wantRequeueMin: 47 * time.Minute,
			wantRequeueMax: 48 * time.Minute,
			wantRefresh:    true,
		},
		{
			name:           "annotation without token data is refreshed",
			cfg:            testConfig{owner: true, sa: true, secret: secretWithToken("", time.Time{}, time.Now().Add(30*time.Minute))},
			wantRequeueMin: 47 * time.Minute,
			wantRequeueMax: 48 * time.Minute,
			wantRefresh:    true,
		},
		{
			name:           "custom lifetime is requested",
			cfg:            testConfig{owner: true, sa: true, secret: secretWithToken("", time.Time{}, time.Time{}), tokenLifeReq: 20 * time.Minute},
			wantRequeueMin: 15 * time.Minute,
			wantRequeueMax: 16 * time.Minute,
			wantRefresh:    true,
		},
		{
			name:              "shorter token than requested does not hot loop",
			cfg:               testConfig{owner: true, sa: true, secret: secretWithToken("", time.Time{}, time.Time{}), tokenTTL: 5 * time.Minute},
			reconciles:        2,
			wantRequeueMin:    3 * time.Minute,
			wantRequeueMax:    4 * time.Minute,
			wantRefresh:       true,
			wantTokenRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, tt.cfg)

			reconciles := tt.reconciles
			if reconciles == 0 {
				reconciles = 1
			}
			var res ctrl.Result
			var err error
			for i := 0; i < reconciles; i++ {
				res, err = env.reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: ownerKey})
				if tt.wantErr {
					break
				}
				require.NoError(t, err)
			}
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			assert.GreaterOrEqual(t, res.RequeueAfter, tt.wantRequeueMin)
			assert.LessOrEqual(t, res.RequeueAfter, tt.wantRequeueMax)

			wantTokenRequests := tt.wantTokenRequests
			if wantTokenRequests == 0 && tt.wantRefresh {
				wantTokenRequests = 1
			}
			assert.Equal(t, wantTokenRequests, env.tokenRequests)

			if !tt.wantRefresh {
				return
			}
			secret := &corev1.Secret{}
			require.NoError(t, env.workload.Get(t.Context(), secKey, secret))
			assert.Equal(t, newToken, string(secret.Data[SecretKeyToken]))
			assert.Equal(t, caBundle, secret.Data[SecretKeyCA])
			assert.Equal(t, saKey.Namespace, string(secret.Data[SecretKeyNamespace]))
			expiration, err := time.Parse(time.RFC3339, secret.Annotations[ExpirationAnnotation(serviceProvider)])
			require.NoError(t, err)
			assert.True(t, expiration.After(time.Now()))
		})
	}
}

func TestExpirationAnnotation(t *testing.T) {
	assert.Equal(t, "velero.services.open-control-plane.io/token-expiration-time", ExpirationAnnotation("velero"))
}

func TestEnsureTokenGuards(t *testing.T) {
	ctx := t.Context()
	secret := &corev1.Secret{}
	binding := Binding{ServiceAccount: saKey, Secret: secKey}

	// A nil mcp cluster is rejected before any cluster access (which would otherwise nil-panic).
	_, err := EnsureToken(ctx, nil, secret, binding, serviceProvider, DefaultTokenLifetime)
	assert.ErrorContains(t, err, "mcp cluster must not be nil")

	// A lifetime below the API-server minimum is rejected.
	mcp := clusters.NewTestClusterFromClient("mcp", fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	_, err = EnsureToken(ctx, mcp, secret, binding, serviceProvider, MinTokenLifetime-time.Second)
	assert.ErrorContains(t, err, "must not be less than")
}

func TestValidate(t *testing.T) {
	valid := func() *Reconciler[*corev1.ConfigMap] {
		return newTestEnv(t, testConfig{}).reconciler
	}
	require.NoError(t, valid().validate())

	r := valid()
	r.ServiceProviderName = ""
	assert.ErrorContains(t, r.validate(), "ServiceProviderName")

	r = valid()
	r.TokenLifetime = 5 * time.Minute
	assert.ErrorContains(t, r.validate(), "TokenLifetime")

	r = valid()
	r.Clusters = nil
	r.Binding = nil
	err := r.validate()
	assert.ErrorContains(t, err, "Clusters")
	assert.ErrorContains(t, err, "Binding")
}
