package upgrade

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func Test_allowed(t *testing.T) {
	tests := []struct {
		name string // description of this test case
		// Named input parameters for target function.
		upgrades []crdUpgrade
		wantErr  bool
	}{
		{
			name: "pass when there are no version changes",
			upgrades: []crdUpgrade{
				{
					from: crdWith([]string{"v1alpha1", "v1beta1"}, []string{"v1alpha1", "v1beta1"}),
					to:   crdWith([]string{"v1alpha1", "v1beta1"}, nil),
				},
			},
			wantErr: false,
		},
		{
			name: "pass when only adding a new version",
			upgrades: []crdUpgrade{
				{
					from: crdWith([]string{"v1alpha1", "v1beta1"}, []string{"v1alpha1", "v1beta1"}),
					to:   crdWith([]string{"v1alpha1", "v1beta1", "v1"}, nil),
				},
			},
			wantErr: false,
		},
		{
			name: "pass when dropping a migrated version",
			upgrades: []crdUpgrade{
				{
					from: crdWith([]string{"v1alpha1", "v1beta1"}, []string{"v1beta1"}),
					to:   crdWith([]string{"v1beta1"}, nil),
				},
			},
			wantErr: false,
		},
		{
			name: "block when dropping a non-migrated version",
			upgrades: []crdUpgrade{
				{
					from: crdWith([]string{"v1alpha1", "v1beta1"}, []string{"v1alpha1", "v1beta1"}),
					to:   crdWith([]string{"v1beta1"}, nil),
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotErr := allowed(tt.upgrades)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("allowed() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("allowed() succeeded unexpectedly")
			}
		})
	}
}

func crdWith(versions, storedVersions []string) apiextensionsv1.CustomResourceDefinition {
	crd := apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: "Foo",
			},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			StoredVersions: storedVersions,
		},
	}
	for _, version := range versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{Name: version})
	}
	return crd
}
