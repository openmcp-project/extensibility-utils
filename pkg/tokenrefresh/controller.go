// Package tokenrefresh provides a controller that keeps a ServiceAccount token from a ManagedControlPlane
// in a Secret on a workload cluster, independent of the service provider's main reconcile loop.
package tokenrefresh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/openmcp-project/controller-utils/pkg/clusteraccess"
	"github.com/openmcp-project/controller-utils/pkg/clusters"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// DefaultTokenLifetime is the requested token lifetime if Reconciler.TokenLifetime is not set.
	DefaultTokenLifetime = time.Hour
	// MinTokenLifetime is the minimum token lifetime accepted by the API server.
	MinTokenLifetime = 10 * time.Minute
	// DefaultRetryInterval is the requeue time if Reconciler.RetryInterval is not set.
	DefaultRetryInterval = 10 * time.Second

	// SecretKeyToken, SecretKeyCA and SecretKeyNamespace are the Secret keys written by the Reconciler.
	// They match the layout of a projected ServiceAccount volume.
	SecretKeyToken     = "token"
	SecretKeyCA        = "ca.crt"
	SecretKeyNamespace = "namespace"
)

// ExpirationAnnotation returns the annotation key under which the token expiration time is stored on the Secret.
func ExpirationAnnotation(serviceProvider string) string {
	return fmt.Sprintf("%s.services.openmcp.cloud/token-expiration-time", serviceProvider)
}

// CreationAnnotation returns the annotation key under which the token creation time is stored on the Secret.
func CreationAnnotation(serviceProvider string) string {
	return fmt.Sprintf("%s.services.openmcp.cloud/token-creation-time", serviceProvider)
}

// Binding identifies the ServiceAccount on the MCP cluster and the Secret on the workload cluster that receives its token.
type Binding struct {
	ServiceAccount types.NamespacedName
	Secret         types.NamespacedName
}

// ClusterResolver returns the MCP and workload cluster for the reconciled object.
type ClusterResolver func(ctx context.Context, req reconcile.Request) (mcp, workload *clusters.Cluster, err error)

// BindingFunc returns the Binding for the given object. If ok is false, the object is not ready yet and is reconciled again later.
type BindingFunc[T client.Object] func(obj T) (binding Binding, ok bool)

// Reconciler refreshes the token in the target Secret before it expires. It only writes the token data and the
// expiration annotation, the Secret itself has to be created by the caller, e.g. the service provider's main reconciler.
type Reconciler[T client.Object] struct {
	// Client reads objects of type T, usually the manager's client.
	Client client.Client
	// ServiceProviderName is used to build the expiration annotation key, see ExpirationAnnotation.
	ServiceProviderName string
	// NewObject returns an empty object of type T.
	NewObject func() T
	// Clusters resolves the MCP and workload cluster of an object.
	Clusters ClusterResolver
	// Binding returns the ServiceAccount and Secret of an object.
	Binding BindingFunc[T]
	// TokenLifetime is the requested token lifetime. Defaults to DefaultTokenLifetime.
	TokenLifetime time.Duration
	// RetryInterval is the requeue time if the token can not be refreshed yet. Defaults to DefaultRetryInterval.
	RetryInterval time.Duration
}

func (r *Reconciler[T]) validate() error {
	var errs []error
	if r.Client == nil {
		errs = append(errs, errors.New("field Client must not be nil"))
	}
	if r.ServiceProviderName == "" {
		errs = append(errs, errors.New("field ServiceProviderName must not be empty"))
	}
	if r.NewObject == nil {
		errs = append(errs, errors.New("field NewObject must not be nil"))
	}
	if r.Clusters == nil {
		errs = append(errs, errors.New("field Clusters must not be nil"))
	}
	if r.Binding == nil {
		errs = append(errs, errors.New("field Binding must not be nil"))
	}
	if r.TokenLifetime != 0 && r.TokenLifetime < MinTokenLifetime {
		errs = append(errs, fmt.Errorf("field TokenLifetime must not be less than %s", MinTokenLifetime))
	}
	return errors.Join(errs...)
}

func (r *Reconciler[T]) tokenLifetime() time.Duration {
	if r.TokenLifetime == 0 {
		return DefaultTokenLifetime
	}
	return r.TokenLifetime
}

func (r *Reconciler[T]) retryInterval() time.Duration {
	if r.RetryInterval <= 0 {
		return DefaultRetryInterval
	}
	return r.RetryInterval
}

// requeueUntil returns the requeue time until t, but at least the retry interval to prevent hot loops.
func (r *Reconciler[T]) requeueUntil(t time.Time) ctrl.Result {
	return ctrl.Result{RequeueAfter: max(time.Until(t), r.retryInterval())}
}

