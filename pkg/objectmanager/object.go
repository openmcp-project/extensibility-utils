package objectmanager

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeletionPolicy distinguishes between deleting and orphaning an object.
type DeletionPolicy string

const (
	Orphan DeletionPolicy = "orphan"
	Delete DeletionPolicy = "delete"
)

const (
	StatusPhaseReady       = "Ready"
	StatusPhaseProgressing = "Progressing"
	StatusPhaseTerminating = "Terminating"
	StatusPhaseUnknown     = "Unknown"
)

// ReconcileFunc configures an object before it is created or updated.
type ReconcileFunc func(context.Context, client.Object) error

// StatusFunc returns the lifecycle status of an object.
type StatusFunc func(client.Object) ManagedObjectStatus

// ObjectConfig configures an object registered with a Cluster.
type ObjectConfig struct {
	ReconcileFunc  ReconcileFunc
	DependsOn      []Object
	DeletionPolicy DeletionPolicy
	StatusFunc     StatusFunc
}

// Object represents an object managed by a Manager.
type Object interface {
	ClientObject() client.Object
	Reconcile(context.Context) error
	Dependencies() []Object
	DeletionPolicy() DeletionPolicy
	Status() ManagedObjectStatus
}

type object struct {
	object         client.Object
	reconcileFunc  ReconcileFunc
	dependencies   []Object
	deletionPolicy DeletionPolicy
	statusFunc     StatusFunc
}

var _ Object = (*object)(nil)

// NewObject registers a Kubernetes object with its reconciliation behavior.
func NewObject(clientObject client.Object, config ObjectConfig) Object {
	if config.DeletionPolicy == "" {
		config.DeletionPolicy = Delete
	}
	return &object{
		object:         clientObject,
		reconcileFunc:  config.ReconcileFunc,
		dependencies:   config.DependsOn,
		deletionPolicy: config.DeletionPolicy,
		statusFunc:     config.StatusFunc,
	}
}

func (o *object) ClientObject() client.Object { return o.object }

func (o *object) Reconcile(ctx context.Context) error {
	if o.reconcileFunc == nil {
		return nil
	}
	return o.reconcileFunc(ctx, o.object)
}

func (o *object) Dependencies() []Object { return o.dependencies }

func (o *object) DeletionPolicy() DeletionPolicy { return o.deletionPolicy }

func (o *object) Status() ManagedObjectStatus {
	if o.statusFunc == nil {
		return ManagedObjectStatus{Phase: StatusPhaseUnknown, Message: "No status function defined."}
	}
	return o.statusFunc(o.object)
}

// ManagedObjectStatus describes an object's observed lifecycle state.
type ManagedObjectStatus struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
}

// ManagedObject is the serializable result returned by Apply and Delete.
// Its JSON tags allow consumers to use it directly in their status API.
type ManagedObject struct {
	APIGroup            string `json:"apiGroup,omitempty"`
	Kind                string `json:"kind"`
	Name                string `json:"name"`
	Namespace           string `json:"namespace,omitempty"`
	Location            string `json:"location,omitempty"`
	ManagedObjectStatus `json:",inline"`
}

// SimpleStatus reports whether an object is terminating, pending, or present.
func SimpleStatus(object client.Object) ManagedObjectStatus {
	if !object.GetDeletionTimestamp().IsZero() {
		return ManagedObjectStatus{Phase: StatusPhaseTerminating, Message: "Resource is terminating."}
	}
	if object.GetUID() == "" {
		return ManagedObjectStatus{Phase: StatusPhaseProgressing, Message: "Resource has not been created yet."}
	}
	return ManagedObjectStatus{Phase: StatusPhaseReady, Message: "Resource exists."}
}

// NoOp does not do anything with the provided object and returns nil.
func NoOp(context.Context, client.Object) error {
	return nil
}
