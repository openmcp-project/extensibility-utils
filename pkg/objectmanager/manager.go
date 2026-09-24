package objectmanager

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openmcp-project/extensibility-utils/pkg/internal"
)

const (
	OperationResultDeletionFailed    controllerutil.OperationResult = "deletionFailed"
	OperationResultDeletionRequested controllerutil.OperationResult = "deletionRequested"
	OperationResultDeleted           controllerutil.OperationResult = "deleted"
	OperationResultOrphaned          controllerutil.OperationResult = "orphaned"
)

// ErrReconcileManagedObjects indicates that one or more managed objects returned an error.
var ErrReconcileManagedObjects = errors.New("managed objects contain reconcile errors")

type dependents map[Object][]dependency

// Manager reconciles objects across one or more clusters.
type Manager interface {
	AddCluster(Cluster)
	AddCleaner(Cleaner)
	Apply(context.Context) (ReconcileResult, error)
	Delete(context.Context) (ReconcileResult, error)
}

// ReconcileResult is the consolidated return value for Apply and Delete.
type ReconcileResult struct {
	Results []Result
	// Requeue indicates
	// - on Apply: at least one object reports status phase != ready
	// - on Delete: at least one object remains that is not deleted/orphaned
	Requeue bool
}

func (rr ReconcileResult) ManagedObjects() []ManagedObject {
	managedObjects := make([]ManagedObject, 0, len(rr.Results))
	for _, result := range rr.Results {
		obj := result.Object.GetObject()
		gvk, _ := result.Cluster.GetClient().GroupVersionKindFor(obj)
		managedObjects = append(managedObjects, ManagedObject{
			APIGroup:  gvk.Group,
			Kind:      gvk.Kind,
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
			Location:  string(result.Cluster.GetClusterType()),
			Status:    result.Object.GetStatus(),
		},
		)
	}
	return managedObjects
}

type manager struct {
	serviceProvider string
	clusters        []Cluster
	cleaners        []Cleaner
}

// NewManager creates a Manager for a service provider.
func NewManager(serviceProvider string) Manager {
	return &manager{serviceProvider: serviceProvider}
}

func (m *manager) AddCluster(cluster Cluster) { m.clusters = append(m.clusters, cluster) }

func (m *manager) AddCleaner(cleaner Cleaner) { m.cleaners = append(m.cleaners, cleaner) }

func (m *manager) Apply(ctx context.Context) (ReconcileResult, error) {
	results, err := m.reconcileObjects(ctx, false)
	allReady, objectsError := allObjectsReady(results)
	return ReconcileResult{
		Results: results,
		Requeue: !allReady,
	}, errors.Join(err, objectsError)
}

func (m *manager) Delete(ctx context.Context) (ReconcileResult, error) {
	results, err := m.reconcileObjects(ctx, true)
	allDeleted, objectsError := allObjectsDeleted(results)
	return ReconcileResult{
		Results: results,
		Requeue: !allDeleted,
	}, errors.Join(err, objectsError)
}

func (m *manager) reconcileObjects(ctx context.Context, deleting bool) ([]Result, error) {
	dependents := m.getDependents()
	results := []Result{}
	for _, cluster := range m.clusters {
		for _, object := range cluster.GetObjects() {
			results = append(results, m.reconcileObject(ctx, cluster, object, dependents, deleting))
		}
	}
	for _, cleaner := range m.cleaners {
		resultsToAdd, err := cleaner.Cleanup(ctx)
		if err != nil {
			return results, err
		}
		results = append(results, resultsToAdd...)
	}
	if len(results) == 0 {
		log.FromContext(ctx).V(1).Info("object manager reconciled zero objects")
	}
	return results, nil
}

func (m *manager) reconcileObject(ctx context.Context, cluster Cluster, object Object, dependents dependents, deleting bool) Result {
	if deleting {
		if err := m.checkForDependents(ctx, dependents[object]); err != nil {
			return Result{Object: object, Cluster: cluster, Error: err}
		}
		if object.GetDeletionPolicy() == Orphan {
			return Result{Object: object, Cluster: cluster, OperationResult: OperationResultOrphaned}
		}
		err := cluster.GetClient().Delete(ctx, object.GetObject())
		if apierrors.IsNotFound(err) {
			return Result{Object: object, Cluster: cluster, OperationResult: OperationResultDeleted}
		}
		if err != nil {
			return Result{Object: object, Cluster: cluster, OperationResult: OperationResultDeletionFailed, Error: err}
		}
		return Result{Object: object, Cluster: cluster, OperationResult: OperationResultDeletionRequested}
	}

	result, err := controllerutil.CreateOrUpdate(ctx, cluster.GetClient(), object.GetObject(), func() error {
		internal.SetManagedBy(object.GetObject(), m.serviceProvider)
		return object.Reconcile(ctx)
	})
	return Result{Object: object, Cluster: cluster, OperationResult: result, Error: err}
}

func (m *manager) checkForDependents(ctx context.Context, dependencies []dependency) error {
	var errs []error
	for _, dependency := range dependencies {
		object := dependency.Object.GetObject()
		err := dependency.Cluster.GetClient().Get(ctx, client.ObjectKeyFromObject(object), object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, fmt.Errorf("dependent object still exists: %s", internal.ObjectID(object)))
	}
	return errors.Join(errs...)
}

func (m *manager) getDependents() dependents {
	dependents := dependents{}
	for _, cluster := range m.clusters {
		for _, object := range cluster.GetObjects() {
			for _, dependencyObject := range object.GetDependencies() {
				dependents[dependencyObject] = append(dependents[dependencyObject], dependency{Object: object, Cluster: cluster})
			}
		}
	}
	return dependents
}

// Result summarizes one object reconciliation result.
type Result struct {
	Object          Object
	Cluster         Cluster
	OperationResult controllerutil.OperationResult
	Error           error
}

type dependency struct {
	Object  Object
	Cluster Cluster
}

// allObjects returns whether all results satisfy eval, and an error if any result contains one.
func allObjects(results []Result, eval func(r Result) bool) (bool, error) {
	allObj := true
	for _, result := range results {
		if !eval(result) {
			allObj = false
		}
		if result.Error != nil {
			return false, ErrReconcileManagedObjects
		}
	}
	return allObj, nil
}

func allObjectsDeleted(results []Result) (bool, error) {
	return allObjects(results, func(r Result) bool {
		return r.OperationResult == OperationResultDeleted || r.OperationResult == OperationResultOrphaned
	})
}

func allObjectsReady(results []Result) (bool, error) {
	return allObjects(results, func(r Result) bool {
		return r.Object.GetStatus().Phase == StatusPhaseReady
	})
}
