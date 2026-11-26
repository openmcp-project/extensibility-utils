package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"helm.sh/helm/v3/pkg/chart/loader"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Allowed fetches the Helm chart from the artifact URL of the given OCIRepository and checks whether
// upgrading to it is safe by comparing its CRDs against the respective live cluster CRDs.
// Upgrades are blocked if a CRD version is removed that is still tracked as a stored version.
func Allowed(ctx context.Context, cl client.Client, ociRepoName types.NamespacedName) error {
	var ociRepo sourcev1.OCIRepository
	if err := cl.Get(ctx, ociRepoName, &ociRepo); err != nil {
		return fmt.Errorf("fetching OCIRepository %q: %w", ociRepoName, err)
	}
	if ociRepo.Status.Artifact == nil {
		return fmt.Errorf("OCIRepository %q has no artifact yet", ociRepoName)
	}
	resp, err := http.Get(ociRepo.Status.Artifact.URL)
	if err != nil {
		return fmt.Errorf("downloading chart artifact from %q: %w", ociRepo.Status.Artifact.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading chart artifact from %q: unexpected status %s", ociRepo.Status.Artifact.URL, resp.Status)
	}
	chart, err := loader.LoadArchive(resp.Body)
	if err != nil {
		return fmt.Errorf("loading helm chart archive: %w", err)
	}
	upgradePairs := []crdUpgrade{}
	for _, crdObj := range chart.CRDObjects() {
		var chartCRD apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(crdObj.File.Data, &chartCRD); err != nil {
			return fmt.Errorf("parsing CRD %q from chart: %w", crdObj.Name, err)
		}
		var liveCRD apiextensionsv1.CustomResourceDefinition
		if err := cl.Get(ctx, types.NamespacedName{Name: chartCRD.Name}, &liveCRD); err != nil {
			return fmt.Errorf("fetching live CRD %q: %w", chartCRD.Name, err)
		}
		upgradePairs = append(upgradePairs, crdUpgrade{from: liveCRD, to: chartCRD})
	}
	return allowed(upgradePairs)
}

type crdUpgrade struct {
	from apiextensionsv1.CustomResourceDefinition
	to   apiextensionsv1.CustomResourceDefinition
}

// allowed returns an error if a CRD upgrade removes a version that is still tracked as stored version
func allowed(upgrades []crdUpgrade) error {
	for _, u := range upgrades {
		// check for each from.Version if it has been removed from to.Versions
		for _, fromVersion := range u.from.Spec.Versions {
			if !slices.ContainsFunc(u.to.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
				return v.Name == fromVersion.Name
				// if a fromVersion has been removed, we need to further investigate if the live CRD still tracks this version as a storedVersion
			}) && slices.Contains(u.from.Status.StoredVersions, fromVersion.Name) {
				return fmt.Errorf("can't remove CRD %q version %q. Migrate all existing %q resources on your control plane and remove %q from Status.StoredVersions of CRD %q", u.from.Spec.Names.Kind, fromVersion.Name, fromVersion.Name, fromVersion.Name, u.from.Spec.Names.Kind)
			}
		}
	}
	return nil
}
