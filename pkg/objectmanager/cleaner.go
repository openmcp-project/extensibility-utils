package objectmanager

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openmcp-project/extensibility-utils/pkg/internal"
)

// ErrCleanup indicates that a cleaner could not list its target objects.
var ErrCleanup = errors.New("cleanup failed")

// Cleaner removes objects that are no longer part of the desired state.
type Cleaner interface {
	Cleanup(context.Context) ([]Result, error)
}

// CleanerConfig describes the objects a Cleaner should retain.
type CleanerConfig[T client.ObjectList] struct {
	ObjectsToKeep    []corev1.LocalObjectReference
	EmptyList        func() T
	PreDeletionSteps func(context.Context, client.Object) (proceed bool, err error)
}

type cleaner[T client.ObjectList] struct {
	cluster         Cluster
	serviceProvider string
	namespace       string
	config          CleanerConfig[T]
}

// NewCleaner removes redundant objects in a target namespace.
func NewCleaner[T client.ObjectList](cluster Cluster, serviceProvider, namespace string, config CleanerConfig[T]) Cleaner {
	return &cleaner[T]{cluster: cluster, serviceProvider: serviceProvider, namespace: namespace, config: config}
}

func (c *cleaner[T]) Cleanup(ctx context.Context) ([]Result, error) {
	if c.config.EmptyList == nil {
		return nil, fmt.Errorf("%w: missing empty list definition", ErrCleanup)
	}
	list := c.config.EmptyList()
	if err := c.cluster.GetClient().List(ctx, list, client.InNamespace(c.namespace), internal.ManagedBy(c.serviceProvider)); err != nil {
		log.FromContext(ctx).Error(err, "failed to list objects for cleanup")
		return nil, fmt.Errorf("%w: %w", ErrCleanup, err)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, fmt.Errorf("cleaner config does not have a proper list type: %w", err)
	}
	results := []Result{}
	for _, item := range items {
		object, ok := item.(client.Object)
		if !ok || slices.ContainsFunc(c.config.ObjectsToKeep, func(reference corev1.LocalObjectReference) bool { return reference.Name == object.GetName() }) {
			continue
		}
		if c.config.PreDeletionSteps != nil {
			proceed, err := c.config.PreDeletionSteps(ctx, object)
			if err != nil {
				results = append(results, c.result(object, OperationResultDeletionFailed, err, StatusPhaseProgressing, fmt.Sprintf("Deletion failed, retrying: %s", err)))
				continue
			}
			if !proceed {
				results = append(results, c.result(object, OperationResultDeletionRequested, nil, StatusPhaseTerminating, "Deletion prepared."))
				continue
			}
		}
		if err := c.cluster.GetClient().Delete(ctx, object); err != nil && client.IgnoreNotFound(err) != nil {
			results = append(results, c.result(object, OperationResultDeletionFailed, err, StatusPhaseProgressing, fmt.Sprintf("Deletion failed, retrying: %s", err)))
		}
	}
	return results, nil
}

func (c *cleaner[T]) result(object client.Object, operation controllerutil.OperationResult, err error, phase, message string) Result {
	return Result{
		Object: NewObject(object, ObjectConfig{
			DeletionPolicy: Delete,
			StatusFunc: func(client.Object) ManagedObjectStatus {
				return ManagedObjectStatus{
					Phase:   phase,
					Message: message,
				}
			},
		}),
		Cluster:         c.cluster,
		OperationResult: operation,
		Error:           err,
	}
}
