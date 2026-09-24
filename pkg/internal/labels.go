package internal

import "sigs.k8s.io/controller-runtime/pkg/client"

const LabelManagedBy = "app.kubernetes.io/managed-by"

// SetManagedBy labels an object with the service provider that owns it.
func SetManagedBy(object client.Object, managedBy string) {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManagedBy] = managedBy
	object.SetLabels(labels)
}

// ManagedBy filters objects by their owning service provider.
func ManagedBy(managedBy string) client.ListOption {
	return client.MatchingLabels{LabelManagedBy: managedBy}
}