// renewalTime returns when the token in secret is due for refresh based on secret annotations. ok is false if either annotation is missing or unparseable.
func (r *Reconciler[T]) renewalTime(secret *corev1.Secret) (renewal time.Time, ok bool) {
	creation, err := time.Parse(time.RFC3339, secret.Annotations[CreationAnnotation(r.ServiceProviderName)])
	if err != nil {
		return time.Time{}, false
	}

	expiration, err := time.Parse(time.RFC3339, secret.Annotations[ExpirationAnnotation(r.ServiceProviderName)])
	if err != nil {
		return time.Time{}, false
	}

	return clusteraccess.ComputeTokenRenewalTime(creation, expiration), true
}

// Reconcile refreshes the token if needed or if it's missing and requeues until the next renewal time.
// Transient failures (object, cluster or secret not ready, or a token mint error) are returned as errors,
// so controller-runtime requeues them with its default backoff.
func (r *Reconciler[T]) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.NewObject()
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get object: %w", err)
	}
	binding, ok := r.Binding(obj)
	if !ok {
		return ctrl.Result{}, errors.New("token binding not ready")
	}
	mcp, workload, err := r.Clusters(ctx, req)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("clusters not available: %w", err)
	}
	if mcp == nil || workload == nil {
		return ctrl.Result{}, errors.New("clusters not ready")
	}

	secret := &corev1.Secret{}
	if err := workload.Client().Get(ctx, binding.Secret, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get token secret %s: %w", binding.Secret, err)
	}

	// Skip the refresh while the current token exists and is still before its renewal time.
	if len(secret.Data[SecretKeyToken]) > 0 {

		renewal, parsed := r.renewalTime(secret)
		if parsed && !renewal.IsZero() && time.Now().Before(renewal) {
			return r.requeueUntil(renewal), nil
		}
	}

	token, err := r.refreshToken(ctx, mcp, secret, binding, workload.Client())
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to refresh token for %s: %w", binding.ServiceAccount, err)
	}
	renewal := clusteraccess.ComputeTokenRenewalTime(token.CreationTimestamp, token.ExpirationTimestamp)
	logger.Info("refreshed token", "secret", binding.Secret, "expiration", token.ExpirationTimestamp, "renewal", renewal)
	return r.requeueUntil(renewal), nil
}

// EnsureToken mints a token for binding.ServiceAccount on the mcp cluster and writes it into secret in memory, without persisting it.
// Callers may use it to mint the first token while creating the Secret. It returns the minted token.
func EnsureToken(ctx context.Context, mcp *clusters.Cluster, secret *corev1.Secret, binding Binding, serviceProvider string, lifetime time.Duration) (*clusteraccess.ServiceAccountToken, error) {
	caData, err := caData(mcp)
	if err != nil {
		return nil, err
	}
	sa := &corev1.ServiceAccount{}
	if err := mcp.Client().Get(ctx, binding.ServiceAccount, sa); err != nil {
		return nil, err
	}
	token, err := clusteraccess.CreateTokenForServiceAccount(ctx, mcp.Client(), sa, &lifetime)
	if err != nil {
		return nil, err
	}
	if token.ExpirationTimestamp.IsZero() {
		return nil, fmt.Errorf("token for ServiceAccount '%s' has no expiration time", binding.ServiceAccount)
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[SecretKeyToken] = []byte(token.Token)
	secret.Data[SecretKeyCA] = caData
	secret.Data[SecretKeyNamespace] = []byte(binding.ServiceAccount.Namespace)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[CreationAnnotation(serviceProvider)] = token.CreationTimestamp.Format(time.RFC3339)
	secret.Annotations[ExpirationAnnotation(serviceProvider)] = token.ExpirationTimestamp.Format(time.RFC3339)
	return token, nil
}

// refreshToken mints a new token via EnsureToken and patches it into the workload Secret.
func (r *Reconciler[T]) refreshToken(ctx context.Context, mcp *clusters.Cluster, secret *corev1.Secret, binding Binding, workloadClient client.Client) (*clusteraccess.ServiceAccountToken, error) {
	patch := client.MergeFrom(secret.DeepCopy())
	token, err := EnsureToken(ctx, mcp, secret, binding, r.ServiceProviderName, r.tokenLifetime())
	if err != nil {
		return nil, err
	}
	if err := workloadClient.Patch(ctx, secret, patch); err != nil {
		return nil, err
	}
	return token, nil
}

// caData returns the CA bundle of the MCP API server.
func caData(mcp *clusters.Cluster) ([]byte, error) {
	cfg := mcp.RESTConfig()
	if cfg == nil {
		return nil, errors.New("MCP rest config must not be nil")
	}
	if cfg.CAFile != "" {
		return os.ReadFile(cfg.CAFile)
	}
	if len(cfg.CAData) == 0 {
		return nil, errors.New("MCP rest config has no CA data")
	}
	return cfg.CAData, nil
}

// SetupWithManager registers the Reconciler with the given manager under the given controller name.
func (r *Reconciler[T]) SetupWithManager(mgr ctrl.Manager, name string) error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("invalid token refresh reconciler: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(r.NewObject(), builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Complete(r)
}
